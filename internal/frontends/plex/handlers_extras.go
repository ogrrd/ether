package plex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Extras (trailers and other clips) and Cinema Trailers. Extras never go
// through the Usenet playback manager: they resolve to a direct URL or a
// local file and are remuxed/transcoded like any other transcode session,
// or direct-played through the part proxy.
// While a trailer plays, the feature behind it is prewarmed so it starts
// instantly when the trailer ends.

// ExtrasProvider is the extras service surface the front end needs.
type ExtrasProvider = frontend.ExtrasProvider

// extraSession is one resolved extra playback (decision + transcode).
type extraSession struct {
	id        string
	userID    int64
	clientID  string
	extra     frontend.ExtraClip
	resolved  frontend.ResolvedExtra
	info      frontend.MediaInfo
	decision  frontend.Decision
	caps      frontend.ClientCaps
	opts      frontend.DecideOptions
	offset    time.Duration
	started   bool
	updatedAt time.Time
}

type extraPlayStore struct {
	mu       sync.Mutex
	sessions map[string]*extraSession
	warmed   map[string]time.Time
	client   *http.Client // part proxy; no total timeout (trailers stream)
}

func newExtraPlayStore(client *http.Client) *extraPlayStore {
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &extraPlayStore{
		sessions: map[string]*extraSession{},
		warmed:   map[string]time.Time{},
		client:   client,
	}
}

func (st *extraPlayStore) get(id string) (*extraSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	if !ok {
		return nil, false
	}
	cp := *s
	return &cp, true
}

func (st *extraPlayStore) put(s *extraSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := *s
	st.sessions[s.id] = &cp
}

// shouldWarm deduplicates feature prewarms per viewer for ten minutes.
func (st *extraPlayStore) shouldWarm(userID int64, feature frontend.RatingKey, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := fmt.Sprintf("%d|%d", userID, int64(feature))
	if at, ok := st.warmed[k]; ok && now.Sub(at) < 10*time.Minute {
		return false
	}
	st.warmed[k] = now
	return true
}

func (st *extraPlayStore) expire(now time.Time, ttl time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, s := range st.sessions {
		if now.Sub(s.updatedAt) > ttl {
			delete(st.sessions, id)
		}
	}
	for k, at := range st.warmed {
		if now.Sub(at) > ttl {
			delete(st.warmed, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Listing and rendering

func (s *Server) viewerLanguage(pr *Request) string {
	if pr.Language != "" {
		return pr.Language
	}
	return s.cfg().Catalog.Language
}

// extrasFor lists a title's playable extras, honouring the extraTypes
// filter (comma-separated Plex numeric types) when present.
func (s *Server) extrasFor(ctx context.Context, pr *Request, parent *frontend.Title, extraTypes string) []frontend.ExtraClip {
	want := map[int]bool{}
	for _, part := range strings.Split(extraTypes, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			want[n] = true
		}
	}
	list, err := s.d.Experience.DetailExtras(ctx, s.d.Extras, parent, s.viewerLanguage(pr), want)
	if err != nil {
		s.log.Debug("extras list", "key", parent.Key, "err", err)
		return nil
	}
	return list
}

// extraEstimateInfo is the virtual media info of an extra before (or
// without) a probe: hosted trailers are overwhelmingly H.264/AAC MP4.
func extraEstimateInfo(e frontend.ExtraClip) frontend.MediaInfo {
	h := e.Size
	if h <= 0 {
		h = 720
	}
	w := h * 16 / 9
	return frontend.MediaInfo{
		Container: "mp4", Duration: e.Duration, BitrateKbps: 8000, Estimated: true,
		Streams: []frontend.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: w, Height: h, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true, Language: e.Language},
		},
	}
}

// extraMetadata renders one extra as PMS does: a clip Video with a
// subtype and numeric extra type, carrying a single Media/Part.
func (s *Server) extraMetadata(e frontend.ExtraClip, parent *frontend.Title, withMedia bool) model.Metadata {
	if e.Source == prerollSource {
		return s.prerollMetadata(e, frontend.PrerollClip{UpdatedAt: e.PublishedAt}, false)
	}
	thumb := s.thumbURL(parent)
	if parent.BackdropPath != "" {
		thumb = artURL(parent) // 16:9 backdrop suits trailer thumbs
	}
	md := model.Metadata{
		RatingKey:       e.Key.String(),
		Key:             "/library/metadata/" + e.Key.String(),
		ParentRatingKey: parent.Key.String(),
		GUID:            guidFor(&frontend.Title{Key: e.Key}),
		Type:            "clip",
		Subtype:         e.Subtype,
		Title:           e.Title,
		ParentKey:       "/library/metadata/" + parent.Key.String(),
		ParentTitle:     parent.Title,
		Thumb:           thumb,
		Art:             artURL(parent),
		ContentRating:   parent.ContentRating,
		Duration:        ms(e.Duration),
		Extra:           []model.Attr{model.A("extraType", e.ExtraType)},
	}
	if !e.PublishedAt.IsZero() {
		md.Year = e.PublishedAt.Year()
		md.OriginallyAvailableAt = e.PublishedAt.Format("2006-01-02")
		md.AddedAt = e.PublishedAt.Unix()
	}
	if sec, ok := s.sectionFor(parent.Type); ok {
		md.LibrarySectionID = sec.ID
		md.LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
		md.LibrarySectionTitle = sec.Title
	}
	if withMedia {
		syn := &frontend.Title{Key: e.Key, Type: frontend.TypeMovie, Title: e.Title, Runtime: e.Duration}
		md.Media = []model.Media{buildMedia(syn, extraEstimateInfo(e), 1)}
		if md.Duration == 0 {
			md.Duration = md.Media[0].Duration
		}
	}
	return md
}

// extraMetadataByKey renders a registered extra whose parent the viewer
// may see.
func (s *Server) extraMetadataByKey(ctx context.Context, pr *Request, key frontend.RatingKey) (model.Metadata, bool) {
	if key.IsPreroll() {
		e, clip, ok := s.prerollExtra(ctx, key)
		if !ok {
			return model.Metadata{}, false
		}
		return s.prerollMetadata(e, clip, true), true
	}
	if s.d.Extras == nil {
		return model.Metadata{}, false
	}
	e, ok := s.d.Extras.Lookup(key)
	if !ok {
		return model.Metadata{}, false
	}
	parent, err := s.metadataTitle(ctx, pr, e.ParentKey)
	if err != nil || !s.visibleTitle(ctx, pr.User, parent) {
		return model.Metadata{}, false
	}
	return s.extraMetadata(e, parent, true), true
}

// handleExtras serves /library/metadata/{key}/extras.
func (s *Server) handleExtras(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil || k.Kind() == frontend.KindExtra {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	parent, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, parent) {
		err = frontend.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(parent.Type)
	for _, e := range s.extrasFor(ctx, pr, parent, r.URL.Query().Get("extraTypes")) {
		mc.Metadata = append(mc.Metadata, s.extraMetadata(e, parent, true))
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Playback

// extraKeyFromRequest returns the KindExtra key a universal playback
// request addresses, when it addresses one.
func extraKeyFromRequest(r *http.Request) (frontend.RatingKey, bool) {
	q := r.URL.Query()
	for _, name := range []string{"path", "key", "ratingKey"} {
		if k := keyFromPath(q.Get(name)); k.Kind() == frontend.KindExtra {
			return k, true
		}
	}
	return 0, false
}

func (s *Server) parseExtraPlayRequest(r *http.Request, key frontend.RatingKey) playRequest {
	pr := FromContext(r.Context())
	q := r.URL.Query()
	caps := s.clientCaps(pr)
	applyDecisionQuery(&caps, q)
	p := playRequest{key: key, caps: caps, audioSel: -1, subSel: -1, protocol: q.Get("protocol"), mde: q.Get("hasMDE") == "1", listAllTracks: androidFamilyClient(pr)}
	if p.protocol == "" {
		p.protocol = "hls"
	}
	p.opts = frontend.DecideOptions{
		DirectPlay: q.Get("directPlay") != "0", DirectStream: q.Get("directStream") != "0",
		MaxBitrateKbps: caps.MaxBitrateKbps, MaxHeight: parseResolutionHeight(q.Get("videoResolution")),
		AudioStream: -1, SubtitleStream: -1,
	}
	if vb, err := strconv.Atoi(q.Get("videoBitrate")); err == nil && vb > 0 && (p.opts.MaxBitrateKbps == 0 || vb < p.opts.MaxBitrateKbps) {
		p.opts.MaxBitrateKbps = vb
	}
	p.sessionID = cmp.Or(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID)
	if p.sessionID == "" {
		p.sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
	}
	p.sessionID = sanitizeSessionID(p.sessionID)
	p.offset = durationParam(q.Get("offset"), time.Second)
	return p
}

// handleExtraPlayback hooks the universal decision/start endpoints for
// KindExtra keys, like handleLiveTVPlayback does for live sessions.
func (s *Server) handleExtraPlayback(w http.ResponseWriter, r *http.Request) bool {
	if s.refuseCapability(w, r) {
		return true
	}
	key, isExtra := extraKeyFromRequest(r)
	if !isExtra {
		return false
	}
	if key.IsPreroll() {
		s.prerollPlayback(w, r, key)
		return true
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if s.d.Extras == nil || !s.d.Extras.Enabled() || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	e, ok := s.d.Extras.Lookup(key)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	parent, err := s.d.Catalog.Get(ctx, e.ParentKey)
	if err != nil || !s.visibleTitle(ctx, pr.User, parent) {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return true
	}
	s.serveExtraPlayback(w, r, pr, key, e, parent)
	return true
}

// serveExtraPlayback runs an authorized extra's decision or start request.
func (s *Server) serveExtraPlayback(w http.ResponseWriter, r *http.Request, pr *Request, key frontend.RatingKey, e frontend.ExtraClip, parent *frontend.Title) {
	ctx := r.Context()
	p := s.parseExtraPlayRequest(r, key)
	if policy, ok := s.policy().(frontend.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
			p.opts.MaxBitrateKbps = limit
		}
	}
	// The trailer masks the feature's startup cost: warm it now.
	s.prewarmFeature(ctx, pr, s.featureForExtra(pr, e), p.caps)
	switch {
	case strings.HasSuffix(r.URL.Path, "/decision"):
		s.extraDecision(w, r, pr, p, e, parent)
	case strings.HasSuffix(r.URL.Path, "/start.mpd"):
		p.protocol = frontend.ProtocolDASH
		s.extraStart(w, r, pr, p, e, parent, true)
	case strings.HasSuffix(r.URL.Path, "/start"):
		p.protocol = frontend.ProtocolHTTP
		p.opts.Protocol = frontend.ProtocolHTTP
		s.extraStart(w, r, pr, p, e, parent, false)
	default:
		s.extraStart(w, r, pr, p, e, parent, false)
	}
}

// prepareExtra resolves, probes (when possible) and decides an extra,
// recording the session for the start endpoints and transcode stop.
func (s *Server) prepareExtra(ctx context.Context, pr *Request, p *playRequest, e frontend.ExtraClip) (*extraSession, error) {
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		return nil, frontend.ErrUnauthorized
	}
	resolved, err := s.resolveExtra(ctx, e)
	if err != nil {
		return nil, err
	}
	info := extraEstimateInfo(e)
	if resolved.Container != "" {
		info.Container = resolved.Container
	}
	if s.d.Transcoder != nil {
		if pi, perr := s.d.Transcoder.Probe(ctx, resolved.Input()); perr == nil && pi != nil && pi.Video() != nil {
			info = *pi
		}
	}
	if info.Duration == 0 {
		info.Duration = e.Duration
	}
	applySelections(p, info, s.audioLanguages(pr.User))
	var dec frontend.Decision
	if s.d.Transcoder != nil {
		dec = s.d.Transcoder.Decide(info, p.caps, p.opts)
	} else {
		dec = frontend.DecideTranscode(info, p.caps, p.opts, frontend.TranscodePolicy{RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps})
		if dec.Kind != frontend.DecisionDirectPlay {
			return nil, errors.New("this client requires media conversion; install or configure FFmpeg")
		}
	}
	sess := &extraSession{
		id: p.sessionID, userID: pr.User.ID, clientID: pr.ClientID, extra: e,
		resolved: resolved, info: info, decision: dec, caps: p.caps, opts: p.opts,
		offset: p.offset, updatedAt: s.now(),
	}
	if s.extraPlay != nil {
		s.extraPlay.put(sess)
	}
	s.transcodes.put(&transcodeEntry{
		id: p.sessionID, playID: "extra:" + p.sessionID, userID: pr.User.ID,
		clientID: pr.ClientID, key: e.Key, decision: dec, info: info,
		caps: p.caps, opts: p.opts, offset: p.offset, updatedAt: s.now(),
	})
	return sess, nil
}

// resolveExtra returns an extra's playable input; a pre-roll is its
// converted local MP4.
func (s *Server) resolveExtra(ctx context.Context, e frontend.ExtraClip) (frontend.ResolvedExtra, error) {
	if e.Source == prerollSource {
		if s.d.Prerolls == nil {
			return frontend.ResolvedExtra{}, frontend.ErrNotFound
		}
		clip, ok := s.d.Prerolls.Clip(ctx, e.Key)
		if !ok {
			return frontend.ResolvedExtra{}, frontend.ErrNotFound
		}
		return frontend.ResolvedExtra{URL: clip.Path, Container: "mp4"}, nil
	}
	return s.d.Extras.Resolve(ctx, e)
}

func (s *Server) extraSessionFor(ctx context.Context, pr *Request, p *playRequest, e frontend.ExtraClip) (*extraSession, error) {
	if s.extraPlay != nil {
		if sess, ok := s.extraPlay.get(p.sessionID); ok && sess.extra.Key == e.Key && sess.userID == pr.User.ID {
			if sess.resolved.Expires.IsZero() || s.now().Before(sess.resolved.Expires) {
				return sess, nil
			}
		}
	}
	return s.prepareExtra(ctx, pr, p, e)
}

func (s *Server) extraDecision(w http.ResponseWriter, r *http.Request, pr *Request, p playRequest, e frontend.ExtraClip, parent *frontend.Title) {
	ctx := r.Context()
	sess, err := s.prepareExtra(ctx, pr, &p, e)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Info("extra decision failed", "key", e.Key, "err", err)
		msg := "This trailer could not be played right now. Try again in a moment."
		mc := s.itemContainer(parent.Type)
		md := s.extraMetadata(e, parent, false)
		decisionItem(&md)
		mc.Metadata = []model.Metadata{md}
		mc.Size = 1
		mc.SetDecision()
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError), model.A("directPlayDecisionText", msg),
			model.A("generalDecisionCode", codeGeneralError), model.A("generalDecisionText", msg),
			model.A("transcodeDecisionCode", codeTranscodeError), model.A("transcodeDecisionText", msg),
			model.A("resourceSession", cmp.Or(pr.ClientID, p.sessionID)),
		}
		s.write(w, r, mc)
		return
	}
	syn := &frontend.Title{Key: e.Key, Type: frontend.TypeMovie, Title: e.Title, Runtime: sess.info.Duration}
	ps := &frontend.PlaySession{ID: "extra:" + p.sessionID, UserID: pr.User.ID, ClientID: pr.ClientID, Key: e.Key, Info: sess.info, StartedAt: s.now()}
	md := s.extraMetadata(e, parent, false)
	decisionItem(&md)
	md.Media = []model.Media{s.decisionMedia(syn, ps, sess.decision, p, pr.User)}
	if md.Duration == 0 {
		md.Duration = md.Media[0].Duration
	}
	mc := s.itemContainer(parent.Type)
	mc.Metadata = []model.Metadata{md}
	mc.Size = 1
	mc.SetDecision()
	if sess.decision.Kind == frontend.DecisionDirectPlay {
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayOK), model.A("directPlayDecisionText", textDirectPlayOK),
			model.A("generalDecisionCode", codeDirectPlayOK), model.A("generalDecisionText", textDirectPlayOK),
		}
	} else {
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError), model.A("directPlayDecisionText", textDirectPlayDisable),
			model.A("generalDecisionCode", codeConversionOK), model.A("generalDecisionText", textConversionOK),
			model.A("transcodeDecisionCode", codeConversionOK), model.A("transcodeDecisionText", textConversionOK),
		}
	}
	mc.Extra = append(mc.Extra, model.A("resourceSession", cmp.Or(pr.ClientID, p.sessionID)))
	s.write(w, r, mc)
}

func (s *Server) extraStart(w http.ResponseWriter, r *http.Request, pr *Request, p playRequest, e frontend.ExtraClip, parent *frontend.Title, dash bool) {
	ctx := r.Context()
	if s.d.Transcoder == nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	sess, err := s.extraSessionFor(ctx, pr, &p, e)
	if err != nil {
		s.log.Info("extra start failed", "key", e.Key, "err", err)
		s.writeErr(w, r, err)
		return
	}
	dec := sess.decision
	if dec.Kind == frontend.DecisionDirectPlay {
		dec.Kind, dec.VideoCopy, dec.AudioCopy, dec.Container = frontend.DecisionRemux, true, true, "mpegts"
	}
	if dec.Container == "" {
		dec.Container = "mpegts"
	}
	if existing, ok := s.d.Transcoder.Session(p.sessionID); ok && existing != nil {
		if sess.started && sess.offset == p.offset {
			if dash {
				if ds, isDASH := existing.(frontend.DASHSession); isDASH {
					s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
					return
				}
			} else if p.protocol == frontend.ProtocolHTTP {
				if hs, ok := existing.(frontend.HTTPSession); ok {
					hs.ServeHTTP(w, r)
					return
				}
			} else {
				s.writeMasterPlaylist(w, r, p.sessionID, existing, sess.info, dec)
				return
			}
		}
		s.d.Transcoder.Stop(p.sessionID)
	}
	proto := ""
	if dash {
		proto = frontend.ProtocolDASH
	} else if p.protocol == frontend.ProtocolHTTP {
		proto = frontend.ProtocolHTTP
		dec.Container = frontend.ContainerMatroska
	}
	hs, err := s.d.Transcoder.StartHLS(context.WithoutCancel(ctx), frontend.HLSRequest{
		SessionID: p.sessionID, InputURL: sess.resolved.Input(),
		InputHeaders: sess.resolved.Headers, Info: sess.info,
		Decision: dec, StartOffset: p.offset, Protocol: proto,
	})
	if err != nil {
		s.log.Warn("start extra session", "session", p.sessionID, "key", e.Key, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	sess.decision, sess.offset, sess.started, sess.updatedAt = dec, p.offset, true, s.now()
	if s.extraPlay != nil {
		s.extraPlay.put(sess)
	}
	s.prerollStarted(ctx, pr, e)
	if entry, ok := s.transcodes.get(p.sessionID); ok {
		s.bindTranscodeStarted(entry, dec, p.offset)
	}
	if dash {
		ds, ok := hs.(frontend.DASHSession)
		if !ok {
			s.d.Transcoder.Stop(p.sessionID)
			writeHTTPError(w, http.StatusInternalServerError)
			return
		}
		s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
		return
	}
	if p.protocol == frontend.ProtocolHTTP {
		stream, ok := hs.(frontend.HTTPSession)
		if !ok {
			s.d.Transcoder.Stop(p.sessionID)
			writeHTTPError(w, http.StatusInternalServerError)
			return
		}
		stream.ServeHTTP(w, r)
		return
	}
	s.writeMasterPlaylist(w, r, p.sessionID, hs, sess.info, dec)
}

// handleExtraPart serves direct-play part requests for extras by
// proxying byte ranges from the resolved URL (or the local file).
// It returns false when the part is not an extra.
func (s *Server) handleExtraPart(w http.ResponseWriter, r *http.Request) bool {
	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	key := frontend.RatingKey(id)
	if err != nil || key.Kind() != frontend.KindExtra {
		return false
	}
	if key.IsPreroll() {
		s.prerollPart(w, r, key)
		return true
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if s.d.Extras == nil || !s.d.Extras.Enabled() || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	e, ok := s.d.Extras.Lookup(key)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	parent, err := s.d.Catalog.Get(ctx, e.ParentKey)
	if err != nil || !s.visibleTitle(ctx, pr.User, parent) {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return true
	}
	s.prewarmFeature(ctx, pr, s.featureForExtra(pr, e), s.clientCaps(pr))
	resolved, err := s.d.Extras.Resolve(ctx, e)
	if err != nil {
		s.log.Info("extra part resolve failed", "key", e.Key, "err", err)
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved.URL, nil)
	if err != nil {
		writeHTTPError(w, http.StatusBadGateway)
		return true
	}
	for k, v := range resolved.Headers {
		req.Header.Set(k, v)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	client := http.DefaultClient
	if s.extraPlay != nil && s.extraPlay.client != nil {
		client = s.extraPlay.client
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Info("extra part proxy failed", "key", e.Key, "err", err)
			writeHTTPError(w, http.StatusBadGateway)
		}
		return true
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
	return true
}

// ---------------------------------------------------------------------------
// Trailer-masked pre-buffering and Cinema Trailers

// featureForExtra finds the feature an extra is masking: the first
// non-extra playable item after it in the viewer's play queue (a Cinema
// Trailer's feature), else the extra's own parent when playable.
func (s *Server) featureForExtra(pr *Request, e frontend.ExtraClip) frontend.RatingKey {
	if pr.User != nil && s.queues != nil {
		if key := s.queues.FollowingPlayable(pr.User.ID, e.Key, isPlayableKey); key != 0 {
			return key
		}
	}
	if isPlayableKey(e.ParentKey) {
		return e.ParentKey
	}
	return 0
}

// prewarmFeature starts background resolution and pre-buffering of the
// feature while its trailer plays, deduplicated per viewer and feature.
func (s *Server) prewarmFeature(ctx context.Context, pr *Request, feature frontend.RatingKey, caps frontend.ClientCaps) {
	if feature == 0 || pr.User == nil || s.d.Playback == nil {
		return
	}
	pw, ok := s.d.Playback.(frontend.Prewarmer)
	if !ok {
		return
	}
	if s.extraPlay != nil && !s.extraPlay.shouldWarm(pr.User.ID, feature, s.now()) {
		return
	}
	bytes := s.cfg().Extras.PrewarmBytesOr()
	if s.d.Extras != nil {
		bytes = s.d.Extras.PrewarmBytes()
	}
	scheduled := pw.Prewarm(context.WithoutCancel(ctx), pr.User, feature, caps, frontend.PrewarmOptions{
		Reason: frontend.PrewarmTrailer, PrefetchBytes: bytes, DuringPlayback: true,
	})
	s.log.Info("trailer prewarm", "feature", feature, "user", pr.User.Username, "scheduled", scheduled, "bytes", bytes)
}

// cinemaTrailerKeys picks up to n trailers of other, unwatched movies
// for this viewer (PMS Cinema Trailers semantics), in candidate order.
func (s *Server) cinemaTrailerKeys(ctx context.Context, pr *Request, feature frontend.RatingKey, n int) []frontend.RatingKey {
	if n <= 0 || s.d.Extras == nil || !s.d.Extras.Enabled() || s.d.Catalog == nil {
		return nil
	}
	if n > frontend.MaxExtras {
		n = frontend.MaxExtras
	}
	candidates, err := s.recommendationCandidates(ctx, pr.User, 24, func(ctx context.Context, fetch int) ([]frontend.Title, error) {
		items, err := s.d.Catalog.Similar(ctx, feature, fetch)
		if err == nil && len(items) > 0 {
			return items, nil
		}
		page, err := s.d.Catalog.Browse(ctx, frontend.BrowseQuery{
			Filter: frontend.Filter{Type: frontend.TypeMovie}, Sort: frontend.SortPopularity, Desc: true, Limit: fetch,
		})
		return page.Items, err
	})
	if err != nil {
		return nil
	}
	candidates = s.visibleTitles(pr.User, candidates)
	state := s.stateFor(ctx, pr, candidates)
	lang := s.viewerLanguage(pr)
	var out []frontend.RatingKey
	checked := 0
	for i := range candidates {
		c := candidates[i]
		if len(out) >= n || checked >= 12 {
			break
		}
		if c.Key == feature || c.Type != frontend.TypeMovie || state[c.Key].ViewCount > 0 {
			continue
		}
		checked++
		list, err := s.d.Extras.List(ctx, &c, lang)
		if err != nil {
			continue
		}
		for _, e := range list {
			if e.IsTrailer() {
				out = append(out, e.Key)
				break
			}
		}
	}
	return out
}
