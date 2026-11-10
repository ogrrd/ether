package plex

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Decision codes. PMS documents the classes (1xxx ok, 2xxx general error,
// 3xxx direct-play error, 4xxx transcode error) but not every member; these
// are the values observed from real servers.
const (
	codeDirectPlayOK      = 1000
	codeConversionOK      = 1001
	codeGeneralError      = 2000
	codeDirectPlayError   = 3000
	codeTranscodeError    = 4000
	textDirectPlayOK      = "Direct play OK."
	textConversionOK      = "Direct play not available; Conversion OK."
	textDirectPlayDisable = "App cannot direct play this item. Direct play is disabled."
)

// User-visible playback failure messages.
const (
	msgNotAvailable = "Not available yet: no playable release of this title was found. Try again later."
	msgStreamLimit  = "Stream limit reached: stop another stream on this server and try again."
	msgPlayFailed   = "This title could not be played right now. Try again in a moment."
	msgPrepareSlow  = "Ether is still finding a source for this title. Try again in a moment."
	// Setup problems: the owner can fix these, and "not available" would
	// send them looking for a missing release instead.
	msgNoSources        = "Ether has no sources to search: add a Stremio addon or Torznab indexer under Sources › Indexers."
	msgNoTorrentSources = "Ether streams through your debrid account but has no torrent indexer to search: add a Stremio addon or Torznab indexer under Sources › Indexers."
	msgDebridAuth       = "Your debrid service rejected Ether's API key: check the account under Sources › Debrid."
	msgNotCached        = "No cached release is available on your debrid service."
	msgDownloadQueued   = "Not cached yet. Sent to your debrid service to download; try again later."
	// Every release is a torrent no debrid account holds, with the owner's
	// "Cached torrents only" on: nothing downloads until it is turned off.
	msgNotCachedOnly       = "No cached copy. Turn off Cached torrents only to download it."
	msgNotCachedOnlyNoNZB  = "No cached copy or Usenet release. Turn off Cached torrents only to download it."
	msgNotCachedOnlyBadNZB = "No cached copy; Usenet releases are incomplete. Turn off Cached torrents only to download it."
	msgNotCachedNoNZB      = "No cached copy or Usenet release is available."
	msgNotCachedBadNZB     = "No cached copy; Usenet releases are incomplete."
	msgDownloadRefused     = "Not cached. The download could not be started; check the Ether log for the reason."
	msgDownloadPaused      = "Not cached. Ether temporarily paused downloads after debrid service refusals. Try again later."
	msgDownloadLimit       = "Not cached. Your debrid account has reached its active download limit."
)

func playbackMessage(err error) string {
	switch {
	case errors.Is(err, frontend.ErrUnreleased):
		return "This title has not been released yet. Playback is unavailable."
	case errors.Is(err, frontend.ErrDebridAuth):
		return msgDebridAuth
	case errors.Is(err, frontend.ErrNoTorrentSources):
		return msgNoTorrentSources
	case errors.Is(err, frontend.ErrNoSources):
		return msgNoSources
	case errors.Is(err, frontend.ErrDownloadQueued):
		return msgDownloadQueued
	case errors.As(err, new(*frontend.UncachedError)), onlyNotCached(err):
		return notCachedMessage(err)
	case errors.Is(err, context.DeadlineExceeded):
		return "The source timed out. Try again later."
	case errors.Is(err, frontend.ErrSourceUnavailable), errors.Is(err, frontend.ErrArticleMissing), errors.Is(err, frontend.ErrArticleCorrupt), errors.Is(err, frontend.ErrReleaseRejected):
		return "The source could not be opened or is incomplete. Try again later."
	case errors.Is(err, frontend.ErrNotAvailable):
		return msgNotAvailable
	case errors.Is(err, frontend.ErrStreamLimit):
		return msgStreamLimit
	case errors.Is(err, frontend.ErrPrepareSlow):
		return msgPrepareSlow
	case errors.Is(err, frontend.ErrBudgetExhausted):
		return "Indexers are temporarily at their limit. Ether will retry automatically; previously cached releases may still play."
	}
	return msgPlayFailed
}

// writeStartErr answers a failed start.m3u8, start.mpd or part request.
// End the media request and queue the reason as
// terminationText for the client's next timeline, as for a start stall.
func (s *Server) writeStartErr(w http.ResponseWriter, r *http.Request, pr *Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	s.log.Warn("playback start failed", "reason", playbackMessage(err), "err", err)
	if pr != nil && pr.User != nil {
		s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), playbackMessage(err))
	}
	// A completed attempt is terminal. Retry-After on a 503 makes media
	// loaders retry while waiting for a timeline they may never send.
	// PMS start.* documents 400/403/404 for unsuccessful starts.
	status := http.StatusBadRequest
	w.Header().Del("Retry-After")
	switch {
	case errors.Is(err, frontend.ErrNotFound), errors.Is(err, frontend.ErrNotAvailable):
		status = http.StatusNotFound
	case errors.Is(err, frontend.ErrUnauthorized):
		status = http.StatusForbidden
	}
	writePlaybackStatus(w, status)
}

// PMS does not specify an error body for start responses. Do not send HTML
// where the client expected a playlist, manifest, or media bytes.
func writePlaybackStatus(w http.ResponseWriter, status int) {
	w.Header().Del("Content-Type")
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// notCachedMessage words a failure whose every cause is frontend.ErrNotCached:
// the account's download limit or a refused download first (the owner can
// act on those), then what the resolver said about the owner's setting and
// Usenet (*frontend.UncachedError), the same reason Quality › History records.
func notCachedMessage(err error) string {
	switch {
	case errors.Is(err, frontend.ErrDownloadPaused):
		return msgDownloadPaused
	case errors.Is(err, frontend.ErrDownloadLimit):
		return msgDownloadLimit
	case errors.Is(err, frontend.ErrDownloadRefused):
		return msgDownloadRefused
	}
	var ue *frontend.UncachedError
	if !errors.As(err, &ue) {
		return msgNotCached
	}
	switch {
	case ue.CachedOnly && ue.Usenet == frontend.UsenetNone:
		return msgNotCachedOnlyNoNZB
	case ue.CachedOnly && ue.Usenet == frontend.UsenetFailed:
		return msgNotCachedOnlyBadNZB
	case ue.CachedOnly:
		return msgNotCachedOnly
	case ue.Usenet == frontend.UsenetNone:
		return msgNotCachedNoNZB
	case ue.Usenet == frontend.UsenetFailed:
		return msgNotCachedBadNZB
	}
	return msgNotCached
}

// onlyNotCached reports whether every cause in err is frontend.ErrNotCached
// (uncached, blocked or add-refused on the debrid accounts). The resolver
// joins one error per failed candidate, so "every" means each joined branch;
// errors.Is alone would match a single uncached torrent among Usenet
// failures. A multi-%w error is treated the same way, which errs towards
// the general message.
func onlyNotCached(err error) bool {
	for err != nil {
		if err == frontend.ErrNotCached {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			errs := x.Unwrap()
			if len(errs) == 0 {
				return false
			}
			for _, e := range errs {
				if !onlyNotCached(e) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		default:
			return false
		}
	}
	return false
}

// playRequest is a parsed decision/start request.
type playRequest struct {
	manualSubtitle bool
	key            frontend.RatingKey
	collection     string
	caps           frontend.ClientCaps
	opts           frontend.DecideOptions
	sessionID      string // client transcode session id
	offset         time.Duration
	protocol       string
	copyTimestamps bool
	audioSel       int                  // stream position, -1 = default
	audioStream    *frontend.StreamInfo // selected identity, remapped after probing
	subSel         int                  // stream position, -1 = none/default
	subSet         bool
	// subNamed: this request's subtitleStreamID named the subtitle (or
	// None). Otherwise the selection is the stored or preferred one, which
	// is burned only when the client was shown it selected (#313).
	// Response shaping, zeroed by sameSettings.
	subNamed bool
	// subStream is the subtitle picked by id (request or stored), to be
	// found again in the session's MediaInfo (resolvePlaySelection).
	subStream     *frontend.StreamInfo
	profileResult *deviceProfileResult
	// mediaIndex is the "Play Version" position the viewer picked
	// (0 = Auto / not sent); partReleaseID names the release directly
	// when the request addressed a variant's Part key.
	mediaIndex    int
	partReleaseID string
	// requestedPart is the Part id in the request path, when the client
	// addressed a Play Version part directly.
	requestedPart int64
	// mde: the client runs its own media decision engine (hasMDE=1, sent
	// by current Plex Web, iOS and Apple TV apps).
	mde bool
	// listAllTracks: Android-family clients receive every probed track in
	// the decision response; recorded PMS decisions for web, iOS and Apple
	// TV are selected-only, so those clients keep that shape. Set from the
	// request context in decisionContainer; zeroed by sameSettings because
	// it is response shaping, not a playback setting (an asymmetric compare
	// would re-prepare and restart the encoder on every start).
	listAllTracks bool
	// variantRelease is the release a Play Version pick resolved to ("" =
	// Auto) and mediaID the Media id the session was admitted on; both are
	// set by prepare and zeroed by sameSettings.
	variantRelease string
	mediaID        int64
	// subtitles is the client's subtitles= delivery mode (auto, burn,
	// none, sidecar, embedded, segmented). It never changes which track
	// is selected: PMS keeps the part's selected subtitle under every mode
	// and only marks how it is delivered (none: decision="ignore", the
	// player draws the track itself; sidecar: location="sidecar-subs").
	subtitles string
	// softText: the client is Plex's HTML player (Plex Web and the apps
	// built on it), which draws a text subtitle only from the ASS stream
	// it reads at /subtitles/:/transcode/universal/start (direct play) or
	// /video/:/transcode/universal/subtitles (conversion). Response
	// shaping, zeroed by sameSettings.
	softText bool
}

// androidFamilyClient reports whether the decision lists every probed track.
// Plex's Android family does; web, iOS and Apple TV stay selected-only.
// That is the PMS response shape, selected by the registry quirk
// plex-list-all-tracks rather than a playback capability.
func androidFamilyClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	return frontend.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, "plex-list-all-tracks")
}

// plexAndroidClient identifies the official Android phone and TV players.
// Plezy, Web and Apple players retain their existing contracts.
func plexAndroidClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	product := strings.ToLower(pr.Product)
	return product == "plex for android" || product == "plex for android (mobile)" ||
		(strings.HasPrefix(product, "plex for android") && (strings.Contains(product, "(tv)") || strings.Contains(strings.ToLower(pr.Device), "shield")))
}

// plexIOSClient identifies the official Plex iOS and iPadOS players.
func plexIOSClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	product := strings.ToLower(strings.TrimSpace(pr.Product))
	platform := strings.ToLower(strings.TrimSpace(pr.Platform))
	return product == "plex for ios" || (product == "plex" && (platform == "ios" || platform == "ipados"))
}

// plexMobileOrTVBedrockClient identifies the official Android and iOS players
// built on Plex's Bedrock player, which populate their subtitle picker from the
// pre-play detail/queue item and do not re-fetch metadata once playback starts.
func plexMobileOrTVBedrockClient(pr *Request) bool {
	return plexAndroidClient(pr) || plexIOSClient(pr)
}

// androidImageSubtitle is retained for native file playback, but must be burned
// once playback is converted: our segmented muxer carries text, not images.
func androidImageSubtitle(pr *Request, info frontend.MediaInfo, opts frontend.DecideOptions) bool {
	if !plexAndroidClient(pr) {
		return false
	}
	for _, st := range info.Streams {
		if st.Type == "subtitle" && !st.External && st.Index == opts.SubtitleStream {
			return frontend.IsImageSubtitle(st.Codec)
		}
	}
	return false
}

// matroskaCarriesText reports whether a conversion for this client is
// Matroska, whose segments carry the selected embedded text track. Plex for
// Android and Android TV send subtitles=none ("the player draws it") with
// the same request that ends in a Direct Stream: the player then reads the
// track from the Matroska segments as it would from the file, so the run
// has to mux it. Dropping it left the remux with no subtitle (#191).
func matroskaCarriesText(p *playRequest, info frontend.MediaInfo) bool {
	if p.protocol == frontend.ProtocolDASH {
		return false
	}
	if p.protocol != frontend.ProtocolHTTP && frontend.HLSContainer(p.caps, "", "") != frontend.ContainerMatroska {
		return false
	}
	for _, st := range info.Streams {
		if st.Type == "subtitle" && st.Index == p.opts.SubtitleStream {
			return !st.External && frontend.IsTextSubtitle(st.Codec)
		}
	}
	return false
}

// browserClient reports Plex Web. Its HTML player never reads a text track
// out of the file or the segments: it renders soft subtitles with libjass
// from a separate ASS stream, and asks for an ASS track to be burned
// (subtitles=burn) under its default "Automatic" burn setting.
func browserClient(pr *Request) bool {
	return pr != nil && strings.Contains(strings.ToLower(pr.Product), "plex web")
}

// sameSettings compares playback controls, excluding the seek position (DASH
// spans the title) and device capabilities (clients may omit their profile on
// subsequent starts). Quality limits from the query are also in opts. Keep the
// original options since prepare adds owner limits and track preferences.
func (p playRequest) sameSettings(other playRequest) bool {
	p.profileResult, other.profileResult = nil, nil
	p.offset, other.offset = 0, 0
	p.caps, other.caps = frontend.ClientCaps{}, frontend.ClientCaps{}
	p.listAllTracks, other.listAllTracks = false, false
	p.variantRelease, other.variantRelease = "", ""
	p.mediaID, other.mediaID = 0, 0
	p.softText, other.softText = false, false
	p.subNamed, other.subNamed = false, false
	// Only subtitles=none changes what the encoder delivers; the other
	// modes shape the response.
	if (p.subtitles == "none") == (other.subtitles == "none") {
		p.subtitles, other.subtitles = "", ""
	}
	return reflect.DeepEqual(p, other)
}

func (s *Server) parsePlayRequest(r *http.Request) (playRequest, error) {
	pr := FromContext(r.Context())
	if pr.ProfileName == "Raspberry PI" && resolvedPlayerProfile(pr) == "" {
		return playRequest{}, fmt.Errorf("profile name must identify a registered client profile")
	}
	if strings.Count(pr.ProfileExtra, "(") != strings.Count(pr.ProfileExtra, ")") {
		return playRequest{}, fmt.Errorf("malformed client profile directives")
	}
	q := r.URL.Query()
	key := keyFromPath(q.Get("path"))
	if key == 0 {
		key = keyFromPath(q.Get("key"))
	}
	if key == 0 {
		key = keyFromPath(q.Get("ratingKey"))
	}
	// A "Play Version" pick may address the chosen Part key directly.
	var partReleaseID string
	if key == 0 {
		for _, v := range []string{q.Get("path"), q.Get("key")} {
			if partID, ok := partIDFromPath(v); ok {
				if k, rel, found := s.partTitle(partID); found {
					key, partReleaseID = k, rel
					break
				}
			}
		}
	}
	if key == 0 || !isPlayableKey(key) {
		return playRequest{}, fmt.Errorf("no playable item in request")
	}
	caps := s.clientCapsForKey(pr, key)
	if s.dvBaseOnly(pr, key) {
		caps.DolbyVisionBaseOnly = true // this start stalled in Dolby Vision
	}
	if s.segmentedSurroundOff(pr) {
		caps.SegmentedSurroundOff = true // a stream stalled on copied surround audio
	}
	applyDecisionQuery(&caps, q)
	// The decision query can identify WAN even when the metadata request
	// looked local. Apply the remote ceiling before source resolution too.
	if limit := s.cfg().Transcode.RemoteMaxBitrateKbps; caps.Remote && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
		caps.MaxBitrateKbps = limit
	}
	p := playRequest{copyTimestamps: q.Get("copyts") == "1", key: key, caps: caps, audioSel: -1, subSel: -1, protocol: q.Get("protocol"), mde: q.Get("hasMDE") == "1", partReleaseID: partReleaseID}
	for _, raw := range []string{q.Get("path"), q.Get("key")} {
		if partID, ok := partIDFromPath(raw); ok {
			p.requestedPart = partID
			break
		}
	}
	for _, raw := range []string{q.Get("path"), q.Get("key")} {
		if u, err := url.Parse(raw); err == nil {
			if c := u.Query().Get("collection"); c != "" {
				p.collection = c
				break
			}
		}
	}
	// "Play Version" selection: position in the rendered Media list.
	// partIndex is always 0 (one Part per Media) and ignored.
	if n, err := strconv.Atoi(q.Get("mediaIndex")); err == nil && n > 0 {
		p.mediaIndex = n
	}
	if p.protocol == "" {
		if strings.HasSuffix(r.URL.Path, "/start") && !strings.HasSuffix(r.URL.Path, "/start.m3u8") && !strings.HasSuffix(r.URL.Path, "/start.mpd") {
			p.protocol = frontend.ProtocolHTTP
		} else {
			p.protocol = "hls"
		}
	}
	p.opts = frontend.DecideOptions{
		DirectPlay:     q.Get("directPlay") != "0",
		DirectStream:   q.Get("directStream") != "0",
		MaxBitrateKbps: caps.MaxBitrateKbps,
		MaxHeight:      parseResolutionHeight(q.Get("videoResolution")),
		AudioStream:    -1,
		SubtitleStream: -1,
		Protocol:       p.protocol,
	}
	if vb, err := strconv.Atoi(q.Get("videoBitrate")); err == nil && vb > 0 && (p.opts.MaxBitrateKbps == 0 || vb < p.opts.MaxBitrateKbps) {
		p.opts.MaxBitrateKbps = vb
	}
	p.subtitles = strings.ToLower(q.Get("subtitles"))
	if p.subtitles == "burn" {
		p.opts.BurnSubtitles = true
	}
	// Explicit stream ids in the request win over stored selections.
	var userID int64
	if pr.User != nil {
		userID = pr.User.ID
	}
	sel := s.selections.get(userID, int64(key))
	if sel.audioSet {
		p.audioSel, p.audioStream = sel.audio, sel.audioStream
	}
	if sel.subtitleSet {
		p.subSel, p.subSet, p.subStream = sel.subtitle, true, sel.subtitleStream
	}
	if id, err := strconv.ParseInt(q.Get("audioStreamID"), 10, 64); err == nil {
		binding, rerr := s.resolveIssuedStream(frontend.WithStreamServe(r.Context(), pr.Remote, ""), pr.User, pr.ClientID, id)
		if rerr != nil && !streamMiss(rerr) {
			return playRequest{}, rerr
		}
		if rerr == nil && binding.Key == key && binding.Info.Type == "audio" {
			info := binding.Info
			p.audioSel, p.audioStream = binding.Index, &info
		}
	}
	if v := q.Get("subtitleStreamID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if id == 0 {
				p.subSel, p.subSet, p.subStream, p.subNamed = -1, true, nil, true
			} else {
				binding, rerr := s.resolveIssuedStream(frontend.WithStreamServe(r.Context(), pr.Remote, ""), pr.User, pr.ClientID, id)
				if rerr != nil && !streamMiss(rerr) {
					return playRequest{}, rerr
				}
				if rerr == nil && binding.Key == key && binding.Info.Type == "subtitle" {
					info := binding.Info
					p.subSel, p.subSet, p.subStream, p.subNamed = binding.Index, true, &info, true
				}
			}
		}
	}
	p.manualSubtitle = p.subSet
	p.sessionID = cmp.Or(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID)
	if p.sessionID == "" {
		p.sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
	}
	p.sessionID = sanitizeSessionID(p.sessionID)
	if p.sessionID == "" {
		p.sessionID = newSessionID()
	}
	p.offset = durationParam(q.Get("offset"), time.Second)
	return p, nil
}

func isPlayableKey(k frontend.RatingKey) bool {
	switch k.Kind() {
	case frontend.KindMovie, frontend.KindEpisode, frontend.KindLocal:
		return true
	}
	return false
}

// sanitizeSessionID keeps session ids path-safe.
func newSessionID() string {
	return rand.Text()
}

func sanitizeSessionID(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < 128; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		}
	}
	return string(b)
}

// applySelections maps stream positions to DecideOptions stream indexes.
func applySelections(p *playRequest, info frontend.MediaInfo, langs []string) {
	audio := p.audioSel
	if audio < 0 || audio >= len(info.Streams) {
		audio = pickAudio(info, langs)
	}
	if audio >= 0 && audio < len(info.Streams) && info.Streams[audio].Type == "audio" {
		p.opts.AudioStream = info.Streams[audio].Index
	}
	if p.subSet && p.subSel >= 0 && p.subSel < len(info.Streams) && info.Streams[p.subSel].Type == "subtitle" {
		st := info.Streams[p.subSel]
		if !st.External {
			p.opts.SubtitleStream = st.Index
		}
	}
	p.audioSel = audio
}

// prepare resolves the title for this client and makes the decision.
func (s *Server) prepare(ctx context.Context, pr *Request, p *playRequest) (*frontend.Title, *frontend.PlaySession, frontend.Decision, error) {
	requested := *p
	if s.d.Playback == nil {
		return nil, nil, frontend.Decision{}, errors.New("playback not configured")
	}
	t, err := s.d.Catalog.Get(ctx, p.key)
	if err != nil || (t != nil && t.IsFutureRelease(s.now())) {
		if title, _, e := s.lookupPlaceholder(ctx, pr, p.key, p.collection); e == nil {
			return title, nil, frontend.Decision{}, frontend.ErrUnreleased
		}
		if err == nil {
			err = frontend.ErrNotFound
		}
	}
	if err != nil {
		return nil, nil, frontend.Decision{}, err
	}
	// Policy only: discovery evidence never blocks a play attempt.
	if !s.playableTitle(ctx, pr.User, t) {
		return nil, nil, frontend.Decision{}, frontend.ErrNotFound
	}
	if policy, ok := s.d.AccessPolicy.(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, p.caps.Remote, s.now()) {
		return nil, nil, frontend.Decision{}, frontend.ErrUnauthorized
	}
	if policy, ok := s.d.AccessPolicy.(frontend.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
			p.opts.MaxBitrateKbps = limit
		}
	}
	s.observeClientAdmission(pr, p.caps)
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		return t, nil, frontend.Decision{}, frontend.ErrUnauthorized
	}
	// The version list this client was issued stays pinned while the
	// session is admitted, so a page render racing a background refill
	// cannot move the Media the request's mediaIndex names.
	ik := s.playIdentityFor(pr, p.key)
	s.playIDs.beginAdmission(ik, s.now())
	var ps *frontend.PlaySession
	admitted := false
	defer func() {
		release := ""
		if admitted && ps.Resolution != nil {
			release = ps.Resolution.Release.ID
		}
		s.playIDs.endAdmission(ik, p.mediaID, release, admitted, s.now())
	}()
	dec, err := s.openPlexPlan(ctx, pr, p, func(ctx context.Context) (frontend.PlanAdmission, error) {
		var prepErr error
		ps, prepErr = s.preparePlayback(ctx, pr, p)
		if prepErr != nil {
			return frontend.PlanAdmission{}, prepErr
		}
		// Tracks cached or picked since the session's media was enriched (a
		// subtitle search pick mid-playback) join its streams for this
		// decision; text sidecars never enter the transcoder's input.
		if info := s.withCachedSidecars(ctx, pr.User, t, ps.Info); len(info.Streams) != len(ps.Info.Streams) {
			cp := *ps
			cp.Info = info
			ps = &cp
		}
		// The chosen sidecar has to be on this media before track resolution.
		// Otherwise a language already present in the probe drops it, and the
		// decision either selects the other track or no subtitle at all.
		ps = ensureSelectedSidecar(ps, p.subStream)
		resolvePlaySelection(p, ps.Info)
		s.applyStreamPreferences(ctx, pr, p, t, ps.Info)
		applySelections(p, ps.Info, s.audioLanguages(pr.User))
		s.selectLegacyDecoderAudio(pr, p, ps.Info)
		switch {
		case p.subtitles == "none" && !matroskaCarriesText(p, ps.Info) && !androidImageSubtitle(pr, ps.Info, p.opts):
			// The track stays selected; nothing delivers it.
			p.opts.SubtitleStream = -1
		case p.opts.BurnSubtitles && (browserClient(pr) || plexAndroidClient(pr)):
			// These players explicitly request a burn when their selected
			// rendering mode will not draw a soft text track.
			p.opts.BurnTextSubtitles = true
		}
		if androidImageSubtitle(pr, ps.Info, p.opts) {
			// Source-change remuxing is also a conversion, even when MDE
			// initially allowed direct play of the original file.
			converting := !p.opts.DirectPlay || ps.RequiresRemux
			if onDemandSession(ps) && p.mde && p.opts.DirectStream && ps.Resolution != nil {
				protocol := ps.Resolution.Release.Protocol
				converting = converting || (protocol != frontend.ProtocolHTTP && protocol != frontend.ProtocolTorrent)
			}
			// A quality/codec limit can require conversion even though the
			// client allowed direct play.
			if converting || frontend.DecideTranscode(ps.Info, p.caps, p.opts, frontend.TranscodePolicy{}).Kind != frontend.DecisionDirectPlay {
				p.opts.BurnSubtitles = true
			}
		}
		s.burnOnlyShownSubtitle(pr, p, ps.Info)
		p.caps.TranscodeVideoCodecs = streamingVideoTarget(pr.ProfileExtra, p.protocol)
		if len(p.caps.VideoCodecs) > 0 && len(p.caps.TranscodeVideoCodecs) > 0 {
			var kept []string
			for _, c := range p.caps.TranscodeVideoCodecs {
				if p.caps.SupportsVideo(c) {
					kept = append(kept, c)
				}
			}
			p.caps.TranscodeVideoCodecs = kept
		}
		p.caps.TranscodeAudioCodecs = streamingAudioTarget(pr.ProfileExtra, p.protocol)
		if len(p.caps.AudioCodecs) > 0 && len(p.caps.TranscodeAudioCodecs) > 0 {
			var kept []string
			for _, c := range p.caps.TranscodeAudioCodecs {
				if p.caps.SupportsAudio(c) {
					kept = append(kept, c)
				}
			}
			p.caps.TranscodeAudioCodecs = kept
		}
		if err := s.applyDeviceProfile(pr, t, ps, p); err != nil {
			return frontend.PlanAdmission{}, err
		}
		return frontend.PlanAdmission{
			Source: ps, Media: ps.Info, Caps: p.caps, Options: p.opts, PreferRemux: p.mde, Protocol: p.protocol,
			RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps,
		}, nil
	})
	if err != nil {
		return t, nil, frontend.Decision{}, err
	}
	p.mediaID = s.admittedMedia(p)
	admitted = true
	if v := ps.Info.Video(); v != nil {
		s.log.Debug("playback media decision", "key", p.key, "client", pr.ClientID, "device", pr.Device, "model", pr.Model, "platformVersion", pr.PlatformVersion, "video", v.Codec, "videoProfile", v.Profile, "height", v.Height, "bitDepth", v.BitDepth, "transfer", v.ColorTransfer, "dvProfile", v.DolbyVisionProfile, "dvLevel", v.DolbyVisionLevel, "dvCompatibility", v.DolbyVisionCompatibilityID, "kind", dec.Kind, "outputContainer", dec.Container, "videoCopy", dec.VideoCopy, "audioCopy", dec.AudioCopy, "hdrCap", p.caps.HDR10, "dvCap", p.caps.DolbyVision, "profileExtra", pr.ProfileExtra)
	}
	subtitle := -1
	if p.subSet && p.subSel >= 0 && p.subSel < len(ps.Info.Streams) && ps.Info.Streams[p.subSel].Type == "subtitle" {
		subtitle = p.subSel
	}
	s.observePlaybackDecision(ctx, pr, p, ps)
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	s.transcodes.put(&transcodeEntry{
		id: p.sessionID, playID: ps.ID, playbackID: pr.SessionID, userID: pr.User.ID, clientID: pr.ClientID, key: p.key,
		requested: requested,
		decision:  dec, info: ps.Info, subtitle: subtitle, caps: p.caps, opts: p.opts, offset: p.offset, updatedAt: s.now(),
	})
	return t, ps, dec, nil
}

func onDemandSession(ps *frontend.PlaySession) bool {
	return ps.Resolution != nil && !strings.HasPrefix(ps.Resolution.Release.ID, "local:")
}

// handleDecision is /video/:/transcode/universal/decision.
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if arm, err := s.parsePlayRequest(r); err == nil {
		s.armDecision(pr, arm.key)
	}
	// A slow source search must not hold the owner gates (timeline and stop
	// share them) or outlast the client's request timeout.
	if t, err := s.awaitSource(ctx, r, pr); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("playback decision failed before admission", "user", pr.User.Username, "reason", playbackMessage(err), "err", err)
		p, _ := s.parsePlayRequest(r)
		// start.m3u8/start.mpd follow a failed decision for the same session:
		// answer them with this error rather than a second full resolve.
		s.transcodes.fail(p.sessionID, pr.User.ID, pr.ClientID, p.key, err, s.now())
		s.write(w, r, s.decisionError(ctx, pr, t, err, p))
		return
	}
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if chi.URLParam(r, "ttype") != "" && chi.URLParam(r, "ttype") != "video" {
		s.writeEmpty(w, r)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	t, ps, dec, err := s.prepare(ctx, pr, &p)
	if err != nil {
		var profileErr *profileDecisionError
		if errors.As(err, &profileErr) && profileErr.code == 0 {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if errors.Is(err, errDecisionMediaIndex) {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if errors.Is(err, frontend.ErrNotFound) && t == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("playback decision failed", "key", p.key, "user", pr.User.Username, "reason", playbackMessage(err), "err", err)
		// The client follows a failed decision with start.mpd/start.m3u8
		// for the same session: answer it with this error, not a second
		// full resolve that fails the same way.
		s.transcodes.fail(p.sessionID, pr.User.ID, pr.ClientID, p.key, err, s.now())
		s.write(w, r, s.decisionError(ctx, pr, t, err, p))
		return
	}
	s.write(w, r, s.decisionContainer(ctx, pr, t, ps, dec, p))
}

// decisionSourceWait bounds how long a decision waits for a source search
// before answering; Plex apps give up on a decision after about 30 s.
const decisionSourceWait = 12 * time.Second

// awaitSource waits, outside the owner gates, for the source an automatic
// decision will use. The search is the playback manager's shared flight:
// the gated admission that follows finds the prepared session at once.
// When the search outlasts decisionSourceWait it keeps running for the
// client's retry and the decision answers frontend.ErrPrepareSlow. Explicit
// version picks and requests this cannot pre-check go straight on.
func (s *Server) awaitSource(ctx context.Context, r *http.Request, pr *Request) (*frontend.Title, error) {
	pw, ok := s.d.Playback.(frontend.PlaybackPrepareWithin)
	if !ok || pr == nil || pr.User == nil || s.d.Catalog == nil {
		return nil, nil
	}
	if tt := chi.URLParam(r, "ttype"); tt != "" && tt != "video" {
		return nil, nil
	}
	p, err := s.parsePlayRequest(r)
	if err != nil || p.key == 0 || p.partReleaseID != "" || p.mediaIndex > 0 || !isPlayableKey(p.key) {
		return nil, nil
	}
	t, err := s.d.Catalog.Get(ctx, p.key)
	if err != nil || t == nil || !s.playableTitle(ctx, pr.User, t) {
		return nil, nil
	}
	if policy, ok := s.d.AccessPolicy.(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, p.caps.Remote, s.now()) {
		return nil, nil
	}
	if policy, ok := s.d.AccessPolicy.(frontend.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
		}
	}
	if p.caps.ClientID == "" {
		p.caps.ClientID = pr.ClientID
	}
	// A slow search answers "still finding a source" and keeps running; any
	// other failure is answered now, so the gated path does not repeat the
	// whole search (the flight does not remember failures).
	if _, err := pw.PrepareWithin(ctx, decisionSourceWait, pr.User, p.key, p.caps, "", false); err != nil {
		return t, err
	}
	return nil, nil
}

// decisionError is the decision response for a failed resolve: no Media,
// and the reason in every *DecisionText so whichever field a client shows
// carries the message instead of a spinner.
func (s *Server) decisionError(ctx context.Context, pr *Request, t *frontend.Title, err error, p playRequest) *model.MediaContainer {
	var profileErr *profileDecisionError
	if errors.As(err, &profileErr) {
		return profileErrorContainer(pr, p, profileErr)
	}
	msg := playbackMessage(err)
	s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), msg)
	mc := libraryContainer()
	if t != nil {
		mc = s.itemContainer(t.Type)
		md := s.metadata(ctx, pr, t, renderOpts{detail: true})
		decisionItem(&md) // shaped and typed like a successful decision's item
		mc.Metadata = []model.Metadata{md}
	}
	mc.Extra = []model.Attr{
		model.A("directPlayDecisionCode", codeDirectPlayError),
		model.A("directPlayDecisionText", msg),
		model.A("generalDecisionCode", codeGeneralError),
		model.A("generalDecisionText", msg),
		model.A("transcodeDecisionCode", codeTranscodeError),
		model.A("transcodeDecisionText", msg),
	}
	if p.mde {
		// MDE clients read mdeDecision* too (Plex Web shows the first
		// *DecisionText whose code is >= 2000).
		mc.Extra = append(mc.Extra, model.A("mdeDecisionCode", codeGeneralError), model.A("mdeDecisionText", msg))
	}
	mc.Extra = append(mc.Extra, model.A("resourceSession", cmp.Or(pr.SessionID, pr.ClientID)))
	mc.Size = len(mc.Metadata)
	mc.SetDecision()
	return mc
}

// decisionItem reshapes a detail item the way PMS's decision responses
// carry it: attributes alphabetical with string-typed ids, no Image or Guid
// children, tag attributes alphabetical with string ids, and
// UltraBlurColors as an array in JSON.
func decisionItem(md *model.Metadata) {
	md.SetAlphabetical()
	md.Image = nil
	md.Guid = nil
	if md.UltraBlurColors != nil {
		u := *md.UltraBlurColors
		u.SetAlphabetical()
		md.UltraBlurList = []model.UltraBlurColors{u}
		md.UltraBlurColors = nil
	}
	for i := range md.Rating_ {
		md.Rating_[i].SetAlphabetical()
	}
	for _, tags := range [][]model.Tag{md.Genre, md.Country, md.Collection, md.Director, md.Writer, md.Role, md.Producer, md.Label, md.Similar} {
		for i := range tags {
			tags[i].SetAlphabetical()
		}
	}
}

func (s *Server) decisionContainer(ctx context.Context, pr *Request, t *frontend.Title, ps *frontend.PlaySession, dec frontend.Decision, p playRequest) *model.MediaContainer {
	mc := s.itemContainer(t.Type)
	md := s.metadata(ctx, pr, t, renderOpts{detail: true})
	decisionItem(&md)
	// With hasMDE=1 and direct play allowed, the client's own decision
	// engine decides: PMS answers with the source media as direct play and
	// only an mdeDecisionCode. Ether's own decision is kept for start.m3u8
	// in case the client then asks for a stream.
	//
	// An on-demand title's metadata is an estimate until it is probed, so
	// the client's engine may have judged it playable from streams it
	// does not have (an AC3 track estimated as AAC for a browser). When
	// Ether's own decision must convert a stream for this client, say so
	// instead of "Direct play OK", whatever directStream says (#246).
	mustConvert := dec.Kind == frontend.DecisionTranscode || (dec.Kind != frontend.DecisionDirectPlay && (!dec.VideoCopy || !dec.AudioCopy))
	pm4k := strings.EqualFold(pr.Product, "PM4K") || strings.EqualFold(pr.Product, "Plex for Kodi")
	// The owner's "Convert Dolby and DTS to AAC" rule outranks the client's
	// engine: a surround track it converted is never answered direct play.
	ownerConverts := p.caps.ConvertSurroundAudio && dec.Kind != frontend.DecisionDirectPlay && !dec.AudioCopy
	mdeDirect := p.mde && p.opts.DirectPlay && !dec.StripDolbyVision && !(plexAndroidClient(pr) && dec.BurnSubtitles) && (!pm4k || dec.Kind == frontend.DecisionDirectPlay) && len(p.opts.AudioFilters) == 0 && !ownerConverts &&
		!(onDemandSession(ps) && (mustConvert || (p.opts.DirectStream && dec.Kind != frontend.DecisionDirectPlay)))
	shown := dec
	if mdeDirect {
		shown = frontend.Decision{Kind: frontend.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: ps.Info.Container}
	}
	p.listAllTracks = androidFamilyClient(pr)
	p.softText = browserClient(pr)
	if plexAndroidClient(pr) && shown.BurnSubtitles && p.subtitles == "none" {
		p.subtitles = "burn"
	}
	md.Media = []model.Media{s.decisionMedia(t, ps, shown, p, pr.User)}
	s.addProbedStreamFeatures(ctx, t.Key, ps.Info, &md)
	if md.Media[0].Duration > 0 {
		md.Duration = md.Media[0].Duration
	}
	mc.Metadata = []model.Metadata{md}
	mc.Size = 1
	mc.SetDecision()
	switch {
	case mdeDirect:
		mc.Extra = []model.Attr{
			model.A("mdeDecisionCode", codeDirectPlayOK),
			model.A("mdeDecisionText", textDirectPlayOK),
		}
	case dec.Kind == frontend.DecisionDirectPlay:
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayOK),
			model.A("directPlayDecisionText", textDirectPlayOK),
			model.A("generalDecisionCode", codeDirectPlayOK),
			model.A("generalDecisionText", textDirectPlayOK),
		}
	default:
		dpText := textDirectPlayDisable
		if p.opts.DirectPlay {
			dpText = "App cannot direct play this item."
			if dec.Reason != "" {
				dpText += " " + dec.Reason
			}
		}
		if p.profileResult != nil && p.profileResult.reason != "" {
			dpText = p.profileResult.reason
		}
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError),
			model.A("directPlayDecisionText", dpText),
			model.A("generalDecisionCode", codeConversionOK),
			model.A("generalDecisionText", textConversionOK),
			model.A("transcodeDecisionCode", codeConversionOK),
			model.A("transcodeDecisionText", textConversionOK),
		}
	}
	if pm4k && mdeDirect {
		mc.Extra = append(mc.Extra, model.A("directPlayDecisionCode", codeDirectPlayOK), model.A("directPlayDecisionText", textDirectPlayOK), model.A("generalDecisionCode", codeDirectPlayOK), model.A("generalDecisionText", textDirectPlayOK))
	}
	if pm4k && p.mde && !mdeDirect {
		code, text := codeConversionOK, textConversionOK
		if dec.Kind == frontend.DecisionDirectPlay {
			code, text = codeDirectPlayOK, textDirectPlayOK
		}
		mc.Extra = append(mc.Extra, model.A("mdeDecisionCode", code), model.A("mdeDecisionText", text))
	}
	// PMS echoes the client identifier as the resource session.
	mc.Extra = append(mc.Extra, model.A("resourceSession", cmp.Or(pr.SessionID, pr.ClientID)))
	return mc
}

// decisionMedia renders the Media with decision attributes.
func (s *Server) decisionMedia(t *frontend.Title, ps *frontend.PlaySession, dec frontend.Decision, p playRequest, u *frontend.User) model.Media {
	stamp := ps.StartedAt.Unix()
	if stamp <= 0 {
		stamp = 1
	}
	info := ps.Info
	m := buildMedia(t, info, stamp)
	// The decision names the Media the session was admitted on, with the
	// Part key and stream ids that Media was issued (play_identity.go).
	var ik playIdentityKey
	if p.mediaID != 0 && u != nil {
		ik = playIdentityKey{userID: u.ID, clientID: p.caps.ClientID, key: t.Key}
		if p.mediaID != int64(t.Key) {
			m.ID = p.mediaID
			m.Part[0].ID = p.mediaID
			m.Part[0].Key = fmt.Sprintf("/library/parts/%d/%d/file.%s", p.mediaID, stamp, containerExt(m.Container))
		}
		m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, s.now())
	}
	if ps.Resolution != nil && s.d.Trickplay != nil && s.d.Trickplay.TrickplayAvailable(frontend.TrickplayCacheKey(ps)) {
		m.Part[0].Indexes = "sd"
	}
	m.Selected = model.Ptr(true)
	m.SetAlphabetical()
	part := &m.Part[0]
	part.Selected = model.Ptr(true)
	part.SetAlphabetical()
	sel := selection{audio: p.audioSel, audioSet: p.audioSel >= 0, subtitle: p.subSel, subtitleSet: p.subSet}
	streams := s.issuedStreams(t.Key, ik, m.ID, info, sel, u)
	for i := range streams {
		streams[i].SetAlphabetical()
		if streams[i].StreamType == 1 {
			// Decisions shorten the video title to the resolution part
			// ("1080p"); the extended title keeps the codec.
			if short, _, ok := strings.Cut(streams[i].DisplayTitle, " ("); ok {
				streams[i].DisplayTitle = short
			}
		}
	}

	// Only the first video elementary stream is played. A separate Dolby
	// Vision enhancement stream remains probe metadata, not a second player track.
	videoSeen := false
	playableStreams := streams[:0]
	for _, st := range streams {
		if st.StreamType == 1 {
			if videoSeen {
				continue
			}
			videoSeen = true
		}
		playableStreams = append(playableStreams, st)
	}
	streams = playableStreams
	selectedOnly := info.Estimated || !p.listAllTracks
	if dec.Kind == frontend.DecisionDirectPlay {
		return directPlayMedia(m, streams, selectedOnly, p.subtitles)
	}
	applyTranscodeMedia(&m, dec, p.protocol)
	var kept []model.Stream
	for _, st := range streams {
		binding, rerr := s.resolveIssuedStream(context.Background(), u, p.caps.ClientID, st.ID)
		if rerr != nil && !streamMiss(rerr) {
			continue
		}
		if shaped, keep := applyStreamDecision(st, binding.Info, dec, selectedOnly, p.subtitles, p.softText); keep {
			if plexAndroidClient(&Request{Product: p.caps.Product, Device: p.caps.Device}) && shaped.Decision == "burn" && frontend.IsImageSubtitle(binding.Info.Codec) && p.protocol == frontend.ProtocolHLS {
				shaped.Location = "segments-av"
				shaped.Extra = append(shaped.Extra, model.A("burn", "1"))
			}
			kept = append(kept, shaped)
		}
	}
	part.Stream = kept
	if p.protocol == frontend.ProtocolDASH {
		dashDecisionMedia(&m)
	}
	if p.protocol == frontend.ProtocolHTTP {
		for i := range kept {
			if kept[i].Location == "segments-av" || kept[i].Location == "segments-video" {
				kept[i].Location = "direct"
			}
		}
	}
	return m
}

// directPlayMedia preserves stream ordering. For Android-family clients
// every audio and subtitle stays in the list and selected marks the one
// that will play: Plex Android builds its playback track menu from this
// response, so dropping the alternates leaves the menu empty. Other
// clients keep the recorded PMS shape (video + selected tracks), which
// TestDecisionConformsToPMS and the goldens pin.
//
// The selected subtitle stays selected whatever subtitles= says, and its
// row names the delivery as PMS 1.42 does for a direct play: subtitles=none
// is decision="ignore" with no location (the player draws the track from
// the file itself; Plex for Android and Android TV send it), sidecar is
// location="sidecar-subs" (Plex Web, which then reads the cues from
// /subtitles/:/transcode/universal/start). An image subtitle only reaches
// direct play when the client can draw it; otherwise Decide burns it and
// this function is not used.
func directPlayMedia(m model.Media, streams []model.Stream, selectedOnly bool, subtitles string) model.Media {
	part := &m.Part[0]
	part.Decision = "directplay"
	var kept []model.Stream
	for _, st := range streams {
		if selectedOnly && st.StreamType != 1 && (st.Selected == nil || !*st.Selected) {
			continue
		}
		st.Location = "direct"
		if st.StreamType == 3 && st.Selected != nil && *st.Selected {
			switch subtitles {
			case "none":
				st.Decision, st.Location = "ignore", ""
			case "sidecar":
				st.Location = "sidecar-subs"
			}
		}
		kept = append(kept, st)
	}
	part.Stream = kept
	return m
}

func textSidecarCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "srt", "subrip", "ass", "ssa", "vtt", "webvtt", "mov_text", "text", "microdvd", "subviewer", "sami", "ttml", "stl":
		return true
	default:
		return false
	}
}

// applyTranscodeMedia describes the output container and media dimensions.
func applyTranscodeMedia(m *model.Media, dec frontend.Decision, protocol string) {
	part := &m.Part[0]
	// Describe the actual HLS segment container so native MDE does not see
	// HEVC paired with the MPEG-TS container it cannot use.
	m.SetTranscodeOrder()
	part.SetTranscodeOrder()
	m.AspectRatio = 0
	m.Protocol = protocol
	m.Container = "mpegts"
	if dec.Container == frontend.ContainerFMP4 {
		m.Container = "mp4"
	} else if dec.Container == frontend.ContainerMatroska {
		m.Container = "mkv"
	}
	part.Container = m.Container
	part.Protocol = protocol
	if protocol == frontend.ProtocolHTTP {
		// Plex Desktop's Matroska stream: Media/Part carry container=mkv
		// and no protocol attribute.
		m.Protocol = ""
		part.Protocol = ""
		m.Container = "mkv"
		part.Container = "mkv"
	}
	part.Decision = "transcode"
	part.Key = ""
	part.File = ""
	part.Size = 0
	m.OptimizedForStreaming, m.Has64bitOffsets = nil, nil
	part.OptimizedForStreaming, part.Has64bitOffsets = nil, nil
	if r := m.VideoResolution; r != "" && r != "4k" && r != "sd" {
		m.VideoResolution = r + "p"
	}
	if !dec.VideoCopy {
		if dec.VideoCodec != "" {
			m.VideoCodec = plexCodec(dec.VideoCodec)
		}
		if dec.MaxHeight > 0 && m.Height > dec.MaxHeight {
			if m.Height > 0 {
				m.Width = m.Width * dec.MaxHeight / m.Height
			}
			m.Height = dec.MaxHeight
			if res := videoResolution(m.Width, m.Height); res != "" && res != "4k" && res != "sd" {
				m.VideoResolution = res + "p"
			} else {
				m.VideoResolution = res
			}
		}
		if dec.BitrateKbps > 0 {
			m.Bitrate = dec.BitrateKbps
		}
	}
	if !dec.AudioCopy && dec.AudioCodec != "" {
		m.AudioCodec = plexCodec(dec.AudioCodec)
	}
	part.Bitrate, part.Width, part.Height = m.Bitrate, m.Width, m.Height
}

// applyStreamDecision shapes one already-resolved stream without registry
// I/O. When selectedOnly is false (Android-family decisions), an unselected
// stream stays in the list bare: it keeps its index and gets no decision or
// location, the same shape its streams carry in library metadata. That is
// the shape under review against a real PMS Android capture; the selected
// stream's shape never changes.
//
// The selected subtitle row follows PMS 1.42 (recorded PMS 1.42.2 capture):
// subtitles=none is decision="ignore" with nothing delivered (except a
// text track a Matroska run carries, which keeps the copy row); a burned text
// track is decision="burn" burn="1" location="segments-av"; Plex Web's text
// track is converted to ASS (codec/container/format "ass",
// decision="transcode", location="segments-subs") and read from
// /video/:/transcode/universal/subtitles; a text track copied into
// Matroska segments keeps its format.
func applyStreamDecision(st model.Stream, src frontend.StreamInfo, dec frontend.Decision, selectedOnly bool, subtitles string, softText bool) (model.Stream, bool) {
	idx := st.Index
	st.Index = nil // segments carry their own stream layout
	switch st.StreamType {
	case 1:
		st.Decision = "copy"
		if dec.StripDolbyVision {
			st.DOVIPresent, st.DOVIProfile, st.DOVILevel, st.DOVIVersion = nil, 0, 0, ""
			st.DOVIBLCompatID, st.DOVIBLPresent, st.DOVIELPresent, st.DOVIRPUPresent = nil, nil, nil, nil
			base := src
			base.DolbyVision = false
			st.DisplayTitle = videoDisplayTitle(base)
			st.ExtendedDisplayTitle = st.DisplayTitle
		}
		if !dec.VideoCopy {
			// A transcoded stream is new: PMS drops the source's
			// profile and chroma details and reports 8-bit output.
			st.Decision = "transcode"
			if dec.VideoCodec != "" {
				st.Codec = plexCodec(dec.VideoCodec)
			}
			if dec.MaxHeight > 0 && st.Height > dec.MaxHeight {
				st.Width = st.Width * dec.MaxHeight / max(st.Height, 1)
				st.Height = dec.MaxHeight
			}
			if dec.BitrateKbps > 0 {
				st.Bitrate = dec.BitrateKbps
			}
			st.Profile, st.ChromaSubsampling, st.ScanType = "", "", ""
			st.DOVIPresent, st.DOVIProfile = nil, 0
			st.DOVIBLCompatID, st.DOVIBLPresent, st.DOVIELPresent, st.DOVIRPUPresent = nil, nil, nil, nil
			st.DOVILevel, st.DOVIVersion, st.Level = 0, "", 0
			if st.Codec == "h264" || dec.ToneMap {
				st.BitDepth = 8
			}
			if dec.ToneMap {
				st.ColorTrc, st.ColorPrimaries, st.ColorSpace = "bt709", "", ""
			}
		}
		st.Location = "segments-av"
	case 2:
		if st.Selected == nil || !*st.Selected {
			if selectedOnly {
				return st, false
			}
			st.Index = idx
			return st, true
		}
		st.Decision = "copy"
		if !dec.AudioCopy {
			st.Decision = "transcode"
			if dec.AudioCodec != "" {
				st.Codec = plexCodec(dec.AudioCodec)
			}
			// Source-only details go, as in PMS's transcoded audio.
			st.Profile, st.AudioChannelLayout, st.SamplingRate, st.Title = "", "", 0, ""
		}
		st.Location = "segments-av"
	case 3:
		if st.Selected == nil || !*st.Selected {
			if selectedOnly {
				return st, false
			}
			st.Index = idx
			return st, true
		}
		text := textSidecarCodec(st.Codec)
		// Under subtitles=none a Matroska run still muxes the selected
		// text track (matroskaCarriesText); its row is the segments copy
		// below, as PMS answers a text track it copies into Plex for
		// Android's Matroska segments.
		inSegments := text && dec.Container == frontend.ContainerMatroska && !dec.BurnSubtitles && dec.SubtitleStream >= 0 && dec.SubtitleStream == src.Index
		switch {
		case subtitles == "none" && !inSegments:
			st.Decision, st.Location = "ignore", ""
			return st, true
		case dec.BurnSubtitles && text:
			st.Decision = "burn"
			st.Location = "segments-av"
			st.Format = ""
			st.Extra = append(st.Extra, model.A("burn", "1"))
			return st, true
		case dec.BurnSubtitles:
			st.Decision = "burn"
			st.Location = "segments-video"
		case softText && text && dec.Container != frontend.ContainerMatroska:
			st.Codec, st.Format = "ass", "ass"
			st.Decision = "transcode"
			st.Location = "segments-subs"
			st.Extra = append(st.Extra, model.A("container", "ass"))
			return st, true
		case src.External:
			st.Decision = "copy"
			st.Location = "sidecar"
		default:
			st.Decision = "copy"
			st.Location = "sidecar"
			if st.Key == "" {
				st.Key = "/library/streams/" + strconv.FormatInt(st.ID, 10)
			}
			if st.Format == "" {
				st.Format = st.Codec
			}
		}
		// Plex tvOS reads the text track from the Matroska segments and does
		// not request a sidecar. The transcoder muxes that track on the
		// same clock as the video; a sidecar key would describe a file
		// this player never opens.
		if st.Location == "sidecar" && dec.Container == frontend.ContainerMatroska && textSidecarCodec(st.Codec) && !dec.BurnSubtitles {
			st.Decision = "copy"
			st.Location = "segments-av"
			st.Key = ""
			st.Format = st.Codec
		}
	}
	return st, true
}

// handleStartHLS is /video/:/transcode/universal/start.m3u8.
func (s *Server) handleStartHLS(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if s.d.Transcoder == nil || s.d.Playback == nil {
		writePlaybackStatus(w, http.StatusServiceUnavailable)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	if !s.decisionArmed(pr, p.key) {
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writePlaybackStatus(w, http.StatusGone)
		return
	}

	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writePlaybackStatus(w, http.StatusForbidden)
		return
	}
	// A slow decision leaves acquisition running. Join that shared flight
	// below; only a completed source failure should short-circuit start.
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed && !errors.Is(err, frontend.ErrPrepareSlow) {
		s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
		s.writeStartErr(w, r, pr, err)
		return
	}
	entry, ok := s.transcodes.get(p.sessionID)
	var ps *frontend.PlaySession
	if ok && entry.key == p.key && entry.userID == pr.User.ID {
		ps, ok = s.d.Playback.Session(entry.playID)
	}
	if !ok || ps == nil || !p.sameSettings(entry.requested) {
		// A client can change quality or tracks in start itself, without a
		// preceding decision request. Reapply the normal policy and decision
		// path before deciding whether a running encoder can be reused.
		if _, ps, _, err = s.prepare(ctx, pr, &p); err != nil {
			if ctx.Err() != nil {
				return // the player dropped this request; its preparation carries on
			}
			s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
			s.writeStartErr(w, r, pr, err)
			return
		}
		entry, _ = s.transcodes.get(p.sessionID)
	} else if dec, err := s.publishBrokerPlayback(ctx, pr, replayPlay(p, entry), ps); err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	} else {
		entry.decision = dec
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	entry.playbackID = pr.SessionID
	s.transcodes.put(entry)

	dec := frontend.HLSDecision(entry.decision, p.opts.DirectPlay, func() frontend.Decision {
		resolvePlaySelection(&p, ps.Info)
		applySelections(&p, ps.Info, s.audioLanguages(pr.User))
		return s.d.Transcoder.Decide(ps.Info, p.caps, p.opts)
	}, func(dec frontend.Decision) frontend.Decision {
		// A client may request HLS after accepting an MDE direct-play
		// decision (notably stable HTTP/debrid files). HLSDecision's legacy
		// fallback is MPEG-TS, which cannot carry the selected text track.
		// Use the Android player's negotiated target even on this late handoff.
		if plexAndroidClient(pr) {
			dec.Container = frontend.HLSContainer(entry.caps, dec.VideoCodec, dec.AudioCodec)
			if androidImageSubtitle(pr, ps.Info, entry.opts) {
				opts := entry.opts
				opts.DirectPlay, opts.BurnSubtitles = false, true
				return s.d.Transcoder.Decide(ps.Info, entry.caps, opts)
			}
		}
		videoOK := dec.VideoCodec == "" || frontend.SegmentedVideoCopyOK(entry.caps, dec.VideoCodec)
		audioOK := dec.AudioCodec == "" || frontend.SegmentedAudioCopyOK(entry.caps, dec.AudioCodec)
		if !videoOK || !audioOK {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = videoOK
			return s.d.Transcoder.Decide(ps.Info, entry.caps, opts)
		}
		return dec
	})
	if existing, ok := s.plexHLSSession(p.sessionID); ok && existing != nil {
		if entry.started && entry.offset == p.offset {
			s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
			unlockEncoder()
			unlockOwner()
			s.writeMasterPlaylist(w, r, p.sessionID, existing, ps.Info, dec)
			return
		}
		s.stopPlexHLS(p.sessionID)
	}
	hs, err := s.startPlexHLS(context.WithoutCancel(ctx), pr.User, ps.ID, p.sessionID, frontend.HLSRequest{
		SoftSubtitleStream: sessionSoftSubtitle(entry, pr, dec),
		SessionID:          p.sessionID,
		ReleaseID:          ps.SourceReleaseID(),
		InputURL:           s.d.Playback.InputURL(ps.ID),
		Info:               ps.Info,
		Decision:           dec,
		StartOffset:        p.offset,
	})
	if err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	}
	s.bindTranscodeStarted(entry, dec, p.offset)
	s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	s.writeMasterPlaylist(w, r, p.sessionID, hs, ps.Info, dec)
}

// singleRungHLSClient reports smart-TV HTML apps (Plex for LG / webOS and
// Plex for VIDAA) whose native HLS demuxer wedges in a seek loop if ABR
// switches across shared-encoder TS resolutions mid-stream (#408). Explicit
// Plex quality-menu changes still renegotiate start.m3u8.
func singleRungHLSClient(r *http.Request, entry *transcodeEntry) bool {
	product, platform := "", ""
	if pr := FromContext(r.Context()); pr != nil {
		product, platform = pr.Product, pr.Platform
	}
	if entry != nil {
		if product == "" {
			product = entry.caps.Product
		}
		if platform == "" {
			platform = entry.caps.Platform
		}
	}
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "plex for lg", "plex for vidaa":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "webos", "vidaa":
		return true
	}
	return false
}

func (s *Server) writeMasterPlaylist(w http.ResponseWriter, r *http.Request, sessionID string, hs frontend.HLSSession, info frontend.MediaInfo, dec frontend.Decision) {
	if err := s.saveDistributedBinding(r.Context(), sessionID); err != nil {
		http.Error(w, "playback unavailable", 503)
		return
	}
	entry, _ := s.transcodes.get(sessionID)
	// A ladder-capable session advertises every rung; the client's native
	// ABR (AVPlayer, hls.js) steps between them and the session starts a
	// rung's encoder lazily on its first segment request. The primary
	// rung keeps the historic "base" URI.
	baseBandwidth := 0
	if ls, ok := hs.(frontend.HLSLadderSession); ok {
		rungs := ls.Rungs()
		if singleRungHLSClient(r, entry) && len(rungs) > 1 {
			rungs = rungs[:1]
		}
		if len(rungs) == 1 {
			baseBandwidth = rungs[0].Bandwidth
		}
		if len(rungs) > 1 {
			var b strings.Builder
			b.WriteString("#EXTM3U\n")
			for _, rg := range rungs {
				line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", rg.Bandwidth)
				if rg.AverageBandwidth > 0 {
					line += fmt.Sprintf(",AVERAGE-BANDWIDTH=%d", rg.AverageBandwidth)
				}
				if rg.Width > 0 && rg.Height > 0 {
					line += fmt.Sprintf(",RESOLUTION=%dx%d", rg.Width, rg.Height)
				}
				if rg.Codecs != "" {
					line += fmt.Sprintf(",CODECS=%q", rg.Codecs)
				}
				if rg.FrameRate > 0 {
					line += fmt.Sprintf(",FRAME-RATE=%.3f", rg.FrameRate)
				}
				uri := s.mediaURL("session/"+sessionID+"/"+rg.ID+"/index.m3u8", sessionID, entry.streamToken)
				b.WriteString(line + "\n" + uri + "\n")
			}
			body := b.String()
			h := w.Header()
			h.Set("Content-Type", "application/vnd.apple.mpegurl")
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, body)
			}
			return
		}
	}
	bw := info.BitrateKbps
	if dec.VideoCopy {
		// BANDWIDTH is the peak segment rate; a copy's segments swing
		// above the container average.
		var segDur time.Duration
		if sd, ok := hs.(interface{ SegmentDuration() time.Duration }); ok {
			segDur = sd.SegmentDuration()
		}
		bw = frontend.HLSCopyPeakKbps(info, segDur)
	} else if dec.BitrateKbps > 0 {
		bw = dec.BitrateKbps
	}
	// A single-rung session still owns measured copy peaks and ramp rate
	// controls. Keep the legacy attribute layout but use its actual budget.
	if baseBandwidth > 0 {
		bw = (baseBandwidth + 999) / 1000
	}
	if bw <= 0 {
		bw = 8000
	}
	line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", bw*1000)
	if v := info.Video(); v != nil && v.Width > 0 && v.Height > 0 {
		wd, ht := v.Width, v.Height
		if !dec.VideoCopy && dec.MaxHeight > 0 && ht > dec.MaxHeight {
			wd, ht = wd*dec.MaxHeight/ht, dec.MaxHeight
		}
		line += fmt.Sprintf(",RESOLUTION=%dx%d", wd, ht)
		if v.FrameRate > 0 {
			line += fmt.Sprintf(",FRAME-RATE=%.3f", v.FrameRate)
		}
	}
	line += frontend.HLSVariantAttributes(info, dec)
	body := "#EXTM3U\n" + line + "\n" + s.mediaURL("session/"+sessionID+"/base/index.m3u8", sessionID, entry.streamToken) + "\n"
	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// handleTranscodeSession serves session/{id}/base/index.m3u8 and segments.
func (s *Server) handleTranscodeSession(w http.ResponseWriter, r *http.Request) {
	if s.d.Transcoder == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	id, ok := routeSessionID(r)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if s.forwardPlexMedia(w, r, id) {
		return
	}
	hs, ok := s.plexHLSSession(id)
	if !ok || hs == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.transcodes.touch(id, s.now())
	rest := chi.URLParam(r, "*")
	name := rest
	if i := strings.LastIndexByte(rest, '/'); i >= 0 {
		name = rest[i+1:]
	}
	// ABR ladder: session/{id}/{rung}/index.m3u8 and .../{rung}/{seg}.ts.
	// The primary rung is "base"; sessions without a ladder fall through
	// to the historic single-rung handling.
	if ls, isLadder := hs.(frontend.HLSLadderSession); isLadder {
		if rungID, _, found := strings.Cut(rest, "/"); found {
			for _, rg := range ls.Rungs() {
				if rg.ID != rungID {
					continue
				}
				if strings.HasSuffix(name, ".m3u8") {
					s.serveStreamManifest(w, r, id, func(w http.ResponseWriter, r *http.Request) {
						ls.ServeRungPlaylist(w, r, rungID)
					})
					return
				}
				n, ok := segmentNumber(name)
				if !ok {
					writeHTTPError(w, http.StatusNotFound)
					return
				}
				cw := s.newCountWriter(w, r)
				ls.ServeRungSegment(cw, r, rungID, n)
				cw.finish()
				return
			}
		}
	}
	if strings.HasSuffix(name, ".m3u8") {
		s.serveStreamManifest(w, r, id, hs.ServePlaylist)
		return
	}
	n, ok := segmentNumber(name)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	cw := s.newCountWriter(w, r)
	hs.ServeSegment(cw, r, n)
	cw.finish()
}

// segmentNumber extracts the segment index from names like "00012.ts",
// "12.ts", "segment-12.m4s".
func segmentNumber(name string) (int, bool) {
	if name == frontend.InitSegmentName || name == frontend.MatroskaHeaderName {
		return frontend.InitSegment, true
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	end := len(base)
	start := end
	for start > 0 && base[start-1] >= '0' && base[start-1] <= '9' {
		start--
	}
	if start == end {
		return 0, false
	}
	n, err := strconv.Atoi(base[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// The control endpoints distinguish an omitted session from an empty one.
func transcodeControlSession(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("session") {
		writeHTTPError(w, http.StatusBadRequest)
		return false
	}
	if q.Get("session") == "" {
		writeHTTPError(w, http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) handleTranscodePing(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if !transcodeControlSession(w, r) {
		return
	}
	if id := sanitizeSessionID(r.URL.Query().Get("session")); id != "" {
		if e, ok := s.transcodes.get(id); ok && e.userID == FromContext(r.Context()).User.ID {
			s.transcodes.touch(id, s.now())
			if s.d.Transcoder != nil {
				if hs, ok := s.plexHLSSession(id); ok {
					if t, ok := hs.(interface{ Touch() }); ok {
						t.Touch()
					}
				}
			}
		}
	}
	if e, ok := s.transcodes.get(sanitizeSessionID(r.URL.Query().Get("session"))); !ok || e.userID != FromContext(r.Context()).User.ID {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTranscodeStop(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if !transcodeControlSession(w, r) {
		return
	}
	id := sanitizeSessionID(r.URL.Query().Get("session"))
	pr := FromContext(r.Context())
	if e, ok := s.transcodes.get(id); !ok || e.userID != pr.User.ID {
		if pr.User != nil && s.transcodes.wasStoppedBy(id, pr.User.ID, s.now()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if e, ok := s.transcodes.get(id); id != "" && ok && e.userID == pr.User.ID {
		unlockOwner, err := s.lockPlaybackOwner(r.Context(), e.userID, e.clientID)
		if err != nil {
			return
		}
		defer unlockOwner()
		unlockEncoder, err := s.lockEncoder(r.Context(), id)
		if err != nil {
			return
		}
		defer unlockEncoder()
		current, ok := s.transcodes.get(id)
		if !ok || current.userID != e.userID || current.clientID != e.clientID {
			w.WriteHeader(http.StatusOK)
			return
		}
		e = current
		if s.playbackOwners.Stale(e.userID, e.clientID, e.key, pr.SessionID) && e.playbackID != pr.SessionID {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Preserve the failure and the last client playhead even if stop
		// removes the encoder before a terminal timeline is delivered.
		if s.d.Transcoder != nil {
			if session, ok := s.plexHLSSession(id); ok {
				if failed, ok := session.(interface{ ProgressiveFailed() bool }); ok && failed.ProgressiveFailed() {
					s.rememberProgressiveStop(r.Context(), e)
				}
			}
		}
		shared := false
		if s.d.Transcoder != nil && e.playID != "" {
			for _, other := range s.transcodes.byPlayID(e.playID) {
				if other != id {
					if _, active := s.plexHLSSession(other); active {
						shared = true
						break
					}
				}
			}
		}
		// A replacement decision owns the source before its encoder starts.
		// Retire a superseded encoder without releasing that prepared source,
		// including stops that omit the external playback identifier.
		superseded := s.playbackOwners.Stale(e.userID, e.clientID, e.key, e.playbackID)
		// Another encoder, or a replacement decision, still owns this source.
		// Broker Stop would release it. The last encoder stops the broker
		// session that owns the source.
		if !superseded && !shared {
			s.endVersionFlow(r.Context(), playIdentityKey{userID: e.userID, clientID: e.clientID, key: e.key})
			if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok && pr.User != nil {
				sid, lookupErr := lookup.PlexSessionForPlayback(r.Context(), s.plexAccess(pr), e.key, pr.SessionID, id)
				if lookupErr == nil && sid != "" {
					playID, known := lookup.PlexSessionSource(sid)
					if sid == id || (known && playID == e.playID) {
						_ = s.playbackBroker.Stop(r.Context(), s.plexAccess(pr), sid)
					}
				}
			}
		}
		if s.d.Transcoder != nil {
			s.stopPlexHLS(id)
		}
		s.dropTranscode(id)
	}
	w.WriteHeader(http.StatusOK)
}

// handleTranscodeSessions is /transcode/sessions.
func (s *Server) handleTranscodeSessions(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{}
	u := FromContext(r.Context()).User
	for _, e := range s.sessions.list() {
		if !u.IsOwner && e.user.ID != u.ID {
			continue
		}
		if e.transcodeID == "" {
			continue
		}
		if te, ok := s.transcodes.get(e.transcodeID); ok {
			mc.TranscodeSess = append(mc.TranscodeSess, s.servedTranscodeSessionModel(te))
		}
	}
	mc.Size = len(mc.TranscodeSess)
	s.write(w, r, mc)
}

// handleTranscodeSessionStatus is /transcode/sessions/{session}, the key every
// TranscodeSession carries. PMS clients poll it for one session's state;
// a desktop app's embedded server does so while it pulls a download
// stream from this server. Viewers see only their own sessions.
func (s *Server) handleTranscodeSessionStatus(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	te, ok := s.transcodes.get(chi.URLParam(r, "session"))
	if !ok || u == nil || (!u.IsOwner && te.userID != u.ID) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, TranscodeSess: []model.TranscodeSess{s.servedTranscodeSessionModel(te)}})
}

// servedTranscodeSessionModel is the TranscodeSession of what is actually
// being served. A player that stepped down to an encoded ABR rung of a
// Direct Stream session is transcoding video, and the Plex dashboard must
// say so just as Ether's Activity does.
func (s *Server) servedTranscodeSessionModel(te *transcodeEntry) model.TranscodeSess {
	ts := transcodeSessionModel(te)
	hs, ok := s.plexHLSSession(te.id)
	if !ok || hs == nil {
		return ts
	}
	src, ok := hs.(interface {
		EncoderStatus() *frontend.EncoderStatus
	})
	if !ok {
		return ts
	}
	if st := src.EncoderStatus(); st != nil && st.Rung != "" && st.Rung != "base" && !st.Decision.VideoCopy {
		ts.VideoDecision = "transcode"
		ts.VideoCodec = plexCodec(st.Decision.VideoCodec)
	}
	return ts
}

// sessionTranscodeMedia renders the active conversion/remux Media for
// /status/sessions once start.m3u8/start.mpd/start has actually started
// (te.started). Tautulli and Plex Dashboard inspect Part@decision on the
// session's Media to distinguish Direct Play (Part@decision omitted) from
// Direct Stream (Part@decision="transcode" with VideoDecision="copy").
func (s *Server) sessionTranscodeMedia(t *frontend.Title, te *transcodeEntry, viewer *Request) (model.Media, bool) {
	if t == nil || te == nil || !te.started || viewer == nil || viewer.User == nil {
		return model.Media{}, false
	}
	ps := &frontend.PlaySession{Info: te.info, StartedAt: t.UpdatedAt}
	if s.d.Playback != nil {
		if live, ok := s.d.Playback.Session(te.playID); ok && live != nil {
			ps = live
		} else if live, ok := s.d.Playback.SessionFor(te.userID, te.clientID, te.key); ok && live != nil {
			ps = live
		}
	}
	if len(ps.Info.Streams) == 0 && len(te.info.Streams) > 0 {
		cp := *ps
		cp.Info = te.info
		ps = &cp
	}
	dec := te.decision
	if dec.Kind == frontend.DecisionDirectPlay {
		dec.Kind = frontend.DecisionRemux
	}
	if hs, ok := s.plexHLSSession(te.id); ok && hs != nil {
		if src, ok := hs.(interface {
			EncoderStatus() *frontend.EncoderStatus
		}); ok {
			if st := src.EncoderStatus(); st != nil && st.Rung != "" && st.Rung != "base" && !st.Decision.VideoCopy {
				dec = st.Decision
			}
		}
	}
	p := te.requested
	p.key = t.Key
	p.caps = te.caps
	if p.caps.ClientID == "" {
		p.caps.ClientID = te.clientID
	}
	if p.protocol == "" {
		if te.opts.Protocol != "" {
			p.protocol = te.opts.Protocol
		} else {
			p.protocol = frontend.ProtocolHLS
		}
	}
	ik := s.playIdentityFor(viewer, t.Key)
	if pin := s.playIDs.pinned(ik, true, s.now()); pin.selected != 0 {
		p.mediaID = pin.selected
	} else if p.mediaID == 0 {
		p.mediaID = int64(t.Key)
	}
	for i, st := range ps.Info.Streams {
		if st.Type == "audio" && st.Index == dec.AudioStream {
			p.audioSel = i
			break
		}
	}
	if te.subtitle >= 0 && te.subtitle < len(ps.Info.Streams) && ps.Info.Streams[te.subtitle].Type == "subtitle" {
		p.subSel, p.subSet = te.subtitle, true
	} else {
		p.subSel, p.subSet = -1, false
	}
	p.listAllTracks = androidFamilyClient(viewer)
	p.softText = browserClient(viewer)
	if plexAndroidClient(viewer) && dec.BurnSubtitles && p.subtitles == "none" {
		p.subtitles = "burn"
	}
	return s.decisionMedia(t, ps, dec, p, viewer.User), true
}

func transcodeSessionModel(te *transcodeEntry) model.TranscodeSess {
	ts := model.TranscodeSess{
		Key:        "/transcode/sessions/" + te.id,
		Context:    "streaming",
		Protocol:   "hls",
		Container:  "mpegts",
		Duration:   ms(te.info.Duration),
		Progress:   -1,
		TimeStamp:  float64(te.updatedAt.Unix()),
		AudioCodec: plexCodec(te.decision.AudioCodec),
		VideoCodec: plexCodec(te.decision.VideoCodec),
	}
	if te.started {
		switch cmp.Or(te.opts.Protocol, te.requested.protocol) {
		case frontend.ProtocolDASH:
			ts.Protocol = frontend.ProtocolDASH
		case frontend.ProtocolHTTP:
			ts.Protocol = frontend.ProtocolHTTP
		default:
			ts.Protocol = frontend.ProtocolHLS
		}
		switch te.decision.Container {
		case frontend.ContainerMatroska:
			ts.Container = "mkv"
		case frontend.ContainerFMP4:
			ts.Container = "mp4"
		default:
			if ts.Protocol == frontend.ProtocolHTTP {
				ts.Container = "mkv"
			} else {
				ts.Container = "mpegts"
			}
		}
	}
	if v := te.info.Video(); v != nil {
		ts.SourceVideoCodec = plexCodec(v.Codec)
	}
	for _, st := range te.info.Streams {
		if st.Type == "audio" {
			ts.SourceAudioCodec = plexCodec(st.Codec)
			break
		}
	}
	ts.VideoDecision, ts.AudioDecision = "copy", "copy"
	if !te.decision.VideoCopy {
		ts.VideoDecision = "transcode"
	} else {
		ts.VideoCodec = ts.SourceVideoCodec
	}
	if !te.decision.AudioCopy {
		ts.AudioDecision = "transcode"
	} else {
		ts.AudioCodec = ts.SourceAudioCodec
	}
	if te.decision.BurnSubtitles {
		ts.SubtitleDecision = "burn"
	} else if te.started && te.subtitle >= 0 && te.subtitle < len(te.info.Streams) && te.info.Streams[te.subtitle].Type == "subtitle" {
		sub := te.info.Streams[te.subtitle]
		codec := plexCodec(sub.Codec)
		text := textSidecarCodec(codec)
		inSegments := text && te.decision.Container == frontend.ContainerMatroska && te.decision.SubtitleStream >= 0 && te.decision.SubtitleStream == sub.Index
		softText := browserClient(&Request{Product: te.caps.Product})
		switch {
		case te.requested.subtitles == "none" && !inSegments:
		case softText && text && te.decision.Container != frontend.ContainerMatroska:
			ts.SubtitleDecision = "transcode"
		default:
			ts.SubtitleDecision = "copy"
		}
	}
	return ts
}

// ---------------------------------------------------------------------------
// Direct play

// handlePart serves /library/parts/{part}/{changestamp}/{file} with Range
// support through Playback.ServeRange. A variant Part id names a specific
// release of the title ("Play Version" direct play).
func (s *Server) handlePart(w http.ResponseWriter, r *http.Request) {
	// Validate before acquisition; padded binary integers are accepted by PMS.
	download := false
	if values, present := r.URL.Query()["download"]; present {
		raw := values[0]
		raw = strings.TrimPrefix(raw, "+")
		raw = strings.TrimLeft(raw, "0")
		if raw != "" && raw != "1" || values[0] == "" || values[0] == "+" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		download = raw == "1"
	}
	if s.handleExtraPart(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if s.playbackTerminated(pr) {
		writePlaybackStatus(w, http.StatusServiceUnavailable)
		return
	}
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	key, releaseID, found := s.partTitle(id)
	if !found {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, titleErr := s.d.Catalog.Get(ctx, key)
	if titleErr != nil || !s.playableTitle(ctx, pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if policy, ok := s.d.AccessPolicy.(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.ClientCaps(0).Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	// Clients may fall back from a failed decision to the original part URL.
	// Do not launch the same failed acquisition again during that attempt.
	if releaseID == "" && pr.ClientID != "" {
		for _, id := range s.transcodes.forClient(pr.User.ID, pr.ClientID, 0) {
			if pr.SessionID != "" && id != sanitizeSessionID(pr.SessionID) {
				continue // a distinct playback attempt must be allowed to retry
			}
			if err, failed := s.transcodes.takeFailure(id, pr.User.ID, key, s.now()); failed && !errors.Is(err, frontend.ErrPrepareSlow) {
				s.writePartErr(w, r, pr, err)
				return
			}
		}
	}
	source := s.plexSources().Part(pr.User.ID, pr.ClientID, key, releaseID)
	ps, ok := source.Session, source.Matches
	live, anonymousContinuation := source.PreviousID, source.AnonymousContinuation
	if anonymousContinuation {
		if confirm, supported := s.playbackBroker.(interface {
			ConfirmPlexAdmission(context.Context, *frontend.Viewer, bool) (*frontend.Viewer, error)
		}); supported {
			viewer, err := confirm.ConfirmPlexAdmission(ctx, pr.User, pr.ClientCaps(0).Remote)
			if err != nil || viewer == nil || viewer.ID != pr.User.ID || !s.playableTitle(ctx, viewer, title) {
				writeHTTPError(w, http.StatusForbidden)
				return
			}
		}
	}
	if !ok || ps == nil {
		t, err := s.d.Catalog.Get(ctx, key)
		if err == nil && !s.titleVisible(pr.User, t) {
			err = frontend.ErrNotFound
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		caps := s.clientCapsForKey(pr, key)
		s.observeClientAdmission(pr, caps)
		ps, err = s.prepareForRelease(ctx, pr.User, key, caps, releaseID)
		if err != nil {
			s.log.Info("direct play failed", "key", key, "reason", playbackMessage(err), "err", err)
			s.writePartErr(w, r, pr, err)
			return
		}
		// The picked version fell back: the playback kept the session that
		// already plays the automatic choice, so this read continues it.
		ok = live != "" && ps.ID == live
		if !ok {
			s.observePlaybackDecision(ctx, pr, &playRequest{key: key, caps: caps, partReleaseID: releaseID, requestedPart: id}, ps)
		}
	}
	if !anonymousContinuation && r.Method != http.MethodHead && !download {
		// A part read is the playing Media for clients that skip the
		// decision; a decision's admission keeps its selection unless this
		// read prepared another release.
		media := int64(key)
		if releaseID != "" {
			media = id
		}
		release := ""
		if ps.Resolution != nil {
			release = ps.Resolution.Release.ID
		}
		s.playIDs.admit(s.playIdentityFor(pr, key), media, release, !ok, s.now())
		s.playbackOwners.Bind(pr.User.ID, pr.ClientID, key, pr.SessionID, ps.ID, false, s.now())
		sessionID := pr.SessionID
		if sessionID == "" {
			sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
		}
		partPlay := playRequest{key: key, caps: s.declaredCaps(pr), sessionID: sanitizeSessionID(sessionID), audioSel: -1, subSel: -1}
		if partPlay.caps.ClientID == "" {
			partPlay.caps.ClientID = pr.ClientID
		}
		partPlay.opts.DirectPlay = true
		partPlay.opts.DirectStream = true
		// A part range of a playback that is already on the broker must not
		// open a second session. That open supersedes the live encoder.
		already := false
		if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok {
			if sid, lookupErr := lookup.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); lookupErr == nil && sid != "" {
				if playID, ok := lookup.PlexSessionSource(sid); ok && playID == ps.ID {
					already = true
				}
			}
		}
		if !already {
			if _, err := s.publishBrokerPlayback(ctx, pr, &partPlay, ps); err != nil {
				s.log.Info("direct play broker publish failed", "key", key, "err", err)
				s.writeErr(w, r, err)
				return
			}
		}
	}
	if download {
		// Plex Web's Download button is a plain part read; only the viewer
		// policy (an owner override or plex.tv allowSync=0) refuses it. The
		// download queue being off is a different feature.
		if p, ok := s.d.AccessPolicy.(frontend.DownloadAccessPolicy); ok && !p.AllowDownloads(pr.User) {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		filename := chi.URLParam(r, "file")
		if decoded, err := url.PathUnescape(filename); err == nil {
			filename = decoded
		}
		w.Header().Set("Content-Disposition", contentDisposition(filename))
	}
	unlockOwner()
	cw := s.newCountWriter(w, r)
	s.plexSources().ServePart(cw, r, ps.ID)
	cw.finish()
}

// Part acquisition failures distinguish an absent provider file from a failed
// upstream transport; the conversion start routes retain their own contract.
func (s *Server) writePartErr(w http.ResponseWriter, r *http.Request, pr *Request, err error) {
	if errors.Is(err, frontend.ErrNotFound) || errors.Is(err, frontend.ErrNotAvailable) || errors.Is(err, frontend.ErrUnauthorized) || errors.Is(err, frontend.ErrStreamLimit) || errors.Is(err, frontend.ErrPrepareSlow) {
		s.writeStartErr(w, r, pr, err)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if pr != nil && pr.User != nil {
		s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), playbackMessage(err))
	}
	writePlaybackStatus(w, http.StatusBadGateway)
}

// handlePartSelect is PUT or POST /library/parts/{part}?audioStreamID=&subtitleStreamID=.
// The stored subtitle is the title's choice. The part id only finds that
// title: Play Version part ids move between releases as the list reorders.
//
// GET /library/metadata is built for that request from this choice. Nothing
// caches the rendered body, and the response is Cache-Control: no-cache, so
// a re-read sees the new selection. Apple TV checks the menu radio from the
// PUT immediately and leaves the detail chip on its previous label until
// that screen loads again. Plex Media Server's client does the same; the
// chip is not a stale Ether document.
func (s *Server) handlePartSelect(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	rawID, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// Resolve and authorize the part before interpreting either selector.
	key, _, found := s.partTitle(rawID)
	if !found || s.d.Catalog == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, err := s.metadataTitle(r.Context(), pr, key)
	if err != nil || title == nil || !s.titleVisible(pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	id := int64(key)
	q := r.URL.Query()
	// These two selection parameters deliberately have different PMS grammars.
	audioID, audioErr := strconv.ParseInt(q.Get("audioStreamID"), 10, 64)
	subID, subErr := strconv.ParseInt(q.Get("subtitleStreamID"), 10, 64)
	if (q.Has("audioStreamID") && audioErr == nil && (audioID == 0 || audioID < -1)) ||
		(q.Has("subtitleStreamID") && subErr == nil && subID < -1) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	audioDefault := q.Has("audioStreamID") && audioErr == nil && audioID == -1
	subDefault := q.Has("subtitleStreamID") && subErr == nil && subID == -1
	ctx := frontend.WithStreamServe(r.Context(), pr.Remote, "")
	var audio *frontend.StreamBinding
	if v := q.Get("audioStreamID"); v != "" && !audioDefault {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sid == 0 {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			binding, rerr := s.resolveIssuedStream(ctx, pr.User, pr.ClientID, sid)
			if rerr != nil && !streamMiss(rerr) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			if rerr != nil || binding.Info.Type != "audio" || binding.Key != key {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			audio = &binding
		}
	}
	clearSub := q.Has("subtitleStreamID") && (subErr != nil || subID == 0)
	var sub *frontend.StreamBinding
	if v := q.Get("subtitleStreamID"); v != "" && !subDefault {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sid == 0 {
				clearSub = true
			} else {
				binding, rerr := s.resolveIssuedStream(ctx, pr.User, pr.ClientID, sid)
				if rerr != nil && !streamMiss(rerr) {
					writeHTTPError(w, http.StatusUnauthorized)
					return
				}
				if rerr != nil || binding.Info.Type != "subtitle" || binding.Key != key || (audio != nil && audio.Key != binding.Key) {
					writeHTTPError(w, http.StatusBadRequest)
					return
				}
				sub = &binding
			}
		}
	}
	if q.Has("subtitleStreamID") {
		if err := s.recordManualSubtitle(r.Context(), pr.User.ID, key, !subDefault); err != nil {
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
	}
	s.selections.update(pr.User.ID, id, func(sel *selection) {
		if audioDefault {
			sel.audio, sel.audioSet, sel.audioStream = 0, false, nil
		}
		if subDefault {
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = 0, false, nil
		}
		if audio != nil {
			info := audio.Info
			sel.audio, sel.audioSet, sel.audioStream = audio.Index, true, &info
		}
		if clearSub {
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, nil
		} else if sub != nil {
			info := sub.Info
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = sub.Index, true, &info
		}
	})
	if clearSub || sub != nil {
		// This client now shows the track it picked (burnOnlyShownSubtitle).
		var picked *frontend.StreamInfo
		if sub != nil {
			picked = &sub.Info
		}
		s.playIDs.pickSubtitle(s.playIdentityFor(pr, frontend.RatingKey(id)), picked, s.now())
	}
	s.rememberStreamRequest(r, pr.User, frontend.RatingKey(id))
	w.WriteHeader(http.StatusOK)
}

var subtitleContentTypes = map[string]string{
	"srt": "application/x-subrip",
	"vtt": "text/vtt",
	"ass": "text/x-ssa",
	"ssa": "text/x-ssa",
}

// handleStream serves external (sidecar) subtitle streams:
// /library/streams/{id}[.ext][?format=srt|vtt|webvtt]. Cached OpenSubtitles
// tracks are UTF-8 (any encoding= parameter is satisfied) and served in
// either format; other sidecars are served as stored.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "stream")
	raw, _, _ = strings.Cut(raw, ".")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		status := http.StatusNotFound
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status)
		return
	}
	if !s.acceptanceCurrent(r) {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	pr := FromContext(r.Context())
	format := subtitleFormat(r.URL.Query().Get("format"))
	if _, ext, ok := strings.Cut(chi.URLParam(r, "stream"), "."); ok {
		format = subtitleFormat(ext)
	}
	if s.boundStreams == nil || pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	binding, rerr := s.resolveIssuedStream(r.Context(), pr.User, pr.ClientID, id)
	if rerr != nil {
		if errors.Is(rerr, frontend.ErrUnauthorized) {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		status := http.StatusNotFound
		if r.Method == http.MethodPut {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status)
		return
	}
	// Embedded tracks cannot be removed from the media bytes. PMS accepts
	// stream PUTs without changing the track; reads still use Ether's
	// extraction and sidecar paths.
	switch r.Method {
	case http.MethodPut:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodDelete:
		if !binding.Info.External {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		deleter, ok := s.d.Subtitles.(subtitleDelete)
		if !ok {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if err := deleter.DeleteSubtitle(r.Context(), binding.Key, pr.User, binding.Info); err != nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.selections.update(pr.User.ID, int64(binding.Key), func(sel *selection) {
			if sel.subtitleStream != nil && sel.subtitleStream.ExternalURL == binding.Info.ExternalURL {
				sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, nil
			}
		})
		if resetter, ok := s.d.TrackPreferences.(interface {
			ForgetChoice(context.Context, int64, frontend.RatingKey, string) error
		}); ok {
			if err := resetter.ForgetChoice(r.Context(), pr.User.ID, binding.Key, "subtitle"); err != nil {
				s.log.Warn("clear deleted subtitle preference", "error", err)
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if binding.Info.Type == "subtitle" && !binding.Info.External {
		s.serveEmbeddedStream(w, r, binding.Key, binding.Info)
		return
	}
	receipt, _ := acceptanceFrom(r.Context())
	reader, info, err := s.boundStreams.OpenSidecar(frontend.WithStreamServe(r.Context(), pr.Remote, format), s.plexStreamAccess(pr.User), receipt, id)
	if err != nil || reader == nil {
		if reader != nil {
			_ = reader.Close()
		}
		if errors.Is(err, frontend.ErrUnauthorized) {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	defer reader.Close()
	ct := subtitleContentTypes[plexCodec(info.Codec)]
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = io.Copy(w, io.LimitReader(reader, 32<<20))
}
