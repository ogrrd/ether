package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

type experienceSituationKey struct{}

func situationFor(req frontend.RowRequest) core.HomeSituation {
	h := core.NewHubContext(time.Now().Local(), req.Device.Product, req.Device.Platform, req.Device.DeviceName)
	h.ClientID = req.Device.ID
	h.SectionID, _ = strconv.Atoi(req.ViewID)
	return core.HomeSituation{Device: h.Device, Daypart: h.Daypart, Weekend: h.Weekend, Now: h.Now, LibraryID: h.SectionID, Product: h.Product, Platform: h.Platform, ClientID: h.ClientID, SessionID: h.SessionID}
}

func (a *App) scheduledEpisodes(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if a.catalog == nil {
		return nil, core.ErrNotFound
	}
	return a.catalog.ScheduledEpisodes(ctx, key)
}

func (a *App) groupedShows(ctx context.Context, viewer *core.User, shows []core.RatingKey) (map[core.RatingKey]bool, error) {
	if a.episodeorder == nil {
		return map[core.RatingKey]bool{}, nil
	}
	return a.episodeorder.Grouped(ctx, viewer, shows)
}

func (a *App) experienceSearch(vis experience.Visibility) experience.Search {
	unrestricted := func(viewer *core.User) bool {
		return a.safety != nil && viewer != nil && !a.safety.Restricted(viewer)
	}
	return experience.SearchFor(a.searchSources(), vis, unrestricted)
}

// searchSources is the one search every frontend uses (Plex included):
// the catalogue with the owner's availability rules, natural-language
// search, viewer affinity and collection readings.
func (a *App) searchSources() experience.SearchSources {
	src := experience.SearchSources{Catalog: a.catalog.Search, Details: a.catalog.SearchDetails}
	if a.availability != nil && !a.cfg.Read().Spike.Enabled {
		src.Catalog = (&availabilityCatalog{Catalog: a.catalog, availability: a.availability}).Search
	}
	if a.discoveryFeatures != nil {
		src.Natural = experience.DiscoverySearch(a.discoveryFeatures.Frontend())
	}
	if a.state != nil {
		src.Familiar = experience.FamiliarFrom(a.state)
	}
	if a.collections != nil {
		src.Vibe = a.collections
	}
	return src
}

// peopleIndex adapts the catalogue's people-name index to experience.
func (a *App) peopleIndex(ctx context.Context, terms []string, limit int) ([]experience.PersonCandidate, error) {
	found, err := a.catalog.MatchPeople(ctx, terms, limit)
	if err != nil {
		return nil, err
	}
	out := make([]experience.PersonCandidate, len(found))
	for i, m := range found {
		out[i] = experience.PersonCandidate{TMDB: m.TMDB, Name: m.Name}
	}
	return out, nil
}

// libraryName is the live name of library id from the libraries
// service, or fallback when the service is not wired (hand-built Apps
// in tests) or the row cannot be read.
func (a *App) libraryName(id core.LibraryID, fallback string) string {
	if a.libraries != nil {
		if lib, err := a.libraries.Get(context.Background(), id); err == nil {
			if n := strings.TrimSpace(lib.Name); n != "" {
				return n
			}
		}
	}
	return fallback
}

// libraryTitles is the live Movies / TV Shows name pair.
func (a *App) libraryTitles() (movies, shows string) {
	return a.libraryName(core.LibraryMovies, "Movies"), a.libraryName(core.LibraryShows, "TV Shows")
}

// configuredLibraries is the live Movies / TV Shows pair. IDs stay 1 and 2 so
// saved ParentId and section UUIDs do not change when the owner renames them.
func (a *App) configuredLibraries() []experience.Library {
	movies, shows := a.libraryTitles()
	return []experience.Library{{ID: 1, Name: movies, Type: core.TypeMovie, Virtual: true}, {ID: 2, Name: shows, Type: core.TypeShow, Virtual: true}}
}

// newSurfaceExperience builds an explicitly surface-bound service. A shared
// situation is inserted by surfaceExperience.Rows before ranking and receipt issuance.
func (a *App) newSurfaceExperience(protocol core.Protocol, artwork ...frontend.Artwork) (frontend.Experience, error) {
	if a.spikeLibrary != nil && protocol == "plex" {
		return a.spikeLibrary.Experience(), nil
	}
	if a.users == nil || a.catalog == nil || a.safety == nil || a.state == nil || a.personalize == nil || a.favorites == nil || a.playlists == nil || a.db == nil {
		return nil, errors.New("experience: dependencies unavailable")
	}
	libs := a.configuredLibraries()
	filter := func(ctx context.Context, ts []core.Title) []core.Title {
		if a.availability == nil {
			return ts
		}
		return a.availability.FilterDiscovery(ctx, ts)
	}
	sessions := func() []*core.PlaySession {
		if a.playback == nil {
			return nil
		}
		return a.playback.Sessions()
	}
	// Home, library rows, and card user-data read the mirror. Refreshing a
	// show from a provider stays on the single-title detail and playback path.
	vis := experienceVisibility(a.safety, a.catalog.CachedGet, filter, sessions, libs)
	vis.JudgedOnFacts = func(ctx context.Context) func(core.Title) bool {
		if a.availability == nil {
			return nil
		}
		return a.availability.JudgedOnFacts(ctx)
	}
	rowVis := vis
	source := experience.CandidateRowSource{Kind: a.personalize.SemanticRowKind, Load: func(ctx context.Context, req frontend.RowRequest) ([]experience.RankedCandidate, error) {
		return a.productionExperienceRows(ctx, req, libs, rowVis)
	}, Additional: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		return a.experienceAdditionalRows(core.WithLiveApp(ctx, string(protocol)), req, a.playlists, rowVis)
	}}
	entryPolicy := experience.RowEntryPolicy{Visibility: rowVis, Favorite: func(ctx context.Context, u *core.User, subject experience.FavoriteSubject) bool {
		return a.favoriteAllowedStored(ctx, u, favorites.Subject{Kind: favorites.Kind(subject.Kind), ID: subject.ID}, rowVis)
	}, Playlist: func(ctx context.Context, u *core.User, key core.RatingKey) bool {
		_, err := a.playlists.Get(ctx, u.ID, key)
		return err == nil
	}}
	interactions := experience.RowInteractionPolicy{Libraries: libs, Visibility: rowVis, Get: a.catalog.CachedGet, AllowEntry: entryPolicy.Subject, CollectionMembers: func(ctx context.Context, u *core.User, key core.RatingKey, entries []core.NeutralEntryRef) bool {
		return rowViewCollectionMembers(ctx, a.collections, u, key, entries)
	}, Impression: a.personalize.RecordNeutralImpression, DetailOpen: a.personalize.RecordDetailOpen}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, PolicyLease: a.cfg.AcquirePolicy, Active: a.users.PolicyUser, TTL: 30 * time.Minute, MaxPerDevice: 10000, Prefetch: func(ctx context.Context, keys []core.RatingKey) {
		_, _ = a.catalog.CachedGetMany(ctx, keys)
	}, Allowed: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject) bool {
		return interactions.Allowed(ctx, u, subject)
	}, AllowedAction: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) bool {
		return (kind == frontend.RowImpression && subject.Kind == "row" && subject.Impression != nil) || (kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle))
	}, Record: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) error {
		return interactions.Record(ctx, u, subject, kind)
	}})
	if err != nil {
		return nil, err
	}
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: source, Visibility: rowVis, Receipts: receipts, RankedSource: true, HomeFallback: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		return a.experienceHomeFallback(ctx, req, libs, rowVis)
	}, Active: func(ctx context.Context, u *core.User) bool {
		v, e := a.users.ActiveUser(ctx, u.ID)
		return e == nil && v != nil
	}, AllowRow: entryPolicy.Row, AllowEntry: entryPolicy.Entry, Layout: func(ctx context.Context, req frontend.RowRequest, rows []frontend.Row) ([]frontend.Row, error) {
		sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return nil, errors.New("experience: missing situation")
		}
		return experience.LayoutRows(ctx, a.personalize, libs, req, sit, rows)
	}, Situation: func(ctx context.Context, req frontend.RowRequest) (core.HomeSituation, error) {
		v, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return core.HomeSituation{}, errors.New("experience: missing situation")
		}
		return v, nil
	}})
	if err != nil {
		return nil, err
	}
	rowViews, err := a.newRowViews(protocol, rows, receipts, vis, libs)
	if err != nil {
		return nil, err
	}
	deps := experience.ServiceDeps{BeginPeopleRead: a.beginPeopleRead, NotInterestedSource: func(ctx context.Context, u *core.User) ([]frontend.NotInterestedEntry, error) {
		entries, _, err := a.notInterested.Snapshot(ctx, u)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.NotInterestedEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, frontend.NotInterestedEntry{Key: entry.Key, AddedAt: entry.AddedAt})
		}
		return out, nil
	}, ByExternalID: a.catalog.ByExternalID, Active: a.users.PolicyUser, Libraries: libs, LiveLibraries: a.configuredLibraries, Visibility: vis, State: a.state, Rows: rows, Protocol: protocol, Browse: a.catalog.Browse, PageExclusions: newLibraryPageExclusions(a), ViewerRules: a.catalog.ViewerRulesJudge, FacetFilter: a.catalog.FacetFilter, FacetPage: a.catalog.FacetPage, StudioByFacetID: a.catalog.StudioByFacetID, PersonByFacetID: a.catalog.PersonByFacetID, CountWatched: a.catalog.CountWatched, CountBrowsable: a.catalog.CountBrowsable, CreditItems: a.catalog.CachedCreditItems, PeopleIndex: a.peopleIndex, PersonCredits: a.catalog.PersonCreditKeys, PersonCreditsMany: a.catalog.PersonCreditsMany, RankingDetails: a.catalog.RankingDetails, PersonCreditForms: a.catalog.PersonCreditForms, PersonFormKeys: a.catalog.PersonFormKeys, AiredEpisodes: a.catalog.CachedAiredEpisodes, AiredEpisodesMany: a.catalog.CachedAiredEpisodesMany, AiredEpisodeTallies: a.catalog.AiredEpisodeTallies, EpisodesSettledBefore: func(ctx context.Context) time.Time {
		if a.availability == nil {
			return time.Time{}
		}
		return a.availability.EpisodesSettledBefore(ctx)
	}, StoredEpisodes: experience.StoredEpisodeDeps{Page: a.catalog.StoredEpisodesByKey, Count: a.catalog.CountStoredEpisodes, Unranked: a.catalog.EpisodesUnranked, Customized: func(ctx context.Context, u *core.User) (bool, error) {
		if a.episodeorder == nil {
			return false, nil
		}
		return a.episodeorder.Customized(ctx, u)
	}}, AddedTimes: a.catalog.CachedAddedTimes, RecentAir: func(ctx context.Context, typ core.MediaType, offset, limit int) ([]core.Title, bool, error) {
		f := core.Filter{Type: typ}
		if a.availability != nil {
			var err error
			f, err = a.availability.PagingFilter(ctx, f)
			if err != nil {
				return nil, false, err
			}
		}
		return a.catalog.RecentAirFiltered(ctx, typ, f, offset, limit)
	}, CountUnplayedAired: a.catalog.CountUnplayedAired, SimilarSource: a.catalog.Similar, FilterRecommendations: a.personalize.FilterRecommendations, Collections: a.collections, Playlists: a.playlists, Scheduled: a.scheduledEpisodes, Grouped: a.groupedShows, Search: a.experienceSearch(vis), CachedGet: a.catalog.CachedGet, CachedGetMany: a.catalog.CachedGetMany, ArtworkGet: a.catalog.ArtworkTitle, ArtworkGetMany: a.catalog.ArtworkTitles, ArtworkRows: a.catalog.ArtworkRows, ParentTitles: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return a.catalog.ResolveShowParents(ctx, keys)
	}, Get: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Get(ctx, u, k)
		}
		return a.catalog.Get(ctx, k)
	}, CachedVirtualSeason: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder == nil {
			return nil, core.ErrNotFound
		}
		return a.episodeorder.CachedVirtualSeason(ctx, u, k)
	}, Children: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Children(ctx, u, k)
		}
		return a.catalog.Children(ctx, k)
	}, Grandchildren: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Grandchildren(ctx, u, k)
		}
		return a.catalog.Grandchildren(ctx, k)
	}, SeasonEpisodes: func(ctx context.Context, u *core.User, show core.RatingKey, season, offset, limit int) (core.Page, bool, error) {
		if a.episodeorder != nil {
			pref, err := a.episodeorder.Preference(ctx, u.ID, show)
			if err != nil {
				return core.Page{}, false, err
			}
			if pref.GroupID != "" {
				return core.Page{}, false, nil
			}
			if season == 0 && pref.Specials == "hidden" {
				return core.Page{Items: []core.Title{}}, true, nil
			}
		}
		page, err := a.catalog.SeasonEpisodePage(ctx, show, season, offset, limit)
		return page, true, err
	}, Enrich: func(ctx context.Context, u *core.User, device core.Device, title *core.Title) (frontend.ItemDetail, error) {
		result := frontend.ItemDetail{Title: *title}
		if title.PosterPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "poster", Path: title.PosterPath})
		}
		if title.BackdropPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "backdrop", Path: title.BackdropPath})
		}
		var overrides []core.ClientCapabilityOverride
		if a.cfg != nil {
			overrides = a.cfg.Read().Clients.Overrides
		}
		// surfaceExperience.Item replaces this list with the shared artwork
		// selection. That selection is the composited poster when overlays are on.
		var (
			resumedInfo    core.MediaInfo
			resumedRelease string
			hasResumed     bool
		)
		if a.playback != nil {
			caps := core.ClientCaps{Protocol: protocol, Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			if protocol != "plex" {
				caps = playbackFloor(caps, overrides)
			}
			if a.cfg != nil {
				caps = resolver.ApplyClientRulesForTitle(a.cfg.Read().Quality, caps, u, title)
			}
			resumedInfo, resumedRelease, hasResumed = a.playback.ResumedTracks(ctx, u, title.Key, caps)
			info := resumedInfo
			if !hasResumed {
				info = a.playback.Estimate(ctx, title, caps)
			}
			markerInfo := experience.MarkerMedia(u, device, title.Key, info, a.playback.SessionFor)
			if a.markers != nil {
				result.Markers = experience.ItemMarkers(ctx, a.markers, title.Key, markerInfo)
			} else {
				result.Markers = experience.ItemMarkers(ctx, nil, title.Key, markerInfo)
			}
			if a.subtitles != nil {
				info.Streams = append(info.Streams, (plexSubtitles{a}).CachedSidecars(ctx, title, u, info.Streams)...)
			}
			result.Tracks = info.Streams
			result.TracksEstimated = info.Estimated
		}
		if a.resolver != nil {
			caps := core.ClientCaps{Protocol: protocol, Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			if protocol != "plex" {
				caps = playbackFloor(caps, overrides)
			}
			if a.cfg != nil {
				caps = resolver.ApplyClientRulesForTitle(a.cfg.Read().Quality, caps, u, title)
			}
			versions, err := scorecardVariants(ctx, a, u, title, caps)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, version := range versions {
				if hasResumed && (version.ID == resumedRelease || (version.Auto && resumedRelease != "")) {
					merged := core.CloneMediaInfo(resumedInfo)
					merged.Estimated = false
					if len(version.Info.Chapters) > 0 && len(merged.Chapters) == 0 {
						merged.Chapters = version.Info.Chapters
					}
					version.Info = merged
				}
				result.Versions = append(result.Versions, frontendVersion(title, version))
			}
			var markerSource experience.MarkerSource
			if a.markers != nil {
				markerSource = a.markers
			}
			for n := range result.Versions {
				result.Versions[n].Markers = experience.ItemMarkers(ctx, markerSource, title.Key, result.Versions[n].Media)
			}
		}
		if a.extras != nil {
			clips, err := a.extras.List(ctx, title, device.Language)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, clip := range clips {
				result.Extras = append(result.Extras, frontend.Extra{Key: clip.Key, ParentKey: clip.ParentKey, Title: clip.Title, Kind: clip.Subtype, Source: clip.Site, SourceID: clip.SiteKey, Language: clip.Language, Official: clip.Official, PublishedAt: clip.PublishedAt, Duration: clip.Duration, Height: clip.Size})
			}
		}
		return result, nil
	}, Favorites: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) (bool, error) {
		return a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()}, on)
	}, IsFavorite: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()})
	}, IsFavoriteMany: func(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		return a.favorites.HasTitles(ctx, u, keys)
	}, FavoriteTitleKeys: func(ctx context.Context, u *core.User, n int) ([]core.RatingKey, bool, error) {
		return a.favorites.TitleKeys(ctx, u, n)
	}, IsNotInterestedMany: func(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		return a.notInterested.HasMany(ctx, u, keys)
	}, IsWatchlistedMany: func(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		if a.watchlist == nil {
			return map[core.RatingKey]bool{}, nil
		}
		return a.watchlist.ContainsMany(ctx, userID, keys)
	}, FavoriteMembership: a.favoriteMembership, IsNotInterested: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.notInterested.Has(ctx, u, k)
	}, SetNotInterestedSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		_, err := a.notInterested.Set(ctx, u, k, on)
		return err
	}, IsWatchlisted: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		if a.watchlist == nil {
			return false, core.ErrNotAvailable
		}
		return a.watchlist.Contains(ctx, u.ID, k)
	}, WatchlistMembership: a.watchlistMembership, SetWatchlistSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		if a.watchlist == nil {
			return core.ErrNotAvailable
		}
		var err error
		if on {
			err = a.watchlist.Remind(ctx, u, k)
		} else {
			err = a.watchlist.Remove(ctx, u.ID, k)
		}
		if err == nil && a.sessionEvents != nil {
			if e := a.sessionEvents.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: u.ID, Item: k, OriginProtocol: protocol}); e != nil && a.log != nil {
				a.log.Warn("watchlist event publication failed", "err", e)
			}
		}
		return err
	}, RemoveContinue: func(ctx context.Context, u *core.User, k core.RatingKey) error {
		if a.discoveryPersonal != nil {
			if err := a.discoveryPersonal.RemoveContinue(ctx, u.ID, k); err != nil {
				return err
			}
		}
		return a.state.Progress(ctx, u.ID, k, 0, 0, "stopped")
	}, CanFavoriteEntity: func(ctx context.Context, u *core.User, r frontend.FavoriteRef) bool {
		return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, vis)
	}, SetEntityFavoriteSource: func(ctx context.Context, u *core.User, r frontend.FavoriteRef, on bool) error {
		_, err := a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, on)
		return err
	}, EntityFavoritesSource: func(ctx context.Context, u *core.User, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
		out := make(map[frontend.FavoriteRef]bool, len(refs))
		for _, r := range refs {
			yes, err := a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID})
			if err != nil {
				return nil, err
			}
			out[r] = yes
		}
		return out, nil
	}}
	if a.recommend != nil {
		deps.Related = a.recommend
	}
	cat := core.Catalog(a.catalog)
	if a.availability != nil {
		cat = &availabilityCatalog{Catalog: a.catalog, availability: a.availability}
		deps.FilterAvailable = a.availability.FilterDiscovery
		deps.JudgedOnFacts = a.availability.JudgedOnFacts
	}
	deps.Catalog = cat
	if a.discoveryPersonal != nil {
		deps.Collections = a.discoveryPersonal.ReviewCollections(a.collections)
	}
	deps.Recommender = a.personalize
	deps.VibeSearch = a.collections
	deps.SearchSources = a.searchSources()
	deps.FavoriteShows = a.favoriteShows
	deps.PrioritizeAvailable = func(ctx context.Context, items []core.Title) []core.Title {
		if a.availability == nil {
			return items
		}
		return a.availability.Prioritize(ctx, items)
	}
	deps.EpisodeOrder = a.episodeorder
	if a.discoveryFeatures != nil {
		deps.Discovery = a.discoveryFeatures.Frontend()
	}
	if a.stats != nil {
		deps.Dashboard = a.stats.Dashboard()
	}
	if a.hostResources != nil {
		deps.Resources = a.hostResources
	}
	deps.LibraryTitles = a.libraryTitles
	deps.Now = parityClock()
	if a.db != nil {
		db := a.db
		deps.HomeRevision = func(ctx context.Context, u *core.User) (frontend.HomeRevision, error) {
			return experience.ReadHomeRevision(ctx, db, u)
		}
		deps.NoProviderWait = func(ctx context.Context) context.Context {
			return catalog.WithReadWaitBudget(ctx, 0)
		}
	}
	service, err := experience.NewService(deps)
	if err != nil {
		return nil, err
	}
	var artworkSource frontend.Artwork
	if len(artwork) > 0 {
		artworkSource = artwork[0]
	}
	detail := detailPersonalization{now: time.Now, ratings: a.ratings}
	if a.personalize != nil {
		detail.micro = a.personalize.MicroReason
	}
	if a.cfg != nil {
		detail.enabled = func() bool { return a.cfg.Read().Personalize.MicroReasons }
	}
	return &surfaceExperience{Service: service, receipts: receipts, protocol: protocol, artwork: artworkSource, active: a.users.ActiveUser, rowViews: rowViews, detail: detail, generation: func(ctx context.Context) (int64, error) {
		return viewerpolicy.Read(ctx, a.db)
	}, catalogGen: func() uint64 {
		if a.catalog == nil {
			return 0
		}
		return a.catalog.WriteGeneration()
	}, posters: func(ctx context.Context, typ core.MediaType, limit int) ([]core.Title, error) {
		if a.catalog == nil {
			return nil, core.ErrNotAvailable
		}
		return a.catalog.LibraryPosters(ctx, typ, limit)
	}, modelReady: func(id int64) bool {
		return a.personalize != nil && a.personalize.ModelReady(id)
	}, publicUsers: a.users.Users, publicEpoch: func() uint64 {
		// The policy epoch: any change to accounts, owner content
		// policy (safety/config) or the catalogue drops cached public
		// artwork verdicts.
		var gen uint64
		if a.users != nil {
			gen = a.users.Generation()
		}
		if a.safety != nil {
			gen = gen*1099511628211 ^ a.safety.Generation()
		}
		if a.cfg != nil {
			gen = gen*1099511628211 ^ a.cfg.Generation()
		}
		if a.catalog != nil {
			gen = gen*1099511628211 ^ a.catalog.WriteGeneration()
		}
		return gen
	}}, nil
}

type surfaceExperience struct {
	*experience.Service
	receipts    *experience.Receipts
	protocol    core.Protocol
	artwork     frontend.Artwork
	active      func(context.Context, int64) (*core.User, error)
	rowViews    *experience.RowViewService
	playability func(context.Context, frontend.ItemRequest) *bool
	detail      detailPersonalization
	generation  func(context.Context) (int64, error)
	catalogGen  func() uint64
	posters     func(context.Context, core.MediaType, int) ([]core.Title, error)
	modelReady  func(int64) bool
	rowMu       sync.Mutex
	rowCache    map[string]rowCacheSlot
	collageMu   sync.Mutex
	collage     map[string]collageSlot
	// Public (unauthenticated) title artwork: the active-user snapshot
	// and per-item intersection verdicts, cached per policy epoch.
	publicUsers func(context.Context) ([]core.User, error)
	publicEpoch func() uint64
	publicMu    sync.Mutex
	publicCache publicArtworkCache
}

type collageSlot struct {
	gen   uint64
	img   frontend.Image
	ready bool
	busy  bool
}

// PolicyGeneration is the viewer-policy epoch. Personal-view caches drop
// when it changes.
func (s *surfaceExperience) PolicyGeneration(ctx context.Context) (int64, error) {
	if s.generation == nil {
		return 0, nil
	}
	return s.generation(ctx)
}

func (s *surfaceExperience) ListRowViews(ctx context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return nil, core.ErrUnauthorized
	}
	return s.rowViews.ListRowViews(ctx, req)
}

func (s *surfaceExperience) ResolveRowView(ctx context.Context, req frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return frontend.RowViewPage{}, core.ErrUnauthorized
	}
	return s.rowViews.ResolveRowView(ctx, req)
}

func (s *surfaceExperience) LibraryCollage(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, viewID string) (frontend.Image, error) {
	if s == nil || s.artwork == nil || viewer == nil || viewID == "" || s.posters == nil {
		return frontend.Image{}, core.ErrNotAvailable
	}
	builder, ok := s.artwork.(interface {
		LibraryCollage(context.Context, *core.Viewer, core.Device, string, []core.Title) (frontend.Image, error)
	})
	if !ok {
		return frontend.Image{}, core.ErrNotAvailable
	}
	typ, ok := s.libraryMediaType(ctx, viewer, viewID)
	if !ok {
		return frontend.Image{}, core.ErrNotFound
	}
	var gen uint64
	if s.catalogGen != nil {
		gen = s.catalogGen()
	}
	if img, fresh := s.collageImage(viewID, gen); fresh {
		return img, nil
	} else if img.Path != "" {
		s.startCollage(viewID, gen, typ, *viewer, device, builder)
		return img, nil
	}
	s.startCollage(viewID, gen, typ, *viewer, device, builder)
	return frontend.Image{}, core.ErrNotFound
}

func (s *surfaceExperience) libraryMediaType(ctx context.Context, viewer *frontend.Viewer, viewID string) (core.MediaType, bool) {
	if s == nil || s.Service == nil {
		return "", false
	}
	n, err := ids.ParseGUID(ids.View, viewID)
	if err != nil {
		return "", false
	}
	want := strconv.FormatInt(n, 10)
	views, err := s.Views(ctx, viewer)
	if err != nil {
		return "", false
	}
	for _, view := range views {
		if view.ID == want {
			return view.Type, true
		}
	}
	return "", false
}

func (s *surfaceExperience) collageImage(viewID string, gen uint64) (frontend.Image, bool) {
	s.collageMu.Lock()
	defer s.collageMu.Unlock()
	slot, ok := s.collage[viewID]
	if !ok || !slot.ready {
		return frontend.Image{}, false
	}
	return slot.img, slot.gen == gen
}

func (s *surfaceExperience) startCollage(viewID string, gen uint64, typ core.MediaType, viewer core.Viewer, device frontend.Device, builder interface {
	LibraryCollage(context.Context, *core.Viewer, core.Device, string, []core.Title) (frontend.Image, error)
}) {
	s.collageMu.Lock()
	if s.collage == nil {
		s.collage = map[string]collageSlot{}
	}
	slot := s.collage[viewID]
	if slot.busy || (slot.ready && slot.gen == gen) {
		s.collageMu.Unlock()
		return
	}
	slot.busy = true
	s.collage[viewID] = slot
	s.collageMu.Unlock()
	go func() {
		defer func() {
			s.collageMu.Lock()
			slot := s.collage[viewID]
			slot.busy = false
			s.collage[viewID] = slot
			s.collageMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		titles, err := s.posters(ctx, typ, 16)
		if err != nil {
			return
		}
		img, err := builder.LibraryCollage(ctx, &viewer, device, viewID, titles)
		if err != nil || img.Path == "" {
			return
		}
		img.Subject = viewID
		s.collageMu.Lock()
		s.collage[viewID] = collageSlot{gen: gen, img: img, ready: true}
		s.collageMu.Unlock()
	}()
}

func (s *surfaceExperience) ItemArtworkPage(ctx context.Context, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Image, error) {
	titles, err := s.PageArtworkTitles(ctx, viewer, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[core.RatingKey][]frontend.Image, len(titles))
	if s.artwork == nil {
		return out, nil
	}
	if page, ok := s.artwork.(frontend.PageArtwork); ok {
		return page.ListPage(ctx, viewer, device, titles)
	}
	for i := range titles {
		images, _, err := s.artwork.List(frontend.WithArtworkTitle(ctx, &titles[i]), frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: titles[i].Key})
		if err != nil {
			return nil, err
		}
		out[titles[i].Key] = images
	}
	return out, nil
}

func (s *surfaceExperience) ItemArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	active, err := s.Service.AuthorizeArtwork(ctx, viewer, device, key)
	if err != nil {
		return nil, false, err
	}
	if s.artwork == nil {
		return nil, false, core.ErrNotAvailable
	}
	return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
}

func (s *surfaceExperience) RelatedArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (frontend.RelatedArtwork, error) {
	service, err := experience.NewRelatedArtworkService(experience.RelatedArtworkDeps{Authorize: s.Service.AuthorizeArtwork, List: func(ctx context.Context, active *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
		if s.artwork == nil {
			return nil, false, core.ErrNotAvailable
		}
		return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
	}})
	if err != nil {
		return frontend.RelatedArtwork{}, err
	}
	return service.RelatedArtwork(ctx, viewer, device, key)
}

func (s *surfaceExperience) Item(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	detail, err := s.Service.Item(ctx, req)
	if err == nil && detail.Title.Key != 0 && s.playability != nil {
		if detail.Title.Type == core.TypeShow || detail.Title.Type == core.TypeSeason {
			playable := false
			detail.CanPlay = &playable
		} else {
			detail.CanPlay = s.playability(ctx, req)
		}
	}
	if err != nil || s.artwork == nil {
		return detail, err
	}
	// The service has already authorized the item. Resolve the active user
	// again before passing it to the artwork policy; do not trust caller fields.
	detail.Images = nil
	detail.ImagesAuthoritative = false
	if req.Viewer == nil {
		return detail, nil
	}
	viewer, err := s.active(ctx, req.Viewer.ID)
	if err != nil || viewer == nil {
		return detail, nil
	}
	images, complete, err := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: req.Device, Item: req.Key})
	if err == nil {
		detail.Images, detail.ImagesAuthoritative = images, complete
	}
	return detail, nil
}

type rowCacheSlot struct {
	gen   uint64
	at    time.Time
	rows  []frontend.Row
	ready bool
	busy  bool
}

const homeRowCacheTTL = 15 * time.Second

func (s *surfaceExperience) SnapshotRows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	return s.cachedRows(ctx, req, false)
}

func (s *surfaceExperience) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	return s.cachedRows(ctx, req, true)
}

func (s *surfaceExperience) cachedRows(ctx context.Context, req frontend.RowRequest, issue bool) ([]frontend.Row, error) {
	// Issued rows mint receipts. A cached success would skip that transaction,
	// so a later bind failure could not roll back and the client would keep
	// handles whose receipts were never written. Snapshots are safe to reuse.
	if issue || (req.Placement != frontend.PlacementHome && req.Placement != frontend.PlacementLibrary) {
		return s.loadRows(ctx, req, issue)
	}
	key, gen := s.rowCacheKey(req)
	s.rowMu.Lock()
	slot := s.rowCache[key]
	fresh := slot.ready && slot.gen == gen && time.Since(slot.at) < homeRowCacheTTL
	if fresh || (slot.ready && slot.gen == gen && slot.busy) {
		rows := cloneHomeRows(slot.rows)
		s.rowMu.Unlock()
		return rows, nil
	}
	stale := slot.ready && slot.gen == gen
	if stale {
		slot.busy = true
		s.rowCache[key] = slot
		rows := cloneHomeRows(slot.rows)
		s.rowMu.Unlock()
		go s.refreshRows(key, gen, req, issue)
		return rows, nil
	}
	s.rowMu.Unlock()
	rows, err := s.loadRows(ctx, req, issue)
	if err != nil {
		return nil, err
	}
	s.storeRows(key, gen, rows)
	return rows, nil
}

func (s *surfaceExperience) refreshRows(key string, gen uint64, req frontend.RowRequest, issue bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		s.rowMu.Lock()
		slot := s.rowCache[key]
		slot.busy = false
		s.rowCache[key] = slot
		s.rowMu.Unlock()
	}()
	rows, err := s.loadRows(ctx, req, issue)
	if err != nil {
		return
	}
	s.storeRows(key, gen, rows)
}

func (s *surfaceExperience) storeRows(key string, gen uint64, rows []frontend.Row) {
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	if s.rowCache == nil {
		s.rowCache = map[string]rowCacheSlot{}
	}
	s.rowCache[key] = rowCacheSlot{gen: gen, at: time.Now(), rows: cloneHomeRows(rows), ready: true}
}

func (s *surfaceExperience) rowCacheKey(req frontend.RowRequest) (string, uint64) {
	viewer := int64(0)
	ready := false
	if req.Viewer != nil {
		viewer = req.Viewer.ID
		if s.modelReady != nil {
			ready = s.modelReady(viewer)
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	key := fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%s\n%d\n%t", s.protocol, viewer, req.Device.ID, req.Placement, req.ViewID, req.Purpose, limit, ready)
	// Home stays cached for the TTL. Catalogue write generation is not part of
	// the key: a background title refresh bumps it on every provider hydrate
	// and was forcing the next Home to rebuild.
	return key, 0
}

func cloneHomeRows(in []frontend.Row) []frontend.Row {
	out := make([]frontend.Row, len(in))
	for i, row := range in {
		out[i] = row
		out[i].Entries = cloneRowEntries(row.Entries)
		if row.Items != nil {
			out[i].Items = append([]core.Title(nil), row.Items...)
		}
	}
	return out
}

func cloneRowEntries(in []frontend.RowEntry) []frontend.RowEntry {
	if in == nil {
		return nil
	}
	out := make([]frontend.RowEntry, len(in))
	for i, entry := range in {
		out[i] = entry
		if entry.Title != nil {
			title := *entry.Title
			out[i].Title = &title
		}
	}
	return out
}

func (s *surfaceExperience) loadRows(ctx context.Context, req frontend.RowRequest, issue bool) ([]frontend.Row, error) {
	ctx = availability.WithDiscoveryMemo(livetv.WithChannelMemo(context.WithValue(ctx, experienceSituationKey{}, situationFor(req))))
	var rows []frontend.Row
	var err error
	if issue {
		rows, err = s.Service.Rows(ctx, req)
	} else {
		rows, err = s.Service.SnapshotRows(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	var active *core.User
	if req.Viewer != nil {
		active, _ = s.active(ctx, req.Viewer.ID)
	}
	for _, row := range rows {
		s.warmArtwork(row.Items, req.Device)
		for _, entry := range row.Entries {
			if entry.Title != nil {
				s.warmArtwork([]core.Title{*entry.Title}, req.Device)
			}
			if channel := entry.LiveChannel; channel != nil {
				// An upstream logo may carry provider credentials. Only return
				// a policy-scoped descriptor to non-Plex front ends.
				channel.Logo = ""
				if active != nil && s.artwork != nil {
					images, complete, e := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: req.Device, Channel: channel.ID})
					if e == nil && complete {
						channel.Images = images
					}
				}
			}
		}
	}
	return rows, nil
}

func (s *surfaceExperience) warmArtwork(items []core.Title, device core.Device) {
	warmer, ok := s.artwork.(interface{ Warm(core.Title, core.Device) })
	if !ok {
		return
	}
	for _, item := range items {
		warmer.Warm(item, device)
	}
}

func (s *surfaceExperience) Children(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.Children(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) LibraryItems(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.LibraryItems(ctx, viewer, device, viewID, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, options frontend.ItemQuery) (frontend.SearchPage, error) {
	if s.protocol == "stremio" {
		ctx = core.WithSearchSummaryFacts(ctx)
	}
	page, err := s.Service.Search(ctx, viewer, device, query, options)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Similar(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (frontend.SearchPage, error) {
	page, err := s.Service.Similar(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}
func (s *surfaceExperience) ReportRowInteraction(ctx context.Context, i frontend.RowInteraction) error {
	if i.Protocol != s.protocol {
		return experience.ErrReceipt
	}
	return s.receipts.Report(ctx, i)
}
