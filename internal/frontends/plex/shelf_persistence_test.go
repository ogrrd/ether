package plex

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func waitShelfWrites(t *testing.T, s *Server) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.shelfWriter.mu.Lock()
		idle := len(s.shelfWriter.pending) == 0 && len(s.shelfWriter.active) == 0
		s.shelfWriter.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-timeout.C:
			t.Fatal("shelf writer did not become idle")
		case <-tick.C:
		}
	}
}

func persistenceFixture(t *testing.T) (*fixture, *sql.DB, *atomic.Int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "shelves.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := &atomic.Int64{}
	clock.Store(fixedNow.Unix())
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return time.Unix(clock.Load(), 0) }))
	if err != nil {
		t.Fatal(err)
	}
	titleCount := 2000
	if os.Getenv("ETHER_285_MEASURE") != "" {
		titleCount = 300000
	}
	base := int64(core.MovieKey(1)) - 1
	_, err = db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?) INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,popularity,rating,year) SELECT ?+n,'movie',printf('Movie %06d',n),printf('Movie %06d',n),?,1,2001-n,8,2000 FROM seq`, titleCount, base, fixedNow.Add(-365*24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecommender{section: map[core.MediaType][]core.Hub{}}
	for row := 0; row < 10; row++ {
		keys := []core.RatingKey{}
		for i := 0; i < 200; i++ {
			keys = append(keys, core.MovieKey(int64(row*200+i+1)))
		}
		titles, err := c.CachedTitles(ctx, keys)
		if err != nil {
			t.Fatal(err)
		}
		rec.section[core.TypeMovie] = append(rec.section[core.TypeMovie], core.Hub{ID: fmt.Sprintf("collection.persistence.%d", row), Title: "Owner list", Type: core.TypeMovie, Items: titles})
	}
	f := newFixture(func(d *Deps) {
		d.DB = db
		d.Catalog = c
		d.Recommender = rec
		d.DisableEnrichment = true
		setDepsNow(d, func() time.Time { return time.Unix(clock.Load(), 0) })
	})
	t.Cleanup(func() { f.srv.Close() })
	return f, db, clock
}

func measuredShelfRequest(t *testing.T, f *fixture, client string) (time.Duration, *store.QueryStats) {
	t.Helper()
	ctx, stats := store.WithQueryStats(context.Background())
	r := httptest.NewRequest("GET", "/hubs/sections/1", nil).WithContext(ctx)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-Plex-Token", ownerToken)
	r.Header.Set("X-Plex-Client-Identifier", client)
	w := httptest.NewRecorder()
	start := time.Now()
	f.srv.ServeHTTP(w, r)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("section: %d %s", w.Code, w.Body.String())
	}
	return elapsed, stats
}

// Persistence is a bounded, lossy acceleration of shared continuity: screen
// batching, lazy refresh, coalescing and overload must never stall a request.
func TestShelfPersistenceBatching(t *testing.T) {
	f, db, clock := persistenceFixture(t)
	w := f.srv.shelfWriter
	_, stats := measuredShelfRequest(t, f, "one")
	if stats.Writes() != 0 {
		t.Fatal("request synchronously wrote shelves")
	}
	waitShelfWrites(t, f.srv)
	if w.commits.Load() != 1 || w.rows.Load() != 10 {
		t.Fatalf("cold screen: commits=%d rows=%d", w.commits.Load(), w.rows.Load())
	}
	_, stats = measuredShelfRequest(t, f, "one")
	waitShelfWrites(t, f.srv)
	if stats.Writes() != 0 || w.commits.Load() != 1 {
		t.Fatal("warm repeat wrote")
	}
	clock.Add(int64((6 * time.Minute) / time.Second))
	measuredShelfRequest(t, f, "one")
	waitShelfWrites(t, f.srv)
	if w.commits.Load() != 1 {
		t.Fatal("expired five-minute cut refreshed a seven-day generation")
	}
	// Evict the local cache: the writer must discover existing shared rows and
	// perform no INSERT/UPDATE or transaction for them.
	f.srv.shelfMu.Lock()
	f.srv.shelfCuts = nil
	f.srv.shelfGenerations = nil
	f.srv.shelfBytes = 0
	f.srv.shelfMu.Unlock()
	measuredShelfRequest(t, f, "one")
	waitShelfWrites(t, f.srv)
	if w.commits.Load() != 1 {
		t.Fatal("existing shared generations were rewritten")
	}
	// Force the last day of retention, then verify one batched refresh and its
	// per-generation hourly guard, even with stale expiry information.
	expire := func() {
		t.Helper()
		if _, err := db.Exec(`UPDATE plex_shelf_generations SET expires_at=?`, clock.Load()+int64((12*time.Hour)/time.Second)); err != nil {
			t.Fatal(err)
		}
		f.srv.shelfMu.Lock()
		for key, g := range f.srv.shelfGenerations {
			g.expires = clock.Load() + int64((12*time.Hour)/time.Second)
			g.lastPersist = time.Unix(clock.Load(), 0).Add(-2 * time.Hour)
			f.srv.shelfGenerations[key] = g
		}
		f.srv.shelfCuts = nil
		f.srv.shelfMu.Unlock()
	}
	expire()
	measuredShelfRequest(t, f, "one")
	waitShelfWrites(t, f.srv)
	if w.commits.Load() != 2 || w.rows.Load() != 20 {
		t.Fatalf("lazy refresh: commits=%d rows=%d", w.commits.Load(), w.rows.Load())
	}
	f.srv.shelfMu.Lock()
	for key, g := range f.srv.shelfGenerations {
		g.expires = clock.Load() + 60
		f.srv.shelfGenerations[key] = g
	}
	f.srv.shelfCuts = nil
	f.srv.shelfMu.Unlock()
	measuredShelfRequest(t, f, "one")
	waitShelfWrites(t, f.srv)
	if w.commits.Load() != 2 {
		t.Fatal("refresh repeated within one hour")
	}
	// Oversized admission drops the whole offered batch without waiting or SQL.
	records := map[string]shelfWrite{}
	for i := 0; i <= shelfPendingLimit; i++ {
		key := fmt.Sprint(i)
		records[key] = shelfWrite{generation: key}
	}
	if f.srv.enqueueShelves(records) || w.dropped.Load() != shelfPendingLimit+1 {
		t.Fatal("unbounded persistence queue")
	}
	// During the publication window, another node falls back successfully
	// while the single writer is held; the producing node retains its own cut.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE plex_shelf_generations SET expires_at=expires_at`); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Accept": "application/json", "X-Plex-Client-Identifier": "unpublished"}
	response := f.request(t, "GET", "/hubs/sections/1", headers)
	screen := decodeShelfContainer(t, response.Body.Bytes())
	var continuation string
	for _, h := range screen.Hub {
		if h.HubIdentifier == "collection.persistence.0" {
			continuation = h.Key
		}
	}
	if continuation == "" {
		t.Fatal("missing unpublished row")
	}
	b := newFixture(func(d *Deps) {
		d.DB = db
		d.Catalog = f.srv.d.Catalog
		d.Recommender = f.srv.d.Recommender
		d.DisableEnrichment = true
		setDepsNow(d, f.srv.now)
	})
	t.Cleanup(func() { b.srv.Close() })
	if got := b.request(t, "GET", continuation, headers); got.Code != 200 {
		t.Fatalf("publication-gap fallback: %d", got.Code)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitShelfWrites(t, f.srv)
	waitShelfWrites(t, b.srv)
	t.Log("cold screen: request writes=0, background transactions=1, generation inserts=10; warm/cut-expired/shared-existing repeats: transactions=0; near-expiry refresh: one transaction, hourly guard held")
}

// Opt-in measurement with ten 200-item shelves and a real WAL catalogue.
// The competing transaction remains locked for the entire contended sample.
func TestShelfPersistenceWriteContention(t *testing.T) {
	if os.Getenv("ETHER_285_MEASURE") == "" {
		t.Skip("set ETHER_285_MEASURE=1")
	}
	f, db, _ := persistenceFixture(t)
	measure := func(name string, unique bool) {
		t.Helper()
		times := make([]time.Duration, 20)
		before := f.srv.shelfWriter.commits.Load()
		for i := range times {
			client := "warm"
			if unique {
				client = fmt.Sprintf("%s-%d", name, i)
			}
			elapsed, stats := measuredShelfRequest(t, f, client)
			if stats.Writes() != 0 {
				t.Fatalf("%s request wrote", name)
			}
			times[i] = elapsed
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s: p50=%s p95=%s request_writes=0 background_commits_during_sample=%d", name, times[10], times[18], f.srv.shelfWriter.commits.Load()-before)
	}
	start := time.Now()
	measuredShelfRequest(t, f, "warm")
	waitShelfWrites(t, f.srv)
	t.Logf("ten-shelf cold build: background_commits=%d generation_rows=%d response-plus-publication=%s", f.srv.shelfWriter.commits.Load(), f.srv.shelfWriter.rows.Load(), time.Since(start))
	measure("warm/uncontended", false)
	measure("cold/uncontended", true)
	waitShelfWrites(t, f.srv)
	measuredShelfRequest(t, f, "warm")
	waitShelfWrites(t, f.srv)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE plex_shelf_generations SET expires_at=expires_at`); err != nil {
		t.Fatal(err)
	}
	measure("warm/writer-locked", false)
	measure("cold/writer-locked", true)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitShelfWrites(t, f.srv)
	t.Logf("queue dropped=%d (bounded at %d pending plus one active batch)", f.srv.shelfWriter.dropped.Load(), shelfPendingLimit)
}
