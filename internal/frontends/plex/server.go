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
	plexEncoder      SegmentedEncoder
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

	d      Deps
	log    *slog.Logger
	router chi.Router

	sections []Section

	artwork          frontend.Artwork
	hostImages       frontend.Images
	imageGrants      frontend.ImageGrants
	events           frontend.Subscriber
	imageURL         frontend.ImageURLFunc
	generatedPosters frontend.TitlePosterRenderer
	liveTV           frontend.LiveTV
	support          frontend.DiagnosticRecorder
	clientSeen       func(string)
	remoteClientSeen func(string)

	images       frontend.ArtworkImageCache
	channelLogos interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	channelRaster     frontend.SVGRasterizer
	boundStreams      frontend.StreamBindings
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
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	if d.SessionStores == nil {
		panic("plex: Deps.SessionStores is required (wire sessions.NewSessionStores())")
	}
	if d.Experience == nil {
		panic("plex: Deps.Experience is required (wire experience.NewSelectors())")
	}
	d.Dashboard, d.Resources = d.Experience.StatisticsDefaults(d.Dashboard, d.Resources, d.Now)
	coord := d.SessionCoordination
	if coord == nil || coord.PlaybackOwners() == nil || coord.LifecycleGates() == nil {
		coord = d.SessionStores.NewCoordination()
	}
	s := &Server{
		activities:        map[string]model.Activity{},
		plexEncoder:       d.Encoder,
		boundStreams:      d.Streams,
		d:                 d,
		log:               d.Logger.With("component", "plex"),
		imageURL:          func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path },
		images:            frontend.NewArtworkRemoteCache(128<<20, d.HTTPClient),
		channelLogos:      frontend.NewUntrustedImageFetcher(d.HTTPClient),
		channelRaster:     frontend.NewSVGRasterizer(),
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
	if !d.DisableEnrichment && d.Catalog != nil {
		s.titleJobs = newBGQueue(ctx, &s.wg, titleFetchWorkers, titleFetchQueueLen)
		s.blurJobs = newBGQueue(ctx, &s.wg, ultraBlurWorkers, ultraBlurQueueLen)
		s.titles = newTitleCache(s.titleJobs, s.now, s.log, s.enrichFetch)
		if pre, ok := d.Catalog.(interface {
			PreloadStored(context.Context, []frontend.RatingKey)
		}); ok {
			// Detail fetches scheduled together (a hub's shows) read their
			// stored titles in one batch; each fetch then reads the cache.
			s.titles.prefetch = pre.PreloadStored
		}
		s.ultra = frontend.NewArtworkPaletteCache(d.DB, s.blurJobs.submit, s.now, s.log, func(ctx context.Context, key frontend.RatingKey, path string) (frontend.ArtworkPalette, error) {
			var art frontend.ArtworkCollectionArt
			if c, ok := s.d.Collections.(frontend.ArtworkCollectionArt); ok {
				art = c
			}
			return frontend.ComputeArtworkItemPalette(ctx, s.images, s.imageURL, key, path, s.now(), art)
		})
	}
	if p, ok := d.Catalog.(interface {
		PersistsArtwork(context.Context, frontend.RatingKey) bool
	}); ok && s.ultra != nil {
		s.ultra.PersistFor(p.PersistsArtwork)
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
	if d.HomeCache != nil && d.HomeCache.Revision != nil {
		s.home = newHomeCache()
		s.wg.Add(1)
		go s.homeRefresher()
	}
	if s.cl != nil {
		s.cl.start()
	}
	return s
}

func (s *Server) bindHost(host frontend.Host) {
	if s == nil {
		return
	}
	// host.Support is a concrete pointer: assign it only when set, or the
	// interface would hold a typed nil that passes every nil check.
	s.support = nil
	if host.Support != nil {
		s.support = host.Support
	}
	s.liveTV = host.LiveTV
	s.clientSeen = host.ClientSeen
	s.remoteClientSeen = host.RemoteClientSeen
	s.artwork = host.Artwork
	if host.Artwork != nil {
		if rc := host.Artwork.RemoteCache(); rc != nil {
			s.images = rc
		}
		if pc := host.Artwork.PaletteCache(); pc != nil {
			s.ultra = pc
		}
		if pr := host.Artwork.PosterRenderer(); pr != nil {
			s.generatedPosters = pr
		}
		if fn := host.Artwork.ImageURLFunc(); fn != nil {
			s.imageURL = fn
		}
	}
	s.hostImages = host.Images
	s.imageGrants = host.ImageGrants
	s.events = host.Events
}

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
	var movies, shows string
	if s.d.LibraryTitles != nil {
		movies, shows = s.d.LibraryTitles()
	}
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
