package plex

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type shelfRow struct {
	HubIdentifier string `json:"hubIdentifier"`
	Context       string `json:"context"`
	Key           string `json:"key"`
	Title         string `json:"title"`
}

func shelfRows(t *testing.T, body []byte) []shelfRow {
	t.Helper()
	var v struct {
		MediaContainer struct {
			Hub []shelfRow `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.MediaContainer.Hub
}

// Infuse Direct Mode draws a library's promoted rows only when they are
// PMS's own kinds and folds any other row's items into the next row it
// draws. Its per-library request must lead with Recently Added, carry every
// Ether row as a promoted collection shelf, and show Home rows once.
func TestInfusePromotedRowsAreCollectionShelves(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{
		home:    []core.Hub{{ID: "home.trending", Title: "Trending Now", Promoted: true, Items: []core.Title{fightClub, alien, breakingBad}}},
		section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.toprated", Title: "Top Rated", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}}}},
	})
	infuse := append([]string{"X-Plex-Product", "Infuse-Direct", "X-Plex-Device", "Apple TV"}, jsonAccept...)
	const q = "/hubs/promoted?count=20&excludeContinueWatching=1&contentDirectoryID="

	movies := shelfRows(t, f.get(t, q+"1", infuse...).Body.Bytes())
	if len(movies) < 3 || movies[0].HubIdentifier != "movie.recentlyadded.1" {
		t.Fatalf("Recently Added must lead the library's rows: %+v", movies)
	}
	titles := map[string]bool{}
	for _, row := range movies[1:] {
		if !strings.HasPrefix(row.HubIdentifier, "custom.collection.1.") || row.Context != "hub.custom.collection" {
			t.Fatalf("row is not a promoted collection shelf: %+v", row)
		}
		titles[row.Title] = true
		// Infuse loads a shelf's cards from the collection its identifier names.
		parts := strings.Split(row.HubIdentifier, ".")
		for _, path := range []string{row.Key, "/library/collections/" + parts[len(parts)-1] + "/children"} {
			if rec := f.get(t, path, infuse...); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ratingKey"`) {
				t.Fatalf("shelf %s at %s: %d %s", row.Title, path, rec.Code, rec.Body.String())
			}
		}
	}
	if !titles["Trending Now"] || !titles["Top Rated"] {
		t.Fatalf("Ether rows missing: %v", titles)
	}
	again := shelfRows(t, f.get(t, q+"1", infuse...).Body.Bytes())
	for i := range movies {
		if i >= len(again) || again[i].HubIdentifier != movies[i].HubIdentifier {
			t.Fatalf("shelf identifiers must be stable: %+v then %+v", movies, again)
		}
	}
	for _, row := range shelfRows(t, f.get(t, q+"2", infuse...).Body.Bytes()) {
		if row.Title == "Trending Now" {
			t.Fatalf("Home row repeated in the second library: %+v", row)
		}
	}

	// Plex's own apps keep Ether's row identities.
	plex := shelfRows(t, f.get(t, q+"1", jsonAccept...).Body.Bytes())
	for _, row := range plex {
		if strings.HasPrefix(row.HubIdentifier, "custom.collection.") {
			t.Fatalf("non-Infuse client got a collection shelf: %+v", row)
		}
	}
}

// After a restart Infuse loads every remembered shelf at once, before it
// asks for the promoted rows again: each must still find its row.
func TestInfuseCollectionShelvesAfterRestart(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{{ID: "home.trending", Title: "Trending Now", Promoted: true, Items: []core.Title{fightClub, alien}}}})
	path := "/library/collections/" + collectionShelfNumber(model.Hub{HubIdentifier: "home.trending"}) + "/children"
	hdr := map[string]string{"X-Plex-Product": "Infuse-Direct", "Accept": "application/json"}
	codes := make(chan string, 6)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.request(t, "GET", path, hdr)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ratingKey"`) {
				codes <- rec.Body.String()
			}
		}()
	}
	wg.Wait()
	close(codes)
	for body := range codes {
		t.Fatalf("shelf not served after restart: %s", body)
	}
	if rec := f.get(t, path, jsonAccept...); rec.Code == 200 && strings.Contains(rec.Body.String(), `"ratingKey"`) {
		t.Fatalf("shelf numbers are for collection-shelf clients only")
	}
}
