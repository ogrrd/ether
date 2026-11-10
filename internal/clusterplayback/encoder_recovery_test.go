package clusterplayback

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type recoveryTestEncoder struct {
	fixtureEncoder
	requests []core.HLSRequest
	ready    chan struct{}
	proceed  chan struct{}
	data     []byte
	join     <-chan struct{}
}

func (e *recoveryTestEncoder) SupportsDistributedRequest(core.HLSRequest) bool { return true }
func (e *recoveryTestEncoder) StartHLSOwned(ctx context.Context, q core.HLSRequest) (core.HLSSession, core.EncoderRetirement, error) {
	if e.ready != nil {
		close(e.ready)
		select {
		case <-e.proceed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	_, token, err := e.fixtureEncoder.StartHLSOwned(ctx, q)
	if err != nil {
		return nil, token, err
	}
	e.mu.Lock()
	e.requests = append(e.requests, cloneAdmissionRequest(q))
	session := e.session
	e.mu.Unlock()
	if e.join != nil {
		token = heldRecoveryRetirement{EncoderRetirement: token, join: e.join}
	}
	return &recoveryTestSession{fixtureSession: session, data: e.data}, token, nil
}

// Test-only independent join gate: stopping the encoder does not manufacture
// its completion proof. The underlying encoder must also have joined.
type heldRecoveryRetirement struct {
	core.EncoderRetirement
	join <-chan struct{}
}

func (p heldRecoveryRetirement) EncoderDone() <-chan struct{} { return p.join }
func (p heldRecoveryRetirement) EncoderErr() error {
	select {
	case <-p.join:
		return p.EncoderRetirement.EncoderErr()
	default:
		return core.ErrEncoderPending
	}
}

// A registered gateway response producer cannot be declared joined merely
// because its request was canceled or its encoder process finished.
type heldRecoveryInputWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *heldRecoveryInputWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(p)
}

type recoveryTestSession struct {
	*fixtureSession
	data []byte
}

func (s *recoveryTestSession) ServeSegment(w http.ResponseWriter, q *http.Request, n int) {
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
	w.Header().Set("X-Segment", strconv.Itoa(n))
	_, _ = w.Write(s.data)
}

// This invariant uses real mTLS RPC, SQLite placement and source gateway input.
// The encoded bytes are a recorded offline TS fixture: it tests ownership and
// complete/unique delivery, not real decoder or audio splice continuity.
func TestEncoderLossRecoveryInvariant(t *testing.T) {
	testEncoderLossRecoveryInvariant(t, false)
}

func TestReencodedAACLossRecoveryInvariant(t *testing.T) {
	testEncoderLossRecoveryInvariant(t, true)
}

func testEncoderLossRecoveryInvariant(t *testing.T, reencoded bool, copyMode ...bool) {
	copied := len(copyMode) > 0 && copyMode[0]
	modes := []string{"exit", "partition", "newboot", "boot-before-recovery", "removed", "clock", "tls-replaced", "tls-denied", "gone", "pause", "stop-before-restore", "terminal-takeover", "refused"}
	if !reencoded && !copied {
		modes = append(modes, "ramp")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			fixtureStarted := time.Now()
			data, err := os.ReadFile("../playbackbuffer/testdata/cpu.ts")
			if err != nil {
				t.Fatal(err)
			}
			db := openFixtureDB(t)
			v := NewVerifier()
			p, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: 10 * time.Minute, LockTimeout: fixtureSQLBudget()}, v)
			if err != nil {
				t.Fatal(err)
			}
			var encoderJoin chan struct{}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				encoderJoin = make(chan struct{})
			}
			a := &recoveryTestEncoder{data: data, join: encoderJoin}
			b := &recoveryTestEncoder{data: data, ready: make(chan struct{}), proceed: make(chan struct{})}
			var prober core.Transcoder = &a.fixtureEncoder
			if copied {
				prober = copyRecoveryProber{fixtureEncoder: &a.fixtureEncoder}
			}
			manager, err := playback.New(playback.Deps{Catalog: fixtureCatalog{}, Resolver: fixtureResolver{}, Engine: fixtureEngine{}, Transcoder: prober, SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile { return core.QualityProfile{} }}, config.Defaults(t.TempDir()), playback.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			access, err := playback.NewSourceAccess(manager, 16, 16)
			if err != nil {
				t.Fatal(err)
			}
			ids := []nodewire.Identity{{Cluster: "loss", NodeID: "source", Boot: "source-boot"}, {Cluster: "loss", NodeID: "a", Boot: "a-boot"}, {Cluster: "loss", NodeID: "b", Boot: "b-boot"}, {Cluster: "loss", NodeID: "controller", Boot: "controller-boot"}}
			newID := ids[1]
			newID.Boot = "a-new-boot"
			guards := fixtureGuards(t, append(append([]nodewire.Identity{}, ids...), newID)...)
			var replaced, gone atomic.Bool
			endpoints := map[nodewire.Identity]string{}
			var clockFault atomic.Bool
			member := func(_ context.Context, id nodewire.Identity, _ config.NodeRole) error {
				if id == ids[1] && (replaced.Load() && mode != "tls-denied" || gone.Load()) {
					return nodewire.ErrDenied
				}
				return nil
			}
			eligible := func(_ context.Context, id nodewire.Identity, _ config.NodeRole) error {
				if id == ids[1] && clockFault.Load() {
					return ErrDenied
				}
				return nil
			}
			base := Options{DB: db, Placement: p, Endpoint: func(_ context.Context, id nodewire.Identity) (string, error) { return endpoints[id], nil }, Eligible: eligible, MemberRole: member, Capacity: 16, ClockBoundKnown: true, ClockSkew: time.Millisecond, EncoderRPCDeadline: 50 * time.Millisecond, EncoderFailureGrace: time.Millisecond, EncoderSegmentWait: time.Second, Decide: func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision { return core.Decision{} }}
			if mode == "ramp" {
				caps := remotetranscode.Supported()
				caps.AuthoredRamp = true
				base.RemoteCapabilities, base.WorkerCapabilities = caps, caps
				base.AuthorRamp = func(q core.HLSRequest) core.HLSRequest {
					return transcode.AuthorDistributedRamp(q, config.Transcode{FastStart: true})
				}
			}
			if copied {
				caps := remotetranscode.Supported()
				caps.CopyTimeline = true
				caps.AuthoredRamp = true
				caps.Keyframes = true
				base.RemoteCapabilities, base.WorkerCapabilities = caps, caps
			}
			var freshMembership atomic.Bool
			freshMembership.Store(true)
			canceledStop := make(chan *http.Request, 1)
			var captureStop sync.Once
			var down atomic.Bool
			pauseSegmentStarted := make(chan struct{})
			var pauseOnce sync.Once
			var workerServer *httptest.Server
			runtimes := make([]*Runtime, 4)
			for i := range ids {
				o := base
				o.Self = ids[i]
				o.Guard = guards[i]
				switch i {
				case 0:
					o.Playback = manager
					o.SourceAccess = access
					o.SourcePolicy = func(context.Context, *core.Viewer, *core.PlaySession) error { return nil }
					o.ActiveViewer = func(context.Context, int64) (*core.Viewer, error) { return &core.Viewer{ID: 9}, nil }
				case 1:
					if mode == "stop-before-restore" || mode == "terminal-takeover" {
						o.MemberRole = func(ctx context.Context, id nodewire.Identity, role config.NodeRole) error {
							if id == ids[3] && !freshMembership.Load() {
								return nodewire.ErrDenied
							}
							return member(ctx, id, role)
						}
					}
					o.ClockReady = func() bool { return !clockFault.Load() }
					o.Encoder = a
				case 2:
					o.Encoder = b
				}
				runtime, err := NewRuntime(o)
				if err != nil {
					t.Fatal(err)
				}
				runtimes[i] = runtime
				defer runtime.Close()
				handler := runtime.Handler()
				if i == 1 {
					handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
						if down.Load() {
							if mode == "pause" || (mode == "stop-before-restore" || mode == "terminal-takeover") {
								var request struct{ Op string }
								raw, _ := io.ReadAll(q.Body)
								_ = json.Unmarshal(raw, &request)
								if request.Op == "segment" {
									pauseOnce.Do(func() { close(pauseSegmentStarted) })
								}
								<-q.Context().Done()
								if (mode == "stop-before-restore" || mode == "terminal-takeover") && request.Op == "stop" {
									captureStop.Do(func() {
										queued := q.Clone(q.Context())
										queued.Body = io.NopCloser(bytes.NewReader(raw))
										canceledStop <- queued
									})
								}
								return
							}
							conn, _, err := w.(http.Hijacker).Hijack()
							if err == nil {
								_ = conn.Close()
							}
							return
						}
						runtime.Handler().ServeHTTP(w, q)
					})
				}
				server := httptest.NewUnstartedServer(handler)
				server.TLS = guards[i].ServerTLS()
				if i == 1 {
					server.Config.SetKeepAlivesEnabled(false)
					server.TLS.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
						if replaced.Load() {
							return guards[4].ServerTLS(), nil
						}
						return guards[1].ServerTLS(), nil
					}
				}
				server.StartTLS()
				if i == 1 {
					workerServer = server
				}
				defer server.Close()
				endpoints[ids[i]] = server.URL
			}
			// OpenTest may create and migrate an isolated PostgreSQL schema.
			// Fixture construction is not playback: a cold/durable database
			// must not consume the placement/recovery harness budget.
			t.Logf("fixture setup: %s (%s); starting measured execution budget", time.Since(fixtureStarted), store.DialectOf(db))
			ctx, cancel := context.WithTimeout(context.Background(), fixtureExecutionBudget(fixtureStarted, 10*time.Second))
			defer cancel()
			front := runtimes[3]
			originalFront := front
			front.o.Roles = config.NodeRoles{config.RoleServer}
			if err = v.Bind(front); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids[1:3] {
				scope := id.NodeID + "/streams"
				if err = p.SetPool(ctx, scope, config.RoleTranscode, 1); err != nil {
					t.Fatal(err)
				}
				if err = p.SetTarget(ctx, placement.Target{NodeID: id.NodeID, Boot: id.Boot}, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {scope}}); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := front.PrepareSource(ctx, ids[0], PrepareRequest{Operation: "prepare", Viewer: 9, Key: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "loss"}, Protocol: "hls"})
			if err != nil {
				t.Fatal(err)
			}
			q := core.HLSRequest{SessionID: "recover", Info: core.MediaInfo{Duration: time.Minute, Container: "mkv", Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Height: 720}}}, SegmentDuration: 4 * time.Second, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCopy: false, AudioCopy: true, Container: "mpegts", SubtitleStream: -1}}
			if reencoded {
				q.Decision.AudioCopy = false
				q.Decision.VideoCodec = "h264"
				q.Decision.AudioCodec = "aac"
				q.Decision.AudioStream = 1
				q.Decision.AudioChannels = 2
				q.Info.Streams[0].FrameRate = 24
				q.Info.Streams = append(q.Info.Streams, core.StreamInfo{Index: 1, Type: "audio", Codec: "ac3", SampleRate: 48000, DecodedPadding: 256, Channels: 6})
			}
			if copied {
				q = copyRecoveryRequest()
				q.SessionID = "recover"
				forged := cloneAdmissionRequest(q)
				forged.Info.CopyTimeline.PacketSHA256[0]++
				if _, err := front.StartPreparedHLS(ctx, 9, prepared, forged); err != ErrDenied {
					t.Fatalf("forged source timeline admitted: %v", err)
				}
			}
			session, err := front.StartPreparedHLS(ctx, 9, prepared, q)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "pause" || (mode == "stop-before-restore" || mode == "terminal-takeover") {
				// The first discovered server is a different replica, just like
				// ingress landing away from the session's coordinator in the lab.
				runtimes[2].o.MediaServers = func(context.Context) ([]nodewire.Identity, error) { return []nodewire.Identity{ids[2], ids[3]}, nil }
				if err := front.BindPlexDelivery(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					parts := strings.Split(q.URL.Path, "/")
					n, err := strconv.Atoi(strings.TrimSuffix(parts[len(parts)-1], ".ts"))
					if err != nil {
						http.NotFound(w, q)
						return
					}
					session.ServeSegment(w, q, n)
				})); err != nil {
					t.Fatal(err)
				}
			}
			fetched := map[int]int{}
			fetch := func(n int) {
				t.Helper()
				w := httptest.NewRecorder()
				if mode == "pause" || (mode == "stop-before-restore" || mode == "terminal-takeover") {
					request := httptest.NewRequest("GET", "https://public/video/:/transcode/universal/session/recover/base/"+strconv.Itoa(n)+".ts", nil).WithContext(ctx)
					if !runtimes[2].ForwardMedia(w, request, "recover") {
						t.Fatal("media missed coordinator capture")
					}
				} else {
					session.ServeSegment(w, httptest.NewRequest("GET", "https://public/segment", nil).WithContext(ctx), n)
				}
				if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("segment %d: %d %s", n, w.Code, w.Body.String())
				}
				fetched[n]++
			}
			fetch(0)
			fetch(1)
			prefix, err := front.SnapshotOutput(q.SessionID)
			if err != nil || prefix.Buffer.State.PublishedFloor != 1 {
				t.Fatalf("delivered no-cache segments did not advance restart floor: %+v %v", prefix, err)
			}
			if mode == "ramp" {
				front.o.AuthorRamp = func(q core.HLSRequest) core.HLSRequest { q.Ramp.Segments = 9; return q }
			}
			front.mu.Lock()
			old := *front.executions[q.SessionID]
			front.mu.Unlock()
			var releaseInput func()
			var inputWriteDone chan struct{}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				gate := make(chan struct{})
				var once sync.Once
				releaseInput = func() { once.Do(func() { close(gate) }) }
				inputWriteDone = make(chan struct{})
				defer func() { releaseInput(); <-inputWriteDone }()
				writer := &heldRecoveryInputWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: gate}
				request := httptest.NewRequest("GET", a.requests[0].InputURL, nil).WithContext(ctx)
				headers, err := a.session.input.HeadersForInput(a.requests[0].InputURL)
				if err != nil {
					t.Fatal(err)
				}
				for k, v := range headers {
					request.Header.Set(k, v)
				}
				go func() {
					defer close(inputWriteDone)
					runtimes[1].inputs[remoteIdentity(old.route)].gateway.ServeHTTP(writer, request)
				}()
				select {
				case <-writer.entered:
				case <-ctx.Done():
					t.Fatal("input response never entered held write")
				}
			}
			lossStarted := time.Now()
			if mode == "refused" {
				front.o.EncoderFailureGrace = time.Hour
				workerServer.Close()
			}
			if mode == "tls-denied" {
				replaced.Store(true)
			}
			if mode == "tls-replaced" {
				replaced.Store(true)
				front.o.EncoderFailureGrace = time.Hour
				if _, err = db.ExecContext(ctx, `INSERT INTO cluster_nodes(id,incarnation,started_at,last_seen) VALUES(?,?,?,?)`, newID.NodeID, newID.Boot, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
				// Membership changes before the replacement publishes placement capacity.
			}
			if mode == "gone" {
				gone.Store(true)
			}
			if mode == "exit" || mode == "newboot" || mode == "boot-before-recovery" || mode == "removed" {
				if err = a.session.RetireEncoder(); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "clock" {
				clockFault.Store(true)
				// This is a clock quarantine, with the transport still live.
				// Renewal must refuse, while status and exact stop stay usable.
				if err := runtimes[1].authorizeWorker(ctx, old.route.Fence.Controller.Boot, "renew", remoteIdentity(old.route), nil); err != ErrDenied {
					t.Fatalf("quarantined renewal: %v", err)
				}
				if err := runtimes[1].authorizeWorker(ctx, old.route.Fence.Controller.Boot, "status", remoteIdentity(old.route), nil); err != nil {
					t.Fatalf("quarantined status: %v", err)
				}
				// Quarantine blocks new authority, not observation of the healthy
				// execution. Wait beyond the failure grace with a live status RPC.
				_ = front.Tick(ctx)
				time.Sleep(2 * time.Millisecond)
				_ = front.Tick(ctx)
				rec, err := p.Lookup(ctx, q.SessionID)
				if err != nil || rec.Generation != 1 || len(b.requests) != 0 {
					t.Fatalf("clock quarantine triggered recovery: %+v %v", rec, err)
				}
				clockFault.Store(false)
				// A subsequent real RPC loss must still recover.
				down.Store(true)
			} else {
				down.Store(true)
			}
			var cancelPaused context.CancelFunc
			var pausedDone chan struct{}
			if mode == "pause" || (mode == "stop-before-restore" || mode == "terminal-takeover") {
				readCtx, cancelRead := context.WithCancel(ctx)
				cancelPaused = cancelRead
				defer cancelRead()
				pausedDone = make(chan struct{})
				go func() {
					defer close(pausedDone)
					old.output.ServeSegment(httptest.NewRecorder(), httptest.NewRequest("GET", "/segment", nil).WithContext(readCtx), 2)
				}()
				select {
				case <-pauseSegmentStarted:
				case <-ctx.Done():
					t.Fatal("worker never received paused segment")
				}
				snap, err := front.SnapshotOutput(q.SessionID)
				if err != nil || snap.Buffer.State.PublishedFloor != 1 {
					t.Fatal("in-flight paused segment changed delivered prefix", snap, err)
				}
			}
			if mode == "boot-before-recovery" {
				// A confirmed boot change bypasses even a long transport grace.
				front.o.EncoderFailureGrace = time.Hour
				// StatefulSet reuses the node ID before the controller detects
				// encoder failure. No spare slot on the first recovery attempt:
				// capacity pressure must preserve the gate for a later retry.
				if err = p.SetTarget(ctx, placement.Target{NodeID: "a", Boot: "a-new-boot"}, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"a/streams"}}); err != nil {
					t.Fatal(err)
				}
				if err = p.SetPool(ctx, "b/streams", config.RoleTranscode, 0); err != nil {
					t.Fatal(err)
				}
				_ = front.Tick(ctx)
				rec, lookupErr := p.Lookup(ctx, q.SessionID)
				h, gateErr := front.readOperationGate(ctx, operationKey{ids[0].Cluster, q.SessionID, 1})
				if lookupErr != nil || gateErr != nil || rec.Retired || rec.Generation != 1 || h.Gate.Seal != nil {
					t.Fatalf("capacity pressure destroyed recovery authority: %+v %+v %v %v", rec, h, lookupErr, gateErr)
				}
				if err = p.SetPool(ctx, "b/streams", config.RoleTranscode, 1); err != nil {
					t.Fatal(err)
				}
			}
			// A transient failure requires at least three failed observations;
			// modes without positive loss evidence take one tick more. Every tick
			// that may start the replacement runs on this goroutine, never the
			// test's: b's start blocks until the test closes b.proceed. On a
			// loaded runner a clock-quarantine status probe can exceed its 50ms
			// deadline and count as a failure, so the start comes a tick early.
			ticks := 3
			if mode != "boot-before-recovery" && mode != "tls-replaced" && mode != "tls-denied" && mode != "gone" && mode != "refused" {
				ticks++
			}
			replacementDone := make(chan error, 1)
			go func() {
				for i := 0; i < ticks; i++ {
					err := front.Tick(ctx)
					front.mu.Lock()
					replaced := front.executions[q.SessionID].route.Fence.Generation > 1
					front.mu.Unlock()
					if replaced {
						replacementDone <- err
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
				replacementDone <- ErrPending
			}()
			select {
			case <-b.ready:
			case <-ctx.Done():
				t.Fatal("replacement not started")
			}
			if cancelPaused != nil {
				cancelPaused()
			}
			if mode == "boot-before-recovery" {
				if err := front.Tick(ctx); err != ErrPending {
					t.Fatalf("concurrent control tick bypassed recovery mutex: %v", err)
				}
			}
			waiting := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				request := httptest.NewRequest("GET", "https://public/wait", nil).WithContext(ctx)
				// Both public forwarding and controller handlers capture the old route
				// before waiting. The source runtime supplies a replica with no local
				// execution map, so successor authorization must use durable history.
				forwarded := runtimes[0].fencedPublicDelivery(w, request, old.route)
				defer forwarded.Close()
				controller := front.fencedPublicDelivery(forwarded, request, old.route)
				defer controller.Close()
				session.ServeSegment(controller, request, 2)
				waiting <- w
			}()
			select {
			case <-waiting:
				t.Fatal("segment request failed before replacement ready")
			case <-time.After(10 * time.Millisecond):
			}
			close(b.proceed)
			if err = <-replacementDone; err != nil {
				t.Fatal("replacement", err)
			}
			if pausedDone != nil {
				<-pausedDone
			}
			response := <-waiting
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), data) {
				t.Fatalf("waiting segment: %d", response.Code)
			}
			fetched[2]++
			// Explicitly retry a retained historical segment after the generation
			// switch: its original bytes are valid under current delivery authority.
			historical := httptest.NewRecorder()
			session.ServeSegment(historical, httptest.NewRequest("GET", "https://public/history", nil).WithContext(ctx), 0)
			if historical.Code != 200 || !bytes.Equal(historical.Body.Bytes(), data) || historical.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("historical segment after recovery: %d", historical.Code)
			}

			t.Logf("recovery latency: %s", time.Since(lossStarted))
			if time.Since(lossStarted) > 3*time.Second {
				t.Fatal("recovery exceeded 3 second bound")
			}
			record, err := p.Lookup(ctx, q.SessionID)
			if err != nil || record.Generation != 2 || record.Target.NodeID != "b" {
				t.Fatalf("replacement: %+v %v", record, err)
			}
			if len(b.requests) != 1 || b.requests[0].Resume == nil || *b.requests[0].Resume != (core.HLSResume{FirstSegment: int(prefix.Buffer.State.PublishedFloor + 1), Generation: 2}) || !reflect.DeepEqual(b.requests[0].Decision, q.Decision) || !reflect.DeepEqual(b.requests[0].Info, q.Info) {
				t.Fatalf("immutable plan/resume: %+v", b.requests)
			}
			if mode == "ramp" && (b.requests[0].Ramp != (core.HLSRamp{Version: 1, Segments: 2, Duration: 2 * time.Second}) || a.requests[0].Ramp != b.requests[0].Ramp) {
				t.Fatal("replacement changed admitted ramp")
			}
			gate, err := front.readOperationGate(ctx, operationKey{ids[0].Cluster, q.SessionID, 1})
			if err != nil || gate.State != gateSealedClaimed || gate.Gate.Arm == nil {
				t.Fatalf("sealed armed gate: %+v %v", gate, err)
			}
			// Original capacity is still charged while its process cannot be checked.
			var charged int
			if err = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 1 {
				t.Fatalf("lost charge: %d %v", charged, err)
			}
			events, eventErr := front.RecoveryEvents(ctx, 256)
			if eventErr != nil || len(events) != 1 || events[0].Status != "recovered" || events[0].FailedNodeID != "a" || events[0].ReplacementNodeID != "b" || events[0].InterruptionMS == nil || events[0].ClientStalls != nil {
				t.Fatalf("events: %+v %v", events, eventErr)
			}
			if mode == "newboot" || mode == "boot-before-recovery" || mode == "tls-replaced" {
				if err = p.SetTarget(ctx, placement.Target{NodeID: "a", Boot: "a-new-boot"}, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"a/streams"}}); err != nil {
					t.Fatal(err)
				}
				// Reconciliation uses durable displaced history even if no local
				// old-session pointer remains after controller restart.
				front.mu.Lock()
				failed := front.executions[q.SessionID].failed
				front.executions[q.SessionID].failed = nil
				front.mu.Unlock()
				if err = front.reconcileBootRetirements(ctx); err != nil {
					t.Fatal(err)
				}
				front.mu.Lock()
				front.executions[q.SessionID].failed = failed
				front.mu.Unlock()
				if err = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 0 {
					t.Fatal("new boot leaked capacity", charged, err)
				}
			}
			if mode == "removed" || mode == "refused" {
				if mode == "refused" {
					_ = a.session.RetireEncoder()
					runtimes[1].Cleanup()
				}
				if err = p.RemoveTarget(ctx, old.route.Fence.Target, "offline-test/operator-fenced-exact-boot"); err != nil {
					t.Fatal(err)
				}
				if err = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 0 {
					t.Fatal("removed node leaked capacity", charged, err)
				}
				if err = p.SetTarget(ctx, old.route.Fence.Target, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"a/streams"}}); err != placement.ErrFenced {
					t.Fatal("removed boot readmitted", err)
				}
			}
			// Retiring the authenticated dead boot must leave the replacement
			// authoritative and charged, with the client still able to continue.
			if mode == "boot-before-recovery" {
				rec, err := p.Lookup(ctx, q.SessionID)
				if err != nil || rec.Retired || !rec.Authority || rec.Generation != 2 {
					t.Fatalf("old boot retirement affected replacement: %+v %v", rec, err)
				}
				if err = db.QueryRow(`SELECT COUNT(*) FROM placement_claims`).Scan(&charged); err != nil || charged != 1 {
					t.Fatalf("replacement capacity lost: %d %v", charged, err)
				}
			}
			for n := 3; n < 6; n++ {
				fetch(n)
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				_ = session.Close()
				for i := 0; i < 30; i++ {
					_ = front.Tick(ctx)
					runtimes[0].Cleanup()
					runtimes[2].Cleanup()
					record, _ = p.Lookup(ctx, q.SessionID)
					if record.Retired {
						break
					}
				}
				if !record.Retired || record.Generation != 2 {
					t.Fatal("replacement did not retire before old worker restored", record)
				}
				for i := 0; i < 2; i++ {
					_ = front.Tick(ctx)
				}
				if err = db.QueryRow(`SELECT COUNT(*) FROM placement_claims`).Scan(&charged); err != nil || charged != 0 {
					t.Fatal("retired replacement retained current capacity", charged, err)
				}
				if err = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 1 {
					t.Fatal("unreachable old worker lost its charge without exit proof", charged, err)
				}
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				lease, err := front.CurrentCoordinator(ctx, q.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(3 * time.Millisecond)
				if err = front.MaintainCoordination(ctx); err != nil {
					t.Fatal(err)
				}
				renewed, err := front.CurrentCoordinator(ctx, q.SessionID)
				if err != nil || !renewed.Deadline.After(lease.Deadline) || renewed.Epoch != lease.Epoch {
					t.Fatal("retired G2 lost terminal lease renewal while displaced G1 remains", lease, renewed, err)
				}
				// Deliberately stop both SQL-only maintenance loops and cross a real
				// coordination lease. Fast fixtures previously finished before expiry.
				timer := time.NewTimer(time.Until(renewed.Deadline.Add(2*front.o.ClockSkew + time.Millisecond)))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					t.Fatal(ctx.Err())
				}
				if _, err := originalFront.coordinatorHeaders(q.SessionID, lease.Epoch)(ctx); err == nil {
					t.Fatal("expired epoch still authorizes old stop")
				}
				if mode == "terminal-takeover" {
					front = runtimes[0]
					front.o.Roles = config.NodeRoles{config.RoleServer, config.RoleSource}
					verifier := NewVerifier()
					terminalLedger, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: 10 * time.Minute, LockTimeout: fixtureSQLBudget()}, verifier)
					if err != nil {
						t.Fatal(err)
					}
					front.o.Placement = terminalLedger
					if err = verifier.Bind(front); err != nil {
						t.Fatal(err)
					}
				}
				if err = front.MaintainCoordination(ctx); err != nil {
					t.Fatal(err)
				}
				terminal, err := front.CurrentCoordinator(ctx, q.SessionID)
				if err != nil || terminal.Identity != front.o.Self || terminal.Epoch <= renewed.Epoch || !terminal.Deadline.After(time.Now()) {
					t.Fatal("terminal lease not reacquired", terminal, err)
				}
				if _, err = originalFront.coordinatorHeaders(q.SessionID, lease.Epoch)(ctx); err == nil {
					t.Fatal("stale epoch revived after terminal takeover")
				}
				staleCtx := context.WithValue(ctx, workerAuthorityKey{}, workerAuthority{peer: originalFront.o.Self, epoch: strconv.FormatInt(lease.Epoch, 10)})
				if err := runtimes[1].authorizeWorker(staleCtx, old.route.Fence.Controller.Boot, "stop", remoteIdentity(old.route), nil); err == nil {
					t.Fatal("worker accepted stale terminal epoch")
				}
				if mode == "terminal-takeover" {
					_ = originalFront.MaintainCoordination(ctx)
					stale, err := originalFront.CurrentCoordinator(ctx, q.SessionID)
					if err != nil || stale.Identity != front.o.Self || stale.Epoch != terminal.Epoch {
						t.Fatal("old owner stole terminal epoch", stale, err)
					}
					originalFront.mu.Lock()
					staleExecution := *originalFront.executions[q.SessionID]
					originalFront.mu.Unlock()
					originalFront.reconcileDisplaced(ctx, &staleExecution)
				}
				t.Logf("terminal coordination: expired epoch%d -> fresh epoch%d owner=%s; G2 stays retired", renewed.Epoch, terminal.Epoch, terminal.Identity.NodeID)
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				freshMembership.Store(false)
			}
			down.Store(false)
			gone.Store(false)
			replaced.Store(false)
			late := httptest.NewRecorder()
			old.session.ServeSegment(late, httptest.NewRequest("GET", "https://public/late", nil).WithContext(ctx), 2)
			if late.Code == 200 {
				t.Fatal("old generation output accepted")
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				var queued *http.Request
				select {
				case queued = <-canceledStop:
				case <-ctx.Done():
					t.Fatal("no canceled exact-old-stop request captured")
				}
				if queued.Context().Err() == nil {
					t.Fatal("queued request did not expire")
				}
				replayed := httptest.NewRecorder()
				runtimes[1].Handler().ServeHTTP(replayed, queued)
				if replayed.Code == 200 {
					t.Fatal("canceled queued stop fabricated terminal success")
				}
				freshMembership.Store(true)
				assertHeld := func(stage string) {
					t.Helper()
					if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 1 {
						t.Fatalf("%s: old charge released before both joins: %d %v", stage, charged, err)
					}
					rec, err := p.Lookup(ctx, q.SessionID)
					if err != nil || !rec.Retired || rec.Generation != 2 {
						t.Fatal("current G2 changed", rec, err)
					}
					front.mu.Lock()
					currentOld := front.executions[q.SessionID].failed[0]
					front.mu.Unlock()
					if currentOld.session.EncoderErr() == nil {
						t.Fatal("partial old proof accepted", stage)
					}
				}
				_ = front.Tick(ctx) // Reattach historical status under the fresh epoch.
				_ = front.Tick(ctx) // Fresh exact stop, both join barriers still pending.
				assertHeld("both pending after fresh membership")
				select {
				case <-a.session.EncoderDone(): // Underlying fake process has joined.
				default:
					t.Fatal("fresh stop never reached the old encoder")
				}
				close(encoderJoin)
				runtimes[1].Cleanup()
				_ = front.Tick(ctx)
				assertHeld("encoder joined, input producer still held")
				releaseInput()
				<-inputWriteDone
				t.Logf("same-boot restoration: canceled stop HTTP%d; charge held for independent encoder/input joins; fresh membership restored", replayed.Code)
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				// New terminal ownership supplies neither encoder authority nor source
				// preparation. Historical start/renew and current retired start refuse.
				for _, op := range []string{"start", "renew"} {
					if err := runtimes[1].authorizeWorker(ctx, old.route.Fence.Controller.Boot, op, remoteIdentity(old.route), nil); err == nil {
						t.Fatal("terminal coordination revived old authority", op)
					}
				}
				front.mu.Lock()
				terminalOld := front.executions[q.SessionID].failed[0]
				front.mu.Unlock()
				if _, _, err := terminalOld.adapter.StartHLSOwned(ctx, q); err == nil {
					t.Fatal("retirement-only adapter started encoder")
				}
				for _, op := range []string{"start", "renew"} {
					if err := runtimes[2].authorizeWorker(ctx, record.Controller.Boot, op, remoteIdentity(front.executions[q.SessionID].route), nil); err == nil {
						t.Fatal("terminal coordination revived retired G2", op)
					}
				}
				if len(a.requests) != 1 || len(b.requests) != 1 {
					t.Fatal("terminal takeover started new encoder")
				}
			}
			// Exact old-worker stop/retirement releases only its displaced charge.
			for i := 0; i < 30; i++ {
				runtimes[1].Cleanup()
				_ = front.Tick(ctx)
				_ = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged)
				if charged == 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err = db.QueryRow(`SELECT COUNT(*) FROM placement_displaced_claims`).Scan(&charged); err != nil || charged != 0 {
				t.Fatalf("displaced capacity leak: %d %v", charged, err)
			}
			if mode == "stop-before-restore" || mode == "terminal-takeover" {
				before, err := front.CurrentCoordinator(ctx, q.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(3 * time.Millisecond)
				if err = front.MaintainCoordination(ctx); err != nil {
					t.Fatal(err)
				}
				after, err := front.CurrentCoordinator(ctx, q.SessionID)
				if err != nil || !after.Deadline.Equal(before.Deadline) {
					t.Fatal("empty terminal coordination renewed forever", before, after, err)
				}
			}

			for n := 0; n < 6; n++ {
				if fetched[n] != 1 {
					t.Fatalf("sequence %d count %d", n, fetched[n])
				}
			}
			_ = session.Close()
			for i := 0; i < 30; i++ {
				_ = front.Tick(ctx)
				runtimes[0].Cleanup()
				runtimes[1].Cleanup()
				runtimes[2].Cleanup()
				record, _ = p.Lookup(ctx, q.SessionID)
				if record.Retired {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !record.Retired || a.session.EncoderErr() != nil || b.session.EncoderErr() != nil {
				t.Fatal("encoder retirement leak")
			}
			if err = db.QueryRow(`SELECT COUNT(*) FROM placement_claims`).Scan(&charged); err != nil || charged != 0 {
				t.Fatalf("current capacity leak: %d %v", charged, err)
			}
			front.mu.Lock()
			done := front.executions[q.SessionID].done
			front.mu.Unlock()
			select {
			case <-done:
			default:
				t.Fatal("session slot leaked")
			}
			t.Logf("%s: generation 2 on b; segments 0..5 fetched once with 200; old/current claims zero after Stop", mode)
			if err = originalFront.ReleaseOutput(ctx, q.SessionID, record.Fence); err != nil {
				t.Fatal("output leak", err)
			}
		})
	}
}
