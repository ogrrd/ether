package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Offline downloads (PMS Download Queue API). Each queue belongs to one
// (user, client); items move deciding -> waiting -> processing ->
// available as the title is resolved with caps.Download set and its
// streams are copied to a temporary faststart MP4 by Deps.Downloads.
// Queue state is persisted asynchronously when a database is wired. Completed
// files remain subject to the configured retention and disk budget.

const (
	downloadDeciding   = "deciding"
	downloadWaiting    = "waiting"
	downloadProcessing = "processing"
	downloadAvailable  = "available"
	downloadError      = "error"
	downloadExpired    = "expired"
	downloadDone       = "done" // queue-level only
)

// downloadSubscribeFeature is the library provider's download capability
// for Plex's desktop apps (Plex for Mac/Windows, Plex HTPC). Their web
// client (Plex Web 4.163 showSyncDownload) shows Download only when the
// item's provider has features.subscribe.flavor == "download" and, for a
// library provider, the server's allowSync is true; it reads nothing else
// from this feature. The download itself is a media subscription created
// on the app's embedded server (state.embeddedServer, not this one), which
// then pulls the title from this server like any remote PMS client:
// metadata, universal decision, then the part or a start stream. No key
// is advertised because no client calls one for this flavor; the Live TV
// DVR keeps /media/subscriptions.
var downloadSubscribeFeature = model.Feature{Flavor: "download", Type: "subscribe"}

// downloadsEnabled reports whether the server can serve downloads at
// all: owner setting on, remuxer and playback wired.
func (s *Server) downloadsEnabled() bool {
	return s.cfg().Downloads.Enabled && s.d.Downloads != nil && s.d.Playback != nil
}

// downloadsAllowed reports whether u may download (and so sees
// allowSync="1"): the owner always, friends only with the owner's
// AllowFriends setting unless the access policy decides per user.
func (s *Server) downloadsAllowed(u *frontend.User) bool {
	return frontend.DownloadsAllowed(u, s.downloadsEnabled(), s.cfg().Downloads.AllowFriends, s.policy())
}

// DownloadsAllowed exposes the same admission decision for informational
// frontend capability reporting; the download handler still enforces it.
func (s *Server) DownloadsAllowed(u *frontend.User) bool { return s.downloadsAllowed(u) }

func (s *Server) requireDownloads(w http.ResponseWriter, r *http.Request) bool {
	if !s.downloadsEnabled() {
		writeHTTPError(w, http.StatusNotFound)
		return false
	}
	if !s.downloadsAllowed(FromContext(r.Context()).User) {
		writeHTTPError(w, http.StatusForbidden)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Store

type downloadItem = frontend.DownloadItem
type downloadQueue = frontend.DownloadQueue
type downloadStore = frontend.DownloadQueueStore

func queueStatus(q *downloadQueue) string {
	seen := map[string]bool{}
	for _, it := range q.Items {
		seen[it.Status] = true
	}
	for _, st := range []string{downloadDeciding, downloadProcessing, downloadWaiting, downloadError} {
		if seen[st] {
			return st
		}
	}
	return downloadDone
}

// ---------------------------------------------------------------------------
// Wire models (components/schemas DownloadQueue, DownloadQueueItem,
// AddedQueueItem and DecisionResult of the PMS OpenAPI spec).

type downloadQueueModel struct {
	ID        int64  `plex:"id"`
	ItemCount int    `plex:"itemCount"`
	Status    string `plex:"status"`
}

type addedQueueItemModel struct {
	ID  int64  `plex:"id"`
	Key string `plex:"key"`
}

type decisionResultModel struct {
	AvailableBandwidth     int    `plex:"availableBandwidth,omitempty"`
	DirectPlayDecisionCode int    `plex:"directPlayDecisionCode"`
	DirectPlayDecisionText string `plex:"directPlayDecisionText"`
	GeneralDecisionCode    int    `plex:"generalDecisionCode"`
	GeneralDecisionText    string `plex:"generalDecisionText"`
	MDEDecisionCode        int    `plex:"mdeDecisionCode"`
	MDEDecisionText        string `plex:"mdeDecisionText"`
	TranscodeDecisionCode  int    `plex:"transcodeDecisionCode"`
	TranscodeDecisionText  string `plex:"transcodeDecisionText"`
}

type downloadQueueItemModel struct {
	ID       int64                `plex:"id"`
	QueueID  int64                `plex:"queueId"`
	Key      string               `plex:"key"`
	Status   string               `plex:"status"`
	Error    string               `plex:"error,omitempty"`
	Decision *decisionResultModel `plex:"DecisionResult,object"`
	Session  *model.TranscodeSess `plex:"TranscodeSession,object"`
}

type downloadContainer struct {
	Size       int    `plex:"size"`
	Identifier string `plex:"identifier,omitempty"`

	Queue []downloadQueueModel     `plex:"DownloadQueue"`
	Items []downloadQueueItemModel `plex:"DownloadQueueItem"`
	Added []addedQueueItemModel    `plex:"AddedQueueItems"`
}

func keyPath(k frontend.RatingKey) string { return "/library/metadata/" + k.String() }

// itemModel renders one queue item. Downloads are converted or remuxed to MP4.
func itemModel(it *downloadItem) downloadQueueItemModel {
	m := downloadQueueItemModel{
		ID: it.ID, QueueID: it.QueueID, Key: keyPath(it.Key), Status: it.Status, Error: it.Error,
	}
	dr := &decisionResultModel{AvailableBandwidth: it.Bandwidth}
	text := "Direct play not available; Conversion to MP4 OK."
	switch it.Status {
	case downloadError:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeDirectPlayError, it.Error
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeGeneralError, it.Error
		dr.MDEDecisionCode, dr.MDEDecisionText = codeGeneralError, it.Error
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeTranscodeError, it.Error
	case downloadDeciding:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeGeneralError, "Deciding."
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeGeneralError, "Deciding."
		dr.MDEDecisionCode, dr.MDEDecisionText = codeGeneralError, "Deciding."
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeGeneralError, "Deciding."
	default:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeDirectPlayError, text
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeConversionOK, text
		dr.MDEDecisionCode, dr.MDEDecisionText = codeConversionOK, text
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeConversionOK, text
	}
	m.Decision = dr
	if it.Status != downloadDeciding {
		m.Session = &model.TranscodeSess{
			Key:              fmt.Sprintf("/downloadQueue/%d/item/%d", it.QueueID, it.ID),
			Complete:         it.Status == downloadAvailable,
			Progress:         it.Progress * 100,
			Duration:         ms(it.Duration),
			Context:          "static",
			SourceVideoCodec: it.VideoCodec,
			SourceAudioCodec: it.AudioCodec,
			VideoDecision:    "copy",
			AudioDecision:    "copy",
			Protocol:         "http",
			Container:        "mp4",
			VideoCodec:       it.VideoCodec,
			AudioCodec:       it.AudioCodec,
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Handlers

// downloadQueueParam resolves {queue} for the requesting user/client.
func (s *Server) downloadQueueParam(w http.ResponseWriter, r *http.Request) *downloadQueue {
	pr := FromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "queue"), 10, 64)
	if err != nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil
	}
	q := s.downloads.QueueByID(id, pr.User.ID, pr.ClientID)
	if q == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil
	}
	return q
}

func (s *Server) writeQueue(w http.ResponseWriter, r *http.Request, q *downloadQueue) {
	s.downloads.Lock()
	m := downloadQueueModel{ID: q.ID, ItemCount: len(q.Items), Status: queueStatus(q)}
	s.downloads.Unlock()
	s.write(w, r, &downloadContainer{Size: 1, Queue: []downloadQueueModel{m}})
}

// POST /downloadQueue — get or create this client+user's queue.
func (s *Server) handleDownloadQueueCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	pr := FromContext(r.Context())
	q := s.downloads.QueueFor(pr.User.ID, pr.ClientID, true)
	s.stageDownloads()
	s.writeQueue(w, r, q)
}

// GET /downloadQueue/{queue}
func (s *Server) handleDownloadQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	if q := s.downloadQueueParam(w, r); q != nil {
		s.writeQueue(w, r, q)
	}
}

// parseDownloadKey accepts "/library/metadata/123[/children]" or a bare
// rating key.
func parseDownloadKey(s string) (frontend.RatingKey, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "metadata/"); i >= 0 {
		s = s[i+len("metadata/"):]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return frontend.ParseRatingKey(s)
}

// syncVideoQualities maps the legacy videoQuality index clients send to
// (max bitrate kbps, max height); the table is PMS's (see python-plexapi
// sync.VIDEO_QUALITIES).
var syncVideoQualities = []struct{ kbps, height int }{
	{64, 128}, {96, 128}, {208, 160}, {320, 240}, {720, 320}, {1500, 480},
	{2000, 720}, {3000, 720}, {4000, 720}, {8000, 1080}, {10000, 1080},
	{12000, 1080}, {20000, 1080},
}

// downloadCaps builds the caps an add request asked for.
func (s *Server) downloadCaps(r *http.Request, pr *Request) frontend.ClientCaps {
	caps := s.declaredCaps(pr)
	q := r.URL.Query()
	if v := q.Get("videoResolution"); v != "" {
		if _, h, ok := strings.Cut(v, "x"); ok {
			if n, err := strconv.Atoi(h); err == nil && n > 0 {
				caps.MaxHeight = n
			}
		}
	}
	for _, name := range []string{"maxVideoBitrate", "videoBitrate"} {
		if v := q.Get(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.MaxBitrateKbps = n
				break
			}
		}
	}
	if caps.MaxBitrateKbps == 0 && q.Get("videoResolution") == "" {
		if v := q.Get("videoQuality"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(syncVideoQualities) {
				caps.MaxBitrateKbps = syncVideoQualities[n].kbps
				caps.MaxHeight = syncVideoQualities[n].height
			}
		}
	}
	if mh := s.cfg().Downloads.MaxHeight; mh > 0 && (caps.MaxHeight == 0 || caps.MaxHeight > mh) {
		caps.MaxHeight = mh
	}
	return caps
}

// expandDownloadKey turns a show/season key into its episodes; playable
// keys pass through. Returns titles the user may see.
func (s *Server) expandDownloadKey(ctx context.Context, u *frontend.User, k frontend.RatingKey) ([]frontend.Title, error) {
	t, err := s.d.Catalog.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if !s.visibleTitle(ctx, u, t) {
		return nil, frontend.ErrNotFound
	}
	switch t.Type {
	case frontend.TypeMovie, frontend.TypeEpisode:
		return []frontend.Title{*t}, nil
	case frontend.TypeSeason:
		kids, err := s.d.Catalog.Children(ctx, k)
		if err != nil {
			return nil, err
		}
		return s.visibleTitles(u, kids), nil
	case frontend.TypeShow:
		eps, err := s.d.Catalog.Grandchildren(ctx, k)
		if err != nil {
			return nil, err
		}
		return s.visibleTitles(u, eps), nil
	}
	return nil, frontend.ErrNotFound
}

// POST /downloadQueue/{queue}/add?keys=...
func (s *Server) handleDownloadQueueAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	pr := FromContext(r.Context())
	ctx := r.Context()
	raw := r.URL.Query()["keys"]
	if len(raw) == 0 {
		raw = r.URL.Query()["key"]
	}
	var keys []frontend.RatingKey
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			k, err := parseDownloadKey(part)
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	caps := s.downloadCaps(r, pr)
	burn := r.URL.Query().Get("subtitles") == "burn"
	added := []addedQueueItemModel{}
	for _, k := range keys {
		titles, err := s.expandDownloadKey(ctx, pr.User, k)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		for _, t := range titles {
			it := s.addDownloadItem(q, pr.User, t, caps, burn)
			added = append(added, addedQueueItemModel{ID: it.ID, Key: keyPath(t.Key)})
		}
	}
	s.scheduleDownloads()
	s.write(w, r, &downloadContainer{Size: len(added), Added: added})
}

// addDownloadItem appends an item, or returns the existing one when the
// same title is already queued with the same parameters (PMS dedupes).
func (s *Server) addDownloadItem(q *downloadQueue, u *frontend.User, t frontend.Title, caps frontend.ClientCaps, burn bool) *downloadItem {
	return s.downloads.Add(q, u, t, caps, burn)
}

// parseItemIDs parses "{item}" path params and ?ids= lists; PMS allows
// comma-separated ids in one segment.
func parseItemIDs(r *http.Request) []int64 {
	raw := chi.URLParam(r, "item")
	if raw == "" {
		raw = r.URL.Query().Get("ids")
	}
	var out []int64
	for _, p := range strings.Split(raw, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) writeDownloadItems(w http.ResponseWriter, r *http.Request, q *downloadQueue, only map[int64]bool) {
	s.downloads.Lock()
	items := []downloadQueueItemModel{}
	for _, it := range q.Items {
		if only != nil && !only[it.ID] {
			continue
		}
		items = append(items, itemModel(it))
	}
	s.downloads.Unlock()
	s.write(w, r, &downloadContainer{Size: len(items), Items: items})
}

// GET /downloadQueue/{queue}/items
func (s *Server) handleDownloadQueueItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	if q := s.downloadQueueParam(w, r); q != nil {
		s.writeDownloadItems(w, r, q, nil)
	}
}

// GET /downloadQueue/{queue}/items/{item}
func (s *Server) handleDownloadQueueItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	only := map[int64]bool{}
	for _, id := range parseItemIDs(r) {
		only[id] = true
	}
	s.writeDownloadItems(w, r, q, only)
}

// DELETE /downloadQueue/{queue}/items/{item} — cancel and clean up.
func (s *Server) handleDownloadQueueItemDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	ids := parseItemIDs(r)
	if len(ids) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	removed := s.downloads.Remove(q, ids)
	for _, it := range removed {
		s.cancelDownloadItem(it)
	}
	s.scheduleDownloads()
	w.WriteHeader(http.StatusNoContent)
}

// POST /downloadQueue/{queue}/items/{item}/restart — reprocess with the
// original decision parameters.
func (s *Server) handleDownloadQueueItemRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	s.downloads.Restart(q, parseItemIDs(r), s.d.Downloads.ReleaseDownload)
	s.scheduleDownloads()
	w.WriteHeader(http.StatusOK)
}

// cancelDownloadItem stops an item's remux (if running) and releases its
// finished file (if any).
func (s *Server) cancelDownloadItem(it *downloadItem) {
	p := s.downloads.CancelItem(it)
	if p != "" && s.d.Downloads != nil {
		s.d.Downloads.ReleaseDownload(p)
	}
}

// downloadItemParam resolves queue+item for the single-item endpoints.
func (s *Server) downloadItemParam(w http.ResponseWriter, r *http.Request) (*downloadQueue, *downloadItem) {
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return nil, nil
	}
	ids := parseItemIDs(r)
	if len(ids) != 1 {
		writeHTTPError(w, http.StatusBadRequest)
		return nil, nil
	}
	s.downloads.Lock()
	it := q.Item(ids[0])
	s.downloads.Unlock()
	if it == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, nil
	}
	return q, it
}

// GET /downloadQueue/{queue}/item/{item}/decision — the playback-style
// decision for the item's chosen media.
func (s *Server) handleDownloadItemDecision(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	_, it := s.downloadItemParam(w, r)
	if it == nil {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	t, err := s.d.Catalog.Get(ctx, it.Key)
	if err == nil && !s.visibleTitle(ctx, pr.User, t) {
		err = frontend.ErrNotFound // restrictions can change after queueing
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.downloads.Lock()
	m := itemModel(it)
	s.downloads.Unlock()
	mc := s.itemContainer(t.Type)
	md := s.metadata(ctx, pr, t, renderOpts{detail: true, media: true})
	decisionItem(&md)
	mc.Metadata = []model.Metadata{md}
	mc.Size = 1
	dr := m.Decision
	mc.Extra = []model.Attr{
		model.A("directPlayDecisionCode", dr.DirectPlayDecisionCode),
		model.A("directPlayDecisionText", dr.DirectPlayDecisionText),
		model.A("generalDecisionCode", dr.GeneralDecisionCode),
		model.A("generalDecisionText", dr.GeneralDecisionText),
		model.A("transcodeDecisionCode", dr.TranscodeDecisionCode),
		model.A("transcodeDecisionText", dr.TranscodeDecisionText),
		model.A("mdeDecisionCode", dr.MDEDecisionCode),
		model.A("mdeDecisionText", dr.MDEDecisionText),
	}
	s.write(w, r, mc)
}

// GET /downloadQueue/{queue}/item/{item}/media — the remuxed MP4 with
// Range support; 503 + Retry-After while it is still being produced.
func (s *Server) handleDownloadItemMedia(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	_, it := s.downloadItemParam(w, r)
	if it == nil {
		return
	}
	s.downloads.Lock()
	status, p, readyAt, title, key := it.Status, it.Path, it.ReadyAt, it.Title, it.Key
	s.downloads.Unlock()
	// Restrictions can change after an item was queued: serve the cached
	// file only while the viewer may still see the title.
	if t, err := s.d.Catalog.Get(r.Context(), key); err != nil || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	switch status {
	case downloadDeciding, downloadWaiting, downloadProcessing:
		w.Header().Set("Retry-After", "-1")
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	case downloadExpired:
		writeHTTPError(w, http.StatusGone)
		return
	case downloadError:
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeHTTPError(w, http.StatusGone)
		return
	}
	defer f.Close()
	name := downloadFilename(title)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	cw := s.newCountWriter(w, r)
	http.ServeContent(cw, r, name, readyAt, f)
	cw.finish()
}

// downloadFilename is the client-visible file name for a download.
func downloadFilename(title string) string {
	if title == "" {
		title = "download"
	}
	var b strings.Builder
	for _, c := range title {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == ' ', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	return strings.TrimSpace(b.String()) + ".mp4"
}

func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename*=UTF-8''%s; filename=\"%s\"",
		pathEscape(strings.NewReplacer("\r", "", "\n", "").Replace(name)), strings.NewReplacer("+", "_", "\"", "_", "\r", "", "\n", "", "\\", "_").Replace(name))
}

func pathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Processing

// scheduleDownloads starts queued items up to the per-user cap. The
// remuxer enforces the server-wide cap.
func (s *Server) scheduleDownloads() {
	defer s.stageDownloads()
	jobs := s.downloads.StartReady(s.ctx, s.cfg().Downloads.MaxConcurrentPerUser, s.downloadsAllowed)
	for _, job := range jobs {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.processDownloadItem(job.Context, job.Item, job.Run) }()
	}
}

func (s *Server) processDownloadItem(ctx context.Context, it *downloadItem, run int64) {
	defer s.scheduleDownloads()
	fail := func(msg string) { s.downloads.Fail(ctx, it, msg) }

	noRelease := func(err error) bool {
		return err != nil && errors.Is(err, frontend.ErrNotAvailable) && playbackMessage(err) == msgNotAvailable
	}
	job := s.d.SessionStores.NewDownloadJob(s.d.Playback, s.d.Downloads)
	p, size, err := job.Run(ctx, frontend.DownloadRequest{Viewer: it.User, Item: it.Key, Caps: it.Caps, Run: run, NoRelease: noRelease,
		Prepared: func(ps *frontend.PlaySession) (frontend.DecideOptions, error) {
			if !s.downloads.Prepared(ctx, it, ps.Info) {
				return frontend.DecideOptions{}, ctx.Err()
			}

			var viewerID int64
			if it.User != nil {
				viewerID = it.User.ID
			}
			subtitle := -1
			sel := resolveSelection(s.selections.get(viewerID, int64(it.Key)), ps.Info)
			if title, err := s.d.Catalog.Get(ctx, it.Key); err == nil && title != nil {
				sel = s.preferredSelection(ctx, it.User, it.Key, ps.Info, title.Language, sel)
			}
			if sel.subtitleSet && sel.subtitle >= 0 && sel.subtitle < len(ps.Info.Streams) {
				st := ps.Info.Streams[sel.subtitle]
				if st.Type == "subtitle" && !st.External {
					subtitle = st.Index
				}
			}
			return frontend.DecideOptions{
				DirectPlay: false, DirectStream: true,
				MaxBitrateKbps: it.Bandwidth, MaxHeight: it.Caps.MaxHeight, AudioStream: -1, SubtitleStream: subtitle,
				BurnSubtitles: it.Burn,
			}, nil
		}, Progress: func(f float64) {
			s.downloads.Progress(ctx, it, f)
		}})

	if err != nil {
		var preparation *frontend.DownloadPreparationError
		if errors.As(err, &preparation) {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("download preparation failed", "key", it.Key, "err", err)
			if noRelease(err) {
				fail("No release available for download at this quality.")
			} else {
				fail(playbackMessage(err))
			}
			return
		}
		if ctx.Err() == nil {
			s.log.Warn("download processing failed", "key", it.Key, "err", err)
		}
		switch {
		case ctx.Err() != nil:
			// Deleted, restarted or shutting down; state already reset.
		case errors.Is(err, frontend.ErrDownloadNeedsTranscode):
			fail("This title cannot be downloaded: it cannot be converted to a playable MP4.")
		case errors.Is(err, frontend.ErrDownloadBudget):
			fail("Not enough temporary disk space on the server for this download.")
		default:
			fail(msgPlayFailed)
		}
		return
	}
	if !s.downloads.Complete(ctx, it, p, size, s.now()) {
		s.d.Downloads.ReleaseDownload(p)
		return
	}
	s.retainDownload(p, it.ReadyAt)
}

// expireDownloads frees finished downloads older than the configured
// retention; their items stay listed as expired until deleted.
func (s *Server) expireDownloads(now time.Time) {
	if s.downloads == nil {
		return
	}
	paths := s.downloads.Expire(now, s.cfg().Downloads.Expiry.D())
	if len(paths) > 0 {
		s.stageDownloads()
	}
	if s.d.Downloads != nil {
		for _, p := range paths {
			s.d.Downloads.ReleaseDownload(p)
		}
	}
}

// releaseAllDownloads cancels in-flight remuxes and deletes every temp
// file (server shutdown).
func (s *Server) releaseAllDownloads() {
	if s.downloads == nil {
		return
	}
	items := s.downloads.AllItems()
	for _, it := range items {
		s.cancelDownloadItem(it)
	}
}
