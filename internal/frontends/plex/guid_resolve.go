package plex

import (
	"context"
	"time"
)

// maxGUIDResolves bounds the plex.tv identities queued for background
// resolution at once.
const maxGUIDResolves = 64

// resolveGUIDsLater resolves plex.tv movie and show identities that have no
// local mapping yet, off the request path, so a client that names them again
// (Infuse reloads its Watchlist row with every Home) finds their titles. The
// lookup stores each verified mapping; identities it cannot verify stay
// unresolved.
func (s *Server) resolveGUIDsLater(guids []string) {
	if s.d.LibraryGUIDs == nil || len(guids) == 0 {
		return
	}
	s.guidResolveMu.Lock()
	if s.guidResolving == nil {
		s.guidResolving = map[string]bool{}
	}
	var queued []string
	for _, g := range guids {
		if len(s.guidResolving) >= maxGUIDResolves {
			break
		}
		if !s.guidResolving[g] {
			s.guidResolving[g] = true
			queued = append(queued, g)
		}
	}
	s.guidResolveMu.Unlock()
	if len(queued) == 0 {
		return
	}
	go func() {
		for _, g := range queued {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if _, err := s.d.LibraryGUIDs.Lookup(ctx, g); err != nil {
				s.log.Debug("plex identity unresolved", "guid", g, "err", err)
			}
			cancel()
			s.guidResolveMu.Lock()
			delete(s.guidResolving, g)
			s.guidResolveMu.Unlock()
		}
	}()
}
