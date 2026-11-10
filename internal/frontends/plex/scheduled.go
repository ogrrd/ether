package plex

import (
	"context"
	"sort"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func (s *Server) scheduledEpisodes(ctx context.Context, pr *Request, season frontend.RatingKey) []frontend.Title {
	if !s.cfg().Catalog.ShowUpcomingEpisodes || season.Kind() != frontend.KindSeason || season.Season() <= 0 {
		return nil
	}
	schedule, ok := s.catalog().(frontend.CatalogSchedule)
	if !ok {
		return nil
	}
	// Alternate groups have their own numbering; do not attach canonical future
	// episodes to an unrelated group or offer a detail page outside that tree.
	if s.episodeOrder() != nil {
		grouped, err := s.episodeOrder().Grouped(ctx, pr.User, []frontend.RatingKey{frontend.ShowKey(season.TMDB())})
		if err != nil || grouped[frontend.ShowKey(season.TMDB())] {
			return nil
		}
	}
	parent, err := s.catalog().Get(ctx, season)
	if err != nil || !s.visibleTitle(ctx, pr.User, parent) {
		return nil
	}
	episodes, err := schedule.ScheduledEpisodes(ctx, season)
	if err != nil {
		return nil
	}
	out := make([]frontend.Title, 0, len(episodes))
	for _, ep := range episodes {
		if ep.Type == frontend.TypeEpisode && ep.Key.Parent() == season && ep.Released.After(s.now()) && s.playableTitle(ctx, pr.User, &ep) {
			out = append(out, ep)
		}
	}
	return out
}

func (s *Server) scheduledEpisode(ctx context.Context, pr *Request, key frontend.RatingKey) (*frontend.Title, error) {
	if key.Kind() == frontend.KindEpisode {
		for _, ep := range s.scheduledEpisodes(ctx, pr, key.Parent()) {
			if ep.Key == key {
				return &ep, nil
			}
		}
	}
	return nil, frontend.ErrNotFound
}

func (s *Server) scheduledMetadata(ctx context.Context, pr *Request, t *frontend.Title) model.Metadata {
	md := s.metadata(ctx, pr, t, renderOpts{})
	md.Media = nil
	md.Title += " (Unaired)"
	md.Summary = "Scheduled air date: " + t.Released.Format("2006-01-02") + ". Not available to play yet. " + md.Summary
	// Base artwork uses the title-scoped handler, which repeats the schedule and
	// viewer checks. Do not invoke overlays or a custom provider for placeholders.
	md.Thumb = s.thumbURL(t)
	md.Art = artURL(t)
	md.Image = nil
	return md
}

func appendScheduled(aired, future []frontend.Title) []frontend.Title {
	if len(future) == 0 {
		return aired
	}
	out := append(append([]frontend.Title(nil), aired...), future...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Episode < out[j].Episode })
	return out
}
