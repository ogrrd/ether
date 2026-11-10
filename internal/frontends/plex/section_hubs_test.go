package plex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

// A library section keeps Plex's conventions (Continue Watching, then
// Recently Added) above the section's own recommended rows, whose hub ids
// keep the movie.<kind> / tv.<kind> scheme, and mixed discovery rows show
// there with only that section's titles.
func TestSectionHubsOrderAndMixedRows(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	db := discoveryDB(t)
	wl := watchlist.New(db, watchlist.Options{Catalog: f.catalog})
	if err := wl.SaveSettings(ctx, watchlist.Settings{Enabled: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	owner := &core.User{ID: 1, IsOwner: true}
	for _, k := range []core.RatingKey{fightClub.Key, bbE1.Key} { // released titles only
		if err := wl.Remind(ctx, owner, k); err != nil {
			t.Fatal(err)
		}
	}
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: time.Minute, LastViewedAt: fixedNow})
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbE1.Key, ViewOffset: time.Minute, LastViewedAt: fixedNow})
	f.srv.setDiscovery((&discovery.Features{Watchlist: wl, Overlays: overlays.New(db, overlays.Options{})}).Frontend())
	f.srv.setRecommender(&fakeRecommender{section: map[core.MediaType][]core.Hub{
		core.TypeMovie: {
			{ID: "movie.cluster.thriller.heist", Title: "Heist Thrillers", Type: core.TypeMovie, Context: "hub.movie.cluster", Items: []core.Title{fightClub}},
			{ID: "movie.era.1970", Title: "Best of the '70s for You", Type: core.TypeMovie, Context: "hub.movie.era", Items: []core.Title{alien}},
		},
		core.TypeShow: {
			{ID: "tv.newseasons", Title: "New Seasons of Your Shows", Type: core.TypeShow, Context: "hub.tv.newseasons", Items: []core.Title{breakingBad}},
		},
	}})
	hubsOf := func(path string) []model.Hub {
		t.Helper()
		rec := f.request(t, "GET", path, map[string]string{"Accept": "application/json"})
		var body struct{ MediaContainer model.MediaContainer }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v %s", path, err, rec.Body.String())
		}
		return body.MediaContainer.Hub
	}
	ids := func(hs []model.Hub) []string {
		var out []string
		for _, h := range hs {
			out = append(out, h.HubIdentifier)
		}
		return out
	}

	movies := hubsOf("/hubs/sections/1")
	want := []string{"movie.inprogress.1", "movie.recentlyadded.1", "movie.cluster.thriller.heist", "movie.era.1970", "watchlist.coming"}
	if got := ids(movies); len(got) != len(want) {
		t.Fatalf("movies hubs = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("movies hubs = %v, want %v", got, want)
			}
		}
	}
	wlRow := movies[4]
	if wlRow.Type != "movie" || len(wlRow.Metadata) != 1 || wlRow.Metadata[0].RatingKey != fightClub.Key.String() {
		t.Fatalf("watchlist row on Movies should hold only films: type %q, %d items", wlRow.Type, len(wlRow.Metadata))
	}
	if movies[2].Context != "hub.movie.cluster" {
		t.Fatalf("lens context = %q", movies[2].Context)
	}
	// The row's "More" fetch is the section's copy too.
	rec := f.request(t, "GET", wlRow.Key, map[string]string{"Accept": "application/json"})
	var more struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &more); err != nil || len(more.MediaContainer.Metadata) != 1 || more.MediaContainer.Metadata[0].RatingKey != fightClub.Key.String() {
		t.Fatalf("section watchlist fetch %s: %s", wlRow.Key, rec.Body.String())
	}

	shows := hubsOf("/hubs/sections/2")
	got := ids(shows)
	if len(got) < 4 || got[0] != "tv.inprogress.2" || got[1] != "tv.recentlyadded.2" || got[2] != "tv.newseasons" || got[3] != "watchlist.coming" {
		t.Fatalf("shows hubs = %v", got)
	}
	if len(shows[3].Metadata) != 1 || shows[3].Metadata[0].RatingKey != bbE1.Key.String() {
		t.Fatalf("watchlist row on TV Shows should hold only series: %d items", len(shows[3].Metadata))
	}
	// The Home keeps the mixed row whole.
	for _, h := range hubsOf("/hubs") {
		if h.HubIdentifier == "watchlist.coming" && len(h.Metadata) != 2 {
			t.Fatalf("Home watchlist row has %d items", len(h.Metadata))
		}
	}
}

func TestSectionViewKeepsRecapsOnHome(t *testing.T) {
	recap := core.Hub{ID: "discovery.review.2025", Context: "hub.home.yearinreview", Items: []core.Title{fightClub, breakingBad}}
	if _, ok := core.SectionView(recap, core.TypeMovie); ok {
		t.Fatal("Year in Review on a section")
	}
	mixed := core.Hub{ID: "discovery.rediscover", Items: []core.Title{fightClub, bbE1, breakingBad}}
	v, ok := core.SectionView(mixed, core.TypeShow)
	if !ok || v.Type != core.TypeShow || len(v.Items) != 2 {
		t.Fatalf("show view = %+v %v", v, ok)
	}
	if _, ok := core.SectionView(core.Hub{Items: []core.Title{breakingBad}}, core.TypeMovie); ok {
		t.Fatal("empty section view kept")
	}
	if _, ok := core.SectionView(core.Hub{Type: core.TypeMovie, Items: []core.Title{fightClub}}, core.TypeMovie); !ok {
		t.Fatal("typed hub dropped")
	}
}
