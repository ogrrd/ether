// Package app wires Ether's components into one running server: the Plex
// front end and the owner UI on one port, and the background services
// (catalogue sync, plex.tv registration, prefetch, LAN discovery) behind it.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/artwork"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/cachefill"
	"github.com/braedonsaunders/ether/internal/calendar"
	"github.com/braedonsaunders/ether/internal/cardigann"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/hoststat"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/migrate"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/plexwatch"
	"github.com/braedonsaunders/ether/internal/prefetch"
	"github.com/braedonsaunders/ether/internal/prerolls"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/simkl"
	"github.com/braedonsaunders/ether/internal/spike"
	"github.com/braedonsaunders/ether/internal/startup"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/stats"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/stream"
	"github.com/braedonsaunders/ether/internal/stremio"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/sysload"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/trakt"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/trickplay"
	"github.com/braedonsaunders/ether/internal/updater"
	"github.com/braedonsaunders/ether/internal/usenet"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configure a run.
type Options struct {
	// BreakEvidenceObserver is an opt-in local owner diagnostic hook. It runs
	// synchronously while the analyzed source lease is open; never retain its URL.
	BreakEvidenceObserver func(context.Context, markers.BreakInput, markers.BreakEvidence)
	clusterSetup          *clusterSetup
	// NotificationExecutable enables the independent crash watcher for executable runs.
	NotificationExecutable string
	DataDir                string
	Version                string
	// Logger receives everything; the admin log ring is teed onto it.
	Logger *slog.Logger
	// Spike forces spike mode (local files, no Usenet) on top of config.
	Spike bool
	// SpikeFiles are extra local files for spike mode.
	SpikeFiles []string
	// DBRetryMin / DBRetryMax bound the backoff while a clustered node
	// waits at startup for an unreachable PostgreSQL server (1s / 30s).
	DBRetryMin, DBRetryMax time.Duration
	// FixtureMedia, when set, is the resolver indexer, release health checker,
	// and playback engine. Production leaves it nil.
	FixtureMedia interface {
		core.Indexer
		core.HealthChecker
		core.Engine
	}
	// FixtureTranscoder, when set, replaces the ffmpeg manager so every
	// protocol shares one transcode budget without an encoder binary.
	FixtureTranscoder core.Transcoder
	// CatalogNow, when set, is the catalog clock from construction. Tests
	// pin it so a minute snapshot cannot expire while a background browse runs.
	CatalogNow func() time.Time
	// plextvHTTP, when set, is the HTTP client used by the plex.tv client in tests.
	plextvHTTP *http.Client
	// diskUsage, when set, replaces the cache volume measurement behind the
	// segment cache's free-space floor, so tests never depend on the free
	// space of the machine running them.
	diskUsage segcache.DiskUsageFunc
	// front is Serve's listener, bound before the database opens; nil
	// when an App is run on its own (tests), which then binds in Run.
	front *frontDoor
	// lock is Serve's hold on the data directory, kept across starts; nil
	// when an App is run on its own, which then takes it itself.
	lock *instanceLock
}

// App is a running Ether server.
type App struct {
	// Nil until a client path proves manual-intent and live selection support.
	automaticSubtitleClientSupported func(frontend.SessionAccess) bool

	deployment       *deploymentRuntime
	brokerPlayback   core.Playback
	brokerTranscoder core.Transcoder
	perf             *perfObserver // per-request database cost, budgets and trace
	availability     *availability.Service
	opts             Options
	log              *slog.Logger
	logs             *admin.LogBuffer
	support          *diagnostics.Recorder
	cfg              *config.Store
	proxyMu          sync.Mutex
	prowlarrMu       sync.Mutex
	prowlarrState    string
	prowlarrKept     bool
	proxyGen         uint64
	proxyReady       bool
	proxyPrefixes    []netip.Prefix
	// seenFamilies records a client family policy once per process. Playback
	// does not take a write lock to refresh last_seen.
	seenFamilies sync.Map
	db           *sql.DB
	// databaseURL is the cluster database, placed in join tokens
	// (cluster_settings.go); empty on a standalone server.
	databaseURL string

	// cluster is Solo on SQLite and Postgres on a shared database; peers
	// caches the other nodes' URIs for the plex.tv publisher.
	cluster      cluster.Cluster
	peers        *peerSet
	egress       *egressIP // this node's public address, heartbeated when clustered
	outboundDial string    // last Usenet dial identity; a change drops warm connections
	// clusterFatal receives the error that ended this node's cluster
	// membership (a duplicate node identity); nil on a standalone server.
	clusterFatal chan error
	// dbAvail is this node's view of shared-database reachability: the
	// heartbeat and the outboxes that hold progress, resume and play
	// records through an outage report to it. Nil on a standalone server.
	dbAvail *store.Availability
	// draining is set while this node drains before installing an update
	// (heartbeated; see rolling.go).
	draining atomic.Bool

	tmdb        *tmdb.Client
	catalog     *catalog.Catalog
	collections *collections.Service
	images      *artwork.Images
	// titlePosterRenderer draws generated posters (titlePosters).
	titlePosterOnce     sync.Once
	titlePosterRenderer *artwork.Renderer
	favorites           *favorites.Service
	notInterested       *notinterested.Service
	playlists           core.Playlists
	state               *state.State
	trakt               *trakt.Client
	simklDevice         *simkl.DeviceManager
	llm                 *llm.Service
	recommend           *recommend.Recommender
	personalize         *personalize.Engine
	subtitles           *subtitles.Service
	lang                *langfeat.Service
	fetcher             *usenet.Fetcher
	cache               *segcache.Cache
	cacheConfig         atomic.Pointer[config.Cache]
	engine              *stream.Engine
	indexers            *newznab.Manager
	sites               *cardigann.Service // built-in indexer sites from owner-installed definitions
	addons              *stremio.Service   // Stremio stream addons configured as indexers
	// Debrid: torrent releases stream through owner debrid accounts. The
	// wrappers dispatch on the payload and delegate Usenet to engine and
	// indexers, so every consumer uses media and sources.
	debrid              *debrid.Service
	media               *debrid.Engine  // core.Engine + core.HealthChecker over engine
	sources             *debrid.Indexer // core.Indexer over indexers
	resolver            *resolver.Service
	guides              *resolver.TrashGuides
	dictionarry         *resolver.Dictionarry
	qualitySync         *resolver.QualitySyncer
	transcoder          *transcode.Manager
	sourceProber        *transcode.SourceProber
	trickplay           *trickplay.Owner
	load                *sysload.Sampler // machine load for the Overview
	playback            *playback.Manager
	tracePlayback       bool // ETHER_TRACE_PLAYBACK, set in buildPlayback
	prefetch            *prefetch.Prefetcher
	cachefill           *cachefill.Manager
	plextv              *plextv.Client
	plexGUID            *plexguid.Service
	plexMeta            *plexmeta.Service
	plexWatch           *plexwatch.Service
	plexServers         *plexwatch.Servers
	users               *auth.Service
	sessionEvents       *sessions.EventBus
	sessionCoordination *sessions.Coordination

	certs     *plextv.CertManager
	registrar *plextv.Registrar
	gdm       *remote.GDMResponder

	episodeorder      *episodeorder.Service
	watchlist         *watchlist.Service
	ratings           *ratings.Service
	overlays          *overlays.Service
	discoveryPersonal *personalize.Discovery
	discoveryFeatures *discovery.Features
	stats             *stats.Service
	hostResources     *hoststat.Resources
	notifications     *notify.Service
	livetv            *livetv.Service
	calendar          *calendar.Service
	trackprefs        *trackprefs.Service
	markers           *markers.Service
	extras            *extras.Service
	prerolls          *prerolls.Service
	pluginContent     *onair.PluginContentCache
	onair             *onair.Service
	backups           *backup.Service
	migration         *migrate.Service
	safety            *safety.Service
	updates           *updateState
	frontends         *frontendRuntime
	frontendClients   *frontendClients
	plexServer        *plex.Server
	artwork           *artworkservice.Source
	downloadRemuxer   core.DownloadRemuxer
	plexBuildMu       sync.Mutex
	plexClose         func()
	spikeLibrary      *spike.Library
	handler           http.Handler
	instanceLock      *instanceLock
	closers           []func()
	closeOnce         sync.Once
	stopPrewarm       context.CancelFunc
	prewarmDone       chan struct{}
	// tracker reports this start's progress on the startup status page.
	tracker *startup.Tracker
	// restored is the report of a restore applied (or rolled back) at this
	// start; boundNow is set when this start bound the front door.
	restored *backup.Report
	boundNow bool
	// readPending reads the restore record (backup.ReadPending when nil).
	readPending func(dataDir string) (*backup.Pending, error)
	// integrity is the database's last integrity check (SQLite).
	integrity         atomic.Pointer[store.Integrity]
	integrityChecking atomic.Bool
	// compaction is the owner's "Compact database" action (dbcompact.go).
	compaction databaseCompaction
	// restart is an owner-requested restart (restart.go).
	restart restartState
	// Extensibility (docs/extend/SPEC.md). Each runtime type lives in its own
	// file and is owned by one workstream: extend.go, console_stream.go,
	// plugins.go and agent.go.
	extendRT  *extendRuntime
	streamRT  *consoleStreamRuntime
	pluginsRT *pluginRuntime
	agentRT   *agentRuntime
}

// New builds every component. Nothing listens until Run.
func New(ctx context.Context, opts Options) (_ *App, err error) {
	if opts.DataDir == "" {
		opts.DataDir = config.DefaultDataDir()
	}
	if opts.Version == "" {
		opts.Version = updater.Version
	}
	a := &App{opts: opts, logs: admin.NewLogBuffer(4000)}
	base := opts.Logger
	if base == nil {
		base = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	// The in-memory buffer behind Logs gets the same scrubbing as the file.
	a.log = slog.New(logredact.Handler(admin.TeeHandler(base.Handler(), a.logs)))
	slog.SetDefault(a.log)
	// Serve holds the data directory's lock across starts, so a retried
	// start never lets another instance in; an App run on its own takes it.
	if opts.lock == nil {
		if a.instanceLock, err = acquireInstanceLock(opts.DataDir); err != nil {
			return nil, err
		}
		a.closers = append(a.closers, a.instanceLock.Release)
	}
	a.perf = newPerfObserver(a.log.With("component", "perf"))
	a.closers = append(a.closers, a.perf.close)
	started := false
	defer func() {
		if !started {
			a.Close()
			// A restore applied at this start that cannot start puts the
			// original files back now, with the reason.
			if a.rollbackUnconfirmed(err) {
				err = fmt.Errorf("%w: %w", errRestoreRolledBack, err)
			}
		}
	}()
	if err := a.confirmUpdateStartup(); err != nil {
		return nil, err
	}
	if err := a.prepareDeployment(ctx); err != nil {
		return nil, err
	}
	a.tracker = startup.NewTracker(opts.Version, time.Now)
	if opts.front != nil && (a.deployment == nil || a.deployment.settings.Roles.FullServer()) {
		a.tracker = opts.front.tracker
		a.tracker.Begin()
		if err := a.openFrontDoor(); err != nil {
			return nil, err
		}
	}
	// A restore requested before the restart replaces the settings and the
	// database now, before anything has opened them.
	if err := a.applyPendingRestore(); err != nil {
		return nil, err
	}

	ctx = store.WithWorkProgress(ctx, a.upgradeProgress)
	a.tracker.Work("settings", 0, 0)
	// Opening the config may rewrite it for this version, so capture the
	// files as they are now: a pre-migrate database snapshot stores them, so
	// the snapshot can be restored together with its matching settings.
	snapshotFiles, snapshotErr := preUpgradeFiles(opts.DataDir)
	if snapshotErr != nil {
		a.log.Warn("pre-upgrade companion files incomplete; database snapshot alone may not restore settings and credentials", "err", snapshotErr)
	}
	if a.cfg, err = config.Open(opts.DataDir); err != nil {
		return nil, startFail(startup.FailConfig, fmt.Errorf("config: %w", err))
	}
	if warning := config.DataDirWarning(opts.DataDir); warning != "" {
		a.log.Warn(warning)
	}
	// ETHER_PORT sets the HTTP port before the UI is reachable, e.g. to run
	// next to a Plex Media Server already using 32400 with host networking.
	// It is saved, so plex.tv, LAN discovery and port mapping all agree.
	port, err := envPort()
	if err != nil {
		return nil, startFail(startup.FailConfig, err)
	}
	if port != 0 && a.cfg.Get().Server.Port != port {
		if err := a.cfg.Update(func(c *config.Config) error { c.Server.Port = port; return nil }); err != nil {
			return nil, startFail(startup.FailConfig, fmt.Errorf("ETHER_PORT: %w", err))
		}
		a.log.Info("server port set from ETHER_PORT", "port", port)
	}
	if err := a.keepListeningPort(); err != nil {
		return nil, startFail(startup.FailConfig, err)
	}
	cfg := a.cfg.Get()
	if opts.Spike {
		_ = a.cfg.Update(func(c *config.Config) error {
			c.Spike.Enabled = true
			c.Spike.LocalFiles = appendUnique(c.Spike.LocalFiles, opts.SpikeFiles...)
			return nil
		})
		cfg = a.cfg.Get()
	}
	dbCfg := cfg.Database.Effective(os.Getenv)
	if a.deployment != nil {
		dbCfg = cfg.Database
	}
	if err := a.validateRuntimeSettings(cfg.Server, dbCfg); err != nil {
		return nil, startFail(startup.FailConfig, fmt.Errorf("node settings: %w", err))
	}
	dirs := []string{cfg.Transcode.TempDir}
	if cfg.Cache.Storage != "shared" {
		dirs = append(dirs, cfg.Cache.Dir)
	}
	for _, dir := range dirs {
		if dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	a.tracker.SetDatabase(trackedDatabase(dbCfg))
	var dialect store.Dialect
	// A clustered node whose PostgreSQL server is briefly unreachable at
	// boot waits for it here (logging every attempt) rather than exiting
	// and being restarted in a tight loop by the service manager. Only the
	// startup status page answers meanwhile — on purpose: a node that
	// cannot reach the shared database must not answer Plex clients or GDM
	// discovery, and it is out of the published connections because it is
	// not heartbeating. The hang watchdog only starts in Run. Errors
	// retrying cannot fix (bad password, missing database, invalid URL)
	// still fail at once.
	var gate store.MigrationGate
	if dbCfg.IsPostgres() {
		id, err := cluster.LoadIdentity(opts.DataDir)
		if err != nil {
			return nil, fmt.Errorf("cluster: %w", err)
		}
		gate = startupMigrationsGate(id.NodeID, cfg.Database.DeferMigrations, clusterLiveAfter, time.Now)
	}
	if a.db, dialect, err = store.OpenWithRetry(ctx, store.Options{
		DataDir: opts.DataDir, Driver: dbCfg.Driver, URL: dbCfg.URL,
		Logger: a.log.With("component", "store"), MigrationGate: gate,
		SnapshotFiles: snapshotFiles, Progress: a.storeProgress,
	}, store.RetryOptions{
		MinBackoff: a.opts.DBRetryMin, MaxBackoff: a.opts.DBRetryMax,
		OnRetry: func(attempt int, wait time.Duration, err error) {
			if errors.Is(err, store.ErrMigrationDeferred) {
				a.tracker.Waiting(startup.PhaseWaitingCluster, attempt, wait, err)
				a.log.Warn("database: waiting before migrating the shared schema", "attempt", attempt, "retry", wait, "reason", err)
				return
			}
			a.tracker.Waiting(startup.PhaseWaitingDatabase, attempt, wait, err)
			a.log.Error("database: PostgreSQL is unreachable; waiting for it before starting",
				"url", dbCfg.Redacted().URL, "attempt", attempt, "retry", wait, "err", err)
		},
	}); err != nil {
		a.Close() // libera o instance lock (o db não foi aberto)
		return nil, startFail(openFailure(err, dbCfg.IsPostgres()), fmt.Errorf("database: %w", err))
	}
	a.cfg.SetPolicyEpoch(a.publishPolicyConfig)
	if err := a.checkRestoredIntegrity(ctx); err != nil {
		return nil, err
	}
	if dialect == store.Postgres {
		a.log.Info("database: using PostgreSQL", "url", dbCfg.Redacted().URL)
		a.warnNewerSchema(ctx)
	}
	a.closers = append(a.closers, func() { a.db.Close() })
	if err := a.buildCluster(dialect); err != nil {
		a.Close()
		return nil, fmt.Errorf("cluster: %w", err)
	}
	// A restored PostgreSQL-style bundle leaves a logical dump to import.
	if err := a.importRestoredDump(ctx); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.checkCredentialKey(ctx); err != nil {
		a.Close()
		return nil, startFail(startup.FailCredentialKey, err)
	}
	if err := a.migrateCredentialBlobs(ctx); err != nil {
		a.Close()
		return nil, fmt.Errorf("convert credential blobs: %w", err)
	}
	if dialect == store.Postgres {
		if err := a.attachCluster(ctx, dbCfg.URL); err != nil {
			a.Close()
			return nil, err
		}
		cfg = a.cfg.Get()
	}
	if err := a.migrateAppOverrides(); err != nil {
		a.Close()
		return nil, fmt.Errorf("migrate app overrides: %w", err)
	}
	cfg = a.cfg.Get()
	// Recheck after the shared settings overlay and override migration, before
	// constructing services. Role/address settings remain node-local.
	if err := a.validateRuntimeSettings(cfg.Server, dbCfg); err != nil {
		return nil, startFail(startup.FailConfig, fmt.Errorf("node settings: %w", err))
	}
	a.tracker.Set(startup.PhaseStartingServices)
	if a.deployment != nil && !cfg.Server.NodeRoles.FullServer() {
		if cfg.Server.NodeRoles.Has(config.RoleServer) {
			if err := a.buildServerRole(ctx, cfg); err != nil {
				return nil, err
			}
			a.connectServerHooks()
			a.initUpdates()
			a.syncOwnerRecovery(ctx)
			started = true
			return a, nil
		}
		if cfg.Server.NodeRoles.Has(config.RoleDelivery) {
			if err := a.buildDeliveryRole(ctx, cfg); err != nil {
				return nil, err
			}
			started = true
			return a, nil
		}
		if cfg.Server.NodeRoles.Has(config.RoleTranscode) {
			if err := a.buildPrivateEncoder(cfg); err != nil {
				return nil, err
			}
		}
		if cfg.Server.NodeRoles.Has(config.RoleSource) {
			if err := a.buildSourceRole(ctx, cfg); err != nil {
				return nil, err
			}
		}
		if err := a.buildPrivateCredentials(); err != nil {
			return nil, err
		}
		if err := a.buildClusterPlayback(ctx, cfg); err != nil {
			return nil, err
		}
		started = true
		return a, nil
	}
	cfg = a.adoptMountedCache(cfg)
	if err := a.build(ctx, cfg); err != nil {
		a.Close()
		return nil, err
	}
	a.connectClusterHooks()
	a.initUpdates()
	// Keep the owner's recovery sign-in in step with the database.
	a.syncOwnerRecovery(ctx)
	started = true
	return a, nil
}

func (a *App) build(ctx context.Context, cfg config.Config) error {
	if err := a.buildPrivateCredentials(); err != nil {
		return err
	}
	a.support = diagnostics.New(a.supportDeps())
	// One guide client serves the import console and the background sync,
	// so a listing fetched by either is cached for the other.
	ua := "Ether/" + a.opts.Version + " (+https://github.com/braedonsaunders/ether)"
	a.guides = resolver.NewTrashGuides(resolver.TrashOptions{UserAgent: ua})
	a.dictionarry = resolver.NewDictionarry(resolver.DictionarryOptions{UserAgent: ua})
	a.qualitySync = resolver.NewQualitySyncer(resolver.QualitySyncDeps{
		Guides:      a.guides,
		Dictionarry: a.dictionarry,
		Config:      a.cfg.Get,
		Save:        a.saveValidated,
		Logger:      a.log.With("component", "quality-sync"),
	})
	cfg.Cache = cfg.EffectiveCache()
	var err error
	if a.safety, err = safety.New(ctx, a.db); err != nil {
		return fmt.Errorf("safety: %w", err)
	}
	// Identity precedes model construction: background builders receive a fully
	// initialized auth service. Scorer consumers are built inside the model's
	// dependency phase before its workers start.
	for _, build := range []func(context.Context, config.Config) error{
		a.buildCatalogue, func(context.Context, config.Config) error {
			a.safety.SetCatalogPolicy(a.catalog.VisibleForViewer, a.catalog.DiscoveryVisibleForViewer)
			return a.rebuildPolicyInputs(ctx)
		}, a.buildIdentity, a.buildSessionEvents, a.buildPersonalization,
		a.buildSources, a.buildPlayback, a.buildClusterPlayback, a.buildPrefetch, a.buildIntegrations,
	} {
		if err := build(ctx, cfg); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.stopPrewarm = cancel
	a.prewarmDone = make(chan struct{})
	go func() {
		defer close(a.prewarmDone)
		a.prewarmBrowse(ctx)
	}()
	return nil
}

// prewarmBrowse fills the catalog and statement caches before the first viewer
// request. It stays on local reads: episode expansion can fetch seasons and
// take the write lock, and a request in flight must not wait for that.
func (a *App) prewarmBrowse(ctx context.Context) {
	if a == nil || a.catalog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	movies, _ := a.catalog.Browse(ctx, core.BrowseQuery{Sort: core.SortAdded, Desc: true, Limit: 16, Filter: core.Filter{Type: core.TypeMovie, KnownAddedDate: true}})
	shows, _ := a.catalog.Browse(ctx, core.BrowseQuery{Sort: core.SortTitle, Limit: 16, Filter: core.Filter{Type: core.TypeShow}})
	keys := make([]core.RatingKey, 0, len(movies.Items)+len(shows.Items))
	for _, title := range movies.Items {
		keys = append(keys, title.Key)
	}
	for _, title := range shows.Items {
		keys = append(keys, title.Key)
	}
	if len(keys) > 0 {
		_, _ = a.catalog.CachedGetMany(ctx, keys)
	}
}

// applyConfig pushes an edited config into the running components.
func (a *App) applyConfig(c config.Config) {
	c.Cache = c.EffectiveCache()
	a.applyOutbound(c)
	a.tmdb.UpdateConfig(c.Catalog)
	a.llm.UpdateConfig(c.LLM)
	a.fetcher.UpdateConfig(c.Usenet)
	a.sites.ApplyConfig(c)
	a.indexers.ApplyConfig(a.withAddonIndexers(c))
	a.engine.UpdateRepair(c.Usenet.Repair)
	a.debrid.UpdateConfig(c.Debrid)
	a.resolver.UpdateConfig(c)
	a.catalog.UpdateConfig(c.Catalog)
	a.configureCache(c.Cache)
	c.Cache = a.runningCacheConfig()
	a.engine.UpdatePlaybackReadAhead(c.Cache.PlaybackReadAhead())
	if a.cachefill != nil {
		a.cachefill.Wake()
	}
	a.playback.SetConfig(c)
	a.prefetch.SetConfig(c.Cache)
	if a.transcoder != nil {
		a.transcoder.UpdateConfig(c.Transcode)
	}
	a.trakt.UpdateConfig(c.Trakt)
	a.subtitles.UpdateConfig(c.OpenSubtitles)
	a.subtitles.UpdateProviders(c.Subtitles.Providers)
	// Re-publish section titles when Movies / TV Shows library names change.
	a.publishLibrarySections()
	a.registrar.Trigger()
}

// protectedReleases is every release that must survive cache eviction:
// live sessions (playback pins those itself) plus pinned in-progress titles.
func (a *App) protectedReleases() map[string]bool {
	out := map[string]bool{}
	for _, s := range a.playback.Sessions() {
		if s.Resolution != nil {
			out[s.Resolution.Release.ID] = true
		}
	}
	return out
}

// Handler is the combined Plex + owner UI handler (tests).
func (a *App) Handler() http.Handler { return a.handler }

// settleBeforeListen finishes startup database work that would otherwise
// hold the write lock across the first play. The listener is not open yet.
func (a *App) settleBeforeListen(ctx context.Context) {
	if a.safety != nil && a.users != nil {
		owner, err := a.users.Owner(ctx)
		if err == nil && owner != nil {
			if _, err := a.safety.ConfirmPlayback(ctx, owner, false, time.Now()); err != nil && !errors.Is(err, core.ErrPolicyUnconfirmed) && !errors.Is(err, core.ErrUnauthorized) {
				a.log.Debug("playback confirm warm-up", "err", err)
			}
		}
	}
	if a.db != nil && store.DialectOf(a.db) == store.SQLite {
		a.upgradeProgress("checkpoint", 0, 0)
		if _, err := a.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			a.log.Warn("startup checkpoint", "err", err)
		}
	}
}

// Config is the settings store.
func (a *App) Config() *config.Store { return a.cfg }

// Run serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ctx = store.WithWorkProgress(ctx, a.upgradeProgress)
	if a.deployment != nil && !a.cfg.Get().Server.NodeRoles.FullServer() {
		if roles := a.cfg.Get().Server.NodeRoles; roles.Has(config.RoleServer) || roles.Has(config.RoleDelivery) {
			return a.runPublicRole(ctx)
		}
		return a.runSpecialist(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.bindRestart(ctx, cancel)
	servePprof(ctx, a.log)
	cfg := a.cfg.Get()

	// Leave the cluster last, after background work has stopped.
	defer a.cluster.Close()
	if err := a.startCluster(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return a.runResult()
		}
		return err
	}
	stopPrivate, err := a.startPrivate(ctx)
	if err != nil {
		return err
	}
	defer stopPrivate()
	if err := a.migrateCredentials(ctx); err != nil {
		return fmt.Errorf("convert stored credentials: %w", err)
	}
	// Credential sealing and the startup WAL checkpoint finish before
	// background workers or the listener start, so neither waits on or
	// contends with that write.
	a.settleBeforeListen(ctx)
	var background backgroundGroup
	defer background.Wait()
	defer cancel()
	background.start(func() { a.runNodeControl(ctx) })
	background.start(func() { a.watchConsoleSessions(ctx) })

	// Singleton work runs only on the cluster leader (always, standalone).
	// trakt.Start stays on every node: it delivers this node's own
	// scrobbles, and its import queue is claimed per row.
	// Tracked like every other background loop, so Run returns (and the
	// caller closes the database) only after they have stopped.
	// Every node serves search: build its title index now, in the
	// background, so the first search does not fall back to SQL.
	a.catalog.WarmSearch(ctx)
	// Every node that serves search keeps its own long-tail index.
	background.start(func() { a.catalog.RunTail(ctx) })
	if parityRunCatalogueWorkers() {
		background.start(func() { a.cluster.RunAsLeader(ctx, "catalogue sync", a.catalog.Run) })
		background.start(func() { a.cluster.RunAsLeader(ctx, "trending charts", a.catalog.RunTrends) })
	}
	background.start(func() { a.cluster.RunAsLeader(ctx, "release availability", a.availability.Run) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "collection refresh", a.collections.Run) })
	background.start(func() { a.trakt.Start(ctx) })
	if a.plexWatch != nil {
		background.start(func() { a.plexWatch.Run(ctx) })
		background.start(func() { a.cluster.RunAsLeader(ctx, "plex watch imports", a.plexWatch.RunImports) })
	}
	if a.plexServers != nil {
		background.start(func() { a.cluster.RunAsLeader(ctx, "plex media server sync", a.plexServers.Run) })
	}
	background.start(func() {
		a.cluster.RunAsLeader(ctx, "simkl delivery", func(ctx context.Context) {
			simkl.RunDelivery(ctx, a.simklService, func() bool { return a.cfg.Get().Simkl.Enabled }, a.log.With("component", "simkl"))
		})
	})
	background.start(func() { a.load.Run(ctx) })
	background.start(func() { a.cache.Run(ctx) })
	background.start(func() { a.cachefill.Run(ctx) })

	background.start(func() { a.housekeeping(ctx) })
	background.start(func() { a.checkpointWAL(ctx) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "database maintenance", a.maintainDB) })
	background.start(func() { a.stats.Run(ctx) })
	if a.hostResources != nil {
		background.start(func() { a.hostResources.Run(ctx) })
	}
	background.start(func() { a.cluster.RunAsLeader(ctx, "notification delivery", a.notifications.Run) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "favorite episode alerts", a.runFavoriteEpisodeAlerts) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "viewer push delivery", a.runViewerPush) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "quality profile sync", a.runQualitySync) })
	if a.clustered() {
		background.start(func() { a.sampleHealth(ctx) })
	}
	if parityRunCatalogueWorkers() {
		background.start(func() { a.cluster.RunAsLeader(ctx, "watchlist sync", a.watchlist.Run) })
		background.start(func() { a.cluster.RunAsLeader(ctx, "release calendar", a.calendar.Run) })
	}
	background.start(func() { a.ratings.Run(ctx) })
	if a.prerolls != nil {
		background.start(func() { a.prerolls.Run(ctx) })
	}
	background.start(func() { a.discoveryFeatures.Run(ctx) })
	background.start(func() {
		a.cluster.RunAsLeader(ctx, "scheduled backups", func(ctx context.Context) {
			alerts := &backupAlerts{log: a.log}
			if a.notifications != nil {
				alerts.notify = func(err error) error { return a.notifications.BackupFailed(ctx, err) }
			}
			a.backups.Run(ctx, alerts.observe)
		})
	})
	background.start(func() { a.runUpdates(ctx) })
	background.start(func() { a.runClusterSync(ctx) })
	background.start(func() { a.runBudgets(ctx) })
	background.start(func() { a.cluster.RunAsLeader(ctx, "prowlarr indexer sync", a.runProwlarrSync) })
	background.start(func() { a.runEgress(ctx) })

	front := a.opts.front
	if front == nil {
		ln, err := a.listen(cfg)
		if err != nil {
			return err
		}
		front = newFrontDoor(a.tracker, a.log)
		front.serve(ln)
		defer front.close()
	}
	addr := front.ln.Addr()
	a.frontends.start(ctx, addr)
	defer a.frontends.stop()
	cfg = a.cfg.Get() // the port may have moved off a busy default
	// Publish the actual listener, including automatic port fallback, without
	// exposing the service's protected configuration to the desktop user.
	if err := service.PublishLocalURL(a.opts.DataDir, strings.TrimSuffix(probeURL(addr), "/identity")); err != nil {
		a.log.Warn("publish local tray endpoint", "err", err)
	}
	front.setTLS(a.certs.TLSConfig)
	front.setProxies(a.cfg.TrustedProxyCIDRs)
	front.setRemote(a.cfg.AllowRemoteConsole)
	// A restore applied at this start counts from here: the server is
	// about to serve on it. If it cannot be recorded, nothing is served
	// and Serve puts the original data back.
	if err := a.confirmRestore(); err != nil {
		return err
	}
	front.gate.Open(a.handler)
	a.markPublicReady()
	uiURL := fmt.Sprintf("http://localhost:%d/ether/", cfg.Server.Port)
	a.log.Info("Ether listening", "addr", addr.String(), "ui", uiURL,
		"version", a.opts.Version, "spike", cfg.Spike.Enabled)
	if next := startupNextStep(cfg, uiURL); next != "" {
		a.log.Info(next)
	}
	// Start plex.tv publication and connectivity checks only after the
	// front-end sections are registered and the listener gate is open, so
	// the first servers.xml carries the library list and plex.tv's inbound
	// connectivity probe never races a closed listener.
	if parityRunRegistrar() {
		if a.clustered() {
			background.start(func() { a.runRegistrarAsLeader(ctx) })
			background.start(func() { a.registrar.RunPortMapping(ctx) })
		} else if err := a.registrar.Start(ctx); err != nil {
			a.log.Warn("plex.tv registrar", "err", err)
		}
	}
	if a.registrar != nil {
		a.registrar.ListenerReady()
	}
	background.start(func() { a.checkIntegrityLater(ctx) })
	a.notifyLifecycle("server.started", "Ether is online", cfg.Server.FriendlyName+" is ready for connections.")
	// Hang watchdog: restart via the supervisor if we stop answering.
	watchdogErr := make(chan error, 1)
	go func() {
		watchdog := newWatchdog(probeURL(addr), a.log.With("component", "watchdog"))
		probe := watchdog.probe
		watchdog.probe = func(ctx context.Context) error {
			if err := probe(ctx); err != nil {
				return err
			}
			if err := a.notifications.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("external uptime heartbeat failed")
			}
			return nil
		}
		watchdogErr <- watchdog.run(ctx)
	}()
	var runErr error
	var setupStop <-chan struct{}
	if a.opts.clusterSetup != nil {
		setupStop = a.opts.clusterSetup.stop
	}
	select {
	case <-setupStop:
		runErr = errClusterSetup
	case <-ctx.Done():
	case err := <-front.errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case runErr = <-watchdogErr:
	case runErr = <-a.privateErrors():
	case runErr = <-a.clusterFatal: // nil channel (never ready) when standalone
	}
	if a.deployment != nil {
		a.drainPrivate()
	}
	if runErr == nil {
		a.notifyLifecycle("server.stopping", "Ether is stopping", "The server is shutting down or restarting.")
	}
	// Hand leadership over first: another node takes over singleton work
	// and plex.tv publication while this one finishes shutting down.
	a.stepDownForShutdown()
	// New requests now get 503 and the stopping status while those in
	// flight finish (at most 10s, as before).
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	front.gate.Close(shutdown)
	a.registrar.Stop()
	if runErr != nil {
		return runErr
	}
	return a.runResult()
}

// runQualitySync checks once a minute whether the owner's interval is due
// and syncs imported profiles. The guide clients are shared with the import
// console; LastRun and LastError are written by the sync's own save.
func (a *App) runQualitySync(ctx context.Context) {
	if a.qualitySync == nil {
		return
	}
	var lastAttempt time.Time
	run := func(now time.Time) {
		sync := a.cfg.Get().Quality.Sync
		due := resolver.SyncDue(sync, now)
		if !due {
			// A run where a profile failed still advances LastRun so the
			// cadence stays sane; retry those sooner without hammering the
			// guide every minute.
			retry := sync.Enabled && sync.LastError != "" && (lastAttempt.IsZero() || now.Sub(lastAttempt) >= 15*time.Minute)
			if !retry {
				return
			}
		}
		lastAttempt = now
		report, err := a.qualitySync.Sync(ctx)
		if err != nil {
			if ctx.Err() == nil {
				a.log.Warn("quality profile sync failed", "err", err)
			}
			return
		}
		if len(report.Warnings) > 0 {
			a.log.Warn("quality profile sync skipped some profiles", "warnings", report.Warnings,
				"profilesUpdated", report.ProfilesUpdated)
			return
		}
		a.log.Info("quality profiles synced", "profilesUpdated", report.ProfilesUpdated,
			"formatsAdded", report.FormatsAdded, "formatsUpdated", report.FormatsUpdated,
			"formatsRemoved", report.FormatsRemoved)
	}
	run(time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			run(now)
		}
	}
}

func (a *App) housekeeping(ctx context.Context) {
	pins := time.NewTicker(5 * time.Minute)
	defer pins.Stop()
	imageTimer := time.NewTicker(5 * time.Second)
	defer imageTimer.Stop()
	var imageSweep, receiptSweep headerMaintenance
	// The PMS release check is cluster-wide: leader only. Pins and
	// seen-token flushes are this node's own.
	lastCompat := time.Now()
	if a.cluster.IsLeader() {
		a.refreshCompatibility(ctx)
	}
	_ = a.playback.ReconcilePins(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = a.users.FlushSeen(context.Background())
			return
		case <-imageTimer.C:
			err := imageSweep.pass(ctx, a.cluster.IsLeader(), time.Now, func(ctx context.Context, now time.Time, shard int64) error {
				_, err := identity.PruneImageGrantShard(ctx, a.db, now, shard)
				return err
			})
			if err != nil && ctx.Err() == nil {
				a.log.Warn("image grant maintenance failed", "err", err)
			}
			err = receiptSweep.pass(ctx, a.cluster.IsLeader(), time.Now, func(ctx context.Context, now time.Time, shard int64) error {
				_, err := experience.PruneReceiptShard(ctx, a.db, now, shard)
				return err
			})
			if err != nil && ctx.Err() == nil {
				a.log.Warn("receipt maintenance failed", "err", err)
			}

		case <-pins.C:
			if a.cluster.IsLeader() {
				pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := capability.PruneExpired(pruneCtx, a.db, time.Now())
				cancel()
				if err != nil && ctx.Err() == nil {
					a.log.Warn("ticket maintenance failed", "err", err)
				}
			}
			if a.cluster.IsLeader() && time.Since(lastCompat) > 6*time.Hour {
				lastCompat = time.Now()
				a.refreshCompatibility(ctx)
			}
			_ = a.playback.ReconcilePins(ctx)
			_ = a.users.FlushSeen(ctx)
			if a.support != nil {
				a.support.Expire()
			}
		}
	}
}

// Close releases every component in reverse order of construction.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		if a.stopPrewarm != nil {
			a.stopPrewarm()
		}
		if a.prewarmDone != nil {
			<-a.prewarmDone
		}
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
	})
}

func appendUnique(xs []string, more ...string) []string {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	for _, m := range more {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			xs = append(xs, m)
			seen[m] = true
		}
	}
	return xs
}

// startupNextStep returns a single actionable startup message for the next
// missing configuration step, or "" when setup, TMDB, and a playback source
// are all ready.
func startupNextStep(cfg config.Config, uiURL string) string {
	if !cfg.SetupComplete {
		return fmt.Sprintf("Open %s to finish setup", uiURL)
	}
	if tmdb.ResolveKey(cfg.Catalog) == "" {
		return "Add a TMDB API key in Integrations"
	}
	if !hasPlaybackSource(cfg) {
		return "Add a debrid or Usenet account in Sources"
	}
	return ""
}

func hasPlaybackSource(cfg config.Config) bool {
	for _, p := range cfg.Usenet.Providers {
		if p.Enabled && strings.TrimSpace(p.Host) != "" {
			return true
		}
	}
	for _, p := range cfg.Debrid.Providers {
		if p.Enabled && strings.TrimSpace(p.APIKey) != "" {
			return true
		}
	}
	for _, ix := range cfg.Indexers {
		if ix.Enabled && ix.IsStremio() && strings.TrimSpace(ix.URL) != "" {
			return true
		}
	}
	return false
}
