package plex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

// A cold mirror must not load and hydrate each history candidate's show
// individually just to check the selected episode's inherited policy.
func TestContinueColdSeriesQueries(t *testing.T) {
	for _, count := range []int{1, 13} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			db := store.OpenTest(t)
			c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
			if err != nil {
				t.Fatal(err)
			}
			f := newFixture(func(d *Deps) { d.Catalog = c })
			defer f.srv.Close()
			history := &completedEpisodeState{fakeState: f.state}
			f.srv.d.State = history
			for id := int64(1); id <= int64(count); id++ {
				titles := []core.Title{
					{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Series", Released: fixedNow.Add(-48 * time.Hour), ChildCount: 1},
					{Key: core.SeasonKey(id, 1), Type: core.TypeSeason, ShowKey: core.ShowKey(id), Season: 1, Title: "Season", Released: fixedNow.Add(-48 * time.Hour), ChildCount: 2},
				}
				for ep := 1; ep <= 2; ep++ {
					titles = append(titles, core.Title{Key: core.EpisodeKey(id, 1, ep), Type: core.TypeEpisode, ShowKey: core.ShowKey(id), Season: 1, Episode: ep, Title: "Episode", Released: fixedNow.Add(-24 * time.Hour)})
				}
				for _, title := range titles {
					if title.Type == core.TypeShow && id == 1 {
						title.Keywords = []string{"blocked"}
					}
					if err := c.UpsertTitle(context.Background(), title, true); err != nil {
						t.Fatal(err)
					}
				}
				f.state.set(ownerUser.ID, ws(core.EpisodeKey(id, 1, 1), 0, 1))
				history.entries = append(history.entries, core.HistoryEntry{Key: core.EpisodeKey(id, 1, 1), ViewedAt: fixedNow, Completed: true})
			}
			// Mark the mirrored seasons detailed, as in the g0 fixture. No
			// provider refresh or missing-season fallback is involved.
			if _, err := db.ExecContext(context.Background(), `UPDATE catalog_titles SET detail=1,detail_at=?,fetched_at=?`, fixedNow.Unix(), fixedNow.Unix()); err != nil {
				t.Fatal(err)
			}
			// Each phase starts with a cold minute cache. A different viewer
			// and a completed next episode must still receive current state.
			for phase := 0; phase < 4; phase++ {
				viewer := ownerUser
				if phase == 1 {
					viewer.ID = ownerUser.ID + 100
					for id := int64(1); id <= int64(count); id++ {
						f.state.set(viewer.ID, ws(core.EpisodeKey(id, 1, 1), 0, 1))
					}
				}
				want := count
				if phase == 2 {
					f.state.set(viewer.ID, ws(core.EpisodeKey(1, 1, 2), 0, 1))
					want--
				}
				if phase == 3 {
					viewer.ID = ownerUser.ID + 100
					viewer.IsOwner = false
					f.srv.setTestPolicy(keywordPolicy{blocked: "blocked"})
					want--
				}
				at := fixedNow.Add(time.Duration(phase) * time.Minute)
				c.SetNow(func() time.Time { return at })
				ctx, stats := store.WithQueryStats(reqmemo.With(context.Background()))
				selector := f.srv.continueExperience(&Request{User: &viewer})
				items, _, _ := selector.Shelf(ctx, &viewer, "", 20)
				if len(items) != want {
					t.Fatalf("phase %d: got %d items, want %d", phase, len(items), want)
				}
				for _, item := range items {
					if phase == 3 && item.ShowKey == core.ShowKey(1) {
						t.Fatal("prefetched series bypassed inherited policy")
					}
					if item.Episode != 2 {
						t.Fatalf("wrong next episode: %+v", item)
					}
				}
				t.Logf("cold shows=%d phase=%d queries=%d rows=%d", count, phase, stats.Queries(), stats.Rows())
				if stats.Queries() > 24 {
					t.Fatalf("per-series query work returned: %d statements", stats.Queries())
				}
			}
		})
	}
}
