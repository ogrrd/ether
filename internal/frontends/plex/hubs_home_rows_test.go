package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/personalize"
)

func hubRows(t *testing.T, body []byte) []struct {
	ID   string `json:"hubIdentifier"`
	Size int    `json:"size"`
} {
	t.Helper()
	var v struct {
		MediaContainer struct {
			Hub []struct {
				ID   string `json:"hubIdentifier"`
				Size int    `json:"size"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.MediaContainer.Hub
}

type paintedHub struct {
	ID    string `json:"hubIdentifier"`
	Title string `json:"title"`
	Style string `json:"style"`
	Size  int    `json:"size"`
	Art   []string
}

func paintedHomeHubs(t *testing.T, body []byte) []paintedHub {
	t.Helper()
	var v struct {
		MediaContainer struct {
			Hub []struct {
				ID       string `json:"hubIdentifier"`
				Title    string `json:"title"`
				Style    string `json:"style"`
				Size     int    `json:"size"`
				Metadata []struct {
					Art   string `json:"art"`
					Thumb string `json:"thumb"`
				} `json:"Metadata"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	out := make([]paintedHub, 0, len(v.MediaContainer.Hub))
	for _, h := range v.MediaContainer.Hub {
		row := paintedHub{ID: h.ID, Title: h.Title, Style: h.Style, Size: h.Size}
		for _, m := range h.Metadata {
			row.Art = append(row.Art, m.Art)
		}
		out = append(out, row)
	}
	return out
}

func iosHomeHeaders() []string {
	return []string{"Accept", "application/json", "X-Plex-Product", "Plex for iOS", "X-Plex-Platform", "iOS", "X-Plex-Device", "iPhone"}
}

func TestPromotedDropsEmptyRestrictedAndDuplicateHubs(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	blocked := fightClub
	blocked.Adult = true
	blocked.Title = "Not shown"
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{
		{ID: "home.movies.recent", Title: "Recent movies", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.movies.recent", Title: "Recent movies again", Promoted: true, Items: []core.Title{alien}},
		{ID: "home.blocked", Title: "Blocked", Promoted: true, Items: []core.Title{blocked}},
		{ID: "home.continue", Title: "Continue Watching", Promoted: true},
	}})

	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		rows := hubRows(t, f.get(t, path, "Accept", "application/json", "X-Plex-Product", "Plex for Android", "X-Plex-Platform", "Android").Body.Bytes())
		seen := map[string]int{}
		for _, h := range rows {
			if h.Size == 0 {
				t.Errorf("%s: empty hub %s", path, h.ID)
			}
			seen[h.ID]++
		}
		if seen["home.movies.recent"] != 1 {
			t.Errorf("%s: home.movies.recent count = %d, rows %+v", path, seen["home.movies.recent"], rows)
		}
		if seen["home.blocked"] != 0 || seen["home.continue"] != 0 {
			t.Errorf("%s: empty rows survived: %+v", path, rows)
		}
	}
}

func TestSectionHubIdentifierOnce(t *testing.T) {
	rec := &contextualRecommender{fakeRecommender: fakeRecommender{
		section: map[core.MediaType][]core.Hub{
			core.TypeMovie: {{ID: "movie.recentlyadded.1", Title: "Recently Added again", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}}},
		},
	}}
	f := newContextualFixture(t, rec)
	rows := hubRows(t, f.get(t, "/hubs/sections/1", "Accept", "application/json").Body.Bytes())
	n := 0
	for _, h := range rows {
		if h.ID == "movie.recentlyadded.1" {
			n++
		}
		if h.Size == 0 {
			t.Errorf("empty section hub %s", h.ID)
		}
	}
	if n != 1 {
		t.Fatalf("movie.recentlyadded.1 appears %d times: %+v", n, rows)
	}
}

func TestHomeRowOrderStableAcrossSessionAndCount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for i := int64(1); i <= 80; i++ {
		title := fightClub
		title.Key = core.MovieKey(10000 + i)
		title.Title = fmt.Sprintf("Comedy %d", i)
		title.Genres = []string{"Comedy"}
		title.Rating = 8.5
		title.VoteCount = 10000
		title.Popularity = 100
		f.catalog.titles[title.Key] = title
	}
	e, err := personalize.New(personalize.Options{DB: discoveryDB(t), Catalog: f.catalog, State: f.srv.state(), RefreshEvery: -1, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.srv.setRecommender(e)
	if err := e.Rebuild(context.Background(), &ownerUser); err != nil {
		t.Fatal(err)
	}
	headers := func(session string) []string {
		return []string{"Accept", "application/json", "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "SHIELD Android TV", "X-Plex-Session-Identifier", session}
	}
	first := hubRows(t, f.get(t, "/hubs?count=12", headers("session-a")...).Body.Bytes())
	second := hubRows(t, f.get(t, "/hubs?count=4", headers("session-b")...).Body.Bytes())
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("row counts %d and %d: %+v vs %+v", len(first), len(second), first, second)
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Size == 0 || second[i].Size == 0 {
			t.Fatalf("order changed or empty at %d: %+v vs %+v", i, first, second)
		}
	}
}

func TestIOSHomeOmitsColdAndArtlessHero(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{
		{ID: "home.trending", Title: "Trending Now", Promoted: true, Items: []core.Title{alien}},
		{ID: "home.because", Title: "Because You Watched", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.shortontime", Title: "Short on Time", Promoted: true, Items: []core.Title{alien}},
	}})

	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		rows := paintedHomeHubs(t, f.get(t, path, iosHomeHeaders()...).Body.Bytes())
		if len(rows) == 0 || rows[0].ID != "home.because" || rows[0].Style != "hero" || rows[0].Size == 0 {
			t.Fatalf("%s: cold/artless lead = %+v", path, rows)
		}
		for _, row := range rows {
			if row.ID == "home.continue" || row.ID == "home.trending" || row.Style == "hero" && row.ID != "home.because" {
				t.Fatalf("%s: reserved empty hero: %+v", path, rows)
			}
			if row.Size == 0 {
				t.Fatalf("%s: empty hub %s", path, row.ID)
			}
		}
		if rows[0].Art[0] == "" {
			t.Fatalf("%s: promoted hero has no art: %+v", path, rows[0])
		}
	}
}

func TestIOSHomeKeepsReadyHeroAndTrending(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: 60_000, LastViewedAt: fixedNow})
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{
		{ID: "home.trending", Title: "Trending Now", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.because", Title: "Because You Watched", Promoted: true, Items: []core.Title{alien}},
	}})

	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		rows := paintedHomeHubs(t, f.get(t, path, iosHomeHeaders()...).Body.Bytes())
		if len(rows) < 3 || rows[0].ID != "home.continue" || rows[0].Style != "hero" || rows[1].ID != "home.trending" || rows[1].Style != "shelf" {
			t.Fatalf("%s: ready rows = %+v", path, rows)
		}
		if rows[0].Size == 0 || rows[0].Art[0] == "" {
			t.Fatalf("%s: ready hero lost art: %+v", path, rows[0])
		}
	}
}

type brokenCollectionRows struct {
	calls atomic.Int32
	stall time.Duration
	err   error
}

func (b *brokenCollectionRows) HomeRowsFor(ctx context.Context, _ *core.User, _ core.HubContext) ([]core.CollectionRow, error) {
	b.calls.Add(1)
	if b.stall > 0 {
		select {
		case <-time.After(b.stall):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, b.err
}

func TestBrokenCollectionCannotStallOrEmptyHubs(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for i := int64(1); i <= 40; i++ {
		title := fightClub
		title.Key = core.MovieKey(20000 + i)
		title.Title = fmt.Sprintf("Sci-Fi %d", i)
		title.Genres = []string{"Science Fiction"}
		title.Rating = 8.5
		title.VoteCount = 10000
		title.Popularity = 100
		f.catalog.titles[title.Key] = title
	}
	broken := &brokenCollectionRows{stall: 2 * time.Second, err: errors.New("broken collection")}
	e, err := personalize.New(personalize.Options{
		DB:            discoveryDB(t),
		Catalog:       f.catalog,
		State:         f.srv.state(),
		Collections:   broken,
		SourceTimeout: 80 * time.Millisecond,
		RefreshEvery:  -1,
		FlushEvery:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.srv.setRecommender(e)
	if err := e.Rebuild(context.Background(), &ownerUser); err != nil {
		t.Fatal(err)
	}

	headers := []string{"Accept", "application/json", "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "SHIELD Android TV"}
	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		started := time.Now()
		res := f.get(t, path, headers...)
		elapsed := time.Since(started)
		if elapsed > 500*time.Millisecond {
			t.Fatalf("%s stalled for %v on broken collection", path, elapsed)
		}
		rows := hubRows(t, res.Body.Bytes())
		if len(rows) == 0 {
			t.Fatalf("%s returned empty hubs when collection was broken", path)
		}
	}
	// Wait for the background owner-row attempt to finish (bounded by SourceTimeout=80ms),
	// then verify repeated /hubs and /hubs/promoted calls are served from cache without
	// re-invoking HomeRowsFor.
	time.Sleep(150 * time.Millisecond)
	callsAfterFirst := broken.calls.Load()
	for i := 0; i < 3; i++ {
		for _, path := range []string{"/hubs", "/hubs/promoted"} {
			rows := hubRows(t, f.get(t, path, headers...).Body.Bytes())
			if len(rows) == 0 {
				t.Fatalf("%s call %d returned empty hubs", path, i)
			}
		}
	}
	if got := broken.calls.Load(); got != callsAfterFirst {
		t.Fatalf("HomeRowsFor called %d times on cached hubs, want %d", got, callsAfterFirst)
	}
}
