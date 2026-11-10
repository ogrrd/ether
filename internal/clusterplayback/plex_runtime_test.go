package clusterplayback

import (
	"context"
	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type composedAuth struct {
	mu      sync.RWMutex
	revoked bool
}

func (a *composedAuth) Authenticate(_ context.Context, token string) (*core.User, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if token == "offline-foreign" && !a.revoked {
		return &core.User{ID: 10, Username: "foreign"}, nil
	}
	if token != "offline-account" || a.revoked {
		return nil, core.ErrUnauthorized
	}
	return &core.User{ID: 9, Username: "offline", IsOwner: true}, nil
}
func (a *composedAuth) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	if id != 9 {
		return nil, core.ErrUnauthorized
	}
	return a.Authenticate(ctx, "offline-account")
}
func (a *composedAuth) Users(ctx context.Context) ([]core.User, error) {
	u, e := a.Authenticate(ctx, "offline-account")
	if e != nil {
		return nil, nil
	}
	return []core.User{*u}, nil
}
func (a *composedAuth) UpdateUser(context.Context, core.User) error { return nil }
func (a *composedAuth) Owner(ctx context.Context) (*core.User, error) {
	return a.Authenticate(ctx, "offline-account")
}

type composedState struct{ core.State }

func (composedState) Get(context.Context, int64, core.RatingKey) (core.WatchStatus, error) {
	return core.WatchStatus{}, nil
}
func (composedState) GetMany(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	return map[core.RatingKey]core.WatchStatus{}, nil
}
func (composedState) Progress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) error {
	return nil
}

type composedPolicy struct{ wan, hidden atomic.Bool }

func (p *composedPolicy) Allowed(*core.User, *core.Title) bool { return !p.hidden.Load() }
func (p *composedPolicy) CanPlay(_ *core.User, remote bool, _ time.Time) bool {
	return !remote || !p.wan.Load()
}

type forwardOnlyEncoder struct{ r *Runtime }

func (e forwardOnlyEncoder) StartSegmentedHLS(context.Context, *core.User, string, string, core.HLSRequest) (core.HLSSession, error) {
	return nil, ErrDenied
}
func (e forwardOnlyEncoder) SegmentedHLSSession(string) (core.HLSSession, bool) { return nil, false }
func (e forwardOnlyEncoder) StopSegmentedHLS(string) bool                       { return false }
func (e forwardOnlyEncoder) ForwardPlexMedia(w http.ResponseWriter, q *http.Request, id string) bool {
	return e.r.ForwardPlexMedia(w, q, id)
}
func (e forwardOnlyEncoder) ForwardPlexGrantMedia(w http.ResponseWriter, q *http.Request, id string) bool {
	return e.r.ForwardPlexGrantMedia(w, q, id)
}

func TestRealPlexRuntimeDecisionStartSeekAcrossFrontends(t *testing.T) {
	sourceID := nodewire.Identity{Cluster: "plex-composition", NodeID: "source", Boot: "source-boot"}
	workerID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "worker", Boot: "worker-boot"}
	ownerID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "owner", Boot: "owner-boot"}
	senderID := nodewire.Identity{Cluster: sourceID.Cluster, NodeID: "sender", Boot: "sender-boot"}
	guards := fixtureGuards(t, sourceID, workerID, ownerID, senderID)
	fixtureStarted := time.Now()
	db := openFixtureDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), fixtureExecutionBudget(fixtureStarted, 15*time.Second))
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
	eligible := func(context.Context, nodewire.Identity, config.NodeRole) error { return nil }
	auth := &composedAuth{}
	policy := &composedPolicy{}
	base := Options{DB: db, Placement: ledger, Endpoint: endpoint, Eligible: eligible, MemberRole: eligible, Capacity: 32, ClockBoundKnown: true, ClockSkew: time.Millisecond, Decide: func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
		return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts", AudioStream: -1, SubtitleStream: -1}
	}}
	base.MediaServers = func(context.Context) ([]nodewire.Identity, error) { return []nodewire.Identity{senderID}, nil }
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
	for i, r := range []*Runtime{source, worker, owner, sender} {
		server := httptest.NewUnstartedServer(r.Handler())
		server.TLS = guards[i].ServerTLS()
		server.StartTLS()
		defer server.Close()
		endpoints[r.o.Self] = server.URL
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
	hub := clusterbus.NewHub()
	ownerNode, senderNode := hub.Node(ownerID.NodeID), hub.Node(senderID.NodeID)
	broker, err := sessions.NewBroker(sessions.BrokerDeps{DB: db, Cluster: ownerNode, Playback: remote, Transcoder: decision, DistributedHLS: owner, Events: events, ActiveViewer: auth.ActiveUser, CanPlay: policy.CanPlay, BitrateLimit: func(*core.User) int { return 0 }, Progress: composedState{}, OnTimeline: func(*core.User, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(c context.Context, u *core.User, d core.Device, k core.RatingKey) (*core.Title, error) {
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
	deps := plex.Deps{SessionStores: sessions.NewSessionStores(), Experience: experience.NewSelectors(), DB: db, Cluster: ownerNode, Catalog: fixtureCatalog{}, State: composedState{}, Auth: auth, AccessPolicy: policy, Playback: remote, Transcoder: decision, Encoder: broker, DisableEnrichment: true, Config: func() config.Config { return cfg }}
	publicOwner := plex.NewServer(deps)
	defer publicOwner.Close()
	publicOwner.UsePlaybackBroker(broker)
	if err = owner.BindPlexDelivery(publicOwner); err != nil {
		t.Fatal(err)
	}
	remoteSender, err := NewRemotePlayback(sender, RemotePlaybackOptions{Estimate: func(context.Context, *core.Title, core.ClientCaps) core.MediaInfo { return core.MediaInfo{} }, SelectSource: func(context.Context) (nodewire.Identity, error) { return sourceID, nil }, Capacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	senderDecision, err := sender.DecisionTranscoder()
	if err != nil {
		t.Fatal(err)
	}
	senderDeps := brokerTestDeps(remoteSender, senderDecision, sender, events, auth, policy)
	senderDeps.DB, senderDeps.Cluster = db, senderNode
	senderBroker, err := sessions.NewBroker(senderDeps)
	if err != nil {
		t.Fatal(err)
	}
	defer senderBroker.Close(ctx)
	deps.Playback, deps.Transcoder, deps.Encoder = remoteSender, senderDecision, senderBroker
	deps.Cluster = senderNode
	publicSender := plex.NewServer(deps)
	defer publicSender.Close()
	publicSender.UsePlaybackBroker(senderBroker)
	if err = sender.BindPlexDelivery(publicSender); err != nil {
		t.Fatal(err)
	}
	forwardDeps := deps
	forwardDeps.DB, forwardDeps.Cluster = nil, nil
	forwardDeps.Playback, forwardDeps.Transcoder, forwardDeps.Encoder = remote, decision, forwardOnlyEncoder{r: owner}
	publicForward := plex.NewServer(forwardDeps)
	defer publicForward.Close()
	startURI := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F" + core.MovieKey(1).String() + "&session=real-composed-plex&directPlay=0&directStream=1"
	start := httptest.NewRequest("GET", "https://public"+startURI, nil)
	start.RemoteAddr = "192.168.1.9:1234"
	start.Header.Set("X-Plex-Token", "offline-account")
	start.Header.Set("X-Plex-Client-Identifier", "composed-client")
	unarmed := httptest.NewRecorder()
	publicOwner.ServeHTTP(unarmed, start.Clone(ctx))
	if unarmed.Code != http.StatusBadRequest || len(remote.Sessions()) != 0 {
		t.Fatalf("unarmed Plex start status %d, admitted sessions %d", unarmed.Code, len(remote.Sessions()))
	}
	// PMS requires a decision for this player and item before HLS start.
	decisionRequest := httptest.NewRequest("GET", "https://public"+strings.Replace(startURI, "start.m3u8", "decision", 1), nil)
	decisionRequest.RemoteAddr = start.RemoteAddr
	decisionRequest.Header = start.Header.Clone()
	decisionResponse := httptest.NewRecorder()
	publicSender.ServeHTTP(decisionResponse, decisionRequest)
	if decisionResponse.Code != http.StatusOK {
		t.Fatalf("real Plex decision status %d: %s", decisionResponse.Code, decisionResponse.Body.String())
	}
	master := httptest.NewRecorder()
	publicOwner.ServeHTTP(master, start)
	if master.Code != 200 {
		t.Fatalf("real Plex start status %d: %s", master.Code, master.Body.String())
	}
	// A seek decision may return to the first frontend while the other owns
	// healthy playback. It must reuse the exact preparation without moving it.
	seek := decisionRequest.Clone(ctx)
	seek.URL, _ = url.Parse(decisionRequest.URL.String() + "&offset=30&maxVideoBitrate=1000")
	seekResponse := httptest.NewRecorder()
	publicSender.ServeHTTP(seekResponse, seek)
	if seekResponse.Code != 200 || !strings.Contains(seekResponse.Body.String(), `generalDecisionCode="1001"`) {
		t.Fatalf("seek decision status %d: %s", seekResponse.Code, seekResponse.Body.String())
	}
	if len(manager.Sessions()) != 1 {
		t.Fatal("frontends opened independent source lifetimes")
	}
	// Sealing the decision frontend must not retire the source now owned by
	// the starting frontend, or publish a source-retired receipt prematurely.
	decisionPlay := remoteSender.Sessions()[0]
	remoteSender.Stop(decisionPlay.ID)
	_ = sender.Tick(ctx)
	if _, ok := manager.Session(source.preparations[senderID.NodeID+"/"+senderID.Boot+"/"+remoteSender.entries[decisionPlay.ID].operation].result.PlayID); !ok {
		t.Fatal("decision retirement detached the shared source")
	}
	select {
	case <-remoteSender.lifetime(decisionPlay.ID).Done():
		t.Fatal("premature shared retirement receipt")
	default:
	}
	var media string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			media = strings.TrimSpace(line)
			break
		}
	}
	baseURL, _ := url.Parse("https://public" + startURI)
	part, e := url.Parse(media)
	if e != nil || media == "" {
		t.Fatal("real Plex master omitted media")
	}
	uri := baseURL.ResolveReference(part)
	if !strings.Contains(uri.RawQuery, "X-Ether-Stream-Token=") || !strings.Contains(uri.RawQuery, "X-Ether-Read-Token=") {
		t.Fatal("master omitted genuine dual grant envelope")
	}
	// Replay to the decision frontend, whose local binding predates the start
	// on the other server. The durable started binding must win.
	q := httptest.NewRequest("GET", uri.String(), nil)
	q.RemoteAddr = start.RemoteAddr
	q.Header = start.Header.Clone()
	variant := httptest.NewRecorder()
	if !owner.ForwardPlexMedia(variant, q, "real-composed-plex") {
		t.Fatal("variant was not forwarded")
	}
	if variant.Code != 200 {
		t.Fatalf("replayed variant status %d", variant.Code)
	}
	segmentURI := *uri
	segmentURI.Path = strings.TrimSuffix(segmentURI.Path, "index.m3u8") + "segment-0.ts"
	for _, target := range []*url.URL{uri, &segmentURI} {
		for _, test := range []struct {
			name, account string
			invalid       bool
			status        int
		}{
			{"stream grant", "", false, 200},
			{"foreign viewer", "offline-foreign", false, 403},
			{"invalid capabilities", "", true, 401},
		} {
			t.Run(test.name+"/"+target.Path[strings.LastIndex(target.Path, "/")+1:], func(t *testing.T) {
				address := *target
				if test.invalid {
					query := address.Query()
					query.Set("X-Ether-Stream-Token", "invalid")
					query.Set("X-Ether-Read-Token", "invalid")
					address.RawQuery = query.Encode()
				}
				q := httptest.NewRequest("GET", address.String(), nil)
				q.RemoteAddr = start.RemoteAddr
				q.Header.Set("X-Plex-Client-Identifier", "composed-client")
				if test.account != "" {
					q.Header.Set("X-Plex-Token", test.account)
				}
				out := httptest.NewRecorder()
				publicForward.ServeHTTP(out, q)
				if out.Code != test.status {
					t.Fatalf("replayed media status %d, want %d", out.Code, test.status)
				}
			})
		}
	}
	uri.Path = strings.TrimSuffix(uri.Path, "index.m3u8") + "0.ts"
	request := func(method, peer, xff string) *httptest.ResponseRecorder {
		q := httptest.NewRequest(method, "https://public"+uri.RequestURI(), nil)
		q.RemoteAddr = peer
		if xff != "" {
			q.Header.Set("X-Forwarded-For", xff)
		}
		q.Header.Set("Range", "bytes=1-3")
		q.Header.Set(deliveryOrigin, `{"RemoteAddr":"127.0.0.1:1","Host":"private","Scheme":"http"}`)
		out := httptest.NewRecorder()
		publicForward.ServeHTTP(out, q)
		return out
	}
	for _, method := range []string{"GET", "HEAD"} {
		out := request(method, "192.168.1.9:1234", "")
		if out.Code != 206 || out.Header().Get("Content-Range") != "bytes 1-3/7" || method == "GET" && out.Body.String() != "egm" || method == "HEAD" && out.Body.Len() != 0 {
			t.Fatalf("composed Plex %s status %d bytes %d", method, out.Code, out.Body.Len())
		}
	}

	fallbackURI := *uri
	query := fallbackURI.Query()
	query.Set("X-Ether-Stream-Token", "wrong")
	fallbackURI.RawQuery = query.Encode()
	for _, test := range []struct {
		device string
		status int
	}{{"composed-client", 206}, {"other-client", 401}, {"", 401}} {
		q := httptest.NewRequest("GET", "https://public"+fallbackURI.RequestURI(), nil)
		q.RemoteAddr = "192.168.1.9:1234"
		q.Header.Set("Range", "bytes=1-3")
		if test.device != "" {
			q.Header.Set("X-Plex-Client-Identifier", test.device)
		}
		out := httptest.NewRecorder()
		publicForward.ServeHTTP(out, q)
		if out.Code != test.status {
			t.Fatalf("real Plex readcap fallback status %d expected %d", out.Code, test.status)
		}
	}
	policy.wan.Store(true)
	for _, test := range []struct {
		peer, xff string
		allowed   bool
	}{{"203.0.113.8:1234", "", false}, {"10.1.0.2:1234", "203.0.113.8", false}, {"10.1.0.2:1234", "192.168.1.9", true}, {"203.0.113.8:1234", "192.168.1.9", false}} {
		out := request("GET", test.peer, test.xff)
		if test.allowed && out.Code != 206 || !test.allowed && out.Code != 401 && out.Code != 403 {
			t.Fatalf("composed original network policy status %d", out.Code)
		}
	}
	policy.wan.Store(false)
	policy.hidden.Store(true)
	if out := request("GET", "192.168.1.9:1234", ""); out.Code != 403 {
		t.Fatalf("current hidden title status %d", out.Code)
	}
	policy.hidden.Store(false)
	auth.mu.Lock()
	auth.revoked = true
	auth.mu.Unlock()
	if out := request("GET", "192.168.1.9:1234", ""); out.Code != 401 {
		t.Fatalf("current revoked viewer status %d", out.Code)
	}
	auth.mu.Lock()
	auth.revoked = false
	auth.mu.Unlock()
	if len(sender.executions) != 0 {
		t.Fatal("sender synthesized an encoder")
	}
	stopRequest := httptest.NewRequest("GET", "https://public/video/:/transcode/universal/stop?session=real-composed-plex", nil)
	stopRequest.RemoteAddr, stopRequest.Header = start.RemoteAddr, start.Header.Clone()
	stopResponse := httptest.NewRecorder()
	publicSender.ServeHTTP(stopResponse, stopRequest)
	if stopResponse.Code != 200 {
		t.Fatalf("cluster stop status %d", stopResponse.Code)
	}
	for i := 0; i < 50; i++ {
		_ = owner.Tick(ctx)
		_ = sender.Tick(ctx)
		worker.Cleanup()
		source.Cleanup()
		record, e := ledger.Lookup(ctx, "real-composed-plex")
		if e == nil && record.Retired {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("composed Plex retirement stayed pending")
}

func brokerTestDeps(remote *RemotePlayback, decision core.Transcoder, owner *Runtime, events *sessions.EventBus, auth *composedAuth, policy *composedPolicy) sessions.BrokerDeps {
	return sessions.BrokerDeps{Playback: remote, Transcoder: decision, DistributedHLS: owner, Events: events, ActiveViewer: auth.ActiveUser, CanPlay: policy.CanPlay, BitrateLimit: func(*core.User) int { return 0 }, Progress: composedState{}, OnTimeline: func(*core.User, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(c context.Context, u *core.User, d core.Device, k core.RatingKey) (*core.Title, error) {
		return fixtureCatalog{}.Get(c, k)
	}}
}
