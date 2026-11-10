package plex

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func historySection(s *Server, t *frontend.Title) string {
	sec, ok := s.sectionFor(t.Type)
	if !ok {
		return ""
	}
	return strconv.Itoa(sec.ID)
}

// handleHistory is /status/sessions/history/all (the caller's history).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := &model.MediaContainer{TotalSize: model.Ptr(0)}
	q := r.URL.Query()
	// History's type is viewType, not the movie/episode metadata type.
	// Validate identifiers before deciding whether a view type selects rows.
	ids := make(map[string]int64)
	for _, name := range []string{"accountID", "librarySectionID", "metadataItemID", "viewedAt>", "viewedAt<", "viewedAt", "limit", "parent", "offset"} {
		if q.Has(name) {
			n, err := strconv.ParseInt(q.Get(name), 10, 64)
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			ids[name] = n
		}
	}
	listHistory := false
	if q.Has("metadataItemID") {
		key := frontend.RatingKey(ids["metadataItemID"])
		if key <= 0 || s.d.Catalog == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		valid := false
		switch key.Kind() {
		case frontend.KindCollection, frontend.KindSmart:
			c, err := s.collection(ctx, pr.User, key)
			valid = err == nil && c != nil
			listHistory = true
		case frontend.KindPlaylist:
			pl, err := s.playlist(ctx, pr.User, key)
			valid = err == nil && pl != nil
			listHistory = true
		default:
			t, err := s.d.Catalog.Get(ctx, key)
			valid = err == nil && t != nil && s.visibleTitle(ctx, pr.User, t)
		}
		if !valid {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
	}
	if q.Has("group") {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	wantHistory := !q.Has("type")
	viewTypes := map[int]bool{0: !q.Has("type")}
	if q.Has("type") {
		for _, raw := range strings.Split(q.Get("type"), ",") {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			wantHistory = wantHistory || n == 1 || n == 0
			viewTypes[int(n)] = true
		}
	}
	asc := false
	if q.Has("sort") {
		field, desc, ok := parseSort(q.Get("sort"))
		if !ok || field != "viewedAt" {
			writeHTTPError(w, 400)
			return
		}
		asc = !desc
	}
	if offset, ok := ids["offset"]; ok && (offset < -2147483648 || offset > 2147483647) {
		writeHTTPError(w, 400)
		return
	}
	_, size := pr.pageOf(0)
	maxRows := min(size, 500)
	if n, ok := ids["limit"]; ok && n < int64(maxRows) {
		maxRows = int(max(n, 0))
	}
	if listHistory || !wantHistory || maxRows == 0 || s.d.State == nil || pr.User == nil || (!pr.User.IsOwner && q.Has("accountID") && ids["accountID"] != pr.User.PlexID) {
		s.write(w, r, mc)
		return
	}
	accounts := map[int64]int64{pr.User.ID: pr.User.PlexID}
	accountUser := pr.User.ID
	if pr.User.IsOwner && s.host.Identity != nil {
		users, _ := s.listUsers(ctx)
		accountUser = -1
		for _, u := range users {
			accounts[u.ID] = u.PlexID
			if u.PlexID == ids["accountID"] {
				accountUser = u.ID
			}
		}
	}
	var since time.Time
	if v, ok := ids["viewedAt>"]; ok {
		since = time.Unix(v, 0)
	}
	// The existing 500-record ceiling bounds all joins and policy checks,
	// including requests whose section or item filter has few matches.
	hist, err := s.d.State.History(ctx, pr.User.ID, since, 500)
	if store, ok := s.d.State.(frontend.HistoryStore); ok {
		userID := accountUser
		if pr.User.IsOwner && !q.Has("accountID") {
			userID = 0
		}
		vtype := 0
		if viewTypes[1] && !viewTypes[0] {
			vtype = 1
		}
		if viewTypes[0] && viewTypes[1] {
			vtype = -1
		}
		query := frontend.HistoryQuery{UserID: userID, ViewType: vtype, Asc: asc, Offset: int(ids["offset"]), Limit: 500}
		if section, ok := ids["librarySectionID"]; ok {
			sec, found := s.sectionByID(int(section))
			if !found {
				s.write(w, r, mc)
				return
			}
			if sec.Type == frontend.TypeMovie {
				query.Kind = frontend.KindMovie
			} else {
				query.Kind = frontend.KindEpisode
			}
		}

		if v, ok := ids["metadataItemID"]; ok {
			query.Key = frontend.RatingKey(v)
		}
		if v, ok := ids["viewedAt>"]; ok {
			query.Since = &v
		}
		if v, ok := ids["viewedAt<"]; ok {
			query.Until = &v
		}
		if v, ok := ids["viewedAt"]; ok {
			query.At = &v
		}
		if device, err := strconv.ParseInt(q.Get("deviceID"), 10, 64); err == nil {
			query.DeviceID = &device
		}
		hist, err = store.QueryHistory(ctx, query)
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if len(hist) > 500 {
		hist = hist[:500]
	}
	keys := make([]frontend.RatingKey, 0, len(hist))
	for _, h := range hist {
		keys = append(keys, h.Key)
	}
	titles := s.getTitles(ctx, dedupeKeys(keys))
	byKey := map[frontend.RatingKey]*frontend.Title{}
	for i := range titles {
		byKey[titles[i].Key] = &titles[i]
	}
	visible := map[frontend.RatingKey]bool{}
	for _, h := range hist {
		if q.Has("accountID") && accounts[h.UserID] != ids["accountID"] {
			continue
		}
		if !viewTypes[h.ViewType] {
			continue
		}
		if n, ok := ids["viewedAt<"]; ok && h.ViewedAt.Unix() > n {
			continue
		}
		if n, ok := ids["viewedAt"]; ok && h.ViewedAt.Unix() != n {
			continue
		}
		if n, err := strconv.ParseInt(q.Get("deviceID"), 10, 64); err == nil && h.DeviceID != n {
			continue
		}
		t, ok := byKey[h.Key]
		// History is rendered by hand, not through renderTitles: apply the
		// same visibility guard (the show's policy included) so titles
		// restricted after they were watched stay hidden.
		if !ok {
			if h.Snapshot == nil || !pr.User.IsOwner {
				continue
			}
			t = h.Snapshot
		}
		v, seen := visible[t.Key]
		if !seen {
			v = s.visibleTitle(ctx, pr.User, t)
			visible[t.Key] = v
		}
		if !v {
			continue
		}
		if q.Has("metadataItemID") && int64(t.Key) != ids["metadataItemID"] {
			continue
		}
		if q.Has("librarySectionID") {
			sec, ok := s.sectionFor(t.Type)
			if !ok || int64(sec.ID) != ids["librarySectionID"] {
				continue
			}
		}
		md := model.HistoryMetadata{RatingKey: t.Key.String(), Key: "/library/metadata/" + t.Key.String(), Type: string(t.Type), Title: t.Title, Thumb: s.thumbURL(t), HistoryKey: "/status/sessions/history/" + strconv.FormatInt(h.ID, 10), ViewedAt: h.ViewedAt.Unix(), LibrarySectionID: historySection(s, t), DeviceID: h.DeviceID, AccountID: accounts[h.UserID]}

		if !t.Released.IsZero() {
			md.OriginallyAvailableAt = t.Released.Format("2006-01-02")
		}
		if t.Type == frontend.TypeEpisode {
			md.GrandparentTitle = t.ShowTitle
			md.Index = model.Ptr(t.Episode)
			md.ParentIndex = model.Ptr(t.Season)
		}
		if _, exists := byKey[h.Key]; !exists {
			md.Key = ""
			md.RatingKey = ""
		}
		if h.ViewType != 0 {
			md.ViewType = model.Ptr(h.ViewType)
		}
		if t.Type == frontend.TypeEpisode {
			md.ParentKey = "/library/metadata/" + frontend.SeasonKey(t.ShowKey.TMDB(), t.Season).String()
			md.ParentThumb = md.ParentKey + "/thumb"
			md.GrandparentThumb = "/library/metadata/" + t.ShowKey.String() + "/thumb"
			md.GrandparentKey = "/library/metadata/" + t.ShowKey.String()
		}
		mc.History = append(mc.History, md)
		if len(mc.History) == maxRows {
			break
		}
	}
	mc.Size = len(mc.History)
	mc.TotalSize = model.Ptr(mc.Size)
	s.write(w, r, mc)
}

func (s *Server) handleHistoryItem(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "history"), 10, 64)
	if err == nil && id > 2147483647 {
		writeHTTPError(w, 400)
		return
	}
	if err != nil || id <= 0 {
		writeHTTPError(w, 404)
		return
	}
	store, ok := s.d.State.(frontend.HistoryStore)
	if !ok || pr.User == nil {
		writeHTTPError(w, 404)
		return
	}
	uid := pr.User.ID
	if pr.User.IsOwner {
		uid = 0
	}
	h, err := store.HistoryByID(r.Context(), uid, id)
	if err != nil {
		writeHTTPError(w, 404)
		return
	}
	t, err := s.d.Catalog.Get(r.Context(), h.Key)
	if err != nil || t == nil {
		if !pr.User.IsOwner {
			writeHTTPError(w, 404)
			return
		}
		t = h.Snapshot
	}
	if t == nil || !s.visibleTitle(r.Context(), pr.User, t) {
		writeHTTPError(w, 404)
		return
	}
	if r.Method == http.MethodDelete {
		if err = store.DeleteHistory(r.Context(), uid, id); err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.write(w, r, &model.MediaContainer{})
		return
	}
	accountID := pr.User.PlexID
	if pr.User.IsOwner && s.host.Identity != nil {
		users, _ := s.listUsers(r.Context())
		for _, u := range users {
			if u.ID == h.UserID {
				accountID = u.PlexID
			}
		}
	}
	md := model.HistoryMetadata{Title: t.Title, Type: string(t.Type), HistoryKey: "/status/sessions/history/" + strconv.FormatInt(h.ID, 10), LibrarySectionID: historySection(s, t), ViewedAt: h.ViewedAt.Unix(), AccountID: accountID, DeviceID: h.DeviceID, ViewType: model.Ptr(1)}

	if !t.Released.IsZero() {
		md.OriginallyAvailableAt = t.Released.Format("2006-01-02")
	}
	if t.Type == frontend.TypeEpisode {
		md.GrandparentTitle = t.ShowTitle
		md.Index = model.Ptr(t.Episode)
		md.ParentIndex = model.Ptr(t.Season)
	}
	s.write(w, r, &model.MediaContainer{Size: 1, History: []model.HistoryMetadata{md}})
}
