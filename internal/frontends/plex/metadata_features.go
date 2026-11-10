package plex

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func (s *Server) metadataTitle(ctx context.Context, pr *Request, k frontend.RatingKey) (*frontend.Title, error) {
	if t, ok := shelfMember(ctx, k); ok {
		return &t, nil
	}
	if s.episodeOrder() != nil {
		return s.episodeOrder().Get(ctx, pr.User, k)
	}
	return s.catalog().Get(ctx, k)
}
func (s *Server) metadataChildren(ctx context.Context, pr *Request, k frontend.RatingKey) ([]frontend.Title, error) {
	if s.episodeOrder() != nil {
		return s.episodeOrder().Children(ctx, pr.User, k)
	}
	return s.catalog().Children(ctx, k)
}
func (s *Server) metadataGrandchildren(ctx context.Context, pr *Request, k frontend.RatingKey) ([]frontend.Title, error) {
	if s.episodeOrder() != nil {
		return s.episodeOrder().Grandchildren(ctx, pr.User, k)
	}
	return s.catalog().Grandchildren(ctx, k)
}

// showEpisodes memoises shows' episodes for one response, so a show page
// counts its leaves, groups its seasons and picks On Deck from one load
// rather than one per feature and per season.
type showEpisodes map[frontend.RatingKey]showEpisodeList

type showEpisodeList struct {
	eps []frontend.Title
	err error
}

// episodesOfShow is metadataGrandchildren through memo (nil: not memoised).
func (s *Server) episodesOfShow(ctx context.Context, pr *Request, memo showEpisodes, show frontend.RatingKey) ([]frontend.Title, error) {
	if l, ok := memo[show]; ok {
		return l.eps, l.err
	}
	eps, err := s.metadataGrandchildren(ctx, pr, show)
	if memo != nil {
		memo[show] = showEpisodeList{eps, err}
	}
	return eps, err
}

// presentedSeasons groups a show's episodes by the season the viewer sees
// them in: their episode-order group, else their own season. Each group is
// what metadataChildren returns for that season.
func (s *Server) presentedSeasons(ctx context.Context, pr *Request, show frontend.RatingKey, eps []frontend.Title) map[int][]frontend.Title {
	grouped := false
	if s.episodeOrder() != nil {
		g, err := s.episodeOrder().Grouped(ctx, pr.User, []frontend.RatingKey{show})
		// If preferences cannot be read, ask for each episode's position.
		grouped = err != nil || g[show]
	}
	out := map[int][]frontend.Title{}
	for _, e := range eps {
		n := e.Season
		if grouped {
			if season, _, _, ok := s.episodeOrder().Position(ctx, pr.User, e.Key); ok {
				n = season
			}
		}
		out[n] = append(out[n], e)
	}
	return out
}

// episodeOrderView is a list's episode-order presentation, resolved before
// its items render. If preferences cannot be read, grouped is nil and each
// title consults the service itself. Detail pages still resolve show counts.
type episodeOrderView struct {
	detailedShows bool                        // detail pages still resolve canonical visible season counts
	grouped       map[frontend.RatingKey]bool // shows with an alternate episode group
	children      map[frontend.RatingKey]int  // visible children of those shows
}

// episodeOrderList resolves a list's episode orders in one preference query.
// Only shows with an alternate group load their layouts, concurrently; the
// rest keep the catalogue's season counts, so a row of shows never loads
// seasons tile by tile (each possibly a TMDB fetch). Loading a grouped show's
// children also caches the layout its episodes' positions come from.
func (s *Server) episodeOrderList(ctx context.Context, pr *Request, ts []frontend.Title, detail bool) episodeOrderView {
	var v episodeOrderView
	if s.episodeOrder() == nil {
		return v
	}
	var shows []frontend.RatingKey
	seen := map[frontend.RatingKey]bool{}
	for i := range ts {
		if ts[i].Type != frontend.TypeShow && ts[i].Type != frontend.TypeEpisode || ts[i].Key.TMDB() <= 0 {
			continue
		}
		if k := frontend.ShowKey(ts[i].Key.TMDB()); !seen[k] {
			seen[k] = true
			shows = append(shows, k)
		}
	}
	grouped, err := s.episodeOrder().Grouped(ctx, pr.User, shows)
	if err != nil {
		s.log.Warn("load episode orders", "err", err)
		return v
	}
	v.grouped, v.children = grouped, map[frontend.RatingKey]int{}
	if detail {
		v.detailedShows = true
		return v
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for k := range grouped {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A cancelled request starts no further layout loads.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if kids, e := s.metadataChildren(ctx, pr, k); e == nil {
				n := len(s.visibleTitles(pr.User, kids))
				mu.Lock()
				v.children[k] = n
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return v
}

// applyEpisodeOrder runs after canonical media rendering: filenames, playback,
// GUIDs and the resolver retain canonical identity while Plex sees the chosen
// group tree and display numbering.
func (s *Server) applyEpisodeOrder(ctx context.Context, pr *Request, t *frontend.Title, m *model.Metadata, v episodeOrderView) {
	if s.episodeOrder() == nil {
		return
	}
	if t.Type == frontend.TypeShow {
		if v.grouped != nil && !v.detailedShows {
			if n, ok := v.children[t.Key]; ok {
				m.ChildCount = model.Ptr(n)
			}
			return
		}
		if kids, e := s.metadataChildren(ctx, pr, t.Key); e == nil {
			m.ChildCount = model.Ptr(len(s.visibleTitles(pr.User, kids)))
		}
		return
	}
	if t.Type == frontend.TypeSeason {
		if t.Season >= 9000 {
			m.GUID = "local://episode-order/" + t.Key.String()
			m.Index = model.Ptr(t.Season - 8999)
		}
		return
	}
	if t.Type != frontend.TypeEpisode || v.grouped != nil && !v.grouped[frontend.ShowKey(t.Key.TMDB())] {
		return
	}
	season, index, name, ok := s.episodeOrder().Position(ctx, pr.User, t.Key)
	if !ok {
		return
	}
	parent := frontend.SeasonKey(t.Key.TMDB(), season)
	m.Index = model.Ptr(index)
	m.ParentIndex = model.Ptr(season)
	if season >= 9000 {
		m.ParentIndex = model.Ptr(season - 8999)
	}
	m.ParentTitle = name
	m.ParentRatingKey = parent.String()
	m.ParentKey = "/library/metadata/" + parent.String()
	m.ParentThumb = "/library/metadata/" + frontend.ShowKey(t.Key.TMDB()).String() + "/thumb"
	// A group is local presentation metadata, not an invented TMDB season.
	m.ParentGUID = "local://episode-order/" + parent.String()
}
