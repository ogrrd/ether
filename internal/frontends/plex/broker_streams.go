package plex

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// UsePlexStreams binds visible stream ids and sidecar reads to the broker.
// Pass sessions.BindStreams(broker).
func (s *Server) UsePlexStreams(api frontend.StreamBindings) {
	if s != nil {
		s.boundStreams = api
	}
}

func (s *Server) plexStreamAccess(u *frontend.Viewer) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: u, Protocol: "plex"}
}

func (s *Server) assignStream(ctx context.Context, u *frontend.Viewer, key frontend.RatingKey, index int, info frontend.StreamInfo, sessionID string) int64 {
	if s == nil || s.boundStreams == nil {
		return 0
	}
	id, err := s.boundStreams.BindStream(ctx, s.plexStreamAccess(u), frontend.StreamBinding{
		Key: key, Index: index, Info: info, SessionID: sessionID,
	})
	if err != nil {
		return 0
	}
	return id
}

func (s *Server) resolveStream(ctx context.Context, u *frontend.Viewer, id int64) (frontend.StreamBinding, error) {
	if s == nil || s.boundStreams == nil {
		return frontend.StreamBinding{}, frontend.ErrNotFound
	}
	return s.boundStreams.ResolveStream(ctx, s.plexStreamAccess(u), id)
}

// resolveIssuedStream resolves a stream id a client sent back. The broker
// authorizes it and names the title; when a probe or failover moved that
// id onto another track for this client, the track it names now is
// returned instead of the title-wide binding.
func (s *Server) resolveIssuedStream(ctx context.Context, u *frontend.Viewer, clientID string, id int64) (frontend.StreamBinding, error) {
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
