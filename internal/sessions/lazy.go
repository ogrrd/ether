package sessions

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
)

// LazyGrants authenticates an issuing surface credential and rechecks the
// non-secret grant reference without extending its lifetime.
type LazyGrants interface {
	GrantReference(context.Context, string, string) (string, *core.Viewer, error)
	ValidateGrantReference(context.Context, string, string, int64) error
}

type lazySession struct {
	id      string
	expires int64
	viewer  int64
}

type lazyStreams struct {
	db           *sql.DB
	broker       *Broker
	protocol     frontend.Protocol
	grants       LazyGrants
	ttl          time.Duration
	remote       func(*http.Request) bool
	newHeartbeat func(time.Duration) (<-chan time.Time, func())
}

// NewLazyStreams creates a surface-bound durable, reusable capability issuer.
// Capabilities expire after at most one hour of inactivity and are
// revalidated on every request.
func NewLazyStreams(db *sql.DB, b *Broker, protocol frontend.Protocol, grants LazyGrants, ttl time.Duration, remote func(*http.Request) bool) (frontend.LazyStreams, error) {
	if db == nil || b == nil || protocol == "" || grants == nil || remote == nil {
		return nil, errors.New("sessions: missing lazy stream dependency")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > time.Hour {
		return nil, errors.New("sessions: lazy stream lifetime exceeds one hour")
	}
	if ttl < 3*time.Second {
		return nil, errors.New("sessions: lazy stream lifetime below three seconds")
	}
	return &lazyStreams{db: db, broker: b, protocol: protocol, grants: grants, ttl: ttl, remote: remote}, nil
}

func lazyHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *lazyStreams) Issue(ctx context.Context, credential string, req frontend.PlanRequest) (string, error) {
	return s.issue(ctx, credential, req, false)
}

// Warm implements frontend.LazyStreamWarmer with playback's detail warm-up:
// the same viewer, device caps and title a GET of the issued URL plays, so
// that Prepare joins or takes the warm resolution instead of resolving after
// the pick. It rechecks the grant, policy and title like Issue.
func (s *lazyStreams) Warm(ctx context.Context, credential string, req frontend.PlanRequest) {
	if req.Protocol != s.protocol || req.Device.ID == "" || req.Viewer == nil || req.Item == 0 || req.Channel != "" || req.Intent.Hints != nil {
		return
	}
	warm, ok := s.broker.deps.Playback.(core.Prewarmer)
	if !ok || s.broker.deps.MediaLookup == nil {
		return
	}
	_, viewer, err := s.grants.GrantReference(ctx, string(s.protocol), credential)
	if err != nil || viewer == nil || viewer.ID != req.Viewer.ID {
		return
	}
	if viewer, err = s.broker.confirm(ctx, viewer, req.Caps); err != nil {
		return
	}
	title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
	if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
		return
	}
	// Plan keys and profiles the later Prepare with the broker's caps, not
	// the declaration; warm with the same caps or Play never finds the entry.
	warm.Prewarm(context.WithoutCancel(ctx), viewer, req.Item, s.broker.playCaps(viewer, req.Protocol, req.Device, req.Caps), core.PrewarmOptions{Reason: core.PrewarmDetail})
}

func (s *lazyStreams) issue(ctx context.Context, credential string, req frontend.PlanRequest, stable bool) (string, error) {
	return s.issueOutput(ctx, credential, req, stable, nil, false, false)
}

// IssueHLS keeps the scoped capability in every playlist and segment URL.
// Planning and encoder startup are deferred until the master is requested.
func (s *lazyStreams) IssueHLS(ctx context.Context, credential string, req frontend.PlanRequest, output frontend.StreamRequest) (string, error) {
	if req.Channel != "" || output.Format != frontend.StreamHLS || output.Position < 0 || output.SegmentDuration < 0 {
		return "", core.ErrNotAvailable
	}
	return s.issueOutput(ctx, credential, req, false, &output, false, false)
}

// IssueLineup binds one media-only capability to an M3U viewer and device.
// The channel remains a request-time policy decision, never a stored wildcard
// authorization. It is isolated from ordinary item and live capabilities.
func (s *lazyStreams) IssueLineup(ctx context.Context, credential string, req frontend.PlanRequest) (string, error) {
	if s.protocol != "m3u" || req.Item != 0 || req.Channel != "" {
		return "", core.ErrUnauthorized
	}
	return s.issueOutput(ctx, credential, req, true, nil, true, false)
}

func (s *lazyStreams) issueOutput(ctx context.Context, credential string, req frontend.PlanRequest, stable bool, output *frontend.StreamRequest, lineup, activeReuse bool) (string, error) {
	if req.Protocol != s.protocol || req.Device.ID == "" || req.Viewer == nil || req.Intent.Hints != nil || (!lineup && (req.Item == 0) == (req.Channel == "")) || (stable && req.Channel != "") {
		return "", core.ErrUnauthorized
	}
	ref, viewer, err := s.grants.GrantReference(ctx, string(s.protocol), credential)
	if err != nil || viewer == nil || viewer.ID != req.Viewer.ID || ref == "" {
		return "", core.ErrUnauthorized
	}
	// Validate policy and item without resolving a release or consuming a stream.
	req.Viewer = viewer
	if lineup {
		if _, err = s.broker.confirm(ctx, viewer, req.Caps); err != nil || s.broker.deps.Live == nil {
			return "", core.ErrUnauthorized
		}
	} else if stable {
		// Authenticated paths are requested for every range. Check only local
		// policy facts here; resolving a release belongs to the first GET.
		if s.broker.deps.MediaLookup == nil {
			return "", core.ErrUnauthorized
		}
		if viewer, err = s.broker.confirm(ctx, viewer, req.Caps); err != nil {
			return "", core.ErrUnauthorized
		}
		title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
		if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
			if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			return "", core.ErrNotFound
		}
	} else {
		estimate := req
		estimate.Version = "" // Version lookup may resolve releases; defer it until GET.
		// Track positions can only be validated against the opened source.
		// The original request is retained for the later playback admission.
		estimate.Intent.AudioOrdinal = nil
		estimate.Intent.SubtitleStreamIndex = nil
		estimate.Intent.Estimate = true
		if _, err = s.broker.Plan(ctx, estimate); err != nil {
			return "", err
		}
	}
	req.Viewer = nil
	req.Intent.Estimate = false
	data, err := json.Marshal(struct {
		frontend.PlanRequest
		HLS    *frontend.StreamRequest `json:"_lazyHLS,omitempty"`
		Lineup bool                    `json:"_lazyLineup,omitempty"`
	}{req, output, lineup})
	if err != nil || len(data) > 65536 {
		return "", core.ErrNotAvailable
	}
	token := rand.Text()
	if stable {
		// This token never leaves the process: it keys an authenticated path's
		// reusable session and cannot be used as the issuing credential.
		identity, err := json.Marshal(struct {
			Protocol           frontend.Protocol
			ViewerID           int64
			DeviceID           string
			Item               core.RatingKey
			Version            string
			ExpectedSourceETag string
			DetailSource       string
		}{s.protocol, viewer.ID, req.Device.ID, req.Item, req.Version, req.ExpectedSourceETag, req.DetailSource})
		if err != nil {
			return "", core.ErrNotAvailable
		}
		if lineup {
			identity, err = json.Marshal(req)
			if err != nil {
				return "", core.ErrNotAvailable
			}
			identity = append([]byte("lineup\x00"), identity...)
		}
		mac := hmac.New(sha256.New, []byte(credential))
		_, _ = mac.Write([]byte("ether-authenticated-lazy-v1\x00"))
		_, _ = mac.Write(identity)
		token = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	now := s.broker.deps.Now()
	expires := now.Add(s.ttl)
	if expiring, ok := s.grants.(interface {
		GrantReferenceExpiry(context.Context, string, string, int64) (time.Time, error)
	}); ok {
		grantExpiry, err := expiring.GrantReferenceExpiry(ctx, string(s.protocol), ref, viewer.ID)
		if err != nil {
			return "", core.ErrUnauthorized
		}
		if !grantExpiry.IsZero() && grantExpiry.Before(expires) {
			expires = grantExpiry
		}
	}
	if !expires.After(now) {
		return "", core.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Serialize with old writers before taking the shared reservation lock.
	if _, err = tx.ExecContext(ctx, `UPDATE sessions_lazy_capacity SET used=used WHERE id=1`); err != nil {
		return "", err
	}
	// Domain guards account for every writer, including binaries without common admission.
	if _, err = capability.PruneHeadersInTx(ctx, tx, "lazy-stream", now.UnixMilli(), capability.HeaderBatchMax); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE expires<=? AND NOT EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=sessions_lazy_streams.hash)`, now.UnixMilli()); err != nil {
		return "", err
	}

	var present int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams WHERE hash=?`, lazyHash(token)).Scan(&present); err != nil {
		return "", err
	}
	if present == 0 {
		if err = capability.AdmitHeadersInTx(ctx, s.db, tx, []capability.Header{{HeaderScope: capability.HeaderScope{Namespace: "lazy-stream", ViewerID: viewer.ID, Surface: string(s.protocol)}, Digest: lazyHash(token), ExpiresMillis: expires.UnixMilli()}}, capability.Limits{PerScope: maxLazyViewerCapabilities, PerShard: maxLazyCapabilities}); err != nil {
			return "", err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, lazyHash(token), string(s.protocol), viewer.ID, ref, string(data), expires.UnixMilli())
	if err != nil {
		return "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 {
		if !stable {
			return "", core.ErrNotAvailable
		}
		var oldRaw, oldRef string
		var oldViewer int64
		if err := tx.QueryRowContext(ctx, `SELECT request,grant_reference,viewer_id FROM sessions_lazy_streams WHERE hash=? AND protocol=? AND expires>?`, lazyHash(token), string(s.protocol), now.UnixMilli()).Scan(&oldRaw, &oldRef, &oldViewer); err != nil {
			return "", core.ErrNotAvailable
		}
		var previous frontend.PlanRequest
		if oldRef != ref || oldViewer != viewer.ID || json.Unmarshal([]byte(oldRaw), &previous) != nil || !sameLazyDecision(previous, req) {
			return "", core.ErrNotAvailable
		}
		var viewerCount, totalCount int
		if err = tx.QueryRowContext(ctx, `SELECT used FROM sessions_lazy_viewer_capacity WHERE viewer_id=?`, viewer.ID).Scan(&viewerCount); err != nil {
			return "", err
		}
		if err = tx.QueryRowContext(ctx, `SELECT used FROM sessions_lazy_capacity WHERE id=1`).Scan(&totalCount); err != nil {
			return "", err
		}
		if (viewerCount > maxLazyViewerCapabilities || totalCount > maxLazyCapabilities) && !(activeReuse && s.broker.activeLazyBinding(lazyHash(token), viewer.ID, s.protocol, req.Device.ID)) {
			return "", &capability.CapacityError{Scope: "viewer"}
		}
		if err = s.renewHeader(ctx, tx, lazyHash(token), viewer.ID, expires.UnixMilli(), now.UnixMilli()); err != nil {
			return "", err
		}
		result, err = tx.ExecContext(ctx, `UPDATE sessions_lazy_streams SET expires=? WHERE hash=? AND protocol=? AND request=? AND expires>?`, expires.UnixMilli(), lazyHash(token), string(s.protocol), oldRaw, now.UnixMilli())
		if err != nil {
			return "", err
		}
		if updated, err := result.RowsAffected(); err != nil || updated != 1 {
			return "", core.ErrNotAvailable
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if stable {
		s.broker.mu.Lock()
		if entry, ok := s.broker.lazySessions[lazyHash(token)]; ok {
			entry.expires = expires.UnixMilli()
			s.broker.lazySessions[lazyHash(token)] = entry
		}
		s.broker.mu.Unlock()
	}
	if lineup {
		return "/sessions/lineup/" + token + "/", nil
	}
	if req.Channel != "" {
		return "/sessions/lazy/" + token + "/live/index.m3u8", nil
	}
	if output != nil {
		return "/sessions/lazy/" + token + "/hls/master.m3u8", nil
	}
	return "/sessions/lazy/" + token + "/file", nil
}

// sameLazyDecision ignores display-only fields and the current network flag.
// Current locality is re-derived on every request before the broker uses it.
func sameLazyDecision(a, b frontend.PlanRequest) bool {
	normalize := func(r frontend.PlanRequest) frontend.PlanRequest {
		r.Device.Client = ""
		r.Device.DeviceName = ""
		r.Caps.DeviceName = ""
		r.Caps.ClientID = ""
		r.Caps.Remote = false
		return r
	}
	x, errX := json.Marshal(normalize(a))
	y, errY := json.Marshal(normalize(b))
	return errX == nil && errY == nil && string(x) == string(y)
}

// ServeAuthenticated is for a surface such as WebDAV or Infuse Static=true
// that authenticates every GET/HEAD and has a stable logical path rather
// than an issued capability URL.
func (s *lazyStreams) ServeAuthenticated(w http.ResponseWriter, r *http.Request, credential string, req frontend.PlanRequest) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	path, err := s.issueOutput(r.Context(), credential, req, true, nil, false, true)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s.writePlaybackError(w, r, err)
		return
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Path = path
	u.RawPath = ""
	clone.URL = &u
	s.ServeHTTP(w, clone)
}

func (s *lazyStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediasafety.SetHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/sessions/lazy/")
	lineup := false
	if !ok {
		rest, ok = strings.CutPrefix(r.URL.Path, "/sessions/lineup/")
		lineup = ok
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, tail, ok := strings.Cut(rest, "/")
	channel := ""
	if lineup {
		encoded, remaining, found := strings.Cut(tail, "/")
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if !found || err != nil || len(decoded) == 0 || len(decoded) > 512 || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
			http.NotFound(w, r)
			return
		}
		channel, tail = string(decoded), remaining
	}
	if !ok || token == "" || len(tail) > 1024 || !validLazyTail(tail) {
		http.NotFound(w, r)
		return
	}
	hash := lazyHash(token)
	ctx := r.Context()
	var viewerID, expires int64
	var reference, raw string
	err := s.db.QueryRowContext(ctx, `SELECT viewer_id,grant_reference,request,expires FROM sessions_lazy_streams WHERE hash=? AND protocol=?`, hash, string(s.protocol)).Scan(&viewerID, &reference, &raw, &expires)
	if err != nil || expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.broker.deps.ActiveViewer(ctx, viewerID)
	if err != nil || viewer == nil {
		http.NotFound(w, r)
		return
	}
	var stored struct {
		frontend.PlanRequest
		HLS    *frontend.StreamRequest `json:"_lazyHLS,omitempty"`
		Lineup bool                    `json:"_lazyLineup,omitempty"`
	}
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.Protocol != s.protocol || stored.Device.ID == "" {
		http.NotFound(w, r)
		return
	}
	if stored.Lineup != lineup || (lineup && (s.protocol != "m3u" || stored.Item != 0 || stored.Channel != "" || stored.HLS != nil)) {
		http.NotFound(w, r)
		return
	}
	req := stored.PlanRequest
	binding := hash
	if lineup {
		req.Channel = channel
		binding = hash + "/" + base64.RawURLEncoding.EncodeToString([]byte(channel))
	}
	if stored.HLS != nil && (stored.HLS.Format != frontend.StreamHLS || stored.HLS.Position < 0 || stored.HLS.SegmentDuration < 0 || req.Channel != "") {
		http.NotFound(w, r)
		return
	}

	req.Caps.Remote = req.Caps.Remote || s.remote(r)
	fresh, err := s.broker.confirm(ctx, viewer, req.Caps)
	if err != nil || (req.Item == 0) == (req.Channel == "") {
		http.NotFound(w, r)
		return
	}
	viewer = fresh
	req.Viewer = viewer
	if req.Channel != "" {
		if !strings.HasPrefix(tail, "live/") || s.broker.deps.Live == nil || s.broker.deps.Live.Allowed(core.WithLiveApp(ctx, string(s.protocol)), viewer, req.Channel) != nil {
			http.NotFound(w, r)
			return
		}
	} else {
		// Missing cache facts fail closed rather than causing network lookups on
		// every range request.
		if (stored.HLS == nil && tail != "file") || (stored.HLS != nil && !strings.HasPrefix(tail, "hls/")) || s.broker.deps.MediaLookup == nil {
			http.NotFound(w, r)
			return
		}
		title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
		if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
			http.NotFound(w, r)
			return
		}
	}
	if r.Method == http.MethodHead {
		if expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
			http.NotFound(w, r)
			return
		}
		if (req.Channel != "" && tail != "live/index.m3u8") || (stored.HLS != nil && tail != "hls/master.m3u8") {
			s.broker.mu.Lock()
			id := s.broker.lazySessions[binding].id
			s.broker.mu.Unlock()
			access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
			if existing, _, err := s.broker.access(ctx, access, id); id == "" || err != nil || (req.Caps.Remote && !existing.caps.Remote) {
				http.NotFound(w, r)
				return
			}
		} else if stored.HLS != nil {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		} else if req.Item != 0 {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}
	// Reserve before opening a source. Stuck dependencies retain a slot, so
	// they cannot create an unbounded number of heartbeat goroutines.
	if !s.broker.reserveLazyHeartbeat() {
		http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
		return
	}
	heartbeatOwned := true
	defer func() {
		if heartbeatOwned {
			s.broker.releaseLazyHeartbeat()
		}
	}()
	if s.protocol == "xtream" {
		streamCtx, release, err := s.acquireXtreamConnection(r, reference, viewerID, strconv.FormatInt(int64(req.Item), 10)+"/"+req.Channel)
		if err != nil {
			http.Error(w, "Connection limit reached", http.StatusTooManyRequests)
			return
		}
		defer release()
		ctx = streamCtx
		r = r.WithContext(ctx)
	}
	unlock, err := s.broker.gates.Acquire(ctx, "lazy/"+hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { unlock() }()
	if s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
		http.NotFound(w, r)
		return
	}
	s.broker.mu.Lock()
	for key, entry := range s.broker.lazySessions {
		if entry.id != "" && s.broker.sessions[entry.id] == nil {
			delete(s.broker.lazySessions, key)
		}
	}
	id := s.broker.lazySessions[binding].id
	s.broker.mu.Unlock()
	access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
	var existingLive, existingHLS *brokerSession
	if id != "" {
		if existing, _, err := s.broker.access(ctx, access, id); err != nil {
			id = ""
		} else if req.Caps.Remote && !existing.caps.Remote {
			if err := s.broker.Stop(ctx, access, id); err != nil {
				http.NotFound(w, r)
				return
			}
			id = ""
		} else if req.Channel != "" && existing.live && existing.public.Channel == req.Channel {
			existingLive = existing
		} else if req.Channel != "" {
			id = ""
		} else if stored.HLS != nil && existing.format == frontend.StreamHLS && !existing.live {
			existingHLS = existing
		}
	}
	if req.Channel != "" && tail != "live/index.m3u8" && id == "" {
		http.NotFound(w, r)
		return
	}
	if stored.HLS != nil && tail != "hls/master.m3u8" && existingHLS == nil {
		http.NotFound(w, r)
		return
	}
	opened := false
	reserved := false
	defer func() {
		if reserved {
			s.broker.releaseLazyCleanup()
		}
	}()
	if id == "" {
		if !s.broker.reserveLazyBinding(binding, viewerID) {
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		defer s.broker.releaseLazyBindingFlight(binding)
		if !s.broker.reserveLazyCleanup() {
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		reserved = true
		plan, err := s.broker.Plan(ctx, req)
		if err != nil {
			s.writePlaybackError(w, r, err)
			return
		}
		session, err := s.broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: req.Device, Protocol: s.protocol, PlanID: plan.ID})
		if err != nil {
			s.writePlaybackError(w, r, err)
			return
		}
		id = session.ID
		opened = true
		s.broker.mu.Lock()
		if s.broker.lazySessions == nil {
			s.broker.lazySessions = make(map[string]lazySession)
		}
		s.broker.lazySessions[binding] = lazySession{id: id, expires: expires, viewer: viewerID}
		s.broker.mu.Unlock()
	}
	format := frontend.StreamDirect
	if req.Channel != "" || stored.HLS != nil {
		format = frontend.StreamHLS
	}
	var path string
	if existingLive != nil {
		// Live sessions are already started; the scoped media handler below
		// revalidates current channel policy again before serving each resource.
		path = "/sessions/media/" + id + "/" + existingLive.grant + "/live/index.m3u8"
	} else if existingHLS != nil && tail != "hls/master.m3u8" {
		path = "/sessions/media/" + id + "/" + existingHLS.grant + "/master.m3u8"
	} else {
		stream := frontend.StreamRequest{Format: format}
		if stored.HLS != nil {
			stream = *stored.HLS
		}
		path, err = s.broker.StartStream(ctx, access, id, stream)
	}
	if err != nil {
		if opened {
			if s.discardOpened(ctx, access, id) {
				reserved = false // retry worker owns the reservation
			}
			s.broker.mu.Lock()
			delete(s.broker.lazySessions, binding)
			s.broker.mu.Unlock()
		}
		s.writePlaybackError(w, r, err)
		return
	}
	if expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil || s.renewActiveBinding(ctx, hash, binding, reference, viewerID, id) != nil {
		if opened {
			if s.discardOpened(ctx, access, id) {
				reserved = false // retry worker owns the reservation
			}
			s.broker.mu.Lock()
			delete(s.broker.lazySessions, binding)
			s.broker.mu.Unlock()
		}
		http.NotFound(w, r)
		return
	}
	if reserved {
		s.broker.releaseLazyCleanup()
		reserved = false
	}
	// The scoped path remains internal; every client request must pass this
	// handler so revoking the issuing grant revokes the capability as well.
	streamCtx, cancelStream := context.WithCancel(ctx)
	unregister, ok := s.broker.registerLazyTransfer(id, cancelStream)
	if !ok {
		cancelStream()
		http.NotFound(w, r)
		return
	}
	defer unregister()
	sourceHeader := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "lazy-stream", ViewerID: viewerID, Surface: string(s.protocol)}, Digest: hash, ExpiresMillis: expires}
	targetHeader := capability.Header{HeaderScope: capability.HeaderScope{Namespace: brokerTransferNamespace, ViewerID: viewerID, Surface: string(s.protocol), DeviceID: req.Device.ID}, Digest: lazyHash(hash + "\x00" + id + "\x00" + rand.Text()), ExpiresMillis: s.broker.deps.Now().Add(2 * time.Hour).UnixMilli()}
	endFlight, flightErr := s.broker.beginLazyFlight(ctx, id, s.db, sourceHeader, targetHeader)
	if flightErr != nil {
		cancelStream()
		if !errors.Is(flightErr, errTransferCommitUnknown) && s.broker.deps.OnEventError != nil {
			s.broker.deps.OnEventError(flightErr)
		}
		http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
		return
	}
	defer endFlight()
	done := make(chan struct{})
	heartbeatOwned = false
	go func() {
		defer close(done)
		defer s.broker.releaseLazyHeartbeat()
		s.heartbeatBinding(streamCtx, cancelStream, hash, binding, reference, viewerID, id, req)
	}()
	defer func() {
		cancelStream()
		// Real dependencies honor context cancellation. A broken dependency
		// must not hold HTTP teardown forever; its slot remains reserved until
		// its worker actually exits.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			if s.broker.deps.OnEventError != nil {
				s.broker.deps.OnEventError(errors.New("sessions: lazy heartbeat ignored cancellation; its capacity remains reserved until it exits or the process restarts"))
			}
		}
	}()
	clone := r.Clone(streamCtx)
	u := *r.URL
	u.Path = path
	if req.Channel != "" {
		u.Path = strings.TrimSuffix(path, "index.m3u8") + strings.TrimPrefix(tail, "live/")
	} else if stored.HLS != nil {
		u.Path = strings.TrimSuffix(path, "master.m3u8") + strings.TrimPrefix(tail, "hls/")
	}
	u.RawPath = ""
	clone.URL = &u
	unlock()
	unlock = func() {}
	if req.Channel != "" {
		depth := len(strings.Split(strings.TrimPrefix(tail, "live/"), "/")) - 1
		base := "./"
		if depth > 0 {
			base = strings.Repeat("../", depth)
		}
		s.broker.serveMediaAt(s.protocol, nil, w, clone, base)
	} else {
		s.broker.ServeMediaFor(s.protocol, w, clone)
	}
}

func (s *lazyStreams) sourceGone(w http.ResponseWriter, r *http.Request) {
	serveSourceGone(s.protocol, w, r)
}

// writePlaybackError answers a static open. A missing file is Jellyfin's
// 404. Only a full encoder or admission slot stays 503, with Retry-After
// when the broker named a delay.
func (s *lazyStreams) writePlaybackError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrNotAvailable) || errors.Is(err, frontend.ErrSourceGone) {
		s.sourceGone(w, r)
		return
	}
	var capacity *frontend.TranscodeCapacityError
	var busy *frontend.SessionBusyError
	var retry time.Duration
	if errors.As(err, &capacity) && capacity != nil {
		retry = capacity.RetryAfter
	} else if errors.As(err, &busy) && busy != nil {
		retry = busy.RetryAfter
	}
	if retry > 0 {
		seconds := int64(retry / time.Second)
		if retry%time.Second != 0 {
			seconds++
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
}
func serveSourceGone(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
	if protocol == "jellyfin" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "Error processing request.")
		}
		return
	}
	http.NotFound(w, r)
}

// Once all slots are held (including by dependencies that ignored cancellation),
// new transfers fail closed with 503 until a worker exits or the process restarts.
const maxLazyHeartbeats = 1024

type lazyTransfer struct{ _ byte }

func (b *Broker) registerLazyTransfer(id string, cancel context.CancelFunc) (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.sessions[id] == nil {
		return nil, false
	}
	if b.lazyTransfers == nil {
		b.lazyTransfers = make(map[string]map[*lazyTransfer]context.CancelFunc)
	}
	if b.lazyTransfers[id] == nil {
		b.lazyTransfers[id] = make(map[*lazyTransfer]context.CancelFunc)
	}
	key := &lazyTransfer{}
	b.lazyTransfers[id][key] = cancel
	return func() {
		b.mu.Lock()
		delete(b.lazyTransfers[id], key)
		if len(b.lazyTransfers[id]) == 0 {
			delete(b.lazyTransfers, id)
		}
		b.mu.Unlock()
	}, true
}

func (b *Broker) reserveLazyHeartbeat() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.lazyHeartbeatCount >= maxLazyHeartbeats {
		return false
	}
	b.lazyHeartbeatCount++
	return true
}

func (b *Broker) releaseLazyHeartbeat() {
	b.mu.Lock()
	b.lazyHeartbeatCount--
	b.mu.Unlock()
}

func (s *lazyStreams) heartbeatBinding(ctx context.Context, cancel context.CancelFunc, hash, binding, reference string, viewerID int64, id string, req frontend.PlanRequest) {
	interval := min(time.Minute, s.ttl/3)
	var ticks <-chan time.Time
	var stop func()
	if s.newHeartbeat != nil {
		ticks, stop = s.newHeartbeat(interval)
	} else {
		ticker := time.NewTicker(interval)
		ticks, stop = ticker.C, ticker.Stop
	}
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			viewer, err := s.broker.deps.ActiveViewer(ctx, viewerID)
			if _, confirmErr := s.broker.confirm(ctx, viewer, req.Caps); err != nil || viewer == nil || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil || confirmErr != nil {
				cancel()
				return
			}
			access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
			if _, _, err = s.broker.access(ctx, access, id); err != nil {
				cancel()
				return
			}
			if req.Channel != "" {
				if s.broker.deps.Live == nil || s.broker.deps.Live.Allowed(core.WithLiveApp(ctx, string(s.protocol)), viewer, req.Channel) != nil {
					cancel()
					return
				}
			} else {
				if s.broker.deps.MediaLookup == nil {
					cancel()
					return
				}
				title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
				if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
					cancel()
					return
				}
			}
			if s.renewActiveBinding(ctx, hash, binding, reference, viewerID, id) != nil {
				cancel()
				return
			}
		}
	}
}

func (s *lazyStreams) renewActiveBinding(ctx context.Context, hash, binding, reference string, viewerID int64, sessionID string) error {
	now := s.broker.deps.Now()
	until := now.Add(s.ttl)
	if expiring, ok := s.grants.(interface {
		GrantReferenceExpiry(context.Context, string, string, int64) (time.Time, error)
	}); ok {
		grantExpiry, err := expiring.GrantReferenceExpiry(ctx, string(s.protocol), reference, viewerID)
		if err != nil {
			return err
		}
		if !grantExpiry.IsZero() && grantExpiry.Before(until) {
			until = grantExpiry
		}
	}
	if !until.After(now) {
		return core.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sessions_lazy_capacity SET used=used WHERE id=1`); err != nil {
		return err
	}
	if err = s.renewHeader(ctx, tx, hash, viewerID, until.UnixMilli(), now.UnixMilli()); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions_lazy_streams SET expires=? WHERE hash=? AND protocol=? AND viewer_id=? AND grant_reference=? AND expires>?`, until.UnixMilli(), hash, string(s.protocol), viewerID, reference, now.UnixMilli())
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return core.ErrUnauthorized
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.broker.mu.Lock()
	if entry, ok := s.broker.lazySessions[binding]; ok && entry.id == sessionID {
		entry.expires = until.UnixMilli()
		s.broker.lazySessions[binding] = entry
	}
	s.broker.mu.Unlock()
	return nil
}

func validLazyTail(tail string) bool {
	if tail == "file" {
		return true
	}
	if !strings.HasPrefix(tail, "live/") && !strings.HasPrefix(tail, "hls/") {
		return false
	}
	for _, part := range strings.Split(tail, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return false
		}
	}
	return true
}

// discardOpened rolls back a session that this request just opened, even when
// its viewer is no longer active. It only operates on the matching local
// session and takes the same ownership gate as normal stop operations.
func (s *lazyStreams) discardOpened(ctx context.Context, access frontend.SessionAccess, id string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.broker.lazyCleanupWait)
	defer cancel()
	s.broker.mu.Lock()
	entry := s.broker.sessions[id]
	if entry == nil || access.Viewer == nil || entry.public.ViewerID != access.Viewer.ID || entry.public.Protocol != access.Protocol || entry.public.Device.ID != access.Device.ID {
		s.broker.mu.Unlock()
		return false
	}
	cleanup := lazyCleanupEntry{session: entry, viewerID: access.Viewer.ID, deviceID: access.Device.ID, protocol: access.Protocol, clientID: entry.clientID}
	s.broker.mu.Unlock()
	if s.broker.attemptLazyCleanup(ctx, cleanup) {
		return false
	}
	return s.broker.queueLazyCleanup(id, cleanup)
}

// Renewal extends an in-use capability without adding a reservation. Current
// policy and source-grant lifetime are checked before this transaction.
func (s *lazyStreams) renewHeader(ctx context.Context, tx *sql.Tx, hash string, viewer, until, now int64) error {
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "lazy-stream", ViewerID: viewer, Surface: string(s.protocol)}, Digest: hash, ExpiresMillis: until}
	renewed, err := capability.RenewHeaderInTx(ctx, s.db, tx, h, now)
	if err != nil {
		return err
	}
	if !renewed {
		return core.ErrUnauthorized
	}
	return nil
}
