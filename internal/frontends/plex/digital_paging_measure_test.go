package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type measureFeeds struct{}

func (measureFeeds) FeedSources() []core.ReleaseFeed { return nil }
func (measureFeeds) Recent(context.Context, string, int, int) ([]core.Release, error) {
	return nil, nil
}

type measureValidator struct{}

func (measureValidator) FeedCandidate(_ context.Context, _ *core.Title, r core.Release) (core.Release, string, error) {
	return r, "", nil
}
func (measureValidator) VerifyFeedCandidate(context.Context, *core.Title, core.Release) error {
	return nil
}

type measureCatalog struct {
	*catalog.Catalog
	a *availability.Service
}

func (c measureCatalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	f, err := c.a.PagingFilter(ctx, q.Filter)
	if err != nil {
		return core.Page{}, err
	}
	q.Filter = f
	return c.Catalog.Browse(ctx, q)
}
func (measureCatalog) BrowseAvailabilityInSQL() bool { return true }

// Opt-in offline measurement; excluded from the ordinary package test budget.
func TestDigitalLargeCatalogueLatency(t *testing.T) {
	if os.Getenv("ETHER_285_MEASURE") == "" {
		t.Skip("set ETHER_285_MEASURE=1")
	}
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	db := discoveryDB(t)
	now := fixedNow
	cfg := config.Defaults(t.TempDir())
	cfg.Availability.Mode = config.ModeDigital
	cfg.Availability.ApplyMode()
	c, err := catalog.New(db, nil, config.Catalog{ReleaseMode: config.ModeDigital}, nil, catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	base := int64(core.MovieKey(1)) - 1
	start := time.Now()
	_, err = db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<300000) INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,popularity,rating,year) SELECT ?+n,'movie',printf('Movie %06d',n),printf('Movie %06d',n),?,1,300001-n,8,2025 FROM seq`, base, now.Add(-24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO catalog_home_release(key,checked,earliest_ts) SELECT key,1,? FROM catalog_titles WHERE key<=?`, now.Add(-180*24*time.Hour).Unix(), base+299400)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed: 300000 movies (299400 settled digital dates, 600 unknown), %s", time.Since(start))
	a, err := availability.New(availability.Deps{DB: db, Config: func() config.Config { return cfg }, Feeds: measureFeeds{}, Catalog: c, Validator: measureValidator{}, Now: func() time.Time { return now }, Async: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	wrapped := measureCatalog{c, a}
	f.srv.setCatalog(wrapped)
	f.srv.d.DB = db
	f.srv.setFilterAvailable(a.FilterDiscovery)
	keys := make([]core.RatingKey, 200)
	for i := range keys {
		keys[i] = core.MovieKey(int64(i + 1))
	}
	titles, err := c.CachedTitles(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.setRecommender(&fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.measure", Type: core.TypeMovie, Title: "Measure", Items: titles}}}})
	measure := func(name string, run func(int)) {
		start := time.Now()
		run(0)
		cold := time.Since(start)
		times := make([]time.Duration, 20)
		for i := range times {
			start = time.Now()
			run(i + 1)
			times[i] = time.Since(start)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s cold=%s p50=%s p95=%s (20 requests)", name, cold, times[10], times[18])
	}
	measure("section paging", func(i int) {
		p, e := wrapped.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle, Offset: i * 50, Limit: 50})
		if e != nil || len(p.Items) != 50 || p.Total != 299400 {
			t.Fatalf("page %d: %d %v", i, len(p.Items), e)
		}
		for j, title := range p.Items {
			if title.Key != core.MovieKey(int64(i*50+j+1)) {
				t.Fatalf("page %d position %d: duplicate/gap at %v", i, j, title.Key)
			}
		}
	})
	measure("/hubs/sections/1", func(i int) {
		res := f.get(t, fmt.Sprintf("/hubs/sections/1?count=12&measure=%d", i), "Accept", "application/json")
		if res.Code != 200 {
			t.Fatalf("hubs: %d %s", res.Code, res.Body.String())
		}
	})
	res := f.get(t, "/hubs/sections/1", "Accept", "application/json")
	var screen struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(res.Body.Bytes(), &screen); err != nil {
		t.Fatal(err)
	}
	var rowKey string
	for _, h := range screen.MediaContainer.Hub {
		if h.HubIdentifier == "movie.measure" {
			rowKey = h.Key
		}
	}
	if rowKey == "" {
		t.Fatal("measure row missing")
	}
	waitShelfWrites(t, f.srv)
	measure("row paging (50 items)", func(i int) {
		u, _ := url.Parse(rowKey)
		q := u.Query()
		q.Set("X-Plex-Container-Start", fmt.Sprint((i%4)*50))
		q.Set("X-Plex-Container-Size", "50")
		u.RawQuery = q.Encode()
		response := f.get(t, u.String(), "Accept", "application/json")
		var page struct{ MediaContainer model.MediaContainer }
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || page.MediaContainer.Size != 50 || page.MediaContainer.TotalSize == nil || *page.MediaContainer.TotalSize != 200 {
			t.Fatalf("row: %d %s", response.Code, response.Body.String())
		}
	})
	measure("row paging (shared-store recovery, 50 items)", func(i int) {
		f.srv.shelfMu.Lock()
		f.srv.shelfGenerations = nil
		f.srv.shelfMu.Unlock()
		u, _ := url.Parse(rowKey)
		q := u.Query()
		q.Set("X-Plex-Container-Start", fmt.Sprint((i%4)*50))
		q.Set("X-Plex-Container-Size", "50")
		u.RawQuery = q.Encode()
		response := f.get(t, u.String(), "Accept", "application/json")
		var page struct{ MediaContainer model.MediaContainer }
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || page.MediaContainer.Size != 50 || page.MediaContainer.TotalSize == nil || *page.MediaContainer.TotalSize != 200 {
			t.Fatalf("row: %d %s", response.Code, response.Body.String())
		}
	})
}
