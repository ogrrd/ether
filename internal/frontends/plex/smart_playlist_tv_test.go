package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// A show predicate and an episode predicate apply at different levels; the
// playlist's count, pages, and queue expansion must all use the same set.
func TestSmartPlaylistTVSeriesAndEpisodePredicates(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	cat, err := catalog.New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat.SetNow(func() time.Time { return fixedNow })
	for _, show := range []core.Title{
		{Key: core.ShowKey(77), Type: core.TypeShow, IDs: core.IDs{TMDB: 77}, Title: "Signal", Year: 2019, Released: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama"}},
		{Key: core.ShowKey(78), Type: core.TypeShow, IDs: core.IDs{TMDB: 78}, Title: "Other", Year: 2020, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Comedy"}},
	} {
		if err := cat.UpsertTitle(ctx, show, true); err != nil {
			t.Fatal(err)
		}
		for n, y := range []int{2020, 2021} {
			ep := core.Title{Key: core.EpisodeKey(show.Key.TMDB(), 1, n+1), ShowKey: show.Key, Type: core.TypeEpisode, IDs: core.IDs{TMDB: show.Key.TMDB()}, Season: 1, Episode: n + 1, Title: "Episode", Year: y, Released: time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)}
			if err := cat.UpsertTitle(ctx, ep, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	ps := state.New(db, state.Options{}).Playlists()
	f := newFixture(func(d *testServerDeps) { d.Catalog = cat; d.Playlists = ps })
	defer f.srv.Close()
	create := func(uri string) string {
		t.Helper()
		rec := f.request(t, http.MethodPost, "/playlists?smart=1&type=video&title=TV&uri="+url.QueryEscape(uri), map[string]string{"Accept": "application/json"})
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		md := decodeJSON(t, rec)["Metadata"].([]any)[0].(map[string]any)
		if md["leafCount"] != float64(2) {
			t.Fatalf("leafCount: %v", md["leafCount"])
		}
		return md["ratingKey"].(string)
	}
	key := create("/library/sections/2/all?type=2&year=2019&and=1&genre=Drama")
	for page := 0; page < 2; page++ {
		rec := f.get(t, "/playlists/"+key+"/items", "Accept", "application/json", "X-Plex-Container-Start", string(rune('0'+page)), "X-Plex-Container-Size", "1")
		mc := decodeJSON(t, rec)
		if mc["totalSize"] != float64(2) || mc["leafCount"] != float64(2) {
			t.Fatalf("page count: %v", mc)
		}
		items := mc["Metadata"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["ratingKey"] != core.EpisodeKey(77, 1, page+1).String() {
			t.Fatalf("page %d: %v", page, items)
		}
	}
	keys, err := f.srv.expandKey(ctx, &Request{User: &ownerUser}, mustKey(key))
	if err != nil || len(keys) != 2 || keys[0] != core.EpisodeKey(77, 1, 1) || keys[1] != core.EpisodeKey(77, 1, 2) {
		t.Fatalf("queue: %v %v", keys, err)
	}
	episodeKey := create("/library/sections/2/all?type=4&year=2020")
	rec := f.get(t, "/playlists/"+episodeKey+"/items", jsonAccept...)
	items := decodeJSON(t, rec)["Metadata"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["ratingKey"] != core.EpisodeKey(77, 1, 1).String() || items[1].(map[string]any)["ratingKey"] != core.EpisodeKey(78, 1, 1).String() {
		t.Fatalf("episode filter: %v", items)
	}
}

// Membership is selected in SQL before parent deduplication, client filtering
// and pagination. A saved limit still applies to the ordered member set.
func TestSmartPlaylistProjectionBeyondExpansionPage(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	cat, err := catalog.New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat.SetNow(func() time.Time { return fixedNow })
	show := core.Title{Key: core.ShowKey(177), Type: core.TypeShow, IDs: core.IDs{TMDB: 177}, Title: "Series", Year: 2020, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := cat.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}
	for season := 1; season <= 2; season++ {
		key := core.SeasonKey(177, season)
		if err := cat.UpsertTitle(ctx, core.Title{Key: key, ShowKey: show.Key, Type: core.TypeSeason, Season: season, Title: fmt.Sprintf("Season%d", season), Released: show.Released, ChildCount: 550}, false); err != nil {
			t.Fatal(err)
		}
		count := 550
		if season == 2 {
			count = 2
		}
		for n := 1; n <= count; n++ {
			title := fmt.Sprintf("Episode%04d", n)
			if season == 2 {
				title = "Tail"
			}
			if err := cat.UpsertTitle(ctx, core.Title{Key: core.EpisodeKey(177, season, n), ShowKey: show.Key, Type: core.TypeEpisode, Season: season, Episode: n, Title: title, Year: 2020, Released: show.Released}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	ps := state.New(db, state.Options{}).Playlists()
	f := newFixture(func(d *testServerDeps) { d.Catalog = cat; d.Playlists = ps })
	defer f.srv.Close()
	create := func(uri string) string {
		t.Helper()
		rec := f.request(t, http.MethodPost, "/playlists?type=video&title=Large&smart=1&uri="+url.QueryEscape(uri), map[string]string{"Accept": "application/json"})
		if rec.Code != 200 {
			t.Fatalf("create%d %s", rec.Code, rec.Body.String())
		}
		return decodeJSON(t, rec)["Metadata"].([]any)[0].(map[string]any)["ratingKey"].(string)
	}
	key := create("/library/sections/2/all?type=4&sort=titleSort")
	probe := func(key, query string, total int) {
		t.Helper()
		if !strings.Contains(query, "X-Plex-Container-Start=") {
			query += "&X-Plex-Container-Start=0"
		}
		rec := f.get(t, "/playlists/"+key+"/items?"+query, jsonAccept...)
		if rec.Code != 200 {
			t.Fatalf("items%d %s", rec.Code, rec.Body.String())
		}
		mc := decodeJSON(t, rec)
		if mc["totalSize"] != float64(total) {
			t.Fatalf("query%s total: %v", query, mc)
		}
		if mc["size"] != float64(1) {
			t.Fatalf("query%s page: %v", query, mc)
		}
	}
	probe(key, "type=3&X-Plex-Container-Start=1&X-Plex-Container-Size=1", 2)
	probe(key, "type=4&title=Tail&X-Plex-Container-Size=1", 2)
	probe(key, "X-Plex-Container-Start=551&X-Plex-Container-Size=1", 552)
	limited := create("/library/sections/2/all?type=4&sort=titleSort&limit=550")
	probe(limited, "type=3&X-Plex-Container-Size=1", 1)
}
