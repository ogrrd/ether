package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type noPlaybackDelivery struct{}

func suppressTimelineWatch(ctx context.Context) bool {
	suppressed, _ := ctx.Value(noPlaybackDelivery{}).(bool)
	return suppressed
}

// playbackNeverDelivered uses output evidence for conversions: ffmpeg can read
// megabytes of source headers without delivering a single playable segment.
// No active source is deliberately inconclusive: Plex can sync offline views.
func (s *Server) playbackNeverDelivered(ps *frontend.PlaySession, tids []string) bool {
	if ps != nil && ps.Experience != nil && ps.Experience.StartFailure != "" {
		return true
	}
	// Pending, failed, or unobservable conversions have no delivery proof.
	converted := false
	for _, id := range tids {
		// A decision reserves a binding even for direct play. Until an encoder
		// starts, that binding uses the source's delivered-byte evidence.
		if entry, ok := s.transcodes.get(id); ok && !entry.started && entry.failErr == nil && entry.decision.Kind == frontend.DecisionDirectPlay {
			continue
		}
		converted = true
		if out, ok := s.plexHLSSession(id); ok {
			if delivered, ok := out.(frontend.PlaybackDeliveredSession); ok {
				if delivered.PlaybackDelivered() {
					return false
				}
			}
		}
	}
	if converted {
		return true
	}
	return ps != nil && ps.BytesServed == 0
}
