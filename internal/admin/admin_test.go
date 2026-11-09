package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

const ownerToken = "owner-secret-token"

func newStore(t *testing.T, mutate func(*config.Config)) *config.Store {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		if err := store.Update(func(c *config.Config) error { mutate(c); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func seededConfig(c *config.Config) {
	c.Plex.Token = ownerToken
	c.Plex.Claimed = true
	c.Plex.OwnerUsername = "alice"
	c.Usenet.Providers = []config.UsenetProvider{{
		ID: "p1", Name: "Main", Host: "news.example.com", Port: 563, TLS: true,
		Username: "u1", Password: "pw-one", Connections: 20, Enabled: true,
	}, {
		ID: "p2", Name: "Block", Host: "block.example.com", Port: 563, TLS: true,
		Username: "u2", Password: "pw-two", Connections: 5, Backfill: true, Enabled: true,
	}}
	c.Indexers = []config.Indexer{{ID: "i1", Name: "Idx", URL: "https://idx.example.com", APIKey: "ix-key", Enabled: true}}
	c.Catalog.TMDBKey = "tmdb-key"
	c.LLM.APIKey = "llm-key"
	c.OpenSubtitles.APIKey = "os-key"
	c.OpenSubtitles.Password = "os-pw"
	c.Trakt.ClientSecret = "trakt-secret"
}

// storedOwner is an IsOwnerToken that accepts the owner token stored in
// config, standing in for internal/auth.
func storedOwner(store *config.Store) func(context.Context, string) bool {
	return func(_ context.Context, tok string) bool {
		stored := store.Get().Plex.Token
		return stored != "" && tok == stored
	}
}

// req builds a request as the SPA would: LAN peer, CSRF header, JSON.
func req(method, target, body string) *http.Request {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rdr)
	r.RemoteAddr = "192.168.1.50:49152"
	r.Host = "192.168.1.10:32400"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Ether-Request", "1")
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func TestRedactionRoundTrip(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})

	r := req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, secret := range []string{ownerToken, "pw-one", "pw-two", "ix-key", "tmdb-key", "llm-key", "os-key", "os-pw", "trakt-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked in GET config", secret)
		}
	}
	if !strings.Contains(body, admin.RedactedSecret) {
		t.Fatal("expected redaction placeholder in GET config")
	}

	// Send it back verbatim (envelope + placeholders), providers reordered.
	var env struct {
		Config config.Config `json:"config"`
	}
	decode(t, rec, &env)
	env.Config.Usenet.Providers[0], env.Config.Usenet.Providers[1] = env.Config.Usenet.Providers[1], env.Config.Usenet.Providers[0]
	env.Config.Server.FriendlyName = "Living Room"
	raw, _ := json.Marshal(env)
	r2 := req(http.MethodPut, "/ether/api/config", string(raw))
	r2.Header.Set("X-Plex-Token", ownerToken)
	rec2 := serve(h, r2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec2.Code, rec2.Body)
	}
	got := store.Get()
	if got.Server.FriendlyName != "Living Room" {
		t.Fatalf("friendlyName not saved: %q", got.Server.FriendlyName)
	}
	if got.Plex.Token != ownerToken || got.Catalog.TMDBKey != "tmdb-key" || got.Indexers[0].APIKey != "ix-key" {
		t.Fatal("scalar secret not preserved")
	}
	// Reordering must not swap passwords (matched by ID).
	if got.Usenet.Providers[0].ID != "p2" || got.Usenet.Providers[0].Password != "pw-two" {
		t.Fatalf("provider order/password mismatch: %+v", got.Usenet.Providers[0])
	}
	if got.Usenet.Providers[1].Password != "pw-one" {
		t.Fatalf("provider password swapped: %+v", got.Usenet.Providers[1])
	}
	// A genuinely new secret replaces the stored one.
	got.Indexers[0].APIKey = "fresh-key"
	if err := store.Update(func(c *config.Config) error { *c = got; return nil }); err != nil {
		t.Fatal(err)
	}
	if store.Get().Indexers[0].APIKey != "fresh-key" {
		t.Fatal("new secret not stored")
	}
}

func TestAuthMatrix(t *testing.T) {
	setupStore := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = false })
	doneStore := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })

	cases := []struct {
		name       string
		store      *config.Store
		remoteAddr string
		token      string
		want       int
	}{
		{"pre-setup LAN no token", setupStore, "10.0.0.5:1", "", 200},
		{"pre-setup localhost no token", setupStore, "127.0.0.1:1", "", 200},
		{"pre-setup WAN no token", setupStore, "203.0.113.9:1", "", 403},
		{"pre-setup WAN owner token", setupStore, "203.0.113.9:1", ownerToken, 200},
		{"post-setup LAN no token", doneStore, "10.0.0.5:1", "", 401},
		{"post-setup wrong token", doneStore, "10.0.0.5:1", "nope", 401},
		{"post-setup owner token", doneStore, "203.0.113.9:1", ownerToken, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := admin.NewHandler(admin.Deps{Config: tc.store, IsOwnerToken: storedOwner(tc.store)})
			r := req(http.MethodGet, "/ether/api/setup/status", "")
			r.RemoteAddr = tc.remoteAddr
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if rec := serve(h, r); rec.Code != tc.want {
				t.Fatalf("got %d want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}

	// IsOwnerToken alone decides; the stored token is not consulted.
	h := admin.NewHandler(admin.Deps{Config: doneStore, IsOwnerToken: func(_ context.Context, tok string) bool {
		return tok == "custom-owner"
	}})
	r := req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", "custom-owner")
	if rec := serve(h, r); rec.Code != 200 {
		t.Fatalf("custom owner token rejected: %d", rec.Code)
	}
	r = req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	if rec := serve(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stored token accepted without IsOwnerToken: %d", rec.Code)
	}
}

func TestCSRFAllowsLiveTVFileUpload(t *testing.T) {
	store := newStore(t, seededConfig)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	post := func(path, origin, contentType string, header bool) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("payload"))
		r.RemoteAddr = "192.168.1.50:49152"
		r.Host = "192.168.1.10:32400"
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("X-Plex-Token", ownerToken)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if header {
			r.Header.Set("X-Ether-Request", "1")
		}
		return serve(h, r).Code
	}
	same := "http://192.168.1.10:32400"
	guide := "/ether/api/livetv/sources/abc/guide"
	if code := post(guide, same, "application/octet-stream", true); code != http.StatusNotFound {
		t.Fatalf("guide upload csrf: got %d want 404 after the check", code)
	}
	playlist := "/ether/api/livetv/sources/abc/playlist"
	if code := post(playlist, same, "application/octet-stream", true); code != http.StatusNotFound {
		t.Fatalf("playlist upload csrf: got %d want 404 after the check", code)
	}
	if code := post(guide, "http://evil.example", "application/octet-stream", true); code != http.StatusForbidden {
		t.Fatalf("cross-origin upload: got %d want 403", code)
	}
	if code := post(guide, same, "application/octet-stream", false); code != http.StatusForbidden {
		t.Fatalf("upload without csrf header: got %d want 403", code)
	}
	for _, path := range []string{"/ether/api/plugins/install", "/ether/api/plugins/install/preview"} {
		if code := post(path, same, "application/octet-stream", true); code != http.StatusNotFound {
			t.Fatalf("plugin upload csrf: got %d", code)
		}
		if code := post(path, "http://evil.example", "application/octet-stream", true); code != http.StatusForbidden {
			t.Fatalf("cross-origin plugin upload: got %d", code)
		}
		if code := post(path, same, "application/octet-stream", false); code != http.StatusForbidden {
			t.Fatalf("plugin upload without header: got %d", code)
		}
	}
	refresh := "/ether/api/livetv/sources/abc/refresh"
	if code := post(refresh, same, "application/octet-stream", true); code != http.StatusForbidden {
		t.Fatalf("octet-stream refresh: got %d want 403", code)
	}
	csvPreview := "/ether/api/adoption/migration/csv/preview?viewer=1&format=trakt&kind=history"
	if code := post(csvPreview, same, "text/csv; charset=utf-8", true); code != http.StatusNotFound {
		t.Fatalf("csv preview upload csrf: got %d want 404 after the check", code)
	}
	if code := post(csvPreview, "http://evil.example", "text/csv", true); code != http.StatusForbidden {
		t.Fatalf("cross-origin csv preview: got %d want 403", code)
	}
	if code := post(csvPreview, same, "text/csv", false); code != http.StatusForbidden {
		t.Fatalf("csv preview without csrf header: got %d want 403", code)
	}
}

func TestCSRFHeaderRequiredForMutations(t *testing.T) {
	store := newStore(t, seededConfig)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	r := req(http.MethodPost, "/ether/api/setup/complete", `{"force":true}`)
	r.Header.Del("X-Ether-Request")
	if rec := serve(h, r); rec.Code != http.StatusForbidden {
		t.Fatalf("mutation without X-Ether-Request: got %d want 403", rec.Code)
	}
	if store.Get().SetupComplete {
		t.Fatal("CSRF-blocked request still mutated state")
	}
}

func TestSetupStatusAndGating(t *testing.T) {
	store := newStore(t, func(c *config.Config) {
		c.Usenet.Providers = []config.UsenetProvider{{ID: "p1", Host: "n", Port: 563, Enabled: true}}
		// no plex claim, no indexer → required steps incomplete
	})
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})

	rec := serve(h, req(http.MethodGet, "/ether/api/setup/status", ""))
	var st struct {
		SetupComplete    bool              `json:"setupComplete"`
		RequiredComplete bool              `json:"requiredComplete"`
		Steps            []admin.SetupStep `json:"steps"`
	}
	decode(t, rec, &st)
	if st.RequiredComplete || len(st.Steps) != 6 {
		t.Fatalf("bad setup status: %+v", st)
	}

	rec = serve(h, req(http.MethodPost, "/ether/api/setup/complete", `{}`))
	if rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "force") {
		t.Fatalf("complete with missing steps: got %d want 409 without API wording (%s)", rec.Code, rec.Body)
	}
	// Forcing never finishes an unclaimed server: nobody could sign in.
	rec = serve(h, req(http.MethodPost, "/ether/api/setup/complete", `{"force":true}`))
	if rec.Code != http.StatusConflict || store.Get().SetupComplete || !strings.Contains(rec.Body.String(), "plex_required") {
		t.Fatalf("forced complete without a Plex owner: %d %s", rec.Code, rec.Body)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Plex.Token, c.Plex.Claimed = "owner-token", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec = serve(h, req(http.MethodPost, "/ether/api/setup/complete", `{"force":true}`))
	if rec.Code != 200 || !store.Get().SetupComplete {
		t.Fatalf("forced complete failed: %d", rec.Code)
	}
	rec = serve(h, req(http.MethodPost, "/ether/api/setup/reopen", `{}`))
	// Now setup IS complete and no owner token is configured → reopen is
	// intentionally locked out (fail closed), proving post-setup auth.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reopen without token after setup: got %d want 401", rec.Code)
	}
}

func TestStremioAddonCountsAsSource(t *testing.T) {
	store := newStore(t, func(c *config.Config) {
		c.Plex.Token, c.Plex.Claimed = "owner-token", true
		c.Indexers = []config.Indexer{{
			ID: "addon", Name: "Torrentio", URL: "https://torrentio.strem.fun/manifest.json",
			Kind: config.IndexerKindStremio, Protocol: "torrent", Enabled: true,
		}}
		c.Catalog.TMDBKey = "bundled-test-key"
		c.Cache.Dir = t.TempDir()
	})
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	rec := serve(h, req(http.MethodGet, "/ether/api/setup/status", ""))
	var st struct {
		RequiredComplete bool              `json:"requiredComplete"`
		Steps            []admin.SetupStep `json:"steps"`
	}
	decode(t, rec, &st)
	var sources admin.SetupStep
	for _, step := range st.Steps {
		if step.ID == "sources" {
			sources = step
		}
	}
	if !sources.Complete || !st.RequiredComplete {
		t.Fatalf("keyless addon did not satisfy sources: %+v required=%v", sources, st.RequiredComplete)
	}
}

func TestSetupReportsCacheRestartUntilSavedDirectoryIsActive(t *testing.T) {
	store := newStore(t, seededConfig)
	activeDir := store.Get().Cache.Dir
	h := admin.NewHandler(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store),
		CacheRestartRequired: func() bool { return store.Get().Cache.Dir != activeDir },
	})
	selectedDir := t.TempDir()
	if err := store.Update(func(c *config.Config) error {
		c.Cache.Dir = selectedDir
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := serve(h, req(http.MethodPost, "/ether/api/setup/complete", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("complete setup: %d %s", rec.Code, rec.Body)
	}
	var status struct {
		SetupComplete        bool `json:"setupComplete"`
		CacheRestartRequired bool `json:"cacheRestartRequired"`
	}
	decode(t, rec, &status)
	if !status.SetupComplete || !status.CacheRestartRequired {
		t.Fatalf("setup must preserve the pending restart: %+v", status)
	}
	for _, restarted := range []bool{false, true} {
		if restarted {
			activeDir = selectedDir
		}
		r := req(http.MethodGet, "/ether/api/setup/status", "")
		r.Header.Set("X-Plex-Token", ownerToken)
		rec = serve(h, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup status: %d %s", rec.Code, rec.Body)
		}
		decode(t, rec, &status)
		if !status.SetupComplete || status.CacheRestartRequired == restarted {
			t.Fatalf("restarted=%v: unexpected setup status %+v", restarted, status)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	store := newStore(t, nil)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	body := `{"port":70000,"friendlyName":""}`
	rec := serve(h, req(http.MethodPatch, "/ether/api/config/server", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid section: got %d (%s)", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code   string             `json:"code"`
			Fields []admin.FieldError `json:"fields"`
		} `json:"error"`
	}
	decode(t, rec, &e)
	if e.Error.Code != "validation" || len(e.Error.Fields) < 2 {
		t.Fatalf("bad validation payload: %+v", e.Error)
	}
	if store.Get().Server.Port == 70000 {
		t.Fatal("invalid config was saved")
	}
	// Unknown section.
	rec = serve(h, req(http.MethodGet, "/ether/api/config/nope", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown section: got %d", rec.Code)
	}
}

func TestSectionPatchPreservesUnsetAndSecrets(t *testing.T) {
	store := newStore(t, seededConfig)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	rec := serve(h, req(http.MethodPatch, "/ether/api/config/catalog", `{"minVotes":42,"tmdbKey":"`+admin.RedactedSecret+`"}`))
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	got := store.Get()
	if got.Catalog.MinVotes != 42 {
		t.Fatalf("minVotes not patched: %d", got.Catalog.MinVotes)
	}
	if got.Catalog.TMDBKey != "tmdb-key" {
		t.Fatal("patched secret placeholder overwrote stored key")
	}
	if got.Catalog.Language != "en-US" {
		t.Fatal("unspecified field was blanked by patch")
	}
}

type fakePlex struct {
	polled   bool
	claimErr error
	synced   int
	syncErr  error
}

func (f *fakePlex) StartPIN(context.Context) (string, string, string, error) {
	return "pin-1", "ABCD", "https://plex.tv/claim", nil
}
func (f *fakePlex) PollPIN(_ context.Context, id string) (bool, error) {
	f.polled = true
	return id == "pin-1", nil
}
func (f *fakePlex) Claim(context.Context) error { return f.claimErr }
func (f *fakePlex) Status() any                 { return map[string]any{"claimed": true} }
func (f *fakePlex) SyncFriends(context.Context) error {
	f.synced++
	return f.syncErr
}

func TestPlexPINFlow(t *testing.T) {
	store := newStore(t, nil)
	fp := &fakePlex{}
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), PlexAuth: fp})
	rec := serve(h, req(http.MethodPost, "/ether/api/plex/pin", `{}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ABCD"`) {
		t.Fatalf("pin start: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/plex/pin/pin-1", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("pin poll: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/plex/status", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"detail":{"claimed":true}`) {
		t.Fatalf("plex status: %d %s", rec.Code, rec.Body)
	}
	fp.claimErr = &plextv.ClaimExchangeError{Err: errors.New("422 unprocessable")}
	rec = serve(h, req(http.MethodPost, "/ether/api/plex/claim", `{}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Plex couldn't link this server. Try again in a minute.") {
		t.Fatalf("claim exchange failure: %d %s", rec.Code, rec.Body)
	}

	// Friend sync rejects while unclaimed, then succeeds once claimed.
	rec = serve(h, req(http.MethodPost, "/ether/api/plex/friends/sync", `{}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("unclaimed friend sync: %d %s", rec.Code, rec.Body)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Plex.Claimed = true
		c.Plex.Token = "server-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refreshed := 0
	h = admin.NewHandler(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store), PlexAuth: fp,
		RefreshViewers: func(context.Context) error { refreshed++; return nil },
	})
	rec = serve(h, req(http.MethodPost, "/ether/api/plex/friends/sync", `{}`))
	if rec.Code != http.StatusOK || fp.synced != 1 || refreshed != 1 {
		t.Fatalf("claimed friend sync: code=%d synced=%d refreshed=%d body=%s", rec.Code, fp.synced, refreshed, rec.Body)
	}
	fp.syncErr = errors.New("plex.tv timeout")
	rec = serve(h, req(http.MethodPost, "/ether/api/plex/friends/sync", `{}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed friend sync: %d %s", rec.Code, rec.Body)
	}
}

func TestTestersUnredactAndReport(t *testing.T) {
	store := newStore(t, seededConfig)
	var gotPassword string
	h := admin.NewHandler(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store),
		TestUsenet: func(_ context.Context, p config.UsenetProvider) error {
			gotPassword = p.Password
			return errors.New("connection refused")
		},
		TestTMDB: func(_ context.Context, key string) error {
			if key != "tmdb-key" {
				return errors.New("stored key not used for placeholder")
			}
			return nil
		},
	})
	body := `{"id":"p1","name":"Main","host":"news.example.com","port":563,"tls":true,"username":"u1","password":"` + admin.RedactedSecret + `","connections":20,"enabled":true}`
	rec := serve(h, req(http.MethodPost, "/ether/api/test/usenet", body))
	if rec.Code != 200 {
		t.Fatalf("test usenet: %d %s", rec.Code, rec.Body)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decode(t, rec, &res)
	if res.OK || res.Error != "connection refused" {
		t.Fatalf("failed test should be ok:false with error: %+v", res)
	}
	if gotPassword != "pw-one" {
		t.Fatalf("tester received placeholder instead of stored password: %q", gotPassword)
	}
	rec = serve(h, req(http.MethodPost, "/ether/api/test/tmdb", `{"key":"`+admin.RedactedSecret+`"}`))
	decode(t, rec, &res)
	if !res.OK {
		t.Fatalf("tmdb test with stored key: %+v", res)
	}
}

type fakeUsers struct {
	mu    sync.Mutex
	users []core.User
}

func (f *fakeUsers) Users(context.Context) ([]core.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.User(nil), f.users...), nil
}
func (f *fakeUsers) UpdateUser(_ context.Context, u core.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.users {
		if f.users[i].ID == u.ID {
			f.users[i] = u
			return nil
		}
	}
	return core.ErrNotFound
}

func TestUsersEndpoints(t *testing.T) {
	store := newStore(t, nil)
	us := &fakeUsers{users: []core.User{{ID: 7, Username: "kid", Title: "Kid", SubtitleLanguages: []string{"fr"}}}}
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Users: us})

	rec := serve(h, req(http.MethodPatch, "/ether/api/users/7", `{"streamLimit":1,"maxContentRating":"PG","subtitleLanguages":["en"]}`))
	if rec.Code != 200 {
		t.Fatalf("update user: %d %s", rec.Code, rec.Body)
	}
	got := us.users[0]
	if got.StreamLimit != 1 || got.MaxContentRating != "PG" || got.Username != "kid" {
		t.Fatalf("user not merged/updated: %+v", got)
	}
	// Languages are owned by the Audio & subtitles rules, not this endpoint.
	if len(got.SubtitleLanguages) != 1 || got.SubtitleLanguages[0] != "fr" {
		t.Fatalf("user languages written through the limits endpoint: %+v", got.SubtitleLanguages)
	}
	if rec := serve(h, req(http.MethodPatch, "/ether/api/users/7", `{"maxContentRating":"bogus"}`)); rec.Code != 400 {
		t.Fatalf("unknown rating: got %d want 400", rec.Code)
	}
	if rec := serve(h, req(http.MethodPatch, "/ether/api/users/99", `{"streamLimit":1}`)); rec.Code != 404 {
		t.Fatalf("missing user: got %d want 404", rec.Code)
	}
	if rec := serve(h, req(http.MethodPatch, "/ether/api/users/7", `{"streamLimit":-2}`)); rec.Code != 400 {
		t.Fatalf("negative limit: got %d want 400", rec.Code)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/users", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"streamLimit":1`) {
		t.Fatalf("list users: %d %s", rec.Code, rec.Body)
	}
}

func TestDashboardDiskAndLogs(t *testing.T) {
	store := newStore(t, nil)
	buf := admin.NewLogBuffer(10)
	log := slog.New(admin.TeeHandler(slog.NewTextHandler(io.Discard, nil), buf))
	log.Info("server started", "port", 32400)
	log.Warn("cache nearly full")

	h := admin.NewHandler(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store), Logs: buf,
		CacheStatus: func(context.Context) any { return map[string]any{"used": 40, "budget": 100} },
		ExtraStatus: map[string]admin.StatusFunc{
			"boom":   func(context.Context) any { panic("kaput") },
			"widget": func(context.Context) any { return map[string]any{"streams": 2} },
		},
		ResolverTraces: func(context.Context) (any, error) { return []string{"trace-1"}, nil },
		DiskUsage:      func(string) (int64, int64, error) { return 1000, 250, nil },
	})
	rec := serve(h, req(http.MethodGet, "/ether/api/dashboard", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"budget":100`) {
		t.Fatalf("dashboard: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "kaput") {
		t.Fatal("panicking status provider was not contained")
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/status", ""))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"streams":2}` {
		t.Fatalf("widget status: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/traces", ""))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"traces":["trace-1"]}` {
		t.Fatalf("traces: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/disk?path=/tmp/cache", ""))
	var disk struct {
		TotalBytes int64 `json:"totalBytes"`
		FreeBytes  int64 `json:"freeBytes"`
	}
	decode(t, rec, &disk)
	if disk.TotalBytes != 1000 || disk.FreeBytes != 250 {
		t.Fatalf("disk: %+v", disk)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/logs?q=cache", ""))
	if !strings.Contains(rec.Body.String(), "cache nearly full") || strings.Contains(rec.Body.String(), "server started") {
		t.Fatalf("log filter failed: %s", rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/logs?level=warn", ""))
	if strings.Contains(rec.Body.String(), "server started") {
		t.Fatalf("level filter failed: %s", rec.Body)
	}
}

func TestSPAFallbackAndAssets(t *testing.T) {
	store := newStore(t, func(c *config.Config) { c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	// Deep client route with no token: SPA shell is public so the login
	// prompt can render; API data stays gated.
	rec := serve(h, req(http.MethodGet, "/ether/settings/providers", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("SPA fallback: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/unknown", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown API post-setup without token: got %d want 401", rec.Code)
	}
	// Bootstrap is public and secret-free.
	rec = serve(h, req(http.MethodGet, "/ether/api/bootstrap", ""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"setupComplete":true`) {
		t.Fatalf("bootstrap: %d %s", rec.Code, rec.Body)
	}
}

func TestLogBufferRingAndConcurrency(t *testing.T) {
	buf := admin.NewLogBuffer(50)
	log := slog.New(buf.Handler())
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				log.Info("tick", "i", i)
			}
		}()
	}
	wg.Wait()
	if buf.Len() != 50 {
		t.Fatalf("ring should cap at 50, got %d", buf.Len())
	}
	entries := buf.Entries(5, "", "")
	if len(entries) != 5 {
		t.Fatalf("limit: got %d", len(entries))
	}
	withGroup := slog.New(buf.Handler()).WithGroup("resolver").With("title", "Dune")
	withGroup.Info("picked release")
	found := buf.Entries(0, "", "resolver.title=dune")
	if len(found) != 1 {
		t.Fatalf("group attr flattening failed: %v", buf.Entries(3, "", ""))
	}
}

func TestArraySectionPut(t *testing.T) {
	store := newStore(t, seededConfig)
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	body := `[{"id":"i1","name":"Idx","url":"https://idx.example.com","apiKey":"` + admin.RedactedSecret + `","dailyLimit":100,"dailyGrabs":0,"priority":0,"enabled":true,"movieCategories":null,"tvCategories":null},{"id":"i2","name":"Two","url":"https://two.example.com","apiKey":"k2","dailyLimit":0,"dailyGrabs":0,"priority":1,"enabled":true,"movieCategories":null,"tvCategories":null}]`
	rec := serve(h, req(http.MethodPut, "/ether/api/config/indexers", body))
	if rec.Code != 200 {
		t.Fatalf("PUT indexers section: %d %s", rec.Code, rec.Body)
	}
	got := store.Get()
	if len(got.Indexers) != 2 || got.Indexers[0].APIKey != "ix-key" || got.Indexers[0].DailyLimit != 100 || got.Indexers[1].APIKey != "k2" {
		t.Fatalf("array section not saved/unredacted: %+v", got.Indexers)
	}
	// GET section round-trips redacted.
	rec = serve(h, req(http.MethodGet, "/ether/api/config/indexers", ""))
	if strings.Contains(rec.Body.String(), "ix-key") || !strings.Contains(rec.Body.String(), admin.RedactedSecret) {
		t.Fatalf("section GET leaks or lacks placeholder: %s", rec.Body)
	}
}

func TestValidateConfigDirect(t *testing.T) {
	good := config.Defaults(t.TempDir())
	if errs := admin.ValidateConfig(good); len(errs) != 0 {
		t.Fatalf("defaults should validate: %v", errs)
	}
	good.Quality.Default = "missing"
	good.Indexers = []config.Indexer{{URL: "ftp://x"}}
	if errs := admin.ValidateConfig(good); len(errs) < 2 {
		t.Fatalf("expected >=2 errors, got %v", errs)
	}
}

// The owner's same-quality version count is 0 (none) to 3.
func TestValidateSameQualityAlternatives(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	for _, n := range []int{0, 1, 2, 3} {
		cfg.Resolver.SameQualityAlternatives = n
		if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
			t.Errorf("%d rejected: %v", n, errs)
		}
	}
	for _, n := range []int{-1, 4} {
		cfg.Resolver.SameQualityAlternatives = n
		errs := admin.ValidateConfig(cfg)
		if len(errs) != 1 || errs[0].Field != "resolver.sameQualityAlternatives" {
			t.Errorf("%d: errs=%v", n, errs)
		}
	}
}

func TestValidateClusterWeight(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	for _, w := range []int{0, 1, 100} {
		cfg.Server.ClusterWeight = w
		if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
			t.Errorf("weight %d rejected: %v", w, errs)
		}
	}
	for _, w := range []int{-1, 101} {
		cfg.Server.ClusterWeight = w
		errs := admin.ValidateConfig(cfg)
		if len(errs) != 1 || errs[0].Field != "server.clusterWeight" {
			t.Errorf("weight %d: errs=%v", w, errs)
		}
	}
}

func TestValidateLibraryNames(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	cfg.Server.MoviesLibraryName = "  "
	cfg.Server.TVShowsLibraryName = ""
	errs := admin.ValidateConfig(cfg)
	fields := map[string]bool{}
	for _, e := range errs {
		fields[e.Field] = true
	}
	if !fields["server.moviesLibraryName"] || !fields["server.tvShowsLibraryName"] {
		t.Fatalf("expected library name required errors, got %v", errs)
	}
}

func TestValidateMaintenanceWindow(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	cfg.Updates.MaintenanceStart = "23:30"
	cfg.Updates.MaintenanceEnd = "" // empty falls back to the default
	if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("valid window rejected: %v", errs)
	}
	for _, bad := range []string{"3am", "24:00", "3:00", "03:60"} {
		cfg.Updates.MaintenanceEnd = bad
		errs := admin.ValidateConfig(cfg)
		if len(errs) != 1 || errs[0].Field != "updates.maintenanceEnd" {
			t.Errorf("%q: errs=%v", bad, errs)
		}
	}
}

func TestDatabaseSettingsRedactedAndNotEditable(t *testing.T) {
	const dbURL = "postgres://ether:db-secret@db.invalid:5432/ether"
	store := newStore(t, func(c *config.Config) {
		seededConfig(c)
		c.SetupComplete = true
		c.Database = config.Database{Driver: config.DatabasePostgres, URL: dbURL}
	})
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	r := req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec := serve(h, r)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "db-secret") {
		t.Fatalf("GET config leaked the database password: %d %s", rec.Code, rec.Body)
	}
	// A client that drops or rewrites the database section changes nothing.
	var env struct {
		Config config.Config `json:"config"`
	}
	decode(t, rec, &env)
	env.Config.Database = config.Database{Driver: config.DatabaseSQLite}
	raw, _ := json.Marshal(env)
	r2 := req(http.MethodPut, "/ether/api/config", string(raw))
	r2.Header.Set("X-Plex-Token", ownerToken)
	if rec2 := serve(h, r2); rec2.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec2.Code, rec2.Body)
	}
	if got := store.Get().Database; got.Driver != config.DatabasePostgres || got.URL != dbURL {
		t.Fatalf("database settings changed through the admin API: %+v", got.Redacted())
	}
}

func TestValidateLLMProvider(t *testing.T) {
	fields := func(l config.LLM) map[string]bool {
		cfg := config.Defaults(t.TempDir())
		cfg.LLM = l
		out := map[string]bool{}
		for _, e := range admin.ValidateConfig(cfg) {
			out[e.Field] = true
		}
		return out
	}
	if f := fields(config.LLM{Enabled: true, Provider: "anthropic", Model: "m", APIKey: "k"}); len(f) != 0 {
		t.Fatalf("hosted provider needs no endpoint: %v", f)
	}
	if f := fields(config.LLM{Enabled: true, Provider: "openai", Model: "m"}); !f["llm.apiKey"] {
		t.Fatalf("hosted provider without key: %v", f)
	}
	if f := fields(config.LLM{Enabled: true, Provider: "ollama", Model: "m"}); len(f) != 0 {
		t.Fatalf("ollama uses its default endpoint: %v", f)
	}
	if f := fields(config.LLM{Enabled: true, Provider: "custom", Model: "m"}); !f["llm.baseUrl"] {
		t.Fatalf("custom needs an endpoint: %v", f)
	}
	if f := fields(config.LLM{Enabled: true, Provider: "lmstudio"}); !f["llm.model"] {
		t.Fatalf("model required: %v", f)
	}
	if f := fields(config.LLM{Provider: "bogus"}); !f["llm.provider"] {
		t.Fatalf("unknown provider accepted: %v", f)
	}
}

func TestLLMProvidersAndModels(t *testing.T) {
	store := newStore(t, seededConfig)
	var got config.LLM
	h := admin.NewHandler(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store),
		LLMModels: func(_ context.Context, c config.LLM) ([]admin.LLMModel, error) {
			got = c
			if c.Provider == "groq" {
				return nil, errors.New("401: invalid key")
			}
			return []admin.LLMModel{{ID: "claude-x", Label: "Claude X"}}, nil
		},
	})
	auth := func(r *http.Request) *http.Request { r.Header.Set("X-Plex-Token", ownerToken); return r }

	rec := serve(h, auth(req(http.MethodGet, "/ether/api/llm/providers", "")))
	var providers struct {
		Providers []config.LLMProvider `json:"providers"`
	}
	decode(t, rec, &providers)
	if rec.Code != 200 || len(providers.Providers) != len(config.LLMProviders) {
		t.Fatalf("providers: %d %s", rec.Code, rec.Body)
	}

	var res struct {
		Models []admin.LLMModel `json:"models"`
		Error  string           `json:"error"`
	}
	// The stored key belongs to the stored provider: switching provider
	// with the placeholder must not send it to the new one.
	rec = serve(h, auth(req(http.MethodPost, "/ether/api/llm/models", `{"provider":"anthropic","apiKey":"`+admin.RedactedSecret+`"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "secret_required") || got.APIKey != "" {
		t.Fatalf("placeholder followed a provider change: %d %s %+v", rec.Code, rec.Body, got)
	}
	if err := store.Update(func(c *config.Config) error { c.LLM.Provider = "anthropic"; return nil }); err != nil {
		t.Fatal(err)
	}
	rec = serve(h, auth(req(http.MethodPost, "/ether/api/llm/models", `{"provider":"anthropic","apiKey":"`+admin.RedactedSecret+`"}`)))
	decode(t, rec, &res)
	if rec.Code != 200 || len(res.Models) != 1 || res.Models[0].Label != "Claude X" {
		t.Fatalf("models: %d %s", rec.Code, rec.Body)
	}
	if got.APIKey != "llm-key" || got.Provider != "anthropic" {
		t.Fatalf("placeholder key not restored: %+v", got)
	}
	rec = serve(h, auth(req(http.MethodPost, "/ether/api/llm/models", `{"provider":"groq","apiKey":"bad"}`)))
	res.Models, res.Error = nil, ""
	decode(t, rec, &res)
	if rec.Code != 200 || res.Error == "" || res.Models == nil {
		t.Fatalf("provider failure should be 200 with an error and empty list: %d %s", rec.Code, rec.Body)
	}
}

func TestSystemLoadRoute(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	auth := func(r *http.Request) *http.Request { r.Header.Set("X-Plex-Token", ownerToken); return r }
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	if rec := serve(h, auth(req(http.MethodGet, "/ether/api/system/load", ""))); rec.Code != http.StatusNotImplemented {
		t.Fatalf("without a sampler: %d", rec.Code)
	}
	h = admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store),
		SystemLoad: func(context.Context) any { return map[string]any{"cores": 8} }})
	if rec := serve(h, req(http.MethodGet, "/ether/api/system/load", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("load must be owner-only: %d", rec.Code)
	}
	rec := serve(h, auth(req(http.MethodGet, "/ether/api/system/load", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cores":8`) {
		t.Fatalf("load: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(serve(h, auth(req(http.MethodGet, "/ether/api/dashboard", ""))).Body.String(), `"cores"`) {
		t.Fatal("dashboard should not carry system load")
	}
}

// Reopening the wizard, or saving a config document loaded before setup
// finished, must not bring back the tokenless local-network access of a
// fresh install: any LAN device could then reconfigure and re-claim the
// server.
func TestReopenKeepsOwnerCredentialRequired(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	owner := func(r *http.Request) *http.Request { r.Header.Set("X-Plex-Token", ownerToken); return r }

	if rec := serve(h, owner(req(http.MethodPost, "/ether/api/setup/reopen", `{}`))); rec.Code != http.StatusOK {
		t.Fatalf("owner reopen: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/config", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless LAN config read after reopen: got %d want 401", rec.Code)
	}

	// A stale document (setupComplete:false) saved by the owner.
	rec := serve(h, owner(req(http.MethodGet, "/ether/api/config", "")))
	var env struct {
		Config config.Config `json:"config"`
	}
	decode(t, rec, &env)
	env.Config.SetupComplete = false
	raw, _ := json.Marshal(env)
	if rec := serve(h, owner(req(http.MethodPut, "/ether/api/config", string(raw)))); rec.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body)
	}
	if !store.Get().SetupComplete {
		t.Fatal("PUT config cleared setupComplete")
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/config", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless LAN config read after stale save: got %d want 401", rec.Code)
	}
}

// Before setup, tokenless LAN access is limited to requests addressed to a
// local name: a public domain re-pointed at the server's LAN address (DNS
// rebinding) must not reach the setup API from the owner's browser.
func TestSetupRejectsRebindingHost(t *testing.T) {
	store := newStore(t, seededConfig) // setup not complete
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	for host, want := range map[string]int{
		"attacker.example.com:32400": http.StatusForbidden,
		"rebind.evil.net":            http.StatusForbidden,
		"192.168.1.10:32400":         http.StatusOK,
		"[fe80::1]:32400":            http.StatusOK,
		"localhost:32400":            http.StatusOK,
		"nas:32400":                  http.StatusOK,
		"nas.local:32400":            http.StatusOK,
		"tower.home.arpa":            http.StatusOK,
	} {
		r := req(http.MethodGet, "/ether/api/config", "")
		r.Host = host
		if rec := serve(h, r); rec.Code != want {
			t.Errorf("Host %s: got %d want %d (%s)", host, rec.Code, want, rec.Body)
		}
	}
	r := req(http.MethodGet, "/ether/api/bootstrap", "")
	r.Host = "attacker.example.com:32400"
	if rec := serve(h, r); strings.Contains(rec.Body.String(), `"authenticated":true`) {
		t.Fatalf("bootstrap reports a rebinding host as authenticated: %s", rec.Body)
	}
	// The owner's credential works under any name.
	r = req(http.MethodGet, "/ether/api/config", "")
	r.Host = "attacker.example.com:32400"
	r.Header.Set("X-Plex-Token", ownerToken)
	if rec := serve(h, r); rec.Code != http.StatusOK {
		t.Fatalf("owner token under a public name: %d", rec.Code)
	}
}

func TestSharedCacheSectionEditsEffectivePolicy(t *testing.T) {
	nasDir := t.TempDir()
	s := newStore(t, func(c *config.Config) {
		seededConfig(c)
		c.Cache.Storage = "shared"
		c.Cache.Dir = nasDir
		p := c.Cache
		p.Dir = ""
		p.Storage = ""
		p.BudgetBytes = 100 << 30
		c.SharedCache = &p
	})
	h := admin.NewHandler(admin.Deps{Config: s, IsOwnerToken: storedOwner(s)})
	rec := serve(h, req(http.MethodPatch, "/ether/api/config/cache", `{"retentionDays":21}`))
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	got := s.Get()
	if got.SharedCache.RetentionDays != 21 || got.SharedCache.BudgetBytes != 100<<30 || got.SharedCache.Dir != "" || got.Cache.Dir != nasDir {
		t.Fatalf("bad shared policy: %+v", got.SharedCache)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/config/cache", ""))
	if !strings.Contains(rec.Body.String(), `"budgetBytes":107374182400`) {
		t.Fatalf("GET did not expose effective policy: %s", rec.Body)
	}
	rec = serve(h, req(http.MethodPatch, "/ether/api/config/cache", `{"storage":"local","budgetBytes":1024}`))
	if rec.Code != 200 {
		t.Fatalf("local: %d %s", rec.Code, rec.Body)
	}
	if s.Get().SharedCache.BudgetBytes != 100<<30 {
		t.Fatal("leaving shared storage changed the pool budget")
	}
}

func TestSimklSecretRedactionBinding(t *testing.T) {
	prev := config.Config{Simkl: config.Simkl{ClientID: "app-one", ClientSecret: "simkl-secret"}}
	redacted := admin.RedactConfig(prev)
	if redacted.Simkl.ClientSecret != admin.RedactedSecret {
		t.Fatal("Simkl secret was not redacted")
	}
	restored, errs := admin.UnredactConfig(redacted, prev)
	if len(errs) != 0 || restored.Simkl.ClientSecret != "simkl-secret" {
		t.Fatalf("restore failed: %+v", errs)
	}
	redacted.Simkl.ClientID = "other-app"
	restored, errs = admin.UnredactConfig(redacted, prev)
	if len(errs) == 0 || restored.Simkl.ClientSecret != "" {
		t.Fatal("secret followed changed client ID")
	}
}

func TestExtensionRootKeepsConsolePrefixAndQuery(t *testing.T) {
	store := newStore(t, seededConfig)
	handler := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Extensions: map[string]http.Handler{"extend/registry": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.URL.Query().Get("kinds") != "page" {
			t.Errorf("wrong resource: %s", r.URL)
		}
		w.WriteHeader(http.StatusOK)
	})}})
	for _, suffix := range []string{"", "/"} {
		req := httptest.NewRequest(http.MethodGet, "/api/extend/registry"+suffix+"?kinds=page", nil)
		req.Header.Set("X-Ether-Admin-Token", ownerToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Header().Get("Location") != "" {
			t.Fatalf("subtree root: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
}

func TestExtensionKeepsEscapedPathBehindConsolePrefix(t *testing.T) {
	store := newStore(t, seededConfig)
	handler := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Extensions: map[string]http.Handler{"plugins": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/studio:community.page/views/overview" {
			t.Errorf("wrong resource: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})}})
	req := httptest.NewRequest(http.MethodGet, "/ether/api/plugins/studio%3Acommunity.page/views/overview", nil)
	req.Header.Set("X-Ether-Admin-Token", ownerToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("encoded plugin path: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigEditsPreserveClaimWhenOmitted(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	get := req(http.MethodGet, "/ether/api/config", "")
	get.Header.Set("X-Plex-Token", ownerToken)
	response := serve(h, get)
	var doc map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	cfg := doc["config"].(map[string]any)
	delete(cfg["plex"].(map[string]any), "claimed")
	body, _ := json.Marshal(doc)
	put := req(http.MethodPut, "/ether/api/config", string(body))
	put.Header.Set("X-Plex-Token", ownerToken)
	if res := serve(h, put); res.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", res.Code, res.Body)
	}
	patch := req(http.MethodPatch, "/ether/api/config/plex", `{"remoteAccess":false}`)
	patch.Header.Set("X-Plex-Token", ownerToken)
	if res := serve(h, patch); res.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", res.Code, res.Body)
	}
	if got := store.Get().Plex; !got.Claimed || got.Token != ownerToken {
		t.Fatal("config edits lost claim or token")
	}
}

func TestConfigRejectsNullClaim(t *testing.T) {
	for _, tc := range []struct{ name, method, path, payload string }{
		{"section null claim", http.MethodPatch, "/ether/api/config/plex", `{"claimed":null}`},
		{"section null object", http.MethodPut, "/ether/api/config/plex", `null`},
		{"section invalid", http.MethodPatch, "/ether/api/config/plex", `{"claimed":"false"}`},
		{"section alias", http.MethodPatch, "/ether/api/config/plex", `{"Claimed":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
			h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
			r := req(tc.method, tc.path, tc.payload)
			r.Header.Set("X-Plex-Token", ownerToken)
			if res := serve(h, r); res.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", res.Code, res.Body)
			}
			if !store.Get().Plex.Claimed {
				t.Fatal("claim changed")
			}
		})
	}
	for _, envelope := range []bool{false, true} {
		t.Run(fmt.Sprintf("full envelope=%v", envelope), func(t *testing.T) {
			store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
			h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
			get := req(http.MethodGet, "/ether/api/config", "")
			get.Header.Set("X-Plex-Token", ownerToken)
			var doc map[string]any
			if err := json.Unmarshal(serve(h, get).Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			cfg := doc["config"].(map[string]any)
			cfg["plex"].(map[string]any)["claimed"] = nil
			var payload any = cfg
			if envelope {
				payload = doc
			}
			b, _ := json.Marshal(payload)
			put := req(http.MethodPut, "/ether/api/config", string(b))
			put.Header.Set("X-Plex-Token", ownerToken)
			if res := serve(h, put); res.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", res.Code, res.Body)
			}
			if !store.Get().Plex.Claimed {
				t.Fatal("claim changed")
			}
		})
	}
}

func TestExplicitClaimChangeRequiresOwner(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	bad := req(http.MethodPatch, "/ether/api/config/plex", `{"claimed":false}`)
	if res := serve(h, bad); res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", res.Code)
	}
	if !store.Get().Plex.Claimed {
		t.Fatal("unauthenticated request changed claim")
	}
	good := req(http.MethodPatch, "/ether/api/config/plex", `{"claimed":false}`)
	good.Header.Set("X-Plex-Token", ownerToken)
	if res := serve(h, good); res.Code != http.StatusOK {
		t.Fatalf("owner status %d: %s", res.Code, res.Body)
	}
	if store.Get().Plex.Claimed {
		t.Fatal("explicit owner unclaim ignored")
	}
}
