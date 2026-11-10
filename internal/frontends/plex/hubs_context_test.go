package plex

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// contextualRecommender records the contexts it is asked for and the
// impressions it is sent.
type contextualRecommender struct {
	fakeRecommender
	mu          sync.Mutex
	homeCtx     []core.HubContext
	sectionCtx  []core.HubContext
	impressions [][]core.Hub
	impCtx      []core.HubContext
}

func (r *contextualRecommender) HomeHubsFor(_ context.Context, _ *core.User, hc core.HubContext, _ int) ([]core.Hub, error) {
	r.mu.Lock()
	r.homeCtx = append(r.homeCtx, hc)
	r.mu.Unlock()
	return r.home, nil
}

func (r *contextualRecommender) SectionHubsFor(_ context.Context, _ *core.User, t core.MediaType, hc core.HubContext, _ int) ([]core.Hub, error) {
	r.mu.Lock()
	r.sectionCtx = append(r.sectionCtx, hc)
	r.mu.Unlock()
	return r.section[t], nil
}

func (r *contextualRecommender) RecordImpressions(_ *core.User, hc core.HubContext, hubs []core.Hub) {
	r.mu.Lock()
	r.impressions = append(r.impressions, hubs)
	r.impCtx = append(r.impCtx, hc)
	r.mu.Unlock()
}

func hubIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var v struct {
		MediaContainer struct {
			Hub []struct {
				HubIdentifier string `json:"hubIdentifier"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range v.MediaContainer.Hub {
		out = append(out, h.HubIdentifier)
	}
	return out
}

func newContextualFixture(t *testing.T, rec *contextualRecommender) *fixture {
	t.Helper()
	f := newFixture()
	f.srv.Close()
	d := f.srv.currentTestDeps()
	d.Recommender = rec
	f.srv = newTestServer(d)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func TestHubsUseContextualRecommender(t *testing.T) {
	rec := &contextualRecommender{fakeRecommender: fakeRecommender{
		home: []core.Hub{
			{ID: "home.top10", Title: "Top 10 for You", Promoted: true, Items: []core.Title{fightClub}},
			{ID: "home.continue", Title: "Continue Watching", Promoted: true},
			{ID: "home.trending", Title: "Trending", Promoted: true, Items: []core.Title{alien}},
		},
		section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.top10", Title: "Top 10", Type: core.TypeMovie, Items: []core.Title{alien}}}},
	}}
	f := newContextualFixture(t, rec)
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: 60_000, LastViewedAt: fixedNow})

	resp := f.get(t, "/hubs", "Accept", "application/json", "X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS")
	got := hubIDs(t, resp.Body.Bytes())
	want := []string{"home.top10", "home.continue", "home.trending"}
	if len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("hub order = %v, want Continue Watching where the recommender ranked it: %v", got, want)
	}
	rec.mu.Lock()
	if len(rec.homeCtx) != 1 || rec.homeCtx[0].Device != core.DeviceTV || rec.homeCtx[0].Daypart == "" {
		t.Fatalf("home context = %+v, want a tv context with a daypart", rec.homeCtx)
	}
	if len(rec.impressions) != 1 {
		t.Fatalf("impressions recorded %d times, want 1", len(rec.impressions))
	}
	var impIDs []string
	for _, h := range rec.impressions[0] {
		impIDs = append(impIDs, h.ID)
	}
	rec.mu.Unlock()
	if len(impIDs) != 3 || impIDs[0] != "home.top10" || impIDs[1] != "home.continue" || impIDs[2] != "home.trending" {
		t.Fatalf("impressions = %v, want the served rows in order", impIDs)
	}

	// Section rows get the section in their context; the front end's own
	// rows are recorded as impressions too.
	f.get(t, "/hubs/sections/1", "Accept", "application/json", "X-Plex-Product", "Plex for iOS", "X-Plex-Platform", "iOS")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sectionCtx) != 1 || rec.sectionCtx[0].SectionID != 1 || rec.sectionCtx[0].Device != core.DeviceMobile {
		t.Fatalf("section context = %+v", rec.sectionCtx)
	}
	last := rec.impressions[len(rec.impressions)-1]
	if len(last) != 3 || last[0].ID != "movie.inprogress.1" || last[2].ID != "movie.top10" {
		var ids []string
		for _, h := range last {
			ids = append(ids, h.ID)
		}
		t.Fatalf("section impressions = %v", ids)
	}
}

func TestImpressionCreditFollowsMainDuplicateRules(t *testing.T) {
	// main 4588b9893 recordImpressions credits served[hubIdentifier]. Home
	// skips a second recommender row whose rendered id was already seen, so
	// the first arm is the one recorded. Section adds overwrite, so the last
	// add is credited even when the visible row is the first copy.
	rec := &contextualRecommender{fakeRecommender: fakeRecommender{
		home: []core.Hub{
			{ID: "home.trending", Title: "First", Promoted: true, Items: []core.Title{fightClub}},
			{ID: "home.trending", Title: "Second", Promoted: true, Items: []core.Title{alien}},
		},
	}}
	f := newContextualFixture(t, rec)
	f.get(t, "/hubs", "Accept", "application/json")
	rec.mu.Lock()
	if len(rec.impressions) != 1 {
		t.Fatalf("home impressions recorded %d times", len(rec.impressions))
	}
	var trending []core.Hub
	for _, h := range rec.impressions[0] {
		if h.ID == "home.trending" {
			trending = append(trending, h)
		}
		if h.ID == "home.playlists" || h.ID == "livetv" {
			t.Fatalf("non-credited row recorded: %+v", h)
		}
	}
	rec.mu.Unlock()
	if len(trending) != 1 || len(trending[0].Items) != 1 || trending[0].Items[0].Key != fightClub.Key {
		t.Fatalf("home credit = %+v, want the first arm", trending)
	}

	section := &contextualRecommender{fakeRecommender: fakeRecommender{
		section: map[core.MediaType][]core.Hub{core.TypeMovie: {
			{ID: "movie.top10", Title: "First", Type: core.TypeMovie, Items: []core.Title{fightClub}},
			{ID: "movie.top10", Title: "Second", Type: core.TypeMovie, Items: []core.Title{alien}},
		}},
	}}
	sf := newContextualFixture(t, section)
	sf.get(t, "/hubs/sections/1", "Accept", "application/json")
	section.mu.Lock()
	defer section.mu.Unlock()
	if len(section.impressions) != 1 {
		t.Fatalf("section impressions recorded %d times", len(section.impressions))
	}
	var top []core.Hub
	for _, h := range section.impressions[0] {
		if h.ID == "movie.top10" {
			top = append(top, h)
		}
	}
	if len(top) != 1 || len(top[0].Items) != 1 || top[0].Items[0].Key != alien.Key {
		t.Fatalf("section credit = %+v, want the last add", top)
	}
}

func TestHubsContinueFirstWithoutPlaceholder(t *testing.T) {
	rec := &contextualRecommender{fakeRecommender: fakeRecommender{
		home: []core.Hub{{ID: "home.trending", Title: "Trending", Promoted: true, Items: []core.Title{fightClub}}},
	}}
	f := newContextualFixture(t, rec)
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: 60_000, LastViewedAt: fixedNow})
	got := hubIDs(t, f.get(t, "/hubs", "Accept", "application/json").Body.Bytes())
	if len(got) < 2 || got[0] != "home.continue" || got[1] != "home.trending" {
		t.Fatalf("hubs = %v, want Continue Watching first", got)
	}
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key})
	got = hubIDs(t, f.get(t, "/hubs/promoted", "Accept", "application/json").Body.Bytes())
	for _, id := range got {
		if id == "home.continue" {
			t.Fatalf("empty Continue Watching stayed on promoted: %v", got)
		}
	}
	got = hubIDs(t, f.get(t, "/hubs?excludeContinueWatching=1", "Accept", "application/json").Body.Bytes())
	for _, id := range got {
		if id == "home.continue" {
			t.Fatalf("excludeContinueWatching still returned it: %v", got)
		}
	}
	// identifier filtering limits the impressions to the rows returned.
	f.get(t, "/hubs?identifier=home.trending", "Accept", "application/json")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	last := rec.impressions[len(rec.impressions)-1]
	if len(last) != 1 || last[0].ID != "home.trending" {
		t.Fatalf("filtered impressions = %+v", last)
	}
}
