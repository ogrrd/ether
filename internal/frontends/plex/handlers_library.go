package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Plex metadata type numbers.
const (
	typeNumMovie   = 1
	typeNumShow    = 2
	typeNumSeason  = 3
	typeNumEpisode = 4
)

func typeNumber(t frontend.MediaType) int {
	switch t {
	case frontend.TypeMovie:
		return typeNumMovie
	case frontend.TypeShow:
		return typeNumShow
	case frontend.TypeSeason:
		return typeNumSeason
	case frontend.TypeEpisode:
		return typeNumEpisode
	}
	return 0
}

func typeFromParam(s string) frontend.MediaType {
	switch strings.ToLower(s) {
	case "1", "movie", "movies":
		return frontend.TypeMovie
	case "2", "show", "shows", "tv":
		return frontend.TypeShow
	case "3", "season", "seasons":
		return frontend.TypeSeason
	case "4", "episode", "episodes":
		return frontend.TypeEpisode
	case "18", "collection":
		return "collection"
	}
	return ""
}

const viewModeList = 65592

// defaultUnpagedLimit bounds list responses for clients that don't page.
const defaultUnpagedLimit = 500

// plexOnDeckLimit bounds On Deck (Continue Watching) at PMS's OnDeckLimit
// default: "Maximum number of Continue Watching items which will appear."
// /:/prefs reports it.
const plexOnDeckLimit = 40

// plexOnDeckWindow is PMS's OnDeckWindow default, 16 weeks: "Media that has
// not been watched in this many weeks will not appear in Continue
// Watching." With OnDeckIncludePremieres (default on) a new season premiere
// appears however long ago the show was watched. /:/prefs reports both.
const plexOnDeckWindow = 16 * 7 * 24 * time.Hour

// onDeckSince is the start of the On Deck window.
func (s *Server) onDeckSince() time.Time { return s.now().Add(-plexOnDeckWindow) }

// inOnDeckWindow reports a deck row PMS would still show: watched within
// the window (st carries the show's latest view for a next-up episode), or
// a season premiere, episode 1 of a later season, that aired within it.
func inOnDeckWindow(t *frontend.Title, st frontend.WatchStatus, since time.Time) bool {
	if st.LastViewedAt.Unix() <= 0 || !st.LastViewedAt.Before(since) {
		return true // an undated view is not known to be old (the deck query's latest<=0)
	}
	return t.Type == frontend.TypeEpisode && t.Episode == 1 && t.Season > 1 && !t.Released.IsZero() && !t.Released.Before(since)
}

// onDeckWindowed keeps the rows inside the On Deck window, in order.
func (s *Server) onDeckWindowed(items []frontend.Title, state map[frontend.RatingKey]frontend.WatchStatus) []frontend.Title {
	since := s.onDeckSince()
	out := items[:0]
	for i := range items {
		if inOnDeckWindow(&items[i], state[items[i].Key], since) {
			out = append(out, items[i])
		}
	}
	return out
}

// sectionPrefs is /library/sections/{id}/prefs: the library settings the
// apps read (visibility, collection mode, episode sorting and ordering,
// season display), with the values Ether actually behaves by. PMS types
// default/value as strings in this endpoint's JSON, unlike /:/prefs.
// Prefs writes are not supported (Ether's libraries are generated). Library
// titles themselves are configured in Ether's admin UI, not via this endpoint.
func sectionPrefs(sec Section) []model.Setting {
	set := func(id, label, summary, typ, def, val, enum string) model.Setting {
		return model.Setting{ID: id, Label: label, Summary: summary, Type: typ, Default: def, Value: val, EnumValues: enum}
	}
	visibility := set("hidden", "Visibility", "Restrict where content from this library should appear.", "int", "0", "0",
		"0:Include in home screen and global search|1:Exclude from home screen|2:Exclude from home screen and global search")
	collections := set("collectionMode", "Collections", "How to display collections on the Library tab.", "int", "2", "2",
		"0:Hide collections but show their items|1:Hide items which are in collections|2:Show collections and their items")
	original := set("originalTitles", "Use original titles", "Use the original titles for all items regardless of the library language.", "bool", "false", "false", "")
	if sec.Type == frontend.TypeShow {
		return []model.Setting{
			visibility,
			set("episodeSort", "Episode sorting", "How to sort the episodes for shows.", "text", "-1", "-1", "0:Oldest first|1:Newest first"),
			set("showOrdering", "Episode ordering", "Determines the ordering of episodes.", "text", "tmdbAiring", "tmdbAiring", "tmdbAiring:The Movie Database"),
			set("useSeasonTitles", "Use season titles", "Use season titles when available.", "bool", "false", "false", ""),
			original,
			set("enableBIFGeneration", "Enable video preview thumbnails", "Generate video preview thumbnails for items in this library when enabled in server settings.", "bool", "true", "false", ""),
			collections,
			set("flattenSeasons", "Seasons", "Choose whether to show or hide seasons.", "int", "0", "0", "0:Show|2:Hide for single-season series|1:Hide"),
			set("enableIntroMarkerGeneration", "Enable intro detection", "Detect intros and allow intro skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
			set("enableCreditsMarkerGeneration", "Enable credits detection", "Detect credits and allow credits skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
		}
	}
	return []model.Setting{
		visibility,
		set("enableCinemaTrailers", "Enable Cinema Trailers", "Allow Cinema Trailers to play before items in this library.", "bool", "true", "false", ""),
		original,
		set("enableBIFGeneration", "Enable video preview thumbnails", "Generate video preview thumbnails for items in this library when enabled in server settings.", "bool", "true", "false", ""),
		collections,
		set("enableCreditsMarkerGeneration", "Enable credits detection", "Detect credits and allow credits skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
	}
}

func sectionTypeTitle(sec Section) (typeTitle, all string) {
	if sec.Type == frontend.TypeShow {
		return "TV Shows", "All Shows"
	}
	return "Movies", "All Movies"
}

func (s *Server) sectionContainer(sec Section) *model.MediaContainer {
	mc := libraryContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	return mc
}

// sectionParam resolves {section} and checks the user's access.
func (s *Server) sectionParam(w http.ResponseWriter, r *http.Request) (Section, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "section"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return Section{}, false
	}
	sec, ok := s.sectionByID(id)
	if !ok || !s.canSeeSection(FromContext(r.Context()).User, id) {
		writeHTTPError(w, http.StatusNotFound)
		return Section{}, false
	}
	return sec, true
}

func (s *Server) handleLibraryRoot(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{AllowSync: model.Ptr(s.downloadsAllowed(FromContext(r.Context()).User)), Identifier: model.LibraryIdentifier, MediaTagPrefix: model.MediaTagPrefix, MediaTagVersion: model.MediaTagVersion, Title1: "Plex Library"}
	for _, d := range []struct{ key, title string }{{"sections", "Library Sections"}, {"recentlyAdded", "Recently Added Content"}, {"onDeck", "On Deck Content"}} {
		mc.Directory = append(mc.Directory, model.Directory{Key: d.key, Title: d.title})
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

func (s *Server) handleSections(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	mc := &model.MediaContainer{AllowSync: model.Ptr(s.downloadsAllowed(u)), Title1: "Plex Library"}
	now := s.now().Unix()
	for _, sec := range s.userSections(u) {
		agent, scanner := agentFor(sec.Type)
		mc.Directory = append(mc.Directory, model.Directory{
			AllowSync:        model.Ptr(s.downloadsAllowed(u) || u != nil && u.IsOwner && (s.cfg().Plex.Claimed || s.cfg().Plex.Token != "")),
			Filters:          model.Ptr(true),
			Refreshing:       model.Ptr(false),
			Key:              strconv.Itoa(sec.ID),
			Type:             string(sec.Type),
			Title:            sec.Title,
			Agent:            agent,
			Scanner:          scanner,
			Language:         sec.Language,
			UUID:             sec.UUID,
			UpdatedAt:        now,
			CreatedAt:        now,
			ScannedAt:        now,
			Content:          model.Ptr(true),
			IsDirectory:      model.Ptr(true),
			ContentChangedAt: now,
			Hidden:           model.Ptr(0),
			Location:         []model.Location{{ID: sec.ID, Path: "/ether/" + strings.ToLower(strings.ReplaceAll(sec.Title, " ", "-"))}},
		})
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

// handleSection is /library/sections/{id}: the section's listing directory
// (and its Meta when includeDetails/includeMeta is set).
func (s *Server) handleSection(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	typeTitle, allTitle := sectionTypeTitle(sec)
	tn := strconv.Itoa(typeNumber(sec.Type))
	mc := libraryContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	mc.Content = "secondary"
	mc.ViewGroup = "secondary"
	sub := []struct {
		key, title string
		secondary  bool
	}{
		{"all", allTitle, false},
		{"unwatched", "Unwatched", false},
		{"newest", "Recently Released", false},
		{"recentlyAdded", "Recently Added", false},
		{"recentlyViewed", "Recently Viewed", false},
		{"onDeck", "Continue Watching", false},
		{"collection", "By Collection", true},
		{"genre", "By Genre", true},
		{"year", "By Year", true},
		{"decade", "By Decade", true},
		{"country", "By Country", true},
		{"contentRating", "By Content Rating", true},
		{"firstCharacter", "By First Letter", true},
	}
	for _, d := range sub {
		dir := model.Directory{Key: d.key, Title: d.title}
		if d.secondary {
			dir.Secondary = model.Ptr(true)
		}
		mc.Directory = append(mc.Directory, dir)
	}
	mc.Directory = append(mc.Directory, model.Directory{Key: "search?type=" + tn, Prompt: "Search " + typeTitle, Search: model.Ptr(true), Title: "Search..."})
	mc.Size = len(mc.Directory)
	q := r.URL.Query()
	switch {
	case q.Get("includeDetails") == "1":
		// Section details carry the filter schema at the root, as PMS does:
		// Plex Web's library details (Advanced Filters) read Type and
		// FieldType there and spin forever on a nested Meta (#273).
		meta := s.meta(sec, sec.Type, "")
		mc.Type, mc.FieldType = meta.Type, meta.FieldType
	case q.Get("includeMeta") == "1":
		mc.Meta = s.meta(sec, sec.Type, "")
	}
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Meta: types, filters, sorts, fields

type sortDef struct {
	key, title, defDir string
}

var sortDefs = []sortDef{
	{"titleSort", "Title", "asc"},
	{"originallyAvailableAt", "Release Date", "desc"},
	{"rating", "Critic Rating", "desc"},
	{"audienceRating", "Audience Rating", "desc"},
	{"addedAt", "Date Added", "desc"},
	{"popularity", "Popularity", "desc"},
}

type filterDef struct {
	filter, filterType, title string
}

var filterDefs = []filterDef{
	{"genre", "string", "Genre"},
	{"year", "integer", "Year"},
	{"decade", "integer", "Decade"},
	{"contentRating", "string", "Content Rating"},
	{"collection", "string", "Collection"},
	{"country", "string", "Country"},
	{"studio", "string", "Studio"},
	{"unwatched", "boolean", "Unwatched"},
}

func (s *Server) metaType(sec Section, t frontend.MediaType, activeSort string) model.MetaType {
	id := strconv.Itoa(sec.ID)
	tn := strconv.Itoa(typeNumber(t))
	typeTitle, _ := sectionTypeTitle(sec)
	mt := model.MetaType{Key: "/library/sections/" + id + "/all?type=" + tn, Type: string(t), Title: typeTitle, Active: true}
	for _, f := range filterDefs {
		key := "/library/sections/" + id + "/" + f.filter
		mt.Filter = append(mt.Filter, model.Filter{Filter: f.filter, FilterType: f.filterType, Key: key, Title: f.title, Type: "filter"})
	}
	activeKey, activeDir := parseSortParam(activeSort)
	for i, sd := range sortDefs {
		so := model.Sort{DefaultDirection: sd.defDir, DescKey: sd.key + ":desc", Key: sd.key, Title: sd.title}
		if sd.key == "titleSort" {
			so.FirstCharacterKey = "/library/sections/" + id + "/firstCharacter"
		}
		if i == 0 {
			so.Default = sd.defDir
		}
		if (activeKey == "" && i == 0) || activeKey == sd.key {
			so.Active = model.Ptr(true)
			dir := activeDir
			if dir == "" {
				dir = sd.defDir
			}
			so.ActiveDirection = dir
		}
		mt.Sort = append(mt.Sort, so)
	}
	mt.Field = []model.Field{
		{Key: "title", Title: "Title", Type: "string"},
		{Key: "studio", Title: "Studio", Type: "string"},
		{Key: "contentRating", Title: "Content Rating", Type: "tag"},
		{Key: "year", Title: "Year", Type: "integer", SubType: "year"},
		{Key: "decade", Title: "Decade", Type: "integer", SubType: "decade"},
		{Key: "originallyAvailableAt", Title: "Release Date", Type: "date"},
		{Key: "duration", Title: "Duration", Type: "integer", SubType: "duration"},
		{Key: "genre", Title: "Genre", Type: "tag"},
		{Key: "collection", Title: "Collection", Type: "tag"},
		{Key: "director", Title: "Director", Type: "tag"},
		{Key: "writer", Title: "Writer", Type: "tag"},
		{Key: "actor", Title: "Actor", Type: "tag"},
		{Key: "country", Title: "Country", Type: "tag"},
		{Key: "audienceRating", Title: "Audience Rating", Type: "integer", SubType: "rating"},
		{Key: "unwatched", Title: "Unwatched", Type: "boolean"},
		{Key: "inProgress", Title: "In Progress", Type: "boolean"},
	}
	return mt
}

func (s *Server) meta(sec Section, t frontend.MediaType, activeSort string) *model.Meta {
	return &model.Meta{
		Type: []model.MetaType{s.metaType(sec, t, activeSort)},
		FieldType: []model.FieldType{
			{Type: "tag", Operator: []model.Operator{{Key: "=", Title: "is"}, {Key: "!=", Title: "is not"}}},
			{Type: "integer", Operator: []model.Operator{{Key: "=", Title: "is"}, {Key: "!=", Title: "is not"}, {Key: ">>=", Title: "is greater than"}, {Key: "<<=", Title: "is less than"}}},
			{Type: "string", Operator: []model.Operator{{Key: "=", Title: "contains"}, {Key: "!=", Title: "does not contain"}, {Key: "==", Title: "is"}, {Key: "!==", Title: "is not"}, {Key: "<=", Title: "begins with"}, {Key: ">=", Title: "ends with"}}},
			{Type: "boolean", Operator: []model.Operator{{Key: "=", Title: "is true"}, {Key: "!=", Title: "is false"}}},
			{Type: "date", Operator: []model.Operator{{Key: "<<=", Title: "is before"}, {Key: ">>=", Title: "is after"}}},
		},
	}
}

func parseSortTerm(term string) (key, dir string) {
	key, opts, _ := strings.Cut(strings.TrimSpace(term), ":")
	key = stripFieldTypePrefix(key)
	for _, o := range strings.Split(opts, ":") {
		if o == "desc" || o == "asc" {
			dir = o
		}
	}
	return key, dir
}

func parseSortParam(s string) (key, dir string) {
	first, _, _ := strings.Cut(s, ",")
	return parseSortTerm(first)
}

// ---------------------------------------------------------------------------
// Browse

type browseRequest struct {
	q                frontend.BrowseQuery
	unwatched        *bool // true = unwatched (ViewCount == 0), false = watched (ViewCount > 0)
	inProgress       *bool // true = in progress (ViewOffset > 0), false = not in progress (ViewOffset == 0)
	sortLastViewed   bool
	lastViewedDesc   bool
	invalidSort      bool
	lastViewedAfter  int64
	lastViewedBefore int64
	limit            int // "limit" param; 0 = none
	// none: the viewer's plex.tv share and the request's own filter admit
	// no rating in common, so the listing is empty.
	none bool
}

func stripFieldTypePrefix(key string) string {
	if key == "title.query" {
		return key
	}
	if prefix, rest, ok := strings.Cut(key, "."); ok && rest != "" && typeFromParam(prefix) != "" {
		return rest
	}
	return key
}

// parseFilterKeyOp extracts the field name, comparison operator ("=", "!=",
// "==", "!==", "<=", ">=", "<<", ">>", "<", ">"), and value from a URL query
// entry. Plex media queries encode the operator before the "=" key-value
// delimiter (e.g. "genre!=Drama" parses as key "genre!", value "Drama";
// "title==Foo" parses as key "title", value "=Foo"; "title!==Foo" parses as
// key "title!", value "=Foo").
func parseFilterKeyOp(rawKey, rawVal string) (key, op, val string) {
	if rawVal == "" {
		for _, sep := range []string{"!==", "==", ">>=", "<<=", ">=", "<=", "!=", ">>", "<<", ">", "<"} {
			if k, v, ok := strings.Cut(rawKey, sep); ok && k != "" && v != "" {
				switch sep {
				case ">>=":
					sep = ">>"
				case "<<=":
					sep = "<<"
				}
				return stripFieldTypePrefix(k), sep, v
			}
		}
	}
	key, op, val = rawKey, "=", rawVal
	switch {
	case strings.HasSuffix(rawKey, "!=="):
		key, op = strings.TrimSuffix(rawKey, "!=="), "!=="
	case strings.HasSuffix(rawKey, "=="):
		key, op = strings.TrimSuffix(rawKey, "=="), "=="
	case strings.HasSuffix(rawKey, ">>="):
		key, op = strings.TrimSuffix(rawKey, ">>="), ">>"
	case strings.HasSuffix(rawKey, "<<="):
		key, op = strings.TrimSuffix(rawKey, "<<="), "<<"
	case strings.HasSuffix(rawKey, ">="):
		key, op = strings.TrimSuffix(rawKey, ">="), ">="
	case strings.HasSuffix(rawKey, "<="):
		key, op = strings.TrimSuffix(rawKey, "<="), "<="
	case strings.HasSuffix(rawKey, "!="):
		key, op = strings.TrimSuffix(rawKey, "!="), "!="
	case strings.HasSuffix(rawKey, ">>"):
		key, op = strings.TrimSuffix(rawKey, ">>"), ">>"
	case strings.HasSuffix(rawKey, "<<"):
		key, op = strings.TrimSuffix(rawKey, "<<"), "<<"
	case strings.HasSuffix(rawKey, ">"):
		key, op = strings.TrimSuffix(rawKey, ">"), ">="
	case strings.HasSuffix(rawKey, "<"):
		key, op = strings.TrimSuffix(rawKey, "<"), "<="
	case strings.HasSuffix(rawKey, "!"):
		key = strings.TrimSuffix(rawKey, "!")
		if strings.HasPrefix(rawVal, "=") {
			op, val = "!==", rawVal[1:]
		} else {
			op = "!="
		}
	case strings.HasSuffix(rawKey, "="):
		key, op = strings.TrimSuffix(rawKey, "="), "=="
	default:
		if strings.HasPrefix(rawVal, "=") {
			op, val = "==", rawVal[1:]
		} else {
			for _, pfx := range []string{">>=", "<<=", ">=", "<=", ">>", "<<", ">", "<"} {
				if strings.HasPrefix(rawVal, pfx) {
					op, val = pfx, strings.TrimPrefix(rawVal, pfx)
					switch op {
					case ">>=":
						op = ">>"
					case "<<=":
						op = "<<"
					}
					break
				}
			}
		}
	}
	return stripFieldTypePrefix(key), op, val
}

func parseBoolFilter(op, val string) (bool, bool) {
	return (op == "=" && val == "1") || (op == "!=" && val == "0"), true
}

func parseDateFilter(op, raw string, now time.Time) (frontend.DateFilter, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return frontend.DateFilter{}, false
	}
	if strings.HasPrefix(v, "-") {
		rel := v[1:]
		for _, unit := range []string{"mon", "s", "m", "h", "d", "w", "y"} {
			if strings.HasSuffix(rel, unit) {
				n, err := strconv.Atoi(strings.TrimSuffix(rel, unit))
				if err != nil || n < 0 {
					return frontend.DateFilter{}, false
				}
				var ts int64
				switch unit {
				case "s":
					ts = now.Add(-time.Duration(n) * time.Second).Unix()
				case "m":
					ts = now.Add(-time.Duration(n) * time.Minute).Unix()
				case "h":
					ts = now.Add(-time.Duration(n) * time.Hour).Unix()
				case "d":
					ts = now.AddDate(0, 0, -n).Unix()
				case "w":
					ts = now.AddDate(0, 0, -7*n).Unix()
				case "mon":
					ts = now.AddDate(0, -n, 0).Unix()
				case "y":
					ts = now.AddDate(-n, 0, 0).Unix()
				}
				return frontend.DateFilter{Op: op, Unix: ts}, true
			}
		}
	}
	if tm, err := time.Parse("2006-01-02", v); err == nil {
		return frontend.DateFilter{Op: op, Unix: tm.UTC().Unix(), SpanDay: true}, true
	}
	if tm, err := time.Parse(time.RFC3339, v); err == nil {
		return frontend.DateFilter{Op: op, Unix: tm.UTC().Unix()}, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return frontend.DateFilter{Op: op, Unix: n}, true
	}
	return frontend.DateFilter{}, false
}

func parseFilterTime(v string, now time.Time) (ts int64, dateOnly bool, ok bool) {
	df, ok := parseDateFilter("=", v, now)
	if !ok {
		return 0, false, false
	}
	return df.Unix, df.SpanDay, true
}

func applyTimeFilter(op string, ts int64, dateOnly bool, after, before *int64) {
	switch op {
	case ">=", ">>", ">>=":
		if ts > *after {
			*after = ts
		}
	case ">":
		if ts+1 > *after {
			*after = ts + 1
		}
	case "<=", "<<", "<<=":
		end := ts
		if dateOnly {
			end += 86399
		}
		if *before == 0 || end < *before {
			*before = end
		}
	case "<":
		if *before == 0 || ts-1 < *before {
			*before = ts - 1
		}
	case "=", "==":
		if ts > *after {
			*after = ts
		}
		end := ts
		if dateOnly {
			end += 86399
		}
		if *before == 0 || end < *before {
			*before = end
		}
	}
}

// parseBrowse converts Plex filter/sort params into a catalogue query.
func (s *Server) parseBrowse(ctx context.Context, sec Section, t frontend.MediaType, u *frontend.User, q url.Values, raw ...string) browseRequest {
	br := browseRequest{}
	var group *frontend.FilterGroup
	if len(raw) > 0 && (q.Has("push") || q.Has("or") || q.Has("pop") || q.Has("and")) {
		tree := parseQueryTree(raw[0], typeNumber(t), nil)
		var convert func(*queryRule) *frontend.FilterGroup
		convert = func(rule *queryRule) *frontend.FilterGroup {
			if rule == nil {
				return nil
			}
			if rule.Type == "composite" {
				g := &frontend.FilterGroup{Operator: rule.Operator}
				for i := range rule.Rule {
					if child := convert(&rule.Rule[i]); child != nil {
						g.Children = append(g.Children, *child)
					}
				}
				if len(g.Children) == 0 {
					return nil
				}
				return g
			}
			if rule.Value == nil {
				return nil
			}
			key := rule.Field + strings.TrimSuffix(rule.Operator, "=")
			parsed := s.parseBrowse(ctx, sec, t, u, url.Values{key: []string{*rule.Value}})
			if parsed.unwatched != nil || parsed.inProgress != nil || parsed.lastViewedAfter > 0 || parsed.lastViewedBefore > 0 {
				var id int64
				if u != nil && s.d.State != nil {
					id = u.ID
				}
				return &frontend.FilterGroup{Unwatched: parsed.unwatched, InProgress: parsed.inProgress, LastViewedAfter: parsed.lastViewedAfter, LastViewedBefore: parsed.lastViewedBefore, UserID: id}
			}
			return &frontend.FilterGroup{Filter: &parsed.q.Filter}
		}
		group = convert(tree.Rule)
		// Keep response-shaping arguments, but rules are represented by the tree.
		reduced := url.Values{}
		for k, v := range q {
			if queryParamIgnored(k) || k == "type" || k == "sort" {
				reduced[k] = v
			}
		}
		q = reduced
	}
	f := frontend.Filter{Type: t, Group: group}
	if u != nil {
		f.MaxContentRating = u.MaxContentRating
	}

	if blocks, ok := s.d.AccessPolicy.(interface {
		BrowseBlocks(*frontend.User) ([]string, []string)
	}); ok {
		f.ExcludeGenres, f.ExcludeKeywords = blocks.BrowseBlocks(u)
	}
	f.SaferPlexRating = s.plexMetaOn()
	splitVals := func(v string) []string {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	atoi := func(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
	minPos := func(cur, n int) int {
		if cur == 0 || n < cur {
			return n
		}
		return cur
	}
	now := s.now()
	rawKeys := make([]string, 0, len(q))
	for k := range q {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)
	for _, rawKey := range rawKeys {
		for _, rawVal := range q[rawKey] {
			key, op, v := parseFilterKeyOp(rawKey, rawVal)
			isNeg := op == "!=" || op == "!=="
			switch key {
			case "genre":
				for _, g := range splitVals(v) {
					name := s.facetTitle(ctx, t, "genre", g)
					if isNeg {
						f.ExcludeGenres = append(f.ExcludeGenres, name)
					} else {
						f.Genres = append(f.Genres, name)
					}
				}
			case "country":
				for _, c := range splitVals(v) {
					name := s.facetTitle(ctx, t, "country", c)
					if isNeg {
						f.ExcludeCountries = append(f.ExcludeCountries, name)
					} else {
						f.Countries = append(f.Countries, name)
					}
				}
			case "contentRating":
				for _, c := range splitVals(v) {
					name := s.facetTitle(ctx, t, "contentRating", c)
					if isNeg {
						f.ExcludeContentRatings = append(f.ExcludeContentRatings, name)
					} else {
						f.ContentRatings = append(f.ContentRatings, name)
					}
				}
			case "studio":
				name := s.facetTitle(ctx, t, "studio", v)
				if op == "=" && f.Studio == "" && len(f.StudioFilters) == 0 {
					f.Studio = name
				}
				f.StudioFilters = append(f.StudioFilters, frontend.StringFilter{Op: op, Value: name})
			case "firstCharacter":
				f.FirstCharacter = v
			case "language":
				f.Languages = append(f.Languages, splitVals(v)...)
			case "year":
				switch op {
				case ">>", ">":
					if n := atoi(v); n > 0 {
						f.YearFrom = max(f.YearFrom, n+1)
					}
				case ">=":
					if n := atoi(v); n > 0 {
						f.YearFrom = max(f.YearFrom, n)
					}
				case "<<", "<":
					if n := atoi(v); n > 1 {
						f.YearTo = minPos(f.YearTo, n-1)
					}
				case "<=":
					if n := atoi(v); n > 0 {
						f.YearTo = minPos(f.YearTo, n)
					}
				case "!=", "!==":
					for _, y := range splitVals(v) {
						if n := atoi(y); n > 0 {
							f.ExcludeYears = append(f.ExcludeYears, n)
						}
					}
				default:
					var ns []int
					for _, y := range splitVals(v) {
						if n := atoi(y); n > 0 {
							ns = append(ns, n)
						}
					}
					if len(ns) == 1 && len(f.Years) == 0 && f.YearFrom == 0 && f.YearTo == 0 {
						f.YearFrom, f.YearTo = ns[0], ns[0]
					} else if len(ns) > 0 {
						if f.YearFrom > 0 && f.YearFrom == f.YearTo && len(f.Years) == 0 {
							f.Years = append(f.Years, f.YearFrom)
							f.YearFrom, f.YearTo = 0, 0
						}
						f.Years = append(f.Years, ns...)
					}
				}
			case "decade":
				if _, err := strconv.ParseInt(v, 10, 32); err != nil {
					br.invalidSort = true
					continue
				}
				switch op {
				case ">>", ">":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearFrom = max(f.YearFrom, d+10)
					}
				case ">=":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearFrom = max(f.YearFrom, d)
					}
				case "<<", "<":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearTo = minPos(f.YearTo, d-1)
					}
				case "<=":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearTo = minPos(f.YearTo, d+9)
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if d := (atoi(p) / 10) * 10; d > 0 {
							for y := d; y <= d+9; y++ {
								f.ExcludeYears = append(f.ExcludeYears, y)
							}
						}
					}
				default:
					var decs []int
					for _, p := range splitVals(v) {
						if d := (atoi(p) / 10) * 10; d > 0 {
							decs = append(decs, d)
						}
					}
					if len(decs) == 1 && len(f.Years) == 0 {
						f.YearFrom = max(f.YearFrom, decs[0])
						f.YearTo = minPos(f.YearTo, decs[0]+9)
					} else if len(decs) > 1 {
						for _, d := range decs {
							for y := d; y <= d+9; y++ {
								f.Years = append(f.Years, y)
							}
						}
					}
				}
			case "originallyAvailableAt":
				if df, ok := parseDateFilter(op, v, now); ok {
					f.ReleasedFilters = append(f.ReleasedFilters, df)
				}
			case "addedAt":
				if ts, dateOnly, ok := parseFilterTime(v, now); ok {
					applyTimeFilter(op, ts, dateOnly, &f.AddedAfter, &f.AddedBefore)
				}
			case "updatedAt":
				if ts, dateOnly, ok := parseFilterTime(v, now); ok {
					applyTimeFilter(op, ts, dateOnly, &f.UpdatedAfter, &f.UpdatedBefore)
				}
			case "lastViewedAt", "lastRatedAt", "lastSkippedAt":
				ts, _, ok := parseFilterTime(v, now)
				if op == "=" {
					n, err := strconv.ParseInt(v, 10, 32)
					ok = err == nil
					ts = n
				}
				if !ok {
					br.invalidSort = true
					continue
				}
				var id int64
				if u != nil {
					id = u.ID
				}
				state := frontend.FilterGroup{UserID: id, StateField: key, StateOperator: op, StateValue: ts}
				if f.Group == nil {
					f.Group = &state
				} else {
					f.Group = &frontend.FilterGroup{Operator: "and", Children: []frontend.FilterGroup{*f.Group, state}}
				}
			case "collection":
				var ids []int64
				for _, p := range splitVals(v) {
					if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
						if rk := frontend.RatingKey(id); rk.Kind() == frontend.KindCollection {
							id = rk.TMDB()
						}
						ids = append(ids, id)
					}
				}
				if len(ids) > 0 {
					if isNeg {
						f.ExcludeCollections = append(f.ExcludeCollections, ids...)
					} else {
						if len(ids) == 1 && f.Collection == 0 && len(f.Collections) == 0 {
							f.Collection = ids[0]
						}
						f.Collections = append(f.Collections, ids...)
					}
				}
			case "actor", "director", "writer", "producer":
				kind := "cast"
				switch key {
				case "director":
					kind = "director"
				case "writer":
					kind = "writer"
				case "producer":
					kind = "crew"
				}
				var ids []int64
				var names []string
				for _, p := range splitVals(v) {
					if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
						ids = append(ids, id)
					} else {
						names = append(names, p)
					}
				}
				if len(ids) > 0 || len(names) > 0 {
					if isNeg {
						f.Credits = append(f.Credits, frontend.CreditFilter{People: ids, Names: names, Kind: kind, Exclude: true})
					} else {
						if len(ids) == 1 && len(names) == 0 && f.Person == 0 {
							f.Person = ids[0]
						}
						f.Credits = append(f.Credits, frontend.CreditFilter{People: ids, Names: names, Kind: kind})
					}
				}
			case "title":
				if op == "=" && f.Query == "" && len(f.TitleFilters) == 0 {
					f.Query = v
				} else {
					f.TitleFilters = append(f.TitleFilters, frontend.StringFilter{Op: op, Value: v})
				}
			case "audienceRating", "rating":
				switch op {
				case ">>", ">=", ">":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						f.MinRating = max(f.MinRating, x)
					}
				case "<<", "<=", "<":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						if f.MaxRating == 0 || x < f.MaxRating {
							f.MaxRating = x
						}
					}
				case "=", "==":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						f.MinRating = max(f.MinRating, x)
						if f.MaxRating == 0 || x < f.MaxRating {
							f.MaxRating = x
						}
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if x, err := strconv.ParseFloat(p, 64); err == nil && x > 0 {
							f.ExcludeRatings = append(f.ExcludeRatings, x)
						}
					}
				}
			case "duration":
				switch op {
				case "<<", "<=", "<":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						if f.MaxRuntime == 0 || d < f.MaxRuntime {
							f.MaxRuntime = d
						}
					}
				case ">>", ">=", ">":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						if d > f.MinRuntime {
							f.MinRuntime = d
						}
					}
				case "=", "==":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						f.MinRuntime, f.MaxRuntime = d, d
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if n := atoi(p); n > 0 {
							f.ExcludeRuntimes = append(f.ExcludeRuntimes, time.Duration(n)*time.Millisecond)
						}
					}
				}
			case "unwatched":
				if want, ok := parseBoolFilter(op, v); ok {
					br.unwatched = &want
				}
			case "inProgress":
				if want, ok := parseBoolFilter(op, v); ok {
					br.inProgress = &want
				}
			case "viewCount", "viewOffset", "skipCount", "viewedLeafCount", "viewedOrInProgressLeafCount", "unviewedLeafCount", "unwatchedLeaves":
				n, err := strconv.ParseInt(v, 10, 32)
				if key == "unwatchedLeaves" {
					n = 0
					if op == "=" && v == "1" || op == "!=" && v == "0" {
						n = 1
					}
					err = nil
				}
				if err != nil {
					br.invalidSort = true
					continue
				}
				var id int64
				if u != nil {
					id = u.ID
				}
				state := frontend.FilterGroup{UserID: id, StateField: key, StateOperator: op, StateValue: n}
				if f.Group == nil {
					f.Group = &state
				} else {
					f.Group = &frontend.FilterGroup{Operator: "and", Children: []frontend.FilterGroup{*f.Group, state}}
				}
			case "limit":
				br.limit = atoi(v)
			}
		}
	}
	br.applySortParam(q.Get("sort"), t)
	if share, ok := s.d.AccessPolicy.(interface {
		BrowseShareFilter(*frontend.User, frontend.MediaType) frontend.Filter
	}); ok && u != nil {
		// The plex.tv share predicate goes into the SQL, so pages fill
		// and totals count only what this viewer may see.
		f, br.none = s.d.Experience.MergeShareFilter(f, share.BrowseShareFilter(u, t))
	}
	br.q.Filter = f
	return br
}

func (br *browseRequest) applySortParam(raw string, t frontend.MediaType) {
	if strings.TrimSpace(raw) == "" {
		if t == frontend.TypeEpisode || t == frontend.TypeSeason {
			br.q.Sort, br.q.Desc = "", false
		} else {
			br.q.Sort, br.q.Desc = frontend.SortTitle, false
		}
		return
	}
	terms := strings.Split(raw, ",")
	for i, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		key, dir := parseSortTerm(term)
		so, desc, lastViewed, ok := sortSpecFor(key, dir)
		if !ok {
			br.invalidSort = true
			return
		}
		if i == 0 {
			if lastViewed {
				br.sortLastViewed = true
				br.lastViewedDesc = desc
				if t == frontend.TypeEpisode || t == frontend.TypeSeason {
					br.q.Sort, br.q.Desc = "", false
				} else {
					br.q.Sort, br.q.Desc = frontend.SortTitle, false
				}
			} else {
				br.q.Sort, br.q.Desc = so, desc
			}
		} else if !lastViewed {
			br.q.SecondarySorts = append(br.q.SecondarySorts, frontend.SortSpec{Sort: so, Desc: desc})
		}
	}
	if len(br.q.SecondarySorts) > 0 && t != frontend.TypeEpisode && t != frontend.TypeSeason {
		hasTitle := br.q.Sort == frontend.SortTitle
		for _, s := range br.q.SecondarySorts {
			if s.Sort == frontend.SortTitle {
				hasTitle = true
				break
			}
		}
		if !hasTitle {
			br.q.SecondarySorts = append(br.q.SecondarySorts, frontend.SortSpec{Sort: frontend.SortTitle, Desc: false})
		}
	}
}

func sortSpecFor(key, dir string) (so frontend.SortOrder, desc, lastViewed, valid bool) {
	def := true // desc
	valid = true
	switch key {
	case "titleSort", "title", "":
		so, def = frontend.SortTitle, false
	case "originallyAvailableAt", "releaseDate":
		so = frontend.SortReleased
	case "year":
		so = frontend.SortYear
	case "rating", "audienceRating", "userRating":
		so = frontend.SortRating
	case "addedAt", "createdAt":
		so = frontend.SortAdded
	case "lastViewedAt", "viewCount":
		so, lastViewed = frontend.SortTitle, true
	case "updatedAt":
		so = frontend.SortUpdated
	case "duration":
		so = frontend.SortDuration
	case "popularity", "random":
		so = frontend.SortPopularity
	default:
		so = frontend.SortPopularity
		valid = false
	}
	switch dir {
	case "desc":
		return so, true, lastViewed, valid
	case "asc":
		return so, false, lastViewed, valid
	}
	return so, def, lastViewed, valid
}

// maxPageSize bounds an explicit X-Plex-Container-Size handed to backends:
// far above any real client page, far below where offset+size overflows.
const maxPageSize = 10_000

// pageOf returns the page window the client asked for.
func (pr *Request) pageOf(limit int) (offset, size int) {
	offset, size = 0, defaultUnpagedLimit
	if pr.HasPage {
		offset = pr.Start
		if pr.Size >= 0 {
			size = min(pr.Size, maxPageSize)
		}
	}
	if limit > 0 {
		if offset >= limit {
			return offset, 0
		}
		size = min(size, limit-offset)
	}
	return offset, size
}

func titleSortKey(t frontend.Title) string {
	if t.SortTitle != "" {
		return strings.ToLower(t.SortTitle)
	}
	return strings.ToLower(t.Title)
}

// browse runs a catalogue query honouring paging and watch-state filters.
func (s *Server) browse(ctx context.Context, pr *Request, br browseRequest) ([]frontend.Title, int, int, error) {
	offset, size := pr.pageOf(br.limit)
	if pr.User != nil && s.d.Playback != nil {
		for _, session := range s.d.Playback.Sessions() {
			if session != nil && session.UserID == pr.User.ID {
				br.q.Filter.AvailabilityKeepKeys = append(br.q.Filter.AvailabilityKeepKeys, session.Key)
			}
		}
	}
	if pr.User != nil && br.q.Filter.Type == frontend.TypeEpisode {
		policy := frontend.Filter{MaxContentRating: pr.User.MaxContentRating, SaferPlexRating: s.needPlexRatingFilter(pr.User)}
		if blocks, ok := s.d.AccessPolicy.(interface {
			BrowseBlocks(*frontend.User) ([]string, []string)
		}); ok {
			policy.ExcludeGenres, policy.ExcludeKeywords = blocks.BrowseBlocks(pr.User)
		}
		if share, ok := s.d.AccessPolicy.(interface {
			BrowseShareFilter(*frontend.User, frontend.MediaType) frontend.Filter
		}); ok {
			policy, _ = s.d.Experience.MergeShareFilter(policy, share.BrowseShareFilter(pr.User, frontend.TypeEpisode))
		}
		br.q.Filter.ChildVisibility = &policy
	}
	if br.invalidSort {
		return nil, 0, offset, fmt.Errorf("plex: unknown sort")
	}
	if br.none {
		return nil, 0, offset, nil
	}
	// Apply watch predicates before catalogue pagination and counting.
	if br.unwatched != nil || br.inProgress != nil || br.lastViewedAfter > 0 || br.lastViewedBefore > 0 {
		var id int64
		if pr.User != nil && s.d.State != nil {
			id = pr.User.ID
		}
		state := frontend.FilterGroup{UserID: id, Unwatched: br.unwatched, InProgress: br.inProgress, LastViewedAfter: br.lastViewedAfter, LastViewedBefore: br.lastViewedBefore}
		if br.q.Filter.Group == nil {
			br.q.Filter.Group = &state
		} else {
			br.q.Filter.Group = &frontend.FilterGroup{Operator: "and", Children: []frontend.FilterGroup{*br.q.Filter.Group, state}}
		}
		br.unwatched, br.inProgress = nil, nil
		br.lastViewedAfter, br.lastViewedBefore = 0, 0
	}
	if br.sortLastViewed {
		br.q.SortLastViewed = true
		br.q.Desc = br.lastViewedDesc
		if pr.User != nil && s.d.State != nil {
			br.q.SortViewerID = pr.User.ID
		}
		br.sortLastViewed = false
	}
	_, hasBrowseBlocks := s.d.AccessPolicy.(interface {
		BrowseBlocks(*frontend.User) ([]string, []string)
	})
	needsPostPolicy := s.d.AccessPolicy != nil && !hasBrowseBlocks && s.viewerRestricted(pr.User)
	if !needsPostPolicy {
		q := br.q
		q.Offset, q.Limit = offset, size
		if size == 0 {
			q.Limit = 1 // learn the total without rendering items
		}
		page, err := s.d.Catalog.Browse(ctx, q)
		if err != nil {
			return nil, 0, offset, err
		}
		items := page.Items
		if size == 0 {
			items = nil
		}
		total := page.Total
		if br.limit > 0 && total > br.limit {
			total = br.limit
		}
		return items, total, offset, nil
	}
	// Custom policies without SQL predicates are evaluated in bounded chunks.
	const chunk, maxScan = 200, 5000
	var out []frontend.Title
	matched, scanned, total := 0, 0, 0
	for scanned < maxScan {
		q := br.q
		q.Offset, q.Limit = scanned, chunk
		page, err := s.d.Catalog.Browse(ctx, q)
		if err != nil {
			return nil, 0, offset, err
		}
		total = page.Total
		if len(page.Items) == 0 {
			break
		}
		for _, t := range page.Items {
			if needsPostPolicy && !s.visibleTitle(ctx, pr.User, &t) {
				total--
				continue
			}
			if matched >= offset && len(out) < size {
				out = append(out, t)
			}
			matched++
		}
		scanned += len(page.Items)
		if scanned >= page.Total {
			break
		}
	}
	if total < matched {
		total = matched
	}
	if br.limit > 0 && total > br.limit {
		total = br.limit
	}
	return out, total, offset, nil
}

// handleSectionSub dispatches /library/sections/{id}/{sub}.
func (s *Server) handleSectionSub(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	sub := chi.URLParam(r, "sub")
	if (sub == "unwatched" || sub == "onDeck") && r.URL.Query().Has("type") {
		if _, err := strconv.ParseInt(r.URL.Query().Get("type"), 10, 64); err != nil {
			writeHTTPError(w, http.StatusInternalServerError)
			return
		}
	}
	switch sub {
	case "all", "allLeaves", "unwatched", "newest", "recentlyAdded", "recentlyViewedShows", "search", "autocomplete":
		s.sectionList(w, r, sec, sub)
	case "onDeck":
		s.sectionOnDeck(w, r, sec)
	case "recentlyViewed":
		s.sectionRecentlyViewed(w, r, sec)
	case "collections", "collection":
		s.sectionCollections(w, r, sec, sub)
	case "genre", "year", "decade", "contentRating", "country", "studio", "firstCharacter", "categories":
		s.sectionFacet(w, r, sec, sub)
	case "sorts":
		mt := s.metaType(sec, sec.Type, r.URL.Query().Get("sort"))
		mc := &model.MediaContainer{Size: len(mt.Sort), Sort: mt.Sort}
		s.write(w, r, mc)
	case "filters":
		mt := s.metaType(sec, sec.Type, "")
		var dirs []model.Directory
		for _, f := range mt.Filter {
			dirs = append(dirs, model.Directory{Key: f.Key, Title: f.Title, Type: "filter", Extra: []model.Attr{model.A("filter", f.Filter), model.A("filterType", f.FilterType)}})
		}
		s.write(w, r, &model.MediaContainer{Size: len(dirs), Directory: dirs})
	case "prefs":
		settings := sectionPrefs(sec)
		s.write(w, r, &model.MediaContainer{Size: len(settings), Setting: settings})
	case "refresh", "analyze", "emptyTrash":
		s.ownerOnly(s.handleOK)(w, r)
	default:
		s.handleUnknown(w, r)
	}
}

// handleSectionSubKey dispatches /library/sections/{id}/{sub}/{key} when a
// client traverses a secondary facet directory by relative key.
func (s *Server) handleSectionSubKey(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	sub := chi.URLParam(r, "sub")
	val, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		val = chi.URLParam(r, "key")
	}
	switch sub {
	case "genre", "year", "decade", "contentRating", "country", "studio", "firstCharacter":
		r2 := r.Clone(r.Context())
		q := r2.URL.Query()
		q.Set(sub, val)
		r2.URL.RawQuery = q.Encode()
		s.sectionList(w, r2, sec, "all")
	case "query":
		if val != "parse" {
			s.handleUnknown(w, r)
			return
		}
		s.handleSectionQueryParse(w, r, sec)
	default:
		s.handleUnknown(w, r)
	}
}

func (s *Server) sectionList(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	if sub == "all" && r.URL.Query().Has("guid") {
		s.libraryByGUID(w, r, &sec)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if sub == "unwatched" && q.Has("type") {
		if _, err := strconv.ParseInt(q.Get("type"), 10, 32); err != nil {
			writeHTTPError(w, 500)
			return
		}
	}
	t := typeFromParam(q.Get("type"))
	if t == "" {
		if sub == "allLeaves" && sec.Type == frontend.TypeShow {
			t = frontend.TypeEpisode
		} else {
			t = sec.Type
		}
	}
	if sub == "unwatched" && q.Has("type") && typeFromParam(q.Get("type")) != sec.Type {
		mc := s.sectionContainer(sec)
		mc.Title2 = "Unplayed"
		mc.ViewGroup = string(frontend.TypeMovie)
		if sec.Type == frontend.TypeShow {
			mc.ViewGroup = string(frontend.TypeEpisode)
		}
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	_, allTitle := sectionTypeTitle(sec)
	mc := s.sectionContainer(sec)
	mc.Title2 = allTitle
	mc.ViewGroup = string(t)
	if t == "collection" {
		s.sectionCollections(w, r, sec, "collections")
		return
	}
	switch {
	case t == sec.Type:
	case sec.Type == frontend.TypeShow && t == frontend.TypeSeason:
		mc.Title2 = "All Seasons"
	case sec.Type == frontend.TypeShow && t == frontend.TypeEpisode:
		mc.Title2 = "All Episodes"
	default:
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	br := s.parseBrowse(ctx, sec, t, pr.User, q, r.URL.RawQuery)
	switch sub {
	case "unwatched":
		br.q.Sort, br.q.Desc = frontend.SortTitle, false
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		want := true
		br.unwatched = &want
		mc.Title2 = "Unplayed"
	case "newest":
		br.q.Sort, br.q.Desc = frontend.SortReleased, true
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		mc.Title2 = "Recently Released"
	case "recentlyViewedShows":
		br.sortLastViewed = true
		br.lastViewedDesc = true
		br.lastViewedAfter = 1
	case "recentlyAdded":
		br.q.Sort, br.q.Desc = frontend.SortAdded, true
		br.q.SecondarySorts = nil
		br.sortLastViewed = false
		br.q.Filter.KnownAddedDate = true
		mc.Title2 = "Recently Added"
	case "search", "autocomplete":
		query := q.Get("query")
		if query == "" {
			query = q.Get("title")
		}
		if query == "" {
			query = q.Get("title.query")
		}
		s.sectionSearch(w, r, sec, t, query, mc)
		return
	}
	items, total, offset, err := s.browse(ctx, pr, br)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	storedOnly := t == frontend.TypeSeason || t == frontend.TypeEpisode
	sqlAvailability, _ := s.d.Catalog.(interface{ BrowseAvailabilityInSQL() bool })
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{
		media:             true,
		inSection:         true,
		guids:             q.Get("includeGuids") == "1",
		storedOnly:        storedOnly,
		availabilityInSQL: sqlAvailability != nil && sqlAvailability.BrowseAvailabilityInSQL(),
	})
	if sub == "unwatched" {
		for i := range mc.Metadata {
			if mc.Metadata[i].Type == "show" || mc.Metadata[i].Type == "season" {
				mc.Metadata[i].Key += "?unwatched=1"
			}
		}
	}
	if len(mc.Metadata) < len(items) && offset+len(items) >= total {
		total = max(offset+len(mc.Metadata), 0)
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	if q.Get("includeMeta") == "1" {
		mc.Meta = s.meta(sec, t, q.Get("sort"))
	}
	s.write(w, r, mc)
}

func (s *Server) sectionSearch(w http.ResponseWriter, r *http.Request, sec Section, t frontend.MediaType, query string, mc *model.MediaContainer) {
	ctx := r.Context()
	pr := FromContext(ctx)
	var items []frontend.Title
	if strings.TrimSpace(query) != "" {
		limit := 50
		if n, _ := strconv.Atoi(r.URL.Query().Get("limit")); n > 0 {
			limit = min(n, 200)
		}
		search := s.searchExperience()
		res, err := search.Keyword(ctx, pr.User, query, []frontend.MediaType{t}, limit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items = res
	}
	mc.Title2 = "Search for '" + query + "'"
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// filterAllowed drops titles in sections the user cannot see.
func (s *Server) filterAllowed(u *frontend.User, ts []frontend.Title) []frontend.Title {
	return s.visibleTitles(u, ts)
}

func (s *Server) sectionFacet(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	ctx := r.Context()
	t := typeFromParam(r.URL.Query().Get("type"))
	if t == "" || t == "collection" {
		t = sec.Type
	}
	facet := sub
	if sub == "categories" {
		facet = "genre"
	}
	mc := libraryContainer()
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	mc.Content = "secondary"
	mc.ViewGroup = "secondary"
	titles := map[string]string{"genre": "By Genre", "year": "By Year", "decade": "By Decade", "contentRating": "By Content Rating", "country": "By Country", "studio": "By Studio", "firstCharacter": "By First Letter", "categories": ""}
	mc.Title2 = titles[sub]
	var (
		vals []frontend.FacetValue
		err  error
	)
	if sub == "firstCharacter" {
		pr := FromContext(ctx)
		br := s.parseBrowse(ctx, sec, t, pr.User, r.URL.Query())
		vals, err = s.firstCharacter(ctx, br.q.Filter)
	} else {
		vals, err = s.facet(ctx, t, facet)
	}
	if err != nil {
		// Catalogues may not support every facet (e.g. firstCharacter);
		// an empty list is what PMS returns for an empty library.
		s.log.Debug("facet unavailable", "facet", facet, "err", err)
		vals = nil
	}
	id := strconv.Itoa(sec.ID)
	for _, v := range vals {
		// Titles that sort after Z stay in the library, but they are not a
		// second jump-bar "#". Plex tvOS adds every directory titled "#" into
		// the offset of A–Z, so a trailing bucket titled "#" lands that many
		// titles past the letter. PMS lists one "#" (the prefix before "a").
		if sub == "firstCharacter" && v.Key == "~" {
			continue
		}
		var d model.Directory
		switch sub {
		case "firstCharacter":
			d = model.Directory{FastKey: "/library/sections/" + id + "/all?firstCharacter=" + url.QueryEscape(v.Key), Size: model.Ptr(v.Count), Key: url.PathEscape(v.Key), Title: v.Title}
		case "categories":
			d = model.Directory{Type: "directory", Key: "/library/sections/" + id + "/all?genre=" + url.QueryEscape(v.Key), Title: v.Title}
		default:
			d = model.Directory{FastKey: "/library/sections/" + id + "/all?" + facet + "=" + url.QueryEscape(v.Key), Key: v.Key, Title: v.Title, Type: facet}
		}
		mc.Directory = append(mc.Directory, d)
	}
	if sub == "firstCharacter" {
		if _, dir := parseSortParam(r.URL.Query().Get("sort")); dir == "desc" {
			for i, j := 0, len(mc.Directory)-1; i < j; i, j = i+1, j-1 {
				mc.Directory[i], mc.Directory[j] = mc.Directory[j], mc.Directory[i]
			}
		}
	}
	if sub == "categories" {
		mc.Meta = &model.Meta{Type: []model.MetaType{{Key: "/library/sections/" + id + "/categories", Type: "directory", Title: "Categories", Active: true}}}
	}
	if sub == "year" || sub == "decade" {
		sort.SliceStable(mc.Directory, func(i, j int) bool { return mc.Directory[i].Title > mc.Directory[j].Title })
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// On deck, recently added, recently viewed

// inProgressTitles loads the user's in-progress items and next episodes.
func (s *Server) inProgressTitles(ctx context.Context, pr *Request, t frontend.MediaType, limit int) ([]frontend.Title, map[frontend.RatingKey]frontend.WatchStatus) {
	return s.continueExperience(pr).InProgress(ctx, pr.User, t, limit)
}

// getTitles fetches titles concurrently, preserving order and skipping
// failures.
func (s *Server) getTitles(ctx context.Context, keys []frontend.RatingKey) []frontend.Title {
	if ctx != nil && ctx.Value(storedPolicyKey{}) != nil {
		if cached, ok := s.d.Catalog.(interface {
			CachedGetMany(context.Context, []frontend.RatingKey) (map[frontend.RatingKey]frontend.Title, error)
		}); ok {
			got, err := cached.CachedGetMany(ctx, keys)
			if err != nil || len(got) == 0 {
				return nil
			}
			out := make([]frontend.Title, 0, len(got))
			seen := map[frontend.RatingKey]bool{}
			for _, k := range keys {
				if seen[k] {
					continue
				}
				seen[k] = true
				if t, ok := got[k]; ok {
					out = append(out, t)
				}
			}
			return out
		}
	}
	if pre, ok := s.d.Catalog.(interface {
		PreloadStored(context.Context, []frontend.RatingKey)
	}); ok && len(keys) > 4 {
		// Read the stored titles in batches first, cached exactly as Get
		// reads them (detail time included): each Get below then finds its
		// title in the catalogue's title cache instead of reading it and its
		// facts alone (a 3000-item playlist read 2,810 titles one by one).
		pre.PreloadStored(ctx, keys)
	}
	res := make([]*frontend.Title, len(keys))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if t, err := s.d.Catalog.Get(ctx, k); err == nil && t != nil {
				res[i] = t
			}
		}()
	}
	wg.Wait()
	out := make([]frontend.Title, 0, len(keys))
	for _, t := range res {
		if t != nil {
			out = append(out, *t)
		}
	}
	return out
}

func (s *Server) sectionOnDeck(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	_, size := pr.pageOf(0)
	items, state := s.inProgressTitles(ctx, pr, sec.Type, s.saferFetchCount(pr.User, min(size, 50)))
	sort.SliceStable(items, func(i, j int) bool {
		a, b := state[items[i].Key].UpdatedAt, state[items[j].Key].UpdatedAt
		if a.Equal(b) {
			return items[i].Key < items[j].Key
		}
		return a.After(b)
	})
	mc := s.sectionContainer(sec)
	mc.Title2 = "On Deck"
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.onDeckItems(ctx, pr, items, state)
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleOnDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	pendingVisible := false
	if pr.Start == 0 && pr.User != nil {
		if pending, ok := s.d.State.(interface{ HasPendingWatch(int64) bool }); ok {
			pendingVisible = pending.HasPendingWatch(pr.User.ID)
		}
	}
	customOrder := false
	if s.d.EpisodeOrder != nil {
		custom, err := s.d.EpisodeOrder.Customized(ctx, pr.User)
		customOrder = custom || err != nil
	}
	if paged, ok := s.d.Catalog.(interface {
		OnDeckPageSince(context.Context, int64, int, int, frontend.Filter, bool, time.Time) (frontend.WatchPage, error)
	}); ok && pr.User != nil && !pendingVisible && !customOrder {
		_, size := pr.pageOf(0)
		filter := frontend.Filter{MaxContentRating: pr.User.MaxContentRating, SaferPlexRating: s.plexMetaOn()}
		movieAllowed, episodeAllowed := false, false
		for _, section := range s.userSections(pr.User) {
			movieAllowed = movieAllowed || section.Type == frontend.TypeMovie
			episodeAllowed = episodeAllowed || section.Type == frontend.TypeShow
		}
		if !movieAllowed && !episodeAllowed {
			mc := libraryContainer()
			mc.SetPage(pr.Start, 0)
			s.write(w, r, mc)
			return
		}
		if !movieAllowed {
			filter.Type = frontend.TypeEpisode
		}
		if !episodeAllowed {
			filter.Type = frontend.TypeMovie
		}
		if blocks, ok := s.d.AccessPolicy.(interface {
			BrowseBlocks(*frontend.User) ([]string, []string)
		}); ok {
			filter.ExcludeGenres, filter.ExcludeKeywords = blocks.BrowseBlocks(pr.User)
		}
		child := filter
		filter.ChildVisibility = &child
		// The deck is bounded by PMS's OnDeckLimit; a page past it is
		// empty and still carries the bounded total.
		start := max(pr.Start, 0)
		size = max(min(size, plexOnDeckLimit-start), 0)
		page, err := paged.OnDeckPageSince(ctx, pr.User.ID, start, size, filter, r.URL.Path == "/hubs/home/onDeck", s.onDeckSince())
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		page.Total = min(page.Total, plexOnDeckLimit)
		state := s.stateFor(ctx, pr, page.Items)
		for key, anchor := range page.Anchors {
			status := state[key]
			if status.LastViewedAt.Before(anchor.LastViewedAt) {
				status.LastViewedAt = anchor.LastViewedAt
			}
			if status.UpdatedAt.Before(anchor.UpdatedAt) {
				status.UpdatedAt = anchor.UpdatedAt
			}
			state[key] = status
		}
		mc := libraryContainer()
		mc.Mixed = model.Ptr(true)
		mc.Metadata = s.onDeckItems(ctx, pr, page.Items, state)
		if r.URL.Path == "/hubs/home/onDeck" {
			for i := range mc.Metadata {
				if sec, ok := s.sectionFor(page.Items[i].Type); ok {
					mc.Metadata[i].LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
					mc.Metadata[i].LibrarySectionUUID = ""
				}
			}
		}
		if r.URL.Query().Get("includeMeta") == "1" {
			mc.Meta = &model.Meta{}
			for _, section := range s.userSections(pr.User) {
				mc.Meta.Type = append(mc.Meta.Type, s.metaType(section, section.Type, ""))
			}
		}
		mc.Size = len(mc.Metadata)
		mc.SetPage(pr.Start, page.Total)
		s.write(w, r, mc)
		return
	}
	items, state := s.continueExperience(pr).Row(ctx, pr.User, "", 200)
	if r.URL.Path == "/hubs/home/onDeck" {
		episodes := items[:0]
		for _, item := range items {
			if item.Type == frontend.TypeEpisode {
				episodes = append(episodes, item)
			}
		}
		items = episodes
	}
	// Page before rendering: the row holds up to 200 titles and a full
	// On Deck render costs statements per title. The filter is the one the
	// render applies, so the total and the page are what rendering the
	// whole row and slicing it produced.
	items = s.onDeckWindowed(items, state)
	items = s.takeVisible(pr.User, items, len(items))
	items = s.openableTitles(withStoredPolicy(ctx), pr.User, items, shelfContainsAll(ctx, items))
	items = items[:min(len(items), plexOnDeckLimit)]
	total := len(items)
	start := min(max(pr.Start, 0), total)
	_, size := pr.pageOf(0)
	end := start + min(max(size, 0), total-start)
	items = items[start:end]
	mc := libraryContainer()
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.onDeckItems(ctx, pr, items, state)
	if r.URL.Path == "/hubs/home/onDeck" {
		for i := range mc.Metadata {
			if sec, ok := s.sectionFor(items[i].Type); ok {
				mc.Metadata[i].LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
				mc.Metadata[i].LibrarySectionUUID = ""
			}
		}
	}
	if r.URL.Query().Get("includeMeta") == "1" {
		mc.Meta = &model.Meta{}
		for _, section := range s.userSections(pr.User) {
			mc.Meta.Type = append(mc.Meta.Type, s.metaType(section, section.Type, ""))
		}
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	s.write(w, r, mc)
}

// onDeckItems renders On Deck the way PMS does: movies as list items,
// episodes in full (tag ids, Guid, Rating, streams), every item led by its
// sync flag and section UUID.
func (s *Server) onDeckItems(ctx context.Context, pr *Request, items []frontend.Title, state map[frontend.RatingKey]frontend.WatchStatus) []model.Metadata {
	// Filter first: renderTitles drops hidden titles, and the loop below
	// pairs its output with items by index.
	items = s.takeVisible(pr.User, items, len(items))
	md := s.renderTitles(ctx, pr, items, renderOpts{detail: true, media: true, detailEpisodes: true, state: state})
	for i := range md {
		md[i].Guid = nil
		md[i].GuidLead = nil
		md[i].Rating_ = nil
		md[i].Producer = nil
		md[i].SetOnDeckOrder()
		md[i].LibrarySectionKey = ""
		md[i].AllowSync = model.Ptr(s.downloadsAllowed(pr.User) && isPlayable(items[i].Type))
		if sec, ok := s.sectionFor(items[i].Type); ok {
			md[i].LibrarySectionUUID = sec.UUID
		}
	}
	return md
}

func (s *Server) sectionRecentlyViewed(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	mc.Title2 = "Recently Viewed"
	if s.d.State != nil && pr.User != nil {
		recent := s.d.Experience.NewRecent(frontend.RecentOptions{Visibility: s.visibility(), Unrestricted: func(u *frontend.User) bool { return !s.viewerRestricted(u) }})
		history := s.d.State.History
		if settings, ok := s.d.State.(interface {
			RecentlyViewedSettings(context.Context, int64, time.Time, int) ([]frontend.HistoryEntry, error)
		}); ok {
			history = settings.RecentlyViewedSettings
		}
		items, err := recent.Viewed(ctx, pr.User, sec.ID, history, s.getTitles, func(t frontend.MediaType) (int, bool) {
			section, ok := s.sectionFor(t)
			return section.ID, ok
		})
		if err == nil {
			mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleRecentlyAdded(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	_, size := pr.pageOf(0)
	size = min(size, 200)
	var items []frontend.Title
	rq := r.URL.Query()
	unwatched := false
	if rq.Has("unwatched") {
		raw := strings.TrimPrefix(rq.Get("unwatched"), "+")
		if raw == "" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		for _, ch := range raw {
			if ch != '0' && ch != '1' {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
		if strings.TrimLeft(raw, "0") != "" && strings.TrimLeft(raw, "0") != "1" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		unwatched = strings.TrimLeft(raw, "0") == "1"
	}
	// Plex leaves an unknown or combined type unfiltered, and it does not
	// apply contentDirectoryID or pinnedContentDirectoryID on this route.
	// sectionID still selects one library. KnownAddedDate drops titles whose
	// release or air date is missing or still in the future.
	wantType := typeFromParam(rq.Get("type"))
	wantSection, _ := strconv.Atoi(rq.Get("sectionID"))
	if paged, ok := s.d.Catalog.(interface {
		RecentPage(context.Context, frontend.BrowseQuery, []frontend.MediaType) (frontend.Page, error)
	}); ok && r.URL.Path == "/library/recentlyAdded" {
		var kinds []frontend.MediaType
		for _, sec := range s.userSections(pr.User) {
			if wantSection > 0 && sec.ID != wantSection || wantType != "" && wantType != sec.Type {
				continue
			}
			kind := sec.Type
			if kind == frontend.TypeShow {
				kind = frontend.TypeEpisode
			}
			kinds = append(kinds, kind)
		}
		_, pageSize := pr.pageOf(0)
		q := frontend.BrowseQuery{Filter: frontend.Filter{KnownAddedDate: true, SaferPlexRating: s.plexMetaOn()}, Sort: frontend.SortAdded, Desc: true, Offset: max(pr.Start, 0), Limit: pageSize}
		if pr.User != nil {
			q.Filter.MaxContentRating = pr.User.MaxContentRating
			if unwatched {
				q.Filter.Group = &frontend.FilterGroup{Unwatched: &unwatched, UserID: pr.User.ID}
			}
		}
		if blocks, ok := s.d.AccessPolicy.(interface {
			BrowseBlocks(*frontend.User) ([]string, []string)
		}); ok {
			q.Filter.ExcludeGenres, q.Filter.ExcludeKeywords = blocks.BrowseBlocks(pr.User)
		}
		child := q.Filter
		q.Filter.ChildVisibility = &child
		page, err := paged.RecentPage(ctx, q, kinds)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		mc := libraryContainer()
		mc.Title1 = "Recently Added"
		mc.Mixed = model.Ptr(true)
		mc.Metadata = s.renderTitles(ctx, pr, page.Items, renderOpts{media: true})
		mc.Size = len(mc.Metadata)
		mc.SetPage(pr.Start, page.Total)
		s.write(w, r, mc)
		return
	}
	for _, sec := range s.userSections(pr.User) {
		if wantSection > 0 && sec.ID != wantSection || wantType != "" && wantType != sec.Type {
			continue
		}
		q := frontend.BrowseQuery{Filter: frontend.Filter{Type: sec.Type, KnownAddedDate: true, SaferPlexRating: s.plexMetaOn()}, Sort: frontend.SortAdded, Desc: true, Limit: 200}
		if r.URL.Path == "/library/recentlyAdded" && sec.Type == frontend.TypeShow {
			q.Filter.Type = frontend.TypeEpisode
		}
		if unwatched && pr.User != nil {
			q.Filter.Group = &frontend.FilterGroup{Unwatched: &unwatched, UserID: pr.User.ID}
		}
		if pr.User != nil {
			q.Filter.MaxContentRating = pr.User.MaxContentRating
		}
		page, err := s.catalogBrowse(ctx, q)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items = append(items, page.Items...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].DateAdded(s.now()).After(items[j].DateAdded(s.now()))
	})
	items = s.openableTitles(withStoredPolicy(ctx), pr.User, items, shelfContainsAll(ctx, items))
	total := min(len(items), 200)
	start := min(max(pr.Start, 0), total)
	end := start + min(size, total-start)
	items = items[start:end]
	mc := libraryContainer()
	mc.Title1 = "Recently Added"
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true, availabilityInSQL: true})
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	s.write(w, r, mc)
}

// handleLibraryAll is /library/all?type=N across sections.
func (s *Server) handleLibraryAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("guid") {
		s.libraryByGUID(w, r, nil)
		return
	}
	t := typeFromParam(q.Get("type"))
	sec, ok := s.sectionFor(t)
	if !ok || t == "" || !s.canSeeSection(FromContext(r.Context()).User, sec.ID) {
		s.write(w, r, libraryContainer())
		return
	}
	s.sectionList(w, r, sec, "all")
}
