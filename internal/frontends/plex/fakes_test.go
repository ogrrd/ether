package plex

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv/livetvtest"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var fixedNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Artwork CDN

// fakeArtwork is an offline image CDN: every image is a JPEG whose four
// quadrants have colours derived from the URL path, so UltraBlurColors are
// deterministic. Paths containing "missing" are 404.
type fakeArtwork struct {
	mu   sync.Mutex
	hits map[string]int
}

func newFakeArtwork() *fakeArtwork { return &fakeArtwork{hits: map[string]int{}} }

func (a *fakeArtwork) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits[path]
}

func (a *fakeArtwork) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.hits[r.URL.Path]++
	a.mu.Unlock()
	if strings.Contains(r.URL.Path, "missing") {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	h := fnv.New32a()
	h.Write([]byte(r.URL.Path))
	seed := h.Sum32()
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for y := 0; y < 36; y++ {
		for x := 0; x < 64; x++ {
			q := uint32(0)
			if x >= 32 {
				q++
			}
			if y >= 18 {
				q += 2
			}
			v := seed >> (q * 6)
			img.Set(x, y, color.RGBA{uint8(v * 37), uint8(v * 91), uint8(v * 53), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"image/jpeg"}},
		Body: io.NopCloser(bytes.NewReader(buf.Bytes())), ContentLength: int64(buf.Len()), Request: r}, nil
}

// ---------------------------------------------------------------------------
// Catalog

type fakeCatalog struct {
	state    *fakeState
	mu       sync.Mutex
	titles   map[core.RatingKey]core.Title
	children map[core.RatingKey][]core.RatingKey
	facets   map[string][]core.FacetValue
	lastQ    core.BrowseQuery
	failOnce map[core.RatingKey]bool // the next Get of these keys fails
}

func (c *fakeCatalog) StoredTitle(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	return c.Get(ctx, k)
}

func (c *fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failOnce[k] {
		delete(c.failOnce, k)
		return nil, errors.New("fake: transient catalogue failure")
	}
	t, ok := c.titles[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &t, nil
}

// CachedVisibilityRows serves the stored rows the mirror would, so hub
// rendering judges series from rows as it does against the catalogue.
func (c *fakeCatalog) CachedVisibilityRows(_ context.Context, keys []core.RatingKey, _ core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[core.RatingKey]core.Title{}
	for _, k := range keys {
		if t, ok := c.titles[k]; ok && t.Type != core.TypeSeason && !t.Adult && !c.failOnce[k] {
			out[k] = t
		}
	}
	return out, nil
}

func (c *fakeCatalog) list(keys []core.RatingKey) []core.Title {
	var out []core.Title
	for _, k := range keys {
		out = append(out, c.titles[k])
	}
	return out
}

func (c *fakeCatalog) Children(_ context.Context, k core.RatingKey) ([]core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.titles[k]; !ok {
		return nil, core.ErrNotFound
	}
	return c.list(c.children[k]), nil
}

func (c *fakeCatalog) Grandchildren(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.Title
	for _, s := range c.children[k] {
		out = append(out, c.list(c.children[s])...)
	}
	return out, nil
}

func compareTitlesBySpec(a, b core.Title, spec core.SortSpec, now time.Time) int {
	var c int
	switch spec.Sort {
	case core.SortTitle:
		c = cmp.Compare(titleSortKey(a), titleSortKey(b))
	case core.SortYear:
		c = cmp.Compare(a.Year, b.Year)
	case core.SortReleased:
		c = cmp.Compare(a.Released.Unix(), b.Released.Unix())
	case core.SortAdded:
		c = cmp.Compare(a.DateAdded(now).Unix(), b.DateAdded(now).Unix())
	case core.SortUpdated:
		c = cmp.Compare(a.UpdatedAt.Unix(), b.UpdatedAt.Unix())
	case core.SortRating:
		c = cmp.Compare(a.Rating, b.Rating)
	case core.SortDuration:
		c = cmp.Compare(a.Runtime, b.Runtime)
	default:
		c = cmp.Compare(a.Popularity, b.Popularity)
	}
	if spec.Desc {
		return -c
	}
	return c
}

func (c *fakeCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastQ = q
	var all []core.Title
	for _, t := range c.titles {
		if !fakeFilterMatch(t, q.Filter) || !c.matchGroup(t, q.Filter.Group) {
			continue
		}
		all = append(all, t)
	}
	slices.SortStableFunc(all, func(a, b core.Title) int {
		if q.SortLastViewed {
			var astate, bstate core.WatchStatus
			if c.state != nil {
				astate, _ = c.state.Get(context.Background(), q.SortViewerID, a.Key)
				bstate, _ = c.state.Get(context.Background(), q.SortViewerID, b.Key)
			}
			av, bv := unixOrZero(astate.LastViewedAt), unixOrZero(bstate.LastViewedAt)
			if (av > 0) != (bv > 0) {
				if av > 0 {
					return -1
				}
				return 1
			}
			if av != bv {
				if q.Desc {
					return cmp.Compare(bv, av)
				}
				return cmp.Compare(av, bv)
			}
			for _, spec := range q.SecondarySorts {
				if v := compareTitlesBySpec(a, b, spec, fixedNow); v != 0 {
					return v
				}
			}
			if q.Filter.Type != core.TypeEpisode && q.Filter.Type != core.TypeSeason {
				if v := cmp.Compare(titleSortKey(a), titleSortKey(b)); v != 0 {
					return v
				}
			}
			return cmp.Compare(a.Key, b.Key)
		}
		if q.Sort == "" {
			if q.Desc {
				return cmp.Compare(b.Key, a.Key)
			}
			if c := cmp.Compare(a.Key, b.Key); c != 0 {
				return c
			}
		} else {
			primary := core.SortSpec{Sort: q.Sort, Desc: q.Desc}
			if c := compareTitlesBySpec(a, b, primary, fixedNow); c != 0 {
				return c
			}
		}
		for _, sec := range q.SecondarySorts {
			if c := compareTitlesBySpec(a, b, sec, fixedNow); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.Key, b.Key)
	})
	total := len(all)
	lo := min(q.Offset, total)
	hi := total
	if q.Limit > 0 {
		hi = min(lo+q.Limit, total)
	}
	return core.Page{Items: all[lo:hi], Total: total}, nil
}

func (c *fakeCatalog) matchGroup(t core.Title, g *core.FilterGroup) bool {
	if g == nil {
		return true
	}
	if g.StateField != "" {
		var ws core.WatchStatus
		if c.state != nil {
			ws, _ = c.state.Get(context.Background(), g.UserID, t.Key)
		}
		n := int64(0)
		switch g.StateField {
		case "viewCount":
			if ws.Key == 0 {
				return false
			}
			n = int64(ws.ViewCount)
		case "skipCount":
			if ws.Key == 0 {
				return false
			}
		case "viewOffset":
			n = ws.ViewOffset.Milliseconds()
		case "lastViewedAt":
			n = unixOrZero(ws.LastViewedAt)
		case "lastRatedAt":
			n = unixOrZero(ws.LastRatedAt)
		case "lastSkippedAt":
			n = 0
		default:
			for _, ep := range c.titles {
				if ep.Type != core.TypeEpisode || ep.ShowKey != t.Key && ep.ShowKey != t.ShowKey || t.Type == core.TypeSeason && ep.Season != t.Season {
					continue
				}
				st, _ := c.state.Get(context.Background(), g.UserID, ep.Key)
				if g.StateField == "viewedLeafCount" && st.ViewCount > 0 || g.StateField == "viewedOrInProgressLeafCount" && (st.ViewCount > 0 || st.ViewOffset > 0) || (g.StateField == "unviewedLeafCount" || g.StateField == "unwatchedLeaves") && st.ViewCount == 0 {
					n++
				}
			}
		}
		target := g.StateValue
		op := g.StateOperator
		if g.StateField == "unwatchedLeaves" {
			target = 0
			op = "="
			if g.StateValue == 1 {
				op = ">"
			}
		}
		switch op {
		case "=", "==":
			return n == target
		case "!=", "!==":
			return n != target
		case "<", "<<":
			return n < target
		case "<=", "<<=":
			return n <= target
		case ">", ">>":
			return n > target
		case ">=", ">>=":
			return n >= target
		}
		return false
	}
	if g.Filter != nil {
		return fakeFilterMatch(t, *g.Filter) && c.matchGroup(t, g.Filter.Group)
	}
	if g.Unwatched != nil || g.InProgress != nil || g.LastViewedAfter > 0 || g.LastViewedBefore > 0 {
		var ws core.WatchStatus
		if c.state != nil {
			ws, _ = c.state.Get(context.Background(), g.UserID, t.Key)
		}
		if g.Unwatched != nil && (ws.ViewCount == 0) != *g.Unwatched {
			return false
		}
		if g.InProgress != nil && (ws.ViewOffset > 0) != *g.InProgress {
			return false
		}
		if g.LastViewedAfter > 0 || g.LastViewedBefore > 0 {
			ts := unixOrZero(ws.LastViewedAt)
			if ts <= 0 || (g.LastViewedAfter > 0 && ts < g.LastViewedAfter) || (g.LastViewedBefore > 0 && ts > g.LastViewedBefore) {
				return false
			}
		}
		return true
	}
	if g.Operator == "or" {
		for i := range g.Children {
			if c.matchGroup(t, &g.Children[i]) {
				return true
			}
		}
		return false
	}
	for i := range g.Children {
		if !c.matchGroup(t, &g.Children[i]) {
			return false
		}
	}
	return true
}

func (c *fakeCatalog) Search(_ context.Context, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.Title
	for _, t := range c.titles {
		ok := false
		for _, ty := range types {
			if t.Type == ty {
				ok = true
			}
		}
		if ok && strings.Contains(strings.ToLower(t.Title), strings.ToLower(query)) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c *fakeCatalog) SearchPeople(_ context.Context, terms []string, limit int) ([]core.Person, error) {
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	type personEntry struct {
		p       core.Person
		credits int
	}
	byID := map[int64]*personEntry{}
	add := func(p core.Person) {
		if p.TMDB <= 0 || strings.TrimSpace(p.Name) == "" {
			return
		}
		e, ok := byID[p.TMDB]
		if !ok {
			e = &personEntry{p: core.Person{TMDB: p.TMDB, Name: p.Name}}
			byID[p.TMDB] = e
		}
		e.credits++
	}
	for _, t := range c.titles {
		if t.Adult {
			continue
		}
		seen := map[int64]bool{}
		for _, p := range t.Cast {
			if !seen[p.TMDB] {
				seen[p.TMDB] = true
				add(p)
			}
		}
		for _, p := range t.Directors {
			if !seen[p.TMDB] {
				seen[p.TMDB] = true
				add(p)
			}
		}
		for _, p := range t.Writers {
			if !seen[p.TMDB] {
				seen[p.TMDB] = true
				add(p)
			}
		}
	}
	var matches []personEntry
	for _, e := range byID {
		words := strings.FieldsFunc(strings.ToLower(e.p.Name), func(r rune) bool {
			return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r >= 0x80)
		})
		ok := true
		for _, term := range terms {
			found := false
			for _, w := range words {
				if strings.HasPrefix(w, term) {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, *e)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].credits != matches[j].credits {
			return matches[i].credits > matches[j].credits
		}
		return matches[i].p.TMDB < matches[j].p.TMDB
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]core.Person, len(matches))
	for i, m := range matches {
		out[i] = m.p
	}
	return out, nil
}

func (c *fakeCatalog) PersonCreditKeys(_ context.Context, person int64, types []core.MediaType, limit int) ([]core.RatingKey, error) {
	if person <= 0 || limit <= 0 || len(types) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	allow := map[core.MediaType]bool{}
	for _, t := range types {
		allow[t] = true
	}
	var titles []core.Title
	for _, t := range c.titles {
		if t.Adult || !allow[t.Type] {
			continue
		}
		credited := false
		for _, p := range t.Cast {
			if p.TMDB == person {
				credited = true
				break
			}
		}
		if !credited {
			for _, p := range t.Directors {
				if p.TMDB == person {
					credited = true
					break
				}
			}
		}
		if !credited {
			for _, p := range t.Writers {
				if p.TMDB == person {
					credited = true
					break
				}
			}
		}
		if credited {
			titles = append(titles, t)
		}
	}
	sort.Slice(titles, func(i, j int) bool {
		if titles[i].Popularity != titles[j].Popularity {
			return titles[i].Popularity > titles[j].Popularity
		}
		return titles[i].Key < titles[j].Key
	})
	if len(titles) > limit {
		titles = titles[:limit]
	}
	out := make([]core.RatingKey, len(titles))
	for i, t := range titles {
		out[i] = t.Key
	}
	return out, nil
}

func (c *fakeCatalog) PersonByTMDB(_ context.Context, person int64) (core.Person, []string, core.RatingKey, error) {
	if person <= 0 {
		return core.Person{}, nil, 0, core.ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out core.Person
	var thumbKey core.RatingKey
	kindSet := map[string]bool{}
	thumbByKind := map[string]core.Person{}
	keyByKind := map[string]core.RatingKey{}
	scan := func(t core.Title, ps []core.Person, kind string) {
		for _, p := range ps {
			if p.TMDB != person {
				continue
			}
			kindSet[kind] = true
			if out.Name == "" {
				out = core.Person{TMDB: p.TMDB, Name: p.Name}
			}
			if p.ThumbPath != "" {
				if _, ok := thumbByKind[kind]; !ok {
					thumbByKind[kind] = p
					keyByKind[kind] = t.Key
				}
			}
		}
	}
	keys := make([]core.RatingKey, 0, len(c.titles))
	for k := range c.titles {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		t := c.titles[k]
		scan(t, t.Cast, "cast")
		scan(t, t.Directors, "director")
		scan(t, t.Writers, "writer")
		scan(t, t.Crew, "crew")
	}
	if len(kindSet) == 0 {
		return core.Person{}, nil, 0, core.ErrNotFound
	}
	kinds := make([]string, 0, len(kindSet))
	for _, k := range []string{"cast", "director", "writer", "crew"} {
		if kindSet[k] {
			kinds = append(kinds, k)
		}
	}
	if kinds[0] != "crew" {
		if p, ok := thumbByKind[kinds[0]]; ok {
			out.ThumbPath = p.ThumbPath
			thumbKey = keyByKind[kinds[0]]
		}
	}
	return out, kinds, thumbKey, nil
}

func (c *fakeCatalog) List(context.Context, string, core.MediaType, int) ([]core.Title, error) {
	return nil, nil
}

func (c *fakeCatalog) Similar(_ context.Context, k core.RatingKey, limit int) ([]core.Title, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.Title
	for _, t := range c.titles {
		if t.Type == core.TypeMovie && t.Key != k {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (c *fakeCatalog) Collection(context.Context, int64) (string, []core.Title, error) {
	return "", nil, core.ErrNotFound
}

func (c *fakeCatalog) Facet(_ context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	v, ok := c.facets[facet]
	if !ok {
		return nil, fmt.Errorf("facet %s unsupported", facet)
	}
	return v, nil
}

func (c *fakeCatalog) NextEpisode(_ context.Context, ep core.RatingKey) (*core.Title, error) {
	n := core.EpisodeKey(ep.TMDB(), ep.Season(), ep.Episode()+1)
	return c.Get(context.Background(), n)
}

func (c *fakeCatalog) ByExternalID(context.Context, string, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}

// ---------------------------------------------------------------------------
// State

type progressCall struct {
	user     int64
	key      core.RatingKey
	offset   time.Duration
	duration time.Duration
	state    string
}

type fakeState struct {
	mu       sync.Mutex
	st       map[int64]map[core.RatingKey]core.WatchStatus
	progress []progressCall
	scrobbed []core.RatingKey
	rated    map[core.RatingKey]float64
}

func newFakeState() *fakeState {
	return &fakeState{st: map[int64]map[core.RatingKey]core.WatchStatus{}, rated: map[core.RatingKey]float64{}}
}

func (s *fakeState) set(user int64, ws core.WatchStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st[user] == nil {
		s.st[user] = map[core.RatingKey]core.WatchStatus{}
	}
	s.st[user][ws.Key] = ws
}

func (s *fakeState) Get(_ context.Context, u int64, k core.RatingKey) (core.WatchStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st[u][k], nil
}

func (s *fakeState) GetMany(_ context.Context, u int64, ks []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[core.RatingKey]core.WatchStatus{}
	for _, k := range ks {
		if ws, ok := s.st[u][k]; ok {
			out[k] = ws
		}
	}
	return out, nil
}

func (s *fakeState) Progress(_ context.Context, u int64, k core.RatingKey, off, dur time.Duration, state string) error {
	s.mu.Lock()
	s.progress = append(s.progress, progressCall{u, k, off, dur, state})
	s.mu.Unlock()
	return nil
}

func (s *fakeState) Scrobble(_ context.Context, u int64, k core.RatingKey) error {
	s.mu.Lock()
	s.scrobbed = append(s.scrobbed, k)
	s.mu.Unlock()
	s.set(u, core.WatchStatus{Key: k, ViewCount: 1, LastViewedAt: fixedNow})
	return nil
}

func (s *fakeState) Unscrobble(_ context.Context, u int64, k core.RatingKey) error {
	s.set(u, core.WatchStatus{Key: k})
	return nil
}

func (s *fakeState) Rate(_ context.Context, u int64, k core.RatingKey, r float64) error {
	s.mu.Lock()
	s.rated[k] = r
	s.mu.Unlock()
	return nil
}

func (s *fakeState) InProgress(_ context.Context, u int64, limit int) ([]core.WatchStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.WatchStatus
	for _, ws := range s.st[u] {
		if ws.ViewOffset > 0 {
			out = append(out, ws)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *fakeState) History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) {
	return nil, nil
}
func (s *fakeState) InProgressAny(context.Context) ([]core.RatingKey, error) { return nil, nil }

func (s *fakeState) WatchedEpisodes(_ context.Context, u int64) ([]core.RatingKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.RatingKey
	for k, ws := range s.st[u] {
		if k.Kind() == core.KindEpisode && ws.ViewCount > 0 {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ---------------------------------------------------------------------------
// Auth

type fakeAuth struct {
	users map[string]core.User
}

func (a *fakeAuth) Authenticate(_ context.Context, token string) (*core.User, error) {
	u, ok := a.users[token]
	if !ok {
		return nil, core.ErrUnauthorized
	}
	return &u, nil
}

func (a *fakeAuth) Users(context.Context) ([]core.User, error) {
	var out []core.User
	for _, u := range a.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (a *fakeAuth) UpdateUser(context.Context, core.User) error { return nil }
func (a *fakeAuth) Owner(context.Context) (*core.User, error) {
	for _, u := range a.users {
		if u.IsOwner {
			return &u, nil
		}
	}
	return nil, core.ErrNotFound
}

// ---------------------------------------------------------------------------
// Playback

type fakePlayback struct {
	mu       sync.Mutex
	info     core.MediaInfo
	data     []byte
	failures map[core.RatingKey]error
	sessions map[string]*core.PlaySession
	stopped  []string
	touched  []string
	prepared int
	// resolution, when set, makes prepared sessions on-demand sources.
	resolution *core.Resolution
}

func newFakePlayback(info core.MediaInfo) *fakePlayback {
	return &fakePlayback{info: info, data: bytes.Repeat([]byte("0123456789"), 1000), failures: map[core.RatingKey]error{}, sessions: map[string]*core.PlaySession{}}
}

func (p *fakePlayback) Estimate(_ context.Context, t *core.Title, _ core.ClientCaps) core.MediaInfo {
	info := p.info
	info.Estimated = true
	return info
}

func (p *fakePlayback) Prepare(_ context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failures[k]; err != nil {
		return nil, err
	}
	p.prepared++
	id := fmt.Sprintf("ps-%d-%s-%d", u.ID, caps.ClientID, k)
	if ps, ok := p.sessions[id]; ok {
		return ps, nil
	}
	ps := &core.PlaySession{ID: id, UserID: u.ID, ClientID: caps.ClientID, Key: k, Info: p.info, StartedAt: fixedNow, Resolution: p.resolution}
	p.sessions[id] = ps
	return ps, nil
}

func (p *fakePlayback) Session(id string) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, ok := p.sessions[id]
	return ps, ok
}

func (p *fakePlayback) SessionFor(u int64, client string, k core.RatingKey) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ps := range p.sessions {
		if ps.UserID == u && ps.ClientID == client && ps.Key == k {
			return ps, true
		}
	}
	return nil, false
}

func (p *fakePlayback) ServeRange(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := p.Session(id); !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	count := &fakeRangeCounter{ResponseWriter: w}
	http.ServeContent(count, r, "file.mkv", fixedNow, bytes.NewReader(p.data))
	p.mu.Lock()
	if ps := p.sessions[id]; ps != nil {
		ps.BytesServed += count.bytes
	}
	p.mu.Unlock()
}

type fakeRangeCounter struct {
	http.ResponseWriter
	bytes  int64
	status int
}

func (w *fakeRangeCounter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *fakeRangeCounter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if w.status == http.StatusOK || w.status == http.StatusPartialContent {
		w.bytes += int64(n)
	}
	return n, err
}

func (p *fakePlayback) InputURL(id string) string { return "http://127.0.0.1:1/source/" + id }

func (p *fakePlayback) SessionContext(string) context.Context { return context.Background() }

func (p *fakePlayback) Failover(context.Context, string, time.Duration) (*core.PlaySession, error) {
	return nil, core.ErrNotAvailable
}

func (p *fakePlayback) Stop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = append(p.stopped, id)
	delete(p.sessions, id)
}

func (p *fakePlayback) Sessions() []*core.PlaySession {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*core.PlaySession
	for _, ps := range p.sessions {
		out = append(out, ps)
	}
	return out
}

func (p *fakePlayback) Touch(id string) {
	p.mu.Lock()
	p.touched = append(p.touched, id)
	p.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Transcoder

type fakeHLS struct {
	playbackState string
	id            string
	req           core.HLSRequest
	rungs         []core.HLSRung
	rungReqs      []string
	rungPlaylist  string
	encoder       *core.EncoderStatus
}

func (h *fakeHLS) EncoderStatus() *core.EncoderStatus { return h.encoder }

func (h *fakeHLS) PlaybackState(state string) { h.playbackState = state }

func (h *fakeHLS) ID() string { return h.id }
func (h *fakeHLS) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\n00000.ts\n")
}
func (h *fakeHLS) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	w.Header().Set("Content-Type", "video/mp2t")
	fmt.Fprintf(w, "segment-%d", n)
}
func (h *fakeHLS) Rungs() []core.HLSRung { return h.rungs }
func (h *fakeHLS) ServeRungPlaylist(w http.ResponseWriter, r *http.Request, rung string) {
	h.rungReqs = append(h.rungReqs, rung+"/index")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if h.rungPlaylist != "" {
		fmt.Fprint(w, h.rungPlaylist)
		return
	}
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\n00000.ts\n# rung %s\n", rung)
}
func (h *fakeHLS) ServeRungSegment(w http.ResponseWriter, r *http.Request, rung string, n int) {
	h.rungReqs = append(h.rungReqs, fmt.Sprintf("%s/%d", rung, n))
	w.Header().Set("Content-Type", "video/mp2t")
	fmt.Fprintf(w, "segment-%s-%d", rung, n)
}
func (h *fakeHLS) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error { return nil }
func (h *fakeHLS) LastAccess() time.Time                                                  { return fixedNow }
func (h *fakeHLS) Close() error                                                           { return nil }
func (h *fakeHLS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Connection", "close")
	if r.Method == http.MethodHead {
		return
	}
	fmt.Fprintf(w, "mkv-%s-%s", h.id, h.req.StartOffset)
}

var _ core.HTTPSession = (*fakeHLS)(nil)

type fakeTranscoder struct {
	mu       sync.Mutex
	decision core.Decision
	sessions map[string]*fakeHLS
	stopped  []string
	lastOpts core.DecideOptions
	lastCaps core.ClientCaps
	rungs    []core.HLSRung
}

func newFakeTranscoder(d core.Decision) *fakeTranscoder {
	return &fakeTranscoder{decision: d, sessions: map[string]*fakeHLS{}}
}

func (t *fakeTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return nil, fmt.Errorf("no probe")
}
func (t *fakeTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastOpts, t.lastCaps = opts, caps
	d := t.decision
	if !opts.DirectPlay && d.Kind == core.DecisionDirectPlay {
		d = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts", Reason: "direct play disabled"}
	}
	d.AudioStream, d.SubtitleStream = opts.AudioStream, opts.SubtitleStream
	return d
}
func (t *fakeTranscoder) StartHLS(_ context.Context, req core.HLSRequest) (core.HLSSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := &fakeHLS{id: req.SessionID, req: req, rungs: t.rungs}
	t.sessions[req.SessionID] = h
	return h, nil
}
func (t *fakeTranscoder) Session(id string) (core.HLSSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.sessions[id]
	if !ok {
		return nil, false
	}
	return h, true
}
func (t *fakeTranscoder) Stop(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = append(t.stopped, id)
	delete(t.sessions, id)
}
func (t *fakeTranscoder) HWAccel() core.HWAccel { return core.HWAccel{Name: "none"} }

// ---------------------------------------------------------------------------
// Collections, recommender, playlists

type fakeCollections struct {
	cols  map[core.RatingKey]core.Collection
	items map[core.RatingKey][]core.Title
}

func (c *fakeCollections) List(_ context.Context, _ *core.User, t core.MediaType) ([]core.Collection, error) {
	var out []core.Collection
	for _, col := range c.cols {
		if col.Type == t {
			out = append(out, col)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (c *fakeCollections) Get(_ context.Context, _ *core.User, k core.RatingKey) (*core.Collection, error) {
	col, ok := c.cols[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &col, nil
}
func (c *fakeCollections) Items(_ context.Context, _ *core.User, k core.RatingKey, off, lim int) (core.Page, error) {
	items := c.items[k]
	lo := min(off, len(items))
	hi := min(lo+lim, len(items))
	return core.Page{Items: items[lo:hi], Total: len(items)}, nil
}
func (c *fakeCollections) CreateSmart(context.Context, core.Collection) (*core.Collection, error) {
	return nil, fmt.Errorf("no")
}
func (c *fakeCollections) DeleteSmart(context.Context, core.RatingKey) error { return nil }

type fakeRecommender struct {
	home    []core.Hub
	section map[core.MediaType][]core.Hub
	related []core.Hub
}

func (r *fakeRecommender) HomeHubs(context.Context, *core.User, int) ([]core.Hub, error) {
	return r.home, nil
}
func (r *fakeRecommender) SectionHubs(_ context.Context, _ *core.User, t core.MediaType, _ int) ([]core.Hub, error) {
	return r.section[t], nil
}
func (r *fakeRecommender) Related(context.Context, *core.User, core.RatingKey, int) ([]core.Hub, error) {
	return r.related, nil
}

type fakePlaylists struct {
	mu   sync.Mutex
	next int64
	pls  map[core.RatingKey]*core.Playlist
}

func newFakePlaylists() *fakePlaylists {
	return &fakePlaylists{next: 1, pls: map[core.RatingKey]*core.Playlist{}}
}

func (p *fakePlaylists) List(_ context.Context, u int64) ([]core.Playlist, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []core.Playlist
	for _, pl := range p.pls {
		if pl.UserID == u {
			out = append(out, *pl)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (p *fakePlaylists) Get(_ context.Context, u int64, k core.RatingKey) (*core.Playlist, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.pls[k]
	if !ok || pl.UserID != u {
		return nil, core.ErrNotFound
	}
	cp := *pl
	cp.Items = append([]core.RatingKey(nil), pl.Items...)
	return &cp, nil
}
func (p *fakePlaylists) Create(_ context.Context, u int64, title string, items []core.RatingKey) (*core.Playlist, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl := &core.Playlist{Key: core.PlaylistKey(p.next), UserID: u, Title: title, Items: items, UpdatedAt: fixedNow}
	p.next++
	p.pls[pl.Key] = pl
	cp := *pl
	return &cp, nil
}
func (p *fakePlaylists) Add(_ context.Context, u int64, k core.RatingKey, items []core.RatingKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pls[k].Items = append(p.pls[k].Items, items...)
	return nil
}
func (p *fakePlaylists) Remove(_ context.Context, u int64, k core.RatingKey, idx int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.pls[k].Items
	p.pls[k].Items = append(it[:idx:idx], it[idx+1:]...)
	return nil
}
func (p *fakePlaylists) Move(_ context.Context, u int64, k core.RatingKey, from, to int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.pls[k].Items
	x := it[from]
	rest := append(append([]core.RatingKey(nil), it[:from]...), it[from+1:]...)
	rest = append(rest[:to], append([]core.RatingKey{x}, rest[to:]...)...)
	p.pls[k].Items = rest
	return nil
}
func (p *fakePlaylists) Delete(_ context.Context, u int64, k core.RatingKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pls, k)
	return nil
}

// ---------------------------------------------------------------------------
// Fixture

const (
	ownerToken  = "owner-token"
	friendToken = "friend-token"
)

var (
	ownerUser  = core.User{ID: 1, PlexID: 1001, Username: "owner", Title: "Owner", IsOwner: true, AudioLanguages: []string{"en"}}
	friendUser = core.User{ID: 2, PlexID: 2002, Username: "friend", Title: "Friend"}

	fightClub = core.Title{
		Key: core.MovieKey(550), Type: core.TypeMovie, IDs: core.IDs{TMDB: 550, IMDB: "tt0137523"},
		Title: "Fjord Harbor", Summary: "An insomniac office worker...", Tagline: "Quiet waters run deep.",
		Year: 1999, Released: time.Date(1999, 10, 15, 0, 0, 0, 0, time.UTC), Runtime: 139 * time.Minute,
		Genres: []string{"Drama", "Thriller"}, Studios: []string{"Northlight Pictures"}, Countries: []string{"United States of America"},
		ContentRating: "R", Rating: 8.43, Popularity: 60, PosterPath: "/poster550.jpg", BackdropPath: "/backdrop550.jpg",
		Cast:      []core.Person{{TMDB: 287, Name: "Theo Varga", Role: "Alex Mercer", ThumbPath: "/brad.jpg"}, {TMDB: 819, Name: "Lucas Brandt", Role: "The Narrator"}},
		Directors: []core.Person{{TMDB: 7467, Name: "Mara Ellison"}},
		Writers:   []core.Person{{TMDB: 7468, Name: "Sasha Petrov"}},
		UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	alien = core.Title{
		Key: core.MovieKey(348), Type: core.TypeMovie, IDs: core.IDs{TMDB: 348},
		Title: "Alien", Year: 1979, Runtime: 117 * time.Minute, Genres: []string{"Horror", "Science Fiction"},
		Popularity: 40, PosterPath: "/alien.jpg", UpdatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	breakingBad = core.Title{
		Key: core.ShowKey(1396), Type: core.TypeShow, IDs: core.IDs{TMDB: 1396, TVDB: 81189},
		Title: "Boundary Bay", Year: 2008, Summary: "A chemistry teacher...", ChildCount: 1, LeafCount: 2,
		AddedAt:    time.Date(2008, 1, 27, 0, 0, 0, 0, time.UTC),
		Popularity: 100, PosterPath: "/bb.jpg", BackdropPath: "/bbart.jpg", UpdatedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}
	bbS1 = core.Title{
		Key: core.SeasonKey(1396, 1), Type: core.TypeSeason, ShowKey: core.ShowKey(1396), ShowTitle: "Boundary Bay",
		AddedAt: time.Date(2008, 1, 27, 0, 0, 0, 0, time.UTC),
		Season:  1, Title: "Season 1", ChildCount: 2, PosterPath: "/bbs1.jpg", UpdatedAt: breakingBad.UpdatedAt,
	}
	bbE1 = core.Title{
		Key: core.EpisodeKey(1396, 1, 1), Type: core.TypeEpisode, ShowKey: core.ShowKey(1396), ShowTitle: "Boundary Bay",
		Season: 1, Episode: 1, Title: "Pilot", Runtime: 58 * time.Minute, StillPath: "/pilot.jpg",
		Released: time.Date(2008, 1, 20, 0, 0, 0, 0, time.UTC), UpdatedAt: breakingBad.UpdatedAt,
	}
	bbE2 = core.Title{
		Key: core.EpisodeKey(1396, 1, 2), Type: core.TypeEpisode, ShowKey: core.ShowKey(1396), ShowTitle: "Boundary Bay",
		Season: 1, Episode: 2, Title: "Second Watch...", Runtime: 48 * time.Minute,
		Released: time.Date(2008, 1, 27, 0, 0, 0, 0, time.UTC), UpdatedAt: breakingBad.UpdatedAt,
	}

	sampleInfo = core.MediaInfo{
		Container: "mkv", Duration: 139 * time.Minute, Size: 8_000_000_000, BitrateKbps: 7600,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080, FrameRate: 23.976, BitDepth: 8, BitrateKbps: 7000},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, Default: true, BitrateKbps: 640},
			{Index: 2, Type: "audio", Codec: "aac", Language: "fr", Channels: 2, SampleRate: 48000, BitrateKbps: 128},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: -1, Type: "subtitle", Codec: "srt", Language: "es", External: true, ExternalURL: "/dev/null"},
		},
	}
)

type fixture struct {
	srv      *Server
	catalog  *fakeCatalog
	state    *fakeState
	playback *fakePlayback
	trans    *fakeTranscoder
	pls      *fakePlaylists
	cols     *fakeCollections
	hooks    chan string
	art      *fakeArtwork
	broker   *sessions.Broker
}

func newFixture(opts ...func(*Deps)) *fixture {
	cat := &fakeCatalog{
		titles: map[core.RatingKey]core.Title{},
		children: map[core.RatingKey][]core.RatingKey{
			breakingBad.Key: {bbS1.Key},
			bbS1.Key:        {bbE1.Key, bbE2.Key},
		},
		facets: map[string][]core.FacetValue{
			"genre": {{Key: "18", Title: "Drama", Count: 1}, {Key: "27", Title: "Horror", Count: 1}},
			"year":  {{Key: "1979", Title: "1979", Count: 1}, {Key: "1999", Title: "1999", Count: 1}},
		},
	}
	for _, t := range []core.Title{fightClub, alien, breakingBad, bbS1, bbE1, bbE2} {
		cat.titles[t.Key] = t
	}
	f := &fixture{
		catalog:  cat,
		state:    newFakeState(),
		playback: newFakePlayback(sampleInfo),
		trans:    newFakeTranscoder(core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}),
		pls:      newFakePlaylists(),
		cols: &fakeCollections{
			cols:  map[core.RatingKey]core.Collection{core.CollectionKey(8091): {Key: core.CollectionKey(8091), Title: "Alien Collection", Kind: "franchise", Type: core.TypeMovie, ChildCount: 1, PosterPath: "/aliencol.jpg"}},
			items: map[core.RatingKey][]core.Title{core.CollectionKey(8091): {alien}},
		},
		hooks: make(chan string, 16),
		art:   newFakeArtwork(),
	}
	cat.state = f.state
	rec := &fakeRecommender{
		home: []core.Hub{{ID: "trending.week.movie", Title: "Trending This Week", Type: core.TypeMovie, Context: "hub.movie.trending", Promoted: true, Items: []core.Title{fightClub, alien}, More: true}},
		section: map[core.MediaType][]core.Hub{
			core.TypeMovie: {{ID: "movie.toprated", Title: "Top Rated", Type: core.TypeMovie, Context: "hub.movie.toprated", Items: []core.Title{fightClub}}},
		},
	}
	d := Deps{
		Catalog:          cat,
		Collections:      f.cols,
		Recommender:      rec,
		State:            f.state,
		Playlists:        f.pls,
		Auth:             &fakeAuth{users: map[string]core.User{ownerToken: ownerUser, friendToken: friendUser}},
		Playback:         f.playback,
		Transcoder:       f.trans,
		ApplyClientRules: resolver.ApplyClientRulesForMedia,
		HTTPClient:       &http.Client{Transport: f.art},
		Config: func() config.Config {
			c := config.Config{}
			c.Server.Port = 32400
			c.Server.FriendlyName = "Ether Test"
			c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
			c.Plex.Claimed = true
			c.Plex.OwnerUsername = "owner"
			c.Transcode.RemoteMaxBitrateKbps = 8000
			return c
		},
		Identity: Identity{Version: "1.43.4.10903-e5521bd8c", Platform: "Linux", PlatformVersion: "6.1"},
		Now:      func() time.Time { return fixedNow },
	}
	for _, o := range opts {
		o(&d)
	}
	f.srv = NewServer(testDeps(d))
	f.attachPlaybackBroker(d)
	return f
}

// fixturePlayback follows Server deps. Tests replace Playback after NewServer.
type fixturePlayback struct{ f *fixture }

func (p *fixturePlayback) current() core.Playback {
	if p.f.srv != nil && p.f.srv.d.Playback != nil {
		return p.f.srv.d.Playback
	}
	return p.f.playback
}

func (p *fixturePlayback) Estimate(ctx context.Context, t *core.Title, caps core.ClientCaps) core.MediaInfo {
	return p.current().Estimate(ctx, t, caps)
}
func (p *fixturePlayback) Prepare(ctx context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	return p.current().Prepare(ctx, u, k, caps)
}
func (p *fixturePlayback) Session(id string) (*core.PlaySession, bool) {
	return p.current().Session(id)
}
func (p *fixturePlayback) SessionFor(u int64, client string, k core.RatingKey) (*core.PlaySession, bool) {
	return p.current().SessionFor(u, client, k)
}
func (p *fixturePlayback) ServeRange(w http.ResponseWriter, r *http.Request, id string) {
	p.current().ServeRange(w, r, id)
}
func (p *fixturePlayback) InputURL(id string) string { return p.current().InputURL(id) }
func (p *fixturePlayback) Failover(ctx context.Context, id string, at time.Duration) (*core.PlaySession, error) {
	return p.current().Failover(ctx, id, at)
}
func (p *fixturePlayback) Stop(id string)                { p.current().Stop(id) }
func (p *fixturePlayback) Sessions() []*core.PlaySession { return p.current().Sessions() }
func (p *fixturePlayback) PrepareSurface(ctx context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps, releaseID string, exact bool) (*core.PlaySession, error) {
	if scoped, ok := p.current().(core.SurfaceScopedPlayback); ok {
		return scoped.PrepareSurface(ctx, u, k, caps, releaseID, exact)
	}
	return nil, core.ErrNotAvailable
}
func (p *fixturePlayback) SessionForSurface(surface string, userID int64, clientID string, key core.RatingKey, releaseID string, exact bool) (*core.PlaySession, bool) {
	if scoped, ok := p.current().(core.SurfaceScopedPlayback); ok {
		return scoped.SessionForSurface(surface, userID, clientID, key, releaseID, exact)
	}
	return nil, false
}
func (p *fixturePlayback) MarkSurfaceOpened(id string) bool {
	if scoped, ok := p.current().(core.SurfaceScopedPlayback); ok {
		return scoped.MarkSurfaceOpened(id)
	}
	return true
}
func (p *fixturePlayback) MarkSurfacePrepared(id string) bool {
	if scoped, ok := p.current().(core.SurfaceScopedPlayback); ok {
		return scoped.MarkSurfacePrepared(id)
	}
	return true
}
func (p *fixturePlayback) PinSourceRevision(ctx context.Context, id, revision string) error {
	if pins, ok := p.current().(core.PlaybackRevisionPin); ok {
		return pins.PinSourceRevision(ctx, id, revision)
	}
	return nil
}
func (p *fixturePlayback) UnpinSourceRevision(id string) {
	if pins, ok := p.current().(core.PlaybackRevisionPin); ok {
		pins.UnpinSourceRevision(id)
	}
}
func (p *fixturePlayback) CurrentSource(ctx context.Context, id string) (core.PlaybackSourceObservation, error) {
	if snap, ok := p.current().(core.PlaybackSourceSnapshot); ok {
		return snap.CurrentSource(ctx, id)
	}
	return core.PlaybackSourceObservation{}, core.ErrNotAvailable
}
func (p *fixturePlayback) Touch(id string) {
	if toucher, ok := p.current().(interface{ Touch(string) }); ok {
		toucher.Touch(id)
	}
}
func (p *fixturePlayback) PlaybackProgress(userID int64, clientID string, key core.RatingKey, position time.Duration, state string) {
	if progress, ok := p.current().(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(userID, clientID, key, position, state)
	}
}

type fixtureTranscoder struct{ f *fixture }

func (t fixtureTranscoder) current() core.Transcoder {
	if t.f.srv != nil && t.f.srv.d.Transcoder != nil {
		return t.f.srv.d.Transcoder
	}
	return t.f.trans
}
func (t fixtureTranscoder) Probe(ctx context.Context, inputURL string) (*core.MediaInfo, error) {
	return t.current().Probe(ctx, inputURL)
}
func (t fixtureTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return t.current().Decide(info, caps, opts)
}
func (t fixtureTranscoder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	return t.current().StartHLS(ctx, req)
}
func (t fixtureTranscoder) Session(id string) (core.HLSSession, bool) {
	return t.current().Session(id)
}
func (t fixtureTranscoder) Stop(id string)        { t.current().Stop(id) }
func (t fixtureTranscoder) HWAccel() core.HWAccel { return t.current().HWAccel() }

type fixtureTrickplay struct{ f *fixture }

func (t fixtureTrickplay) provider() core.TrickplayProvider {
	if t.f.srv != nil && t.f.srv.d.Trickplay != nil {
		return t.f.srv.d.Trickplay
	}
	return nil
}
func (t fixtureTrickplay) EnsureTrickplay(ctx context.Context, src core.TrickplaySource) {
	if p := t.provider(); p != nil {
		p.EnsureTrickplay(ctx, src)
	}
}
func (t fixtureTrickplay) TrickplayAvailable(id string) bool {
	if p := t.provider(); p != nil {
		return p.TrickplayAvailable(id)
	}
	return false
}
func (t fixtureTrickplay) TrickplayBIF(id string) ([]byte, bool) {
	if p := t.provider(); p != nil {
		return p.TrickplayBIF(id)
	}
	return nil, false
}
func (t fixtureTrickplay) TrickplayFrame(id string, at time.Duration) ([]byte, bool) {
	if p := t.provider(); p != nil {
		return p.TrickplayFrame(id, at)
	}
	return nil, false
}
func (t fixtureTrickplay) SetTrickplayFallback(id string, jpeg []byte) {
	if p := t.provider(); p != nil {
		p.SetTrickplayFallback(id, jpeg)
	}
}
func (t fixtureTrickplay) AcquireTrickplay(ctx context.Context, src core.TrickplaySource) (core.TrickplayLease, error) {
	p := t.provider()
	if p == nil {
		return nil, core.ErrNotAvailable
	}
	if leaser, ok := p.(core.TrickplayLeaser); ok {
		return leaser.AcquireTrickplay(ctx, src)
	}
	p.EnsureTrickplay(ctx, src)
	return fixtureTrickplayLease{ensure: p.EnsureTrickplay}, nil
}

type fixtureTrickplayLease struct {
	ensure func(context.Context, core.TrickplaySource)
}

func (l fixtureTrickplayLease) Ensure(ctx context.Context, src core.TrickplaySource) {
	if l.ensure != nil {
		l.ensure(ctx, src)
	}
}
func (fixtureTrickplayLease) Release(bool) {}

type fixtureProgress struct{ f *fixture }

func (p fixtureProgress) Progress(ctx context.Context, viewer int64, key core.RatingKey, position, duration time.Duration, state string) error {
	if p.f.srv != nil && p.f.srv.d.State != nil {
		return p.f.srv.d.State.Progress(ctx, viewer, key, position, duration, state)
	}
	return p.f.state.Progress(ctx, viewer, key, position, duration, state)
}

func (f *fixture) attachPlaybackBroker(d Deps) {
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id <= 0 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := sessions.NewEventBus(nil, active)
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		DB: d.DB, Cluster: d.Cluster,
		Lookup: func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			if f.srv != nil && f.srv.d.Catalog != nil {
				return f.srv.d.Catalog.Get(ctx, key)
			}
			if d.Catalog != nil {
				return d.Catalog.Get(ctx, key)
			}
			return f.catalog.Get(ctx, key)
		},
		Playback: &fixturePlayback{f: f}, Transcoder: fixtureTranscoder{f: f}, Progress: fixtureProgress{f: f}, Trickplay: fixtureTrickplay{f: f}, ActiveViewer: active,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline: func(v *core.Viewer, key core.RatingKey, pos, dur time.Duration, state string) {
			if v == nil {
				return
			}
			u := *v
			if u.Username == "" && u.ID == ownerUser.ID {
				u = ownerUser
			}
			select {
			case f.hooks <- fmt.Sprintf("%s %s %d %s", u.Username, key, pos.Milliseconds(), state):
			default:
			}
		},
		OpenSubtitle: func(id, format string) (io.ReadCloser, string, error) {
			if f.srv == nil || f.srv.d.Subtitles == nil {
				return nil, "", core.ErrNotAvailable
			}
			return f.srv.d.Subtitles.OpenFormat(id, format)
		},
		Events: events, Now: d.Now,
	})
	if err != nil {
		panic(err)
	}
	f.broker = broker
	f.srv.UsePlaybackBroker(sessions.BindSurface(broker, "plex"))
	f.srv.UsePlexStreams(sessions.BindStreams(broker))
}

func (f *fixture) bindStream(key core.RatingKey, index int, info core.StreamInfo) int64 {
	return f.srv.assignStream(context.Background(), &ownerUser, key, index, info, "")
}

// mountTestBroker is the same broker the app mounts. A server that renders
// stream ids uses it; there is no second id map.
func mountTestBroker(s *Server, now func() time.Time) {
	if s == nil {
		return
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id <= 0 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := sessions.NewEventBus(nil, active)
	progress, _ := s.d.State.(sessions.ProgressWriter)
	if progress == nil {
		progress = discardProgress{}
	}
	if now == nil {
		now = time.Now
	}
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Lookup: func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			if s.d.Catalog == nil {
				return nil, core.ErrNotFound
			}
			return s.d.Catalog.Get(ctx, key)
		},
		Playback: s.d.Playback, Transcoder: s.d.Transcoder, Progress: progress, ActiveViewer: active,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		OpenSubtitle: func(id, format string) (io.ReadCloser, string, error) {
			if s.d.Subtitles == nil {
				return nil, "", core.ErrNotAvailable
			}
			return s.d.Subtitles.OpenFormat(id, format)
		},
		Events: events, Now: now,
	})
	if err != nil {
		panic(err)
	}
	s.UsePlaybackBroker(sessions.BindSurface(broker, "plex"))
	s.UsePlexStreams(sessions.BindStreams(broker))
}

type discardProgress struct{}

func (discardProgress) Progress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) error {
	return nil
}

func (t *fakeTranscoder) StartEncoder(_ context.Context, r transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	return &livetvtest.Encoder{Index: livetvtest.Index{Payload: "shared-ts-", Height: r.Decision.MaxHeight}}, nil
}
func (t *fakeTranscoder) StartDecoder(_ context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
	return &livetvtest.Decoder{Format: r.Format}, nil
}
