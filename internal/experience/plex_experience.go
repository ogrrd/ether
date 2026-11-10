package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// NewDirectService constructs a Service for direct/standalone callers (such as
// spike and unit tests) that supply a subset of ServiceDeps.
func NewDirectService(d ServiceDeps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Visibility.FilterAvailable != nil && d.FilterAvailable == nil {
		d.FilterAvailable = d.Visibility.FilterAvailable
	}
	if d.Visibility.JudgedOnFacts != nil && d.JudgedOnFacts == nil {
		d.JudgedOnFacts = d.Visibility.JudgedOnFacts
	}
	d.Dashboard, d.Resources = Statistics(d.Dashboard, d.Resources, d.Now)
	return &Service{d: d, cursors: &episodeCursors{}}
}

func (*Service) NewRecent(o frontend.RecentOptions) frontend.Recent {
	return Recent{Visibility: o.Visibility, Unrestricted: o.Unrestricted}
}
func (*Service) NewDetailRelated(o frontend.DetailRelatedOptions) frontend.DetailRelated {
	return DetailRelated{Source: o.Source, Visibility: o.Visibility, Get: o.Get}
}
func (*Service) NewEpisodeBatch(o frontend.EpisodeBatchOptions) frontend.EpisodeBatch {
	b := EpisodeBatch{Catalog: o.Catalog, State: o.State}
	if o.EpisodeOrder != nil {
		b.EpisodeOrder = o.EpisodeOrder
	}
	return b
}
func (*Service) NewContinueDeck(o frontend.ContinueDeckOptions) frontend.ContinueDeck {
	return &ContinueDeck{Load: o.Load, Order: o.Order, Hydrate: o.Hydrate, Unavailable: o.Unavailable, Fallback: o.Fallback}
}
func (*Service) NewContinue(o frontend.ContinueOptions) frontend.Continue {
	c := Continue{
		ShowOnDeck: o.ShowOnDeck, PrepareShowOnDeck: o.PrepareShowOnDeck, PrefetchTitles: o.PrefetchTitles,
		Filter: o.Filter, State: o.State, Now: o.Now, Grandchildren: o.Grandchildren,
		NextEpisode: o.NextEpisode, NextEpisodes: o.NextEpisodes, Visible: o.Visible,
		SameDayUnavailable: o.SameDayUnavailable, GetTitles: o.GetTitles, SectionAllowed: o.SectionAllowed,
		OnError: o.OnError, FavoriteShows: o.FavoriteShows, Unrestricted: o.Unrestricted,
	}
	if o.EpisodeOrder != nil {
		c.EpisodeOrder = o.EpisodeOrder
	}
	return c
}
func (*Service) NewRowSelection(o frontend.RowSelectionOptions) frontend.RowSelection {
	return RowSelection{Visibility: o.Visibility, PrioritizeAvailable: o.PrioritizeAvailable, SameDayUnavailable: o.SameDayUnavailable}
}
func (*Service) NewHomeSelection(o frontend.HomeSelectionOptions) frontend.HomeSelection {
	return HomeSelection{Source: o.Source, Visibility: o.Visibility, Restricted: o.Restricted}
}
func (*Service) NewScopedCollection(o frontend.ScopedCollectionOptions) frontend.ScopedCollection {
	return ScopedCollection{Collections: o.Collections, Visibility: o.Visibility, Now: o.Now, Unrestricted: o.Unrestricted, TitleAllowed: o.TitleAllowed, Openable: o.Openable}
}
func (*Service) NewPlaylistSelection(o frontend.PlaylistSelectionOptions) frontend.PlaylistSelection {
	return PlaylistSelection{Playlists: o.Playlists, Collections: o.Collections, Visibility: o.Visibility, Children: o.Children, Grandchildren: o.Grandchildren}
}
func (*Service) NewPostPlaySelection(o frontend.PostPlaySelectionOptions) frontend.PostPlaySelection {
	return PostPlaySelection{Visibility: o.Visibility, Unrestricted: o.Unrestricted, Picks: o.Picks, Related: o.Related}
}
func (*Service) NewDiscoverySelection(o frontend.DiscoverySelectionOptions) frontend.DiscoverySelection {
	return DiscoverySelection{Visibility: o.Visibility, Unrestricted: o.Unrestricted}
}
func (*Service) NewTrackPreferenceSelector(o frontend.TrackPreferenceSelectorOptions) frontend.TrackPreferenceSelector {
	return TrackPreferences{Source: o.Source, GetTitle: o.GetTitle}
}
func (*Service) SearchFor(src frontend.SearchSources, vis frontend.Visibility, unrestricted func(*core.User) bool) frontend.Search {
	return SearchFor(src, vis, unrestricted)
}
func (*Service) StatisticsDefaults(dashboard frontend.Dashboard, resources frontend.ResourceStatistics, now func() time.Time) (frontend.Dashboard, frontend.ResourceStatistics) {
	return Statistics(dashboard, resources, now)
}
func (*Service) MergeShareFilter(filter, share core.Filter) (core.Filter, bool) {
	return MergeShareFilter(filter, share)
}
func (*Service) ArrangeHome(ctx context.Context, source core.Recommender, viewer *core.User, hc core.HubContext, section string, rows []core.Hub, candidates []core.NeutralHomeRow) ([]int, error) {
	return ArrangeHome(ctx, source, viewer, hc, section, rows, candidates)
}
func (*Service) DetailExtras(ctx context.Context, source frontend.ExtrasSource, parent *core.Title, language string, wanted map[int]bool) ([]frontend.ExtraClip, error) {
	return DetailExtras(ctx, source, parent, language, wanted)
}
func (*Service) DiscoverySearch(source *frontend.DiscoveryFeatures) func(context.Context, *core.User, string, int) ([]core.Title, bool) {
	return DiscoverySearch(source)
}
func (*Service) FamiliarFrom(st any) func(context.Context, *core.User, []core.RatingKey) map[core.RatingKey]int {
	return FamiliarFrom(st)
}
func (*Service) ContinueFilter(state core.State, personal func(context.Context, *core.User, []core.Title) []core.Title) func(context.Context, *core.User, []core.Title) []core.Title {
	return ContinueFilter(state, personal)
}
func (*Service) SameDayUnavailable(ctx context.Context, title *core.Title, now time.Time, filter func(context.Context, []core.Title) []core.Title, source frontend.AvailabilitySource) bool {
	return SameDayUnavailable(ctx, title, now, filter, source)
}
func (*Service) SeasonNextEpisode(eps []core.Title, st map[core.RatingKey]core.WatchStatus, now time.Time, unavailable func(*core.Title) bool) *core.Title {
	return SeasonNextEpisode(eps, st, now, unavailable)
}
func (*Service) ShowOnDeck(eps []core.Title, st map[core.RatingKey]core.WatchStatus, unavailable func(*core.Title) bool) *core.Title {
	return ShowOnDeck(eps, st, unavailable)
}
func (*Service) RequestDatabaseObservation(ctx context.Context) (int64, time.Duration) {
	return RequestDatabaseObservation(ctx)
}
func (*Service) PlaybackMarkerRanges(ctx context.Context, source frontend.MarkerRanges, key core.RatingKey, info core.MediaInfo) ([]frontend.MarkerRange, error) {
	return PlaybackMarkerRanges(ctx, source, key, info)
}

func (s *Service) CatalogSource() core.Catalog         { return s.d.Catalog }
func (s *Service) CollectionsSource() core.Collections { return s.d.Collections }
func (s *Service) StateSource() core.State             { return s.d.State }
func (s *Service) PlaylistsSource() core.Playlists     { return s.d.Playlists }
func (s *Service) RecommenderSource() core.Recommender {
	if s.d.Recommender != nil {
		return s.d.Recommender
	}
	return s.d.Related
}
func (s *Service) VibeSearchSource() frontend.VibeSearcher { return s.d.VibeSearch }
func (s *Service) SearchSources() frontend.SearchSources   { return s.d.SearchSources }
func (s *Service) FavoriteShowsFunc() func(context.Context, *core.User) (map[core.RatingKey]bool, error) {
	return s.d.FavoriteShows
}
func (s *Service) FilterAvailableFunc() func(context.Context, []core.Title) []core.Title {
	if s.d.FilterAvailable != nil {
		return s.d.FilterAvailable
	}
	return s.d.Visibility.FilterAvailable
}
func (s *Service) JudgedOnFactsFunc() func(context.Context) func(core.Title) bool {
	if s.d.JudgedOnFacts != nil {
		return s.d.JudgedOnFacts
	}
	return s.d.Visibility.JudgedOnFacts
}
func (s *Service) PrioritizeAvailableFunc() func(context.Context, []core.Title) []core.Title {
	return s.d.PrioritizeAvailable
}
func (s *Service) EpisodeOrderSource() frontend.EpisodeOrder      { return s.d.EpisodeOrder }
func (s *Service) DiscoveryFeatures() *frontend.DiscoveryFeatures { return s.d.Discovery }
func (s *Service) DashboardSource() frontend.Dashboard {
	now := s.d.Now
	if now == nil {
		now = time.Now
	}
	dash, _ := Statistics(s.d.Dashboard, s.d.Resources, now)
	return dash
}
func (s *Service) ResourcesSource() frontend.ResourceStatistics {
	now := s.d.Now
	if now == nil {
		now = time.Now
	}
	_, res := Statistics(s.d.Dashboard, s.d.Resources, now)
	return res
}
func (s *Service) LibraryTitles() (movies, shows string) {
	if s.d.LibraryTitles != nil {
		return s.d.LibraryTitles()
	}
	return "", ""
}
func (s *Service) HomeRevision(ctx context.Context, viewer *core.User) (frontend.HomeRevision, bool, error) {
	if s.d.HomeRevision == nil {
		return frontend.HomeRevision{}, false, nil
	}
	if viewer == nil {
		return frontend.HomeRevision{}, true, nil
	}
	rev, err := s.d.HomeRevision(ctx, viewer)
	return rev, true, err
}
func (s *Service) NoProviderWait(ctx context.Context) context.Context {
	if s.d.NoProviderWait != nil {
		return s.d.NoProviderWait(ctx)
	}
	return ctx
}

var _ frontend.ExperienceServices = (*Service)(nil)
