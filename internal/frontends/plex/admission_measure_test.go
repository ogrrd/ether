package plex

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type admissionCounter struct {
	*catalog.Catalog
	loads, batches, predicates int
}

func (c *admissionCounter) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	c.loads++
	return c.Catalog.Get(ctx, k)
}
func (c *admissionCounter) OpenableTitles(ctx context.Context, ts []core.Title) []core.Title {
	c.batches++
	if old, ok := any(c.Catalog).(interface {
		OpenableTitles(context.Context, []core.Title) []core.Title
	}); ok {
		return old.OpenableTitles(ctx, ts)
	}
	return ts
}
func (c *admissionCounter) TitleOpenable(t *core.Title) bool {
	c.predicates++
	if pure, ok := any(c.Catalog).(interface{ TitleOpenable(*core.Title) bool }); ok {
		return pure.TitleOpenable(t)
	}
	return true
}

// Opt-in measurement, uses the existing realistic 300k-title WAL fixture.
func TestAdmissionRequestCost(t *testing.T) {
	if os.Getenv("ETHER_285_MEASURE") == "" {
		t.Skip("set ETHER_285_MEASURE=1")
	}
	f, db, _ := persistenceFixture(t)
	if _, err := db.Exec(`UPDATE catalog_titles SET poster='/cost.jpg' WHERE key=?`, core.MovieKey(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.catalog().(*catalog.Catalog).RefreshStoredTitle(context.Background(), core.MovieKey(1)); err != nil {
		t.Fatal(err)
	}
	counter := &admissionCounter{Catalog: f.srv.catalog().(*catalog.Catalog)}
	f.srv.setCatalog(counter)
	rec := f.srv.recommender().(*fakeRecommender)
	rec.home = rec.section[core.TypeMovie]
	key := core.MovieKey(1).String()
	for _, target := range []struct{ name, path string }{
		{"metadata", "/library/metadata/" + key},
		{"thumb", "/library/metadata/" + key + "/thumb"},
		{"photo", "/photo/:/transcode?url=/library/metadata/" + key + "/thumb&width=200&height=300"},
		{"play-start", "/video/:/transcode/universal/start.m3u8?path=/library/metadata/" + key + "&session=admission-cost&protocol=hls&directPlay=0&directStream=1"},
		{"hubs", "/hubs"},
		{"section-hubs", "/hubs/sections/1"},
	} {
		for sample := 0; sample < 3; sample++ {
			counter.loads = 0
			counter.batches = 0
			counter.predicates = 0
			ctx, stats := store.WithQueryStats(context.Background())
			req := httptest.NewRequest("GET", target.path, nil).WithContext(ctx)
			req.Header.Set("X-Plex-Token", ownerToken)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("X-Plex-Client-Identifier", "cost")
			response := httptest.NewRecorder()
			start := time.Now()
			f.srv.ServeHTTP(response, req)
			t.Logf("COST %s sample=%d status=%d queries=%d writes=%d rows=%d get=%d batches=%d predicates=%d elapsed=%s", target.name, sample, response.Code, stats.Queries(), stats.Writes(), stats.Rows(), counter.loads, counter.batches, counter.predicates, time.Since(start))
			if stats.Writes() > 0 {
				for _, st := range stats.Statements() {
					t.Logf("SQL %s count=%d", st.SQL, st.Count)
				}
			}
		}
	}
}

// An unmirrored title remains readable with the SQLite writer held. This also
// measures the cold row-fact lookup separately from the pure admission check.
func TestAdmissionUnmirroredCost(t *testing.T) {
	if os.Getenv("ETHER_285_MEASURE") == "" {
		t.Skip("set ETHER_285_MEASURE=1")
	}
	f, db, _ := persistenceFixture(t)
	c := f.srv.catalog().(*catalog.Catalog)
	missing := fightClub
	missing.Key = core.MovieKey(1662858)
	rec := f.srv.recommender().(*fakeRecommender)
	rec.home = []core.Hub{{ID: "collection.unmirrored", Type: core.TypeMovie, Items: []core.Title{missing}}}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { tx.Rollback(); c.WaitRefreshes() }()
	if _, err = tx.Exec(`INSERT INTO catalog_meta(k,v) VALUES('held-for-admission','1')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hubs", "/library/metadata/" + missing.Key.String(), "/library/metadata/" + missing.Key.String() + "/thumb", "/photo/:/transcode?url=/library/metadata/" + missing.Key.String() + "/thumb&width=200&height=300"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ctx, stats := store.WithQueryStats(ctx)
		req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Plex-Token", ownerToken)
		response := httptest.NewRecorder()
		start := time.Now()
		f.srv.ServeHTTP(response, req)
		cancel()
		t.Logf("MISSING path=%s status=%d queries=%d writes=%d rows=%d elapsed=%s", path, response.Code, stats.Queries(), stats.Writes(), stats.Rows(), time.Since(start))
		if response.Code != 200 || stats.Writes() != 0 {
			t.Fatalf("read-through status=%d writes=%d: %s", response.Code, stats.Writes(), response.Body.String())
		}
	}
}
