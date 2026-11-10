package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
)

const uncachedKey = "owner-debrid-key"

// uncachedDebrid is one fake debrid account served over HTTP in two
// dialects: Real-Debrid (/rd: no availability check, so a torrent is added
// and then inspected) and TorBox (/tb: a bulk instant check, and adds that
// can be restricted to cached torrents).
type uncachedDebrid struct {
	mu       sync.Mutex
	cached   map[string]bool   // hashes the service already holds
	torrents map[string]string // torrent id -> hash
	selected map[string]bool   // RD torrent id -> files selected
	refuse   string            // "", "infringing", "slots": how RD answers an add
	adds     []string          // hashes added to the account, in order
	next     int
}

func newUncachedDebrid() *uncachedDebrid {
	return &uncachedDebrid{cached: map[string]bool{}, torrents: map[string]string{}, selected: map[string]bool{}}
}

func (d *uncachedDebrid) finish(hash string) {
	d.mu.Lock()
	d.cached[hash] = true
	d.mu.Unlock()
}

func (d *uncachedDebrid) added() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.adds...)
}

func magnetHash(m string) string {
	_, h, _ := strings.Cut(m, "btih:")
	h, _, _ = strings.Cut(h, "&")
	return strings.ToLower(h)
}

const uncachedFiles = `[{"id":1,"path":"/Fjord.Harbor.1999/Fjord.Harbor.1999.1080p.mkv","bytes":4000000000,"selected":%d}]`

func (d *uncachedDebrid) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+uncachedKey {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	// Real-Debrid.
	mux.HandleFunc("POST /rd/torrents/addMagnet", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		d.mu.Lock()
		defer d.mu.Unlock()
		switch d.refuse {
		case "infringing":
			w.WriteHeader(451)
			fmt.Fprint(w, `{"error":"infringing_file","error_code":35}`)
			return
		case "slots":
			w.WriteHeader(509)
			fmt.Fprint(w, `{"error":"too_many_active_downloads","error_code":21}`)
			return
		}
		d.next++
		id := fmt.Sprintf("T%d", d.next)
		h := magnetHash(r.PostForm.Get("magnet"))
		d.torrents[id] = h
		d.adds = append(d.adds, h)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":%q}`, id)
	}))
	mux.HandleFunc("GET /rd/torrents", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var items []map[string]string
		for id, h := range d.torrents {
			st := "downloading"
			if d.cached[h] && d.selected[id] {
				st = "downloaded"
			}
			items = append(items, map[string]string{"id": id, "hash": h, "status": st})
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	mux.HandleFunc("GET /rd/torrents/info/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		id := r.PathValue("id")
		h, ok := d.torrents[id]
		switch {
		case !ok:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"unknown_ressource","error_code":7}`)
		case !d.selected[id]:
			fmt.Fprintf(w, `{"id":%q,"hash":%q,"status":"waiting_files_selection","files":`+uncachedFiles+`,"links":[]}`, id, h, 0)
		case d.cached[h]:
			fmt.Fprintf(w, `{"id":%q,"hash":%q,"status":"downloaded","files":`+uncachedFiles+`,"links":["https://rd.example/d/%s"]}`, id, h, 1, id)
		default:
			fmt.Fprintf(w, `{"id":%q,"hash":%q,"status":"downloading","files":`+uncachedFiles+`,"links":[]}`, id, h, 1)
		}
	}))
	mux.HandleFunc("POST /rd/torrents/selectFiles/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.selected[r.PathValue("id")] = true
		d.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("DELETE /rd/torrents/delete/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		delete(d.torrents, r.PathValue("id"))
		d.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /rd/unrestrict/link", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"download":"https://cdn.example/Fjord.Harbor.1999.1080p.mkv","filesize":4000000000,"filename":"Fjord.Harbor.1999.1080p.mkv"}`)
	}))
	// TorBox.
	ok := func(w http.ResponseWriter, data string) {
		fmt.Fprintf(w, `{"success":true,"error":null,"detail":"ok","data":%s}`, data)
	}
	mux.HandleFunc("GET /tb/torrents/checkcached", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		found := map[string]any{}
		for _, h := range strings.Split(r.URL.Query().Get("hash"), ",") {
			if d.cached[strings.ToLower(h)] {
				found[strings.ToLower(h)] = map[string]any{"hash": h}
			}
		}
		b, _ := json.Marshal(found)
		ok(w, string(b))
	}))
	mux.HandleFunc("POST /tb/torrents/createtorrent", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		h := magnetHash(r.FormValue("magnet"))
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.FormValue("add_only_if_cached") == "true" && !d.cached[h] {
			fmt.Fprint(w, `{"success":false,"error":"TORRENT_NOT_CACHED","detail":"not cached","data":null}`)
			return
		}
		if d.refuse == "slots" {
			fmt.Fprint(w, `{"success":false,"error":"ACTIVE_LIMIT","detail":"active download limit reached","data":null}`)
			return
		}
		for _, have := range d.torrents {
			if have == h {
				fmt.Fprint(w, `{"success":false,"error":"DUPLICATE_ITEM","detail":"already on the account","data":null}`)
				return
			}
		}
		d.next++
		id := fmt.Sprint(d.next)
		d.torrents[id] = h
		d.adds = append(d.adds, h)
		ok(w, fmt.Sprintf(`{"torrent_id":%s,"hash":%q}`, id, h))
	}))
	mux.HandleFunc("GET /tb/torrents/mylist", auth(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		item := func(id, h string) map[string]any {
			return map[string]any{"id": json.Number(id), "hash": h, "download_present": d.cached[h], "download_finished": d.cached[h],
				"files": []map[string]any{{"id": 0, "name": "Fjord.Harbor.1999/Fjord.Harbor.1999.1080p.mkv", "size": 4000000000}}}
		}
		if id := r.URL.Query().Get("id"); id != "" {
			h, found := d.torrents[id]
			if !found {
				fmt.Fprint(w, `{"success":false,"error":"ITEM_NOT_FOUND","detail":"gone","data":null}`)
				return
			}
			b, _ := json.Marshal(item(id, h))
			ok(w, string(b))
			return
		}
		var all []map[string]any
		for id, h := range d.torrents {
			all = append(all, item(id, h))
		}
		b, _ := json.Marshal(all)
		ok(w, string(b))
	}))
	mux.HandleFunc("GET /tb/torrents/requestdl", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, `"https://cdn.example/Fjord.Harbor.1999.1080p.mkv"`)
	}))
	mux.HandleFunc("POST /tb/torrents/controltorrent", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID json.Number `json:"torrent_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		delete(d.torrents, body.ID.String())
		d.mu.Unlock()
		ok(w, `null`)
	}))
	return mux
}

// uncachedIndexer is the owner's indexers: torrent results for the title
// and, when usenet is set, a Usenet indexer that finds nothing.
type uncachedIndexer struct {
	torrents []core.Release
	usenet   []core.Release
	nzbErr   error // what fetching a Usenet release answers
	err      error // the Usenet indexer's search failure
}

func (i *uncachedIndexer) Name() string { return "offline" }
func (i *uncachedIndexer) Search(context.Context, core.SearchRequest) ([]core.Release, error) {
	out := append(append([]core.Release(nil), i.torrents...), i.usenet...)
	return out, i.err
}
func (i *uncachedIndexer) FetchNZB(context.Context, core.Release) ([]byte, error) {
	return nil, i.nzbErr
}
func (i *uncachedIndexer) MissingSources() error   { return nil }
func (i *uncachedIndexer) HasTorrentIndexer() bool { return true }

type uncachedEnv struct {
	f      *fixture
	cfg    config.Config
	dbr    *debrid.Service
	rs     *resolver.Service
	mgr    *playback.Manager
	remote *uncachedDebrid
	ix     *uncachedIndexer
	hashes []string
}

// setCachedOnly is the owner flipping Sources › Debrid › Cached torrents
// only in the console: the running components get the edited config the
// way app.applyConfig hands it over, with no restart.
func (e *uncachedEnv) setCachedOnly(on bool) {
	e.cfg.Debrid.CachedOnly = on
	e.dbr.UpdateConfig(e.cfg.Debrid)
	e.rs.UpdateConfig(e.cfg)
	e.mgr.SetConfig(e.cfg)
}

func newUncachedEnv(t *testing.T, kind string, cachedOnly, usenet bool) *uncachedEnv {
	t.Helper()
	remote := newUncachedDebrid()
	srv := httptest.NewServer(remote.handler())
	t.Cleanup(srv.Close)
	cfg := config.Defaults(t.TempDir())
	cfg.Debrid.CachedOnly = cachedOnly
	cfg.Debrid.Providers = []config.DebridProvider{{ID: kind, Name: kind, Kind: kind, APIKey: uncachedKey, Enabled: true}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbr := debrid.NewService(cfg.Debrid, debrid.Options{Logger: quiet,
		ProviderOptions: debrid.ProviderOptions{HTTPClient: srv.Client(), PollInterval: time.Millisecond, ReadyPolls: 2},
		NewProvider: func(pc config.DebridProvider, po debrid.ProviderOptions) (debrid.Provider, error) {
			po.BaseURL = srv.URL + map[string]string{config.DebridRealDebrid: "/rd", config.DebridTorBox: "/tb"}[pc.Kind]
			return debrid.NewProvider(pc, po)
		}})
	var hashes []string
	var rels []core.Release
	for i, q := range []string{"2160p.WEB-DL.DV.HEVC", "1080p.BluRay.x264", "1080p.WEB-DL.H264", "720p.WEB-DL.H264"} {
		h := strings.Repeat(fmt.Sprintf("%x", i+10), 40)[:40]
		hashes = append(hashes, h)
		rels = append(rels, core.Release{ID: "t" + fmt.Sprint(i), GUID: "t" + fmt.Sprint(i), Indexer: "offline", Protocol: core.ProtocolTorrent,
			Name: "Fjord.Harbor.1999." + q + "-GRP", InfoHash: h, Size: int64(8-i) << 30, Seeders: 50, PubDate: time.Now().Add(-48 * time.Hour)})
	}
	inner := &uncachedIndexer{torrents: rels}
	ix := debrid.NewIndexer(inner, dbr)
	ix.SetUsenetReady(func() bool { return usenet })
	rs := resolver.New(ix, nil, nil, cfg, resolver.Options{Logger: quiet})
	t.Cleanup(func() { rs.Close() })
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true; d.Hooks = Hooks{} })
	tr := &issue94Transcoder{newFakeTranscoder(core.Decision{})}
	mgr, err := playback.New(playback.Deps{Catalog: f.catalog, Resolver: rs, Engine: issue94Engine{}, Transcoder: tr, SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return rs.ProfileFor(c, u) },
		FetchNZB: ix.FetchNZB,
		NativeProbe: func(context.Context, core.MediaSource) (*core.MediaInfo, error) {
			info := sampleInfo
			return &info, nil
		}}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.d.Playback = mgr
	f.srv.d.Transcoder = tr
	t.Cleanup(func() { f.srv.Close(); mgr.Close() })
	return &uncachedEnv{f: f, cfg: cfg, dbr: dbr, rs: rs, mgr: mgr, remote: remote, ix: inner, hashes: hashes}
}

const uncachedPath = "/library/metadata/1000000000000550"

func uncachedDecision(session string, mde bool) string {
	q := url.Values{"path": {uncachedPath}, "session": {session}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}}
	if mde {
		q.Set("hasMDE", "1")
	}
	return "/video/:/transcode/universal/decision?" + q.Encode()
}

func uncachedStart(session, ext string) string {
	q := url.Values{"path": {uncachedPath}, "session": {session}, "protocol": {"hls"}, "directPlay": {"0"}, "directStream": {"1"}}
	if ext == "mpd" {
		q.Set("protocol", "dash")
	}
	return "/video/:/transcode/universal/start." + ext + "?" + q.Encode()
}

// decide sends a Plex Web decision (hasMDE=1, JSON) and returns its code
// and the text the player shows.
func (e *uncachedEnv) decide(t *testing.T, session string) (float64, string) {
	t.Helper()
	mc := decodeJSON(t, e.f.get(t, uncachedDecision(session, true), append(jsonAccept, "X-Plex-Product", "Plex Web", "X-Plex-Client-Identifier", session)...))
	if mc["mdeDecisionText"] != nil && mc["mdeDecisionText"] != mc["generalDecisionText"] {
		t.Fatalf("Plex Web decision texts disagree: %v", mc)
	}
	code, _ := mc["generalDecisionCode"].(float64)
	text, _ := mc["generalDecisionText"].(string)
	return code, text
}

func (e *uncachedEnv) onAccount() int {
	e.remote.mu.Lock()
	defer e.remote.mu.Unlock()
	return len(e.remote.torrents)
}

// Every release of the title is a torrent no debrid account holds. Whatever
// the provider (Real-Debrid adds and inspects; TorBox answers a bulk instant
// check) and the owner's Cached torrents only switch, each Plex path ends
// with the reason instead of a spinner: the decision for Plex Web (JSON,
// mdeDecisionText) and Android TV (XML), the remembered failure for the
// start.m3u8 that follows it, and for start.mpd and part requests a terminal status plus
// the reason as terminationText on the client's next timeline. With the
// switch on nothing is left on the account. With it off exactly one release
// is sent to download, retries follow it rather than adding it again, and it
// plays once the download finishes. The switch takes effect without a
// restart.
func TestEveryCandidateUncachedMatrix(t *testing.T) {
	for _, kind := range []string{config.DebridRealDebrid, config.DebridTorBox} {
		for _, cachedOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cachedOnly=%v", kind, cachedOnly), func(t *testing.T) {
				e := newUncachedEnv(t, kind, cachedOnly, false)
				want := msgNotCachedOnly
				if !cachedOnly {
					want = msgDownloadQueued
				}
				if code, text := e.decide(t, "web"); code != codeGeneralError || text != want {
					t.Fatalf("Plex Web decision: %v %q, want %q", code, text, want)
				}
				android := map[string]string{"X-Plex-Product": "Plex for Android (TV)", "X-Plex-Client-Identifier": "atv", "X-Plex-Session-Identifier": "atv-play"}
				rec := e.f.request(t, http.MethodGet, uncachedDecision("atv", false), android)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `generalDecisionText="`+want+`"`) {
					t.Fatalf("Android TV decision: %d %s", rec.Code, rec.Body.String())
				}
				if rec := e.f.request(t, http.MethodGet, uncachedStart("atv", "m3u8"), android); rec.Code != http.StatusNotFound || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("start.m3u8 after the failed decision: %d", rec.Code)
				}
				for _, path := range []string{uncachedStart("dash", "mpd"), "/library/parts/1000000000000550/1759276800/file.mkv"} {
					client := map[string]string{"X-Plex-Client-Identifier": "c-" + path[len(path)-3:], "X-Plex-Session-Identifier": "play-" + path[len(path)-3:]}
					if strings.Contains(path, "/start.") {
						if rec := e.f.request(t, http.MethodGet, decisionPath(path), client); rec.Code != http.StatusOK {
							t.Fatalf("decision before %s: %d %s", path, rec.Code, rec.Body.String())
						}
					}
					if rec := e.f.request(t, http.MethodGet, path, client); rec.Code != http.StatusNotFound {
						t.Fatalf("%s: %d", path, rec.Code)
					}
					client["Accept"] = "application/json"
					tl := decodeJSON(t, e.f.request(t, http.MethodGet, "/:/timeline?ratingKey=1000000000000550&key=%2Flibrary%2Fmetadata%2F1000000000000550&state=buffering&time=0&duration=8340000", client))
					if tl["terminationText"] != want {
						t.Fatalf("timeline after %s: %v, want terminationText %q", path, tl, want)
					}
				}
				// Retries answer the same, without new adds.
				adds := len(e.remote.added())
				if _, text := e.decide(t, "retry"); text != want || len(e.remote.added()) != adds {
					t.Fatalf("retry: %q, adds %d -> %d", text, adds, len(e.remote.added()))
				}
				if cachedOnly {
					if n := e.onAccount(); n != 0 {
						t.Fatalf("cached-only left %d torrents on the account", n)
					}
					// Turned off in the console: the next Play downloads.
					e.setCachedOnly(false)
					if _, text := e.decide(t, "toggled"); text != msgDownloadQueued {
						t.Fatalf("after turning cached-only off: %q", text)
					}
				}
				if n := e.onAccount(); n != 1 {
					t.Fatalf("%d torrents left downloading, want the one best release", n)
				}
				for _, h := range e.hashes {
					e.remote.finish(h)
				}
				if code, text := e.decide(t, "done"); code == codeGeneralError {
					t.Fatalf("after the download finished: %q", text)
				}
			})
		}
	}
}

// A refused download and an account at its download limit say so, and a
// Usenet provider that found nothing is named.
func TestUncachedReasonsAreNamed(t *testing.T) {
	e := newUncachedEnv(t, config.DebridRealDebrid, false, false)
	e.remote.refuse = "slots"
	if _, text := e.decide(t, "slots"); text != msgDownloadLimit {
		t.Fatalf("download limit: %q", text)
	}
	e = newUncachedEnv(t, config.DebridRealDebrid, false, false)
	e.remote.refuse = "infringing"
	// Every blocked release is independent; none pauses the account.
	e.ix.torrents = e.ix.torrents[1:2]
	if _, text := e.decide(t, "refused"); text != msgDownloadRefused {
		t.Fatalf("refused: %q", text)
	}
	e = newUncachedEnv(t, config.DebridTorBox, true, true)
	if _, text := e.decide(t, "usenet"); text != msgNotCachedOnlyNoNZB {
		t.Fatalf("Usenet found nothing: %q", text)
	}
	e = newUncachedEnv(t, config.DebridTorBox, true, true)
	e.ix.usenet = []core.Release{{ID: "u1", GUID: "u1", Indexer: "offline", Name: "Fjord.Harbor.1999.1080p.WEB-DL.H264-NZB", Size: 6 << 30, PubDate: time.Now().Add(-24 * time.Hour), NZBURL: "x"}}
	e.ix.nzbErr = fmt.Errorf("nzb: %w", core.ErrArticleMissing)
	if _, text := e.decide(t, "usenet2"); text != msgNotCachedOnlyBadNZB {
		t.Fatalf("Usenet releases broken: %q", text)
	}
}
