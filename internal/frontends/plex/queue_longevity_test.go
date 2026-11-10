package plex

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Advancing the server clock exercises the real queue TTL without claiming
// that a few seconds of execution is a year of real device playback.
func TestQueueLongevityActiveTimelineSurvivesExpiry(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "standalone"
		if shared {
			name = "shared-cold-cache"
		}
		t.Run(name, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(fixedNow.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			f := newFixture(func(d *Deps) {
				setDepsNow(d, now)
				d.DisableEnrichment, d.Hooks = true, Hooks{}
				if shared {
					d.DB = store.OpenTest(t)
					d.Cluster = clusterbus.NewHub().Node("longevity")
				}
			})
			defer f.srv.Close()
			qs := f.srv.queues
			active, err := qs.CreateWith(ownerUser.ID, "active", []core.RatingKey{bbE1.Key, bbE2.Key}, now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			idle, err := qs.CreateWith(ownerUser.ID, "abandoned", []core.RatingKey{alien.Key}, now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			// Include 30-day, year-end and leap-year crossings. The selected
			// item need not change: long pauses and repeated timelines count.
			for hour := 1; hour <= 24*370; hour++ {
				clock.Add(int64(time.Hour))
				if shared {
					qs.Forget(active.ID)
				} // a different/cold node
				qs.SelectItem(ownerUser.ID, active.Items[0].ID)
				qs.SelectItem(friendUser.ID, idle.Items[0].ID) // cannot keep another user's queue alive
				qs.Expire(now(), 6*time.Hour)
				if _, ok := qs.Get(active.ID, ownerUser.ID); !ok {
					t.Fatalf("active queue expired after %d hours of valid timelines", hour)
				}
				if hour > 6 {
					if _, ok := qs.Get(idle.ID, ownerUser.ID); ok {
						t.Fatal("abandoned queue retained")
					}
				}
			}
			clock.Add(int64(7 * time.Hour))
			qs.Expire(now(), 6*time.Hour)
			if _, ok := qs.Get(active.ID, ownerUser.ID); ok {
				t.Fatal("queue retained after timelines stop")
			}
		})
	}
}

// Wall clocks can repeat a value (coarse resolution or a clock adjustment).
// Queue capacity must not turn equal timestamps into random eviction of the
// queue that a client has just created.
func TestQueueCapacityKeepsNewestWhenTimestampsTie(t *testing.T) {
	qs := testStores().NewPlayQueues()
	qs.Configure(func() time.Time { return fixedNow }, nil)
	for i := 0; i < maxQueues+100; i++ {
		q, err := qs.CreateWith(ownerUser.ID, "burst-"+strconv.Itoa(i), []core.RatingKey{alien.Key}, fixedNow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := qs.Get(q.ID, ownerUser.ID); !ok {
			t.Fatal("new queue immediately lost")
		}
	}
	for id := int64(1); id <= 100; id++ {
		if _, ok := qs.Get(id, ownerUser.ID); ok {
			t.Fatalf("old queue %d kept ahead of newer equal-time queues", id)
		}
	}
	for id := int64(101); id <= int64(maxQueues+100); id++ {
		if _, ok := qs.Get(id, ownerUser.ID); !ok {
			t.Fatalf("newer queue %d evicted at equal timestamp", id)
		}
	}
}
