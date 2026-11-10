package plex

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Per-directory responses are independently fetched and stacked by Plex clients.
func TestHomeDirectoryPartition(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{
		{ID: "home.trending", Title: "Trending", Style: "hero", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.top10", Title: "Top", Promoted: true, Items: []core.Title{alien}},
	}, section: map[core.MediaType][]core.Hub{
		core.TypeMovie: {{ID: "movie.popular", Title: "Movies", Type: core.TypeMovie, Items: []core.Title{fightClub}}},
		core.TypeShow:  {{ID: "tv.popular", Title: "TV", Type: core.TypeShow, Items: []core.Title{breakingBad}}},
	}})
	if _, err := f.pls.Create(context.Background(), ownerUser.ID, "List", []core.RatingKey{fightClub.Key}); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"1,2,playlists", "playlists,2", "1,2"} {
		seen := map[string]string{}
		for _, dir := range strings.Split(pin, ",") {
			path := "/hubs/promoted?excludeContinueWatching=1&contentDirectoryID=" + url.QueryEscape(dir) + "&pinnedContentDirectoryID=" + url.QueryEscape(pin)
			rec := f.get(t, path, jsonAccept...)
			var body struct {
				MediaContainer struct {
					Hub []model.Hub `json:"Hub"`
				} `json:"MediaContainer"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, h := range body.MediaContainer.Hub {
				if prior := seen[h.HubIdentifier]; prior != "" {
					t.Fatalf("pin=%s duplicate %s in %s and %s", pin, h.HubIdentifier, prior, dir)
				}
				seen[h.HubIdentifier] = dir
				if h.Style == "hero" || h.HubIdentifier == "home.continue" {
					t.Fatalf("unexpected row: %+v", h)
				}
			}
		}
		first := strings.Split(pin, ",")[0]
		for _, id := range []string{"home.trending", "home.top10"} {
			if seen[id] != first {
				t.Fatalf("pin=%s %s placed in %q", pin, id, seen[id])
			}
		}
		if strings.Contains(pin, "1") && seen["movie.popular"] != "1" {
			t.Fatalf("pin=%s movie placement: %v", pin, seen)
		}
		if seen["tv.popular"] != "2" {
			t.Fatalf("pin=%s TV placement: %v", pin, seen)
		}
		if strings.Contains(pin, "playlists") && seen["home.playlists"] != "playlists" {
			t.Fatalf("pin=%s playlist placement: %v", pin, seen)
		}
	}
	if rec := f.get(t, "/hubs/continueWatching?contentDirectoryID=playlists", jsonAccept...); rec.Code != 404 {
		t.Fatalf("playlist-only continue: %d", rec.Code)
	}
	for _, path := range []string{"/hubs?contentDirectoryID=unknown", "/hubs/promoted?contentDirectoryID=unknown"} {
		if ids := hubIDs(t, f.get(t, path, jsonAccept...).Body.Bytes()); len(ids) != 0 {
			t.Fatalf("%s leaked rows: %v", path, ids)
		}
	}
}

func TestHomeDirectoryContinueFilter(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, ws(fightClub.Key, 60000, 0))
	f.state.set(ownerUser.ID, ws(bbE1.Key, 60000, 0))
	for _, tc := range []struct {
		dir  string
		want map[string]bool
	}{
		{"1", map[string]bool{fightClub.Key.String(): true}},
		{"2", map[string]bool{bbE1.Key.String(): true}},
		{"playlists,2", map[string]bool{bbE1.Key.String(): true}},
		{"1,2,playlists", map[string]bool{fightClub.Key.String(): true, bbE1.Key.String(): true}},
	} {
		mc := decodeJSON(t, f.get(t, "/hubs/continueWatching?contentDirectoryID="+url.QueryEscape(tc.dir), jsonAccept...))
		hubs := mc["Hub"].([]any)
		items, _ := hubs[0].(map[string]any)["Metadata"].([]any)
		got := map[string]bool{}
		for _, item := range items {
			got[item.(map[string]any)["ratingKey"].(string)] = true
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v want %v", tc.dir, got, tc.want)
		}
		for key := range tc.want {
			if !got[key] {
				t.Fatalf("%s: got %v want %v", tc.dir, got, tc.want)
			}
		}
	}
}

type hiddenHomeRows struct{ *fakeRecommender }

func (*hiddenHomeRows) HomeRowEnabled(context.Context, *core.User, core.HubContext, string) bool {
	return false
}

func (*hiddenHomeRows) FinalizeNeutralHome(_ context.Context, _ *core.User, _ core.HomeSituation, _ string, rows []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	out := rows[:0]
	for _, row := range rows {
		if row.ID != "home.trending" && row.ID != "home.continue" {
			out = append(out, row)
		}
	}
	return out, nil
}

func TestHomeDirectoryRespectsHiddenRows(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&hiddenHomeRows{&fakeRecommender{home: []core.Hub{
		{ID: "home.trending", Title: "Trending", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.top10", Title: "Top", Promoted: true, Items: []core.Title{alien}},
	}}})
	f.state.set(ownerUser.ID, ws(fightClub.Key, 60000, 0))
	for _, dir := range []string{"1", "2", "playlists"} {
		ids := hubIDs(t, f.get(t, "/hubs/promoted?contentDirectoryID="+dir+"&pinnedContentDirectoryID=1,2,playlists", jsonAccept...).Body.Bytes())
		for _, id := range ids {
			if id == "home.trending" || id == "home.continue" {
				t.Fatalf("hidden %s in %s", id, dir)
			}
		}
	}
	if rec := f.get(t, "/hubs/continueWatching?contentDirectoryID=1,2", jsonAccept...); rec.Code != 404 {
		t.Fatalf("hidden continue endpoint returned %d", rec.Code)
	}
}
