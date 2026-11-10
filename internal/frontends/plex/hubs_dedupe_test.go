package plex

import (
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// The front end builds Continue Watching itself (PMS's shape, with watch
// state); a recommender that also returns it must not produce two rows.
func TestHomeHubsDropRecommenderDuplicates(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: 60_000, LastViewedAt: fixedNow})
	d := f.srv.currentTestDeps()
	d.Recommender = &fakeRecommender{home: []core.Hub{
		{ID: "home.continue", Title: "Continue Watching", Context: "hub.home.continue", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "trending.week.movie", Title: "Trending This Week", Type: core.TypeMovie, Context: "hub.movie.trending", Promoted: true, Items: []core.Title{fightClub}},
	}}
	f.srv = newTestServer(d)
	defer f.srv.Close()

	rec := f.get(t, "/hubs", "Accept", "application/json")
	var body struct {
		MediaContainer struct {
			Hub []struct {
				HubIdentifier string `json:"hubIdentifier"`
				Title         string `json:"title"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, h := range body.MediaContainer.Hub {
		seen[h.HubIdentifier]++
	}
	if seen["home.continue"] != 1 {
		t.Errorf("home.continue appears %d times, want 1 (%v)", seen["home.continue"], body.MediaContainer.Hub)
	}
	if seen["trending.week.movie"] != 1 {
		t.Errorf("distinct recommender hub lost: %v", body.MediaContainer.Hub)
	}
}
