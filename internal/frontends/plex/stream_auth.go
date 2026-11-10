package plex

import (
	"bytes"
	"context"
	"html"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const streamGrantParam = frontend.StreamGrantParam

func transcodeSessionID(path string) string {
	rest, ok := strings.CutPrefix(path, "/video/:/transcode/universal/session/")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return id
}

// routeSessionID is the {session} route parameter, only when it is exactly
// the session the authentication middleware authorized. chi routes on the
// escaped path, so an escaped segment ("a%2Fb") would otherwise authorize
// one binding ("a") and, once sanitized, serve another ("a2Fb").
func routeSessionID(r *http.Request) (string, bool) {
	id := chi.URLParam(r, "session")
	if id == "" || id != transcodeSessionID(r.URL.Path) || sanitizeSessionID(id) != id {
		return "", false
	}
	return id, true
}

// capabilityPathOK reports whether a capability credential (stream grant
// or a live session id) may be considered for this request. Encoded
// paths are refused: chi routes on RawPath while transcodeSessionID
// reads the decoded Path, so an encoded slash can authorize one
// binding and then reach decision/start as that viewer.
func capabilityPathOK(r *http.Request) bool {
	if r.URL.RawPath != "" {
		return false
	}
	if strings.Contains(r.URL.EscapedPath(), "%") {
		return false
	}
	return true
}

func (s *Server) refuseCapability(w http.ResponseWriter, r *http.Request) bool {
	if !FromContext(r.Context()).Capability {
		return false
	}
	writeHTTPError(w, http.StatusUnauthorized)
	return true
}

func (s *Server) activeViewer(ctx context.Context, id int64) *frontend.User {
	if s.host.Identity == nil {
		return nil
	}
	u, err := s.host.Identity.ActiveUser(ctx, id)
	if err == nil {
		return u
	}
	return nil
}

// Native media loaders often discard Plex headers. An independent, random
// capability authorizes only this viewer's running stream, never control APIs.
// Revocation is checked on every request, including cached media segments.
func (s *Server) authenticateStreamGrant(r *http.Request, pr *Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !capabilityPathOK(r) {
		return false
	}
	id := transcodeSessionID(r.URL.Path)
	if id == "" {
		return false
	}
	e, ok := s.transcodes.get(id)
	if !ok {
		return false
	}
	u, ok := frontend.AuthenticateStreamGrant(r.Context(), r.Method, r.URL.Query().Get(streamGrantParam), frontend.StreamGrantBinding{Token: e.streamToken, ViewerID: e.userID, Started: e.started}, s.activeViewer)
	if !ok {
		return false
	}
	if policy, ok := s.policy().(frontend.UserPolicySource); ok {
		u = policy.UserPolicy(u)
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
		return false
	}

	pr.User = u
	pr.Capability = true
	return true
}

// authenticateSessionRead authorizes playlist and segment GET/HEAD by a
// server-minted read capability bound to the starting device. The
// client-chosen session id in the path is not a credential. Native
// loaders that drop headers keep using stream grants.
func (s *Server) authenticateSessionRead(r *http.Request, pr *Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !capabilityPathOK(r) {
		return false
	}
	if id := transcodeSessionID(r.URL.Path); id != "" {
		_, _ = s.transcodes.get(id)
	}
	cap, ok := s.readCaps.lookup(readCapFrom(r))
	if !ok {
		return false
	}
	id := transcodeSessionID(r.URL.Path)
	if id == "" || id != cap.sessionID {
		return false
	}
	if pr.ClientID == "" || cap.clientID == "" || pr.ClientID != cap.clientID {
		return false
	}
	e, ok := s.transcodes.get(cap.sessionID)
	if !ok || !e.started || e.userID != cap.userID || e.clientID != cap.clientID {
		return false
	}
	u := s.activeViewer(r.Context(), cap.userID)
	if u == nil {
		return false
	}
	if policy, ok := s.policy().(frontend.UserPolicySource); ok {
		u = policy.UserPolicy(u)
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
		return false
	}
	pr.User = u
	pr.Capability = true
	return true
}

func streamGrantURL(raw, token string) string {
	return frontend.StreamGrantURL(raw, token)
}

// Only manifest responses are buffered. Segments remain streamed directly.
// URL templates retain their $Number$ / $RepresentationID$ placeholders.
type manifestResponse struct {
	http.ResponseWriter
	body   bytes.Buffer
	status int
}

func (w *manifestResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *manifestResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}

var manifestURI = regexp.MustCompile(`(URI|initialization|media)="([^"]+)"`)

func (s *Server) serveStreamManifest(w http.ResponseWriter, r *http.Request, id string, serve http.HandlerFunc) {
	if !s.acceptanceCurrent(r) {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	e, ok := s.transcodes.get(id)
	if !ok {
		writeHTTPError(w, 404)
		return
	}
	rec := &manifestResponse{ResponseWriter: w}
	serve(rec, r)
	body := rec.body.String()
	if rec.status == 0 {
		rec.status = 200
	}
	if rec.status == 200 {
		xml := strings.Contains(w.Header().Get("Content-Type"), "dash+xml")
		body = manifestURI.ReplaceAllStringFunc(body, func(attr string) string {
			m := manifestURI.FindStringSubmatch(attr)
			raw := m[2]
			if xml {
				raw = html.UnescapeString(raw)
			}
			raw = s.mediaURL(raw, e.id, e.streamToken)
			if xml {
				raw = html.EscapeString(raw)
			}
			return m[1] + `="` + raw + `"`
		})
		if !xml {
			lines := strings.Split(body, "\n")
			for i, line := range lines {
				if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
					lines[i] = s.mediaURL(line, e.id, e.streamToken)
				}
			}
			body = strings.Join(lines, "\n")
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
