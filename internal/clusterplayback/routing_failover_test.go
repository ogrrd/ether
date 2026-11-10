package clusterplayback

import (
	"context"
	"fmt"
	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/sessions"

	"testing"
	"time"
)

func TestDeliveryAndControllerLossKeepExecution(t *testing.T) {
	fixtureStarted := time.Now()
	sourceID := nodewire.Identity{Cluster: "plex-composition", NodeID: "source", Boot: "source-boot"}
	workerID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "worker", Boot: "worker-boot"}
	ownerID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "owner", Boot: "owner-boot"}
	senderID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "sender", Boot: "sender-boot"}
	deliveryIDs := []nodewire.Identity{{Cluster: sourceID.Cluster, NodeID: "delivery-a", Boot: "delivery-a-boot"}, {Cluster: sourceID.Cluster, NodeID: "delivery-b", Boot: "delivery-b-boot"}}
	guards := fixtureGuards(t, sourceID, workerID, ownerID, senderID, deliveryIDs[0], deliveryIDs[1])
	db := openFixtureDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), fixtureExecutionBudget(fixtureStarted, 45*time.Second))
	defer cancel()
	verifier := NewVerifier()
	ledger, err := placement.New(db, placement.Options{MaxSessions: 32, MaxLease: 10 * time.Minute, LockTimeout: fixtureSQLBudget()}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	encoder := &fixtureEncoder{}
	manager, err := playback.New(playback.Deps{Catalog: fixtureCatalog{}, Resolver: fixtureResolver{}, Engine: fixtureEngine{}, Transcoder: encoder, SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile { return core.QualityProfile{} }}, config.Defaults(t.TempDir()), playback.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	access, err := playback.NewSourceAccess(manager, 32, 32)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := map[nodewire.Identity]string{}
	endpoint := func(_ context.Context, id nodewire.Identity) (string, error) {
		ep, ok := endpoints[id]
		if !ok {
			return "", ErrDenied
		}
		return ep, nil
	}
	var ownerGone atomic.Bool
	eligible := func(_ context.Context, id nodewire.Identity, _ config.NodeRole) error {
		if id == ownerID && ownerGone.Load() {
			return ErrDenied
		}
		return nil
	}
	var timelineProgress atomic.Int64
	auth := &composedAuth{}
	policy := &composedPolicy{}
	base := Options{DB: db, Placement: ledger, Endpoint: endpoint, Eligible: eligible, MemberRole: eligible, Capacity: 32, ClockBoundKnown: true, ClockSkew: time.Millisecond, Decide: func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
		return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts", AudioStream: -1, SubtitleStream: -1}
	}}
	sourceOpts := base
	sourceOpts.Self = sourceID
	sourceOpts.Guard = guards[0]
	sourceOpts.Roles = config.NodeRoles{config.RoleSource}
	sourceOpts.Playback = manager
	sourceOpts.SourceAccess = access
	sourceOpts.ActiveViewer = auth.ActiveUser
	sourceOpts.SourcePolicy = func(context.Context, *core.User, *core.PlaySession) error { return nil }
	source, err := NewRuntime(sourceOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	workerOpts := base
	workerOpts.Self = workerID
	workerOpts.Guard = guards[1]
	workerOpts.Roles = config.NodeRoles{config.RoleTranscode}
	workerOpts.Encoder = encoder
	worker, err := NewRuntime(workerOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ownerOpts := base
	ownerOpts.Self = ownerID
	ownerOpts.Guard = guards[2]
	ownerOpts.Roles = config.NodeRoles{config.RoleServer, config.RoleDelivery}
	owner, err := NewRuntime(ownerOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = verifier.Bind(owner); err != nil {
		t.Fatal(err)
	}
	senderOpts := base
	senderOpts.Self = senderID
	senderOpts.Guard = guards[3]
	senderOpts.Roles = config.NodeRoles{config.RoleServer, config.RoleDelivery}
	sender, err := NewRuntime(senderOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	var ownerPrivate *httptest.Server
	for i, r := range []*Runtime{source, worker, owner, sender} {
		server := httptest.NewUnstartedServer(r.Handler())
		server.TLS = guards[i].ServerTLS()
		server.StartTLS()
		defer server.Close()
		endpoints[r.o.Self] = server.URL
		if r == owner {
			ownerPrivate = server
		}
	}
	if err = ledger.SetPool(ctx, "plex-worker", config.RoleTranscode, 1); err != nil {
		t.Fatal(err)
	}
	if err = ledger.SetTarget(ctx, placement.Target{NodeID: workerID.NodeID, Boot: workerID.Boot}, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"plex-worker"}}); err != nil {
		t.Fatal(err)
	}
	remote, err := NewRemotePlayback(owner, RemotePlaybackOptions{Estimate: func(context.Context, *core.Title, core.ClientCaps) core.MediaInfo { return core.MediaInfo{} }, SelectSource: func(context.Context) (nodewire.Identity, error) { return sourceID, nil }, Capacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := owner.DecisionTranscoder()
	if err != nil {
		t.Fatal(err)
	}
	events := sessions.NewEventBus(nil, auth.ActiveUser)
	defer events.Close()
	broker, err := sessions.NewBroker(sessions.BrokerDeps{Playback: remote, Transcoder: decision, DistributedHLS: owner, Events: events, ActiveViewer: auth.ActiveUser, CanPlay: policy.CanPlay, BitrateLimit: func(*core.User) int { return 0 }, Progress: composedState{}, OnTimeline: func(_ *core.User, _ core.RatingKey, position time.Duration, _ time.Duration, _ string) {
		timelineProgress.Store(int64(position))
	}, Lookup: func(c context.Context, u *core.User, d core.Device, k core.RatingKey) (*core.Title, error) {
		return fixtureCatalog{}.Get(c, k)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	cfg := config.Defaults(t.TempDir())
	cfg.Plex.Claimed = true
	cfg.Server.MachineIdentifier = "offline-machine"
	cfg.Server.TrustedProxyCIDRs = []string{"10.1.0.0/16"}
	deps := plex.Deps{SessionStores: sessions.NewSessionStores(), Experience: experience.NewSelectors(), Catalog: fixtureCatalog{}, State: composedState{}, Auth: auth, AccessPolicy: policy, Playback: remote, Transcoder: decision, Encoder: broker, DisableEnrichment: true, Config: func() config.Config { return cfg }}
	publicOwner := plex.NewServer(deps)
	defer publicOwner.Close()
	publicOwner.UsePlaybackBroker(broker)
	if err = owner.BindPlexDelivery(publicOwner); err != nil {
		t.Fatal(err)
	}
	survivorRemote, err := NewRemotePlayback(sender, RemotePlaybackOptions{Estimate: func(context.Context, *core.Title, core.ClientCaps) core.MediaInfo { return core.MediaInfo{} }, SelectSource: func(context.Context) (nodewire.Identity, error) { return sourceID, nil }, Capacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	survivorDecision, err := sender.DecisionTranscoder()
	if err != nil {
		t.Fatal(err)
	}
	survivorBroker, err := sessions.NewBroker(sessions.BrokerDeps{Playback: survivorRemote, Transcoder: survivorDecision, DistributedHLS: sender, Events: events, ActiveViewer: auth.ActiveUser, CanPlay: policy.CanPlay, BitrateLimit: func(*core.User) int { return 0 }, Progress: composedState{}, OnTimeline: func(_ *core.User, _ core.RatingKey, position time.Duration, _ time.Duration, _ string) {
		timelineProgress.Store(int64(position))
	}, Lookup: func(c context.Context, u *core.User, d core.Device, k core.RatingKey) (*core.Title, error) {
		return fixtureCatalog{}.Get(c, k)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer survivorBroker.Close(ctx)
	deps.Playback = survivorRemote
	deps.Transcoder = survivorDecision
	deps.Encoder = survivorBroker
	publicSender := plex.NewServer(deps)
	defer publicSender.Close()
	publicSender.UsePlaybackBroker(survivorBroker)
	if err = sender.BindPlexDelivery(publicSender); err != nil {
		t.Fatal(err)
	}
	startURI := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F" + core.MovieKey(1).String() + "&session=routing-loss&directPlay=0&directStream=1"
	start := httptest.NewRequest("GET", "https://public"+startURI, nil)
	start.Header.Set("X-Plex-Token", "offline-account")
	start.Header.Set("X-Plex-Client-Identifier", "composed-client")
	initialDecisionRequest := httptest.NewRequest("GET", "https://public"+strings.Replace(startURI, "start.m3u8", "decision", 1), nil)
	initialDecisionRequest.Header = start.Header.Clone()
	decisionResult := httptest.NewRecorder()
	publicOwner.ServeHTTP(decisionResult, initialDecisionRequest)
	if decisionResult.Code != 200 || strings.Contains(decisionResult.Body.String(), `generalDecisionCode="2000"`) {
		t.Fatalf("initial decision %d", decisionResult.Code)
	}
	master := httptest.NewRecorder()
	publicOwner.ServeHTTP(master, start)
	if master.Code != 200 {
		t.Fatalf("start %d %s", master.Code, master.Body.String())
	}
	var media string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			media = strings.TrimSpace(line)
			break
		}
	}
	baseURL, _ := url.Parse("https://public" + startURI)
	part, _ := url.Parse(media)
	uri := baseURL.ResolveReference(part)
	uri.Path = strings.TrimSuffix(uri.Path, "index.m3u8") + "0.ts"
	fetch := func(handler http.Handler, method, byteRange string) *httptest.ResponseRecorder {
		t.Helper()
		q := httptest.NewRequest(method, "https://public"+uri.RequestURI(), nil)
		if byteRange != "" {
			q.Header.Set("Range", byteRange)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, q)
		return out
	}
	baseline := fetch(publicSender, "GET", "")
	if baseline.Code != 200 {
		t.Fatalf("initial %d %s", baseline.Code, baseline.Body.String())
	}
	original, err := ledger.Lookup(ctx, "routing-loss")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("delivery", func(t *testing.T) {
		var senders []*httptest.Server
		for i, id := range deliveryIDs {
			options := base
			options.Self = id
			options.Guard = guards[i+4]
			options.Roles = config.NodeRoles{config.RoleDelivery}
			runtime, err := NewRuntime(options)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			handler, err := runtime.NewPublicDelivery(func(context.Context, *http.Request, string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			senders = append(senders, server)
		}
		began := time.Now()
		for i := 0; i < 8; i++ {
			target := senders[0]
			if i >= 4 {
				target = senders[1]
			}
			if i == 4 {
				senders[0].Close()
			}
			path := *uri
			path.Path = strings.TrimSuffix(uri.Path, "0.ts") + fmt.Sprintf("%d.ts", i)
			response, err := http.Get(target.URL + path.RequestURI())
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != baseline.Body.String() {
				t.Fatalf("delivery segment %d status %d %v", i, response.StatusCode, err)
			}
		}
		if elapsed := time.Since(began); elapsed > 2*time.Second {
			t.Fatalf("delivery sequence latency %v", elapsed)
		}
		t.Logf("eight segment indices fetched once; delivery-a stopped after index 3; latency %s", time.Since(began))
	})

	parity := map[string]*httptest.ResponseRecorder{}
	for _, method := range []string{"GET", "HEAD"} {
		for _, byteRange := range []string{"", "bytes=0-2", "bytes=999999-"} {
			parity[method+byteRange] = fetch(publicOwner, method, byteRange)
		}
	}
	t.Run("controller", func(t *testing.T) {
		// Stop the old controller's private HTTP listener, retaining healthy source
		// and encoder nodes. Wait for the real coordination lease to expire.
		ownerPrivate.Close()
		ownerGone.Store(true)
		began := time.Now()
		for !sender.ownsCoordination(ctx, "routing-loss") {
			if err = sender.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if time.Since(began) > 20*time.Second {
				t.Fatal("coordinator failed to take over within forward buffer")
			}
			if !sender.ownsCoordination(ctx, "routing-loss") {
				time.Sleep(100 * time.Millisecond)
			}
		}

		coordinator, err := sender.CurrentCoordinator(ctx, "routing-loss")
		if err != nil || coordinator.Identity != senderID || coordinator.Epoch != 2 {
			t.Fatalf("takeover %#v %v", coordinator, err)
		}
		assignments, assignmentErr := sender.Assignments(ctx)
		if assignmentErr != nil || len(assignments) != 1 || assignments[0].ControllerNodeID != senderID.NodeID || assignments[0].Generation != original.Generation || assignments[0].EncoderNodeID != original.Target.NodeID {
			t.Fatalf("console must follow coordinator takeover without moving encoding: %#v %v", assignments, assignmentErr)
		}
		for _, test := range []struct {
			method, byteRange string
			status            int
		}{{"GET", "", 200}, {"HEAD", "", 200}, {"GET", "bytes=0-2", 206}, {"GET", "bytes=999999-", 416}} {
			before := parity[test.method+test.byteRange]
			after := fetch(publicSender, test.method, test.byteRange)
			if after.Code != test.status || after.Code != before.Code || after.Body.String() != before.Body.String() || after.Header().Get("Content-Range") != before.Header().Get("Content-Range") {
				t.Fatalf("%s %s status %d/%d body %q/%q", test.method, test.byteRange, before.Code, after.Code, before.Body.String(), after.Body.String())
			}
		}
		sender.mu.Lock()
		renewed := sender.executions["routing-loss"].session.Lease()
		sender.mu.Unlock()
		if renewed.Version == 0 {
			t.Fatal("adopted encoder/source lease was not renewed")
		}
		timeline := httptest.NewRequest("GET", "https://public/:/timeline?ratingKey="+core.MovieKey(1).String()+"&state=playing&time=10000&duration=60000&session=routing-loss", nil)
		timeline.Header.Set("X-Plex-Token", "offline-account")
		timeline.Header.Set("X-Plex-Client-Identifier", "composed-client")
		timelineResult := httptest.NewRecorder()
		publicSender.ServeHTTP(timelineResult, timeline)
		if timelineProgress.Load() == 0 {
			t.Fatal("surviving timeline did not record progress")
		}
		if timelineResult.Code != 200 {
			t.Fatalf("surviving timeline %d", timelineResult.Code)
		}
		decisionRequest := httptest.NewRequest("GET", "https://public"+strings.ReplaceAll(strings.Replace(startURI, "start.m3u8", "decision", 1), "session=routing-loss", "session=next-api-decision"), nil)
		decisionRequest.Header.Set("X-Plex-Token", "offline-account")
		decisionRequest.Header.Set("X-Plex-Client-Identifier", "composed-client")
		decisionResponse := httptest.NewRecorder()
		publicSender.ServeHTTP(decisionResponse, decisionRequest)
		if decisionResponse.Code != 200 || strings.Contains(decisionResponse.Body.String(), `generalDecisionCode="2000"`) {
			t.Fatalf("surviving decision %d", decisionResponse.Code)
		}
		current, err := ledger.Lookup(ctx, "routing-loss")
		if err != nil || current.Fence != original.Fence {
			t.Fatal("healthy execution replaced", err)
		}
		encoder.mu.Lock()
		starts := encoder.starts
		encoder.mu.Unlock()
		if starts != 1 {
			t.Fatalf("encoder starts %d", starts)
		}
		if elapsed := time.Since(began); elapsed > 20*time.Second {
			t.Fatalf("takeover requests exceeded budget: %v", elapsed)
		}
		// The obsolete controller cannot mutate the worker after its epoch is lost.
		owner.mu.Lock()
		oldSession := owner.executions["routing-loss"].session
		owner.mu.Unlock()
		if oldSession.RetireEncoder() == nil {
			t.Fatal("stale controller stopped healthy encoder")
		}
		out := fetch(publicSender, "GET", "")
		if out.Code != 200 || out.Body.String() != baseline.Body.String() {
			t.Fatalf("after stale stop %d", out.Code)
		}
		t.Logf("takeover latency including range/head requests: %s; execution generation=%d; encoder starts=%d", time.Since(began), current.Generation, starts)
	})
}
