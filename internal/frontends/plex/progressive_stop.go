package plex

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type progressiveStopKey struct {
	user     int64
	client   string
	playback string
	key      frontend.RatingKey
}

type progressiveStop struct {
	offset, duration time.Duration
	at               time.Time
}

type progressiveStopGuard struct {
	mu sync.Mutex
	m  map[progressiveStopKey]progressiveStop
}

const progressiveStopTTL = 5 * time.Minute

func (g *progressiveStopGuard) put(k progressiveStopKey, v progressiveStop) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = make(map[progressiveStopKey]progressiveStop)
	}
	for key, old := range g.m {
		if v.at.Sub(old.at) > progressiveStopTTL {
			delete(g.m, key)
		}
	}
	if len(g.m) >= 1024 {
		var oldestKey progressiveStopKey
		var oldest time.Time
		for key, old := range g.m {
			if oldest.IsZero() || old.at.Before(oldest) {
				oldestKey, oldest = key, old.at
			}
		}
		delete(g.m, oldestKey)
	}
	g.m[k] = v
}

func (g *progressiveStopGuard) get(user int64, client, playback string, key frontend.RatingKey, now time.Time) (progressiveStop, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := progressiveStopKey{user, client, playback, key}
	v, ok := g.m[k]
	if ok && now.Sub(v.at) > progressiveStopTTL {
		delete(g.m, k)
		return progressiveStop{}, false
	}
	return v, ok
}

func (g *progressiveStopGuard) clear(user int64, client, playback string, key frontend.RatingKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, progressiveStopKey{user, client, playback, key})
}

func (s *Server) rememberProgressiveStop(ctx context.Context, e *transcodeEntry) {
	saved := progressiveStop{at: s.now()}
	found := false
	id := trackID(e.userID, e.clientID, e.playbackID)
	for _, activity := range s.sessions.list() {
		if activity.id == id && activity.key == e.key {
			saved.offset, saved.duration, found = activity.offset, activity.duration, true
			break
		}
	}
	if !found && s.state() != nil {
		if previous, err := s.state().Get(ctx, e.userID, e.key); err == nil {
			saved.offset, saved.duration = previous.ViewOffset, previous.Duration
		}
	}
	s.progressiveStops.put(progressiveStopKey{e.userID, e.clientID, e.playbackID, e.key}, saved)
}
