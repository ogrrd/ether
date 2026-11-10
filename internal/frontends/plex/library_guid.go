package plex

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// GUID lookups are exact identity requests, never a fall-through to browsing.
// The catalogue remains responsible for release eligibility; the same viewer
// policy used for metadata applies before anything is rendered or counted.
func (s *Server) libraryByGUID(w http.ResponseWriter, r *http.Request, sec *Section) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	pr := FromContext(ctx)
	q := r.URL.Query()
	mc := libraryContainer()
	if sec != nil {
		mc = s.sectionContainer(*sec)
	} else {
		mc.Mixed = model.Ptr(true)
	}
	typ := typeFromParam(q.Get("type"))
	if (q.Has("type") && !validGUIDMediaType(typ)) || (sec != nil && (!s.canSeeSection(pr.User, sec.ID) || !sectionAllowsGUIDType(sec, typ))) {
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	if sec != nil && typ == "" {
		typ = sec.Type
	}
	var guids []string
	for _, value := range q["guid"] {
		for _, guid := range strings.Split(value, ",") {
			if len(guids) == 20 {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			guids = append(guids, strings.TrimSpace(guid))
		}
	}
	var titles []frontend.Title
	seen := map[frontend.RatingKey]bool{}
	for _, guid := range guids {
		keys, err := s.guidKeys(ctx, guid, typ)
		if err != nil && !errors.Is(err, frontend.ErrNotFound) {
			s.writeErr(w, r, err)
			return
		}
		for _, key := range keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			title, err := s.d.Catalog.Get(ctx, key)
			if errors.Is(err, frontend.ErrNotFound) {
				continue
			}
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
			if title == nil || (typ != "" && title.Type != typ) || !validGUIDMediaType(title.Type) || title.IsFutureRelease(s.now()) || !s.visibleTitle(ctx, pr.User, title) {
				continue
			}
			titles = append(titles, *title)
		}
	}
	offset, size := pr.pageOf(0)
	total := len(titles)
	start := min(offset, total)
	titles = titles[start:min(start+size, total)]
	mc.Metadata = s.renderTitles(ctx, pr, titles, renderOpts{media: true, inSection: sec != nil})
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	s.write(w, r, mc)
}

func validGUIDMediaType(typ frontend.MediaType) bool {
	return typ == frontend.TypeMovie || typ == frontend.TypeShow || typ == frontend.TypeSeason || typ == frontend.TypeEpisode
}

func sectionAllowsGUIDType(sec *Section, typ frontend.MediaType) bool {
	if sec == nil || typ == "" || typ == sec.Type {
		return true
	}
	return sec.Type == frontend.TypeShow && (typ == frontend.TypeSeason || typ == frontend.TypeEpisode)
}

func (s *Server) guidKeys(ctx context.Context, guid string, typ frontend.MediaType) ([]frontend.RatingKey, error) {
	guid = strings.TrimSpace(guid)
	cleanGUID, _, _ := strings.Cut(guid, "?")
	scheme, id, ok := strings.Cut(cleanGUID, "://")
	if !ok {
		return nil, frontend.ErrNotFound
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	id = strings.Trim(strings.TrimSpace(id), "/")
	if id == "" {
		return nil, frontend.ErrNotFound
	}
	switch scheme {
	case "plex":
		if s.d.LibraryGUIDs == nil {
			return nil, frontend.ErrNotFound
		}
		if strings.HasPrefix(cleanGUID, "plex://season/") || strings.HasPrefix(cleanGUID, "plex://episode/") {
			k, err := s.d.LibraryGUIDs.ExactKey(ctx, cleanGUID)
			if err != nil {
				if errors.Is(err, frontend.ErrAmbiguousLibraryGUID) {
					return nil, frontend.ErrNotFound
				}
				return nil, err
			}
			return []frontend.RatingKey{k}, nil
		}
		k, err := s.d.LibraryGUIDs.Lookup(ctx, cleanGUID)
		if err != nil {
			return nil, err
		}
		return []frontend.RatingKey{k}, nil
	case "tmdb", "themoviedb", "com.plexapp.agents.themoviedb":
		return tmdbGUIDKeys(id, typ)
	case "imdb", "com.plexapp.agents.imdb":
		if !strings.HasPrefix(id, "tt") {
			return nil, frontend.ErrNotFound
		}
		raw := strings.TrimPrefix(id, "tt")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || strings.ContainsAny(raw, "+- /") {
			return nil, frontend.ErrNotFound
		}
		t, err := s.d.Catalog.ByExternalID(ctx, "imdb", id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, frontend.ErrNotFound
		}
		return []frontend.RatingKey{t.Key}, nil
	case "tvdb", "thetvdb", "com.plexapp.agents.thetvdb":
		return s.tvdbGUIDKeys(ctx, id, typ)
	}
	return nil, frontend.ErrNotFound
}

func parsePositiveTMDBID(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n >= 1_000_000_000_000_000 || strconv.FormatInt(n, 10) != raw {
		return 0, false
	}
	return n, true
}

func parseChildNumber(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n >= 10000 || strconv.Itoa(n) != raw {
		return 0, false
	}
	return n, true
}

func tmdbGUIDKeys(id string, typ frontend.MediaType) ([]frontend.RatingKey, error) {
	parts := strings.Split(id, "/")
	switch len(parts) {
	case 1:
		n, ok := parsePositiveTMDBID(parts[0])
		if !ok {
			return nil, frontend.ErrNotFound
		}
		var keys []frontend.RatingKey
		if typ == "" || typ == frontend.TypeMovie {
			keys = append(keys, frontend.MovieKey(n))
		}
		if typ == "" || typ == frontend.TypeShow {
			keys = append(keys, frontend.ShowKey(n))
		}
		return keys, nil
	case 2:
		showID, ok1 := parsePositiveTMDBID(parts[0])
		seasonNum, ok2 := parseChildNumber(parts[1])
		if !ok1 || !ok2 {
			return nil, frontend.ErrNotFound
		}
		switch typ {
		case frontend.TypeSeason:
			return []frontend.RatingKey{frontend.SeasonKey(showID, seasonNum)}, nil
		case "", frontend.TypeShow:
			return []frontend.RatingKey{frontend.ShowKey(showID)}, nil
		default:
			return nil, frontend.ErrNotFound
		}
	case 3:
		showID, ok1 := parsePositiveTMDBID(parts[0])
		seasonNum, ok2 := parseChildNumber(parts[1])
		epNum, ok3 := parseChildNumber(parts[2])
		if !ok1 || !ok2 || !ok3 {
			return nil, frontend.ErrNotFound
		}
		switch typ {
		case frontend.TypeEpisode:
			return []frontend.RatingKey{frontend.EpisodeKey(showID, seasonNum, epNum)}, nil
		case frontend.TypeSeason:
			return []frontend.RatingKey{frontend.SeasonKey(showID, seasonNum)}, nil
		case "", frontend.TypeShow:
			return []frontend.RatingKey{frontend.ShowKey(showID)}, nil
		default:
			return nil, frontend.ErrNotFound
		}
	default:
		return nil, frontend.ErrNotFound
	}
}

func (s *Server) tvdbGUIDKeys(ctx context.Context, id string, typ frontend.MediaType) ([]frontend.RatingKey, error) {
	parts := strings.Split(id, "/")
	if len(parts) < 1 || len(parts) > 3 {
		return nil, frontend.ErrNotFound
	}
	showRaw := parts[0]
	n, err := strconv.ParseInt(showRaw, 10, 64)
	if err != nil || n <= 0 || strings.ContainsAny(showRaw, "+- /") || strconv.FormatInt(n, 10) != showRaw {
		return nil, frontend.ErrNotFound
	}
	var seasonNum, epNum int
	if len(parts) >= 2 {
		var ok bool
		if seasonNum, ok = parseChildNumber(parts[1]); !ok {
			return nil, frontend.ErrNotFound
		}
	}
	if len(parts) == 3 {
		var ok bool
		if epNum, ok = parseChildNumber(parts[2]); !ok {
			return nil, frontend.ErrNotFound
		}
	}
	t, err := s.d.Catalog.ByExternalID(ctx, "tvdb", showRaw)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, frontend.ErrNotFound
	}
	if len(parts) == 1 {
		return []frontend.RatingKey{t.Key}, nil
	}
	if t.Key.Kind() != frontend.KindShow {
		return nil, frontend.ErrNotFound
	}
	switch typ {
	case frontend.TypeEpisode:
		if len(parts) == 3 {
			return []frontend.RatingKey{frontend.EpisodeKey(t.Key.TMDB(), seasonNum, epNum)}, nil
		}
		return nil, frontend.ErrNotFound
	case frontend.TypeSeason:
		return []frontend.RatingKey{frontend.SeasonKey(t.Key.TMDB(), seasonNum)}, nil
	case "", frontend.TypeShow:
		return []frontend.RatingKey{t.Key}, nil
	default:
		return nil, frontend.ErrNotFound
	}
}
