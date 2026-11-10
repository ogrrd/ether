package plex

// NoteOwnerStop tells the Plex client why a stream is ending. The shared
// broker stops the source.
func (s *Server) NoteOwnerStop(id, reason string) {
	if s.d.LiveTV != nil {
		for _, v := range s.d.LiveTV.ViewingSessions() {
			if v.ID == id {
				return
			}
		}
	}
	if pb := s.playback(); pb != nil {
		if ps, ok := pb.Session(id); ok && ps != nil {
			unlock, err := s.lockPlaybackOwner(s.ctx, ps.UserID, ps.ClientID)
			if err != nil {
				return
			}
			defer unlock()
			for _, e := range s.sessions.list() {
				if found, ok := pb.SessionFor(e.user.ID, e.client.ClientID, e.key); ok && found.ID == id {
					s.setOwnerTermination(e.id, reason)
				}
			}
			return
		}
	}
}

func (s *Server) liveTVAPIVersion() int {
	if s.liveTVEnabled() {
		return 4
	}
	return 0
}
