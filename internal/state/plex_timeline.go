package state

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// TimelineResult describes persistence, rather than echoing the playhead.
type TimelineResult = frontend.TimelinePersistenceResult
type plexStatusKey struct{}

// WithPlexTimelineStored keeps broker liveness reporting from writing a second watch update.
func WithPlexTimelineStored(ctx context.Context) context.Context {
	return frontend.WithTimelineStored(ctx)
}

type plexPlayID struct {
	user            int64
	client, session string
	playlist        bool
}
type plexPlay struct {
	key                          core.RatingKey
	queue                        string
	position                     time.Duration
	progressed, complete, closed bool
	at                           time.Time
}
type plexTimelineStore struct {
	sync.Mutex
	plays map[plexPlayID]plexPlay
}

// TimelineStore applies the session-local crossing and resume rules measured on PMS.
// The media and playlist tracks are independent. The cache has a fixed bound;
// its cost depends on active clients, never the catalogue's size.
func (s *State) StoreTimeline(ctx context.Context, user int64, key core.RatingKey, offset, duration time.Duration, status, queue string, offline *int) (TimelineResult, error) {
	ctx = context.WithValue(ctx, plexStatusKey{}, status)
	s.plexTimeline.Lock()
	defer s.plexTimeline.Unlock()
	cur, err := s.Get(ctx, user, key)
	if err != nil {
		return TimelineResult{}, err
	}
	out := TimelineResult{PlaybackState: "ignore", ViewOffset: cur.ViewOffset, ViewCount: cur.ViewCount}
	if offline != nil {
		if *offline < 0 {
			return out, nil
		}
		mode := "plex-offline-clear"
		out.PlaybackState = "complete"
		if *offline > 0 {
			mode = "plex-complete"
			out.PlaybackState = "complete"
			out.Completed = true
			out.ViewCount++
		} else if cur.ViewCount == 0 {
			return out, nil
		}
		err = s.Progress(ctx, user, key, offset, duration, mode)
		out.Changed = err == nil
		out.ViewOffset = 0
		if *offline == 0 {
			out.ViewCount = 0
		}
		return out, err
	}
	origin := originFrom(ctx)
	id := plexPlayID{user, origin.ClientID, origin.SessionID, key.Kind() == core.KindPlaylist}
	if s.plexTimeline.plays == nil {
		s.plexTimeline.plays = make(map[plexPlayID]plexPlay)
	}
	play := s.plexTimeline.plays[id]
	previous := play.position
	if play.key != key || play.queue != queue {
		play = plexPlay{key: key, queue: queue}
	}
	now := s.now()
	play.at = now
	mode := ""
	if duration > 0 {
		threshold := time.Duration(duration.Milliseconds()*9/10) * time.Millisecond
		nearEnd := offset >= threshold
		if !nearEnd {
			play.complete = false
			play.closed = false
			if offset > time.Minute {
				play.progressed = true
			}
		}
		crossing := nearEnd && previous > 0 && previous < threshold && !play.complete && !play.closed
		if crossing {
			play.complete = true
			play.progressed = true
			mode = "plex-complete"
			out.Completed = true
			out.ViewCount++
			out.ViewOffset = 0
		}
		switch {
		case play.complete && !play.closed:
			out.PlaybackState = "complete"
		case play.closed:
			out.PlaybackState = "ignore"
		case play.progressed:
			out.PlaybackState = "progress"
		}
		if mode == "" && !nearEnd && offset > time.Minute && offset != cur.ViewOffset {
			mode = "plex-resume"
			out.ViewOffset = offset
		} else if mode == "" && !nearEnd && offset <= time.Minute && play.progressed && status != "stopped" && cur.ViewOffset != 0 {
			mode = "plex-clear"
			out.ViewOffset = 0
		}
	}
	play.position = offset
	if status == "stopped" {
		if play.complete {
			play.closed = true
		}
		play.progressed = offset > time.Minute && (play.progressed || play.complete)
	}
	// Refused requests never reach this function. Missing duration is a no-op.
	s.plexTimeline.plays[id] = play
	if len(s.plexTimeline.plays) > 4096 {
		var oldest plexPlayID
		var at time.Time
		for k, p := range s.plexTimeline.plays {
			if at.IsZero() || p.at.Before(at) {
				oldest, at = k, p.at
			}
		}
		delete(s.plexTimeline.plays, oldest)
	}
	if mode != "" {
		err = s.Progress(ctx, user, key, offset, duration, mode)
		out.Changed = err == nil
	}
	return out, err
}

func (s *State) plexProgressAt(ctx context.Context, user int64, key core.RatingKey, w progressWrite) error {
	return s.updateAt(ctx, user, []core.RatingKey{key}, w.at, func(cur *row, now time.Time) (bool, bool, []Event) {
		complete := w.state == "plex-complete"
		before := cur.ws.ViewOffset
		if w.duration > 0 {
			cur.ws.Duration = w.duration
		}
		switch w.state {
		case "plex-resume":
			cur.ws.ViewOffset = w.offset
			cur.ws.LastViewedAt = now
		case "plex-complete":
			cur.ws.ViewOffset = 0
			cur.ws.ViewCount++
			cur.ws.LastViewedAt = now
		case "plex-clear":
			cur.ws.ViewOffset = 0
		case "plex-offline-clear":
			cur.ws.ViewOffset = 0
			cur.ws.ViewCount = 0
		}
		cur.ws.UpdatedAt = now
		cur.watchAt = now
		kind := EventProgress
		if complete {
			kind = EventCompleted
		}
		ev := Event{Kind: kind, UserID: user, Key: key, Offset: w.offset, Duration: cur.ws.Duration, Completed: complete, State: w.timelineState, At: now}
		if complete {
			ev.Fraction = 1
		} else {
			ev.ResumeSet, ev.Resume = cur.ws.ViewOffset != before, cur.ws.ViewOffset
		}
		return complete && key.Kind() != core.KindPlaylist, complete, []Event{ev}
	})
}

// applyProgressChain retains completion events while coalescing held positions.
func (s *State) applyProgressChain(ctx context.Context, k progressKey, w progressWrite) error {
	if w.before != nil {
		if err := s.applyProgressChain(ctx, k, *w.before); err != nil {
			return err
		}
	}
	// Retrying a partially flushed chain must not append the same history again.
	if !w.at.IsZero() && len(w.state) > 5 && w.state[:5] == "plex-" {
		var at int64
		err := s.db.QueryRowContext(ctx, `SELECT watch_at FROM state_watch WHERE user_id=? AND rating_key=?`, k.user, int64(k.key)).Scan(&at)
		if err == nil && !unixTime(at).Before(w.at) {
			return nil
		}
	}
	return s.progressAt(ctx, k.user, k.key, w)
}
