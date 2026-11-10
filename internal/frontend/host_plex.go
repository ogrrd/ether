package frontend

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// PlexIdentity is how the Plex server presents itself. Empty fields fall back to
// the config snapshot (friendly name, machine identifier) or sensible defaults.
type PlexIdentity struct {
	MachineIdentifier string
	FriendlyName      string
	Version           string // PMS-style version string
	Platform          string // "MacOSX", "Windows", "Linux"
	PlatformVersion   string
	// MyPlexSubscription reports whether the owner has Plex Pass; Ether
	// reports this honestly from plex.tv.
	MyPlexSubscription bool
	OwnerFeatures      []string
}

// PlexHooks are optional callbacks into other components. They run on their own
// goroutine and must be safe for concurrent use.
type PlexHooks struct {
	// OnClientSeen receives the client identifier after authentication.
	OnClientSeen func(clientID string)
	// OnRemoteClientSeen records authenticated inbound connectivity. The
	// address has passed the configured trusted-proxy rules; no token is sent.
	OnRemoteClientSeen func(address string)
	// OnTimeline is called for every timeline update (prefetch uses it).
	OnTimeline func(user *User, key RatingKey, offset, duration time.Duration, state string)
	// OnScrobble is called when a user marks an item watched.
	OnScrobble func(user *User, key RatingKey)
}

// PlexSection is a library section exposed to Plex clients.
type PlexSection struct {
	ID       int
	Type     MediaType // movie or show
	Title    string
	UUID     string // stable; derived from the machine identifier when empty
	Language string
}

// PlexHomeCacheSource supplies the revision inputs of cached Plex Home screens.
// Revision must be cheap and shared across cluster nodes (the database);
// Layout and Catalog are local generations.
type PlexHomeCacheSource struct {
	Revision func(context.Context, *User) (HomeRevision, error)
	// Layout changes when the owner saves the Home layout (hard).
	Layout func() uint64
	// Config changes when the owner's settings are replaced (hard).
	Config func() uint64
	// Catalog changes on catalogue writes (refreshes behind the response).
	Catalog func() uint64
	// NoProviderWait marks a Home build's context so catalogue reads never
	// wait on TMDB: a title or season the mirror lacks is fetched and stored
	// behind the build, and the catalogue write it makes refreshes the
	// screen. A Home request therefore never writes or waits on a provider.
	NoProviderWait func(context.Context) context.Context
}

// PlexEncoder is an opt-in trusted host seam implemented by the shared broker.
// Starts occur only after existing Plex authentication, owner gates, preparation
// and admission. The broker must revalidate the exact source revision and retain
// uncertain execution responsibility. It may choose local execution only before
// remote allocation; a returned error is never permission for Plex to retry local.
// Session/Stop true means the broker owns responsibility, even when Session is
// nil because start is pending. Transport errors must never turn true into false.
type PlexEncoder interface {
	StartPlexHLS(context.Context, *User, string, string, HLSRequest) (HLSSession, error)
	PlexHLSSession(string) (HLSSession, bool)
	StopPlexHLS(string) bool
}

// PlexSubtitleSource resolves cached OpenSubtitles and synchronized sidecars.
type PlexSubtitleSource interface {
	OpenFormat(string, string) (io.ReadCloser, string, error)
}

// PlexSubtitleCatalog is optionally implemented by PlexDeps.Subtitles: the sidecars
// already cached for a title and viewer, without network access, skipping
// streams embedded already lists (by ExternalURL) or covers (by language).
type PlexSubtitleCatalog interface {
	CachedSidecars(ctx context.Context, t *Title, u *User, embedded []StreamInfo) []StreamInfo
}

// PlexSubtitleOffer is one subtitle search result.
type PlexSubtitleOffer struct {
	ID              int64  // provider file id, passed back to DownloadSubtitle
	Language        string // ISO 639-1
	Title           string // release or file name
	Provider        string // "OpenSubtitles"
	Score           int
	HearingImpaired bool
	Forced          bool
	PerfectMatch    bool // matched the playing file's hash or release
}

// PlexSubtitleSearch is optionally implemented by PlexDeps.Subtitles: Plex's
// on-demand subtitle search and pick.
type PlexSubtitleSearch interface {
	SearchSubtitles(ctx context.Context, t *Title, u *User, lang, release string) ([]PlexSubtitleOffer, error)
	// DownloadSubtitle fetches an offer returned by SearchSubtitles for t
	// and returns its sidecar stream; ErrNotFound for unknown offers.
	DownloadSubtitle(ctx context.Context, t *Title, u *User, id int64) (StreamInfo, error)
}

// PlexSubtitleUpload is optionally implemented by PlexDeps.Subtitles: Plex Web's
// "Upload Subtitles" (POST /library/metadata/{key}/subtitles).
type PlexSubtitleUpload interface {
	UploadSubtitle(ctx context.Context, t *Title, u *User, filename, format, language string, body []byte) (StreamInfo, error)
}

// PlexDeps are the Plex server's dependencies. Catalog, State, Auth and Playback
// are required; the rest degrade gracefully when nil.
type PlexDeps struct {
	// PlexEncoder optionally dispatches authenticated universal playback through
	// the shared broker; nil preserves the existing local encoder path.
	PlexEncoder     PlexEncoder
	PlexStreams     PlexStreamAPI
	ArtworkImages   ArtworkImageCache
	ArtworkPalettes ArtworkPaletteCache
	// ArtworkStores constructs the artwork caches/sources this server owns
	// and provides the artwork and Live TV helpers it calls. Required:
	// NewServer panics without it. The app wires artworkservice.NewPlexArtworkStores().
	ArtworkStores PlexArtworkStores
	// SessionStores constructs the session stores this server owns and
	// provides the session helpers it calls. Required: NewServer panics
	// without it. The app wires sessions.NewPlexSessionStores().
	SessionStores PlexSessionStores
	// Experience constructs the experience selectors this server uses and
	// provides its operations, including statistics defaults. Required:
	// NewServer panics without it. The app wires experience.NewPlexExperience().
	Experience PlexExperience
	// GeneratedPosters draws Ether's poster for a movie or show with no
	// artwork; nil leaves such titles without a thumb.
	GeneratedPosters    any
	SessionCoordination Coordination
	// LibraryTitles returns the Movies / TV Shows library names from the
	// libraries service (libraries 1/2); nil uses "Movies" / "TV Shows".
	LibraryTitles   func() (movies, shows string)
	FavoriteShows   func(context.Context, *User) (map[RatingKey]bool, error)
	FilterAvailable func(context.Context, []Title) []Title
	// JudgedOnFacts reports, for a request, the titles FilterAvailable judges
	// on their full facts (availability.Service.JudgedOnFacts).
	JudgedOnFacts       func(context.Context) func(Title) bool
	PrioritizeAvailable func(context.Context, []Title) []Title
	EpisodeOrder        EpisodeOrder
	PlexGUID            LibraryGUIDs
	PlexMeta            LibraryMetadata
	Discovery           *DiscoveryFeatures
	// Search is the app's shared search (every frontend's); zero builds it
	// from this server's own dependencies.
	Search       SearchSources
	LiveTV       LiveTVHost
	AccessPolicy AccessPolicy
	Catalog      Catalog
	Collections  Collections
	VibeSearch   VibeSearcher
	Recommender  Recommender
	State        State
	Playlists    Playlists
	Auth         Auth
	Playback     Playback
	Transcoder   Transcoder
	// Variants lists distinct release versions for "Play Version" detail
	// pages (optional: without it, detail pages stay single-Media).
	Variants VariantResolver
	// DecisionTraces is the latest resolution for a title, copied into a
	// support capture. Nil leaves playback unchanged.
	DecisionTraces DecisionTraceSource
	// Downloads remuxes resolved sources to temp MP4s for the Download
	// Queue API; nil disables downloads (allowSync stays "0").
	Downloads DownloadRemuxer
	Subtitles PlexSubtitleSource

	// ApplyClientRules adjusts caps for the owner's quality rules. Nil leaves
	// the request caps unchanged. The app wires resolver.ApplyClientRules so
	// this package stays outside the resolver.
	ApplyClientRules func(HostQualityConfig, ClientCaps, *User, string) ClientCaps
	// Config returns the current settings snapshot.
	Config func() HostConfig
	// OwnerSubscription returns the current entitlement snapshot from plex.tv.
	OwnerSubscription func() (active bool, features []string)
	// ImageURL builds TMDB image URLs; defaults to image.tmdb.org.
	ImageURL ImageURLFunc
	Logger   *slog.Logger
	Identity PlexIdentity
	Hooks    PlexHooks

	// Sections overrides the default Movies (1) and TV Shows (2) sections.
	Sections []PlexSection
	// HTTPClient fetches artwork for the image proxy; defaults to a client
	// with a 20 s timeout. The app wires the metadata route so this package
	// does not import the egress policy.
	HTTPClient *http.Client
	// DirectHTTP proxies trailer bytes. Nil uses the default transport.
	// The app wires the direct route so this package does not import egress.
	DirectHTTP *http.Client
	// ImageCacheBytes bounds the in-memory artwork cache (default 128 MiB).
	ImageCacheBytes int64
	// DB persists artwork data and ordered shelf-generation keys. Clustered
	// nodes share this store so continuations survive failover and restarts.
	// Optional: without it, these caches remain process-local.
	DB *sql.DB
	// DisableEnrichment turns off the background title-detail and
	// UltraBlurColors work (the spike test library has nothing to fetch).
	DisableEnrichment bool
	// Cluster, when set (clustered nodes only; requires DB), shares play
	// sessions, play queues, terminations and notifications with the other
	// nodes. Nil keeps everything in this process.
	Cluster ClusterNode
	// SessionHeartbeat is how often a clustered node refreshes its shared
	// play sessions (default 10s).
	SessionHeartbeat time.Duration
	// Now is the clock (tests).
	Now func() time.Time
	// PublicURL is this server's published public address
	// ("https://<ip>.<hash>.plex.direct:<port>"), or "" without remote access.
	PublicURL func() string
	// LiveLogoKey signs token-free channel logo URLs; empty uses a
	// process-local key.
	LiveLogoKey []byte
	// RepoFileURL is a public, immutable URL for a file committed to Ether's
	// repository at the source this build came from, or "" when unknown.
	RepoFileURL func(path string) string
	// Dashboard is this node's in-memory Plex Web Dashboard store. Nil is
	// replaced with a process-local store at NewServer. Statistics are not
	// clustered; a client reads the node it connected to.
	Dashboard Dashboard
	// Resources is this node's host sample ring for /statistics/resources.
	Resources ResourceStatistics
	// Support records neutral request and playback facts. Nil leaves playback unchanged.
	Support DiagnosticRecorder
	// HomeCache serves Plex Home from memory (home_cache.go); nil builds
	// every Home request.
	HomeCache *PlexHomeCacheSource
	// TracePlayback logs HLS/transcode requests when ETHER_TRACE_PLAYBACK=1.
	// It does not change responses.
	TracePlayback bool
}

// PlexServer is the host-visible contract of a constructed Plex server.
type PlexServer interface {
	http.Handler
	Close() error
	Closed() bool
	Done() <-chan struct{}
	ViewerHomeChanged(userID int64)
	NotifyLiveLineup()
	LibrarySections() []PlexSection
}

// PlexHost supplies Plex's host services and lifecycle hooks on Host.
type PlexHost interface {
	Server(ctx context.Context) (PlexServer, bool, error)
	Deps(ctx context.Context) (PlexDeps, error)
	BindServer(PlexServer)
	WatchRefresh(fn func(userID int64, key RatingKey)) func()
	Status(ctx context.Context) Status
	Listeners() []ListenerSpec
}
