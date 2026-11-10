package plex

import (
	"context"
	"database/sql"
	"encoding/json"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discoveryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, e := store.OpenMemory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func TestDiscoveryPlexHubsAndArtworkRespectViewerGuard(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	db := discoveryDB(t)
	wl := watchlist.New(db, watchlist.Options{Catalog: f.catalog})
	if e := wl.SaveSettings(ctx, watchlist.Settings{Enabled: true, CheckHours: 6}); e != nil {
		t.Fatal(e)
	}
	owner := &core.User{ID: 1, IsOwner: true}
	if e := wl.Remind(ctx, owner, fightClub.Key); e != nil {
		t.Fatal(e)
	}
	f.srv.setDiscovery((&discovery.Features{Watchlist: wl, Overlays: overlays.New(db, overlays.Options{})}).Frontend())
	f.srv.d.Auth.(*fakeAuth).users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	for _, path := range []string{"/hubs", "/library/metadata/" + fightClub.Key.String() + "/discoveryPoster/version"} {
		r := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "kid"})
		if path == "/hubs" {
			if r.Code != 200 || strings.Contains(r.Body.String(), "Fjord Harbor") {
				t.Fatalf("hub guard %d %s", r.Code, r.Body.String())
			}
		} else if r.Code != 404 {
			t.Fatalf("poster guard %d", r.Code)
		}
	}
}
func TestDiscoveryLocalizedGridQueuesWithoutNetworkWait(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	db := discoveryDB(t)
	ov := overlays.New(db, overlays.Options{})
	if e := ov.SaveSettings(ctx, overlays.Settings{Enabled: true, Localize: true}); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int64
	d := &discovery.Features{Overlays: ov, Localize: func(ctx context.Context, t *core.Title, lang string) (*core.Title, error) {
		calls.Add(1)
		copy := *t
		copy.Title = "Titre français"
		return &copy, nil
	}}
	f.srv.setDiscovery(d.Frontend())
	pr := &Request{User: &core.User{ID: 1, IsOwner: true}, Language: "fr"}
	before := time.Now()
	m := f.srv.metadata(ctx, pr, &fightClub, renderOpts{})
	if calls.Load() != 0 || m.Title != fightClub.Title || time.Since(before) > time.Second {
		t.Fatal("metadata waited for remote localization")
	}
	jobCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { d.Run(jobCtx); close(done) }()
	deadline := time.After(2 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("translation worker did not run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	m = f.srv.metadata(ctx, pr, &fightClub, renderOpts{})
	if m.Title != "Titre français" || fightClub.Title != "Fjord Harbor" {
		t.Fatalf("localized cache/canonical mutation %q %q", m.Title, fightClub.Title)
	}
}
func TestOverviewTranslationRunsOnDetailNotOnAList(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var calls atomic.Int32
	f.srv.setDiscovery(&DiscoveryFeatures{OverviewTranslate: func(_ context.Context, _ *core.Title, lang string, detail bool) (string, string, bool) {
		if lang != "fr" || !detail {
			return "", "", false
		}
		calls.Add(1)
		return "Bonjour", "Slogan", true
	}})
	pr := &Request{User: &core.User{ID: 1, IsOwner: true}, Language: "fr"}
	title := fightClub
	listed := f.srv.metadata(context.Background(), pr, &title, renderOpts{})
	if calls.Load() != 0 || listed.Summary == "Bonjour" {
		t.Fatalf("list row called translation %d %q", calls.Load(), listed.Summary)
	}
	opened := f.srv.metadata(context.Background(), pr, &title, renderOpts{detail: true})
	if calls.Load() != 1 || opened.Summary != "Bonjour" || opened.Tagline != "Slogan" {
		t.Fatalf("detail %d %q %q", calls.Load(), opened.Summary, opened.Tagline)
	}
}

func TestDiscoveryPlexRatingNormalizationAndStickyDismissal(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	db := discoveryDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Response": "True", "Ratings": []map[string]string{{"Source": "Rotten Tomatoes", "Value": "92%"}}})
	}))
	defer upstream.Close()
	rs := ratings.New(db, ratings.Options{OMDbBaseURL: upstream.URL})
	if e := rs.SaveSettings(ctx, ratings.Settings{Enabled: true, Provider: "omdb", APIKey: "owner-key", Critic: "tomatoes", CacheHours: 24}); e != nil {
		t.Fatal(e)
	}
	if _, e := rs.Refresh(ctx, &fightClub); e != nil {
		t.Fatal(e)
	}
	personal := personalize.NewDiscovery(db, f.catalog, f.srv.state(), nil)
	f.srv.setDiscovery((&discovery.Features{Ratings: rs, Personal: personal}).Frontend())
	pr := &Request{User: &core.User{ID: 1, IsOwner: true}}
	md := f.srv.metadata(ctx, pr, &fightClub, renderOpts{})
	if md.Rating != 9.2 || md.RatingImage != "rottentomatoes://image.rating.ripe" {
		t.Fatalf("rating/icon %.1f %s", md.Rating, md.RatingImage)
	}
	if e := f.srv.discoveryRemoveContinue(ctx, pr, fightClub.Key); e != nil {
		t.Fatal(e)
	}
	if got := personal.FilterContinue(ctx, pr.User, []core.Title{fightClub}); len(got) != 0 {
		t.Fatal("sticky dismissal ignored")
	}
	other := &core.User{ID: 2}
	if got := personal.FilterContinue(ctx, other, []core.Title{fightClub}); len(got) != 1 {
		t.Fatal("dismissal crossed viewers")
	}
}

// Explicit backdrop dimensions must select a covering source, with independent
// validators so a previously cached smaller response cannot satisfy the request.
func TestDiscoveryBackdropResolutionValidator(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var fetched string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = r.URL.Path
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("backdrop"))
	}))
	defer upstream.Close()
	f.srv.images = artworkservice.NewRemoteCache(1024, http.DefaultClient)
	f.srv.d.ImageURL = func(path, size string) string { return upstream.URL + "/" + size + path }
	f.srv.setDiscovery((&discovery.Features{Overlays: overlays.New(discoveryDB(t), overlays.Options{})}).Frontend())
	path := "/library/metadata/" + fightClub.Key.String() + "/discoveryPoster/version?kind=backdrop&width=2688"
	first := f.request(t, "GET", path, nil)
	if first.Code != 200 || !strings.HasPrefix(fetched, "/original/") || !strings.HasSuffix(first.Header().Get("ETag"), "w2688\"") {
		t.Fatalf("%d %s %s", first.Code, fetched, first.Header().Get("ETag"))
	}
	cached := f.request(t, "GET", path, map[string]string{"If-None-Match": first.Header().Get("ETag")})
	if cached.Code != 304 {
		t.Fatal("lost conditional GET", cached.Code)
	}
	smaller := f.request(t, "GET", strings.Replace(path, "2688", "1920", 1), map[string]string{"If-None-Match": first.Header().Get("ETag")})
	if smaller.Code != 200 {
		t.Fatal("cross-size validator reused", smaller.Code)
	}
}
