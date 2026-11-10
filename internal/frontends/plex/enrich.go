package plex

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lru"
)

// ---------------------------------------------------------------------------
// Title detail cache.
//
// Hub rows and other lists come from TMDB list endpoints, whose results
// carry no credits, studios or certification. PMS lists include those
// (Director/Writer/Role/Country/Collection tags, studio, contentRating,
// tagline, clearLogo), and the newer apps show them on hub tiles and hero
// rows. Fetching every title's details inline would make listings slow, so
// the detail is fetched in the background on first sight and merged into
// later renders of the same title. A season or episode uses its show's
// cached detail for show-level fields (studio, rating, clear logo).

const (
	titleCacheEntries  = 4000
	titleCacheTTL      = 6 * time.Hour
	titleCacheRetry    = 30 * time.Minute // after a failed fetch
	titleFetchTimeout  = 20 * time.Second
	titleCacheMaxCast  = 10
	titleCacheMaxCrew  = 5
	titleFetchWorkers  = 3
	titleFetchQueueLen = 512
)

type titleEntry struct {
	key       frontend.RatingKey
	t         *frontend.Title // nil after a failed fetch
	fetchedAt time.Time
}

type titleCache struct {
	fetch func(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error)
	// prefetch, when set, reads a list's missing details in one batch
	// (warm) before they are fetched one by one from what it read, so a
	// hub of episodes costs a few statements for its shows, not a read each.
	prefetch func(ctx context.Context, keys []frontend.RatingKey)
	q        *bgQueue
	now      func() time.Time
	log      *slog.Logger

	mu       sync.Mutex
	items    *lru.Cache[frontend.RatingKey, *titleEntry]
	batching map[frontend.RatingKey]bool // in a warm batch not yet fetched
}

func newTitleCache(q *bgQueue, now func() time.Time, log *slog.Logger, fetch func(context.Context, frontend.RatingKey) (*frontend.Title, error)) *titleCache {
	return &titleCache{fetch: fetch, q: q, now: now, log: log, items: lru.New(lru.Options[frontend.RatingKey, *titleEntry]{MaxEntries: titleCacheEntries})}
}

// stale reports that key has no entry or an expired one. c.mu is held.
func (c *titleCache) staleLocked(key frontend.RatingKey) (*frontend.Title, bool) {
	e, ok := c.items.Get(key)
	if !ok {
		return nil, true
	}
	age := c.now().Sub(e.fetchedAt)
	return e.t, (e.t != nil && age >= titleCacheTTL) || (e.t == nil && age >= titleCacheRetry)
}

// get returns the cached detail for key without blocking. A missing or
// expired entry schedules a background fetch; a stale entry is still
// returned meanwhile.
func (c *titleCache) get(key frontend.RatingKey) *frontend.Title {
	c.mu.Lock()
	t, refresh := c.staleLocked(key)
	refresh = refresh && !c.batching[key]
	c.mu.Unlock()
	if refresh {
		c.schedule(key)
	}
	return t
}

// warm schedules one background job for the keys of a list about to be
// rendered whose detail is missing or expired: their stored titles are
// read in one batch (prefetch), then each is fetched from what it read.
// The list's own get calls then schedule nothing for them.
func (c *titleCache) warm(keys []frontend.RatingKey) {
	if c.prefetch == nil || len(keys) < 2 {
		return
	}
	c.mu.Lock()
	var batch []frontend.RatingKey
	for _, key := range keys {
		if _, stale := c.staleLocked(key); stale && !c.batching[key] {
			if c.batching == nil {
				c.batching = map[frontend.RatingKey]bool{}
			}
			c.batching[key] = true
			batch = append(batch, key)
		}
	}
	c.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	done := func() {
		c.mu.Lock()
		for _, key := range batch {
			delete(c.batching, key)
		}
		c.mu.Unlock()
	}
	if !c.q.submit("titles:"+strconv.FormatInt(int64(batch[0]), 10)+":"+strconv.Itoa(len(batch)), func(ctx context.Context) {
		defer done()
		fctx, cancel := context.WithTimeout(ctx, titleFetchTimeout)
		defer cancel()
		if len(batch) > 1 {
			c.prefetch(fctx, batch)
		}
		for _, key := range batch {
			c.fetchOne(ctx, fctx, key)
		}
	}) {
		done()
	}
}

// peek returns the cached detail for key without scheduling a background fetch.
func (c *titleCache) peek(key frontend.RatingKey) *frontend.Title {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items.Peek(key); ok {
		return e.t
	}
	return nil
}

func (c *titleCache) schedule(key frontend.RatingKey) {
	c.q.submit("title:"+strconv.FormatInt(int64(key), 10), func(ctx context.Context) {
		fctx, cancel := context.WithTimeout(ctx, titleFetchTimeout)
		defer cancel()
		c.fetchOne(ctx, fctx, key)
	})
}

// fetchOne fetches key's detail into the cache; ctx is the queue's.
func (c *titleCache) fetchOne(ctx, fctx context.Context, key frontend.RatingKey) {
	t, err := c.fetch(fctx, key)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: don't record a failure
		}
		c.log.Debug("title detail fetch failed", "key", key, "err", err)
		c.store(key, nil)
		return
	}
	c.put(t)
}

// put records a detailed title (e.g. one just fetched for a detail page).
func (c *titleCache) put(t *frontend.Title) {
	if t == nil || t.Key == 0 {
		return
	}
	c.store(t.Key, trimForCache(t))
}

func (c *titleCache) store(key frontend.RatingKey, t *frontend.Title) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items.Peek(key); ok && t == nil && old.t != nil {
		// Keep serving the stale detail; retry later.
		old.fetchedAt = c.now().Add(-titleCacheTTL + titleCacheRetry)
		return
	}
	c.items.Add(key, &titleEntry{key: key, t: t, fetchedAt: c.now()})
}

// trimForCache copies the fields list renders need, bounding credit lists.
func trimForCache(t *frontend.Title) *frontend.Title {
	cp := *t
	cp.Keywords = nil
	cp.Cast = clipPeople(t.Cast, titleCacheMaxCast)
	cp.Directors = clipPeople(t.Directors, titleCacheMaxCrew)
	cp.Writers = clipPeople(t.Writers, titleCacheMaxCrew)
	cp.Genres = append([]string(nil), t.Genres...)
	cp.Studios = append([]string(nil), t.Studios...)
	cp.Countries = append([]string(nil), t.Countries...)
	return &cp
}

func clipPeople(ps []frontend.Person, n int) []frontend.Person {
	if len(ps) > n {
		ps = ps[:n]
	}
	return append([]frontend.Person(nil), ps...)
}

// lacksDetail reports whether a movie or show came from a list summary
// without credits, studios or certification.
func lacksDetail(t *frontend.Title) bool {
	return len(t.Cast) == 0 && len(t.Directors) == 0 && len(t.Writers) == 0 && len(t.Studios) == 0 && t.ContentRating == ""
}

// mergeDetail fills fields the summary lacks from the detailed title.
func mergeDetail(t *frontend.Title, d *frontend.Title) {
	if len(t.Genres) == 0 {
		t.Genres = d.Genres
	}
	if len(t.Studios) == 0 {
		t.Studios = d.Studios
	}
	if len(t.Countries) == 0 {
		t.Countries = d.Countries
	}
	if len(t.Cast) == 0 {
		t.Cast = d.Cast
	}
	if len(t.Directors) == 0 {
		t.Directors = d.Directors
	}
	if len(t.Writers) == 0 {
		t.Writers = d.Writers
	}
	if t.ContentRating == "" {
		t.ContentRating = d.ContentRating
	}
	if t.Tagline == "" {
		t.Tagline = d.Tagline
	}
	if t.LogoPath == "" {
		t.LogoPath = d.LogoPath
	}
	if t.CollectionID == 0 && d.CollectionID != 0 {
		t.CollectionID, t.CollectionName = d.CollectionID, d.CollectionName
	}
	if t.Runtime == 0 {
		t.Runtime = d.Runtime
	}
	if t.ChildCount == 0 {
		t.ChildCount = d.ChildCount
	}
	if t.LeafCount == 0 {
		t.LeafCount = d.LeafCount
	}
	if t.IDs.IMDB == "" {
		t.IDs.IMDB = d.IDs.IMDB
	}
	if t.IDs.TVDB == 0 {
		t.IDs.TVDB = d.IDs.TVDB
	}
	if t.BackdropPath == "" {
		t.BackdropPath = d.BackdropPath
	}
	if t.PosterPath == "" {
		t.PosterPath = d.PosterPath
	}
}

// warmTitleDetails schedules, in one batch, the detail a list's rendering
// will ask the detail cache for (enrich, showOfOpts): movies and shows that
// lack it, and the shows of seasons and episodes.
func (s *Server) warmTitleDetails(ts []frontend.Title, storedOnly bool) {
	if s.titles == nil {
		return
	}
	var keys []frontend.RatingKey
	seen := map[frontend.RatingKey]bool{}
	add := func(k frontend.RatingKey) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for i := range ts {
		t := &ts[i]
		switch t.Type {
		case frontend.TypeMovie, frontend.TypeShow:
			if (t.Key.Kind() == frontend.KindMovie || t.Key.Kind() == frontend.KindShow) && lacksDetail(t) {
				add(t.Key)
			}
		case frontend.TypeSeason, frontend.TypeEpisode:
			if storedOnly {
				continue
			}
			show := t.ShowKey
			if show == 0 {
				show = t.Key.Parent()
				if t.Type == frontend.TypeEpisode {
					show = show.Parent()
				}
			}
			if show.Kind() == frontend.KindShow {
				add(show)
			}
		}
	}
	s.titles.warm(keys)
}

// enrich returns t with list-summary gaps filled from the detail cache.
// Movies and shows are enriched from their own detail; t is returned
// unchanged when nothing is cached yet (a fetch is scheduled).
func (s *Server) enrich(t *frontend.Title) *frontend.Title {
	if s.titles == nil {
		return t
	}
	switch t.Type {
	case frontend.TypeMovie, frontend.TypeShow:
	default:
		return t
	}
	switch t.Key.Kind() {
	case frontend.KindMovie, frontend.KindShow:
	default:
		return t // spike/local items have no TMDB detail
	}
	if !lacksDetail(t) {
		return t
	}
	d := s.titles.get(t.Key)
	if d == nil {
		return t
	}
	cp := *t
	mergeDetail(&cp, d)
	return &cp
}

func (s *Server) showOfOpts(t *frontend.Title, storedOnly bool) *frontend.Title {
	if s.titles == nil || (t.Type != frontend.TypeSeason && t.Type != frontend.TypeEpisode) {
		return nil
	}
	show := t.ShowKey
	if show == 0 {
		show = t.Key.Parent()
		if t.Type == frontend.TypeEpisode {
			show = show.Parent()
		}
	}
	if show.Kind() != frontend.KindShow {
		return nil
	}
	if storedOnly {
		return s.titles.peek(show)
	}
	return s.titles.get(show)
}

// rememberTitle caches a detailed title fetched for a detail page.
func (s *Server) rememberTitle(t *frontend.Title) {
	if s.titles == nil || t == nil {
		return
	}
	switch t.Key.Kind() {
	case frontend.KindMovie, frontend.KindShow:
		if !lacksDetail(t) {
			s.titles.put(t)
		}
	}
}

// enrichFetch reads the detail a rendered title's gaps are filled from. A
// search or list hit with no row is read from memory: rendering a search
// page must not open (promote) every hit on it, only opening a title does.
func (s *Server) enrichFetch(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
	if m, ok := s.catalog().(memoryTitles); ok {
		t, stored, err := m.MemoryTitle(ctx, key)
		switch {
		case err != nil:
			return nil, err
		case !stored && t != nil:
			return t, nil
		case !stored:
			return nil, frontend.ErrNotFound
		}
	}
	return s.catalog().Get(ctx, key)
}
