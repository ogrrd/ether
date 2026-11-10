package plex

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Server) plexStreamAccess(u *frontend.Viewer) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: u, Protocol: "plex"}
}

func (s *Server) assignStream(ctx context.Context, u *frontend.Viewer, key frontend.RatingKey, index int, info frontend.StreamInfo, sessionID string) int64 {
	if s == nil || s.streamRuntime == nil {
		return 0
	}
	id, err := s.streamRuntime.BindStream(ctx, s.plexStreamAccess(u), frontend.PlexStreamBinding{
		Key: key, Index: index, Info: info, SessionID: sessionID,
	})
	if err != nil {
		return 0
	}
	return id
}

func (s *Server) resolveStream(ctx context.Context, u *frontend.Viewer, id int64) (frontend.PlexStreamBinding, error) {
	if s == nil || s.streamRuntime == nil {
		return frontend.PlexStreamBinding{}, frontend.ErrNotFound
	}
	return s.streamRuntime.ResolveStream(ctx, s.plexStreamAccess(u), id)
}

// resolveIssuedStream resolves a stream id a client sent back. The broker
// authorizes it and names the title; when a probe or failover moved that
// id onto another track for this client, the track it names now is
// returned instead of the title-wide binding.
func (s *Server) resolveIssuedStream(ctx context.Context, u *frontend.Viewer, clientID string, id int64) (frontend.PlexStreamBinding, error) {
	binding, err := s.resolveStream(ctx, u, id)
	if err != nil || u == nil || s.playIDs == nil {
		return binding, err
	}
	if t, ok := s.playIDs.resolve(playIdentityKey{userID: u.ID, clientID: clientID, key: binding.Key}, id); ok {
		binding.Index, binding.Info = t.index, t.info
	}
	return binding, nil
}

func streamMiss(err error) bool {
	return err == nil || errors.Is(err, frontend.ErrNotFound)
}
