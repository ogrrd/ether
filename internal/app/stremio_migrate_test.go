package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/stremio"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

// legacyAddonHost serves a catalog add-on under /cat-key and a subtitle
// add-on under /sub-key; their paths stand for the owner's configuration.
func legacyAddonHost(t *testing.T, downloads *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cat-key/manifest.json":
			fmt.Fprint(w, `{"id":"org.example.cinemeta","name":"Cinemeta","version":"3.0.0","resources":["catalog","meta"],"types":["movie","series"],
				"catalogs":[{"type":"movie","id":"top","name":"Popular","extra":[{"name":"genre","options":["Action"]}]},{"type":"series","id":"top","name":"Popular"}]}`)
		case r.URL.Path == "/sub-key/manifest.json":
			fmt.Fprint(w, `{"id":"org.subsense.addon","name":"SubSense","version":"1.2.0","types":["movie","series"],"idPrefixes":["tt"],"resources":["subtitles"]}`)
		case strings.HasPrefix(r.URL.Path, "/sub-key/subtitles/movie/tt1234567"):
			fmt.Fprintf(w, `{"subtitles":[{"id":"one","lang":"eng","url":"http://%s/file.srt?token=secret"}]}`, r.Host)
		default:
			downloads.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMigrateLegacyStremioCollectionsAndSubtitles(t *testing.T) {
	ctx := context.Background()
	var downloads atomic.Int32
	srv := legacyAddonHost(t, &downloads)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mgr, err := plugins.New(plugins.Deps{DB: db, Registry: extend.New(), Logger: log, IsLeader: func() bool { return true }, FetchClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	// The add-ons are on loopback, which the guarded remote runtime refuses;
	// this runtime reads the installed link directly.
	mgr.RegisterRuntime("remote", func(inst plugins.Installed) (plugins.Runtime, error) {
		return linkRuntime{base: inst.SourceURL, client: srv.Client()}, nil
	})
	cat, err := catalog.New(db, nil, config.Catalog{Language: "en-US", Region: "US"}, log)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := collections.New(db, cat, log, collections.WithSealer(cfg))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg, db: db, log: log, collections: cols, pluginsRT: &pluginRuntime{mgr: mgr}}

	subLink := srv.URL + "/sub-key/" // the same add-on as its manifest link
	if err := cfg.Update(func(c *config.Config) error {
		c.Subtitles.Providers = []config.SubtitleProvider{
			{ID: "opensubtitles", Kind: config.SubtitleProviderOpenSubtitles, Enabled: true},
			{ID: "subsense", Kind: config.SubtitleProviderStremio, Name: "My subtitles", URL: srv.URL + "/sub-key/manifest.json", Enabled: true},
			{ID: "again", Kind: config.SubtitleProviderStremio, Name: "Same add-on", URL: subLink, Enabled: false},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spec := legacyCatalogCollection(t, cols, db, cfg, srv.URL+"/cat-key/manifest.json")

	for run := 0; run < 2; run++ {
		if err := a.migrateLegacyStremioSubtitles(ctx); err != nil {
			t.Fatalf("run %d subtitles: %v", run, err)
		}
		if err := a.migrateLegacyStremioCollections(ctx); err != nil {
			t.Fatalf("run %d collections: %v", run, err)
		}
	}

	// Subtitles: the add-on takes the entry's place, name and state; the same
	// link entered twice is listed once.
	subID, _, _ := plugins.StremioInstallID(subLink)
	kind := extend.PluginID(subID, "subtitles")
	got := cfg.Get().Subtitles.Providers
	if len(got) != 2 || got[0].Kind != config.SubtitleProviderOpenSubtitles ||
		got[1] != (config.SubtitleProvider{ID: kind, Kind: kind, Name: "My subtitles", Enabled: true}) {
		t.Fatalf("subtitle providers = %+v", got)
	}
	// It searches as before: listing only, no link query in the offer.
	svc := subtitles.New(subtitles.Options{CacheDir: t.TempDir(), Providers: got})
	adapters.AttachSubtitles(mgr, svc)
	a.subtitles = svc
	offers, err := subtitleSurface{app: a}.SearchSubtitles(ctx, &core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1234567"}}, nil, "en", "")
	if err != nil || len(offers) != 1 || offers[0].Provider != "SubSense" || strings.Contains(offers[0].Title, "secret") || downloads.Load() != 0 {
		t.Fatalf("offers=%+v downloads=%d err=%v", offers, downloads.Load(), err)
	}

	// Collections: the source reads the installed add-on's catalog with the
	// same genre and limit; the collection keeps its id.
	catID, _, _ := plugins.StremioInstallID(srv.URL + "/cat-key")
	rec, err := mgr.Get(ctx, catID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Manifest.HasCapability(plugins.CapListSource) || rec.Manifest.HasCapability(plugins.CapMetadata) {
		t.Fatalf("catalog add-on roles: off=%v", rec.RolesOff)
	}
	sp, err := cols.GetSpec(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := extend.PluginID(catID, plugins.StremioCatalogKey(stremio.Catalog{Type: "movie", ID: "top"}))
	if len(sp.Sources) != 1 || sp.Sources[0].Kind != want || sp.Sources[0].URL != "" || sp.Sources[0].MaxItems != 150 ||
		sp.Sources[0].Params["genre"] != "Action" || len(sp.Sources[0].Params) != 1 {
		t.Fatalf("migrated sources = %+v, want %s", sp.Sources, want)
	}
}

// legacyCatalogCollection stores a collection as the retired Stremio catalog
// source left it (link sealed for this collection) and returns its id.
func legacyCatalogCollection(t *testing.T, cols *collections.Service, db *sql.DB, sealer config.FieldSealer, link string) string {
	t.Helper()
	ctx := context.Background()
	created, err := cols.CreateSpec(ctx, core.CollectionSpec{Name: "Popular", MediaType: "movie", Refresh: "daily",
		Sources: []core.CollectionSource{{Kind: "titles", Keys: []string{core.MovieKey(1).String()}}}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := config.SealWith(sealer, link, "collections_specs", "sources.url", created.ID, collections.LegacyStremioCatalogKind)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT spec_json FROM collections_specs WHERE id=?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var sp core.CollectionSpec
	_ = json.Unmarshal([]byte(raw), &sp)
	// search is not a declared extra of this catalog: it is dropped.
	sp.Sources = []core.CollectionSource{{Kind: collections.LegacyStremioCatalogKind, URL: sealed, List: "top", MediaType: "movie", MaxItems: 150,
		Params: map[string]string{"genre": "Action", "search": "x"}}}
	js, _ := json.Marshal(sp)
	if _, err := db.Exec(`UPDATE collections_specs SET spec_json=? WHERE id=?`, string(js), created.ID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// linkRuntime answers an add-on's routes from its installed link.
type linkRuntime struct {
	base   string
	client *http.Client
}

func (r linkRuntime) Call(ctx context.Context, req plugins.Request) (plugins.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+req.Path, nil)
	if err != nil {
		return plugins.Response{}, err
	}
	resp, err := r.client.Do(hreq)
	if err != nil {
		return plugins.Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return plugins.Response{Status: resp.StatusCode, Body: body}, err
}

func (linkRuntime) Close() error { return nil }
