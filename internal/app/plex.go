package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/updater"
)

// buildPlex assembles the host services for the PMS-compatible front end. In
// spike mode the library is the configured local files instead of the TMDB
// catalogue, but auth, plex.tv registration and the transcoder are the real
// ones, so the spike exercises the same path official apps will use.
func (a *App) buildPlex(ctx context.Context, cfg config.Config) (frontend.PlexDeps, error) {
	a.plexBuildMu.Lock()
	defer a.plexBuildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return frontend.PlexDeps{}, err
	}
	tc := a.activeTranscoder()
	identity := plex.Identity{
		MachineIdentifier: cfg.Server.MachineIdentifier,
		FriendlyName:      cfg.Server.FriendlyName,
		Version:           plextv.DefaultServerVersion,
		Platform:          plexPlatform(),
	}

	var deps plex.Deps
	if cfg.Spike.Enabled {
		deps = a.spikeLibrary.Deps()
		deps.Auth = a.users
		if tc != nil {
			deps.Transcoder = tc
		}
	} else {
		playback := core.Playback(a.playback)
		if a.brokerPlayback != nil {
			playback = a.brokerPlayback
		}
		deps = plex.Deps{
			Catalog:     a.catalog,
			Collections: a.discoveryPersonal.ReviewCollections(a.collections),
			VibeSearch:  a.collections,
			Recommender: a.personalize,
			State:       a.state,
			Playlists:   a.playlists,
			Auth:        a.users,
			Playback:    playback,
			Transcoder:  tc,
			DB:          a.db, // plex_ultrablur
		}
	}
	if !cfg.Spike.Enabled && a.availability != nil {
		deps.Catalog = &availabilityCatalog{Catalog: a.catalog, availability: a.availability}
		deps.FilterAvailable = a.availability.FilterDiscovery
		deps.JudgedOnFacts = a.availability.JudgedOnFacts
	}
	if !cfg.Spike.Enabled {
		deps.Search = a.searchSources()
	}
	deps.PrioritizeAvailable = func(ctx context.Context, items []core.Title) []core.Title {
		if a.availability == nil {
			return items
		}
		return a.availability.Prioritize(ctx, items)
	}
	deps.Variants = a.resolver // "Play Version" release picker (cache-only)
	deps.PublicURL = func() string {
		if a.registrar == nil {
			return ""
		}
		return publicPlexOrigin(a.registrar.Status().Connections)
	}
	deps.RepoFileURL = repoFileURL(sourceRef(updater.Version, updater.Commit, buildRevision()))
	deps.DecisionTraces = a.resolver // latest resolution, for the support capture
	clientID := func() string {
		cfg := a.cfg.Read()
		return plexguid.ClientIdentifier(cfg.Plex.ClientIdentifier, cfg.Server.MachineIdentifier)
	}
	plexToken := func() string {
		cfg := a.cfg.Read()
		if !cfg.Plex.Claimed || (cfg.Frontends != nil && !cfg.Frontends["plex"].Enabled) {
			return ""
		}
		return cfg.Plex.Token
	}
	guidService := plexguid.New(a.db, plexguid.Options{
		HTTP:             a.opts.plextvHTTP,
		Token:            plexToken,
		ClientIdentifier: clientID,
		Background:       true,
	})
	a.plexGUID, deps.LibraryGUIDs = guidService, guidService
	metadataService := plexmeta.New(a.db, plexmeta.Options{
		HTTP:             a.opts.plextvHTTP,
		Token:            plexToken,
		ClientIdentifier: clientID,
		GUID: func(ctx context.Context, k core.RatingKey) string {
			if deps.LibraryGUIDs == nil {
				return ""
			}
			return deps.LibraryGUIDs.Cached(ctx, k)
		},
		Enabled: func() bool {
			cfg := a.cfg.Read()
			return cfg.Plex.Claimed && cfg.Plex.UsePlexMetadata && (cfg.Frontends == nil || cfg.Frontends["plex"].Enabled)
		},
		Background: true,
	})
	a.plexMeta, deps.LibraryMetadata = metadataService, metadataService
	if err := a.buildPlexWatch(); err != nil {
		guidService.Close()
		metadataService.Close()
		return frontend.PlexDeps{}, fmt.Errorf("plex watch state: %w", err)
	}
	if !cfg.Spike.Enabled && a.episodeorder != nil {
		// EpisodeOrder is TMDB-backed (build_integrations.go): metadataTitle
		// prefers it over Catalog, and the real catalog does not know the
		// spike catalog's KindLocal keys, so wiring it in spike mode makes
		// every /library/metadata/<KindLocal> 404 while hubs and play
		// queues (which read Catalog directly) keep working.
		deps.EpisodeOrder = a.episodeorder
	}
	deps.Discovery = a.discoveryFeatures.Frontend()
	deps.Subtitles = plexSubtitles{a}
	deps.TrackPreferences = a.trackprefs
	deps.SessionCoordination = a.sessionCoordination
	deps.SessionStores = sessions.NewSessionStores()
	deps.Experience = experience.NewSelectors()
	deps.Markers = a.markers
	// The remuxer is built once in buildIntegrations and shared with every
	// front end.
	deps.Downloads = a.downloadRemuxer
	deps.Extras = newPlexExtras(a.extras)
	deps.Prerolls = a.prerollProvider()
	deps.Trickplay = a.trickplay.Borrow()
	a.wirePlexPolicy(&deps)
	deps.FavoriteShows = a.favoriteShows
	deps.Now = parityClock()
	deps.Config = a.cfg.Read
	deps.ApplyClientRules = resolver.ApplyClientRulesForMedia
	deps.HTTPClient = egress.Client(egress.CapMetadata, 20*time.Second)
	deps.DirectHTTP = &http.Client{Transport: egress.Transport(egress.CapDirect)}
	deps.Logger = a.log.With("component", "plex")
	deps.TracePlayback = a.tracePlayback
	deps.Identity = identity
	deps.OwnerSubscription = func() (bool, []string) {
		st := a.registrar.Status()
		return st.PlexPass, st.OwnerFeatures
	}
	deps.LibraryTitles = a.libraryTitles
	if a.stats != nil {
		deps.Dashboard = a.stats.Dashboard()
	}
	if a.hostResources != nil {
		deps.Resources = a.hostResources
	}
	if node := a.clusterNode(); node != nil && deps.DB != nil {
		deps.Cluster = node
	}
	if !cfg.Spike.Enabled && a.db != nil {
		db := a.db
		deps.HomeCache = &plex.HomeCacheSource{
			Revision: func(ctx context.Context, u *core.User) (experience.HomeRevision, error) {
				return experience.ReadHomeRevision(ctx, db, u)
			},
			NoProviderWait: func(ctx context.Context) context.Context { return catalog.WithReadWaitBudget(ctx, 0) },
		}
		if a.catalog != nil {
			deps.HomeCache.Catalog = a.catalog.WriteGeneration
		}
		if a.cfg != nil {
			deps.HomeCache.Config = a.cfg.Generation
		}
		if a.personalize != nil {
			deps.HomeCache.Layout = a.personalize.LayoutVersion
		}
	}
	return deps, nil
}

func (a *App) bindPlexServer(server frontend.PlexServer, spikeEnabled bool) {
	srv, ok := server.(*plex.Server)
	if !ok || srv == nil {
		return
	}
	a.plexBuildMu.Lock()
	a.plexServer = srv
	guidService, metadataService := a.plexGUID, a.plexMeta
	// Frontend unmount closes the server and the GUID worker. Server.Close is
	// idempotent, so the app closer may run as well. Watch refresh stays on
	// the Plex mount (ExplicitWatchOnly in spike), not a second subscription.
	a.plexClose = func() {
		_ = srv.Close()
		if guidService != nil {
			guidService.Close()
		}
		if metadataService != nil {
			metadataService.Close()
		}
	}
	a.closers = append(a.closers, func() { _ = srv.Close() })
	a.plexBuildMu.Unlock()

	if !spikeEnabled && a.db != nil && a.state != nil {
		// Rebuild the viewer's cached Home behind playback, so the Home a
		// client opens after it stops is already current.
		unsub := a.state.Subscribe(func(ev state.Event) {
			switch ev.Kind {
			case state.EventProgress:
				if ev.State != "stopped" {
					return
				}
			case state.EventCompleted, state.EventScrobble, state.EventUnscrobble, state.EventRated:
			default:
				return
			}
			srv.ViewerHomeChanged(ev.UserID)
		})
		go func() {
			<-srv.Done()
			unsub()
		}()
	}
	if a.livetv != nil {
		a.livetv.SetOnChange(srv.NotifyLiveLineup)
	}
	a.publishLibrarySections()
}

// publishLibrarySections pushes the live Movies / TV Shows identities
// (including configured titles) to plex.tv account registration.
func (a *App) publishLibrarySections() {
	if a.plexServer == nil || a.registrar == nil {
		return
	}
	libraries := make([]plextv.LibrarySection, 0)
	for _, sec := range a.plexServer.LibrarySections() {
		libraries = append(libraries, plextv.LibrarySection{
			Key: sec.ID, Type: string(sec.Type), Title: sec.Title,
			UUID: sec.UUID, Language: sec.Language,
		})
	}
	a.registrar.SetLibrarySections(libraries)
}

func plexPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOSX"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// wirePlexPolicy supplies the same safety service to every Plex deployment.
func (a *App) wirePlexPolicy(deps *plex.Deps) {
	deps.AccessPolicy = a.safety
}

// currentPlexServer publishes lazy construction to playback callbacks. The
// mutex is released before callers invoke the adapter or shared playback.
func (a *App) currentPlexServer() *plex.Server {
	a.plexBuildMu.Lock()
	defer a.plexBuildMu.Unlock()
	return a.plexServer
}

// publicPlexOrigin is the origin of the first published public connection
// (secure first): a public IP or a plex.direct name for one. Names that would
// need a DNS lookup are skipped; this runs on request paths.
func publicPlexOrigin(connections []remote.Connection) string {
	for _, secure := range []bool{true, false} {
		for _, c := range connections {
			if c.Local || c.Relay {
				continue
			}
			u, err := url.Parse(c.URI)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme == "https") != secure || (u.Scheme != "https" && u.Scheme != "http") {
				continue
			}
			host := u.Hostname()
			ip := net.ParseIP(host)
			if ip == nil {
				ip, _, _ = remote.ParsePlexDirectHost(host)
			}
			if remote.IsPublicIP(ip) {
				return u.Scheme + "://" + u.Host
			}
		}
	}
	return ""
}

// repoFileURL serves files committed to Ether's public repository through
// jsDelivr, pinned to ref so a published URL never changes under a release.
func repoFileURL(ref string) func(string) string {
	return func(path string) string {
		if ref == "" {
			return ""
		}
		return "https://cdn.jsdelivr.net/gh/braedonsaunders/ether@" + ref + "/" + strings.TrimPrefix(path, "/")
	}
}

var commitRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// sourceRef names the source this build came from: a release's tag
// ("v0.3.6"), else the commit a release candidate, development, PR or local
// build was made from. "" when neither is known.
func sourceRef(version, commit, revision string) string {
	// Release candidates (0.3.7-rc.3) are not tagged on GitHub: only a
	// release version names a tag; anything else is pinned to its commit.
	if v := strings.TrimSpace(version); v != "dev" && !strings.ContainsAny(v, "+/ -") {
		if _, err := updater.ParseVersion(v); err == nil {
			return "v" + strings.TrimPrefix(v, "v")
		}
	}
	for _, c := range []string{commit, revision} {
		if c = strings.ToLower(strings.TrimSpace(c)); commitRef.MatchString(c) {
			return c
		}
	}
	return ""
}

// buildRevision is the VCS commit Go stamped into this binary, if any.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
