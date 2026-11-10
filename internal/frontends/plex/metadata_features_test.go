package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type orderedSource struct{}

func (orderedSource) EpisodeGroups(context.Context, int64) ([]tmdb.EpisodeGroupSummary, error) {
	return []tmdb.EpisodeGroupSummary{{ID: "dvd", Type: 3}}, nil
}
func (orderedSource) EpisodeGroup(context.Context, string) (*tmdb.EpisodeGroup, error) {
	return &tmdb.EpisodeGroup{Groups: []tmdb.EpisodeSubgroup{{Name: "Disc 1", Order: 0, Episodes: []tmdb.OrderedEpisode{{Episode: tmdb.Episode{SeasonNumber: 1, EpisodeNumber: 2}, Order: 0}, {Episode: tmdb.Episode{SeasonNumber: 1, EpisodeNumber: 1}, Order: 1}}}}}, nil
}
func TestAlternateOrderPlexTreeAndCanonicalPlayback(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	db, e := store.OpenMemory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := episodeorder.New(db, orderedSource{}, f.catalog)
	f.srv.d.EpisodeOrder = s
	if e = s.Save(context.Background(), 0, breakingBad.Key, episodeorder.Preference{GroupID: "dvd", Specials: "last"}); e != nil {
		t.Fatal(e)
	}
	virtual := core.SeasonKey(breakingBad.Key.TMDB(), 9000)
	rec := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"/children?excludeAllLeaves=1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), virtual.String()) || strings.Contains(rec.Body.String(), `ratingKey="`+bbS1.Key.String()+`"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/library/metadata/"+virtual.String()+"/children")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `parentRatingKey="`+virtual.String()+`"`) {
		t.Fatal(rec.Code, body)
	}
	if strings.Index(body, `ratingKey="`+bbE2.Key.String()+`"`) > strings.Index(body, `ratingKey="`+bbE1.Key.String()+`"`) {
		t.Fatal("order not applied", body)
	}
	// The chosen episode keeps canonical s01e02 identity and part path.
	rec = f.get(t, "/library/metadata/"+bbE2.Key.String())
	if !strings.Contains(rec.Body.String(), `index="1"`) || !strings.Contains(rec.Body.String(), `ratingKey="`+bbE2.Key.String()+`"`) {
		t.Fatal(rec.Body.String())
	}
	pr := &Request{User: &ownerUser}
	keys, err := f.srv.continuousKeys(context.Background(), pr, "/library/metadata/"+bbE2.Key.String(), true)
	if err != nil || len(keys) != 2 || keys[0] != bbE2.Key || keys[1] != bbE1.Key {
		t.Fatal("continuous order", keys, err)
	}
	keys, err = f.srv.expandKey(context.Background(), pr, virtual)
	if err != nil || len(keys) != 2 || keys[0] != bbE2.Key {
		t.Fatal("virtual container expansion", keys, err)
	}
	rec = f.get(t, "/library/metadata/"+bbE2.Key.String()+"/parent")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `ratingKey="`+virtual.String()+`"`) {
		t.Fatal("parent", rec.Code, rec.Body.String())
	}
	canonical, _ := f.catalog.Get(context.Background(), bbE2.Key)
	if canonical.Season != 1 || canonical.Episode != 2 {
		t.Fatal("canonical changed", canonical)
	}
	f.srv.testAuth().users["movie-only"] = core.User{ID: 88, AllowedLibraries: []core.LibraryID{1}}
	for _, path := range []string{"/library/metadata/" + virtual.String(), "/library/metadata/" + virtual.String() + "/children", "/library/metadata/" + breakingBad.Key.String() + "/allLeaves", "/library/metadata/" + bbE2.Key.String() + "/parent"} {
		rec = f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "movie-only"})
		if rec.Code != 404 {
			t.Fatalf("section leakage %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// blockingCatalog stands in for a catalogue whose season loads go to TMDB:
// Children and Grandchildren of the blocked shows wait until released.
type blockingCatalog struct {
	core.Catalog
	block   map[core.RatingKey]bool
	release chan struct{}
	loads   atomic.Int32
}

func (c *blockingCatalog) wait(k core.RatingKey) {
	if c.block[core.ShowKey(k.TMDB())] {
		c.loads.Add(1)
		<-c.release
	}
}
func (c *blockingCatalog) Children(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	c.wait(k)
	return c.Catalog.Children(ctx, k)
}
func (c *blockingCatalog) Grandchildren(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	c.wait(k)
	return c.Catalog.Grandchildren(ctx, k)
}

type twoDiscSource struct{}

func (twoDiscSource) EpisodeGroups(context.Context, int64) ([]tmdb.EpisodeGroupSummary, error) {
	return []tmdb.EpisodeGroupSummary{{ID: "dvd", Type: 3}}, nil
}
func (twoDiscSource) EpisodeGroup(context.Context, string) (*tmdb.EpisodeGroup, error) {
	disc := func(name string, order, episode int) tmdb.EpisodeSubgroup {
		return tmdb.EpisodeSubgroup{Name: name, Order: order, Episodes: []tmdb.OrderedEpisode{{Episode: tmdb.Episode{SeasonNumber: 1, EpisodeNumber: episode}}}}
	}
	return &tmdb.EpisodeGroup{Groups: []tmdb.EpisodeSubgroup{disc("Disc 1", 0, 2), disc("Disc 2", 1, 1)}}, nil
}

// recommendedRow serves a TV Recommended row of two shows and an episode
// with an episode-order service over cat, and returns the rendered row.
func recommendedRow(t *testing.T, f *fixture, cat core.Catalog, token string, setup func(*episodeorder.Service)) []model.Metadata {
	t.Helper()
	db, e := store.OpenMemory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	s := episodeorder.New(db, twoDiscSource{}, cat)
	if setup != nil {
		setup(s)
	}
	f.srv.d.EpisodeOrder = s
	f.srv.d.Recommender = &fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeShow: {{
		ID: "tv.recommended", Title: "Recommended", Type: core.TypeShow, Context: "hub.tv.recommended",
		Items: []core.Title{breakingBad, unorderedShow, bbE1},
	}}}}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.request(t, http.MethodGet, "/hubs/sections/2?count=20", map[string]string{"Accept": "application/json", "X-Plex-Token": token})
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recommended row blocked on per-tile season loads")
	}
	var body struct{ MediaContainer model.MediaContainer }
	if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil || rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, h := range body.MediaContainer.Hub {
		if h.HubIdentifier == "tv.recommended" {
			return h.Metadata
		}
	}
	t.Fatalf("no Recommended row: %s", rec.Body.String())
	return nil
}

var unorderedShow = core.Title{Key: core.ShowKey(4242), Type: core.TypeShow, IDs: core.IDs{TMDB: 4242}, Title: "Harbour Lights", ChildCount: 3, LeafCount: 30}

func tileOf(t *testing.T, row []model.Metadata, k core.RatingKey) model.Metadata {
	t.Helper()
	for _, m := range row {
		if m.RatingKey == k.String() {
			return m
		}
	}
	t.Fatalf("%s missing from row", k)
	return model.Metadata{}
}

func TestListRenderDoesNotLoadSeasonsOfUnorderedShows(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.catalog.titles[unorderedShow.Key] = unorderedShow
	cat := &blockingCatalog{Catalog: f.catalog, block: map[core.RatingKey]bool{breakingBad.Key: true, unorderedShow.Key: true}, release: make(chan struct{})}
	defer close(cat.release)
	row := recommendedRow(t, f, cat, ownerToken, nil)
	if n := cat.loads.Load(); n != 0 {
		t.Fatalf("list loaded seasons %d times", n)
	}
	for _, show := range []core.Title{breakingBad, unorderedShow} {
		if m := tileOf(t, row, show.Key); m.ChildCount == nil || *m.ChildCount != show.ChildCount {
			t.Fatalf("%s childCount = %v, want catalogue %d", show.Title, m.ChildCount, show.ChildCount)
		}
	}
	if m := tileOf(t, row, bbE1.Key); m.ParentRatingKey != bbS1.Key.String() || *m.Index != 1 {
		t.Fatalf("canonical episode tile: parent %s index %v", m.ParentRatingKey, *m.Index)
	}
}

func TestListRenderAppliesConfiguredEpisodeGroups(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.catalog.titles[unorderedShow.Key] = unorderedShow
	// Only the show without an order would block: the grouped show loads.
	cat := &blockingCatalog{Catalog: f.catalog, block: map[core.RatingKey]bool{unorderedShow.Key: true}, release: make(chan struct{})}
	defer close(cat.release)
	save := func(s *episodeorder.Service) {
		ctx := context.Background()
		if e := s.Save(ctx, 0, breakingBad.Key, episodeorder.Preference{GroupID: "dvd", Specials: "last"}); e != nil {
			t.Fatal(e)
		}
		// The friend keeps the canonical order the owner's default replaces.
		if e := s.Save(ctx, friendUser.ID, breakingBad.Key, episodeorder.Preference{Specials: "last"}); e != nil {
			t.Fatal(e)
		}
	}
	row := recommendedRow(t, f, cat, ownerToken, save)
	if m := tileOf(t, row, breakingBad.Key); m.ChildCount == nil || *m.ChildCount != 2 {
		t.Fatalf("grouped show childCount = %v, want its 2 discs", m.ChildCount)
	}
	if m := tileOf(t, row, unorderedShow.Key); m.ChildCount == nil || *m.ChildCount != 3 {
		t.Fatalf("unordered show childCount = %v", m.ChildCount)
	}
	disc2 := core.SeasonKey(breakingBad.Key.TMDB(), 9001)
	if m := tileOf(t, row, bbE1.Key); m.ParentRatingKey != disc2.String() || *m.ParentIndex != 2 || *m.Index != 1 || m.ParentTitle != "Disc 2" {
		t.Fatalf("grouped episode tile: parent %s %v index %v %q", m.ParentRatingKey, *m.ParentIndex, *m.Index, m.ParentTitle)
	}
	if n := cat.loads.Load(); n != 0 {
		t.Fatalf("unordered show loaded seasons %d times", n)
	}

	f = newFixture()
	defer f.srv.Close()
	f.catalog.titles[unorderedShow.Key] = unorderedShow
	cat = &blockingCatalog{Catalog: f.catalog, block: map[core.RatingKey]bool{breakingBad.Key: true, unorderedShow.Key: true}, release: make(chan struct{})}
	defer close(cat.release)
	row = recommendedRow(t, f, cat, friendToken, save)
	if m := tileOf(t, row, breakingBad.Key); m.ChildCount == nil || *m.ChildCount != 1 {
		t.Fatalf("friend's canonical childCount = %v", m.ChildCount)
	}
	if m := tileOf(t, row, bbE1.Key); m.ParentRatingKey != bbS1.Key.String() {
		t.Fatalf("friend's episode moved to %s", m.ParentRatingKey)
	}
}
