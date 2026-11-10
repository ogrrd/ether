package plex

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// /library/people/{id} and /library/people/{id}/media are the person page
// PMS serves behind every Cast & Crew tile: the web client builds the link
// as /library/people/${tag.tagKey} for any non-cloud server, and other
// clients use the tag's numeric id. Shapes below were recorded from a real
// PMS (GET only): one Directory row (id, filter, tag, tagType, tagKey,
// thumb) in a bare library container, and a media list of listing rows
// naming their library, in titleSort order, narrowed by the usual library
// filters. The numeric form is typed by its tag — an actor's list carries
// viewGroup "movie" and viewMode 65592 (even when its rows are shows), a
// director's, writer's or producer's carries neither — while the tagKey
// form lists the union across kinds in an untyped container. An unknown
// id is a 404 on both routes.
//
// Ether's person ids are TMDB ids shared across credit kinds, so the
// numeric form resolves to the person's primary kind (actor, then
// director, then writer, then crew-as-producer) and lists that kind's
// titles; the tmdb-person-<id> tagKey form keeps PMS's union behaviour.
// A 24-hex plex.tv person id cannot be resolved: Ether has no TMDB →
// plex.tv person mapping (internal/plexguid maps titles only, and never
// invents plex:// ids), so that form 404s like any unknown id. Discover
// person pages on plex.tv therefore still cannot match Ether titles by
// plex://person guid; person pages reached from Ether's own items work.

type personByIDCatalog interface {
	PersonByTMDB(ctx context.Context, person int64) (frontend.Person, []string, frontend.RatingKey, error)
}

type personKind struct {
	credit  string // catalog_credits.kind
	filter  string // Plex filter field name
	tagType int
}

var personKinds = []personKind{
	{"cast", "actor", 6},
	{"director", "director", 4},
	{"writer", "writer", 5},
	{"crew", "producer", 7},
}

// personMediaCap bounds the titles fetched per library type for one
// person. The query rides the credits index, so the work scales with the
// person's own credits; the cap only guards against pathological rows.
const personMediaCap = 2000

type resolvedPerson struct {
	id         int64
	tagKeyForm bool
	person     frontend.Person
	primary    personKind
	thumbKey   frontend.RatingKey
}

// parsePersonID accepts the numeric TMDB id and Ether's tmdb-person-<id>
// tagKey. Anything else (including a 24-hex plex.tv person id) is not a
// person Ether can name.
func parsePersonID(raw string) (id int64, tagKeyForm, ok bool) {
	if rest, found := strings.CutPrefix(raw, "tmdb-person-"); found {
		id, err := strconv.ParseInt(rest, 10, 64)
		return id, true, err == nil && id > 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, false, err == nil && id > 0
}

func (s *Server) resolvePerson(w http.ResponseWriter, r *http.Request) (resolvedPerson, bool) {
	var out resolvedPerson
	id, tagKeyForm, ok := parsePersonID(chi.URLParam(r, "id"))
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return out, false
	}
	cat, ok := s.catalog().(personByIDCatalog)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return out, false
	}
	person, kinds, thumbKey, err := cat.PersonByTMDB(r.Context(), id)
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			writeHTTPError(w, http.StatusNotFound)
		} else {
			s.writeErr(w, r, err)
		}
		return out, false
	}
	out = resolvedPerson{id: id, tagKeyForm: tagKeyForm, person: person, thumbKey: thumbKey}
	for _, k := range personKinds {
		if slices.Contains(kinds, k.credit) {
			out.primary = k
			return out, true
		}
	}
	writeHTTPError(w, http.StatusNotFound)
	return resolvedPerson{}, false
}

func peopleContainer() *model.MediaContainer {
	return &model.MediaContainer{
		Identifier:      model.LibraryIdentifier,
		MediaTagPrefix:  model.MediaTagPrefix,
		MediaTagVersion: model.MediaTagVersion,
	}
}

// handleLibraryPerson is GET /library/people/{id}.
func (s *Server) handleLibraryPerson(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.resolvePerson(w, r)
	if !ok {
		return
	}
	idStr := strconv.FormatInt(rp.id, 10)
	attrs := []model.Attr{
		model.A("id", rp.id),
		model.A("filter", rp.primary.filter+"="+idStr),
		model.A("tag", rp.person.Name),
		model.A("tagType", rp.primary.tagType),
		model.A("tagKey", "tmdb-person-"+idStr),
	}
	if rp.person.ThumbPath != "" && rp.thumbKey != 0 && rp.primary.credit != "crew" {
		attrs = append(attrs, model.A("thumb", "/library/metadata/"+rp.thumbKey.String()+"/person/"+rp.primary.filter+"/"+base64.RawURLEncoding.EncodeToString([]byte(rp.person.ThumbPath))))
	}
	mc := peopleContainer()
	mc.Directory = []model.Directory{{Extra: attrs}}
	mc.Size = 1
	s.write(w, r, mc)
}

// handleLibraryPersonMedia is GET /library/people/{id}/media.
func (s *Server) handleLibraryPersonMedia(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.resolvePerson(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()

	credit := frontend.CreditFilter{People: []int64{rp.id}}
	if !rp.tagKeyForm {
		credit.Kind = rp.primary.credit
	}

	// One browse per library type the viewer can see, each narrowed by
	// the request's library filters and the viewer's own policy exactly
	// as a section listing is (parseBrowse + browse), then merged in
	// PMS's titleSort order. PMS ignores sort= for the ordering here
	// (measured) but still refuses an unknown sort key, which browse
	// reproduces.
	fetchPr := *pr
	fetchPr.HasPage = true
	fetchPr.Start = 0
	fetchPr.Size = personMediaCap
	limit := 0
	var all []frontend.Title
	seenTypes := map[frontend.MediaType]bool{}
	for _, sec := range s.userSections(pr.User) {
		if seenTypes[sec.Type] {
			continue
		}
		seenTypes[sec.Type] = true
		br := s.parseBrowse(ctx, sec, sec.Type, pr.User, q)
		if limit == 0 {
			limit = br.limit
		}
		br.q.Filter.Credits = append(br.q.Filter.Credits, credit)
		items, _, _, err := s.browse(ctx, &fetchPr, br)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		all = append(all, items...)
	}

	seenKeys := map[frontend.RatingKey]bool{}
	deduped := all[:0]
	for _, t := range all {
		if seenKeys[t.Key] {
			continue
		}
		seenKeys[t.Key] = true
		deduped = append(deduped, t)
	}
	all = deduped
	// The same visibility renderTitles applies, done before paging so
	// size and totalSize count only rows this viewer may see.
	all = s.visibleTitles(pr.User, all)
	all = s.availableForViewer(ctx, pr.User, all)
	slices.SortStableFunc(all, func(a, b frontend.Title) int {
		if c := strings.Compare(titleSortKey(a), titleSortKey(b)); c != 0 {
			return c
		}
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	total := len(all)
	offset, size := pr.pageOf(0)
	lo := min(offset, total)
	hi := min(lo+size, total)
	page := all[lo:hi]

	excluded := excludedElements(q)
	mc := peopleContainer()
	if !rp.tagKeyForm && rp.primary.credit == "cast" {
		mc.ViewGroup = "movie"
		mc.ViewMode = viewModeList
	}
	mc.Metadata = s.renderTitles(ctx, pr, page, renderOpts{
		media: !excluded["Media"],
		guids: q.Get("includeGuids") == "1",
	})
	applyExcludedElements(mc.Metadata, excluded)
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(offset, total)
	}
	s.write(w, r, mc)
}

func excludedElements(q map[string][]string) map[string]bool {
	out := map[string]bool{}
	for _, v := range q["excludeElements"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// applyExcludedElements strips child elements a client asked to leave
// out, PMS-style; excluded Media is replaced by its count.
func applyExcludedElements(rows []model.Metadata, excluded map[string]bool) {
	if len(excluded) == 0 {
		return
	}
	for i := range rows {
		m := &rows[i]
		if excluded["Media"] {
			count := len(m.Media)
			if count == 0 && isPlayable(frontend.MediaType(m.Type)) {
				count = 1 // not rendered, but Ether exposes exactly one version
			}
			if count > 0 {
				m.MediaCount = model.Ptr(count)
				m.MediaCountOptimized = model.Ptr(0)
			}
			m.Media = nil
		}
		if excluded["Genre"] {
			m.Genre = nil
		}
		if excluded["Country"] {
			m.Country = nil
		}
		if excluded["Collection"] {
			m.Collection = nil
		}
		if excluded["Director"] {
			m.Director = nil
		}
		if excluded["Writer"] {
			m.Writer = nil
		}
		if excluded["Role"] {
			m.Role = nil
		}
		if excluded["Guid"] {
			m.Guid = nil
		}
		if excluded["Image"] {
			m.Image = nil
		}
	}
}
