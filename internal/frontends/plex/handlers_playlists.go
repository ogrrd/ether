package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// maxExpand caps how many items a URI may expand to (a whole section).
const maxExpand = frontend.PlaylistExpandLimit

var (
	reMetadataPath   = regexp.MustCompile(`^/library/metadata/([\d,]+)(?:/(children|allLeaves|grandchildren))?/?$`)
	reCollectionPath = regexp.MustCompile(`^/library/collections/(\d+)(?:/(children|items))?/?$`)
	rePlaylistPath   = regexp.MustCompile(`^/playlists/(\d+)(?:/items)?/?$`)
	reSectionPath    = regexp.MustCompile(`^/library/sections/(\d+)/(all|unwatched|newest|recentlyAdded)$`)
)

// uriPath extracts the library path (and query) from a Plex content URI:
//
//	server://{machineId}/com.plexapp.plugins.library/library/metadata/1,2
//	library://{sectionUUID}/item/%2Flibrary%2Fmetadata%2F123
//	library://{sectionUUID}/directory/%2Flibrary%2Fsections%2F1%2Fall%3Ftype%3D1
//	/library/metadata/123
func uriPath(uri string) (string, error) {
	switch {
	case strings.HasPrefix(uri, "server://"):
		rest := strings.TrimPrefix(uri, "server://")
		_, rest, ok := strings.Cut(rest, "/") // machine id
		if !ok {
			return "", fmt.Errorf("bad uri %q", uri)
		}
		_, rest, ok = strings.Cut(rest, "/") // provider
		if !ok {
			return "", fmt.Errorf("bad uri %q", uri)
		}
		p := "/" + rest
		if dec, err := url.PathUnescape(p); err == nil && strings.Contains(p, "%2F") {
			p = dec
		}
		return p, nil
	case strings.HasPrefix(uri, "library://"):
		rest := strings.TrimPrefix(uri, "library://")
		_, rest, ok := strings.Cut(rest, "/")
		if !ok {
			return "", fmt.Errorf("bad uri %q", uri)
		}
		_, enc, ok := strings.Cut(rest, "/") // "item" or "directory"
		if !ok {
			return "", fmt.Errorf("bad uri %q", uri)
		}
		p, err := url.PathUnescape(enc)
		if err != nil {
			return "", err
		}
		return p, nil
	case strings.HasPrefix(uri, "/"):
		return uri, nil
	}
	return "", fmt.Errorf("unsupported uri %q", uri)
}

// expandKey expands containers (show, season, collection, playlist) into
// playable items.
func (s *Server) expandKey(ctx context.Context, pr *Request, k frontend.RatingKey) ([]frontend.RatingKey, error) {
	if k.Kind() == frontend.KindPlaylist {
		pl, err := s.playlist(ctx, pr.User, k)
		if err != nil {
			return nil, err
		}
		if pl.SmartURI != "" {
			copyRequest := *pr
			copyRequest.HasPage, copyRequest.Start, copyRequest.Size = true, 0, maxExpand
			titles, _, _, err := s.smartPlaylistBrowse(ctx, &copyRequest, pl.SmartURI)
			if err != nil {
				return nil, err
			}
			var out []frontend.RatingKey
			for _, title := range titles {
				keys, err := s.expandKey(ctx, pr, title.Key)
				if err == nil {
					out = append(out, keys...)
				}
				if len(out) >= maxExpand {
					break
				}
			}
			return capKeys(out), nil
		}
	}
	selection := s.exp().NewPlaylistSelection(frontend.PlaylistSelectionOptions{Playlists: s.playlists(), Collections: s.collections(), Visibility: s.visibility(), Children: func(ctx context.Context, key frontend.RatingKey) ([]frontend.Title, error) {
		return s.metadataChildren(ctx, pr, key)
	}, Grandchildren: func(ctx context.Context, key frontend.RatingKey) ([]frontend.Title, error) {
		return s.metadataGrandchildren(ctx, pr, key)
	}})
	return selection.Expand(ctx, pr.User, k)
}

// smartSectionURI keeps only a local section query, without client credentials.
func (s *Server) smartSectionURI(u *frontend.User, uri string) (string, error) {
	p, err := uriPath(uri)
	if err != nil {
		return "", err
	}
	path, raw, _ := strings.Cut(p, "?")
	m := reSectionPath.FindStringSubmatch(path)
	if len(m) == 0 || m[2] != "all" {
		return "", frontend.ErrNotFound
	}
	id, _ := strconv.Atoi(m[1])
	sec, ok := s.sectionByID(id)
	if !ok || !s.canSeeSection(u, id) {
		return "", frontend.ErrNotFound
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", err
	}
	t := typeFromParam(q.Get("type"))
	if t != "" && t != sec.Type && !(sec.Type == frontend.TypeShow && (t == frontend.TypeSeason || t == frontend.TypeEpisode)) {
		return "", frontend.ErrNotFound
	}
	var safe []string
	for _, pair := range strings.Split(raw, "&") {
		key, _, _ := strings.Cut(pair, "=")
		key, err = url.QueryUnescape(key)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(strings.ToLower(key), "x-plex-") || strings.HasPrefix(strings.ToLower(key), "x-ether-") {
			continue
		}
		if pair != "" {
			safe = append(safe, pair)
		}
	}
	return path + "?" + strings.Join(safe, "&"), nil
}

func (s *Server) smartPlaylistRequest(ctx context.Context, pr *Request, uri string) (browseRequest, error) {
	p, err := s.smartSectionURI(pr.User, uri)
	if err != nil {
		return browseRequest{}, err
	}
	path, raw, _ := strings.Cut(p, "?")
	m := reSectionPath.FindStringSubmatch(path)
	id, _ := strconv.Atoi(m[1])
	sec, _ := s.sectionByID(id)
	q, _ := url.ParseQuery(raw)
	t := typeFromParam(q.Get("type"))
	if t == "" {
		t = sec.Type
	}
	br := s.parseBrowse(ctx, sec, t, pr.User, q, raw)
	if t == frontend.TypeShow {
		// Select episodes by the complete show predicate in SQL. Do not
		// reinterpret show title/year/watch rules as episode rules or expand
		// all matching shows in memory before paging.
		show := br.q.Filter
		if br.unwatched != nil || br.inProgress != nil || br.lastViewedAfter > 0 || br.lastViewedBefore > 0 {
			var uid int64
			if pr.User != nil && s.state() != nil {
				uid = pr.User.ID
			}
			state := frontend.FilterGroup{UserID: uid, Unwatched: br.unwatched, InProgress: br.inProgress, LastViewedAfter: br.lastViewedAfter, LastViewedBefore: br.lastViewedBefore}
			if show.Group == nil {
				show.Group = &state
			} else {
				show.Group = &frontend.FilterGroup{Operator: "and", Children: []frontend.FilterGroup{*show.Group, state}}
			}
			br.unwatched, br.inProgress = nil, nil
			br.lastViewedAfter, br.lastViewedBefore = 0, 0
		}
		br.q.Filter = frontend.Filter{Type: frontend.TypeEpisode, Group: &frontend.FilterGroup{ParentOnly: true, Filter: &show}}
	}
	return br, nil
}

func (s *Server) smartPlaylistBrowse(ctx context.Context, pr *Request, uri string) ([]frontend.Title, int, int, error) {
	br, err := s.smartPlaylistRequest(ctx, pr, uri)
	if err != nil {
		return nil, 0, 0, err
	}
	return s.browse(ctx, pr, br)
}

func titleKeys(ts []frontend.Title) []frontend.RatingKey {
	out := make([]frontend.RatingKey, len(ts))
	for i := range ts {
		out[i] = ts[i].Key
	}
	return out
}

// resolveURI turns a content URI into playable rating keys.
func (s *Server) resolveURI(ctx context.Context, pr *Request, uri string) ([]frontend.RatingKey, error) {
	p, err := uriPath(uri)
	if err != nil {
		return nil, err
	}
	path, rawQuery, _ := strings.Cut(p, "?")
	switch {
	case path == "/hubs/items" || strings.HasPrefix(path, "/hubs/items/"):
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return nil, err
		}
		id := q.Get("identifier")
		if path != "/hubs/items" {
			id, err = url.PathUnescape(strings.TrimPrefix(path, "/hubs/items/"))
			if err != nil {
				return nil, err
			}
		}
		h, ok := s.findHub(ctx, pr, id, parseSectionList(q.Get("contentDirectoryID")), q.Get("metadataID"), maxExpand)
		if !ok {
			return nil, frontend.ErrNotFound
		}
		// Use the rendered row so availability and viewer restrictions match
		// the items the client was offered, including episode parent policy.
		var out []frontend.RatingKey
		for _, md := range s.renderHub(ctx, pr, h, hubCtx{}).Metadata {
			k, err := frontend.ParseRatingKey(md.RatingKey)
			if err != nil {
				continue
			}
			sub, err := s.expandKey(ctx, pr, k)
			if err == nil {
				out = append(out, sub...)
			}
			if len(out) >= maxExpand {
				break
			}
		}
		return capKeys(out), nil
	case reMetadataPath.MatchString(path):
		m := reMetadataPath.FindStringSubmatch(path)
		keys, err := parseKeys(m[1])
		if err != nil {
			return nil, err
		}
		var out []frontend.RatingKey
		for _, k := range keys {
			// An addressed title (or the show above it) the viewer may
			// not see never reaches a playlist or play queue. Policy only:
			// discovery evidence hides a recent title from lists but must
			// not stop a viewer queueing it to play.
			switch k.Kind() {
			case frontend.KindShow, frontend.KindSeason, frontend.KindMovie, frontend.KindEpisode:
				if t, err := s.metadataTitle(ctx, pr, k); err == nil && !s.playableTitle(ctx, pr.User, t) {
					return nil, frontend.ErrNotFound
				}
			}
			sub, err := s.expandKey(ctx, pr, k)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			if len(out) >= maxExpand {
				break
			}
		}
		return capKeys(out), nil
	case reCollectionPath.MatchString(path):
		m := reCollectionPath.FindStringSubmatch(path)
		k, err := frontend.ParseRatingKey(m[1])
		if err != nil {
			return nil, err
		}
		keys, err := s.expandKey(ctx, pr, k)
		return capKeys(keys), err
	case rePlaylistPath.MatchString(path):
		m := rePlaylistPath.FindStringSubmatch(path)
		k, err := frontend.ParseRatingKey(m[1])
		if err != nil {
			return nil, err
		}
		return s.expandKey(ctx, pr, k)
	case reSectionPath.MatchString(path):
		m := reSectionPath.FindStringSubmatch(path)
		id, _ := strconv.Atoi(m[1])
		sec, ok := s.sectionByID(id)
		if !ok || !s.canSeeSection(pr.User, id) {
			return nil, frontend.ErrNotFound
		}
		q, _ := url.ParseQuery(rawQuery)
		br := s.parseBrowse(ctx, sec, sec.Type, pr.User, q, rawQuery)
		br.q.Offset, br.q.Limit = 0, maxExpand
		page, err := s.catalog().Browse(ctx, br.q)
		if err != nil {
			return nil, err
		}
		var out []frontend.RatingKey
		for _, t := range page.Items {
			sub, err := s.expandKey(ctx, pr, t.Key)
			if err == nil {
				out = append(out, sub...)
			}
			if len(out) >= maxExpand {
				break
			}
		}
		return capKeys(out), nil
	}
	return nil, fmt.Errorf("unsupported uri path %q", path)
}

func capKeys(ks []frontend.RatingKey) []frontend.RatingKey {
	if len(ks) > maxExpand {
		return ks[:maxExpand]
	}
	return ks
}

// ---------------------------------------------------------------------------
// Playlists

func (s *Server) playlist(ctx context.Context, u *frontend.User, k frontend.RatingKey) (*frontend.Playlist, error) {
	return (s.exp().NewPlaylistSelection(frontend.PlaylistSelectionOptions{Playlists: s.playlists()})).Get(ctx, u, k)
}

func (s *Server) playlistMetadata(ctx context.Context, pl *frontend.Playlist, omitState ...bool) model.Metadata {
	rk := pl.Key.String()
	count := len(pl.Items)
	if pl.SmartURI != "" {
		pr := *FromContext(ctx)
		pr.HasPage, pr.Start, pr.Size = true, 0, 0
		if _, total, _, err := s.smartPlaylistBrowse(ctx, &pr, pl.SmartURI); err == nil {
			count = total
		}
	}
	stamp := unixOrZero(pl.UpdatedAt)
	if stamp == 0 {
		stamp = 1
	}
	m := model.Metadata{
		RatingKey:    rk,
		Key:          "/playlists/" + rk + "/items",
		GUID:         "com.plexapp.agents.none://playlist-" + rk,
		Type:         "playlist",
		Title:        pl.Title,
		Summary:      pl.Summary,
		Smart:        model.Ptr(pl.Smart),
		PlaylistType: "video",
		Composite:    "/playlists/" + rk + "/composite/" + strconv.FormatInt(stamp, 10),
		LeafCount:    model.Ptr(count),
		AddedAt:      unixOrZero(pl.AddedAt),
		UpdatedAt:    unixOrZero(pl.UpdatedAt),
	}
	if pl.GUID != "" {
		m.GUID = pl.GUID
	}
	if m.AddedAt == 0 {
		m.AddedAt = unixOrZero(pl.UpdatedAt)
	}
	if count == 0 {
		m.Composite = ""
	}
	if pl.Smart {
		m.Icon = "playlist://image.smart"
	}
	m.Duration = s.playlistDuration(pl.Items)
	if len(omitState) > 0 && !omitState[0] {
		s.applyListWatch(ctx, &m, pl.Key)
	}
	return m
}

// playlistDuration is the total runtime PMS reports on a playlist, from
// the title-detail cache without blocking: it is omitted (0) until every
// item's runtime is known, and unknown items are fetched in the background.
func (s *Server) playlistDuration(items []frontend.RatingKey) int64 {
	if s.titles == nil {
		return 0
	}
	var seconds int64
	known := true
	for _, key := range items {
		title := s.titles.get(key)
		if title == nil || title.Runtime <= 0 {
			known = false
			continue
		}
		// PMS truncates each member before summing; truncating the total
		// instead incorrectly carries fractional seconds between members.
		seconds += title.Runtime.Milliseconds() / 1000
	}
	if !known {
		return 0
	}
	return seconds * 1000
}

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	s.writePlaylistListing(w, r)
}

func (s *Server) requirePlaylists(w http.ResponseWriter, r *http.Request) bool {
	if s.playlists() == nil || FromContext(r.Context()).User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlaylists(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if !q.Has("title") || !q.Has("type") || (!q.Has("uri") && !q.Has("playQueueID")) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mediaType := q.Get("type")
	if mediaType != "video" && mediaType != "audio" && mediaType != "photo" {
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	title := q.Get("title")
	// Ether has video catalogue sections only. Refuse unsupported smart media
	// domains before attempting to interpret a section query.
	if mediaType != "video" && q.Get("smart") == "1" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if q.Get("smart") == "1" {
		uri, err := s.smartSectionURI(pr.User, q.Get("uri"))
		store, ok := s.playlists().(frontend.SmartPlaylists)
		if err != nil || !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		pl, err := store.CreateSmartPlaylist(ctx, pr.User.ID, title, uri)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.notifyPlaylistLifecycle(pl, true)
		s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(ctx, pl, true)}})
		return
	}
	var keys []frontend.RatingKey
	if uri := q.Get("uri"); uri != "" {
		// Unresolvable sources are valid empty playlist creations.
		if strings.HasPrefix(uri, "server://"+s.identity().MachineIdentifier+"/") || strings.HasPrefix(uri, "library://") {
			keys, _ = s.resolveURI(ctx, pr, uri)
		}

	} else if pqID, err := strconv.ParseInt(q.Get("playQueueID"), 10, 64); err == nil {
		if pq, ok := s.queues.Get(pqID, pr.User.ID); ok {
			for _, it := range pq.Items {
				keys = append(keys, it.Key)
			}
		}
	}
	// Unresolvable non-video sources cannot name an Ether section. Give the
	// client a graceful refusal rather than create a mislabeled video list.
	if mediaType != "video" && len(keys) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mismatch := mediaType != "video" && len(keys) > 0
	if mismatch {
		keys = nil
	}
	pl, err := s.playlists().Create(ctx, pr.User.ID, title, keys)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.notifyPlaylistLifecycle(pl, true)
	if mismatch {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mc := &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(ctx, pl, true)}}
	s.write(w, r, mc)
}

func (s *Server) playlistFromParam(w http.ResponseWriter, r *http.Request) (*frontend.Playlist, bool) {
	if !s.requirePlaylists(w, r) {
		return nil, false
	}
	k, err := keyParam(r)
	if err != nil || k.Kind() != frontend.KindPlaylist {
		writeHTTPError(w, http.StatusNotFound)
		return nil, false
	}
	pl, err := s.playlist(r.Context(), FromContext(r.Context()).User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return nil, false
	}
	return pl, true
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	md := s.playlistMetadata(r.Context(), pl, true)
	if pl.SmartURI != "" {
		md.Content = "library://x/directory/" + url.QueryEscape(pl.SmartURI)
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{md}})
}

// handlePlaylistEdit persists the user-facing playlist name and summary.
func (s *Server) handlePlaylistEdit(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	edit := frontend.PlaylistEdit{}
	for _, field := range []struct {
		name   string
		target **string
	}{{"title", &edit.Title}, {"summary", &edit.Summary}} {
		if q.Has(field.name + ".value") {
			value := q.Get(field.name + ".value")
			*field.target = &value
		} else if q.Has(field.name) {
			value := q.Get(field.name)
			*field.target = &value
		}
	}
	if edit.Title == nil && edit.Summary == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if edit.Title != nil || edit.Summary != nil {
		store, ok := s.playlists().(frontend.PlaylistEditor)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		var err error
		pl, err = store.EditPlaylist(r.Context(), FromContext(r.Context()).User.ID, pl.Key, edit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), pl)}})
}

func (s *Server) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	if err := s.playlists().Delete(r.Context(), FromContext(r.Context()).User.ID, pl.Key); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.notifyPlaylistLifecycle(pl, false)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePlaylistItems(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if status := playlistQueryStatus(q, true); status != 0 {
		writeHTTPError(w, status)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	// Ordinary smart reads keep counting and pagination inside the catalogue.
	// Parent projections need a bounded membership expansion below.
	if pl.SmartURI != "" && s.smartPlaylistSQLAvailable() && (q.Get("type") != "" && q.Get("type") != "-1" || playlistHasItemFilters(q)) {
		s.writeSmartPlaylistItems(w, r, pl)
		return
	}
	if pl.SmartURI != "" && (q.Get("type") == "" || q.Get("type") == "-1") && !playlistHasItemFilters(q) {
		titles, total, offset, err := s.smartPlaylistBrowse(ctx, pr, pl.SmartURI)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		mc := &model.MediaContainer{RatingKey: pl.Key.String(), Title1: pl.Title, PlaylistType: "video", Smart: model.Ptr(true), LeafCount: model.Ptr(total)}
		storedOnly := len(titles) > 0 && titles[0].Type == frontend.TypeEpisode
		sqlAvailability, _ := s.catalog().(interface{ BrowseAvailabilityInSQL() bool })
		mc.Metadata = s.renderTitles(ctx, pr, titles, renderOpts{media: true, storedOnly: storedOnly, availabilityInSQL: sqlAvailability != nil && sqlAvailability.BrowseAvailabilityInSQL()})
		if total > 0 {
			mc.Composite = "/playlists/" + pl.Key.String() + "/composite/" + strconv.FormatInt(unixOrZero(pl.UpdatedAt), 10)
		}
		for _, md := range mc.Metadata {
			mc.Duration += md.Duration / 1000
		}
		if q.Get("includeMeta") == "1" {
			types := map[frontend.MediaType]bool{}
			for _, t := range titles {
				types[t.Type] = true
				if t.Type == frontend.TypeEpisode {
					types[frontend.TypeShow] = true
					types[frontend.TypeSeason] = true
				}
			}
			mc.Meta = playlistItemsMeta(r, types)
		}
		mc.Size = len(mc.Metadata)
		if pr.HasPage {
			mc.SetPage(offset, total)
		}
		s.write(w, r, mc)
		return
	}
	page, err := s.playlistItemRows(ctx, pr, pl, q)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	total := page.total
	mc := &model.MediaContainer{RatingKey: pl.Key.String(), Key: "/playlists/" + pl.Key.String() + "/items", Title1: pl.Title, PlaylistType: "video", Smart: model.Ptr(pl.Smart), LeafCount: model.Ptr(total), Metadata: page.rows}
	if total > 0 {
		mc.Composite = "/playlists/" + pl.Key.String() + "/composite/" + strconv.FormatInt(unixOrZero(pl.UpdatedAt), 10)
	}
	mc.Duration = page.durationSec
	if q.Get("includeMeta") == "1" {
		mc.Meta = playlistItemsMeta(r, page.types)
	}
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(page.lo, total)
	}
	s.write(w, r, mc)
}

func (s *Server) handlePlaylistAdd(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	if pl.SmartURI != "" {
		uri, err := s.smartSectionURI(FromContext(r.Context()).User, r.URL.Query().Get("uri"))
		store, ok := s.playlists().(frontend.PlaylistEditor)
		if err != nil || !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		updated, err := store.EditPlaylist(r.Context(), FromContext(r.Context()).User.ID, pl.Key, frontend.PlaylistEdit{SmartURI: &uri})
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), updated)}})
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	uri := r.URL.Query().Get("uri")
	var keys []frontend.RatingKey
	if strings.HasPrefix(uri, "server://"+s.identity().MachineIdentifier+"/") || strings.HasPrefix(uri, "library://") {
		keys, _ = s.resolveURI(ctx, pr, uri)
	}
	if store, ok := s.playlists().(frontend.PlaylistMutator); ok {
		updated, added, err := store.MutatePlaylist(ctx, pr.User.ID, pl.Key, frontend.PlaylistMutation{Operation: "add", Items: keys})
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Extra: []model.Attr{model.A("leafCountAdded", added), model.A("leafCountRequested", len(keys))}, Metadata: []model.Metadata{s.playlistMetadata(ctx, updated)}})
		return
	}
	requested := len(keys)
	seen := make(map[frontend.RatingKey]bool, len(pl.Items)+len(keys))
	for _, key := range pl.Items {
		seen[key] = true
	}
	additions := keys[:0]
	for _, key := range keys {
		if !seen[key] {
			additions = append(additions, key)
			seen[key] = true
		}
	}
	keys = additions
	if err := s.playlists().Add(ctx, pr.User.ID, pl.Key, keys); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if updated, err := s.playlist(ctx, pr.User, pl.Key); err == nil {
		pl = updated
	}
	md := s.playlistMetadata(ctx, pl)
	s.write(w, r, &model.MediaContainer{Size: 1, Extra: []model.Attr{model.A("leafCountAdded", len(keys)), model.A("leafCountRequested", requested)}, Metadata: []model.Metadata{md}})
}

func itemIndexParam(r *http.Request, pl *frontend.Playlist) (int, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "item"))
	if len(pl.ItemIDs) == len(pl.Items) && len(pl.ItemIDs) > 0 {
		if err != nil {
			return 0, false
		}
		for i, itemID := range pl.ItemIDs {
			if int64(id) == itemID {
				return i, true
			}
		}
		return 0, false
	}
	if err != nil || id < 1 || id > len(pl.Items) {
		return 0, false
	}
	return id - 1, true
}

func (s *Server) handlePlaylistRemove(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	if pl.SmartURI != "" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if store, ok := s.playlists().(frontend.PlaylistMutator); ok {
		item, _ := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64)
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		updated, _, err := store.MutatePlaylist(r.Context(), FromContext(r.Context()).User.ID, pl.Key, frontend.PlaylistMutation{Operation: "remove", ItemID: item, AfterID: after})
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), updated)}})
		return
	}
	ctx := r.Context()
	uid := FromContext(ctx).User.ID
	idx, ok := itemIndexParam(r, pl)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if err := s.playlists().Remove(ctx, uid, pl.Key, idx); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.handlePlaylistItemsAfterChange(w, r, pl.Key)
}

func (s *Server) handlePlaylistClear(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	if pl.SmartURI != "" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if store, ok := s.playlists().(frontend.PlaylistMutator); ok {
		item, _ := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64)
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		updated, _, err := store.MutatePlaylist(r.Context(), FromContext(r.Context()).User.ID, pl.Key, frontend.PlaylistMutation{Operation: "clear", ItemID: item, AfterID: after})
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), updated)}})
		return
	}
	ctx := r.Context()
	uid := FromContext(ctx).User.ID
	for i := len(pl.Items) - 1; i >= 0; i-- {
		if err := s.playlists().Remove(ctx, uid, pl.Key, i); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	s.handlePlaylistItemsAfterChange(w, r, pl.Key)
}

func (s *Server) handlePlaylistMove(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromParam(w, r)
	if !ok {
		return
	}
	if pl.SmartURI != "" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if store, ok := s.playlists().(frontend.PlaylistMutator); ok {
		item, _ := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64)
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		updated, _, err := store.MutatePlaylist(r.Context(), FromContext(r.Context()).User.ID, pl.Key, frontend.PlaylistMutation{Operation: "move", ItemID: item, AfterID: after})
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), updated)}})
		return
	}
	ctx := r.Context()
	uid := FromContext(ctx).User.ID
	from, ok := itemIndexParam(r, pl)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	to := 0
	if after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil {
		anchor := -1
		if len(pl.ItemIDs) == len(pl.Items) && len(pl.ItemIDs) > 0 {
			for i, id := range pl.ItemIDs {
				if id == after {
					anchor = i
					break
				}
			}
		} else if after >= 1 && after <= int64(len(pl.Items)) {
			anchor = int(after) - 1
		}
		if anchor >= 0 {
			to = anchor
			if from > anchor {
				to++
			}
		}
	}

	if from != to {
		if err := s.playlists().Move(ctx, uid, pl.Key, from, to); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	s.handlePlaylistItemsAfterChange(w, r, pl.Key)
}

// handlePlaylistItemsAfterChange answers a mutation with the updated items,
// as PMS does.
func (s *Server) handlePlaylistItemsAfterChange(w http.ResponseWriter, r *http.Request, k frontend.RatingKey) {
	pl, err := s.playlist(r.Context(), FromContext(r.Context()).User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Metadata: []model.Metadata{s.playlistMetadata(r.Context(), pl)}})
}
