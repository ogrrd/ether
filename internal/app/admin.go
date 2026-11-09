package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"github.com/braedonsaunders/ether/internal/trakt"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func (a *App) buildAdmin() http.Handler {
	local, _ := identity.NewLocal(a.db)
	grants, _ := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	limiter, _ := identity.NewLoginLimiter(a.db, 15*time.Minute, 30, 8, nil)
	pollLimiter, _ := identity.NewPollLimiter(a.db, time.Minute, 120, 60, nil)
	quick, _ := identity.NewQuickConnect(a.db, 10*time.Minute, 30*24*time.Hour, nil)
	invites, _ := identity.NewInvite(a.db, 24*time.Hour, 30*24*time.Hour, nil)
	deps := admin.Deps{
		FavoritesMirror:  a.favorites,
		NotInterested:    notInterestedPortal{a},
		ViewerAlerts:     &notify.ViewerAlerts{DB: a.db, Active: a.users.ActiveUser},
		ActiveViewer:     a.users.ActiveUser,
		ViewerPortalURL:  a.frontends.viewerPortalURL,
		StremioPortalURL: a.frontends.viewerPortalSecureURL,
		CreateDLNAGuest:  a.createDLNAGuest,
		Invites:          invites,
		Lifecycle:        a.users,
		LocalOwnerConfigured: func(ctx context.Context) bool {
			owner, err := a.users.Owner(ctx)
			if err != nil {
				return false
			}
			links, err := local.Links(ctx, owner.ID)
			if err != nil {
				return true
			} // fail closed if identity storage is unavailable
			for _, link := range links {
				if link.Provider == "local" {
					return true
				}
			}
			return false
		},
		AuthenticatePlex: a.users.Authenticate, LinkPlex: a.users.LinkPlex, LocalIdentity: local, IdentityGrants: grants, IdentityLimiter: limiter, OwnerCredentialChanged: a.syncOwnerRecovery, PollLimiter: pollLimiter, QuickConnect: quick, RefreshViewers: a.users.RefreshViewers, OwnerConfigured: func(ctx context.Context) bool { _, err := a.users.Owner(ctx); return err == nil },
		Config:           a.cfg,
		Frontends:        a.frontends.control,
		Extensions:       a.extensionHandlers(),
		PublicExtensions: a.publicExtensionHandlers(),
		ValidateExtra:    a.validateExtra,
		Logger:           a.log.With("component", "admin"),
		Logs:             a.logs,
		Support:          a.support,
		LogDir:           filepath.Join(a.opts.DataDir, "logs"),
		Version:          a.opts.Version,
		PlexAuth:         &plexAuth{app: a, pins: map[string]*plextv.PIN{}},
		SignIn:           &plexSignIn{app: a, pins: map[string]*plextv.PIN{}},
		IsOwnerToken: func(ctx context.Context, token string) bool {
			if auth.IsOwnerSession(token) {
				u, err := a.users.OwnerSession(ctx, token)
				return err == nil && u.IsOwner
			}
			u, err := a.users.Authenticate(ctx, token)
			return err == nil && u.IsOwner
		},
		Sessions: a.users,
		TestUsenet: func(ctx context.Context, p config.UsenetProvider) error {
			return usenet.TestProvider(ctx, p)
		},
		TestIndexer: func(ctx context.Context, ix config.Indexer) error {
			if ix.IsStremio() {
				return a.addons.Test(ctx, ix)
			}
			if ix.IsBuiltin() {
				if res := a.sites.Test(ctx, ix, false); !res.OK {
					return errors.New(res.Error)
				}
				return nil
			}
			if a.indexers != nil {
				return a.indexers.TestIndexer(ctx, ix, a.cfg.Get().ClientIdentity)
			}
			return newznab.Test(ctx, ix, a.cfg.Get().ClientIdentity)
		},
		TestDebrid: func(ctx context.Context, p config.DebridProvider) error {
			return a.debrid.Test(ctx, p)
		},
		TestTMDB: func(ctx context.Context, key string) error {
			c := tmdb.New(key, a.log)
			defer c.Close()
			_, err := c.Configuration(ctx)
			return err
		},
		TestOpenSubtitles: subtitles.Test,
		TestTrakt:         trakt.Test,
		TestIntroDB: func(ctx context.Context, apiKey string) error {
			return markers.Probe(ctx, "", apiKey)
		},
		TestLLM: func(ctx context.Context, c config.LLM) error {
			c.Enabled = true // test the settings before the owner switches them on
			_, err := llm.NewClient(llm.ClientOptions{Config: c, Logger: a.log}).Chat(ctx,
				[]llm.Message{{Role: "user", Content: "Reply with the word ok."}}, false)
			return err
		},
		LLMModels: func(ctx context.Context, c config.LLM) ([]admin.LLMModel, error) {
			models, err := llm.NewClient(llm.ClientOptions{Config: c, Logger: a.log}).Models(ctx)
			out := make([]admin.LLMModel, len(models))
			for i, m := range models {
				out[i] = admin.LLMModel{ID: m.ID, Label: m.Label}
			}
			return out, err
		},
		SessionsStatus: func(context.Context) any {
			return ownerSessionStatuses((ownerPlayback{Playback: a.playback, app: a}).Sessions())
		},
		CacheStatus:          a.cacheStatus,
		CacheRestartRequired: a.cacheRestartRequired,
		ProvidersStatus:      func(context.Context) any { return a.fetcher.Stats() },
		IndexersStatus: func(ctx context.Context) any {
			r, err := a.indexers.Remaining(ctx)
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			return r
		},
		CatalogStatus: func(ctx context.Context) any {
			s, err := a.catalog.Stats(ctx)
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			return s
		},
		PauseTierMigration: a.catalog.SetTierMigrationPaused,
		RemoteStatus:       func(context.Context) any { return a.registrar.Status() },
		SystemLoad:         func(context.Context) any { return a.load.Snapshot() },
		HWAccelStatus: func(context.Context) any {
			if a.transcoder == nil {
				return map[string]string{"error": "ffmpeg not found"}
			}
			accel := a.transcoder.HWAccel()
			hw := map[string]any{"hwaccel": accel, "ffmpeg": a.transcoder.Version(), "enhancement": a.transcoder.EnhancementStatus()}
			if vendor := transcode.GPUVendor(accel.Device); vendor != "" {
				hw["vendor"] = vendor
			}
			cap := a.transcoder.Capability()
			if cap.Notice != "" {
				hw["notice"] = cap.Notice
			}
			if cap.PlaybackError != "" {
				hw["playbackError"] = cap.PlaybackError
			}
			if len(cap.ToneMap) > 0 {
				hw["toneMap"] = cap.ToneMap
			}
			if len(cap.Colour) > 0 {
				hw["colour"] = cap.Colour
			}
			hw["filtersProbed"] = cap.FiltersProbed
			return hw
		},
		DatabaseCheck:         a.checkDatabase,
		DatabaseCompactStatus: a.compactionStatus,
		DatabaseCompact:       a.startCompaction,
		ExtraStatus: map[string]admin.StatusFunc{
			"stream":   func(context.Context) any { return a.engine.Stats() },
			"widget":   a.widgetStatus,
			"prefetch": func(context.Context) any { return a.prefetch.Stats() },
			"debrid":   a.debridStatus,
			"sources":  func(context.Context) any { return map[string]any{"startup": a.resolver.SourceStartups()} },
			"database": a.databaseStatus,
			"updates":  a.updateStatus,
			"storage":  a.storageStatus,
		},
		ResolverTraces:     func(context.Context) (any, error) { return a.resolver.Traces(), nil },
		Users:              a.users,
		ViewerDirectory:    a.viewerDirectory,
		Personalization:    a.personalize,
		Collections:        a.collections,
		CatalogHelper:      a.collections,
		ViewerCatalogRules: a.catalog,
		TitleSchedules:     a.catalog,
		WaitingReleaseKeys: func(ctx context.Context) []core.RatingKey {
			if a.availability == nil {
				return nil
			}
			return a.availability.HiddenMovies(ctx)
		},
		CatalogVisibility: func(ctx context.Context, specs []core.LibraryExclusion, fallback string) (any, error) {
			return a.catalog.PreviewVisibilityPolicy(ctx, specs, fallback)
		},
		Images:       a.images,
		TitlePosters: a.titlePosters(),
		PlexWatch:    a.plexWatchAPI(),
		PlexServers:  a.plexServersAPI(),
		Prerolls:     a.prerollAdmin(),
	}
	// Optional role-owned dependencies stay absent until a real local or private
	// service supplies them. This preserves console/auth/config/catalog without
	// constructing provider transports on a server-only node.
	if a.fetcher == nil {
		deps.TestUsenet = nil
		deps.ProvidersStatus = nil
	}
	if a.indexers == nil {
		deps.TestIndexer = nil
		deps.IndexersStatus = nil
	}
	if a.debrid == nil {
		deps.TestDebrid = nil
		delete(deps.ExtraStatus, "debrid")
	}
	if a.cache == nil {
		deps.CacheStatus = nil
		deps.CacheRestartRequired = nil
		delete(deps.ExtraStatus, "storage")
	}
	if a.engine == nil {
		delete(deps.ExtraStatus, "stream")
	}
	if a.playback == nil {
		delete(deps.ExtraStatus, "widget")
	}
	if a.prefetch == nil {
		delete(deps.ExtraStatus, "prefetch")
	}
	if a.resolver == nil {
		deps.ResolverTraces = nil
		delete(deps.ExtraStatus, "sources")
	}
	if a.load == nil {
		deps.SystemLoad = nil
	}
	a.remoteSourceAdminDeps(&deps)
	h := admin.NewHandler(deps)
	if a.streamRT != nil {
		a.streamRT.SetDashboard(h.SnapshotDashboard)
	}
	return sourceOwnerContext(h)
}

func (a *App) plexWatchAPI() admin.PlexWatch {
	if a.plexWatch == nil {
		return nil
	}
	return plexWatchAdmin{a.plexWatch}
}

// debridStatus is provider health, counters and latency EWMAs plus the
// streaming engine's counters.
func (a *App) debridStatus(context.Context) any {
	st := a.debrid.Status()
	es := a.media.Stats()
	st.Engine = &es
	return st
}

func (a *App) cacheStatus(context.Context) any {
	used, budget := a.cache.Usage()
	return map[string]any{"used": used, "budget": budget, "mode": a.cfg.Read().Cache.Mode}
}

// plexAuth adapts the plex.tv PIN sign-in and claim flow to the wizard.
type plexAuth struct {
	app       *App
	mu        sync.Mutex
	pins      map[string]*plextv.PIN
	userToken string
}

func (p *plexAuth) client() *plextv.Client { return p.app.registrar.SignInClient() }

func (p *plexAuth) StartPIN(ctx context.Context) (id, code, authURL string, err error) {
	pin, err := p.client().CreatePIN(ctx)
	if err != nil {
		return "", "", "", err
	}
	id = strconv.FormatInt(pin.ID, 10)
	p.mu.Lock()
	rememberPIN(p.pins, id, pin)
	p.mu.Unlock()
	return id, pin.Code, p.client().AuthURL(pin, ""), nil
}

func (p *plexAuth) PollPIN(ctx context.Context, id string) (bool, error) {
	p.mu.Lock()
	pin := p.pins[id]
	if pin != nil && !pin.ExpiresAt.IsZero() && !time.Now().Before(pin.ExpiresAt) {
		delete(p.pins, id)
		pin = nil
	}
	p.mu.Unlock()
	if pin == nil {
		return false, errors.New("unknown sign-in request; start again")
	}
	got, err := p.client().CheckPIN(ctx, pin.ID, pin.Code)
	if err != nil {
		return false, err
	}
	if got.AuthToken == "" {
		return false, nil
	}
	acct, err := p.client().User(ctx, got.AuthToken)
	if err != nil {
		return false, fmt.Errorf("plex.tv account: %w", err)
	}
	ownerRejected := false
	if err := p.app.cfg.Update(func(c *config.Config) error {
		if c.Plex.Claimed && c.Plex.OwnerPlexID != 0 && c.Plex.OwnerPlexID != acct.ID {
			ownerRejected = true
			return errors.New("this Plex account does not own the server; sign in as the existing owner")
		}
		if !c.Plex.Claimed {
			c.Plex.Token = ""
		}
		c.Plex.OwnerPlexID = acct.ID
		c.Plex.OwnerUsername = acct.Username
		return nil
	}); err != nil {
		if ownerRejected {
			p.mu.Lock()
			delete(p.pins, id)
			p.mu.Unlock()
		}
		return false, err
	}
	p.mu.Lock()
	p.userToken = got.AuthToken
	delete(p.pins, id)
	p.mu.Unlock()
	return true, p.app.users.BootstrapOwner(ctx, p.app.cfg.Get().Plex)
}

func (p *plexAuth) Claim(ctx context.Context) error {
	p.mu.Lock()
	tok := p.userToken
	p.mu.Unlock()
	if tok == "" {
		tok = p.app.cfg.Get().Plex.Token
	}
	if tok == "" {
		return errors.New("sign in to plex.tv first")
	}
	if _, err := p.app.registrar.Claim(ctx, tok); err != nil {
		return err
	}
	p.mu.Lock()
	if p.userToken == tok {
		p.userToken = ""
	}
	p.mu.Unlock()
	return p.app.users.BootstrapOwner(ctx, p.app.cfg.Get().Plex)
}

func (p *plexAuth) Status() any { return p.app.registrar.Status() }

func (p *plexAuth) SyncFriends(ctx context.Context) error {
	return p.app.registrar.SyncFriendsNow(ctx)
}

// plexSignIn is the admin login: a plex.tv PIN whose token the admin
// handler accepts only if it belongs to the owner. Unlike plexAuth it
// changes no settings.
type plexSignIn struct {
	app  *App
	mu   sync.Mutex
	pins map[string]*plextv.PIN
}

func (p *plexSignIn) StartPIN(ctx context.Context, forwardURL string) (id, code, authURL string, err error) {
	c := p.app.registrar.SignInClient()
	pin, err := c.CreatePIN(ctx)
	if err != nil {
		return "", "", "", err
	}
	id = strconv.FormatInt(pin.ID, 10)
	p.mu.Lock()
	rememberPIN(p.pins, id, pin)
	p.mu.Unlock()
	return id, pin.Code, c.AuthURL(pin, forwardURL), nil
}

func (p *plexSignIn) PINToken(ctx context.Context, id string) (string, error) {
	// AuthToken is written under mu by a concurrent poll; read it there too.
	p.mu.Lock()
	pin := p.pins[id]
	var token string
	if pin != nil {
		token = pin.AuthToken
	}
	p.mu.Unlock()
	if pin == nil || (!pin.ExpiresAt.IsZero() && time.Now().After(pin.ExpiresAt)) {
		return "", errors.New("unknown sign-in request")
	}
	if token != "" {
		return token, nil
	}
	got, err := p.app.registrar.SignInClient().CheckPIN(ctx, pin.ID, pin.Code)
	if err != nil {
		return "", err
	}
	if got.AuthToken != "" {
		p.mu.Lock()
		pin.AuthToken = got.AuthToken
		p.mu.Unlock()
	}
	return got.AuthToken, nil
}

// rememberPIN bounds abandoned sign-ins even if the upstream omits an expiry.
// The caller holds the map's mutex. Discard the oldest pending request when
// the cap is reached; its browser can start a new sign-in.
func rememberPIN(pins map[string]*plextv.PIN, id string, pin *plextv.PIN) {
	now := time.Now()
	deadline := now.Add(10 * time.Minute)
	if pin.ExpiresAt.IsZero() || pin.ExpiresAt.After(deadline) {
		pin.ExpiresAt = deadline
	}
	for k, v := range pins {
		if v == nil || (!v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt)) {
			delete(pins, k)
		}
	}
	const maxPINs = 256
	for len(pins) >= maxPINs {
		oldest := ""
		var at time.Time
		for k, v := range pins {
			if oldest == "" || v.ExpiresAt.Before(at) {
				oldest, at = k, v.ExpiresAt
			}
		}
		delete(pins, oldest)
	}
	pins[id] = pin
}

// Keep historical/imported identities visible without changing active membership.
func (a *App) viewerDirectory(ctx context.Context) ([]admin.ViewerRecord, error) {
	stored, err := a.users.AllUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]admin.ViewerRecord, 0, len(stored))
	for _, u := range stored {
		out = append(out, admin.ViewerRecord{User: u.User, Active: u.Active, Origin: u.Origin, Home: u.Home, OwnerEnabled: u.OwnerEnabled, PlexShared: u.PlexShared, HasPlex: u.HasPlex})
	}
	return out, nil
}
