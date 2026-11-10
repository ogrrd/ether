package plex

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type plexSourceAccess struct{ s *Server }

func (s *Server) plexSources() plexSourceAccess { return plexSourceAccess{s: s} }

func (a plexSourceAccess) Prepare(ctx context.Context, viewer *frontend.User, key frontend.RatingKey, caps frontend.ClientCaps, release string, wait time.Duration) (*frontend.PlaySession, error) {
	return a.s.prepareSource(ctx, viewer, key, caps, release, wait)
}

func (a plexSourceAccess) Part(viewer int64, client string, key frontend.RatingKey, release string) frontend.PlexPartSource {
	return a.s.partSource(viewer, client, key, release)
}

func (a plexSourceAccess) ServePart(w http.ResponseWriter, r *http.Request, sourceID string) {
	a.s.servePartSource(w, r, sourceID)
}

func (s *Server) prepareSource(ctx context.Context, viewer *frontend.User, key frontend.RatingKey, caps frontend.ClientCaps, release string, wait time.Duration) (*frontend.PlaySession, error) {
	return s.runtime.PrepareSource(ctx, viewer, key, caps, release, wait)
}

func (s *Server) partSource(viewer int64, client string, key frontend.RatingKey, release string) frontend.PlexPartSource {
	return s.runtime.PartSource(viewer, client, key, release)
}

func (s *Server) servePartSource(w http.ResponseWriter, r *http.Request, sourceID string) {
	s.runtime.ServePartSource(w, r, sourceID)
}
