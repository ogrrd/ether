package plex

import (
	"bytes"

	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// handleIndexBIF serves the sparse BIF snapshot for a currently available
// part. Like direct part requests, it is scoped to the authenticated
// viewer's active playback session.
func (s *Server) handleIndexBIF(w http.ResponseWriter, r *http.Request) {
	key, releaseID, ok := s.indexPart(r)
	if !ok || chi.URLParam(r, "index") != "sd" || s.d.Trickplay == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	data, ok := s.d.Trickplay.TrickplayBIF(releaseID)
	if !ok || len(data) == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if raw := r.URL.Query().Get("interval"); raw != "" {
		interval, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || interval < 10_000 {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if interval > 10_000 {
			data = resampleBIF(data, time.Duration(interval)*time.Millisecond)
			if len(data) == 0 {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, fmt.Sprintf("%s.bif", key), s.now(), bytes.NewReader(data))
}

func resampleBIF(data []byte, interval time.Duration) []byte {
	b, err := frontend.ParseBIF(data)
	if err != nil || b.Len() == 0 {
		return nil
	}
	frames := make([]frontend.BIFFrame, 0, b.Len())
	var last time.Duration
	for i := 0; i < b.Len(); i++ {
		at := b.Time(i)
		if i > 0 && at-last < interval {
			continue
		}
		frames = append(frames, frontend.BIFFrame{Time: at, JPEG: b.Frame(i)})
		last = at
	}
	out, err := frontend.MarshalBIF(frames)
	if err != nil {
		return nil
	}
	return out
}

func (s *Server) cancelTrickplayIfIdle(ps *frontend.PlaySession) {
	if ps == nil || ps.Resolution == nil || s.d.Playback == nil {
		return
	}
	releaseID := frontend.TrickplayCacheKey(ps)
	if releaseID == "" {
		return
	}
	idle := true
	for _, active := range s.d.Playback.Sessions() {
		if active != nil && active.Resolution != nil && frontend.TrickplayCacheKey(active) == releaseID {
			idle = false
			break
		}
	}
	if idle {
		if canceler, ok := s.d.Trickplay.(frontend.TrickplayCanceler); ok {
			canceler.CancelTrickplay(releaseID)
		}
	}
}

// handleIndexFrame serves a single generated JPEG frame. Offset is
// expressed in milliseconds, matching Plex Web's indexes/{index}/{offset}
// request form.
func (s *Server) handleIndexFrame(w http.ResponseWriter, r *http.Request) {
	_, releaseID, ok := s.indexPart(r)
	if !ok || chi.URLParam(r, "index") != "sd" || s.d.Trickplay == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	ms, err := strconv.ParseInt(chi.URLParam(r, "offset"), 10, 64)
	if err != nil || ms < 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	frame, ok := s.d.Trickplay.TrickplayFrame(releaseID, time.Duration(ms)*time.Millisecond)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	setArtworkHeaders(w.Header(), "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
}

// indexPart resolves a virtual part to the active session for this viewer,
// applying the same title visibility and playback policy as handlePart.
func (s *Server) indexPart(r *http.Request) (frontend.RatingKey, string, bool) {
	pr := FromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || pr.User == nil ||
		s.d.Playback == nil || s.d.Catalog == nil {
		return 0, "", false
	}
	key, wantedReleaseID, found := s.partTitle(id)
	if !found {
		return 0, "", false
	}
	title, err := s.d.Catalog.Get(r.Context(), key)
	if err != nil || !s.visibleTitle(r.Context(), pr.User, title) {
		return 0, "", false
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.ClientCaps(0).Remote, s.now()) {
		return 0, "", false
	}
	ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID == "" {
		return 0, "", false
	}
	if wantedReleaseID != "" && ps.Resolution.Release.ID != wantedReleaseID {
		return 0, "", false
	}
	cacheKey := frontend.TrickplayCacheKey(ps)
	return key, cacheKey, cacheKey != ""
}

func (s *Server) previewAvailable(pr *Request, key frontend.RatingKey, release string) bool {
	if s.d.Trickplay == nil || s.d.Playback == nil || pr.User == nil {
		return false
	}
	ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID != release {
		return false
	}
	id := frontend.TrickplayCacheKey(ps)
	return id != "" && s.d.Trickplay.TrickplayAvailable(id)
}
