package plex

// Sidecar (external) subtitles: listing cached OpenSubtitles tracks before a
// play session exists, keeping a viewer's choice of one across the
// estimate/probe change, the viewer-driven subtitle search
// (GET/PUT /library/metadata/{key}/subtitles) and the transcode sidecar
// endpoint (/video/:/transcode/universal/subtitles).
//
// Sidecar files are downloaded only when the viewer picks a search result.
// Detail pages and playback list whatever is already cached (SubtitleCatalog);
// a live session lists its enriched streams plus any track picked since.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"golang.org/x/text/language"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/subtext"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// SubtitleCatalog is optionally implemented by Deps.Subtitles: the sidecars
// already cached for a title and viewer, without network access, skipping
// streams embedded already lists (by ExternalURL) or covers (by language).
type SubtitleCatalog = frontend.SubtitleSidecars

// SubtitleOffer is one subtitle search result.
type SubtitleOffer = frontend.SidecarOffer

// SubtitleSearch is optionally implemented by Deps.Subtitles: Plex's
// on-demand subtitle search and pick.
type SubtitleSearch = frontend.SubtitleSearch

// infoWithSelectedSidecar keeps a chosen external subtitle on a metadata
// stream list. Cached sidecars omit a language the file already embeds, so
// the next detail response would select that embedded track, or Off when
// the viewer's subtitle mode is off.
func infoWithSelectedSidecar(info frontend.MediaInfo, want *frontend.StreamInfo) frontend.MediaInfo {
	ps := ensureSelectedSidecar(&frontend.PlaySession{Info: info}, want)
	if ps == nil {
		return info
	}
	return ps.Info
}

// ensureSelectedSidecar keeps a viewer's chosen external subtitle on the
// play session when the probe has no external file of that language.
// Cached sidecars omit a language the file already embeds, so an embedded
// PGS (or another track the client cannot draw) would otherwise win the
// decision and Continue Watching would show no SRT. A probed external of
// the same language is left alone: the stored choice follows that release
// sidecar instead of pinning the earlier cached copy beside it.
func ensureSelectedSidecar(ps *frontend.PlaySession, want *frontend.StreamInfo) *frontend.PlaySession {
	if ps == nil || want == nil || want.Type != "subtitle" || !want.External || want.ExternalURL == "" {
		return ps
	}
	for _, st := range ps.Info.Streams {
		if st.Type != "subtitle" || !st.External {
			continue
		}
		if st.ExternalURL == want.ExternalURL || sameLanguage(st.Language, want.Language) {
			return ps
		}
	}
	cp := *ps
	streams := make([]frontend.StreamInfo, len(ps.Info.Streams)+1)
	copy(streams, ps.Info.Streams)
	streams[len(ps.Info.Streams)] = *want
	cp.Info.Streams = streams
	return &cp
}

// withCachedSidecars appends the viewer's cached sidecars that info does not
// already list. It never touches the network.
func (s *Server) withCachedSidecars(ctx context.Context, u *frontend.User, t *frontend.Title, info frontend.MediaInfo) frontend.MediaInfo {
	c, ok := s.d.Subtitles.(SubtitleCatalog)
	if !ok || t == nil {
		return info
	}
	extra := c.CachedSidecars(ctx, t, u, info.Streams)
	if len(extra) == 0 {
		return info
	}
	out := info
	out.Streams = append(append(make([]frontend.StreamInfo, 0, len(info.Streams)+len(extra)), info.Streams...), extra...)
	return out
}

// resolveSubtitle finds a chosen subtitle in info by language, codec, and
// source. The old stream position is only a tie-break. -1 when info has none.
func resolveSubtitle(want *frontend.StreamInfo, pos int, info frontend.MediaInfo) int {
	return frontend.ResolveSubtitle(want, pos, info)
}

// resolveAudio retains the requested language across estimates and releases.
// Within that language prefer the same codec, channel layout and track label;
// position breaks ties but can never override a language mismatch.
func resolveAudio(want *frontend.StreamInfo, pos int, info frontend.MediaInfo) int {
	if want == nil {
		return -1
	}
	return frontend.ResolveAudio(want, pos, info)
}

// resolveSelection maps stored track identities onto the current positions. A
// choice that info cannot satisfy is dropped, so viewer preferences apply.
func resolveSelection(sel selection, info frontend.MediaInfo) selection {
	if sel.audioSet && sel.audioStream != nil {
		sel.audio = resolveAudio(sel.audioStream, sel.audio, info)
		sel.audioSet = sel.audio >= 0
	}
	if !sel.subtitleSet || sel.subtitleStream == nil {
		return sel
	}
	if p := resolveSubtitle(sel.subtitleStream, sel.subtitle, info); p >= 0 {
		sel.subtitle = p
	} else {
		sel.subtitle, sel.subtitleSet = -1, false
	}
	return sel
}

// burnOnlyShownSubtitle keeps a subtitle this request did not name out of
// the picture unless the client was shown it selected (#313). Stored and
// preferred selections resolve on the session's probed tracks, while the
// stream list the client holds can be the pre-playback estimate (which has
// no subtitle tracks) or another release's: the app shows None, and burning
// the track turned a remux into a full transcode the viewer could not turn
// off. Such a selection plays as None, which is what the client shows. A
// track the viewer picks, or one the client was shown selected, is burned
// as before.
func (s *Server) burnOnlyShownSubtitle(pr *Request, p *playRequest, info frontend.MediaInfo) {
	if p.subNamed || !p.subSet || p.opts.SubtitleStream < 0 || p.subSel < 0 || p.subSel >= len(info.Streams) {
		return
	}
	st := info.Streams[p.subSel]
	if st.Type != "subtitle" || st.External || st.Index != p.opts.SubtitleStream || !frontend.BurnsSubtitle(st, p.caps, p.opts) {
		return
	}
	ik := s.playIdentityFor(pr, p.key)
	if !ik.valid() {
		return
	}
	shown, _ := s.playIDs.shownSubtitles(ik, s.now())
	for i := range shown {
		want := shown[i]
		if want.External == st.External && frontend.IsImageSubtitle(want.Codec) == frontend.IsImageSubtitle(st.Codec) && resolveSubtitle(&want, -1, info) == p.subSel {
			return
		}
	}
	s.log.Info("subtitle not burned: the client was not shown it selected", "key", p.key, "client", pr.ClientID, "codec", st.Codec, "language", st.Language)
	p.subSel, p.subSet, p.subStream = -1, true, nil
	p.opts.SubtitleStream = -1
}

// resolvePlaySelection is resolveSelection for a play request.
func resolvePlaySelection(p *playRequest, info frontend.MediaInfo) {
	out, err := frontend.TranslatePlexTrackIdentity(frontend.IssuedTrackIdentity{
		Audio: p.audioStream, AudioPosition: p.audioSel, AudioSet: p.audioSel >= 0,
		Subtitle: p.subStream, SubtitlePosition: p.subSel, SubtitleSet: p.subSet,
	}, info)
	if err != nil {
		return
	}
	p.audioSel = out.Audio
	p.subSel, p.subSet = out.Subtitle, out.SubtitleSet
	if !p.subSet {
		p.subSel = -1
	}
}

// stampMatroskaVTT maps WebVTT cue times onto segmented Matroska.
// Cluster timestamps begin at the 10s mux origin (900000 in the 90 kHz
// clock X-TIMESTAMP-MAP uses). Cue times stay on the title clock, so
// LOCAL 00:00:00.000 is the first frame of the title, not PTS 0.
func stampMatroskaVTT(body []byte) []byte {
	const magic = "WEBVTT"
	s := string(body)
	if !strings.HasPrefix(s, magic) {
		s = magic + "\n\n" + s
	}
	if strings.Contains(s, "X-TIMESTAMP-MAP=") {
		return []byte(s)
	}
	rest := strings.TrimPrefix(s[len(magic):], "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	return []byte(magic + "\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:900000\n" + rest)
}

// subtitleFormat maps a requested subtitle format to one Ether serves.
func subtitleFormat(v string) string {
	switch strings.ToLower(strings.TrimPrefix(v, ".")) {
	case "vtt", "webvtt":
		return "vtt"
	}
	return "srt"
}

// subtitleTitle resolves the playable title of a /library/metadata/{key}/
// subtitles request the viewer may see.
func (s *Server) subtitleTitle(r *http.Request) (*frontend.Title, bool) {
	keys, err := parseKeys(chi.URLParam(r, "key"))
	if err != nil || len(keys) != 1 || !isPlayableKey(keys[0]) || s.d.Catalog == nil {
		return nil, false
	}
	t, err := s.d.Catalog.Get(r.Context(), keys[0])
	if err != nil || t == nil || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		return nil, false
	}
	return t, true
}

// subtitleOffers keeps bounded, viewer-scoped provider references behind local IDs.
// No library lookup or persistent write is needed to resolve a search leaf.
type subtitleOffers struct {
	mu   sync.Mutex
	next int64
	rows map[int64]subtitleOfferRef
}
type subtitleOfferRef struct {
	key      frontend.RatingKey
	user     int64
	provider int64
}

const maxSubtitleOffers = 4096

func (o *subtitleOffers) add(key frontend.RatingKey, user, provider int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rows == nil {
		o.rows = make(map[int64]subtitleOfferRef)
		o.next = 1 << 40
	}
	o.next++
	o.rows[o.next] = subtitleOfferRef{key, user, provider}
	delete(o.rows, o.next-maxSubtitleOffers)
	return o.next
}
func (o *subtitleOffers) get(id int64, key frontend.RatingKey, user int64) (int64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	row, ok := o.rows[id]
	return row.provider, ok && row.key == key && row.user == user
}

// subtitlePicks tracks in-flight viewer picks (PUT .../subtitles). The PUT
// answers before the download finishes, as on PMS. Clients that do not
// follow the activity socket (Plex for Android TV during playback, #389)
// re-read the item at once and look the new track up in it, so a
// single-item metadata read waits briefly for a pick of that title to
// land instead of answering without the track.
type subtitlePicks struct {
	mu      sync.Mutex
	pending map[subtitlePickKey]*subtitlePick
}

type subtitlePickKey struct {
	user int64
	key  frontend.RatingKey
}

type subtitlePick struct {
	n    int
	done chan struct{}
}

// subtitlePickWait bounds how long one metadata read waits for a pick.
// Downloads are a small text file; a client that polls several times gets
// a fresh wait on each read.
const subtitlePickWait = 5 * time.Second

func (p *subtitlePicks) begin(user int64, key frontend.RatingKey) func() {
	k := subtitlePickKey{user, key}
	p.mu.Lock()
	if p.pending == nil {
		p.pending = map[subtitlePickKey]*subtitlePick{}
	}
	cur := p.pending[k]
	if cur == nil {
		cur = &subtitlePick{done: make(chan struct{})}
		p.pending[k] = cur
	}
	cur.n++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		cur.n--
		if cur.n <= 0 {
			close(cur.done)
			delete(p.pending, k)
		}
		p.mu.Unlock()
	}
}

func (p *subtitlePicks) wait(ctx context.Context, user int64, key frontend.RatingKey) {
	p.mu.Lock()
	cur := p.pending[subtitlePickKey{user, key}]
	p.mu.Unlock()
	if cur == nil {
		return
	}
	timer := time.NewTimer(subtitlePickWait)
	defer timer.Stop()
	select {
	case <-cur.done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// searchLevel parses Plex's hearingImpaired/forced search options:
// 0 prefer without, 1 prefer with, 2 only with, 3 only without.
func searchLevel(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 3 {
		return 0
	}
	return n
}

func keepOffer(level int, has bool) bool {
	switch level {
	case 2:
		return has
	case 3:
		return !has
	}
	return true
}

func preferOffer(level int, has bool) bool {
	if level == 1 {
		return has
	}
	return !has
}

// handleSubtitleSearch is GET /library/metadata/{key}/subtitles
// ?language=&hearingImpaired=&forced=: Plex's subtitle "Search". Results
// are subtitle Streams (streamType 3) whose key a PUT downloads. Without
// a configured subtitle service, or when the provider fails, the answer is
// an empty container so the client stops searching.
func (s *Server) handleSubtitleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, ok := s.subtitleTitle(r)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if !q.Has("language") {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	lang := q.Get("language")
	primary := strings.Split(lang, "-")[0]
	tag, err := language.Parse(lang)
	base, confidence := tag.Base()
	if len(primary) != 2 || err != nil || confidence == language.No || base.String() == "und" {
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	lang = frontend.NormalizeLanguage(lang)
	mc := libraryContainer()
	searcher, ok := s.d.Subtitles.(SubtitleSearch)
	if !ok {
		s.write(w, r, mc)
		return
	}
	release := ""
	if pr.User != nil && s.d.Playback != nil {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, t.Key); ok && ps != nil && ps.Resolution != nil {
			release = ps.Resolution.Release.Name
		}
	}
	offers, err := searcher.SearchSubtitles(ctx, t, pr.User, lang, release)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Info("subtitle search failed", "key", t.Key, "language", lang, "err", err)
		}
		s.write(w, r, mc)
		return
	}
	hi, forced := searchLevel(q.Get("hearingImpaired")), searchLevel(q.Get("forced"))
	var preferred, rest []model.Stream
	userID := int64(0)
	if pr.User != nil {
		userID = pr.User.ID
	}
	for _, o := range offers {
		if !keepOffer(hi, o.HearingImpaired) || !keepOffer(forced, o.Forced) {
			continue
		}
		display := subtitleDisplayTitle(frontend.StreamInfo{Type: "subtitle", Codec: "srt", Language: o.Language, Forced: o.Forced})
		id := s.subtitleOffers.add(t.Key, userID, o.ID)
		transient := 1
		st := model.Stream{
			ID:                   id,
			Transient:            &transient,
			StreamType:           3,
			Codec:                "srt",
			Key:                  "/library/streams/" + strconv.FormatInt(id, 10),
			Forced:               boolPtrIf(o.Forced),
			Language:             languageName(o.Language),
			LanguageTag:          languageCode3(o.Language),
			LanguageCode:         languageCode3(o.Language),
			Format:               "srt",
			Title:                o.Title,
			DisplayTitle:         display,
			ExtendedDisplayTitle: extendedTitle(frontend.StreamInfo{Type: "subtitle", Codec: "srt", Title: o.Title, Language: o.Language}, display, "SRT"),
			Extra: []model.Attr{
				model.A("hearingImpaired", o.HearingImpaired),
				model.A("perfectMatch", o.PerfectMatch),
				model.A("providerTitle", o.Provider),
				model.A("score", strconv.Itoa(o.Score)),
				model.A("sourceKey", strconv.FormatInt(o.ID, 10)),
				model.A("userID", userID),
			},
		}
		st.SetSubtitleSearchOrder()
		if preferOffer(hi, o.HearingImpaired) && preferOffer(forced, o.Forced) {
			preferred = append(preferred, st)
		} else {
			rest = append(rest, st)
		}
	}
	mc.Stream = append(preferred, rest...)
	mc.Size = len(mc.Stream)
	s.write(w, r, mc)
}

// handleSubtitlePick is PUT /library/metadata/{key}/subtitles?key=: download
// a search result, add it to the title as an external subtitle stream and
// select it for the viewer.
func (s *Server) handleSubtitlePick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, ok := s.subtitleTitle(r)
	if !ok || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	searcher, ok := s.d.Subtitles.(SubtitleSearch)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if !r.URL.Query().Has("key") {
		s.handleSubtitleSearch(w, r)
		return
	}
	raw, found := strings.CutPrefix(r.URL.Query().Get("key"), "/library/streams/")
	id, err := strconv.ParseInt(raw, 10, 64)
	if !found || err != nil || id <= 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	providerID, ok := s.subtitleOffers.get(id, t.Key, pr.User.ID)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if s.Closed() {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	if err := s.recordManualSubtitle(r.Context(), pr.User.ID, t.Key, true); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	activity := rand.Text()
	// Register the pick before answering, so a metadata re-read racing
	// this response waits for the download instead of missing the track.
	finishPick := s.subtitlePicks.begin(pr.User.ID, t.Key)
	w.Header().Set("X-Plex-Activity", activity)
	w.WriteHeader(http.StatusOK)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer finishPick()
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		s.publishNotification(model.NotificationContainer{Type: "activity", Size: 1, ActivityNotification: []model.ActivityNotification{{Event: "started", UUID: activity, Activity: &model.Activity{UUID: activity, Type: "subtitle.download", Title: "Downloading subtitles", UserID: pr.User.ID}}}}, pr.User.ID)
		st, err := searcher.DownloadSubtitle(ctx, t, pr.User, providerID)
		if err != nil {
			s.log.Warn("subtitle download failed", "key", t.Key, "offer", providerID, "err", err)
			s.finishSubtitleActivity(activity, pr.User.ID)
			return
		}
		s.selections.update(pr.User.ID, int64(t.Key), func(sel *selection) { sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, &st })
		if s.d.TrackPreferences != nil {
			if err := s.d.TrackPreferences.Remember(ctx, pr.User.ID, t.Key, "subtitle", &st); err != nil {
				s.log.Warn("save stream preference", "error", err)
			}
		}
		s.finishSubtitleActivity(activity, pr.User.ID)
	}()

}

// subtitleActivityDelay holds the "ended" event until the client has read
// the PUT response: Plex Web only matches an event against the activity id
// it stored from that response's X-Plex-Activity header.
var subtitleActivityDelay = time.Second

// finishSubtitleActivity publishes the subtitle.download activity's end, as
// PMS does when a picked subtitle is stored. Plex Web's search modal keeps
// its spinner until an "ended" ActivityNotification with the uuid from
// X-Plex-Activity arrives on the event connection (it polls only when no
// event connection is open), then re-reads the item and lists the track
// (#268). The download already finished, so there is no progress to report.
func (s *Server) finishSubtitleActivity(uuid string, userID int64) {
	if s.Closed() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(subtitleActivityDelay)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		s.publishNotification(model.NotificationContainer{
			Type: "activity",
			Size: 1,
			ActivityNotification: []model.ActivityNotification{{
				Event: "ended",
				UUID:  uuid,
				Activity: &model.Activity{
					UUID:     uuid,
					Type:     "subtitle.download",
					Title:    "Downloading subtitles",
					Progress: 100,
					UserID:   userID,
				},
			}},
		}, userID)
	}()
}

// SubtitleUpload is optionally implemented by Deps.Subtitles: Plex Web's
// "Upload Subtitles" (POST /library/metadata/{key}/subtitles).
type SubtitleUpload = frontend.SubtitleUpload

const maxUploadBytes = 16 << 20

// handleSubtitleUpload is POST /library/metadata/{key}/subtitles?title=&format=
// with the file as the body. Matches the Plex Web player modal: empty body
// is 406, a missing title or a title with no extension is 500, an
// unaccepted format is 400, and a successful store is 200 with an empty
// body. The sidecar is selected for the viewer.
func (s *Server) handleSubtitleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, ok := s.subtitleTitle(r)
	if !ok || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// There is no recorded measurement that shared or managed users can upload
	// of the Upload Subtitle modal. Owner only
	// so a friend cannot change anyone else's pick.
	if !pr.User.IsOwner {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	uploader, ok := s.d.Subtitles.(SubtitleUpload)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	// PMS: empty body is 406 before the title or format checks.
	if len(body) == 0 {
		writeHTTPError(w, http.StatusNotAcceptable)
		return
	}
	if len(body) > maxUploadBytes {
		writeHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	q := r.URL.Query()
	title := strings.TrimSpace(q.Get("title"))
	format := q.Get("format")
	if title == "" || !strings.Contains(title, ".") {
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	if format == "vtt" || !subtext.AcceptsUploadFormat(format) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	st, err := uploader.UploadSubtitle(ctx, t, pr.User, title, format, q.Get("language"), body)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("subtitle upload failed", "key", t.Key, "err", err)
		}
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if err := s.recordManualSubtitle(ctx, pr.User.ID, t.Key, true); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	s.selections.update(pr.User.ID, int64(t.Key), func(sel *selection) {
		sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, &st
	})
	if s.d.TrackPreferences != nil {
		if err := s.d.TrackPreferences.Remember(ctx, pr.User.ID, t.Key, "subtitle", &st); err != nil {
			s.log.Warn("save stream preference", "error", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handleTranscodeSubtitles is /video/:/transcode/universal/subtitles (a
// conversion) and /subtitles/:/transcode/universal/start (a direct play):
// the selected subtitle of the session the request's transcoder parameters
// name. Plex Web asks for both without a format and parses the answer as an
// ASS script with libjass (PMS converts text tracks to ASS for its
// profile), so a request without a format gets ASS from the direct-play
// route and for Plex Web, SRT otherwise; format=ass|srt|vtt|webvtt picks
// one. Cues stay on the title
// clock: VOD video retains title timestamps across a resume or seek, and
// a direct-play player's clock is the file's. Embedded text tracks are read
// from the session's source; image subtitles and unknown sessions get an
// empty container.
func (s *Server) handleTranscodeSubtitles(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	pr := FromContext(r.Context())
	q := r.URL.Query()
	p, perr := s.parsePlayRequest(r)
	sessionID := p.sessionID
	if perr != nil {
		sessionID = sanitizeSessionID(cmp.Or(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID))
	}
	entry, ok := s.transcodes.get(sessionID)
	if !ok || pr.User == nil || entry.userID != pr.User.ID || (pr.ClientID != "" && entry.clientID != "" && entry.clientID != pr.ClientID) || entry.key == 0 || (perr == nil && p.key != entry.key) {
		s.writeEmpty(w, r)
		return
	}
	pos := entry.subtitle
	if perr == nil && q.Get("subtitleStreamID") != "" {
		resolvePlaySelection(&p, entry.info)
		pos = p.subSel
	}
	if pos < 0 || pos >= len(entry.info.Streams) {
		s.writeEmpty(w, r)
		return
	}
	st := entry.info.Streams[pos]
	if st.Type != "subtitle" {
		s.writeEmpty(w, r)
		return
	}
	format := subtitleFormat(q.Get("format"))
	if soft := q.Get("format") == "" && (strings.HasPrefix(r.URL.Path, "/subtitles/") || browserClient(pr)); soft || softASSFormat(q.Get("format")) {
		format = "ass"
	}
	if !st.External {
		if format == "ass" {
			if strings.HasPrefix(r.URL.Path, "/video/") && browserClient(pr) {
				s.serveSessionASS(w, r, sessionID, st.Index)
				return
			}
			// Read from where the player is (a direct play names its
			// position in offset; a conversion starts at its own offset),
			// streaming each cue as it is read.
			from := entry.offset
			if perr == nil && q.Get("offset") != "" {
				from = p.offset
			} else if ps, ok := s.d.Playback.SessionFor(entry.userID, entry.clientID, entry.key); ok && ps != nil && ps.ID == entry.playID && ps.ViewOffset != nil {
				// A conversion's player reopens the stream after a seek
				// without naming its position; its timeline has.
				from = time.Duration(*ps.ViewOffset) * time.Millisecond
			}
			if !s.writeEmbeddedASS(w, r, entry.playID, st, from) {
				s.writeEmpty(w, r)
			}
			return
		}
		// Embedded track: extract it from the playing source so a subtitle
		// picked in-player renders without restarting the session.
		if !s.writeEmbeddedSubtitle(w, r, entry.playID, st, format, entry.decision.Container == frontend.ContainerMatroska) {
			s.writeEmpty(w, r)
		}
		return
	}
	if !strings.HasPrefix(st.ExternalURL, "ether-subtitle:") {
		s.writeEmpty(w, r)
		return
	}
	timeline := s.d.Subtitles
	if timeline == nil {
		s.writeEmpty(w, r)
		return
	}
	title, err := s.d.Catalog.Get(r.Context(), entry.key)
	if err != nil || !s.visibleTitle(r.Context(), pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	source := ""
	if keyer, ok := s.d.Playback.(interface{ SourceKey(string) string }); ok {
		source = keyer.SourceKey(entry.playID)
	}
	var reader io.ReadCloser
	var actual string
	if from, ok := timeline.(interface {
		OpenFormatFrom(string, string, string) (io.ReadCloser, string, error)
	}); ok {
		reader, actual, err = from.OpenFormatFrom(st.ExternalURL, subtitleFormat(format), source)
	} else {
		reader, actual, err = timeline.OpenFormat(st.ExternalURL, subtitleFormat(format))
	}
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, 32<<20))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if format == "ass" {
		cues, perr := subtext.ParseCues(string(body))
		if perr != nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		body, actual = []byte(subtext.RenderASS(cues)), "ass"
	}
	if actual == "vtt" && entry.decision.Container == frontend.ContainerMatroska {
		body = stampMatroskaVTT(body)
	}
	w.Header().Set("Content-Type", subtitleContentTypes[actual])
	_, _ = w.Write(body)
}

// sessionSoftSubtitle asks the remux to carry the selected browser text
// subtitle through the same source read as the video.
func sessionSoftSubtitle(entry *transcodeEntry, pr *Request, dec frontend.Decision) *int {
	if entry == nil || !browserClient(pr) || dec.BurnSubtitles || entry.subtitle < 0 || entry.subtitle >= len(entry.info.Streams) {
		return nil
	}
	st := entry.info.Streams[entry.subtitle]
	if st.External || st.Type != "subtitle" || !frontend.IsTextSubtitle(st.Codec) {
		return nil
	}
	return &st.Index
}

func (s *Server) serveSessionASS(w http.ResponseWriter, r *http.Request, sessionID string, index int) {
	if s.d.Transcoder == nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	// The subtitle request can race the start request. Wait for the actual
	// encoder rather than launching another read of the source.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if session, ok := s.plexHLSSession(sessionID); ok {
			if ass, ok := session.(interface {
				ServeSoftSubtitle(http.ResponseWriter, *http.Request, int) bool
			}); ok && ass.ServeSoftSubtitle(w, r, index) {
				return
			}
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		case <-tick.C:
		}
	}
}

// softASSFormat reports an explicit request for an ASS script.
func softASSFormat(v string) bool {
	switch strings.ToLower(strings.TrimPrefix(v, ".")) {
	case "ass", "ssa":
		return true
	}
	return false
}

// subtitleStreamer is implemented by the transcoder: an embedded text
// track streamed as ASS from a position, and a whole track already read.
type subtitleStreamer interface {
	StreamSubtitle(ctx context.Context, inputURL string, streamIndex int, codec string, from time.Duration, w io.Writer) error
	CachedSubtitle(ctx context.Context, streamIndex int, codec string) ([]byte, bool)
}

// softSubtitleLead starts an ASS stream this far before the player's
// position, so the cue already on screen there is included.
const softSubtitleLead = 10 * time.Second

// writeEmbeddedASS answers an embedded text track as an ASS script. A
// track already read whole is answered at once. Otherwise the cues are
// streamed as the source is read from the player's position: the script
// header arrives within seconds and libjass starts drawing, later cues
// follow, and a blank line (skipped by ASS parsers) every
// subtitleKeepAlive holds the connection through gaps in the dialogue. It
// reports false, with nothing written, when the track cannot be read.
func (s *Server) writeEmbeddedASS(w http.ResponseWriter, r *http.Request, playID string, st frontend.StreamInfo, from time.Duration) bool {
	streamer, ok := s.d.Transcoder.(subtitleStreamer)
	if !ok || s.d.Playback == nil || playID == "" {
		return false
	}
	input := s.d.Playback.InputURL(playID)
	if input == "" {
		return false
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if keyer, ok := s.d.Playback.(interface{ SourceKey(string) string }); ok {
		ctx = frontend.WithSubtitleSource(ctx, keyer.SourceKey(playID))
	}
	if vtt, ok := streamer.CachedSubtitle(ctx, st.Index, st.Codec); ok {
		if cues, err := subtext.ParseCues(string(vtt)); err == nil {
			w.Header().Set("Content-Type", subtitleContentTypes["ass"])
			_, _ = io.WriteString(w, subtext.RenderASS(cues))
			return true
		}
	}
	from = max(0, from-softSubtitleLead)
	pr, pw := io.Pipe()
	defer pr.Close()
	done := make(chan error, 1)
	go func() {
		err := streamer.StreamSubtitle(ctx, input, st.Index, st.Codec, from, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		br := bufio.NewReader(pr)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	flusher, _ := w.(http.Flusher)
	started := false
	start := func() {
		if !started {
			started = true
			w.Header().Set("Content-Type", subtitleContentTypes["ass"])
			w.WriteHeader(http.StatusOK)
		}
	}
	write := func(b string) {
		start()
		_, _ = io.WriteString(w, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	quiet := time.NewTimer(subtitleQuietWait)
	defer quiet.Stop()
	alive := time.NewTicker(subtitleKeepAlive)
	defer alive.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				if !started {
					// Nothing was read: a failed start, or a track
					// with no cues from this position.
					if err := <-done; err != nil {
						return false
					}
					write("")
				}
				return true
			}
			write(line)
			alive.Reset(subtitleKeepAlive)
		case <-quiet.C:
			write("\n")
		case <-alive.C:
			if started {
				write("\n")
			}
		case <-r.Context().Done():
			return true
		}
	}
}

func (s *Server) extractEmbeddedSubtitle(ctx context.Context, playID string, st frontend.StreamInfo, format string) ([]byte, string, bool) {
	ex, ok := s.d.Transcoder.(frontend.SubtitleExtractor)
	if !ok || s.d.Playback == nil || playID == "" {
		return nil, "", false
	}
	input := s.d.Playback.InputURL(playID)
	if input == "" {
		return nil, "", false
	}
	if keyer, ok := s.d.Playback.(interface{ SourceKey(string) string }); ok {
		ctx = frontend.WithSubtitleSource(ctx, keyer.SourceKey(playID))
	}
	var buf bytes.Buffer
	if err := ex.ExtractSubtitle(ctx, input, st.Index, st.Codec, &buf); err != nil || buf.Len() == 0 {
		return nil, "", false
	}
	body := buf.Bytes()
	if format != "vtt" {
		// ExtractSubtitle always emits WebVTT; re-render as SRT.
		cues, perr := subtext.ParseCues(string(body))
		if perr != nil {
			return nil, "", false
		}
		body = []byte(subtext.RenderSRT(cues))
		format = "srt"
	}
	return body, format, true
}

func (s *Server) serveEmbeddedStream(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, info frontend.StreamInfo) {
	pr := FromContext(r.Context())
	if pr == nil || pr.User == nil || info.Type != "subtitle" || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var playID string
	var bestAt time.Time
	matroska := false
	for _, id := range s.transcodes.ids() {
		e, ok := s.transcodes.get(id)
		if !ok || e.userID != pr.User.ID || e.key != key || e.playID == "" {
			continue
		}
		if pr.ClientID != "" && e.clientID != "" && e.clientID != pr.ClientID {
			continue
		}
		if playID == "" || e.updatedAt.After(bestAt) {
			playID, bestAt = e.playID, e.updatedAt
			matroska = e.decision.Container == frontend.ContainerMatroska
		}
	}
	if playID == "" && pr.ClientID != "" {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil {
			playID = ps.ID
		}
	}
	if playID == "" {
		for _, ps := range s.d.Playback.Sessions() {
			if ps.UserID == pr.User.ID && ps.Key == key {
				playID = ps.ID
				break
			}
		}
	}
	q := r.URL.Query()
	format := subtitleFormat(q.Get("format"))
	if _, ext, ok := strings.Cut(chi.URLParam(r, "stream"), "."); ok && q.Get("format") == "" {
		format = subtitleFormat(ext)
	}
	if !s.writeEmbeddedSubtitle(w, r, playID, info, format, matroska) {
		writeHTTPError(w, http.StatusNotFound)
	}
}

// Extracting an embedded track reads the release from its first byte, and
// on a remote release of several GB that takes minutes. Players give up on
// a silent subtitle download long before that (ExoPlayer's read timeout is
// 8 s), which left picked subtitles blank (#191). A track that is not ready
// within subtitleQuietWait gets its response started at once, and a blank
// line every subtitleKeepAlive holds it open until the cues arrive. Blank
// lines are legal between WebVTT blocks and skipped by SRT readers.
var (
	subtitleQuietWait = 2 * time.Second
	subtitleKeepAlive = 4 * time.Second
)

// writeEmbeddedSubtitle serves an embedded text track as format (srt or
// vtt). It reports false, with nothing written, when extraction failed
// before the response had to start.
func (s *Server) writeEmbeddedSubtitle(w http.ResponseWriter, r *http.Request, playID string, st frontend.StreamInfo, format string, matroska bool) bool {
	type extracted struct {
		body   []byte
		actual string
		ok     bool
	}
	done := make(chan extracted, 1)
	go func() {
		body, actual, ok := s.extractEmbeddedSubtitle(r.Context(), playID, st, format)
		done <- extracted{body, actual, ok}
	}()
	finish := func(got extracted) []byte {
		if got.actual == "vtt" && matroska {
			return stampMatroskaVTT(got.body)
		}
		return got.body
	}
	quiet := time.NewTimer(subtitleQuietWait)
	defer quiet.Stop()
	select {
	case got := <-done:
		if !got.ok {
			return false
		}
		w.Header().Set("Content-Type", subtitleContentTypes[got.actual])
		_, _ = w.Write(finish(got))
		return true
	case <-quiet.C:
	case <-r.Context().Done():
		return true
	}
	// extractEmbeddedSubtitle answers WebVTT when asked for it, else SRT.
	actual := "srt"
	if format == "vtt" {
		actual = "vtt"
	}
	w.Header().Set("Content-Type", subtitleContentTypes[actual])
	w.WriteHeader(http.StatusOK)
	header := "\n"
	if actual == "vtt" {
		header = string(finish(extracted{body: []byte("WEBVTT\n\n"), actual: "vtt"}))
	}
	flusher, _ := w.(http.Flusher)
	write := func(b []byte) {
		_, _ = w.Write(b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	write([]byte(header))
	alive := time.NewTicker(subtitleKeepAlive)
	defer alive.Stop()
	for {
		select {
		case got := <-done:
			if got.ok {
				body := finish(got)
				if actual == "vtt" {
					body = vttCues(body)
				}
				write(body)
			}
			return true
		case <-alive.C:
			write([]byte("\n"))
		case <-r.Context().Done():
			return true
		}
	}
}

// vttCues drops a WebVTT header block (through its first blank line), for
// cues written after a header that was already sent.
func vttCues(body []byte) []byte {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(text, "WEBVTT") {
		return []byte(text)
	}
	if _, cues, ok := strings.Cut(text, "\n\n"); ok {
		return []byte(cues)
	}
	return nil
}
