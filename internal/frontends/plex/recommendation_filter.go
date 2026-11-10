package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type recommendationFilter interface {
	FilterRecommendations(context.Context, *frontend.Viewer, []frontend.Title) ([]frontend.Title, error)
}

// recommendationCandidates preserves the original first source request and
// expands only if a viewer exclusion leaves the bounded source page short.
func (s *Server) recommendationCandidates(ctx context.Context, user *frontend.Viewer, limit int, load func(context.Context, int) ([]frontend.Title, error)) ([]frontend.Title, error) {
	filter, ok := s.recommender().(recommendationFilter)
	if !ok || user == nil || limit <= 0 {
		return load(ctx, limit)
	}
	fetch := limit
	for {
		items, err := load(ctx, fetch)
		if err != nil {
			return nil, err
		}
		filtered, err := filter.FilterRecommendations(ctx, user, items)
		if err != nil {
			return nil, err
		}
		// Preserve even a custom source's overfull first response when nothing
		// was excluded; adapters must not alter legacy output in that case.
		if fetch == limit && len(filtered) == len(items) {
			return filtered, nil
		}
		if len(filtered) >= limit || len(items) < fetch || fetch >= 1000 || ctx.Err() != nil {
			if len(filtered) > limit {
				filtered = filtered[:limit]
			}
			return filtered, ctx.Err()
		}
		fetch = min(fetch*2, 1000)
	}
}
