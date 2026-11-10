package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSectionDateSortRoutesUseAirDates(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	date := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	a, b := core.ShowKey(901), core.ShowKey(902)
	season := core.SeasonKey(901, 1)
	items := []core.Title{
		{Key: a, Type: core.TypeShow, Title: "Alpha", Released: date(2000)},
		{Key: b, Type: core.TypeShow, Title: "Beta", Released: date(2010)},
		{Key: season, Type: core.TypeSeason, ShowKey: a, Season: 1, Title: "Season 1", Released: date(2001)},
		{Key: core.EpisodeKey(901, 1, 1), Type: core.TypeEpisode, ShowKey: a, Season: 1, Episode: 1, Title: "Aired", Released: date(2024)},
		{Key: core.EpisodeKey(901, 1, 2), Type: core.TypeEpisode, ShowKey: a, Season: 1, Episode: 2, Title: "Future", Released: date(2030)},
		{Key: core.EpisodeKey(902, 1, 1), Type: core.TypeEpisode, ShowKey: b, Season: 1, Episode: 1, Title: "Earlier", Released: date(2020)},
	}
	for _, item := range items {
		if err := c.UpsertTitle(ctx, item, item.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	// Separate assignments keep the bound timestamps typed. A CASE over
	// untyped parameters is text on PostgreSQL and cannot land in fetched_at.
	for _, set := range []struct {
		key  core.RatingKey
		when int64
	}{{a, date(2021).Unix()}, {b, date(2025).Unix()}} {
		if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET fetched_at=? WHERE key=?`, set.when, int64(set.key)); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(func(d *testServerDeps) { d.Catalog = c })
	defer f.srv.Close()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct{ MediaContainer model.MediaContainer }
		if rec.Code != 200 {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}
	for _, tc := range []struct {
		path string
		keys []core.RatingKey
	}{
		{"/library/sections/2/recentlyAdded", []core.RatingKey{a, b}},
		{"/library/sections/2/all?sort=addedAt:asc", []core.RatingKey{b, a}},
		{"/library/sections/2/all?sort=addedAt:desc", []core.RatingKey{a, b}},
		{"/library/sections/2/all?sort=updatedAt:desc", []core.RatingKey{b, a}},
		{"/library/sections/2/all", []core.RatingKey{a, b}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			mc := decode(t, f.get(t, tc.path, jsonAccept...))
			if len(mc.Metadata) != len(tc.keys) {
				t.Fatalf("%s count=%d", tc.path, len(mc.Metadata))
			}
			for i, key := range tc.keys {
				if mc.Metadata[i].RatingKey != key.String() {
					t.Fatalf("%s order at %d got %s want %s", tc.path, i, mc.Metadata[i].RatingKey, key)
				}
			}
		})
	}
	rec := decode(t, f.get(t, fmt.Sprintf("/library/metadata/%s", season), jsonAccept...))
	if len(rec.Metadata) != 1 || rec.Metadata[0].AddedAt != date(2024).Unix() || rec.Metadata[0].OriginallyAvailableAt != "" {
		t.Fatalf("season latest aired: %+v", rec.Metadata)
	}
}

func TestSectionAllLeavesAndSeasonRoutesForTracearr(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	date := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	showA, showB := core.ShowKey(901), core.ShowKey(902)
	sA1, sB1 := core.SeasonKey(901, 1), core.SeasonKey(902, 1)
	eA1 := core.EpisodeKey(901, 1, 1)
	eA2 := core.EpisodeKey(901, 1, 2)
	eAFuture := core.EpisodeKey(901, 1, 3)
	eB1 := core.EpisodeKey(902, 1, 1)

	movie := core.Title{
		Key: core.MovieKey(550), Type: core.TypeMovie, IDs: core.IDs{TMDB: 550, IMDB: "tt0137523"},
		Title: "Fjord Harbor", Year: 1999, Released: date(1999, 10, 15), Runtime: 139 * time.Minute,
	}
	if err := c.UpsertTitle(ctx, movie, true); err != nil {
		t.Fatal(err)
	}
	items := []core.Title{
		{Key: showA, Type: core.TypeShow, IDs: core.IDs{TMDB: 901, TVDB: 81189}, Title: "Alpha", Year: 2020, Released: date(2020, 1, 1), ContentRating: "TV-14"},
		{Key: showB, Type: core.TypeShow, IDs: core.IDs{TMDB: 902, TVDB: 81190}, Title: "Beta", Year: 2021, Released: date(2021, 1, 1), ContentRating: "TV-MA"},
		{Key: sA1, Type: core.TypeSeason, ShowKey: showA, IDs: core.IDs{TMDB: 901}, Season: 1, Title: "Season 1", Released: date(2020, 1, 1), PosterPath: "/sa1.jpg"},
		{Key: sB1, Type: core.TypeSeason, ShowKey: showB, IDs: core.IDs{TMDB: 902}, Season: 1, Title: "Season 1", Released: date(2021, 1, 1), PosterPath: "/sb1.jpg"},
		{Key: eA1, Type: core.TypeEpisode, ShowKey: showA, IDs: core.IDs{TMDB: 901, IMDB: "tt9010101", TVDB: 9010101}, Season: 1, Episode: 1, Title: "A1", Released: date(2020, 1, 2), Runtime: 45 * time.Minute, StillPath: "/ea1.jpg"},
		{Key: eA2, Type: core.TypeEpisode, ShowKey: showA, IDs: core.IDs{TMDB: 901, IMDB: "tt9010102", TVDB: 9010102}, Season: 1, Episode: 2, Title: "A2", Released: date(2024, 5, 1), Runtime: 46 * time.Minute, StillPath: "/ea2.jpg"},
		{Key: eAFuture, Type: core.TypeEpisode, ShowKey: showA, IDs: core.IDs{TMDB: 901}, Season: 1, Episode: 3, Title: "Future", Released: date(2030, 1, 1)},
		{Key: eB1, Type: core.TypeEpisode, ShowKey: showB, IDs: core.IDs{TMDB: 902, IMDB: "tt9020101", TVDB: 9020101}, Season: 1, Episode: 1, Title: "B1", Released: date(2022, 3, 15), Runtime: 50 * time.Minute, StillPath: "/eb1.jpg"},
	}
	for _, it := range items {
		if err := c.UpsertTitle(ctx, it, it.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	u1, u2, u3 := date(2025, 1, 1).Unix(), date(2025, 6, 1).Unix(), date(2026, 2, 1).Unix()
	for _, set := range []struct {
		key  core.RatingKey
		when int64
	}{
		{sA1, u1}, {sB1, u3},
		{eA1, u1}, {eB1, u2}, {eA2, u3},
	} {
		if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET fetched_at=? WHERE key=?`, set.when, int64(set.key)); err != nil {
			t.Fatal(err)
		}
	}

	f := newFixture(func(d *testServerDeps) { d.Catalog = c })
	defer f.srv.Close()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct{ MediaContainer model.MediaContainer }
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}

	// 1. TV section /allLeaves and /all?type=4 return all 3 aired episodes in key order.
	for _, path := range []string{
		"/library/sections/2/allLeaves?includeGuids=1",
		"/library/sections/2/all?type=4&includeGuids=1",
		"/library/all?type=4&includeGuids=1",
	} {
		mc := decode(t, f.get(t, path, jsonAccept...))
		if mc.ViewGroup != "episode" || mc.Title2 != "All Episodes" {
			t.Fatalf("%s container headers: viewGroup=%q title2=%q", path, mc.ViewGroup, mc.Title2)
		}
		if mc.TotalSize == nil || *mc.TotalSize != 3 || mc.Size != 3 {
			t.Fatalf("%s counts: totalSize=%v size=%d", path, mc.TotalSize, mc.Size)
		}
		want := []core.RatingKey{eA1, eA2, eB1}
		for i, k := range want {
			m := mc.Metadata[i]
			if m.RatingKey != k.String() || m.Type != "episode" {
				t.Fatalf("%s[%d] got %s (%s) want %s", path, i, m.RatingKey, m.Type, k)
			}
			if m.ParentRatingKey == "" || m.GrandparentRatingKey == "" || m.GrandparentTitle == "" || m.Index == nil || m.ParentIndex == nil {
				t.Fatalf("%s[%d] missing hierarchy attrs: %+v", path, i, m)
			}
			if len(m.Guid) == 0 {
				t.Fatalf("%s[%d] missing includeGuids Guid array: %+v", path, i, m)
			}
			if len(m.Media) == 0 || len(m.Media[0].Part) == 0 {
				t.Fatalf("%s[%d] missing Media/Part: %+v", path, i, m)
			}
		}
	}

	// 2. Pagination via query params and headers (Tracearr probes 0,1 then pages).
	probe := decode(t, f.request(t, http.MethodGet, "/library/sections/2/allLeaves?includeGuids=1&X-Plex-Container-Start=0&X-Plex-Container-Size=1", map[string]string{"Accept": "application/json"}))
	if probe.TotalSize == nil || *probe.TotalSize != 3 || probe.Offset == nil || *probe.Offset != 0 || probe.Size != 1 || probe.Metadata[0].RatingKey != eA1.String() {
		t.Fatalf("episode probe: %+v", probe)
	}
	page2 := decode(t, f.request(t, http.MethodGet, "/library/sections/2/allLeaves?includeGuids=1", map[string]string{
		"Accept":                 "application/json",
		"X-Plex-Container-Start": "1",
		"X-Plex-Container-Size":  "2",
	}))
	if page2.TotalSize == nil || *page2.TotalSize != 3 || page2.Offset == nil || *page2.Offset != 1 || page2.Size != 2 ||
		page2.Metadata[0].RatingKey != eA2.String() || page2.Metadata[1].RatingKey != eB1.String() {
		t.Fatalf("episode page 2: %+v", page2)
	}

	// 3. TV section /all?type=3 returns all 2 aired seasons with accurate leafCount and parent metadata.
	seasonsMC := decode(t, f.request(t, http.MethodGet, "/library/sections/2/all?type=3&includeGuids=1", map[string]string{
		"Accept":                 "application/json",
		"X-Plex-Container-Start": "0",
		"X-Plex-Container-Size":  "100",
	}))
	if seasonsMC.ViewGroup != "season" || seasonsMC.Title2 != "All Seasons" || seasonsMC.TotalSize == nil || *seasonsMC.TotalSize != 2 || seasonsMC.Size != 2 {
		t.Fatalf("seasons container: %+v", seasonsMC)
	}
	if seasonsMC.Metadata[0].RatingKey != sA1.String() || seasonsMC.Metadata[0].ParentRatingKey != showA.String() ||
		seasonsMC.Metadata[0].ParentTitle != "Alpha" || seasonsMC.Metadata[0].ParentYear != 2020 ||
		seasonsMC.Metadata[0].LeafCount == nil || *seasonsMC.Metadata[0].LeafCount != 2 ||
		seasonsMC.Metadata[0].AddedAt != date(2024, 5, 1).Unix() || len(seasonsMC.Metadata[0].Guid) == 0 {
		t.Fatalf("season A1 metadata: %+v", seasonsMC.Metadata[0])
	}
	if seasonsMC.Metadata[1].RatingKey != sB1.String() || seasonsMC.Metadata[1].LeafCount == nil || *seasonsMC.Metadata[1].LeafCount != 1 {
		t.Fatalf("season B1 metadata: %+v", seasonsMC.Metadata[1])
	}

	// 4. Movie section /allLeaves returns movies.
	movieLeaves := decode(t, f.get(t, "/library/sections/1/allLeaves?includeGuids=1", jsonAccept...))
	if movieLeaves.ViewGroup != "movie" || movieLeaves.Size != 1 || movieLeaves.Metadata[0].RatingKey != movie.Key.String() || len(movieLeaves.Metadata[0].Guid) == 0 {
		t.Fatalf("movie allLeaves: %+v", movieLeaves)
	}

	// 5. Incremental sync sorts and filters: sort=updatedAt:desc, sort=addedAt:desc, updatedAt>=, updatedAt>>=, updatedAt>, addedAt>=.
	incEp := decode(t, f.get(t, "/library/sections/2/allLeaves?includeGuids=1&sort=updatedAt:desc", jsonAccept...))
	if len(incEp.Metadata) != 3 || incEp.Metadata[0].RatingKey != eA2.String() || incEp.Metadata[1].RatingKey != eB1.String() || incEp.Metadata[2].RatingKey != eA1.String() {
		t.Fatalf("allLeaves sort=updatedAt:desc: %+v", incEp.Metadata)
	}
	incSe := decode(t, f.get(t, "/library/sections/2/all?type=3&includeGuids=1&sort=updatedAt:desc", jsonAccept...))
	if len(incSe.Metadata) != 2 || incSe.Metadata[0].RatingKey != sB1.String() || incSe.Metadata[1].RatingKey != sA1.String() {
		t.Fatalf("seasons sort=updatedAt:desc: %+v", incSe.Metadata)
	}
	for _, q := range []string{
		"updatedAt>=" + strconv.FormatInt(u2, 10),
		"updatedAt>>=" + strconv.FormatInt(u2, 10),
		"updatedAt>" + strconv.FormatInt(u1, 10),
	} {
		filtered := decode(t, f.get(t, "/library/sections/2/allLeaves?sort=updatedAt:desc&"+q, jsonAccept...))
		if len(filtered.Metadata) != 2 || filtered.Metadata[0].RatingKey != eA2.String() || filtered.Metadata[1].RatingKey != eB1.String() {
			t.Fatalf("filter %s: %+v", q, filtered.Metadata)
		}
	}
	addedFiltered := decode(t, f.get(t, "/library/sections/2/allLeaves?sort=addedAt:desc&addedAt>=2022-01-01", jsonAccept...))
	if len(addedFiltered.Metadata) != 2 || addedFiltered.Metadata[0].RatingKey != eA2.String() || addedFiltered.Metadata[1].RatingKey != eB1.String() {
		t.Fatalf("addedAt filter: %+v", addedFiltered.Metadata)
	}

	// 6. Count agreement between section shows, per-show allLeaves, and section allLeaves.
	showsMC := decode(t, f.get(t, "/library/sections/2/all", jsonAccept...))
	sumLeaves := 0
	for _, sh := range showsMC.Metadata {
		if sh.LeafCount == nil {
			t.Fatalf("show %s missing leafCount", sh.RatingKey)
		}
		perShow := decode(t, f.get(t, "/library/metadata/"+sh.RatingKey+"/allLeaves", jsonAccept...))
		if perShow.Size != *sh.LeafCount {
			t.Fatalf("show %s leafCount=%d != allLeaves=%d", sh.RatingKey, *sh.LeafCount, perShow.Size)
		}
		sumLeaves += *sh.LeafCount
	}
	if sumLeaves != *probe.TotalSize {
		t.Fatalf("sum of show leafCounts %d != section allLeaves totalSize %d", sumLeaves, *probe.TotalSize)
	}
}
