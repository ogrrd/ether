package plex

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func queueFlag(q url.Values, name string) (bool, bool) {
	if !q.Has(name) {
		return false, true
	}
	raw := q.Get(name)
	digits := strings.TrimPrefix(raw, "+")
	if digits == "" {
		return false, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false, false
		}
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	return n == 1, err == nil && n <= 1
}

func (s *Server) handlePlayQueueCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	if pr.User == nil {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	shuffle, valid := queueFlag(q, "shuffle")
	_, repeatValid := queueFlag(q, "repeat")
	if !valid || !repeatValid || !validQueueWindow(q) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if t := q.Get("type"); t != "video" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	var keys []frontend.RatingKey
	var plKey frontend.RatingKey
	var plTitle string
	uri := q.Get("uri")
	switch {
	case q.Get("playlistID") != "":
		k, err := frontend.ParseRatingKey(q.Get("playlistID"))
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		pl, err := s.playlist(ctx, pr.User, k)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		keys, err = s.expandKey(ctx, pr, pl.Key)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		plKey, plTitle = pl.Key, pl.Title
		if uri == "" {
			uri = "library://playlist/item/%2Fplaylists%2F" + pl.Key.String()
		}
	case uri != "":
		var err error
		keys, err = s.continuousKeys(ctx, pr, uri, q.Get("continuous") == "1")
		if path, e := uriPath(uri); e == nil {
			if m := rePlaylistPath.FindStringSubmatch(path); len(m) > 0 {
				if k, e := frontend.ParseRatingKey(m[1]); e == nil {
					if pl, e := s.playlist(ctx, pr.User, k); e == nil {
						plKey, plTitle = pl.Key, pl.Title
					}
				}
			}
		}
		if err != nil {
			s.log.Debug("play queue uri", "uri", uri, "err", err)
			if errors.Is(err, frontend.ErrNotFound) {
				writeHTTPError(w, http.StatusNotFound)
			} else {
				writeHTTPError(w, http.StatusBadRequest)
			}
			return
		}
	default:
		writeHTTPError(w, http.StatusBadRequest)
		return
	}

	// A show queue follows regular episodes, including specials explicitly
	// placed inside a regular presented season, but not the Specials season.
	if plKey == 0 {
		if path, err := uriPath(uri); err == nil {
			path, _, _ = strings.Cut(path, "?")
			if m := reMetadataPath.FindStringSubmatch(path); len(m) > 0 {
				if k, err := frontend.ParseRatingKey(m[1]); err == nil && k.Kind() == frontend.KindShow {
					filtered := keys[:0]
					for _, item := range keys {
						if !s.queueSpecial(ctx, pr, item) {
							filtered = append(filtered, item)
						}
					}
					keys = filtered
				}
			}
		}
	}

	// Cinema Trailers and the owner's pre-roll: prepend them before the
	// feature and warm the feature while they play. A pre-roll needs no
	// extrasPrefixCount; it never precedes a resume.
	prefixAt := -1
	if !shuffle && len(keys) > 0 {
		n, err := strconv.Atoi(q.Get("extrasPrefixCount"))
		cinema := err == nil && n > 0
		featureIdx := 0
		if start := keyFromPath(q.Get("key")); start != 0 {
			for i, k := range keys {
				if k == start {
					featureIdx = i
					break
				}
			}
		}
		feature := keys[featureIdx]
		var prefix []frontend.RatingKey
		if cinema && feature.Kind() == frontend.KindMovie {
			prefix = s.cinemaTrailerKeys(ctx, pr, feature, n)
		}
		if preroll, order := s.prerollKey(ctx, pr, feature, cinema); preroll != 0 {
			if order == frontend.PrerollAfterTrailers {
				prefix = append(prefix, preroll)
			} else {
				prefix = append([]frontend.RatingKey{preroll}, prefix...)
			}
		}
		if len(prefix) > 0 {
			merged := append([]frontend.RatingKey(nil), keys[:featureIdx]...)
			merged = append(append(merged, prefix...), keys[featureIdx:]...)
			keys = merged
			prefixAt = featureIdx
			s.prewarmFeature(ctx, pr, feature, s.clientCaps(pr))
		}
	}
	snap, err := s.queues.CreateWith(pr.User.ID, pr.ClientID, keys, s.now(), func(p *playQueue) {
		p.SourceURI = uri
		if len(keys) > 0 && !strings.HasPrefix(uri, "library://") {
			if path, e := uriPath(uri); e == nil {
				authority := s.identity().MachineIdentifier
				if strings.HasPrefix(uri, "server://") {
					authority, _, _ = strings.Cut(strings.TrimPrefix(uri, "server://"), "/")
				}
				kind := "item"
				sourceKey := keyFromPath(path)
				if strings.Contains(path, ",") || strings.Contains(path, "/sections/") || strings.HasSuffix(path, "/children") || strings.HasSuffix(path, "/allLeaves") || strings.HasSuffix(path, "/grandchildren") || sourceKey.Kind() == frontend.KindShow || sourceKey.Kind() == frontend.KindSeason || sourceKey.Kind() == frontend.KindCollection {
					kind = "directory"
				}
				p.SourceURI = "library://" + authority + "/" + kind + "/" + url.QueryEscape(path)
			}
		}
		p.PlaylistID, p.PlaylistTitle = plKey, plTitle
		if start := keyFromPath(q.Get("key")); start != 0 {
			for i, it := range p.Items {
				if it.Key == start {
					p.Selected = i
					break
				}
			}
		}
		if prefixAt >= 0 {
			p.Selected = prefixAt // playback starts with the first trailer
		}
		if shuffle {
			s.d.SessionStores.ShuffleQueue(p)
		}
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writePlayQueue(w, r, snap)
}

// continuousKeys resolves a play queue URI; with continuous=1 a single
// episode expands to the remaining episodes in its presented season.
func (s *Server) continuousKeys(ctx context.Context, pr *Request, uri string, continuous bool) ([]frontend.RatingKey, error) {
	if strings.HasPrefix(uri, "server://") {
		authority, _, _ := strings.Cut(strings.TrimPrefix(uri, "server://"), "/")
		if authority != s.identity().MachineIdentifier {
			return nil, nil
		}
	}
	keys, err := s.resolveURI(ctx, pr, uri)
	if err != nil {
		return nil, err
	}
	if !continuous || len(keys) != 1 || keys[0].Kind() != frontend.KindEpisode {
		return keys, nil
	}
	ep := keys[0]
	season := ep.Season()
	if s.episodeOrder() != nil {
		if n, _, _, ok := s.episodeOrder().Position(ctx, pr.User, ep); ok {
			season = n
		}
	}
	eps, err := s.metadataChildren(ctx, pr, frontend.SeasonKey(ep.TMDB(), season))
	if err != nil {
		return keys, nil
	}
	for i, e := range eps {
		if e.Key == ep {
			candidates := make([]frontend.Title, 0, len(eps)-i)
			for _, candidate := range eps[i+1:] {
				// Presented seasons: an episode order may place a TMDB special
				// inside a regular season, where it belongs in the queue.
				if (ep.Season() > 0 && s.queueSpecial(ctx, pr, candidate.Key)) || (!candidate.Released.IsZero() && candidate.Released.After(s.now())) {
					continue
				}
				candidates = append(candidates, candidate)
			}
			candidates = s.availableForViewer(ctx, pr.User, s.visibleTitles(pr.User, candidates))
			return capKeys(append([]frontend.RatingKey{ep}, titleKeys(candidates)...)), nil
		}
	}
	return keys, nil
}

// queueSpecial identifies a special outside an explicitly interleaved order.
func (s *Server) queueSpecial(ctx context.Context, pr *Request, key frontend.RatingKey) bool {
	if key.Kind() != frontend.KindEpisode || key.Season() != 0 {
		return false
	}
	if s.episodeOrder() != nil {
		if season, _, _, ok := s.episodeOrder().Position(ctx, pr.User, key); ok && season > 0 {
			return false
		}
	}
	return true
}

// keyFromPath extracts a rating key from "/library/metadata/123" or "123".
func keyFromPath(p string) frontend.RatingKey {
	if p == "" {
		return 0
	}
	if i := strings.Index(p, "/library/metadata/"); i >= 0 {
		p = p[i+len("/library/metadata/"):]
	}
	p, _, _ = strings.Cut(p, "/")
	p, _, _ = strings.Cut(p, "?")
	k, err := frontend.ParseRatingKey(p)
	if err != nil {
		return 0
	}
	return k
}

func (s *Server) pqParam(w http.ResponseWriter, r *http.Request) (int64, *frontend.User, bool) {
	u := FromContext(r.Context()).User
	id, err := strconv.ParseInt(chi.URLParam(r, "pq"), 10, 32)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return 0, nil, false
	}
	if u == nil {
		writeHTTPError(w, http.StatusNotFound)
		return 0, nil, false
	}
	return id, u, true
}

func (s *Server) handlePlayQueue(w http.ResponseWriter, r *http.Request) {
	id, u, ok := s.pqParam(w, r)
	if !ok {
		return
	}
	var snap playQueue
	if r.URL.Query().Get("own") == "1" {
		snap, ok = s.queues.Take(id, u.ID, FromContext(r.Context()).ClientID, s.now())
	} else {
		snap, ok = s.queues.Get(id, u.ID)
	}
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.writePlayQueue(w, r, snap)
}

// handlePlayQueueMutate covers PUT /playQueues/{id} (add) and the
// shuffle/unshuffle/reset/clear/remove/move sub-resources.
func (s *Server) handlePlayQueueMutate(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, u, ok := s.pqParam(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		pr := FromContext(ctx)
		q := r.URL.Query()
		if !validQueueWindow(q) {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if op == "move" && q.Has("after") && q.Get("after") == "" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if existing, found := s.queues.Get(id, u.ID); found {
			if existing.LiveSession != "" {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			// Once the viewer has explicitly queued content, PMS refuses
			// shuffle controls rather than replacing that Up Next order.
			if (op == "shuffle" || op == "unshuffle") && existing.LastAdded != 0 {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
		}
		var addKeys []frontend.RatingKey
		if op == "add" {
			var err error
			switch {
			case q.Get("uri") != "":
				addKeys, err = s.continuousKeys(ctx, pr, q.Get("uri"), q.Get("continuous") == "1")
			case q.Get("playlistID") != "":
				var k frontend.RatingKey
				if k, err = frontend.ParseRatingKey(q.Get("playlistID")); err == nil {
					addKeys, err = s.expandKey(ctx, pr, k)
				}
			}
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
		pre, err := s.queues.Preallocate(addKeys)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		itemID, _ := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64)
		var bad bool
		var before playQueue
		var removedSelected bool
		snap, ok := s.queues.With(id, u.ID, s.now(), func(p *playQueue) {
			before = p.Snapshot()
			removedSelected = false
			bad = false // a clustered edit may run again after a conflict
			switch op {
			case "add":
				s.queues.AddItems(p, addKeys, pre, q.Get("next") == "1")
			case "shuffle":
				s.queues.Reshuffle(p, true)
			case "unshuffle":
				s.queues.Reshuffle(p, false)
			case "reset":
				p.Selected = 0
				p.Version++
			case "clear":
				// Keep the playing item, drop the rest.
				if p.Selected >= 0 && p.Selected < len(p.Items) {
					p.Items = []pqItem{p.Items[p.Selected]}
				} else {
					p.Items = nil
				}
				p.Selected = 0
				p.Original = nil
				p.Version++
			case "remove":
				idx := s.d.SessionStores.QueueItemIndex(p.Items, itemID)
				if idx < 0 {
					bad = true
					return
				}
				removedSelected = idx == p.Selected
				p.Items = append(p.Items[:idx], p.Items[idx+1:]...)
				if idx < p.Selected {
					p.Selected--
				}
				if removedSelected && p.Selected >= len(p.Items) {
					p.Selected = -1
				}
				p.ReanchorAdded()
				p.Version++
			case "move":
				idx := s.d.SessionStores.QueueItemIndex(p.Items, itemID)
				if idx < 0 {
					bad = true
					return
				}
				if q.Get("after") == strconv.FormatInt(itemID, 10) {
					bad = true
					return
				}
				var selID int64
				if p.Selected >= 0 && p.Selected < len(p.Items) {
					selID = p.Items[p.Selected].ID
				}
				it := p.Items[idx]
				rest := append(append([]pqItem(nil), p.Items[:idx]...), p.Items[idx+1:]...)
				to := 0
				if after, err := strconv.ParseInt(q.Get("after"), 10, 64); err == nil && after > 0 {
					if a := s.d.SessionStores.QueueItemIndex(rest, after); a >= 0 {
						to = a + 1
					}
				}
				p.Items = append(rest[:to], append([]pqItem{it}, rest[to:]...)...)
				p.Selected = s.d.SessionStores.QueueItemIndex(p.Items, selID)
				p.ReanchorAdded()
				p.Version++
			}
		})
		if !ok || bad {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if op == "remove" {
			before.SuppressItem = itemID
			if removedSelected {
				before.SuppressRows = true
				before.SuppressMetadata = true
				before.ForceOffsetZero = true
			}
			s.writePlayQueue(w, r, before)
			return
		}
		s.writePlayQueue(w, r, snap)
	}
}

func (s *Server) writePlayQueue(w http.ResponseWriter, r *http.Request, pq playQueue) {
	if pq.LiveSession != "" {
		s.writeLivePlayQueue(w, r, pq)
		return
	}
	// The queue's write is done; the rest only reads. Queue requests are
	// POST/PUT, so they arrive without the read memo GETs get, and every
	// item would otherwise re-read the same viewer rows (track rules and
	// memories, resume records, server settings) once per item and version.
	if !reqmemo.Active(r.Context()) {
		r = r.WithContext(reqmemo.With(r.Context()))
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if !validQueueWindow(q) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	center := pq.Selected
	if q.Has("center") {
		id, err := strconv.ParseInt(q.Get("center"), 10, 64)
		center = -1
		if err == nil {
			center = s.d.SessionStores.QueueItemIndex(pq.Items, id)
		}
	}
	lo, hi := 0, 0
	if center >= 0 && center < len(pq.Items) {
		lo, hi = 0, len(pq.Items)
		if q.Has("window") {
			n, _ := strconv.Atoi(q.Get("window"))
			if n == 0 {
				lo, hi = center, center
			} else {
				lo, hi = max(0, center-n), min(len(pq.Items), center+n+1)
			}
		}
		before, _ := queueFlag(q, "includeBefore")
		after, _ := queueFlag(q, "includeAfter")
		if q.Has("includeBefore") && !before {
			lo = min(hi, center+1)
		}
		if q.Has("includeAfter") && !after {
			hi = max(lo, center)
		}
	}
	if pq.SuppressRows {
		lo, hi = 0, 0
	}
	slice := pq.Items[lo:hi]
	keys := make([]frontend.RatingKey, len(slice))
	for i, it := range slice {
		keys[i] = it.Key
	}
	// Selection identity is independent of the requested centre/window.
	if pq.Selected >= 0 && pq.Selected < len(pq.Items) && !pq.SuppressMetadata {
		keys = append(keys, pq.Items[pq.Selected].Key)
	}
	titles := s.visibleTitles(pr.User, s.getTitles(ctx, dedupeKeys(keys)))
	byKey := make(map[frontend.RatingKey]*frontend.Title, len(titles))
	for i := range titles {
		byKey[titles[i].Key] = &titles[i]
	}
	state := s.stateFor(ctx, pr, titles)

	var selItem pqItem
	if pq.Selected >= 0 && pq.Selected < len(pq.Items) {
		selItem = pq.Items[pq.Selected]
	}
	if selTitle, ok := byKey[selItem.Key]; ok {
		if pr != nil && pr.detailWaitUntil.IsZero() {
			startDetailPageWait(pr)
		}
		s.warmDetail(ctx, pr, selTitle)
	}
	mc := libraryContainer()
	mc.AllowSync = nil
	mc.Extra = []model.Attr{model.A("playQueueID", pq.ID), model.A("playQueueShuffled", pq.Shuffled), model.A("playQueueVersion", pq.Version)}
	if len(pq.Items) > 0 || pq.Selected < 0 {
		offset := max(0, pq.Selected)
		if pq.ForceOffsetZero {
			offset = 0
		}
		mc.Extra = append(mc.Extra, model.A("playQueueSelectedItemOffset", offset), model.A("playQueueTotalCount", len(pq.Items)))
	}
	if selItem.ID != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueSelectedItemID", selItem.ID))
		selectedVisible := byKey[selItem.Key] != nil
		if selItem.Key.Kind() == frontend.KindExtra {
			_, selectedVisible = s.extraMetadataByKey(ctx, pr, selItem.Key)
		}
		if selectedVisible && !pq.SuppressMetadata {
			mc.Extra = append(mc.Extra, model.A("playQueueSelectedMetadataItemID", selItem.Key.String()))
		}
	}
	if pq.PlaylistID == 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueSourceURI", pq.SourceURI))
	}
	if pq.LastAdded != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueueLastAddedItemID", pq.LastAdded))
	}
	if pq.PlaylistID != 0 {
		mc.Extra = append(mc.Extra, model.A("playQueuePlaylistID", pq.PlaylistID.String()), model.A("playQueuePlaylistTitle", pq.PlaylistTitle))
	}
	pr.versionFlowKey = selItem.Key
	pr.versionItemKey = selItem.Key
	// The rows a client pages through are the items that render; a page
	// renders only its own rows, never the whole queue.
	type queueRow struct {
		it    pqItem
		t     *frontend.Title
		extra *model.Metadata
	}
	var rows []queueRow
	for _, it := range slice {
		if it.ID == pq.SuppressItem {
			continue
		}
		if it.Key.Kind() == frontend.KindExtra {
			if md, ok := s.extraMetadataByKey(ctx, pr, it.Key); ok {
				rows = append(rows, queueRow{it: it, extra: &md})
			}
			continue
		}
		if t, ok := byKey[it.Key]; ok {
			rows = append(rows, queueRow{it: it, t: t})
		}
	}
	if pr.HasPage {
		lo, hi := pr.window(len(rows))
		rows = rows[lo:hi]
	}
	// The rows' remembered tracks, languages and backdrop colours in a few
	// batches, as renderTitles reads them, not per row.
	if len(rows) > 1 {
		page := make([]frontend.Title, 0, len(rows))
		for _, row := range rows {
			if row.t != nil {
				page = append(page, *row.t)
			}
		}
		o := renderOpts{detail: true, media: true}
		s.preloadUltraBlur(ctx, page)
		s.preloadShowLanguages(ctx, pr, page, o)
		s.preloadTrackChoices(ctx, pr, page, o)
	}
	for _, row := range rows {
		var md model.Metadata
		if row.extra != nil {
			md = *row.extra
		} else {
			md = s.metadata(ctx, pr, row.t, renderOpts{detail: true, media: true, state: state})
		}
		md.PlayQueueItemID = row.it.ID
		mc.Metadata = append(mc.Metadata, md)
	}
	applyExcludedElements(mc.Metadata, excludedElements(q))
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func dedupeKeys(ks []frontend.RatingKey) []frontend.RatingKey {
	seen := make(map[frontend.RatingKey]bool, len(ks))
	out := ks[:0:0]
	for _, k := range ks {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func validQueueWindow(q url.Values) bool {
	if q.Has("includeBefore") && q.Has("includeAfter") {
		before, _ := queueFlag(q, "includeBefore")
		after, _ := queueFlag(q, "includeAfter")
		if !before && !after {
			return false
		}
	}
	for _, name := range []string{"includeBefore", "includeAfter"} {
		if _, ok := queueFlag(q, name); !ok {
			return false
		}
	}
	if q.Has("window") {
		n, err := strconv.Atoi(q.Get("window"))
		if err != nil || n < 0 {
			return false
		}
	}
	return true
}

type pqItem = frontend.QueueItem
type playQueue = frontend.PlayQueue
type playQueueStore = frontend.PlayQueues

const maxQueues = frontend.MaxPlayQueues
