package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/store"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Every partition of a canonical row must reassemble exactly that row, even
// when another screen publishes a new generation between arbitrary requests.
// Inputs deliberately include mixed types, repeated source keys, different
// page sizes, retries, out-of-order pages, expiry and daypart changes.
func TestShelfGenerationPagingInvariant(t *testing.T) {
	for _, collection := range []bool{true, false} {
		for _, evidence := range []bool{false, true} {
			t.Run(fmt.Sprintf("collection=%t/evidence=%t", collection, evidence), func(t *testing.T) {
				shelfGenerationPagingInvariant(t, collection, evidence)
			})
		}
	}
}
func shelfGenerationPagingInvariant(t *testing.T, collection, evidence bool) {
	f := newFixture()
	defer f.srv.Close()
	// Background palette jobs read the clock while the test advances it.
	var now atomic.Int64
	now.Store(fixedNow.UnixNano())
	f.srv.setTestNow(func() time.Time { return time.Unix(0, now.Load()).In(fixedNow.Location()) })
	var mixed []core.Title
	for i := 0; i < 140; i++ {
		movie, show := fightClub, breakingBad
		movie.Key = core.MovieKey(int64(10000 + i))
		show.Key = core.ShowKey(int64(10000 + i))
		mixed = append(mixed, movie, show)
		f.catalog.titles[movie.Key] = movie
		f.catalog.titles[show.Key] = show
	}
	// Duplicates must disappear before offsets/counts, not per page.
	mixed = append(mixed[:30], append([]core.Title{mixed[4], mixed[4]}, mixed[30:]...)...)
	key := core.RatingKey(0)
	if collection {
		key = core.CollectionKey(123)
	}
	if collection {
		f.cols.cols[key] = core.Collection{Key: key, Title: "Mixed", ChildCount: len(mixed)}
		f.cols.items[key] = mixed
	}
	rec := &fakeRecommender{section: map[core.MediaType][]core.Hub{}}
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		rec.section[typ] = []core.Hub{{ID: "collection.mixed", Key: key, Title: "Mixed", Items: mixed}}
	}
	f.srv.d.Recommender = rec
	headers := map[string]string{"Accept": "application/json"}
	for k, vs := range conformplex.Clients()["web"].Headers {
		if k != "X-Plex-Token" {
			headers[k] = vs[0]
		}
	}
	get := func(path string) model.MediaContainer {
		t.Helper()
		response := f.request(t, "GET", path, headers)
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		return decodeShelfContainer(t, response.Body.Bytes())
	}
	row := func(section int) model.Hub {
		t.Helper()
		for _, h := range get(fmt.Sprintf("/hubs/sections/%d", section)).Hub {
			if h.HubIdentifier == "collection.mixed" {
				return h
			}
		}
		t.Fatal("mixed row missing")
		return model.Hub{}
	}
	keys := func(items []model.Metadata) []string {
		out := []string{}
		for _, item := range items {
			out = append(out, item.RatingKey)
		}
		return out
	}
	initial := []model.Hub{row(1), row(2)}
	for section, h := range initial {
		expected := []string{}
		for i := 0; i < 140; i++ {
			k := core.MovieKey(int64(10000 + i))
			if section == 1 {
				k = core.ShowKey(int64(10000 + i))
			}
			expected = append(expected, k.String())
		}
		if !reflect.DeepEqual(keys(h.Metadata), expected[:h.Size]) || h.Size != 12 || !h.More {
			t.Fatalf("initial section %d: size=%d more=%v keys=%v", section, h.Size, h.More, keys(h.Metadata))
		}
		if got := get(h.HubKey); !reflect.DeepEqual(keys(got.Metadata), expected[:h.Size]) {
			t.Fatal("hubKey changed visible subset")
		}
		for seed := int64(0); seed < 12; seed++ {
			rng := rand.New(rand.NewSource(seed))
			type page struct{ start, size int }
			pages := []page{}
			for start := h.Size; start < len(expected); {
				size := min(1+rng.Intn(50), len(expected)-start)
				pages = append(pages, page{start, size})
				start += size
			}
			rng.Shuffle(len(pages), func(i, j int) { pages[i], pages[j] = pages[j], pages[i] })
			assembled := append([]string(nil), expected[:h.Size]...)
			assembled = append(assembled, make([]string, len(expected)-h.Size)...)
			for _, p := range pages {
				now.Add(int64(6 * time.Hour)) // expires presentation/shelf caches AND crosses dayparts
				// New evidence changes discovery eligibility while the old generation
				// remains complete; section switching can publish the new answer.
				if evidence {
					f.srv.d.FilterAvailable = func(_ context.Context, titles []core.Title) []core.Title {
						out := []core.Title{}
						for _, title := range titles {
							if title.Key != mixed[0].Key {
								out = append(out, title)
							}
						}
						return out
					}
				}
				// Change membership and order, then switch sections and publish new screens.
				for typ, rows := range rec.section {
					items := append([]core.Title(nil), mixed...)
					if p.start%2 == 0 {
						for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
							items[i], items[j] = items[j], items[i]
						}
					}
					rows[0].Items = items
					rec.section[typ] = rows
				}
				row(2 - section)
				row(section + 1)
				u, _ := url.Parse(h.Key)
				q := u.Query()
				q.Set("X-Plex-Container-Start", fmt.Sprint(p.start))
				q.Set("X-Plex-Container-Size", fmt.Sprint(p.size))
				u.RawQuery = q.Encode()
				got := get(u.String())
				if got.Size != p.size || got.TotalSize == nil || *got.TotalSize != len(expected) || !reflect.DeepEqual(keys(got.Metadata), expected[p.start:p.start+p.size]) {
					t.Fatalf("seed=%d section=%d page=%+v size=%d total=%v keys=%v", seed, section, p, got.Size, got.TotalSize, keys(got.Metadata))
				}
				copy(assembled[p.start:], keys(got.Metadata))
				// Retry the exact page: it must be idempotent.
				if retry := get(u.String()); !reflect.DeepEqual(keys(retry.Metadata), keys(got.Metadata)) {
					t.Fatal("retry shifted")
				}
			}
			if !reflect.DeepEqual(assembled, expected) {
				t.Fatal("duplicate or gap in page union")
			}
		}
	}
	// A generation is not authorization; changing clients must fail closed.
	if response := f.request(t, "GET", initial[0].Key, map[string]string{"X-Plex-Client-Identifier": "another-client"}); response.Code != 404 {
		t.Fatalf("cross-client generation: %d", response.Code)
	}
	// An unrecoverable key serves the current route instead of breaking the shelf.
	f.srv.shelfMu.Lock()
	f.srv.shelfGenerations = nil
	f.srv.shelfMu.Unlock()
	if response := f.request(t, "GET", initial[0].Key, headers); response.Code != http.StatusOK {
		t.Fatalf("lost generation: %d", response.Code)
	}
}

func decodeShelfContainer(t *testing.T, data []byte) model.MediaContainer {
	t.Helper()
	var body struct {
		MediaContainer struct {
			Size      int
			TotalSize *int
			Offset    *int
			Hub       []model.Hub
			Metadata  []struct{ RatingKey string }
		}
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	wire := body.MediaContainer
	mc := model.MediaContainer{Size: wire.Size, TotalSize: wire.TotalSize, Offset: wire.Offset, Hub: wire.Hub}
	for _, item := range wire.Metadata {
		mc.Metadata = append(mc.Metadata, model.Metadata{RatingKey: item.RatingKey})
	}
	return mc
}

func TestBrowseScopeOmitsPagingAndCredentials(t *testing.T) {
	pr := &Request{ClientID: "client-one"}
	first := httptest.NewRequest("GET", "/library/sections/1/all?sort=titleSort&X-Plex-Container-Start=0&X-Plex-Container-Size=50&X-Plex-Token=secret", nil)
	next := httptest.NewRequest("GET", "/library/sections/1/all?sort=titleSort&X-Plex-Container-Start=50&X-Plex-Container-Size=100", nil)
	a, b := browseScope(first, pr), browseScope(next, pr)
	if a != b || strings.Contains(a, "secret") {
		t.Fatalf("unstable/credential-bearing scope: %q %q", a, b)
	}
	next.URL.RawQuery = "sort=addedAt"
	if a == browseScope(next, pr) {
		t.Fatal("sorts shared a sequence")
	}
}

// The same listing survives node changes, local eviction and a complete
// server/store close and reopen. Only ordered integer keys live in the store.
func TestShelfGenerationAcrossNodesAndRestart(t *testing.T) {
	for _, collection := range []bool{false, true} {
		t.Run(fmt.Sprint(collection), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "shared.db")
			db, err := store.OpenPath(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if db != nil {
					db.Close()
				}
			}()
			opts := func(counter uint64) func(*Deps) {
				return func(d *Deps) {
					d.DB = db
					d.DisableEnrichment = true
					d.HomeCache = &HomeCacheSource{Revision: func(context.Context, *core.User) (experience.HomeRevision, error) {
						return experience.HomeRevision{Viewer: "shared-viewer", Policy: 1}, nil
					}, Config: func() uint64 { return counter }}
				}
			}
			a := newFixture(opts(1))
			var mixed []core.Title
			for i := int64(0); i < 140; i++ {
				m, v := fightClub, breakingBad
				m.Key = core.MovieKey(50000 + i)
				v.Key = core.ShowKey(50000 + i)
				a.catalog.titles[m.Key] = m
				a.catalog.titles[v.Key] = v
				mixed = append(mixed, m, v)
			}
			rec := &fakeRecommender{section: map[core.MediaType][]core.Hub{}}
			for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
				key := core.RatingKey(0)
				if collection {
					key = core.SectionCollectionKey(core.SmartKey(777), typ)
					a.cols.cols[key] = core.Collection{Key: key, Type: typ, Title: "Mixed", ChildCount: 140}
					for _, title := range mixed {
						if title.Type == typ {
							a.cols.items[key] = append(a.cols.items[key], title)
						}
					}
				}
				rec.section[typ] = []core.Hub{{ID: "collection.shared", Key: key, Title: "Mixed", Items: mixed}}
			}
			a.srv.d.Recommender = rec
			b := newFixture(opts(99))
			b.srv.d.Catalog = a.catalog
			b.srv.d.Collections = a.cols
			b.srv.d.Recommender = rec
			closedA, closedB := false, false
			defer func() {
				if !closedA {
					a.srv.Close()
				}
				if !closedB {
					b.srv.Close()
				}
			}()
			get := func(f *fixture, raw string) model.MediaContainer {
				t.Helper()
				response := f.get(t, raw, "Accept", "application/json")
				if response.Code != 200 {
					t.Fatalf("node request %s: %d %s", raw, response.Code, response.Body.String())
				}
				return decodeShelfContainer(t, response.Body.Bytes())
			}
			initial := []model.Hub{}
			for _, section := range []int{1, 2} {
				for _, h := range get(a, fmt.Sprintf("/hubs/sections/%d", section)).Hub {
					if h.HubIdentifier == "collection.shared" {
						initial = append(initial, h)
					}
				}
			}
			if len(initial) != 2 {
				t.Fatal("missing shelves")
			}
			// Shared durability is asynchronous; after publication, recovery must
			// preserve the old order across nodes and restart.
			waitShelfWrites(t, a.srv)
			// Rebuilds now have the opposite order; a recovering node must not use them.
			for typ, rows := range rec.section {
				items := append([]core.Title(nil), mixed...)
				for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
					items[i], items[j] = items[j], items[i]
				}
				rows[0].Items = items
				rec.section[typ] = rows
			}
			totals := make([][]string, 2)
			page := func(f *fixture, h model.Hub, start, size int) []string {
				t.Helper()
				u, _ := url.Parse(h.Key)
				q := u.Query()
				q.Set("X-Plex-Container-Start", fmt.Sprint(start))
				q.Set("X-Plex-Container-Size", fmt.Sprint(size))
				u.RawQuery = q.Encode()
				mc := get(f, u.String())
				if mc.Size != size || mc.TotalSize == nil || *mc.TotalSize != 140 {
					t.Fatalf("bad recovered count: size=%d total=%v", mc.Size, mc.TotalSize)
				}
				out := []string{}
				for _, md := range mc.Metadata {
					out = append(out, md.RatingKey)
				}
				return out
			}
			for i, h := range initial {
				for _, md := range h.Metadata {
					totals[i] = append(totals[i], md.RatingKey)
				}
				totals[i] = append(totals[i], page(b, h, h.Size, 50)...)
			}
			// Shut down both nodes and the actual DB handle before constructing a fresh node.
			catalog, collections := a.catalog, a.cols
			a.srv.Close()
			closedA = true
			b.srv.Close()
			closedB = true
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.OpenPath(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			restarted := newFixture(opts(123))
			defer restarted.srv.Close()
			restarted.srv.d.Catalog = catalog
			restarted.srv.d.Collections = collections
			restarted.srv.d.Recommender = rec
			for i, h := range initial {
				totals[i] = append(totals[i], page(restarted, h, h.Size+50, 140-h.Size-50)...)
				expected := []string{}
				for n := int64(0); n < 140; n++ {
					key := core.MovieKey(50000 + n)
					if i == 1 {
						key = core.ShowKey(50000 + n)
					}
					expected = append(expected, key.String())
				}
				if !reflect.DeepEqual(totals[i], expected) {
					t.Fatal("duplicate or gap across nodes/restart")
				}
				if got := get(restarted, h.HubKey); got.Size != h.Size {
					t.Fatal("hubKey recovery changed size")
				}
			}
			var payload string
			if err := db.QueryRow(`SELECT keys_json FROM plex_shelf_generations LIMIT 1`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var stored []int64
			if err := json.Unmarshal([]byte(payload), &stored); err != nil || len(stored) != 140 {
				t.Fatalf("store must hold keys only: %s %v", payload, err)
			}
			// Deleted/TTL-expired generations still answer a deduplicated current page.
			if _, err := db.Exec(`UPDATE plex_shelf_generations SET expires_at=0`); err != nil {
				t.Fatal(err)
			}
			for _, h := range initial {
				u, _ := url.Parse(h.Key)
				q := u.Query()
				q.Set("X-Plex-Container-Start", "0")
				q.Set("X-Plex-Container-Size", "2")
				u.RawQuery = q.Encode()
				mc := get(restarted, u.String())
				seen := map[string]bool{}
				for _, md := range mc.Metadata {
					if seen[md.RatingKey] {
						t.Fatal("fallback repeated key")
					}
					seen[md.RatingKey] = true
				}
			}
		})
	}
}

// The token changes membership selection, not the existing PMS route's wire
// format. Compare the complete response bodies of both continuation families.
func TestShelfGenerationWireParity(t *testing.T) {
	for _, format := range []string{"application/json", "application/xml"} {
		t.Run(format, func(t *testing.T) {
			f := newFixture(func(d *Deps) { d.DisableEnrichment = true })
			defer f.srv.Close()
			key := core.CollectionKey(8091)
			f.cols.items[key] = []core.Title{fightClub, alien}
			c := f.cols.cols[key]
			c.ChildCount = 2
			f.cols.cols[key] = c
			f.srv.d.Recommender = &fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeMovie: {
				{ID: "movie.parity", Type: core.TypeMovie, Items: []core.Title{fightClub, alien}},
				{ID: "collection.parity", Key: key, Type: core.TypeMovie, Items: []core.Title{fightClub, alien}},
			}}}
			screen := f.get(t, "/hubs/sections/1", "Accept", "application/json")
			for _, h := range decodeShelfContainer(t, screen.Body.Bytes()).Hub {
				if h.HubIdentifier != "movie.parity" && h.HubIdentifier != "collection.parity" {
					continue
				}
				uPage, _ := url.Parse(h.Key)
				qPage := uPage.Query()
				qPage.Set("X-Plex-Container-Start", "1")
				qPage.Set("X-Plex-Container-Size", "1")
				uPage.RawQuery = qPage.Encode()
				for _, raw := range []string{h.Key, uPage.String(), h.HubKey} {
					u, _ := url.Parse(raw)
					q := u.Query()
					q.Del(shelfGenerationParam)
					u.RawQuery = q.Encode()
					plain := f.getWarm(t, u.String(), "Accept", format)
					pinned := f.getWarm(t, raw, "Accept", format)
					if plain.Code != 200 || pinned.Code != 200 || !bytes.Equal(plain.Body.Bytes(), pinned.Body.Bytes()) {
						t.Fatalf("wire changed %s\nplain=%s\npinned=%s", raw, plain.Body.String(), pinned.Body.String())
					}
				}
			}
		})
	}
}

func BenchmarkShelfGETWrapper(b *testing.B) {
	for _, query := range []struct{ name, raw string }{{"empty", ""}, {"query", "type=1&sort=titleSort&X-Plex-Container-Start=0&X-Plex-Container-Size=50"}} {
		for _, wrapped := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/wrapped=%t", query.name, wrapped), func(b *testing.B) {
				r := httptest.NewRequest("GET", "/identity?"+query.raw, nil)
				s := &Server{}
				calls := 0
				next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
				handler := next
				if wrapped {
					handler = func(w http.ResponseWriter, r *http.Request) {
						if !s.serveShelfGeneration(w, r) {
							next(w, r)
						}
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					handler(nil, r)
				}
				b.StopTimer()
				if calls != b.N {
					b.Fatal("route was not called")
				}
			})
		}
	}
}
