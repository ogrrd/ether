package plex

import "github.com/braedonsaunders/ether/internal/frontend"

func (s *Server) plexSources() frontend.PartSources {
	return s.d.SessionStores.NewPartSources(s.d.Playback, s.log)
}
