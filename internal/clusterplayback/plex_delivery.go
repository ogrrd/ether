package clusterplayback

import (
	"github.com/braedonsaunders/ether/internal/nodewire/outbound"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const PlexDeliveryPath = "/cluster/playback/plex-media"

// BindPlexDelivery must receive the original authenticated public Plex handler,
// including existing token/grant/policy middleware, before listeners start.
func (r *Runtime) BindPlexDelivery(handler http.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if handler == nil || r.plexDelivery != nil {
		return ErrDenied
	}
	r.plexDelivery = handler
	return nil
}
func plexMediaSession(u *url.URL) (string, bool) {
	const prefix = "/video/:/transcode/universal/session/"
	if !strings.HasPrefix(u.Path, prefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	for _, part := range parts {
		if part == "." || part == ".." || len(part) > 128 {
			return "", false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return "", false
			}
		}
	}
	file := parts[2]
	if file != "index.m3u8" && file != "init.mp4" && file != "header" && !strings.HasSuffix(file, ".ts") && !strings.HasSuffix(file, ".m4s") && !strings.HasSuffix(file, ".mkv") {
		return "", false
	}
	return parts[0], true
}

var plexForwardHeaders = []string{"Authorization", "Accept", "User-Agent", "X-Plex-Token", "X-Plex-Client-Identifier", "X-Plex-Client-Id", "X-Plex-Product", "X-Plex-Version", "X-Plex-Platform", "X-Plex-Platform-Version", "X-Plex-Device", "X-Plex-Device-Name", "X-Plex-Model", "X-Plex-Device-Vendor", "X-Plex-Language", "X-Plex-Features", "X-Plex-Client-Profile-Extra", "X-Plex-Client-Capabilities", "X-Plex-Session-Identifier", "X-Plex-Session-Id", "X-Plex-Container-Start", "X-Plex-Container-Size", "Range", "If-Range", "If-None-Match", "If-Modified-Since"}

func copyPlexHeaders(dst, src http.Header) error {
	total := 0
	for _, h := range plexForwardHeaders {
		values := src.Values(h)
		if len(values) > 16 {
			return ErrDenied
		}
		for _, v := range values {
			total += len(h) + len(v)
			if len(v) > 4096 || total > 16384 {
				return ErrDenied
			}
		}
		dst[h] = append([]string(nil), values...)
	}
	return nil
}
func (r *Runtime) plexDeliveryHTTP(w http.ResponseWriter, q *http.Request) {
	peer, e := r.o.Guard.Request(q)
	if e != nil || !r.mediaPeer(q.Context(), peer) {
		http.Error(w, "denied", 401)
		return
	}
	if q.Method != "GET" && q.Method != "HEAD" {
		http.Error(w, "method", 405)
		return
	}
	if len(q.Header.Values(deliveryHop)) != 1 || q.Header.Get(deliveryHop) != "1" || len(q.Header.Values(deliveryURI)) != 1 {
		http.Error(w, "scope", 400)
		return
	}
	raw := q.Header.Get(deliveryURI)
	if len(raw) > 16384 {
		http.Error(w, "scope", 400)
		return
	}
	u, e := url.ParseRequestURI(raw)
	if e != nil || u.IsAbs() || u.Host != "" || u.RawPath != "" || u.Fragment != "" {
		http.Error(w, "scope", 400)
		return
	}
	id, ok := plexMediaSession(u)
	if !ok {
		http.NotFound(w, q)
		return
	}
	route, e := r.route(q.Context(), id)
	if e != nil {
		http.NotFound(w, q)
		return
	}
	r.mu.Lock()
	handler := r.plexDelivery
	r.mu.Unlock()
	if handler == nil {
		http.Error(w, "unavailable", 503)
		return
	}
	clone, e := restoreOrigin(q, u, raw)
	if e != nil {
		http.Error(w, "scope", 400)
		return
	}
	guarded := r.fencedPublicDelivery(w, q, route)
	defer guarded.Close()
	handler.ServeHTTP(guarded, clone)
}

// ForwardMedia forwards only validated representation paths. The receiver
// runs the original Plex token/grant checks. No control/start endpoint is eligible.
func (r *Runtime) ForwardEncodedMedia(w http.ResponseWriter, q *http.Request, publicID string) bool {
	if q.Header.Get(deliveryHop) != "" {
		return false
	}
	id, ok := plexMediaSession(q.URL)
	if !ok || id != publicID {
		return false
	}
	if q.Method != "GET" && q.Method != "HEAD" {
		return false
	}
	owner, e := r.mediaServer(q.Context(), id)
	if e != nil || owner == r.o.Self {
		return false
	}
	route, e := r.route(q.Context(), id)
	if e != nil {
		http.NotFound(w, q)
		return true
	}
	ep, tc, check, e := r.peer(q.Context(), owner, PlexDeliveryPath)
	if e != nil {
		http.Error(w, "delivery unavailable", 502)
		return true
	}
	transport, e := outbound.New(ep, tc, 500*time.Millisecond, check)
	if e != nil {
		http.Error(w, "delivery unavailable", 502)
		return true
	}
	transport.ResponseHeaderTimeout(r.o.EncoderSegmentWait)
	defer transport.CloseIdleConnections()
	request, e := http.NewRequestWithContext(q.Context(), q.Method, ep, nil)
	if e != nil {
		http.Error(w, "delivery unavailable", 502)
		return true
	}
	if copyPlexHeaders(request.Header, q.Header) != nil {
		http.Error(w, "request headers too large", 431)
		return true
	}
	origin, e := captureOrigin(q)
	if e != nil {
		http.Error(w, "invalid request origin", 400)
		return true
	}
	request.Header.Set(deliveryOrigin, origin)
	request.Header.Set(deliveryHop, "1")
	request.Header.Set(deliveryURI, q.URL.RequestURI())
	client := &http.Client{Transport: &mediaFailoverTransport{runtime: r, first: transport, owner: owner, path: PlexDeliveryPath}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		http.Error(w, "delivery unavailable", 502)
		return true
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != 304 {
		http.Error(w, "delivery unavailable", 502)
		return true
	}
	for _, h := range deliveryHeaders {
		if values := response.Header.Values(h); len(values) > 0 {
			w.Header()[h] = values
		}
	}
	guarded := r.fencedPublicDelivery(w, q, route)
	defer guarded.Close()
	w = guarded
	w.WriteHeader(response.StatusCode)
	if q.Method != "HEAD" {
		_, _ = io.Copy(w, response.Body)
	}
	return true
}
