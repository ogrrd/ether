package plex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// longShowTMDB serves one long-running show the way TMDB does: show
// details list every season, and each season's episodes cost a request.
// Seasons 1..seasons have perSeason episodes aired twice a year; the last
// season is airing (its first three episodes have aired); a Specials season
// has two. Any other TMDB call panics on the nil embedded interface.
type longShowTMDB struct {
	catalog.TMDB
	id        int64
	seasons   int
	perSeason int
	now       func() time.Time
	delay     time.Duration

	mu          sync.Mutex
	seasonCalls map[int]int
	tvCalls     atomic.Int32
}

func (f *longShowTMDB) airDate(season, episode int) string {
	if season == f.seasons {
		// The airing season: three aired episodes, the rest to come.
		return f.now().AddDate(0, 0, 7*(episode-3)-1).Format("2006-01-02")
	}
	if season == 0 {
		return fmt.Sprintf("%d-12-20", 2000+episode)
	}
	// Two seasons a year, from 2000.
	return time.Date(2000, time.Month(2+6*(season-1)), episode, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func (f *longShowTMDB) episodes(season int) int {
	if season == 0 {
		return 2
	}
	return f.perSeason
}

func (f *longShowTMDB) TVDetails(_ context.Context, id int64) (*tmdb.TVDetails, error) {
	f.tvCalls.Add(1)
	d := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Island Vote", FirstAirDate: "2000-02-01", Popularity: 90, VoteCount: 900, VoteAverage: 7.5, OriginalLanguage: "en"},
		Status: "Returning Series", NumberOfSeasons: f.seasons, NumberOfEpisodes: f.seasons * f.perSeason}
	for n := 0; n <= f.seasons; n++ {
		d.Seasons = append(d.Seasons, tmdb.SeasonSummary{ID: int64(9000 + n), Name: fmt.Sprintf("Season %d", n), SeasonNumber: n,
			EpisodeCount: f.episodes(n), AirDate: f.airDate(n, 1)})
	}
	d.Seasons[0].Name = "Specials"
	return d, nil
}

func (f *longShowTMDB) SeasonDetails(_ context.Context, id int64, season int) (*tmdb.SeasonDetails, error) {
	f.mu.Lock()
	f.seasonCalls[season]++
	f.mu.Unlock()
	time.Sleep(f.delay)
	sd := &tmdb.SeasonDetails{ID: int64(9000 + season), Name: fmt.Sprintf("Season %d", season), SeasonNumber: season, AirDate: f.airDate(season, 1)}
	for e := 1; e <= f.episodes(season); e++ {
		sd.Episodes = append(sd.Episodes, tmdb.Episode{ID: int64(season*1000 + e), Name: fmt.Sprintf("Episode %d", e),
			SeasonNumber: season, EpisodeNumber: e, AirDate: f.airDate(season, e), ShowID: id})
	}
	return sd, nil
}

func (f *longShowTMDB) EpisodeExternalIDs(context.Context, int64, int, int) (*tmdb.ExternalIDs, error) {
	return &tmdb.ExternalIDs{}, nil
}

// seasonFetches returns and resets the per-season SeasonDetails counts.
func (f *longShowTMDB) seasonFetches() map[int]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seasonCalls
	f.seasonCalls = map[int]int{}
	return out
}

// countingCatalog counts the catalogue's season-level reads.
type countingCatalog struct {
	core.Catalog
	children, grandchildren atomic.Int32
}

func (c *countingCatalog) Children(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	c.children.Add(1)
	return c.Catalog.Children(ctx, k)
}

func (c *countingCatalog) Grandchildren(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	c.grandchildren.Add(1)
	return c.Catalog.Grandchildren(ctx, k)
}

type longShow struct {
	f     *fixture
	db    *sql.DB
	tmdb  *longShowTMDB
	cat   *countingCatalog
	clock atomic.Int64 // unix nanos
	show  core.RatingKey
}

func newLongShow(t *testing.T, seasons int) *longShow {
	t.Helper()
	ls := &longShow{show: core.ShowKey(14658)}
	ls.clock.Store(fixedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, ls.clock.Load()).UTC() }
	ls.tmdb = &longShowTMDB{id: 14658, seasons: seasons, perSeason: 14, now: func() time.Time { return fixedNow }, seasonCalls: map[int]int{}}
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ls.db = db
	cfg := config.Catalog{Language: "en-US", Region: "US", RefreshInterval: config.Duration(24 * time.Hour)}
	c, err := catalog.New(db, ls.tmdb, cfg, slog.New(slog.DiscardHandler), catalog.WithNow(now))
	if err != nil {
		t.Fatal(err)
	}
	ls.cat = &countingCatalog{Catalog: c}
	ls.f = newFixture(func(d *testServerDeps) { d.Catalog = ls.cat })
	t.Cleanup(func() { ls.f.srv.Close() })
	return ls
}

func (ls *longShow) advance(d time.Duration) { ls.clock.Add(int64(d)) }

// pageItem is the part of a rendered item the long-show tests read.
type pageItem struct {
	RatingKey       string `json:"ratingKey"`
	Index           *int   `json:"index"`
	LeafCount       *int   `json:"leafCount"`
	ViewedLeafCount *int   `json:"viewedLeafCount"`
	ChildCount      *int   `json:"childCount"`
	OnDeck          *struct {
		Metadata pageItem `json:"Metadata"`
	} `json:"OnDeck"`
}

type pageContainer struct {
	Size      int        `json:"size"`
	Metadata  []pageItem `json:"Metadata"`
	Directory []pageItem `json:"Directory"`
}

func (ls *longShow) get(t *testing.T, path string) pageContainer {
	t.Helper()
	rec := ls.f.request(t, http.MethodGet, path, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var body struct{ MediaContainer pageContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.MediaContainer
}

// openShowPage makes the requests the Apple TV app makes to open a show.
func (ls *longShow) openShowPage(t *testing.T) (detail, children pageContainer) {
	t.Helper()
	k := ls.show.String()
	detail = ls.get(t, "/library/metadata/"+k+"?includeExtras=1&includeOnDeck=1&includeRelated=1&includeRelatedCount=1&includeReviews=1&includeExternalMetadata=1")
	children = ls.get(t, "/library/metadata/"+k+"/children")
	return detail, children
}

// useEpisodeOrder serves the show through an episode-order service whose
// only group is twoDiscSource's.
func (ls *longShow) useEpisodeOrder(t *testing.T, p *episodeorder.Preference) {
	t.Helper()
	svc := episodeorder.New(ls.db, twoDiscSource{}, ls.cat)
	if p != nil {
		if err := svc.Save(context.Background(), 0, ls.show, *p); err != nil {
			t.Fatal(err)
		}
	}
	ls.f.srv.setEpisodeOrder(svc)
}

func (ls *longShow) watch(keys ...core.RatingKey) {
	for _, k := range keys {
		ls.f.state.set(ownerUser.ID, core.WatchStatus{Key: k, ViewCount: 1})
	}
}

func itemByKey(t *testing.T, items []pageItem, k core.RatingKey) pageItem {
	t.Helper()
	for _, m := range items {
		if m.RatingKey == k.String() {
			return m
		}
	}
	t.Fatalf("%s not listed", k)
	return pageItem{}
}

func wantLeaves(t *testing.T, what string, m pageItem, leaves, viewed int) {
	t.Helper()
	if m.LeafCount == nil || *m.LeafCount != leaves || m.ViewedLeafCount == nil || *m.ViewedLeafCount != viewed {
		t.Fatalf("%s: leafCount %v viewedLeafCount %v, want %d/%d", what, m.LeafCount, m.ViewedLeafCount, leaves, viewed)
	}
}

// A long-running show's page (Survivor: 51 seasons) loads its episodes once
// per request, never season by season, and once mirrored refetches only the
// season that is still airing when its details go stale.
func TestLongShowPageLoadsSeasonsOnce(t *testing.T) {
	ls := newLongShow(t, 51)
	ls.useEpisodeOrder(t, nil)
	var season1 []core.RatingKey
	for e := 1; e <= 14; e++ {
		season1 = append(season1, core.EpisodeKey(14658, 1, e))
	}
	ls.watch(append(season1, core.EpisodeKey(14658, 51, 1))...)
	for _, step := range []struct {
		name    string
		advance time.Duration
		fetches map[int]int
	}{
		{"cold", 0, nil}, // every listed season once
		{"warm", time.Minute, map[int]int{}},
		{"stale", 25 * time.Hour, map[int]int{51: 1}},
	} {
		ls.advance(step.advance)
		ls.cat.children.Store(0)
		ls.cat.grandchildren.Store(0)
		detail, children := ls.openShowPage(t)
		fetches := ls.tmdb.seasonFetches()
		if step.fetches == nil {
			if len(fetches) != 52 {
				t.Fatalf("cold: fetched %d seasons, want all 52", len(fetches))
			}
			for season, n := range fetches {
				if n != 1 {
					t.Fatalf("cold: season %d fetched %d times", season, n)
				}
			}
		} else if fmt.Sprint(fetches) != fmt.Sprint(step.fetches) {
			t.Fatalf("%s: season fetches %v, want %v", step.name, fetches, step.fetches)
		}
		// One whole-show load per request, and season reads only for the
		// show's own children: none per season.
		if g, c := ls.cat.grandchildren.Load(), ls.cat.children.Load(); g > 2 || c > 2 {
			t.Fatalf("%s: catalogue Grandchildren=%d Children=%d", step.name, g, c)
		}
		if len(detail.Metadata) != 1 {
			t.Fatalf("%s: detail size %d", step.name, len(detail.Metadata))
		}
		show := detail.Metadata[0]
		wantLeaves(t, step.name+" show", show, 50*14+3, 15)
		if show.OnDeck == nil || show.OnDeck.Metadata.RatingKey != core.EpisodeKey(14658, 51, 2).String() {
			t.Fatalf("%s: onDeck %+v, want S51E02", step.name, show.OnDeck)
		}
		if len(children.Metadata) != 52 {
			t.Fatalf("%s: %d seasons listed, want 52", step.name, len(children.Metadata))
		}
		wantLeaves(t, step.name+" season 1", itemByKey(t, children.Metadata, core.SeasonKey(14658, 1)), 14, 14)
		wantLeaves(t, step.name+" season 2", itemByKey(t, children.Metadata, core.SeasonKey(14658, 2)), 14, 0)
		wantLeaves(t, step.name+" airing season", itemByKey(t, children.Metadata, core.SeasonKey(14658, 51)), 3, 1)
		wantLeaves(t, step.name+" specials", itemByKey(t, children.Metadata, core.SeasonKey(14658, 0)), 2, 0)
	}
}

// Season counts on a show's children come from the show's episodes grouped
// by season; each must still equal what that season's own page lists, in
// canonical order, an alternate episode group, and with specials hidden.
func TestLongShowSeasonCountsMatchSeasonPages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pref    *episodeorder.Preference
		seasons int
	}{
		{"canonical", nil, 5},
		{"episode group", &episodeorder.Preference{GroupID: "dvd", Specials: "first"}, 4},
		{"specials hidden", &episodeorder.Preference{Specials: "hidden"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := newLongShow(t, 4)
			ls.useEpisodeOrder(t, tc.pref)
			ls.watch(core.EpisodeKey(14658, 1, 1), core.EpisodeKey(14658, 0, 2), core.EpisodeKey(14658, 3, 5))
			_, children := ls.openShowPage(t)
			if len(children.Metadata) != tc.seasons {
				t.Fatalf("%d seasons listed, want %d", len(children.Metadata), tc.seasons)
			}
			for _, season := range children.Metadata {
				eps := ls.get(t, "/library/metadata/"+season.RatingKey+"/children").Metadata
				viewed := 0
				for _, e := range eps {
					for _, k := range []core.RatingKey{core.EpisodeKey(14658, 1, 1), core.EpisodeKey(14658, 0, 2), core.EpisodeKey(14658, 3, 5)} {
						if e.RatingKey == k.String() {
							viewed++
						}
					}
				}
				wantLeaves(t, "season "+season.RatingKey, season, len(eps), viewed)
			}
		})
	}
}
