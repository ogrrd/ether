package plextv

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPresenceStreamAndEvents(t *testing.T) {
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sub/eventsource/42/machine-1" || r.URL.Query().Get("X-Plex-Token") != "srv-token" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": \n\n: -1\n\n")
		fl.Flush()
		fmt.Fprint(w, `data: <Message address="1.2.3.4" port="32400" asyncIdentifier="abc" connectivity="1" command="notifyConnectivity"/>`+"\n\n")
		fl.Flush()
		<-r.Context().Done()
	}))
	defer sse.Close()
	host := strings.TrimPrefix(sse.URL, "http://")
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/pubsub/servers" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<Servers size="1"><Server host=%q region="yyz"/></Servers>`, host)
	}))
	defer tv.Close()
	c, err := New(Options{BaseURL: tv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan PubsubMessage, 1)
	p := NewPresence(PresenceOptions{
		Client:       c,
		Credentials:  func() (int64, string, string, bool) { return 42, "machine-1", "srv-token", true },
		OnMessage:    func(m PubsubMessage) { got <- m },
		StreamClient: sse.Client(),
		StreamURL: func(h string, owner int64, machine, token string) string {
			return fmt.Sprintf("http://%s/sub/eventsource/%d/%s?X-Plex-Token=%s", h, owner, machine, token)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case m := <-got:
		if m.Command != "notifyConnectivity" || m.AsyncIdentifier != "abc" || !m.Reachable() {
			t.Fatalf("message %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	if st := p.Status(); !st.Connected || st.Host != host {
		t.Fatalf("status %+v", st)
	}
	cancel()
	<-done
}

func TestRegistrarTrustsOnlyOutstandingConnectivityAnswer(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	r := &Registrar{o: RegistrarOptions{Now: time.Now}, next: map[string]time.Time{}, wake: make(chan struct{}, 1), log: logger}
	r.asyncID = "want"
	r.onPubsub(PubsubMessage{Command: "notifyConnectivity", AsyncIdentifier: "other", Connectivity: "1"})
	if r.st.PlexReachable != nil {
		t.Fatal("accepted an answer to someone else's request")
	}
	r.onPubsub(PubsubMessage{Command: "notifyConnectivity", AsyncIdentifier: "want", Connectivity: "0", Address: "203.0.113.7", Port: "32400"})
	if r.st.PlexReachable == nil || *r.st.PlexReachable {
		t.Fatalf("reachable = %v", r.st.PlexReachable)
	}
	logged := logBuf.String()
	for _, want := range []string{"direction=inbound", "address=203.0.113.7", "port=32400", "reachable=false"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("connectivity log %q missing %q", logged, want)
		}
	}
}

func TestRegistrarFriendSyncIntervalAndSharingHomePubsub(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.OwnerPlexID = 42
	cfg.Plex.Claimed = true

	var mu sync.Mutex
	now := time.Now()
	users := &fakeUsers{}
	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg},
		Client: newServerClient(t, f),
		Users:  users,
		WAN:    &fakeWAN{},
		Mapper: &fakeMapper{},
		LAN:    func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
		Now:    func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.o.FriendSyncInterval != 30*time.Minute {
		t.Fatalf("default FriendSyncInterval = %v, want 30m", r.o.FriendSyncInterval)
	}

	ctx := context.Background()
	r.runDue(ctx)
	initialSyncs := f.count("GET /api/v2/server/access_tokens")
	if initialSyncs != 1 {
		t.Fatalf("initial access_tokens fetches = %d, want 1", initialSyncs)
	}

	// After 5 minutes (well below the 30-minute default), runDue must not re-sync.
	mu.Lock()
	now = now.Add(5 * time.Minute)
	mu.Unlock()
	r.runDue(ctx)
	if got := f.count("GET /api/v2/server/access_tokens"); got != initialSyncs {
		t.Fatalf("access_tokens fetches after 5m = %d, want %d", got, initialSyncs)
	}

	// Both notifySharingChange and notifyHomeChange schedule an immediate friend sync.
	for i, cmd := range []string{"notifySharingChange", "notifyHomeChange"} {
		r.onPubsub(PubsubMessage{Command: cmd})
		r.runDue(ctx)
		want := initialSyncs + i + 1
		if got := f.count("GET /api/v2/server/access_tokens"); got != want {
			t.Fatalf("%s: access_tokens fetches = %d, want %d", cmd, got, want)
		}
	}

	// Rate-limited unknown-token sync runs immediately once, then suppresses
	// back-to-back calls within UnknownTokenSyncMinSpacing.
	before := f.count("GET /api/v2/server/access_tokens")
	if err := r.SyncFriendsOnUnknownToken(ctx); err != nil {
		t.Fatalf("SyncFriendsOnUnknownToken: %v", err)
	}
	if got := f.count("GET /api/v2/server/access_tokens"); got != before+1 {
		t.Fatalf("unknown-token sync fetches = %d, want %d", got, before+1)
	}
	if err := r.SyncFriendsOnUnknownToken(ctx); err != nil {
		t.Fatalf("rate-limited SyncFriendsOnUnknownToken: %v", err)
	}
	if got := f.count("GET /api/v2/server/access_tokens"); got != before+1 {
		t.Fatalf("rate-limited unknown-token sync should not fetch again, got %d", got)
	}
	mu.Lock()
	now = now.Add(20 * time.Second)
	mu.Unlock()
	if err := r.SyncFriendsOnUnknownToken(ctx); err != nil {
		t.Fatalf("post-window SyncFriendsOnUnknownToken: %v", err)
	}
	if got := f.count("GET /api/v2/server/access_tokens"); got != before+2 {
		t.Fatalf("post-window unknown-token sync fetches = %d, want %d", got, before+2)
	}
}

func TestRegistrarNeverPublishesCGNATOrJunkCustomURLs(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.OwnerPlexID = 42
	cfg.Plex.Claimed = true
	cfg.Plex.RemoteAccess = true
	cfg.Plex.CustomURLs = []string{
		"http://100.88.1.9:32400",
		"https://plex.example.com:8443",
		"[http://192.168.1.5:32400](http://192.168.1.5:32400/)",
	}

	r, err := NewRegistrar(RegistrarOptions{
		Config: &memConfig{cfg: cfg},
		Client: newServerClient(t, f),
		WAN:    &fakeWAN{ip: "100.64.0.9"},
		Mapper: &fakeMapper{},
		LAN: func() ([]net.IP, error) {
			return []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("100.100.1.2")}, nil
		},
		PeerURIs: func() []string {
			return []string{"http://100.77.1.3:32400", "http://192.168.1.6:32400"}
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.PublishNow(context.Background()); err != nil {
		t.Fatalf("PublishNow: %v", err)
	}

	wantPublished := []string{
		"http://192.168.1.5:32400",
		"https://plex.example.com:8443",
		"http://192.168.1.6:32400",
	}
	if !slices.Equal(f.published, wantPublished) {
		t.Fatalf("published URIs = %v, want %v", f.published, wantPublished)
	}
	if len(f.registered) == 0 {
		t.Fatal("expected servers.xml registration")
	}
	for _, forbidden := range []string{"100.100.1.2", "100.88.1.9", "100.77.1.3", "100.64.0.9"} {
		if strings.Contains(f.registered[0], forbidden) {
			t.Fatalf("servers.xml leaked CGNAT address %s: %s", forbidden, f.registered[0])
		}
	}

	// CGNAT addresses remain in Status().Connections as local entries so local/Tailscale use keeps working.
	st := r.Status()
	hasLocalTailscaleLAN := false
	hasLocalTailscaleCustom := false
	for _, c := range st.Connections {
		if c.URI == "http://100.100.1.2:32400" && c.Local {
			hasLocalTailscaleLAN = true
		}
		if c.URI == "http://100.88.1.9:32400" && c.Local && c.Custom {
			hasLocalTailscaleCustom = true
		}
	}
	if !hasLocalTailscaleLAN || !hasLocalTailscaleCustom {
		t.Fatalf("expected CGNAT LAN and custom URLs to remain locally usable in Connections, got %+v", st.Connections)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
