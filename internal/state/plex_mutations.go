package state

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"math"
	"time"
)

// RateExact preserves the distinction between clearing a rating and writing
// another negative number. Equal values do not refresh the user's timestamps.
func (s *State) RateExact(ctx context.Context, user int64, key core.RatingKey, rating float64) error {
	return s.update(ctx, user, []core.RatingKey{key}, func(cur *row, now time.Time) (bool, bool, []Event) {
		value := math.Max(0, math.Min(10, rating))
		if (rating >= 0 || rating == -1) && cur.ws.UserRating == value {
			return false, false, nil
		}
		cur.ws.UserRating = value
		cur.ws.UpdatedAt = now
		cur.ratingAt = now
		if rating == -1 {
			cur.ratingAt = time.Time{}
		}
		cur.ws.LastRatedAt = cur.ratingAt
		return false, false, []Event{{Kind: EventRated, UserID: user, Key: key, Rating: value, At: now}}
	})
}

// ScrobbleTree keeps parent counters independent of the watched-leaf totals.
// Descendant expansion is supplied by the catalogue, never a state-table scan.
func (s *State) ScrobbleTree(ctx context.Context, user int64, key core.RatingKey, leaves []core.RatingKey, watched bool) error {
	if watched && (key.Kind() == core.KindCollection || key.Kind() == core.KindSmart) {
		return nil
	}
	keys := append([]core.RatingKey{}, leaves...)
	parentOnly := map[core.RatingKey]bool{}
	seen := map[core.RatingKey]bool{}
	for _, k := range keys {
		seen[k] = true
	}
	add := func(k core.RatingKey) {
		if k != 0 && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
			parentOnly[k] = true
		}
	}
	switch key.Kind() {
	case core.KindEpisode:
		if watched {
			add(key.Parent())
			add(key.Parent().Parent())
		}
	case core.KindShow, core.KindSeason:
		add(key)
		if !watched && key.Kind() == core.KindShow {
			for _, leaf := range leaves {
				add(leaf.Parent())
			}
		}
	}
	return s.update(ctx, user, keys, func(cur *row, now time.Time) (bool, bool, []Event) {
		k := cur.ws.Key
		if parentOnly[k] && watched && key.Kind() != core.KindEpisode {
			cur.ws.LastViewedAt = now
			cur.ws.UpdatedAt = now
			cur.watchAt = now
			return false, false, nil
		}
		cur.ws.ViewOffset = 0
		cur.ws.UpdatedAt = now
		cur.watchAt = now
		cur.counted = watched
		if watched {
			cur.ws.ViewCount++
			cur.ws.LastViewedAt = now
		} else {
			cur.ws.ViewCount = 0
			cur.ws.LastViewedAt = time.Time{}
		}
		kind := EventScrobble
		if !watched {
			kind = EventUnscrobble
		}
		// Later PMS measurements distinguish manual TV scrobbles from movie history.
		history := watched && !parentOnly[k] && k.Kind() == core.KindMovie
		return history, watched, []Event{{Kind: kind, UserID: user, Key: k, Completed: watched, At: now}}
	})
}

// PlexTouchParents applies an episode playback's timestamps and independent
// completion counters to its two ancestors without manufacturing history.
func (s *State) PlexTouchParents(ctx context.Context, user int64, key core.RatingKey, at time.Time, completed bool) error {
	if key.Kind() != core.KindEpisode {
		return nil
	}
	return s.updateAt(ctx, user, []core.RatingKey{key.Parent(), key.Parent().Parent()}, at, func(cur *row, now time.Time) (bool, bool, []Event) {
		cur.ws.LastViewedAt = now
		cur.ws.UpdatedAt = now
		cur.watchAt = now
		if completed {
			cur.ws.ViewCount++
		}
		return false, false, nil
	})
}
