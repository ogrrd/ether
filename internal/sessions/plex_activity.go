package sessions

import (
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (b *Broker) HasActivity(id string) bool {
	if b == nil || id == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.plexActivities[id]
	return ok
}

func (b *Broker) EnsureActivity(id string, key int) {
	if b == nil || id == "" || key <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.plexActivities[id]; ok {
		return
	}
	b.plexActivities[id] = &PlaybackActivity{ID: id, SessionKey: key}
}

func (b *Broker) UpsertActivity(id string, fn func(*PlaybackActivity)) PlaybackActivity {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.plexActivities[id]
	if !ok {
		b.plexActivityNext++
		e = &PlaybackActivity{ID: id, SessionKey: b.plexActivityNext}
		b.plexActivities[id] = e
	}
	if fn != nil {
		fn(e)
	}
	out := *e
	out.User = e.User
	return out
}

func (b *Broker) RemoveActivity(id string) (PlaybackActivity, bool) {
	if b == nil {
		return PlaybackActivity{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.plexActivities[id]
	if !ok {
		return PlaybackActivity{}, false
	}
	delete(b.plexActivities, id)
	return *e, true
}

func (b *Broker) ListActivities() []PlaybackActivity {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PlaybackActivity, 0, len(b.plexActivities))
	for _, e := range b.plexActivities {
		out = append(out, *e)
	}
	return out
}

func (b *Broker) ExpireActivities(now time.Time, ttl time.Duration) []PlaybackActivity {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []PlaybackActivity
	for id, e := range b.plexActivities {
		if now.Sub(e.UpdatedAt) > ttl {
			out = append(out, *e)
			delete(b.plexActivities, id)
		}
	}
	return out
}

type PlaybackActivity = frontend.PlaybackActivity
