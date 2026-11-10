package plex

import (
	"context"
	"fmt"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Server) lockPlaybackOwner(ctx context.Context, user int64, client string) (func(), error) {
	return s.playbackLifecycle.Acquire(ctx, fmt.Sprintf("owner/%d/%s", user, client))
}
func (s *Server) lockPlaybackRequest(w http.ResponseWriter, r *http.Request) (func(), bool) {
	pr := FromContext(r.Context())
	if pr.User == nil {
		writeHTTPError(w, http.StatusUnauthorized)
		return nil, false
	}
	unlock, err := s.lockPlaybackOwner(r.Context(), pr.User.ID, pr.ClientID)
	if err != nil {
		return nil, false
	}
	return unlock, true
}
func (s *Server) lockEncoder(ctx context.Context, id string) (func(), error) {
	return s.playbackLifecycle.Acquire(ctx, "encoder/"+id)
}

// The caller holds the owner gate. Never release the shared playback source.
func (s *Server) cleanSupersededEncoders(ctx context.Context, user int64, client string, key frontend.RatingKey, playbackID string) {
	for _, id := range s.transcodes.forClient(user, client, key) {
		unlock, err := s.lockEncoder(ctx, id)
		if err != nil {
			return
		}
		e, ok := s.transcodes.get(id)
		if ok && e.userID == user && e.clientID == client && e.key == key &&
			e.playbackID == playbackID && s.playbackOwners.Stale(user, client, key, playbackID) {
			if s.transcoder() != nil {
				s.stopPlexHLS(id)
			}
			s.dropTranscode(id)
		}
		unlock()
	}
}
