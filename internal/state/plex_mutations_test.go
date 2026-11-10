package state

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestPlexRatingTimestampMatrix(t *testing.T) {
	s, now := newTestState(t)
	k := core.MovieKey(42)
	rate := func(v float64) core.WatchStatus {
		t.Helper()
		if err := s.RateExact(ctx, 1, k, v); err != nil {
			t.Fatal(err)
		}
		st, err := s.Get(ctx, 1, k)
		if err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
		return st
	}
	initial := rate(7)
	same := rate(7)
	if !same.LastRatedAt.Equal(initial.LastRatedAt) || !same.UpdatedAt.Equal(initial.UpdatedAt) {
		t.Fatal("equal rating moved timestamps")
	}
	negative := rate(-5)
	if negative.UserRating != 0 || !negative.LastRatedAt.After(same.LastRatedAt) {
		t.Fatalf("negative: %+v", negative)
	}
	clearEmpty := rate(-1)
	if !clearEmpty.LastRatedAt.Equal(negative.LastRatedAt) || !clearEmpty.UpdatedAt.Equal(negative.UpdatedAt) {
		t.Fatal("clearing empty rating moved timestamps")
	}
	secondNegative := rate(-5)
	if !secondNegative.LastRatedAt.After(negative.LastRatedAt) {
		t.Fatal("negative no-op did not stamp")
	}
	rate(8)
	cleared := rate(-1)
	if !cleared.LastRatedAt.IsZero() {
		t.Fatal("explicit clear retained rating timestamp")
	}
}

func TestPlexParentCountersAndManualHistory(t *testing.T) {
	s, _ := newTestState(t)
	ep := core.EpisodeKey(42, 1, 1)
	season := ep.Parent()
	show := season.Parent()
	apply := func(k core.RatingKey, leaves []core.RatingKey, watched bool) {
		t.Helper()
		if err := s.ScrobbleTree(ctx, 1, k, leaves, watched); err != nil {
			t.Fatal(err)
		}
	}
	apply(ep, []core.RatingKey{ep}, true)
	apply(ep, []core.RatingKey{ep}, true)
	apply(season, []core.RatingKey{ep}, true)
	st, _ := s.GetMany(ctx, 1, []core.RatingKey{ep, season, show})
	if st[ep].ViewCount != 3 || st[season].ViewCount != 2 || st[show].ViewCount != 2 {
		t.Fatalf("independent counters %+v", st)
	}
	apply(ep, []core.RatingKey{ep}, false)
	st, _ = s.GetMany(ctx, 1, []core.RatingKey{season, show})
	if st[season].ViewCount != 2 || st[show].ViewCount != 2 {
		t.Fatal("leaf unscrobble changed parents")
	}
	apply(show, []core.RatingKey{ep}, false)
	st, _ = s.GetMany(ctx, 1, []core.RatingKey{season, show})
	if st[season].ViewCount != 0 || st[show].ViewCount != 0 {
		t.Fatal("show unscrobble retained parent counters")
	}
	history, err := s.History(ctx, 1, time.Time{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("manual TV scrobble history %+v", history)
	}
	col := core.CollectionKey(9)
	if err := s.LegacyProgress(ctx, 1, col, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	apply(col, []core.RatingKey{col}, true)
	status, _ := s.Get(ctx, 1, col)
	if status.ViewCount != 0 || status.ViewOffset != 2*time.Minute {
		t.Fatal("collection scrobble mutated state")
	}
}
