package plex

import (
	"context"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// SegmentedEncoder is an opt-in trusted host seam implemented by the shared broker.
// Starts occur only after existing Plex authentication, owner gates, preparation
// and admission. The broker must revalidate the exact source revision and retain
// uncertain execution responsibility. It may choose local execution only before
// remote allocation; a returned error is never permission for Plex to retry local.
// Session/Stop true means the broker owns responsibility, even when Session is
// nil because start is pending. Transport errors must never turn true into false.
type SegmentedEncoder = frontend.SegmentedEncoder

// UsePlexEncoder installs an optional broker independent of playbackBroker.
// Keep it installed while releasing a mount: clearing playbackBroker does not
// relinquish pending remote encoder responsibility. Nil preserves local behavior.
func (s *Server) UsePlexEncoder(encoder SegmentedEncoder) {
	s.plexEncoderMu.Lock()
	s.plexEncoder = encoder
	s.plexEncoderMu.Unlock()
}
func (s *Server) currentPlexEncoder() SegmentedEncoder {
	s.plexEncoderMu.RLock()
	defer s.plexEncoderMu.RUnlock()
	return s.plexEncoder
}
func (s *Server) startPlexHLS(ctx context.Context, viewer *frontend.User, playID, id string, req frontend.HLSRequest) (frontend.HLSSession, error) {
	if encoder := s.currentPlexEncoder(); encoder != nil {
		return encoder.StartSegmentedHLS(ctx, viewer, playID, id, req)
	}
	if s.d.Transcoder == nil {
		return nil, frontend.ErrNotAvailable
	}
	return s.d.Transcoder.StartHLS(ctx, req)
}
func (s *Server) plexHLSSession(id string) (frontend.HLSSession, bool) {
	if encoder := s.currentPlexEncoder(); encoder != nil {
		if session, owned := encoder.SegmentedHLSSession(id); owned {
			return session, true
		}
	}
	if s.d.Transcoder == nil {
		return nil, false
	}
	return s.d.Transcoder.Session(id)
}
func (s *Server) stopPlexHLS(id string) {
	if encoder := s.currentPlexEncoder(); encoder != nil && encoder.StopSegmentedHLS(id) {
		return
	}
	if s.d.Transcoder != nil {
		s.d.Transcoder.Stop(id)
	}
}

// forwardPlexMedia runs only inside an already-authorized public media handler.
// The typed private receiver must replay the ORIGINAL public handler (including
// token/grant/policy middleware); mTLS peer trust is never viewer authorization.
func (s *Server) forwardPlexMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	if forward, ok := s.currentPlexEncoder().(interface {
		ForwardPlexMedia(http.ResponseWriter, *http.Request, string) bool
	}); ok {
		return forward.ForwardPlexMedia(w, r, id)
	}
	return false
}

// missingPlexMediaID is deliberately narrower than general Plex routing. It is
// eligible only for terminal owner transfer, never local authentication success.
func missingPlexMediaID(r *http.Request) (string, bool) {
	if len(r.Header.Values("X-Ether-Delivery-Hop")) != 0 {
		return "", false
	}
	return strictPlexMediaID(r)
}
func strictPlexMediaID(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	if r.URL == nil || !capabilityPathOK(r) || len(r.URL.RequestURI()) > 16384 {
		return "", false
	}
	const prefix = "/video/:/transcode/universal/session/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 128 || sanitizeSessionID(part) != part {
			return "", false
		}
	}
	file := parts[2]
	if file != "index.m3u8" && file != "init.mp4" && file != "header" && !strings.HasSuffix(file, ".ts") && !strings.HasSuffix(file, ".m4s") && !strings.HasSuffix(file, ".mkv") {
		return "", false
	}
	return parts[0], true
}
