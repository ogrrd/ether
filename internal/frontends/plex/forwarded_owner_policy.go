package plex

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// forwardedOwnerPolicy only ADDS restrictions after original authentication and
// exact local owner/grant checks. The marker is not an auth/provenance assertion:
// a public spoof can force stricter checks, never grant access. The private
// receiver independently authenticates peer/owner/fence/hop and restores the
// original socket/proxy provenance before entering the original public handler.
func (s *Server) forwardedOwnerPolicy(r *http.Request, pr *Request) bool {
	if s.currentPlexEncoder() == nil || r.Header.Get("X-Ether-Delivery-Hop") == "" {
		return true
	}
	id, media := strictPlexMediaID(r)
	if !media {
		return true
	} // Non-media behavior remains exactly the local path.
	if len(r.Header.Values("X-Ether-Delivery-Hop")) != 1 || r.Header.Get("X-Ether-Delivery-Hop") != "1" || pr.User == nil {
		return false
	}
	entry, ok := s.transcodes.get(id)
	if !ok || !entry.started || entry.userID != pr.User.ID || entry.playID == "" || s.d.Catalog == nil || s.d.Playback == nil {
		return false
	}
	viewer := s.activeViewer(r.Context(), pr.User.ID)
	if viewer == nil || viewer.ID != entry.userID {
		return false
	}
	if policy, ok := s.policy().(frontend.UserPolicySource); ok {
		viewer = policy.UserPolicy(viewer)
	}
	if viewer == nil || viewer.ID != entry.userID {
		return false
	}
	// pr.Remote comes from restored original provenance, never a delivery peer's
	// LAN address or an unauthenticated caller-provided origin/user header.
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(viewer, pr.Remote, s.now()) {
		return false
	}
	title, err := s.d.Catalog.Get(r.Context(), entry.key)
	if err != nil || title == nil || title.Key != entry.key || !s.playableTitle(r.Context(), viewer, title) {
		return false
	}
	source, ok := s.d.Playback.Session(entry.playID)
	if !ok || source == nil || source.ID != entry.playID || source.UserID != entry.userID || source.Key != entry.key {
		return false
	}
	if !s.acceptanceCurrent(r) {
		return false
	}
	pr.User = viewer
	return true
}
