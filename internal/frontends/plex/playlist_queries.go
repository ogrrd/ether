package plex

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

var playlistSortDefs = []sortDef{{"titleSort", "Name", "asc"}, {"addedAt", "Date Added", "desc"}, {"lastViewedAt", "Last Played", "desc"}, {"viewCount", "Play Count", "desc"}, {"duration", "Duration", "desc"}, {"mediaCount", "Item Count", "desc"}}

func playlistSorts(active string, enabled bool) []model.Sort {
	key, dir := parseSortParam(active)
	var out []model.Sort
	for _, d := range playlistSortDefs {
		row := model.Sort{Key: d.key, DescKey: d.key + ":desc", Title: d.title, DefaultDirection: d.defDir}
		if d.key == "titleSort" {
			row.Default = "asc"
		}
		row.Active = model.Ptr(enabled && (key == d.key || key == "" && d.key == "titleSort"))
		if *row.Active {
			if dir == "" {
				dir = d.defDir
			}
			row.ActiveDirection = dir
		}
		out = append(out, row)
	}
	return out
}

func playlistMeta(r *http.Request) *model.Meta {
	q := r.URL.Query()
	keep := url.Values{}
	for _, key := range []string{"playlistType", "sectionID", "type"} {
		if q.Has(key) {
			keep[key] = q[key]
		}
	}
	key := r.URL.Path
	if enc := keep.Encode(); enc != "" {
		key += "?" + enc
	}
	return &model.Meta{Type: []model.MetaType{{Key: key, Type: "playlist", Title: "Playlists", Active: r.URL.Path == "/playlists", Sort: playlistSorts(q.Get("sort"), r.URL.Path == "/playlists")}}}
}

func (s *Server) handlePlaylistSorts(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{}
	for _, d := range playlistSortDefs {
		mc.Directory = append(mc.Directory, model.Directory{Key: d.key, Title: d.title})
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

func (s *Server) playlistListingMetadata(ctx context.Context, pl *frontend.Playlist) model.Metadata {
	md := s.playlistMetadata(ctx, pl)
	md.ViewCount = 1
	md.LastViewedAt = md.AddedAt
	pr := FromContext(ctx)
	if s.d.State != nil && pr != nil && pr.User != nil {
		if st, err := s.d.State.Get(ctx, pr.User.ID, pl.Key); err == nil && (!st.UpdatedAt.IsZero() || st.ViewCount > 0 || !st.LastViewedAt.IsZero()) {
			md.ViewCount = st.ViewCount
			md.LastViewedAt = unixOrZero(st.LastViewedAt)
			md.ViewOffset = st.ViewOffset.Milliseconds()
			md.UserRating = st.UserRating
			md.LastRatedAt = unixOrZero(st.LastRatedAt)
		}
	}
	return md
}

// These parameters are parsed even on endpoints which ignore their values.
func playlistQueryStatus(q url.Values, items bool) int {
	if q.Has("parentIndex") {
		return 500
	}
	for _, key := range []string{"limit", "parent"} {
		if q.Has(key) {
			if _, err := strconv.Atoi(q.Get(key)); err != nil {
				return 500
			}
		}
	}
	if q.Has("resolution") {
		for _, v := range strings.Split(q.Get("resolution"), ",") {
			switch v {
			case "sd", "480", "576", "720", "1080", "2k", "4k", "8k":
			default:
				return 500
			}
		}
	}
	if q.Has("type") {
		if _, err := strconv.Atoi(q.Get("type")); err != nil {
			if items {
				return 400
			}
			return 500
		}
	}
	if v := q.Get("sort"); v != "" {
		for _, part := range strings.Split(v, ",") {
			key, dir := parseSortParam(part)
			good := false
			for _, d := range playlistSortDefs {
				if key == d.key {
					good = true
				}
			}
			if items {
				for _, d := range sortDefs {
					if key == d.key {
						good = true
					}
				}
			}
			if !good || dir != "" && dir != "asc" && dir != "desc" {
				return 500
			}
		}
	}
	return 0
}

func (s *Server) writePlaylistListing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if status := playlistQueryStatus(q, false); status != 0 {
		writeHTTPError(w, status)
		return
	}
	mc := &model.MediaContainer{}
	if q.Get("includeMeta") == "1" {
		mc.Meta = playlistMeta(r)
	}
	var rows []model.Metadata
	if s.d.Playlists != nil && pr.User != nil {
		pls, err := s.d.Playlists.List(ctx, pr.User.ID)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		for i := range pls {
			pl := &pls[i]
			if pl.UserID != pr.User.ID {
				continue
			}
			if pt := q.Get("playlistType"); pt != "" && !strings.Contains(","+pt+",", ",video,") {
				continue
			}
			if q.Has("type") && q.Get("type") != "15" && q.Get("type") != "-1" {
				continue
			}
			if q.Get("smart") == "1" && !pl.Smart || q.Get("smart") == "0" && pl.Smart {
				continue
			}
			if title := q.Get("title"); title != "" && !strings.Contains(strings.ToLower(pl.Title), strings.ToLower(title)) {
				continue
			}
			if section := q.Get("sectionID"); section != "" {
				found := len(pl.Items) == 0 && pl.SmartURI == ""
				for _, key := range pl.Items {
					if sec, ok := s.sectionFor(playlistKeyMediaType(key)); ok && strconv.Itoa(sec.ID) == section {
						found = true
						break
					}
				}
				if pl.SmartURI != "" {
					for _, id := range strings.Split(section, ",") {
						if strings.HasPrefix(pl.SmartURI, "/library/sections/"+id+"/") {
							found = true
						}
					}
				}
				if !found {
					continue
				}
			}
			rows = append(rows, s.playlistListingMetadata(ctx, pl))
		}
	}
	key, dir := parseSortParam(q.Get("sort"))
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		cmp := 0
		switch key {
		case "mediaCount":
			cmp = *a.LeafCount - *b.LeafCount
		case "duration":
			if a.Duration < b.Duration {
				cmp = -1
			} else if a.Duration > b.Duration {
				cmp = 1
			}
		case "addedAt":
			if a.AddedAt < b.AddedAt {
				cmp = -1
			} else if a.AddedAt > b.AddedAt {
				cmp = 1
			}
		case "lastViewedAt":
			if a.LastViewedAt < b.LastViewedAt {
				cmp = -1
			} else if a.LastViewedAt > b.LastViewedAt {
				cmp = 1
			}
		case "viewCount":
			cmp = a.ViewCount - b.ViewCount
		default:
			if key == "" && *a.Smart != *b.Smart {
				return *a.Smart
			}
			cmp = strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
		}
		if cmp == 0 {
			return a.RatingKey < b.RatingKey
		}
		if dir == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
	total := len(rows)
	lo, hi := pr.window(total)
	mc.Metadata = rows[lo:hi]
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(lo, total)
	}
	s.write(w, r, mc)
}

func playlistKeyMediaType(key frontend.RatingKey) frontend.MediaType {
	if key.Kind() == frontend.KindMovie {
		return frontend.TypeMovie
	}
	return frontend.TypeShow
}

func playlistItemsMeta(r *http.Request, types map[frontend.MediaType]bool) *model.Meta {
	q := r.URL.Query()
	selected := q.Get("type")
	base := r.URL.Path
	out := &model.Meta{Type: []model.MetaType{{Key: base, Type: "mixed", Title: "All", Active: selected == "" || selected == "-1"}}}
	for _, tab := range []struct {
		t     frontend.MediaType
		title string
	}{{frontend.TypeMovie, "Movies"}, {frontend.TypeEpisode, "Episodes"}, {frontend.TypeSeason, "Seasons"}, {frontend.TypeShow, "TV Shows"}} {
		if !types[tab.t] {
			continue
		}
		number := strconv.Itoa(typeNumber(tab.t))
		out.Type = append(out.Type, model.MetaType{Key: base + "?type=" + number, Type: string(tab.t), Title: tab.title, Active: selected == number})
	}
	return out
}

// playlistRows is one page of a stored playlist's items: the window's
// rendered rows, and the totals the container reports for every item.
type playlistRows struct {
	rows        []model.Metadata
	total, lo   int
	durationSec int64 // the sum of every item's duration in whole seconds
	types       map[frontend.MediaType]bool
}

// playlistItemRows lists a playlist's items and renders only the page the
// client asked for: a 3000-item playlist read 50 at a time renders 50 rows,
// not every item for each page.
func (s *Server) playlistItemRows(ctx context.Context, pr *Request, pl *frontend.Playlist, q url.Values) (playlistRows, error) {
	var titles []frontend.Title
	if pl.SmartURI != "" {
		all := *pr
		all.HasPage, all.Start, all.Size = true, 0, maxExpand
		var err error
		titles, _, _, err = s.smartPlaylistBrowse(ctx, &all, pl.SmartURI)
		if err != nil {
			return playlistRows{}, err
		}
	} else {
		titles = s.visibleTitles(pr.User, s.getTitles(ctx, pl.Items))
	}
	types := map[frontend.MediaType]bool{}
	for _, t := range titles {
		types[t.Type] = true
		if t.Type == frontend.TypeEpisode {
			types[frontend.TypeSeason] = true
			types[frontend.TypeShow] = true
		}
	}
	want := typeFromParam(q.Get("type"))
	unknown := q.Has("type") && want == "" && q.Get("type") != "-1"
	// Resolve parent identities once, retaining first appearance rather than sorting membership.
	selected := make([]frontend.Title, 0, len(titles))
	seen := map[frontend.RatingKey]bool{}
	for _, t := range titles {
		if unknown {
			continue
		}
		if t.Type == frontend.TypeEpisode && (want == frontend.TypeShow || want == frontend.TypeSeason) {
			key := t.ShowKey
			if want == frontend.TypeSeason {
				key = frontend.SeasonKey(t.Key.TMDB(), t.Season)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if parent, err := s.metadataTitle(ctx, pr, key); err == nil {
				selected = append(selected, *parent)
			}
		} else if want == "" || want == t.Type {
			selected = append(selected, t)
		}
	}
	// Reuse SQL browse predicates with an explicit key bound. This cannot scan the
	// library; the stored list is the only candidate set, and its order is restored.
	filterQuery := url.Values{}
	for key, vals := range q {
		switch key {
		case "sort", "limit", "type", "includeMeta", "X-Plex-Container-Start", "X-Plex-Container-Size":
			continue
		}
		filterQuery[key] = vals
	}
	if len(filterQuery) > 0 && len(selected) > 0 {
		allowed := map[frontend.RatingKey]bool{}
		groups := map[frontend.MediaType][]frontend.RatingKey{}
		for _, t := range selected {
			groups[t.Type] = append(groups[t.Type], t.Key)
		}
		for typ, keys := range groups {
			sec, ok := s.sectionFor(typ)
			if !ok {
				continue
			}
			br := s.parseBrowse(ctx, sec, typ, pr.User, filterQuery, filterQuery.Encode())
			br.q.Filter.IncludeKeys = keys
			br.q.Limit = maxExpand
			page, err := s.d.Catalog.Browse(ctx, br.q)
			if err != nil {
				return playlistRows{}, err
			}
			for _, t := range page.Items {
				allowed[t.Key] = true
			}
		}
		kept := selected[:0]
		for _, t := range selected {
			if allowed[t.Key] {
				kept = append(kept, t)
			}
		}
		selected = kept
	}
	// Duplicate plain members retain their distinct stable item identities.
	ids := map[frontend.RatingKey][]int64{}
	for i, key := range pl.Items {
		id := int64(i + 1)
		if i < len(pl.ItemIDs) {
			id = pl.ItemIDs[i]
		}
		ids[key] = append(ids[key], id)
	}
	itemIDs := make([]int64, len(selected))
	if pl.SmartURI == "" && want != frontend.TypeShow && want != frontend.TypeSeason {
		for i := range selected {
			if q := ids[selected[i].Key]; len(q) > 0 {
				itemIDs[i], ids[selected[i].Key] = q[0], q[1:]
			}
		}
	}
	out := playlistRows{total: len(selected), types: types}
	lo, hi := pr.window(len(selected))
	out.lo = lo
	page := selected[lo:hi]
	if len(selected) > 0 {
		// As the full render was: an empty window of a non-empty list is an
		// empty, not absent, row list.
		out.rows = make([]model.Metadata, 0, len(page))
	}
	state := s.stateFor(ctx, pr, page)
	// Each row's Playable check (resumed tracks) reads its series from one
	// batch of rows.
	ctx = s.withSeriesRows(ctx, pr.User, page)
	for i := range page {
		md := s.metadata(ctx, pr, &page[i], renderOpts{media: true, state: state})
		if id := itemIDs[lo+i]; id != 0 {
			md.PlaylistItemID = id
		}
		out.rows = append(out.rows, md)
	}
	// The container's duration covers every item. A title with a runtime
	// renders it as its duration; only one without needs its media.
	for i := range selected {
		if i >= lo && i < hi {
			out.durationSec += out.rows[i-lo].Duration / 1000
		} else if selected[i].Runtime > 0 {
			out.durationSec += ms(selected[i].Runtime) / 1000
		} else {
			out.durationSec += s.metadata(ctx, pr, &selected[i], renderOpts{media: true, state: state}).Duration / 1000
		}
	}
	return out, nil
}

func playlistHasItemFilters(q url.Values) bool {
	for key := range q {
		switch key {
		case "sort", "limit", "type", "includeMeta", "X-Plex-Container-Start", "X-Plex-Container-Size":
			continue
		default:
			if !strings.HasPrefix(key, "X-Plex-") {
				return true
			}
		}
	}
	return false
}

func (s *Server) smartPlaylistSQLAvailable() bool {
	c, ok := s.d.Catalog.(interface{ BrowsePlaylistInSQL() bool })
	return ok && c.BrowsePlaylistInSQL()
}

func (s *Server) writeSmartPlaylistItems(w http.ResponseWriter, r *http.Request, pl *frontend.Playlist) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	source, err := s.smartPlaylistRequest(ctx, pr, pl.SmartURI)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if source.invalidSort {
		writeHTTPError(w, 500)
		return
	}
	if source.none {
		source.q.Filter.IncludeKeys = []frontend.RatingKey{}
	}
	if source.unwatched != nil || source.inProgress != nil || source.lastViewedAfter > 0 || source.lastViewedBefore > 0 {
		uid := int64(0)
		if pr.User != nil && s.d.State != nil {
			uid = pr.User.ID
		}
		state := frontend.FilterGroup{UserID: uid, Unwatched: source.unwatched, InProgress: source.inProgress, LastViewedAfter: source.lastViewedAfter, LastViewedBefore: source.lastViewedBefore}
		if source.q.Filter.Group == nil {
			source.q.Filter.Group = &state
		} else {
			source.q.Filter.Group = &frontend.FilterGroup{Operator: "and", Children: []frontend.FilterGroup{*source.q.Filter.Group, state}}
		}
	}
	if source.sortLastViewed {
		source.q.SortLastViewed = true
		source.q.Desc = source.lastViewedDesc
		if pr.User != nil && s.d.State != nil {
			source.q.SortViewerID = pr.User.ID
		}
	}

	if pr.User != nil && source.q.Filter.Type == frontend.TypeEpisode {
		policy := frontend.Filter{MaxContentRating: pr.User.MaxContentRating, SaferPlexRating: s.needPlexRatingFilter(pr.User)}
		if blocks, ok := s.policy().(frontend.BrowseBlocksPolicy); ok {
			policy.ExcludeGenres, policy.ExcludeKeywords = blocks.BrowseBlocks(pr.User)
		}
		if share, ok := s.policy().(frontend.BrowseShareFilterPolicy); ok {
			policy, _ = s.d.Experience.MergeShareFilter(policy, share.BrowseShareFilter(pr.User, frontend.TypeEpisode))
		}
		source.q.Filter.ChildVisibility = &policy
	}
	typ := source.q.Filter.Type
	if q.Has("type") && q.Get("type") != "-1" {
		typ = typeFromParam(q.Get("type"))
		if typ == "" {
			s.write(w, r, &model.MediaContainer{})
			return
		}
	}
	sec, _ := s.sectionFor(typ)
	filters := url.Values{}
	for key, val := range q {
		switch key {
		case "sort", "limit", "type":
			continue
		}
		filters[key] = val
	}
	outer := s.parseBrowse(ctx, sec, typ, pr.User, filters, filters.Encode())
	outer.q.PlaylistSource = &source.q
	outer.q.PlaylistSourceLimit = source.limit
	titles, total, offset, err := s.browse(ctx, pr, outer)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	storedOnly := typ == frontend.TypeEpisode
	sqlAvailability, _ := s.d.Catalog.(interface{ BrowseAvailabilityInSQL() bool })
	rows := s.renderTitles(ctx, pr, titles, renderOpts{media: true, storedOnly: storedOnly, availabilityInSQL: sqlAvailability != nil && sqlAvailability.BrowseAvailabilityInSQL()})
	mc := &model.MediaContainer{RatingKey: pl.Key.String(), Title1: pl.Title, PlaylistType: "video", Smart: model.Ptr(true), LeafCount: model.Ptr(total), Metadata: rows, Size: len(rows)}
	if total > 0 {
		mc.Composite = "/playlists/" + pl.Key.String() + "/composite/" + strconv.FormatInt(unixOrZero(pl.UpdatedAt), 10)
	}
	for _, md := range rows {
		mc.Duration += md.Duration / 1000
	}
	if q.Get("includeMeta") == "1" {
		types := map[frontend.MediaType]bool{source.q.Filter.Type: true}
		if source.q.Filter.Type == frontend.TypeEpisode {
			types[frontend.TypeShow] = true
			types[frontend.TypeSeason] = true
		}
		mc.Meta = playlistItemsMeta(r, types)
	}
	if pr.HasPage {
		mc.SetPage(offset, total)
	}
	s.write(w, r, mc)
}
