// Package plex is Ether's Plex Media Server front end: the HTTP API that
// official Plex apps (Web, iOS, Apple TV, Android, TVs) talk to. Everything
// behind it is reached through internal/frontend host interfaces.
package plex

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// DefaultPMSVersion is reported when Identity.Version is empty. Clients gate
// features on the server version, so it must look like a current PMS build.
const DefaultPMSVersion = "1.43.4.10903-e5521bd8c"

// Identity is how the server presents itself. Empty fields fall back to the
// config snapshot (friendly name, machine identifier) or sensible defaults.
type Identity = frontend.PlexIdentity

// Hooks are optional callbacks into other components. They run on their own
// goroutine and must be safe for concurrent use.
type Hooks = frontend.PlexHooks

// Section is a library section exposed to clients.
type Section = frontend.PlexSection

// SessionToucher is optionally implemented by frontend.Playback to keep a play
// session alive on timeline updates.
type SessionToucher interface {
	Touch(sessionID string)
}

// Deps are the server's dependencies. Catalog, State, Auth and Playback are
// required; the rest degrade gracefully when nil.
type Deps = frontend.PlexDeps

// Server is the PMS API handler. It is an http.Handler; call Close on
// shutdown to stop background work and notification streams.
type Server struct {
	activityMu       sync.Mutex
	activities       map[string]model.Activity
	shelfMu          sync.Mutex
	shelfCuts        map[string]shelfCut
	shelfGenerations map[string]shelfGeneration
	shelfBytes       int
	shelfSerial      uint64
	shelfWriter      *shelfPersistence
	plexEncoderMu    sync.RWMutex
	plexEncoder      PlexEncoder
	homeSharedMu     sync.Mutex
	homeShared       map[string]*homeSharedEntry
	shelfRefMu       sync.Mutex
	shelfRefs        map[string]shelfRef
	shelfRefRebuilt  time.Time
	shelfRebuild     chan struct{}
	guidResolveMu    sync.Mutex
	liveLogoKeyOnce  sync.Once
	liveLogoKey      []byte
	guidResolving    map[string]bool
	home             *homeCache

	d             Deps
	experience    frontend.Experience
	personalHomes frontend.PersonalHomeStore
	pageFreshness func() frontend.PageFreshness
	log           *slog.Logger
	router        chi.Router

	sections []Section

	images       frontend.ArtworkImageCache
	channelLogos interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	channelRaster     frontend.SVGRasterizer
	boundStreams      frontend.PlexStreamAPI
	variantIDs        *variantIDRegistry
	playIDs           *playIdentities
	versionFlows      *versionFlowStore
	variantWatch      variantWatches
	downloads         frontend.DownloadQueueStore
	queues            frontend.PlayQueues
	sessions          *sessionActivities
	playbackBroker    frontend.Sessions
	playbackOwners    frontend.PlaybackOwners
	playbackLifecycle frontend.Lifecycle
	transcodes        *transcodeTable
	progressiveStops  progressiveStopGuard
	readCaps          *readCapabilityStore
	extraPlay         *extraPlayStore
	subtitleOffers    subtitleOffers
	subtitlePicks     subtitlePicks
	selections        *selectionStore
	notifier          *notifier
	facets            *facetCache
	cl                *clusterState // nil on a standalone server

	// Background enrichment (nil when disabled).
	titles    *titleCache
	ultra     frontend.ArtworkPaletteCache
	titleJobs *bgQueue
	blurJobs  *bgQueue

	// Owner stream kills, delivered on the client's next timeline; the
	// janitor drops those whose client never reports again.
	termMu       sync.Mutex
	terms        map[string]termination
	decisionArms frontend.DecisionArms

	// Stalled-start watches and Dolby Vision base-layer fallbacks.
	starts frontend.StartGuard

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// NewServer builds the PMS API handler.
func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Config == nil {
		d.Config = func() frontend.HostConfig { return frontend.HostConfig{} }
	}
	if d.ImageURL == nil {
		d.ImageURL = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if d.ImageCacheBytes <= 0 {
		d.ImageCacheBytes = 128 << 20
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	if d.SessionStores == nil {
		panic("plex: Deps.SessionStores is required (wire sessions.NewPlexSessionStores())")
	}
	if d.ArtworkStores == nil {
		panic("plex: Deps.ArtworkStores is required (wire artworkservice.NewPlexArtworkStores())")
	}
	coord := d.SessionCoordination
	if coord == nil || coord.PlaybackOwners() == nil || coord.LifecycleGates() == nil {
		coord = d.SessionStores.NewCoordination()
	}
	s := &Server{
		activities:        map[string]model.Activity{},
		plexEncoder:       d.PlexEncoder,
		boundStreams:      d.PlexStreams,
		d:                 d,
		log:               d.Logger.With("component", "plex"),
		images:            d.ArtworkStores.NewRemoteCache(d.ImageCacheBytes, d.HTTPClient),
		channelLogos:      d.ArtworkStores.NewUntrustedImages(d.HTTPClient),
		channelRaster:     d.ArtworkStores.NewSVGRasterizer(),
		variantIDs:        newVariantIDRegistry(d.SessionStores, d.DB),
		playIDs:           newPlayIdentities(),
		downloads:         d.SessionStores.NewDownloadQueueStore(),
		queues:            d.SessionStores.NewPlayQueues(),
		sessions:          newSessionActivities(),
		playbackOwners:    coord.PlaybackOwners(),
		playbackLifecycle: coord.LifecycleGates(),
		transcodes:        newTranscodeTable(),
		readCaps:          newReadCapabilityStore(),
		extraPlay:         newExtraPlayStore(d.DirectHTTP),
		selections:        newSelectionStore(),
		notifier:          newNotifier(),
		facets:            newFacetCache(),
		ctx:               ctx,
		cancel:            cancel,
	}
	if d.ArtworkImages != nil {
		s.images = d.ArtworkImages
	}
	s.shelfWriter = &shelfPersistence{pending: map[string]shelfWrite{}, active: map[string]bool{}, wake: make(chan struct{}, 1)}
	s.wg.Add(1)
	go s.writeShelves()
	s.variantIDs.start(ctx, &s.wg)
	s.transcodes.load = s.loadDistributedBinding
	s.sections = s.buildSections()
	s.queues.Configure(d.Now, nil)
	if d.DB != nil && d.Cluster == nil {
		// Standalone queues share the durable store without starting cluster sessions.
		s.queues.Configure(d.Now, newClusterState(s, d.DB, frontend.StandaloneClusterNode("standalone-queues")).queuePersistence())
	}
	if d.Cluster != nil && d.DB != nil {
		s.cl = newClusterState(s, d.DB, d.Cluster)
		s.versionFlows = newVersionFlowStore(s, d.DB)
		s.queues.Configure(d.Now, s.cl.queuePersistence())
	}
	if d.ArtworkPalettes != nil {
		s.ultra = d.ArtworkPalettes
	}
	node := "standalone"
	if s.cl != nil {
		node = s.cl.self
	}
	s.starts = d.SessionStores.NewStartGuard()
	s.decisionArms = d.SessionStores.NewDecisionArms(s.ctx, d.DB, s.now, node, s.log, &s.wg)
	s.clearOwnDecisionArms()
	s.loadDownloads()
	s.wg.Add(1)
	go s.writeDownloads(s.downloadPersistence())
	s.scheduleDownloads()
	s.router = s.routes()
	s.wg.Add(1)
	go s.janitor()
	if s.cl != nil {
		s.cl.start()
	}
	return s
}

func (s *Server) attachHost(host frontend.Host) {
	if s == nil {
		return
	}
	if host.PersonalHomes != nil {
		s.personalHomes = host.PersonalHomes
	}
	if host.PageFreshness != nil {
		s.pageFreshness = host.PageFreshness
	}
	if host.Experience != nil {
		s.attachExperience(host.Experience)
	}
}

func (s *Server) attachExperience(exp frontend.Experience) {
	if s == nil || exp == nil {
		return
	}
	s.experience = exp
	cat := s.catalog()
	if !s.d.DisableEnrichment && cat != nil && s.titles == nil {
		s.titleJobs = newBGQueue(s.ctx, &s.wg, titleFetchWorkers, titleFetchQueueLen)
		s.blurJobs = newBGQueue(s.ctx, &s.wg, ultraBlurWorkers, ultraBlurQueueLen)
		s.titles = newTitleCache(s.titleJobs, s.now, s.log, s.enrichFetch)
		if s.ultra == nil {
			s.ultra = s.d.ArtworkStores.NewPaletteCache(s.d.DB, s.blurJobs.submit, s.now, s.log, func(ctx context.Context, key frontend.RatingKey, path string) (frontend.ArtworkPalette, error) {
				var art frontend.ArtworkCollectionArt
				if c, ok := s.collections().(frontend.ArtworkCollectionArt); ok {
					art = c
				}
				return s.d.ArtworkStores.ComputeItemPalette(ctx, s.images, s.d.ImageURL, key, path, s.now(), art)
			})
		}
	}
	if s.titles != nil {
		if pre, ok := cat.(interface {
			PreloadStored(context.Context, []frontend.RatingKey)
		}); ok {
			s.titles.prefetch = pre.PreloadStored
		} else {
			s.titles.prefetch = nil
		}
	}
	if p, ok := cat.(interface {
		PersistsArtwork(context.Context, frontend.RatingKey) bool
	}); ok && s.ultra != nil {
		s.ultra.PersistFor(p.PersistsArtwork)
	}
	if s.home == nil {
		if _, hasRev, _ := exp.HomeRevision(s.ctx, nil); hasRev {
			s.home = newHomeCache()
			s.wg.Add(1)
			go s.homeRefresher()
		}
	}
	s.sections = s.buildSections()
}

func (s *Server) exp() frontend.Experience {
	if s != nil && s.experience != nil {
		return s.experience
	}
	return noopExperience{}
}
func (s *Server) catalog() frontend.Catalog         { return s.exp().CatalogSource() }
func (s *Server) collections() frontend.Collections { return s.exp().CollectionsSource() }
func (s *Server) state() frontend.State             { return s.exp().StateSource() }
func (s *Server) playlists() frontend.Playlists     { return s.exp().PlaylistsSource() }
func (s *Server) recommender() frontend.Recommender { return s.exp().RecommenderSource() }
func (s *Server) vibeSearch() frontend.VibeSearcher { return s.exp().VibeSearchSource() }
func (s *Server) searchSources() frontend.SearchSources {
	return s.exp().SearchSources()
}
func (s *Server) favoriteShows() func(context.Context, *frontend.User) (map[frontend.RatingKey]bool, error) {
	return s.exp().FavoriteShowsFunc()
}
func (s *Server) filterAvailable() func(context.Context, []frontend.Title) []frontend.Title {
	return s.exp().FilterAvailableFunc()
}
func (s *Server) judgedOnFacts() func(context.Context) func(frontend.Title) bool {
	return s.exp().JudgedOnFactsFunc()
}
func (s *Server) prioritizeAvailable() func(context.Context, []frontend.Title) []frontend.Title {
	return s.exp().PrioritizeAvailableFunc()
}
func (s *Server) episodeOrder() frontend.EpisodeOrder       { return s.exp().EpisodeOrderSource() }
func (s *Server) discovery() *frontend.DiscoveryFeatures    { return s.exp().DiscoveryFeatures() }
func (s *Server) dashboard() frontend.Dashboard             { return s.exp().DashboardSource() }
func (s *Server) resources() frontend.ResourceStatistics    { return s.exp().ResourcesSource() }
func (s *Server) libraryTitles() (movies, shows string)     { return s.exp().LibraryTitles() }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

// Closed reports whether Close has begun.
func (s *Server) Closed() bool {
	return s == nil || s.ctx.Err() != nil
}

// Done is closed when the server is closed.
func (s *Server) Done() <-chan struct{} { return s.ctx.Done() }

// Close stops background goroutines and disconnects notification clients.
func (s *Server) Close() error {
	s.once.Do(func() {
		if s.d.DB != nil {
			s.stopDownloadsForRestart()
		} else {
			s.releaseAllDownloads()
		}
		s.cancel()

		s.notifier.close()
		s.wg.Wait()
		s.clearOwnDecisionArms()
		if err := s.saveDownloads(); err != nil {
			s.log.Warn("persist downloads on close", "err", err)
		}
		if s.cl != nil {
			s.cl.close()
		}
	})
	return nil
}

func (s *Server) now() time.Time { return s.d.Now() }

func (s *Server) cfg() frontend.HostConfig { return s.d.Config() }

// identity resolves the effective identity.
func (s *Server) identity() Identity {
	id := s.d.Identity
	if s.d.OwnerSubscription != nil {
		id.MyPlexSubscription, id.OwnerFeatures = s.d.OwnerSubscription()
	}
	c := s.cfg()
	if id.MachineIdentifier == "" {
		id.MachineIdentifier = c.Server.MachineIdentifier
	}
	if id.MachineIdentifier == "" {
		id.MachineIdentifier = "ether"
	}
	if name := strings.TrimSpace(c.Server.FriendlyName); name != "" {
		id.FriendlyName = name
	} else if strings.TrimSpace(id.FriendlyName) == "" {
		id.FriendlyName = "Ether"
	}
	if c.Plex.CompatibilityVersion != "" {
		id.Version = c.Plex.CompatibilityVersion
	} else if c.Plex.TrackLatestVersion && c.Plex.LatestCompatibilityVersion != "" {
		id.Version = c.Plex.LatestCompatibilityVersion
	}
	if id.Version == "" {
		id.Version = DefaultPMSVersion
	}
	if id.Platform == "" {
		switch runtime.GOOS {
		case "darwin":
			id.Platform = "MacOSX"
		case "windows":
			id.Platform = "Windows"
		default:
			id.Platform = "Linux"
		}
	}
	return id
}

// LibrarySections returns the canonical section identities for account
// registration. Titles follow the live config so renames publish to plex.tv.
// The copy prevents publishers from changing local libraries.
func (s *Server) LibrarySections() []Section {
	return s.withLibraryTitles(s.sections)
}

func (s *Server) buildSections() []Section {
	secs := s.d.Sections
	if len(secs) == 0 {
		secs = []Section{
			{ID: 1, Type: frontend.TypeMovie},
			{ID: 2, Type: frontend.TypeShow},
		}
	}
	out := make([]Section, len(secs))
	for i, sec := range secs {
		if sec.Language == "" {
			sec.Language = "en-US"
		}
		if sec.UUID == "" {
			sum := sha1.Sum([]byte(fmt.Sprintf("%s/section/%d", s.identity().MachineIdentifier, sec.ID)))
			h := hex.EncodeToString(sum[:16])
			sec.UUID = h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
		}
		out[i] = sec
	}
	return s.withLibraryTitles(out)
}

// librarySectionTitle is the Movies / TV Shows name Ether advertises to
// Plex, from libraries 1/2 via Deps.LibraryTitles.
func librarySectionTitle(t frontend.MediaType, movies, shows string) string {
	switch t {
	case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
		if n := strings.TrimSpace(shows); n != "" {
			return n
		}
		return "TV Shows"
	default:
		if n := strings.TrimSpace(movies); n != "" {
			return n
		}
		return "Movies"
	}
}

// withLibraryTitles copies secs and applies the library names.
// IDs and UUIDs are not rewritten; UUIDs stay derived from section id only.
// Empty library names already resolve to Movies and TV Shows.
func (s *Server) withLibraryTitles(secs []Section) []Section {
	movies, shows := s.libraryTitles()
	out := append([]Section(nil), secs...)
	for i := range out {
		switch out[i].Type {
		case frontend.TypeMovie, frontend.TypeShow:
			out[i].Title = librarySectionTitle(out[i].Type, movies, shows)
		}
	}
	return out
}

// sectionByID returns the section with the given id.
func (s *Server) sectionByID(id int) (Section, bool) {
	for _, sec := range s.withLibraryTitles(s.sections) {
		if sec.ID == id {
			return sec, true
		}
	}
	return Section{}, false
}

// sectionFor returns the section an item of type t lives in.
func (s *Server) sectionFor(t frontend.MediaType) (Section, bool) {
	sec, ok := s.untitledSectionFor(t)
	if !ok {
		return Section{}, false
	}
	return s.withLibraryTitles([]Section{sec})[0], true
}

// untitledSectionFor is sectionFor without the configured title. A
// visibility check needs only the section id, and runs for every title a
// page shows, so it reads no configuration.
func (s *Server) untitledSectionFor(t frontend.MediaType) (Section, bool) {
	want := frontend.TypeMovie
	switch t {
	case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
		want = frontend.TypeShow
	}
	for _, sec := range s.sections {
		if sec.Type == want {
			return sec, true
		}
	}
	return Section{}, false
}

// userSections lists the sections a user may see.
func (s *Server) userSections(u *frontend.User) []Section {
	secs := s.withLibraryTitles(s.sections)
	if u == nil || len(u.AllowedLibraries) == 0 || u.IsOwner {
		return secs
	}
	var out []Section
	for _, sec := range secs {
		if frontend.SectionAllowed(u, sec.ID) {
			out = append(out, sec)
		}
	}
	return out
}

func (s *Server) canSeeSection(u *frontend.User, id int) bool {
	for _, sec := range s.sections {
		if sec.ID == id {
			return u == nil || len(u.AllowedLibraries) == 0 || u.IsOwner || frontend.SectionAllowed(u, sec.ID)
		}
	}
	return false
}

func (s *Server) expireTranscodes(now time.Time, ttl time.Duration) {
	for _, id := range s.transcodes.expire(now, ttl) {
		s.revokeReadCapability(id)
	}
}

// janitor expires idle play queues, sessions and transcode bindings.
func (s *Server) janitor() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			now := s.now()

			s.expireDownloads(now)
			s.queues.Expire(now, 6*time.Hour)
			s.playIDs.expire(now, s.playIdentityAlive)
			s.expireTranscodes(now, 2*time.Hour)
			s.playbackOwners.Expire(now, 2*time.Hour)
			s.extraPlay.expire(now, 2*time.Hour)
			s.expireTerminations(now, terminationTTL)
			s.refreshDecisionArms(now)
			s.starts.Expire(now)
			expired := s.sessions.expire(now, 3*time.Minute)
			for _, e := range expired {
				s.notifyPlaying(e, "stopped")
				// Timeline silence is a stop, not just a display expiry:
				// end the playback and encoder sessions too (#410).
				s.endExpiredEntry(e)
			}
			if len(expired) > 0 {
				s.cl.poke()
			}
		}
	}
}

// goHook runs fn on its own goroutine, recovering panics.
func (s *Server) goHook(name string, fn func()) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("hook panicked", "hook", name, "panic", v)
			}
		}()
		fn()
	}()
}

func trimSlash(p string) string { return strings.TrimSuffix(p, "/") }

// releaseMountedPlayback stops encoders, trickplay, and downloads this server
// started. The HTTP server itself stays up so an in-flight request can finish
// and a later mount can reuse it.
func (s *Server) releaseMountedPlayback() {
	if s == nil {
		return
	}
	s.UsePlaybackBroker(nil)
	var ids []string
	if s.transcodes != nil {
		ids = s.transcodes.ids()
	}
	for _, id := range ids {
		if s.d.Transcoder != nil {
			s.stopPlexHLS(id)
		}
		s.dropTranscode(id)
	}

	if s.d.DB != nil {
		s.stopDownloadsForRestart()
		s.stageDownloads()
	} else {
		s.releaseAllDownloads()
	}
}

type noopExperience struct{}

func (noopExperience) Views(context.Context, *frontend.Viewer) ([]frontend.View, error) {
	return nil, nil
}
func (noopExperience) Collections(context.Context, *frontend.Viewer, frontend.Device, frontend.CollectionQuery) (frontend.CollectionPage, error) {
	return frontend.CollectionPage{}, nil
}
func (noopExperience) NotInterested(context.Context, *frontend.Viewer, frontend.Device, frontend.PageQuery) (frontend.NotInterestedPage, error) {
	return frontend.NotInterestedPage{}, nil
}
func (noopExperience) NotInterestedItem(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.NotInterestedEntry, error) {
	return frontend.NotInterestedEntry{}, frontend.ErrNotFound
}
func (noopExperience) ResolveExternal(context.Context, *frontend.Viewer, frontend.Device, frontend.ExternalRequest) (frontend.RatingKey, error) {
	return 0, frontend.ErrNotFound
}
func (noopExperience) Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	return nil, nil
}
func (noopExperience) Item(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
	return frontend.ItemDetail{}, frontend.ErrNotFound
}
func (noopExperience) Children(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.Page, error) {
	return frontend.Page{}, nil
}
func (noopExperience) LibraryItems(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
	return frontend.Page{}, nil
}
func (noopExperience) Similar(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.SearchPage, error) {
	return frontend.SearchPage{}, nil
}
func (noopExperience) Ancestors(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) ([]frontend.Title, error) {
	return nil, nil
}
func (noopExperience) Facets(context.Context, *frontend.Viewer, frontend.Device, string, string) ([]frontend.FacetValue, error) {
	return nil, nil
}
func (noopExperience) ResolveFacet(context.Context, *frontend.Viewer, frontend.Device, string, string, string) (frontend.FacetValue, error) {
	return frontend.FacetValue{}, frontend.ErrNotFound
}
func (noopExperience) PersonFacetIDs(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, []int64) (map[int64]string, error) {
	return nil, nil
}
func (noopExperience) NamedFacetIDs(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, string, []string) (map[string]string, error) {
	return nil, nil
}
func (noopExperience) Search(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.SearchPage, error) {
	return frontend.SearchPage{}, nil
}
func (noopExperience) UserData(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
	return nil, nil
}
func (noopExperience) SetPlayed(context.Context, *frontend.Viewer, frontend.RatingKey) error {
	return nil
}
func (noopExperience) SetUnplayed(context.Context, *frontend.Viewer, frontend.RatingKey) error {
	return nil
}
func (noopExperience) SetFavorite(context.Context, *frontend.Viewer, frontend.RatingKey, bool) error {
	return nil
}
func (noopExperience) SetNotInterested(context.Context, *frontend.Viewer, frontend.RatingKey, bool) error {
	return nil
}
func (noopExperience) SetWatchlist(context.Context, *frontend.Viewer, frontend.RatingKey, bool) error {
	return nil
}
func (noopExperience) SetEntityFavorite(context.Context, *frontend.Viewer, frontend.FavoriteRef, bool) error {
	return nil
}
func (noopExperience) EntityFavorites(context.Context, *frontend.Viewer, []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
	return nil, nil
}
func (noopExperience) ResolveEventSubject(context.Context, *frontend.Viewer, frontend.Device, frontend.FavoriteRef) (frontend.ResolvedEventSubject, error) {
	return frontend.ResolvedEventSubject{}, frontend.ErrNotFound
}
func (noopExperience) SetRating(context.Context, *frontend.Viewer, frontend.RatingKey, float64) error {
	return nil
}
func (noopExperience) SetLikes(context.Context, *frontend.Viewer, frontend.RatingKey, *bool) error {
	return nil
}
func (noopExperience) RemoveFromContinue(context.Context, *frontend.Viewer, frontend.RatingKey) error {
	return nil
}
func (noopExperience) NewRecent(frontend.RecentOptions) frontend.Recent { return nil }
func (noopExperience) NewDetailRelated(frontend.DetailRelatedOptions) frontend.DetailRelated {
	return nil
}
func (noopExperience) NewEpisodeBatch(frontend.EpisodeBatchOptions) frontend.EpisodeBatch {
	return nil
}
func (noopExperience) NewContinueDeck(frontend.ContinueDeckOptions) frontend.ContinueDeck {
	return nil
}
func (noopExperience) NewContinue(frontend.ContinueOptions) frontend.Continue { return nil }
func (noopExperience) NewRowSelection(frontend.RowSelectionOptions) frontend.RowSelection {
	return nil
}
func (noopExperience) NewHomeSelection(frontend.HomeSelectionOptions) frontend.HomeSelection {
	return nil
}
func (noopExperience) NewScopedCollection(frontend.ScopedCollectionOptions) frontend.ScopedCollection {
	return nil
}
func (noopExperience) NewPlaylistSelection(frontend.PlaylistSelectionOptions) frontend.PlaylistSelection {
	return nil
}
func (noopExperience) NewPostPlaySelection(frontend.PostPlaySelectionOptions) frontend.PostPlaySelection {
	return nil
}
func (noopExperience) NewDiscoverySelection(frontend.DiscoverySelectionOptions) frontend.DiscoverySelection {
	return nil
}
func (noopExperience) NewTrackPreferenceSelector(frontend.TrackPreferenceSelectorOptions) frontend.TrackPreferenceSelector {
	return nil
}
func (noopExperience) SearchFor(frontend.SearchSources, frontend.Visibility, func(*frontend.User) bool) frontend.Search {
	return nil
}
func (noopExperience) StatisticsDefaults(d frontend.Dashboard, r frontend.ResourceStatistics, _ func() time.Time) (frontend.Dashboard, frontend.ResourceStatistics) {
	return d, r
}
func (noopExperience) MergeShareFilter(filter, _ frontend.Filter) (frontend.Filter, bool) {
	return filter, false
}
func (noopExperience) ArrangeHome(_ context.Context, _ frontend.Recommender, _ *frontend.User, _ frontend.HubContext, _ string, rows []frontend.Hub, _ []frontend.NeutralHomeRow) ([]int, error) {
	idx := make([]int, len(rows))
	for i := range rows {
		idx[i] = i
	}
	return idx, nil
}
func (noopExperience) DetailExtras(context.Context, frontend.ExtrasSource, *frontend.Title, string, map[int]bool) ([]frontend.ExtraClip, error) {
	return nil, nil
}
func (noopExperience) DiscoverySearch(*frontend.DiscoveryFeatures) func(context.Context, *frontend.User, string, int) ([]frontend.Title, bool) {
	return nil
}
func (noopExperience) FamiliarFrom(any) func(context.Context, *frontend.User, []frontend.RatingKey) map[frontend.RatingKey]int {
	return nil
}
func (noopExperience) ContinueFilter(_ frontend.State, personal func(context.Context, *frontend.User, []frontend.Title) []frontend.Title) func(context.Context, *frontend.User, []frontend.Title) []frontend.Title {
	return personal
}
func (noopExperience) SameDayUnavailable(context.Context, *frontend.Title, time.Time, func(context.Context, []frontend.Title) []frontend.Title, frontend.AvailabilitySource) bool {
	return false
}
func (noopExperience) SeasonNextEpisode([]frontend.Title, map[frontend.RatingKey]frontend.WatchStatus, time.Time, func(*frontend.Title) bool) *frontend.Title {
	return nil
}
func (noopExperience) ShowOnDeck([]frontend.Title, map[frontend.RatingKey]frontend.WatchStatus, func(*frontend.Title) bool) *frontend.Title {
	return nil
}
func (noopExperience) RequestDatabaseObservation(context.Context) (int64, time.Duration) {
	return 0, 0
}
func (noopExperience) PlaybackMarkerRanges(context.Context, frontend.MarkerRanges, frontend.RatingKey, frontend.MediaInfo) ([]frontend.MarkerRange, error) {
	return nil, nil
}
func (noopExperience) CatalogSource() frontend.Catalog         { return nil }
func (noopExperience) CollectionsSource() frontend.Collections { return nil }
func (noopExperience) StateSource() frontend.State             { return nil }
func (noopExperience) PlaylistsSource() frontend.Playlists     { return nil }
func (noopExperience) RecommenderSource() frontend.Recommender { return nil }
func (noopExperience) VibeSearchSource() frontend.VibeSearcher { return nil }
func (noopExperience) SearchSources() frontend.SearchSources   { return frontend.SearchSources{} }
func (noopExperience) FavoriteShowsFunc() func(context.Context, *frontend.User) (map[frontend.RatingKey]bool, error) {
	return nil
}
func (noopExperience) FilterAvailableFunc() func(context.Context, []frontend.Title) []frontend.Title {
	return nil
}
func (noopExperience) JudgedOnFactsFunc() func(context.Context) func(frontend.Title) bool {
	return nil
}
func (noopExperience) PrioritizeAvailableFunc() func(context.Context, []frontend.Title) []frontend.Title {
	return nil
}
func (noopExperience) EpisodeOrderSource() frontend.EpisodeOrder      { return nil }
func (noopExperience) DiscoveryFeatures() *frontend.DiscoveryFeatures { return nil }
func (noopExperience) DashboardSource() frontend.Dashboard            { return nil }
func (noopExperience) ResourcesSource() frontend.ResourceStatistics   { return nil }
func (noopExperience) LibraryTitles() (string, string)                { return "", "" }
func (noopExperience) HomeRevision(context.Context, *frontend.User) (frontend.HomeRevision, bool, error) {
	return frontend.HomeRevision{}, false, nil
}
func (noopExperience) NoProviderWait(ctx context.Context) context.Context { return ctx }
