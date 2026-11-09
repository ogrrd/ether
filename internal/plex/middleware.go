package plex

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontend/kit/faillog"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type acceptanceContextKey struct{}

func withAcceptance(ctx context.Context, a frontend.CredentialAcceptance) context.Context {
	return context.WithValue(ctx, acceptanceContextKey{}, a)
}

func acceptanceFrom(ctx context.Context) (frontend.CredentialAcceptance, bool) {
	a, ok := ctx.Value(acceptanceContextKey{}).(frontend.CredentialAcceptance)
	return a, ok && a.Valid()
}

const corsAllowHeaders = "Accept, Accept-Language, Content-Type, Range, X-Plex-Token, X-Plex-Client-Identifier, X-Plex-Product, " +
	"X-Plex-Version, X-Plex-Platform, X-Plex-Platform-Version, X-Plex-Device, X-Plex-Device-Name, X-Plex-Device-Vendor, " +
	"X-Plex-Device-Screen-Resolution, X-Plex-Model, X-Plex-Language, X-Plex-Features, X-Plex-Provides, X-Plex-Drm, " +
	"X-Plex-Container-Start, X-Plex-Container-Size, X-Plex-Client-Profile-Extra, X-Plex-Client-Profile-Name, " +
	"X-Plex-Client-Capabilities, X-Plex-Session-Identifier, X-Plex-Pms-Api-Version, X-Plex-Text-Format, X-Plex-Restriction-Profile, " +
	"X-Plex-Marketplace, X-Plex-Sync-Version, X-Plex-Http-Pipeline"

const corsExposeHeaders = "Content-Length, Content-Range, Content-Type, Date, Location, X-Plex-Protocol, " +
	"X-Plex-Container-Start, X-Plex-Container-Total-Size, X-Plex-Content-Original-Length, X-Plex-Content-Compressed-Length, X-Plex-Activity"

// commonHeaders sets X-Plex-Protocol and CORS on every response and answers
// preflights.
func (s *Server) commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Plex-Protocol", "1.0")
		// Plex routes serve data and upstream media, never the owner SPA
		// (which is mounted separately at /ether/). A provider can return
		// HTML/SVG even from a media URL; direct navigation must not execute
		// it on the origin that holds the owner's localStorage credential.
		// Leave media/image sources unrestricted for ordinary playback.
		// Artwork handlers replace this with their stricter image policy.
		h.Set("Content-Security-Policy", "script-src 'none'; object-src 'none'; base-uri 'none'; sandbox")
		h.Set("X-Content-Type-Options", "nosniff")
		// Like PMS, CORS headers are only sent to cross-origin (browser)
		// requests.
		if origin := r.Header.Get("Origin"); origin != "" {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		}
		if r.Method == http.MethodOptions {
			// PMS leaves subtitle-search/upload OPTIONS unimplemented, even
			// though the rest of the API accepts generic CORS preflights.
			if strings.HasPrefix(r.URL.Path, "/library/metadata/") && (strings.HasSuffix(r.URL.Path, "/subtitles") || strings.HasSuffix(r.URL.Path, "/tree")) {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				h.Set("Access-Control-Allow-Headers", req)
			} else {
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			}
			h.Set("Access-Control-Max-Age", "1209600")
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns handler panics into 500s.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("handler panic", "path", r.URL.Path, "panic", v)
				writeHTTPError(w, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status    int
	written   int64
	sample    []byte
	sampling  bool
	truncated bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.written += int64(n)
	if sr.sampling {
		const maxSample = 1 << 20
		remaining := maxSample - len(sr.sample)
		if remaining < n {
			sr.truncated = true
		}
		if remaining > 0 {
			if n < remaining {
				remaining = n
			}
			sr.sample = append(sr.sample, b[:remaining]...)
		}
	}
	return n, err
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// requestLog logs requests at debug level and summarizes noisy 401 failures.
func (s *Server) requestLog(next http.Handler) http.Handler {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	failures := faillog.New(log.With("frontend", "plex"), "authentication")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		samplePath := supportDocumentPath(r.URL.Path)
		sample := s.d.Support != nil && samplePath && s.d.Support.BeginSample(FromContext(r.Context()).ClientID)
		if sample {
			defer s.d.Support.EndSample()
		}
		sr := &statusRecorder{ResponseWriter: w, sampling: sample}
		next.ServeHTTP(sr, r)
		s.observeRequest(r, sr, time.Since(start))
		s.notifyRequestLog(r, sr.status)
		if sr.status == http.StatusUnauthorized {
			failures.Fail(errors.New("unauthorized"))
			return
		}
		pr := FromContext(r.Context())
		if s.d.TracePlayback && traceTranscodePath(r.URL.Path) {
			status := sr.status
			if status == 0 {
				status = http.StatusOK
			}
			s.log.Info("hls request", "method", r.Method, "path", r.URL.Path, "query", redactTraceQuery(r.URL.Query()),
				"range", r.Header.Get("Range"), "status", status, "bytes", sr.written, "duration", time.Since(start))
		}
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sr.status,
			"dur", time.Since(start).Round(time.Millisecond), "product", pr.Product, "client", pr.ClientID,
			"range", r.Header.Get("Range"), "contentRange", sr.Header().Get("Content-Range"),
			"bytes", sr.written, "contentLength", sr.Header().Get("Content-Length"),
			"pageStart", pr.Start, "pageSize", pr.Size, "paged", pr.HasPage, "format", pr.Format, "includeChildren", r.URL.Query().Get("includeChildren"),
			"model", pr.Model, "device", pr.Device, "version", pr.Version, "platformVersion", pr.PlatformVersion, "peer", pr.RemoteIP.String())
	})
}

// traceTranscodePath reports Plex HLS and session playlist/segment routes.
func traceTranscodePath(path string) bool {
	return strings.HasPrefix(path, "/video/:/transcode/") || strings.Contains(path, "/:/transcode/universal/session/")
}

// redactTraceQuery keeps the request query except credential parameters.
func redactTraceQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	cloned := url.Values{}
	for k, vs := range q {
		if traceSecretQuery(k) {
			cloned[k] = []string{"redacted"}
			continue
		}
		cloned[k] = vs
	}
	return cloned.Encode()
}

func traceSecretQuery(k string) bool {
	switch strings.ToLower(k) {
	case "x-plex-token", "token", "accesstoken", "access_token",
		strings.ToLower(streamGrantParam), strings.ToLower(readCapParam):
		return true
	default:
		return false
	}
}

// parse attaches the parsed request context.
func (s *Server) parse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := ParseRequest(r, clientnet.ProxyPrefixes(s.cfg().Server.TrustedProxyCIDRs)...)
		if pr.pagingInvalid {
			writeHTTPError(w, http.StatusInternalServerError)
			return
		}
		ctx := withRequest(r.Context(), pr)
		ctx = core.WithBrowseScope(ctx, browseScope(r, pr))
		if s.plexMetaOn() {
			ctx = core.WithPlexSaferRating(ctx)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// publicPaths need no token.
var publicPaths = map[string]bool{
	"/identity": true,
}

// authenticate maps the token to a user. Requests without a valid token get
// PMS's 401 body.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The bare read has no representation, even before authentication.
		if r.URL.Path == "/downloadQueue" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if publicPaths[trimSlash(r.URL.Path)] || strings.HasPrefix(r.URL.Path, signedLogoPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		pr := FromContext(r.Context())
		if pr.Token != "" && s.d.Auth != nil {
			u, receipt, err := s.acceptToken(r.Context(), pr.Token)
			switch {
			case err == nil && u != nil:
				if policy, ok := s.d.AccessPolicy.(core.UserPolicySource); ok {
					u = policy.UserPolicy(u)
				}
				if id := transcodeSessionID(r.URL.Path); id != "" {
					e, ok := s.transcodes.get(id)
					if !ok {
						// A valid local token is not permission for this stream. Only a
						// bounded terminal transfer to its exact owner may decide access;
						// the original public owner handler reauthenticates all credentials.
						if remoteID, eligible := missingPlexMediaID(r); eligible && remoteID == id && s.currentPlexEncoder() != nil {
							checked := r
							if receipt.Valid() {
								checked = r.WithContext(withAcceptance(r.Context(), receipt))
							}
							if !s.acceptanceCurrent(checked) {
								writeHTTPError(w, http.StatusUnauthorized)
								return
							}
							if s.forwardPlexMedia(w, checked, id) {
								return
							}
						}
						writeHTTPError(w, http.StatusNotFound)
						return
					}
					if e.userID != u.ID {
						writeHTTPError(w, http.StatusForbidden)
						return
					}
				}
				pr.User = u
				if receipt.Valid() {
					r = r.WithContext(withAcceptance(r.Context(), receipt))
				}
				if !s.acceptanceCurrent(r) {
					writeHTTPError(w, http.StatusUnauthorized)
					return
				}
				if !s.forwardedOwnerPolicy(r, pr) {
					writeHTTPError(w, http.StatusForbidden)
					return
				}
				if s.d.Hooks.OnClientSeen != nil {
					s.d.Hooks.OnClientSeen(pr.ClientID)
				}
				if s.d.LiveTV != nil {
					s.d.LiveTV.NoteClientActivity()
				}
				if pr.Remote && s.d.Hooks.OnRemoteClientSeen != nil {
					s.d.Hooks.OnRemoteClientSeen(pr.RemoteIP.String())
				}
				if s.d.Dashboard != nil {
					s.d.Dashboard.SeeDevice(pr.ClientID, pr.DeviceName, pr.Platform)
				}
				s.observePlaybackRequestStart(r, pr)
				next.ServeHTTP(w, r)
				return
			case err == nil, errors.Is(err, core.ErrUnauthorized), errors.Is(err, core.ErrNotFound):
				// Stale or unknown token: playlist/segment reads may still
				// pass with a live stream grant or minted read capability.
			case errors.Is(err, r.Context().Err()):
				return
			default:
				s.log.Warn("authentication failed", "err", err)
				writeHTTPError(w, http.StatusServiceUnavailable)
				return
			}
		}
		if s.authenticateStreamGrant(r, pr) || s.authenticateSessionRead(r, pr) || s.authenticateLiveGrant(r, pr) {
			if !s.acceptanceCurrent(r) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			if !s.forwardedOwnerPolicy(r, pr) {
				writeHTTPError(w, http.StatusForbidden)
				return
			}
			if pr.Remote && s.d.Hooks.OnRemoteClientSeen != nil {
				s.d.Hooks.OnRemoteClientSeen(pr.RemoteIP.String())
			}
			s.observePlaybackRequestStart(r, pr)
			next.ServeHTTP(w, r)
			return
		}
		if s.forwardMissingPlexGrant(w, r, pr) {
			return
		}
		writeHTTPError(w, http.StatusUnauthorized)
	})
}

type tokenAcceptor = frontend.CredentialAcceptor

func (s *Server) acceptToken(ctx context.Context, token string) (*core.User, frontend.CredentialAcceptance, error) {
	if acc, ok := s.d.Auth.(tokenAcceptor); ok {
		return acc.Accept(ctx, token)
	}
	u, err := s.d.Auth.Authenticate(ctx, token)
	return u, frontend.CredentialAcceptance{}, err
}

// acceptanceCurrent rechecks a receipt minted for this request. Requests that
// did not mint one, including stream grants, stay on their own check.
func (s *Server) acceptanceCurrent(r *http.Request) bool {
	receipt, ok := acceptanceFrom(r.Context())
	if !ok {
		return true
	}
	checker, ok := s.d.Auth.(tokenAcceptor)
	if !ok {
		return false
	}
	return checker.ReceiptCurrent(r.Context(), receipt) == nil
}

func (s *Server) authenticateLiveGrant(r *http.Request, pr *Request) bool {
	if s.d.LiveTV == nil {
		return false
	}
	if !capabilityPathOK(r) {
		return false
	}
	u, ok := s.d.LiveTV.AuthenticateMediaGrant(r, s.activeViewer)
	if !ok {
		return false
	}
	if policy, ok := s.d.AccessPolicy.(core.UserPolicySource); ok {
		u = policy.UserPolicy(u)
	}
	if fresh, ok := s.d.AccessPolicy.(interface {
		ConfirmPlayback(context.Context, *core.User, bool, time.Time) (*core.User, error)
	}); ok {
		confirmed, err := fresh.ConfirmPlayback(r.Context(), u, pr.ClientCaps(0).Remote, s.now())
		if err != nil && !errors.Is(err, core.ErrPolicyUnconfirmed) {
			return false
		}
		if err == nil {
			if confirmed == nil || confirmed.ID != u.ID {
				return false
			}
			u = confirmed
		} else if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
			return false
		}
	} else if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
		return false
	}
	pr.User = u
	pr.Capability = true
	return true
}

// ownerOnly restricts a handler to the server owner.
func (s *Server) ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := FromContext(r.Context()).User; u == nil || !u.IsOwner {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// writeHTTPError writes PMS's minimal HTML error body.
func writeHTTPError(w http.ResponseWriter, code int) {
	text := http.StatusText(code)
	body := "<html><head><title>" + text + "</title></head><body><h1>" + strconv.Itoa(code) + " " + text + "</h1></body></html>"
	h := w.Header()
	h.Set("Content-Type", "text/html")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Del("Content-Encoding")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// write encodes a response model in the client's format, gzip-compressed
// when the client accepts it and the body is worth compressing.
func (s *Server) write(w http.ResponseWriter, r *http.Request, v any) {
	s.writeStatus(w, r, http.StatusOK, v)
}

func (s *Server) writeStatus(w http.ResponseWriter, r *http.Request, status int, v any) {
	if err := FromContext(r.Context()).versionErr; err != nil {
		s.log.Warn("version flow unavailable", "err", err)
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	body, err := s.encodeResponse(r, v)
	if err != nil {
		s.log.Error("encode response", "path", r.URL.Path, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	var offset, total *int
	if mc, ok := v.(*model.MediaContainer); ok {
		offset, total = mc.Offset, mc.TotalSize
	}
	s.writeBody(w, r, status, body, nil, offset, total)
}

// encodeResponse renders a response model in the client's format.
func (s *Server) encodeResponse(r *http.Request, v any) ([]byte, error) {
	pr := FromContext(r.Context())
	// allowSync on library containers follows the viewer's download
	// permission; containers that don't carry the attribute keep not
	// carrying it.
	if mc, ok := v.(*model.MediaContainer); ok {
		if !strings.HasSuffix(r.URL.Path, "/decision") && !metadataOptionOn(r.URL.Query().Get("includeChapters")) {
			for i := range mc.Metadata {
				omitChapterRows(&mc.Metadata[i])
			}
		}
		if r.URL.Path == "/hubs" || strings.HasPrefix(r.URL.Path, "/hubs/") {
			switch {
			case strings.HasSuffix(r.URL.Path, "/postplay"):
				mc.SetSyncProjection(false)
			case strings.HasPrefix(r.URL.Path, "/hubs/search"), strings.HasPrefix(r.URL.Path, "/hubs/items"):
				mc.AllowSync = nil
			default:
				mc.SetSyncProjection(true)
			}
		}
		if mc.AllowSync != nil {
			if mc.HasSyncProjection() {
				if policy, ok := s.d.AccessPolicy.(core.DownloadAccessPolicy); ok && !policy.AllowDownloads(pr.User) {
					*mc.AllowSync = false
				}
				// A context capability is distinct from permission to fetch a file.
				// Restricted viewers must still retain their own download grant.
				if pr.User == nil || !pr.User.IsOwner || !s.cfg().Plex.Claimed {
					*mc.AllowSync = *mc.AllowSync && s.downloadsAllowed(pr.User)
				}
			} else {
				*mc.AllowSync = s.downloadsAllowed(pr.User)
			}
		}

		// PMS exposes playlist syncing to clients that declare the sync
		// protocol. Keep Ether's download permission as the authority.
		if r.Header.Get("X-Plex-Sync-Version") != "" && strings.HasPrefix(r.URL.Path, "/playlists") {
			allowed := s.downloadsAllowed(pr.User)
			for i := range mc.Metadata {
				if mc.Metadata[i].Type == "playlist" {
					mc.Metadata[i].AllowSync = model.Ptr(allowed)
				}
			}
			if strings.HasSuffix(r.URL.Path, "/items") {
				mc.AllowSync = model.Ptr(allowed)
			}
		}
	}
	if mc, ok := v.(*model.MediaContainer); ok {
		if r.URL.Path == "/hubs" {
			s.applyHubDetailOptions(r.Context(), mc, r.URL.Query())
		}
		applyInlineOnDeckExclusions(mc, excludedElements(r.URL.Query()))
	}
	v = localizeDecisionResponse(r, v)

	if strings.HasPrefix(r.URL.Path, "/playlists") || strings.HasPrefix(r.URL.Path, "/playQueues") {
		if mc, ok := v.(*model.MediaContainer); ok && strings.HasPrefix(r.URL.Path, "/playlists") && r.Method != http.MethodGet && r.Method != http.MethodHead && pr.HasPage {
			total := len(mc.Metadata)
			lo, hi := pr.window(total)
			mc.Metadata = mc.Metadata[lo:hi]
			mc.Size = len(mc.Metadata)
			mc.SetPage(lo, total)
		}
		names := func(key string) map[string]bool {
			out := map[string]bool{}
			for _, raw := range r.URL.Query()[key] {
				for _, name := range strings.Split(raw, ",") {
					out[name] = true
				}
			}
			return out
		}
		elements := names("excludeElements")
		if mc, ok := v.(*model.MediaContainer); ok && elements["Part"] && !elements["Media"] {
			// PMS replaces excluded Parts with Media@partCount; Plex's TV
			// apps exclude Part from their play queues. Containers here are
			// built per request, so setting the count does not leak.
			for i := range mc.Metadata {
				for j := range mc.Metadata[i].Media {
					m := &mc.Metadata[i].Media[j]
					m.PartCount = model.Ptr(len(m.Part))
				}
			}
		}
		return model.MarshalWithExclusions(v, pr.Format, names("excludeFields"), elements)
	}
	if mc, ok := v.(*model.MediaContainer); ok {
		if fields, elements := responseExclusions(r); len(fields) > 0 || len(elements) > 0 {
			substituteExcludedCounts(mc, elements)
			return model.MarshalWithExclusions(v, pr.Format, fields, elements)
		}
	}
	return model.Marshal(v, pr.Format)

}

// writeBody writes an encoded body, gzip-compressed when the client accepts
// it and the body is worth compressing. gz, when set, is body already
// compressed (a cached Home screen).
func (s *Server) writeBody(w http.ResponseWriter, r *http.Request, status int, body, gz []byte, offset, total *int) {
	pr := FromContext(r.Context())
	h := w.Header()
	h.Set("Content-Type", pr.Format.ContentType())
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
	if offset != nil {
		h.Set("X-Plex-Container-Start", strconv.Itoa(*offset))
		if total != nil {
			h.Set("X-Plex-Container-Total-Size", strconv.Itoa(*total))
		}
	}
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		return
	}
	if len(body) > 1024 && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		h.Set("X-Plex-Content-Original-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if gz != nil {
			_, _ = w.Write(gz)
			return
		}
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(body)
		_ = zw.Close()
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, p := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(p), ";")
			if strings.EqualFold(strings.TrimSpace(name), "gzip") && !zeroQuality(params) {
				return true
			}
		}
	}
	return false
}

// zeroQuality reports whether Accept-Encoding parameters carry q=0 (the
// coding is refused); "q=0.5" still accepts it.
func zeroQuality(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q == 0
	}
	return false
}

// container returns a MediaContainer with the attributes PMS puts on
// library responses.
func libraryContainer() *model.MediaContainer {
	return &model.MediaContainer{
		AllowSync:       model.Ptr(false),
		Identifier:      model.LibraryIdentifier,
		MediaTagPrefix:  model.MediaTagPrefix,
		MediaTagVersion: model.MediaTagVersion,
	}
}

// hubContainer is the container PMS uses for hub lists.
func hubContainer() *model.MediaContainer {
	mc := &model.MediaContainer{Identifier: model.LibraryIdentifier}
	mc.SetSyncProjection(true)
	return mc
}

// writeEmpty writes an empty MediaContainer.
func (s *Server) writeEmpty(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{})
}

// notAvailableRetryAfter is the Retry-After (seconds) sent with a 503 for a
// title that cannot play right now.
const notAvailableRetryAfter = "30"

// writeErr maps a component error to an HTTP status.
func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeHTTPError(w, http.StatusNotFound)
	case errors.Is(err, core.ErrUnauthorized):
		writeHTTPError(w, http.StatusUnauthorized)
	case errors.Is(err, core.ErrStreamLimit), errors.Is(err, transcode.ErrTooManyTranscodes):
		w.Header().Set("Retry-After", "5")
		writeHTTPError(w, http.StatusServiceUnavailable)
	case errors.Is(err, core.ErrNotAvailable), errors.Is(err, core.ErrUnreleased), errors.Is(err, core.ErrDebridAuth), errors.Is(err, core.ErrSourceUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, core.ErrPrepareSlow):
		// The title exists; nothing can play it right now (no release yet,
		// nothing cached, no sources, a rejected debrid key). A 404 here
		// reads as "this item is gone" and Plex apps show a bare "404
		// error"; 503 says "temporarily", and the reason travels in the
		// decision response the client fetched first.
		w.Header().Set("Retry-After", notAvailableRetryAfter)
		writeHTTPError(w, http.StatusServiceUnavailable)
	case errors.Is(err, r.Context().Err()):
		// client gone; nothing useful to write
	default:
		s.log.Warn("request failed", "path", r.URL.Path, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
	}
}
