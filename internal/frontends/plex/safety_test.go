package plex

import (
	"context"
	"database/sql"
	"encoding/base64"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRestrictedViewerCannotRetrieveHiddenTitles(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	key := fightClub.Key.String()
	for _, path := range []string{"/library/metadata/" + key, "/library/metadata/" + key + "/thumb/1", "/library/parts/" + key + "/1/movie.mkv", "/photo/:/transcode?url=" + url.QueryEscape("/library/metadata/"+key+"/thumb/1"), "/library/metadata/" + breakingBad.Key.String() + "/children"} {
		r := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "kid"})
		if r.Code != 404 {
			t.Errorf("%s status %d body %s", path, r.Code, r.Body.String())
		}
	}
	for _, path := range []string{"/hubs", "/hubs/search?query=Fight", "/library/sections/1/all", "/hubs/sections/1"} {
		r := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "kid"})
		if r.Code != 200 || strings.Contains(r.Body.String(), `title="Fjord Harbor"`) {
			t.Errorf("%s leaked title %s", path, r.Body.String())
		}
	}
}
func TestAdultTitlesHiddenEvenFromOwner(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	x := fightClub
	x.Adult = true
	f.catalog.titles[x.Key] = x
	r := f.get(t, "/library/metadata/"+x.Key.String())
	if r.Code != 404 {
		t.Fatalf("adult metadata %d", r.Code)
	}
}

func TestRestrictedAbsoluteArtworkRequiresVisibleTitle(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE catalog_titles(key INTEGER,adult INTEGER,poster TEXT,backdrop TEXT,still TEXT,logo TEXT); INSERT INTO catalog_titles VALUES(?,0,'/visible.jpg','','',''),(?,0,'/hidden.jpg','','','')`, core.MovieKey(999), fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.DB = db
	f.catalog.titles[core.MovieKey(999)] = core.Title{Key: core.MovieKey(999), Type: core.TypeMovie, ContentRating: "PG"}
	r := httptest.NewRequest("GET", "/photo/:/transcode", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &Request{User: &core.User{ID: 30, MaxContentRating: "PG"}}))
	for _, c := range []struct {
		path    string
		allowed bool
	}{{"https://image.tmdb.org/t/p/w500/visible.jpg", true}, {"https://image.tmdb.org/t/p/w500/hidden.jpg", false}, {"https://image.tmdb.org/t/p/w500/unknown.jpg", false}} {
		u, _ := url.Parse(c.path)
		if got := f.srv.remoteArtworkAllowed(r, u); got != c.allowed {
			t.Errorf("%s allowed=%v", c.path, got)
		}
	}
}

type keywordPolicy struct{ blocked string }
func (keywordPolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (p keywordPolicy) Allowed(u *core.User, t *core.Title) bool {
	if u == nil || u.IsOwner {
		return true
	}
	for _, k := range t.Keywords {
		if k == p.blocked {
			return false
		}
	}
	return true
}

// Seasons and episodes do not carry the show's keywords, so a show blocked
// by owner policy must also hide its children when addressed directly.
func TestBlockedShowChildrenStayHidden(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	show := breakingBad
	show.Keywords = []string{"blocked"}
	f.catalog.titles[show.Key] = show
	ep, season := bbE1.Key.String(), bbS1.Key.String()
	for _, path := range []string{
		"/library/metadata/" + show.Key.String(),
		"/library/metadata/" + ep,
		"/library/metadata/" + season,
		"/library/metadata/" + season + "/children",
		"/library/metadata/" + season + "/allLeaves",
		"/library/metadata/" + ep + "/parent",
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F" + ep,
		"/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F" + ep + "&session=blocked-ep",
	} {
		if rec := f.get(t, path, "X-Plex-Token", friendToken); rec.Code != 404 {
			t.Errorf("%s: status %d body %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+url.QueryEscape("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/"+ep), map[string]string{"X-Plex-Token": friendToken})
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "Pilot") {
		t.Errorf("play queue of a blocked episode: %d %s", rec.Code, rec.Body.String())
	}
	// The owner is unaffected.
	if rec := f.get(t, "/library/metadata/"+ep); rec.Code != 200 {
		t.Errorf("owner episode: %d", rec.Code)
	}
}

// Play queues and playlists render only titles the viewer may see.
func TestRestrictedViewerQueuesAndPlaylistsHideTitles(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	pg := alien
	pg.ContentRating = "PG"
	f.catalog.titles[pg.Key] = pg
	uri := url.QueryEscape("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String())
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+uri, map[string]string{"X-Plex-Token": "kid"})
	if strings.Contains(rec.Body.String(), "Fjord Harbor") {
		t.Errorf("play queue leaked a hidden title: %d %s", rec.Code, rec.Body.String())
	}
	// A playlist holding an item from before the restriction applied.
	pl, err := f.pls.Create(context.Background(), 30, "Mine", []core.RatingKey{fightClub.Key, alien.Key})
	if err != nil {
		t.Fatal(err)
	}
	rec = f.get(t, "/playlists/"+pl.Key.String()+"/items", "X-Plex-Token", "kid")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Fjord Harbor") || !strings.Contains(rec.Body.String(), "Alien") {
		t.Errorf("playlist items: %d %s", rec.Code, rec.Body.String())
	}
}

// Up Next and a show's On Deck must not surface an episode of a show the
// viewer's policy blocks, even when the episode lacks the show's keywords.
func TestBlockedShowEpisodesStayOutOfUpNextAndOnDeck(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	show := breakingBad
	show.Keywords = []string{"blocked"}
	f.catalog.titles[show.Key] = show
	rec := f.get(t, "/hubs/metadata/"+bbE1.Key.String()+"/postplay", "X-Plex-Token", friendToken)
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "Second Watch") || strings.Contains(rec.Body.String(), "tv.upnext") {
		t.Errorf("postplay: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/library/metadata/"+show.Key.String()+"/onDeck", "X-Plex-Token", friendToken)
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "Pilot") {
		t.Errorf("show onDeck: %d %s", rec.Code, rec.Body.String())
	}
	// A completed episode makes the owner eligible for postplay next-up.
	f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
	// The owner still gets both.
	if rec := f.get(t, "/hubs/metadata/"+bbE1.Key.String()+"/postplay"); !strings.Contains(rec.Body.String(), "Second Watch") {
		t.Errorf("owner postplay lost Up Next: %s", rec.Body.String())
	}
	if rec := f.get(t, "/library/metadata/"+show.Key.String()+"/onDeck"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Second Watch") {
		t.Errorf("owner onDeck: %d %s", rec.Code, rec.Body.String())
	}
}

type historyState struct {
	*fakeState
	hist []core.HistoryEntry
}

func (h historyState) History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) {
	return h.hist, nil
}

// Watch history must not list titles the viewer may no longer see.
func TestHistoryHidesRestrictedTitles(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	pg := alien
	pg.ContentRating = "PG"
	f.catalog.titles[pg.Key] = pg
	f.srv.d.State = historyState{fakeState: f.state, hist: []core.HistoryEntry{
		{UserID: 30, Key: fightClub.Key, ViewedAt: fixedNow, ViewType: 1}, {UserID: 30, Key: pg.Key, ViewedAt: fixedNow, ViewType: 1},
	}}
	rec := f.get(t, "/status/sessions/history/all?type=1", "X-Plex-Token", "kid")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Fjord Harbor") || !strings.Contains(rec.Body.String(), "Alien") {
		t.Errorf("history: %d %s", rec.Code, rec.Body.String())
	}
}

// A collection without its own poster falls back to an item's poster, but
// never to one the viewer may not see.
func TestCollectionPosterFallbackSkipsHiddenItems(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	pg := alien
	pg.ContentRating = "PG"
	f.catalog.titles[pg.Key] = pg
	key := core.CollectionKey(77)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Mixed", Kind: "franchise", Type: core.TypeMovie, ChildCount: 2}
	f.cols.items[key] = []core.Title{fightClub, pg}
	rec := f.get(t, "/library/collections/"+key.String()+"/thumb/1", "X-Plex-Token", "kid")
	if rec.Code != 200 {
		t.Fatalf("collection thumb: %d", rec.Code)
	}
	if f.art.count("/t/p/w500/poster550.jpg") != 0 || f.art.count("/t/p/w500/alien.jpg") != 1 {
		t.Fatalf("fallback poster fetched %v", f.art.hits)
	}
}

// On Deck pairs rendered items with their titles: a hidden in-progress
// title must not shift another item's section attributes.
func TestOnDeckHiddenTitleKeepsSectionAttributes(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	movie := fightClub
	movie.Keywords = []string{"blocked"}
	f.catalog.titles[movie.Key] = movie
	f.state.set(friendUser.ID, core.WatchStatus{Key: movie.Key, ViewOffset: time.Minute})
	f.state.set(friendUser.ID, core.WatchStatus{Key: bbE1.Key, ViewOffset: time.Minute})
	rec := f.get(t, "/library/onDeck", "X-Plex-Token", friendToken, "Accept", "application/json")
	mc := decodeJSON(t, rec)
	items, _ := mc["Metadata"].([]any)
	if len(items) != 1 {
		t.Fatalf("onDeck items: %s", rec.Body.String())
	}
	tv, _ := f.srv.sectionFor(core.TypeEpisode)
	if got := items[0].(map[string]any)["librarySectionUUID"]; got != tv.UUID {
		t.Fatalf("episode librarySectionUUID = %v, want %s", got, tv.UUID)
	}
}

// A viewer limited to some sections must not open a collection of another.
func TestCollectionOfHiddenSectionIsNotFound(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["tvonly"] = core.User{ID: 31, Username: "tvonly", AllowedLibraries: []core.LibraryID{2}}
	key := core.CollectionKey(8091).String()
	for _, path := range []string{"/library/collections/" + key, "/library/collections/" + key + "/children", "/library/metadata/" + key, "/library/collections/" + key + "/thumb/1"} {
		if rec := f.get(t, path, "X-Plex-Token", "tvonly"); rec.Code != 404 {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := f.get(t, "/library/collections/"+key); rec.Code != 200 {
		t.Errorf("owner collection: %d", rec.Code)
	}
}

// Related, similar and post-play rows of a hidden title are not served: they
// are built from that title's credits and collection.
func TestRelatedRowsOfHiddenTitleAreNotFound(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	key := fightClub.Key.String()
	for _, path := range []string{"/hubs/metadata/" + key, "/hubs/metadata/" + key + "/related", "/library/metadata/" + key + "/related", "/library/metadata/" + key + "/similar", "/hubs/metadata/" + key + "/postplay"} {
		if rec := f.get(t, path, "X-Plex-Token", "kid"); rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if rec := f.get(t, path); rec.Code != 200 {
			t.Errorf("owner %s: %d", path, rec.Code)
		}
	}
}

// allLeaves pages over the episodes the viewer may see.
func TestAllLeavesPagesVisibleEpisodes(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	ep := bbE1
	ep.Keywords = []string{"blocked"}
	f.catalog.titles[ep.Key] = ep
	rec := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"/allLeaves?X-Plex-Container-Start=0&X-Plex-Container-Size=1", "X-Plex-Token", friendToken, "Accept", "application/json")
	mc := decodeJSON(t, rec)
	items, _ := mc["Metadata"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != bbE2.Title || mc["totalSize"].(float64) != 1 {
		t.Fatalf("allLeaves page: %s", rec.Body.String())
	}
}

// A lookup that fails while checking the source title must not let its
// related rows through: the recommender fetches the title again.
func TestRelatedRowsFailClosedOnLookupError(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.testAuth().users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	f.catalog.failOnce = map[core.RatingKey]bool{fightClub.Key: true}
	if rec := f.get(t, "/hubs/metadata/"+fightClub.Key.String()+"/related", "X-Plex-Token", "kid"); rec.Code == 200 {
		t.Fatalf("related rows served after a failed visibility lookup: %s", rec.Body.String())
	}
}

// History and collection poster fallbacks apply the show's policy to its
// episodes, like every other listing.
func TestHistoryAndPosterHideBlockedShowEpisodes(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	show := breakingBad
	show.Keywords = []string{"blocked"}
	f.catalog.titles[show.Key] = show
	f.srv.d.State = historyState{fakeState: f.state, hist: []core.HistoryEntry{{UserID: friendUser.ID, Key: bbE1.Key, ViewedAt: fixedNow, ViewType: 1}}}
	if rec := f.get(t, "/status/sessions/history/all?type=1", "X-Plex-Token", friendToken); rec.Code != 200 || strings.Contains(rec.Body.String(), bbE1.Title) {
		t.Errorf("history: %d %s", rec.Code, rec.Body.String())
	}
	key := core.CollectionKey(78)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Eps", Kind: "franchise", Type: core.TypeEpisode, ChildCount: 1}
	ep := bbE1
	ep.PosterPath = "/blocked-ep.jpg"
	f.cols.items[key] = []core.Title{ep}
	f.get(t, "/library/collections/"+key.String()+"/thumb/1", "X-Plex-Token", friendToken)
	if n := f.art.count("/t/p/w500/blocked-ep.jpg"); n != 0 {
		t.Fatalf("blocked show's episode poster used as fallback (%d fetches)", n)
	}
}

func TestRecentAvailabilityAppliesToListsAndDirectMetadata(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.FilterAvailable = func(_ context.Context, ts []core.Title) []core.Title {
		var out []core.Title
		for _, v := range ts {
			if v.Key != fightClub.Key {
				out = append(out, v)
			}
		}
		return out
	}
	for _, path := range []string{"/library/metadata/" + fightClub.Key.String(), "/library/metadata/" + fightClub.Key.String() + "/thumb/1"} {
		r := f.request(t, http.MethodGet, path, nil)
		if r.Code != 404 {
			t.Fatalf("%s: %d", path, r.Code)
		}
	}
	for _, path := range []string{"/hubs", "/hubs/search?query=Fight", "/library/sections/1/all", "/hubs/sections/1"} {
		r := f.request(t, http.MethodGet, path, nil)
		if r.Code != 200 || strings.Contains(r.Body.String(), `title="Fjord Harbor"`) {
			t.Fatalf("%s leaked hidden title: %d", path, r.Code)
		}
	}
}

func TestAvailabilityDoesNotInterruptAnOpenViewerSession(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	user := &core.User{ID: 1, IsOwner: true}
	if _, err := f.playback.Prepare(t.Context(), user, fightClub.Key, core.ClientCaps{ClientID: "watching"}); err != nil {
		t.Fatal(err)
	}
	if !f.srv.visibleTitle(t.Context(), user, &fightClub) {
		t.Fatal("visibility change revoked active playback")
	}
	if f.srv.visibleTitle(t.Context(), &core.User{ID: 2}, &fightClub) {
		t.Fatal("active viewer granted access to another viewer")
	}
}

func TestCastPortraitTranscodeVisibility(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE catalog_titles(key INTEGER,adult INTEGER,poster TEXT,backdrop TEXT,still TEXT,logo TEXT); CREATE TABLE catalog_credits(key INTEGER,thumb TEXT);`)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.DB = db
	f.srv.testAuth().users["kid"] = core.User{ID: 30, MaxContentRating: "PG"}
	cases := []struct {
		name        string
		key         core.RatingKey
		rating      string
		adult       bool
		unavailable bool
		want        int
	}{
		{"visible", core.MovieKey(1001), "PG", false, false, 200}, {"forbidden", core.MovieKey(1002), "R", false, false, 404}, {"future", core.MovieKey(1003), "PG", false, true, 404}, {"adult", core.MovieKey(1004), "PG", true, false, 404}, {"unknown-release", core.MovieKey(1005), "PG", false, false, 404},
	}
	f.srv.testAuth().users["tvonly"] = core.User{ID: 31, AllowedLibraries: []core.LibraryID{2}}
	for _, c := range cases {
		portrait := "/" + c.name + "-actor.jpg"
		title := core.Title{Key: c.key, Type: core.TypeMovie, Title: c.name, ContentRating: c.rating, Adult: c.adult, Released: fixedNow.Add(-time.Hour), Cast: []core.Person{{Name: "Actor", TMDB: 123, ThumbPath: portrait}}}
		if c.unavailable {
			title.Released = fixedNow.Add(48 * time.Hour)
		}
		if c.name == "unknown-release" {
			title.Released = time.Time{}
		}
		if c.want == 200 {
			title.Directors = []core.Person{{Name: "Director", TMDB: 124, ThumbPath: "/director.jpg"}}
			title.Writers = []core.Person{{Name: "Writer", TMDB: 125, ThumbPath: "/writer.jpg"}}
			for _, p := range []string{"/director.jpg", "/writer.jpg"} {
				if _, err := db.Exec(`INSERT INTO catalog_credits VALUES(?,?)`, c.key, p); err != nil {
					t.Fatal(err)
				}
			}
		}
		f.catalog.titles[c.key] = title
		if _, err := db.Exec(`INSERT INTO catalog_titles VALUES(?,?, '', '', '', '')`, c.key, c.adult); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO catalog_credits VALUES(?,?)`, c.key, portrait); err != nil {
			t.Fatal(err)
		}
		if c.want == 200 {
			rec := f.get(t, "/library/metadata/"+c.key.String(), "X-Plex-Token", "kid", "Accept", "application/json")
			mc := decodeJSON(t, rec)
			items := mc["Metadata"].([]any)
			roles := items[0].(map[string]any)["Role"].([]any)
			thumb := roles[0].(map[string]any)["thumb"].(string)
			rec = f.get(t, "/photo/:/transcode?width=100&url="+url.QueryEscape(thumb), "X-Plex-Token", "kid")
			if rec.Code != 200 {
				t.Errorf("rendered role portrait: got %d", rec.Code)
			}
			if direct := f.get(t, thumb, "X-Plex-Token", "kid"); direct.Code != 200 {
				t.Errorf("direct portrait: %d", direct.Code)
			}
			if blocked := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(thumb), "X-Plex-Token", "tvonly"); blocked.Code != 404 {
				t.Errorf("section blocked: %d", blocked.Code)
			}
			for _, tag := range []string{"Director", "Writer"} {
				tags := items[0].(map[string]any)[tag].([]any)
				personThumb := tags[0].(map[string]any)["thumb"].(string)
				if got := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(personThumb), "X-Plex-Token", "kid"); got.Code != 200 {
					t.Errorf("%s portrait: %d", tag, got.Code)
				}
			}
			if rec = f.get(t, "/photo/:/transcode?width=100&url="+url.QueryEscape(thumb)); rec.Code != 200 {
				t.Errorf("owner portrait: got %d", rec.Code)
			}
			if f.art.count("/t/p/w185/visible-actor.jpg") == 0 {
				t.Error("owner dimensions did not fetch w185 profile")
			}
			reordered := title
			reordered.Cast = []core.Person{{Name: "Other", ThumbPath: "/other.jpg"}, title.Cast[0]}
			f.catalog.titles[c.key] = reordered
			if got := f.get(t, thumb, "X-Plex-Token", "kid"); got.Code != 200 || f.art.count("/t/p/w185/other.jpg") != 0 {
				t.Errorf("reordered portrait: %d", got.Code)
			}
			reordered.Cast = reordered.Cast[:1]
			f.catalog.titles[c.key] = reordered
			if got := f.get(t, thumb, "X-Plex-Token", "kid"); got.Code != 404 {
				t.Errorf("removed portrait: %d", got.Code)
			}

		} else {
			thumb := "/library/metadata/" + c.key.String() + "/person/actor/" + base64.RawURLEncoding.EncodeToString([]byte(portrait))
			rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(thumb), "X-Plex-Token", "kid")
			if rec.Code != c.want {
				t.Errorf("%s: got %d", c.name, rec.Code)
			}
		}
	}
	for _, raw := range []string{"https://image.tmdb.org/t/p/w185/unknown.jpg", "https://example.com/a.jpg"} {
		if rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(raw), "X-Plex-Token", "kid"); rec.Code != 404 {
			t.Errorf("unknown %s: %d", raw, rec.Code)
		}
	}
}

type portraitCountingCatalog struct {
	*fakeCatalog
	calls  int
	cancel context.CancelFunc
}

func (c *portraitCountingCatalog) Get(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	c.calls++
	t, err := c.fakeCatalog.Get(ctx, key)
	if c.cancel != nil {
		c.cancel()
	}
	return t, err
}

func TestPersonPortraitUsesOnlyScopedTitleAndHonorsCancellation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	cat := &portraitCountingCatalog{fakeCatalog: f.catalog}
	f.srv.d.Catalog = cat
	f.srv.testAuth().users["kid"] = core.User{ID: 30, MaxContentRating: "PG"}
	for i := 0; i < 100; i++ {
		k := core.MovieKey(int64(2000 + i))
		f.catalog.titles[k] = core.Title{Key: k, Type: core.TypeMovie, ContentRating: "R", Released: fixedNow.Add(-time.Hour), Cast: []core.Person{{ThumbPath: "/shared.jpg"}}}
	}
	k := core.MovieKey(3000)
	f.catalog.titles[k] = core.Title{Key: k, Type: core.TypeMovie, ContentRating: "PG", Released: fixedNow.Add(-time.Hour), Cast: []core.Person{{ThumbPath: "/shared.jpg"}}}
	path := "/library/metadata/" + k.String() + "/person/actor/" + base64.RawURLEncoding.EncodeToString([]byte("/shared.jpg"))
	if rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(path), "X-Plex-Token", "kid"); rec.Code != 200 {
		t.Fatalf("portrait: %d", rec.Code)
	}
	if cat.calls != 1 {
		t.Fatalf("catalog lookups = %d, want 1", cat.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.srv.servePersonImage(rec, req, k, "actor", base64.RawURLEncoding.EncodeToString([]byte("/shared.jpg")))
	if cat.calls != 1 {
		t.Fatalf("canceled request did %d lookups", cat.calls-1)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cat.cancel = cancel2
	before := f.art.count("/t/p/w185/shared.jpg")
	req = httptest.NewRequest("GET", path, nil).WithContext(ctx2)
	rec = httptest.NewRecorder()
	f.srv.servePersonImage(rec, req, k, "actor", base64.RawURLEncoding.EncodeToString([]byte("/shared.jpg")))
	if rec.Code != http.StatusNotFound || f.art.count("/t/p/w185/shared.jpg") != before {
		t.Fatal("late cancellation served or fetched portrait")
	}

}
