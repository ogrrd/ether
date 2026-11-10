package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/homefilter"
)

// homeSharedEntry carries only candidate hubs, not rendered or authorized wire
// metadata. Every response reapplies visibility and the current Home layout.
// A client repeating a directory begins a new Home load.
type homeSharedEntry struct {
	done            chan struct{}
	seen            map[string]bool
	expires         time.Time
	hubs, discovery []frontend.Hub
	err             error
	filter          *homefilter.Snapshot
}

func (s *Server) sharedHome(ctx context.Context, pr *Request, hc frontend.HubContext, directory, pinned string, count, discoveryCount int) ([]frontend.Hub, []frontend.Hub, *homefilter.Snapshot, error) {
	filter := &homefilter.Snapshot{}
	load := func() ([]frontend.Hub, []frontend.Hub, error) {
		ctx := homefilter.With(ctx, filter)
		var hubs []frontend.Hub
		var err error
		if s.recommender() != nil {
			hubs, err = s.recommendHome(ctx, pr, hc, count)
		}
		var discovery []frontend.Hub
		for _, spec := range s.discoveryHubs(ctx, pr, discoveryCount) {
			discovery = append(discovery, spec.Hub)
		}
		return hubs, discovery, err
	}
	if pr.User == nil || hc.ClientID == "" {
		hubs, discovery, err := load()
		return hubs, discovery, filter, err
	}
	u := pr.User
	key := fmt.Sprintf("%d|%t|%q|%v|%q|%q|%q|%q|%q|%q|%t|%s|%d|%d|%q", u.ID, u.IsOwner, u.MaxContentRating, u.AllowedLibraries, hc.ClientID, hc.SessionID, hc.Product, hc.Platform, hc.Device, hc.Daypart, hc.Weekend, hc.Now.Format("2006-01-02"), count, discoveryCount, pinned)
	now := time.Now()
	s.homeSharedMu.Lock()
	if s.homeShared == nil {
		s.homeShared = make(map[string]*homeSharedEntry)
	}
	for k, e := range s.homeShared {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(s.homeShared, k)
		}
	}
	if e := s.homeShared[key]; e != nil && !e.seen[directory] {
		e.seen[directory] = true
		s.homeSharedMu.Unlock()
		select {
		case <-e.done:
			if e.err != nil && ctx.Err() == nil {
				hubs, discovery, err := load()
				return hubs, discovery, filter, err
			}
			return e.hubs, e.discovery, e.filter, e.err
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		}
	}
	// Bound both the number and size of snapshots, and do not retain oversized
	// candidate sets. Eviction never cancels an in-flight request.
	if len(s.homeShared) >= 8 {
		for k := range s.homeShared {
			delete(s.homeShared, k)
			break
		}
	}
	e := &homeSharedEntry{done: make(chan struct{}), seen: map[string]bool{directory: true}, filter: filter}
	s.homeShared[key] = e
	s.homeSharedMu.Unlock()
	completed := false
	defer func() {
		if completed {
			return
		}
		s.homeSharedMu.Lock()
		e.err = errors.New("home candidate build interrupted")
		if s.homeShared[key] == e {
			delete(s.homeShared, key)
		}
		close(e.done)
		s.homeSharedMu.Unlock()
	}()
	hubs, discovery, err := load()
	size := 0
	for _, h := range hubs {
		size += len(h.Items)
	}
	for _, h := range discovery {
		size += len(h.Items)
	}
	cacheable := err == nil && len(hubs)+len(discovery) <= 64 && size <= 512
	if cacheable {
		encoded, encodeErr := json.Marshal([][]frontend.Hub{hubs, discovery})
		cacheable = encodeErr == nil && len(encoded) <= 1<<20
	}
	s.homeSharedMu.Lock()
	e.hubs, e.discovery, e.err = hubs, discovery, err
	e.expires = time.Now().Add(10 * time.Second)
	if !cacheable {
		if s.homeShared[key] == e {
			delete(s.homeShared, key)
		}
	}
	close(e.done)
	completed = true
	s.homeSharedMu.Unlock()
	return hubs, discovery, filter, err
}
