package plex

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plex Home (/hubs, /hubs/promoted, /hubs/sections/{id}) is served from
// memory. A screen is built once per viewer and request shape, encoded, and
// stamped with the viewer's revision (frontend.HomeRevision: watch state,
// playlists, watchlist, favorites, exclusions, plus the policy epoch for a
// restricted viewer), the viewer's effective policy, the owner's settings
// and the Home layout. A
// request whose stamp matches is answered from the stored bytes with one
// revision read, whatever the library size; a different stamp is rebuilt on
// the request, so Continue Watching and policy changes are never late.
// Shared inputs (catalogue writes, the epoch for unrestricted viewers, the
// daypart, age) refresh the screen behind the response, one background
// build at a time. The key holds the viewer and the client's effective
// rendering profile, never the client identifier or session, so a second
// device or a reinstalled app of the same kind is served from the cache.
const (
	homeCacheMaxEntries = 256
	homeCacheMaxBytes   = 48 << 20
	// homeRefreshAfter: a screen this old is served and rebuilt behind it.
	homeRefreshAfter = time.Minute
	// homeProvisionalRefresh: a screen built before the viewer's
	// recommendations were ready is only reused this long.
	homeProvisionalRefresh = 5 * time.Second
	// homeMaxStale: older than this, a screen is rebuilt on the request.
	homeMaxStale = 30 * time.Minute
	// Screens used within homeKeepWarm are rebuilt in the background once
	// they are homeRewarmAge old, so a returning viewer is not cold.
	homeKeepWarm  = 2 * time.Hour
	homeRewarmAge = 25 * time.Minute
	// homeRefreshQueue bounds pending background rebuilds; one runs at a time.
	homeRefreshQueue   = 64
	homeRefreshTimeout = 30 * time.Second
	homeRewarmEvery    = time.Minute
)

// homeBuild is one built Home screen.
type homeBuild struct {
	mc *model.MediaContainer
	// served is the recommender's view of the written rows (impressions).
	served map[string]frontend.Hub
	// section is the personalization context's section (0 = Home).
	section int
	// provisional: the recommender had no rows yet.
	provisional bool
}

type homeBuildFunc func(ctx context.Context, r *http.Request) homeBuild

type homeEntry struct {
	key         string
	viewer      int64
	hard, soft  string
	body, gz    []byte
	offset      *int
	total       *int
	size        int64
	builtAt     time.Time
	usedAt      time.Time
	provisional bool
	impressions []homeImpression
	warm        frontend.RatingKey
	section     int
	build       homeBuildFunc
	tmpl        *http.Request // the latest request of this shape, detached
	pr          Request
	queued      bool
	flight      *homeFlight
}

type homeFlight struct {
	hard string
	done chan struct{}
}

type homeCache struct {
	mu      sync.Mutex
	entries map[string]*homeEntry
	bytes   int64
	queue   chan string
}

func newHomeCache() *homeCache {
	return &homeCache{entries: map[string]*homeEntry{}, queue: make(chan string, homeRefreshQueue)}
}

// serveHome answers a Home route from the viewer's cached screen, or builds,
// writes and stores it.
func (s *Server) serveHome(w http.ResponseWriter, r *http.Request, route string, build homeBuildFunc) {
	ctx := r.Context()
	pr := FromContext(ctx)
	if s.home == nil || pr == nil || pr.User == nil || s.liveSessionOnClient(pr) {
		s.writeHomeBuild(w, r, build(ctx, r))
		return
	}
	rev, ok, err := s.exp().HomeRevision(ctx, pr.User)
	if !ok || err != nil {
		s.writeHomeBuild(w, r, build(ctx, r))
		return
	}
	key := s.homeKey(route, r, pr)
	hard, soft := s.homeStamps(pr, rev)
	for waited := false; ; waited = true {
		now := time.Now()
		s.home.mu.Lock()
		e := s.home.entries[key]
		if e != nil && e.hard == hard && now.Sub(e.builtAt) < homeMaxStale && (!e.provisional || now.Sub(e.builtAt) < homeProvisionalRefresh) {
			e.usedAt = now
			e.tmpl, e.pr = detachedTemplate(r, pr)
			stale := e.soft != soft || now.Sub(e.builtAt) >= homeRefreshAfter
			body, gz, offset, total := e.body, e.gz, e.offset, e.total
			impressions, warm, section := e.impressions, e.warm, e.section
			s.home.mu.Unlock()
			if stale {
				s.queueHomeRefresh(key)
			}
			s.writeBody(w, r, http.StatusOK, body, gz, offset, total)
			s.recordCompactImpressions(pr, s.hubContext(pr, section), impressions)
			s.warmKey(ctx, pr, warm)
			return
		}
		if e != nil && e.flight != nil && e.flight.hard == hard && !waited {
			// A background rebuild for exactly this revision (after the
			// viewer's playback stopped) is running: wait for it.
			done := e.flight.done
			s.home.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return
			}
		}
		s.home.mu.Unlock()
		break
	}
	b := build(ctx, s.homeBuildRequest(r))
	body, err := s.encodeResponse(r, b.mc)
	if err != nil {
		s.log.Error("encode response", "path", r.URL.Path, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	gz := gzipBody(body)
	s.writeBody(w, r, http.StatusOK, body, gz, b.mc.Offset, b.mc.TotalSize)
	impressions := s.impressionHubs(pr, b.mc.Hub, b.served)
	s.recordCompactImpressions(pr, s.hubContext(pr, b.section), impressions)
	warm, _ := firstBrowseWarmKey(b.mc.Hub)
	s.warmKey(ctx, pr, warm)
	tmpl, prc := detachedTemplate(r, pr)
	s.storeHome(&homeEntry{key: key, viewer: pr.User.ID, hard: hard, soft: soft, body: body, gz: gz, offset: b.mc.Offset, total: b.mc.TotalSize,
		builtAt: time.Now(), usedAt: time.Now(), provisional: b.provisional, impressions: impressions, warm: warm, section: b.section,
		build: build, tmpl: tmpl, pr: prc})
}

// homeBuildRequest is r with the cache's build context.
func (s *Server) homeBuildRequest(r *http.Request) *http.Request {
	return r.WithContext(s.exp().NoProviderWait(r.Context()))
}

// writeHomeBuild writes an uncached build exactly as the handler always did.
func (s *Server) writeHomeBuild(w http.ResponseWriter, r *http.Request, b homeBuild) {
	pr := FromContext(r.Context())
	s.write(w, r, b.mc)
	s.recordImpressions(pr, s.hubContext(pr, b.section), b.mc.Hub, b.served)
	s.warmBrowseLink(r.Context(), pr, b.mc.Hub)
}

// liveSessionOnClient reports a live playback session of this viewer on
// this client: its rows render the session's probed media, so they are
// built fresh and not stored.
func (s *Server) liveSessionOnClient(pr *Request) bool {
	if s.d.Playback == nil || pr.ClientID == "" {
		return false
	}
	for _, ps := range s.d.Playback.Sessions() {
		if ps != nil && ps.UserID == pr.User.ID && ps.ClientID == pr.ClientID {
			return true
		}
	}
	return false
}

// homeKey is the viewer, route and everything of the request that shapes
// the response, except the client's identity: the query (minus credentials
// and client identity), the response format and page, and the effective
// client profile the media estimates are drawn for.
func (s *Server) homeKey(route string, r *http.Request, pr *Request) string {
	q := url.Values{}
	for k, vs := range r.URL.Query() {
		switch strings.ToLower(k) {
		case "x-plex-token", "x-plex-client-identifier", "x-plex-session-identifier", "x-plex-device-name":
			continue
		}
		vs = append([]string(nil), vs...)
		sort.Strings(vs)
		q[k] = vs
	}
	caps := s.clientCaps(pr)
	caps.ClientID, caps.DeviceName = "", ""
	capsJSON, _ := json.Marshal(caps)
	h := sha256.New()
	// Section screens contain client-scoped generation URLs.
	if strings.HasPrefix(route, "hubs/sections/") {
		h.Write([]byte(pr.ClientID))
		h.Write([]byte{0})
	}
	for _, part := range []string{route, strconv.FormatInt(pr.User.ID, 10), q.Encode(), strconv.Itoa(int(pr.Format)),
		strconv.Itoa(pr.Start), strconv.Itoa(pr.Size), strconv.FormatBool(pr.HasPage), strconv.FormatBool(pr.Remote),
		pr.Product, pr.Version, pr.Platform, pr.PlatformVersion, pr.Device, pr.Model, pr.Language, string(capsJSON)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// homeStamps returns the hard stamp (a mismatch rebuilds on the request)
// and the soft one (a mismatch refreshes behind the response).
func (s *Server) homeStamps(pr *Request, rev frontend.HomeRevision) (string, string) {
	restricted := s.viewerRestricted(pr.User)
	userJSON, _ := json.Marshal(pr.User)
	var layout, config, catalog uint64
	if s.pageFreshness != nil {
		f := s.pageFreshness()
		layout, config, catalog = f.Layout, f.Config, f.Catalog
	}
	hard := rev.Viewer + "|" + strconv.FormatUint(layout, 36) + "|" + strconv.FormatUint(config, 36) + "|" + strconv.FormatBool(restricted) + "|" + strconv.FormatBool(pr.User.Delegated) + "|" + string(userJSON)
	soft := strconv.FormatUint(catalog, 36)
	if restricted {
		hard += "|" + strconv.FormatInt(rev.Policy, 36)
	} else {
		soft += "|" + strconv.FormatInt(rev.Policy, 36)
	}
	hc := s.hubContext(pr, 0)
	soft += "|" + hc.Daypart + "|" + strconv.FormatBool(hc.Weekend) + "|" + hc.Now.Format("2006-01-02T15")
	return hard, soft
}

// detachedTemplate keeps what a background rebuild needs from a request:
// its URL and headers, without its body or context.
func detachedTemplate(r *http.Request, pr *Request) (*http.Request, Request) {
	t := &http.Request{Method: http.MethodGet, URL: r.URL, Header: r.Header.Clone(), Host: r.Host, RemoteAddr: r.RemoteAddr, Proto: r.Proto, ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor}
	return t, *pr
}

func (s *Server) storeHome(e *homeEntry) {
	e.size = int64(len(e.body)+len(e.gz)) + 2048
	for _, r := range e.impressions {
		e.size += int64(len(r.id)+len(r.title)) + 8*int64(len(r.keys)) + 64
	}
	c := s.home
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[e.key]; old != nil {
		c.bytes -= old.size
		e.queued = old.queued
		if old.flight != nil {
			e.flight = old.flight
		}
		delete(c.entries, e.key)
	}
	if e.size > homeCacheMaxBytes/8 {
		return
	}
	for len(c.entries) >= homeCacheMaxEntries || c.bytes+e.size > homeCacheMaxBytes {
		var victim *homeEntry
		for _, o := range c.entries {
			if victim == nil || o.usedAt.Before(victim.usedAt) {
				victim = o
			}
		}
		if victim == nil {
			break
		}
		c.bytes -= victim.size
		delete(c.entries, victim.key)
	}
	c.entries[e.key] = e
	c.bytes += e.size
}

func (s *Server) queueHomeRefresh(key string) {
	c := s.home
	c.mu.Lock()
	e := c.entries[key]
	if e == nil || e.queued {
		c.mu.Unlock()
		return
	}
	e.queued = true
	c.mu.Unlock()
	select {
	case c.queue <- key:
	default:
		c.mu.Lock()
		if e := c.entries[key]; e != nil {
			e.queued = false
		}
		c.mu.Unlock()
	}
}

// ViewerHomeChanged rebuilds the viewer's recently used Home screens in the
// background, so the next Home after playback stops is already current.
func (s *Server) ViewerHomeChanged(viewerID int64) {
	if s == nil || s.home == nil {
		return
	}
	now := time.Now()
	var keys []string
	s.home.mu.Lock()
	for k, e := range s.home.entries {
		if e.viewer == viewerID && now.Sub(e.usedAt) < homeKeepWarm {
			keys = append(keys, k)
		}
	}
	s.home.mu.Unlock()
	for _, k := range keys {
		s.queueHomeRefresh(k)
	}
}

// homeRefresher runs background rebuilds one at a time and keeps recently
// used screens warm.
func (s *Server) homeRefresher() {
	defer s.wg.Done()
	t := time.NewTicker(homeRewarmEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case key := <-s.home.queue:
			s.refreshHome(key)
		case <-t.C:
			now := time.Now()
			var keys []string
			s.home.mu.Lock()
			for k, e := range s.home.entries {
				if now.Sub(e.usedAt) < homeKeepWarm && now.Sub(e.builtAt) >= homeRewarmAge {
					keys = append(keys, k)
				}
			}
			s.home.mu.Unlock()
			for _, k := range keys {
				s.queueHomeRefresh(k)
			}
		}
	}
}

func (s *Server) refreshHome(key string) {
	c := s.home
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		c.mu.Unlock()
		return
	}
	e.queued = false
	build, tmpl, prc, section := e.build, e.tmpl, e.pr, e.section
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, homeRefreshTimeout)
	defer cancel()
	ctx = withRequest(ctx, &prc)
	if s.plexMetaOn() {
		ctx = frontend.WithPlexSaferRating(ctx)
	}
	ctx = reqmemo.With(ctx)
	pr := FromContext(ctx)
	rev, ok, err := s.exp().HomeRevision(ctx, pr.User)
	if !ok || err != nil {
		return
	}
	hard, soft := s.homeStamps(pr, rev)
	flight := &homeFlight{hard: hard, done: make(chan struct{})}
	c.mu.Lock()
	if cur := c.entries[key]; cur != nil && cur.flight == nil {
		cur.flight = flight
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if cur := c.entries[key]; cur != nil && cur.flight == flight {
			cur.flight = nil
		}
		c.mu.Unlock()
		close(flight.done)
	}()
	r := s.homeBuildRequest(tmpl.WithContext(ctx))
	b := build(r.Context(), r)
	if ctx.Err() != nil {
		return
	}
	body, err := s.encodeResponse(r, b.mc)
	if err != nil {
		return
	}
	impressions := s.impressionHubs(pr, b.mc.Hub, b.served)
	warm, _ := firstBrowseWarmKey(b.mc.Hub)
	c.mu.Lock()
	usedAt := time.Now()
	if cur := c.entries[key]; cur != nil {
		usedAt = cur.usedAt
		tmpl, prc = cur.tmpl, cur.pr
	}
	c.mu.Unlock()
	s.storeHome(&homeEntry{key: key, viewer: pr.User.ID, hard: hard, soft: soft, body: body, gz: gzipBody(body), offset: b.mc.Offset, total: b.mc.TotalSize,
		builtAt: time.Now(), usedAt: usedAt, provisional: b.provisional, impressions: impressions, warm: warm, section: section,
		build: build, tmpl: tmpl, pr: prc})
}

// gzipBody precompresses a stored body the way writeBody would.
func gzipBody(body []byte) []byte {
	if len(body) <= 1024 {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(body)
	_ = gz.Close()
	return buf.Bytes()
}

// homeImpression is one written row as recordImpressions reports it: the
// row's identity and its item keys (the recommender reads nothing else).
type homeImpression struct {
	id, title string
	keys      []frontend.RatingKey
}

// impressionHubs is what recordImpressions reports for the written rows.
func (s *Server) impressionHubs(pr *Request, written []model.Hub, served map[string]frontend.Hub) []homeImpression {
	if _, ok := s.recommender().(frontend.ImpressionSink); !ok || pr.User == nil {
		return nil
	}
	out := make([]homeImpression, 0, len(written))
	for _, mh := range written {
		h, ok := served[mh.HubIdentifier]
		if !ok {
			continue
		}
		items := s.filterAllowed(pr.User, h.Items)
		keys := make([]frontend.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		out = append(out, homeImpression{id: h.ID, title: h.Title, keys: keys})
	}
	return out
}

func (s *Server) recordCompactImpressions(pr *Request, hc frontend.HubContext, rows []homeImpression) {
	sink, ok := s.recommender().(frontend.ImpressionSink)
	if !ok || pr.User == nil || len(rows) == 0 {
		return
	}
	hubs := make([]frontend.Hub, len(rows))
	for i, r := range rows {
		items := make([]frontend.Title, len(r.keys))
		for j, k := range r.keys {
			items[j].Key = k
		}
		hubs[i] = frontend.Hub{ID: r.id, Title: r.title, Items: items}
	}
	sink.RecordImpressions(pr.User, hc, hubs)
}

func (s *Server) warmKey(ctx context.Context, pr *Request, key frontend.RatingKey) {
	if key == 0 || pr == nil || pr.User == nil {
		return
	}
	if pw, ok := s.d.Playback.(frontend.Prewarmer); ok {
		pw.Prewarm(frontend.DetachQueryStats(context.WithoutCancel(ctx)), pr.User, key, s.clientCaps(pr), frontend.PrewarmOptions{Reason: frontend.PrewarmBrowse})
	}
}

// BackgroundBusy is how many Home screens are queued for or being rebuilt
// behind a response: the performance gate waits for none before each
// measured request.
func (s *Server) BackgroundBusy() int {
	if s.home == nil {
		return 0
	}
	c := s.home
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.queue)
	for _, e := range c.entries {
		if e.queued || e.flight != nil {
			n++
		}
	}
	return n
}
