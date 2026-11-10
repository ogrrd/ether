package plex

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func TestRecommendationShelvesHaveDistinctFetchPaths(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeMovie: {
		{ID: "movie.trending", Title: "Trending", Type: core.TypeMovie, Items: []core.Title{fightClub}},
		{ID: "movie.popular", Title: "Popular", Type: core.TypeMovie, Items: []core.Title{fightClub}},
		{ID: "movie.because.1", Title: "Because You Watched", Type: core.TypeMovie, Items: []core.Title{fightClub}},
	}}})
	var body struct{ MediaContainer model.MediaContainer }
	rec := f.request(t, "GET", "/hubs/sections/1", map[string]string{"Accept": "application/json"})
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, h := range body.MediaContainer.Hub {
		u, _ := url.Parse(h.Key)
		if paths[u.Path] {
			t.Fatalf("shelves collapse to one client entity: %s", u.Path)
		}
		paths[u.Path] = true
		if h.Title == "Trending" || h.Title == "Popular" || h.Title == "Because You Watched" {
			item := f.request(t, "GET", h.Key, map[string]string{"Accept": "application/json"})
			var fetched struct{ MediaContainer model.MediaContainer }
			if err := json.Unmarshal(item.Body.Bytes(), &fetched); err != nil || len(fetched.MediaContainer.Metadata) != 1 || fetched.MediaContainer.Metadata[0].RatingKey != fightClub.Key.String() || len(fetched.MediaContainer.Hub) != 0 {
				t.Fatalf("shelf fetch failed: %s %s", h.Key, item.Body.String())
			}
		}
	}
}

func TestShelfFetchReportsPageSizeOffsetAndTotal(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	third := fightClub
	third.Key = core.MovieKey(551)
	items := []core.Title{fightClub, alien, third}
	f.srv.setRecommender(&fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.popular", Title: "Popular", Type: core.TypeMovie, Items: items}}}})
	rec := f.request(t, "GET", "/hubs/items/movie.popular?contentDirectoryID=1&X-Plex-Container-Start=1&X-Plex-Container-Size=1", map[string]string{"Accept": "application/json"})
	var fetched struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	mc := fetched.MediaContainer
	if mc.Size != 1 || mc.Offset == nil || *mc.Offset != 1 || mc.TotalSize == nil || *mc.TotalSize != 3 || len(mc.Metadata) != 1 || mc.Metadata[0].RatingKey != alien.Key.String() {
		t.Fatalf("invalid shelf page: %s", rec.Body.String())
	}
}
