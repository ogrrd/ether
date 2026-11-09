// Package admin is Ether's owner-facing HTTP surface: the JSON API under
// /ether/api/ and the embedded React SPA (setup wizard, dashboard,
// settings, logs, resolver traces) served under /ether/.
//
// The package deliberately depends only on internal/config, internal/core
// (for types) and the small interfaces/funcs declared in Deps; internal/app
// wires the real implementations. The SPA is embedded in the same binary,
// so every endpoint it calls is always present: the component deps in Deps
// are required, and a handler only reaches the deps its routes use.
//
// Mounting:
//
//	mux.Handle("/ether/", admin.New(admin.Deps{Config: store, ...}))
//
// The handler accepts both "/ether/..." paths and paths already stripped
// of the prefix (http.StripPrefix style).
//
// Logging: pass a shared ring buffer so the Logs page can tail the server:
//
//	buf := admin.NewLogBuffer(2000)
//	logger := slog.New(admin.TeeHandler(slog.NewTextHandler(os.Stderr, nil), buf))
//	h := admin.New(admin.Deps{Config: store, Logs: buf, Logger: logger, ...})
package admin

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/identity"
)

//go:embed all:dist
var distFS embed.FS

// RedactedSecret is the placeholder shown in place of a stored secret.
// A PUT that sends it back unchanged preserves the stored value.
const RedactedSecret = "••••"

// PlexAuth is the plex.tv sign-in/claim surface the wizard needs.
// Implemented by internal/plextv.
type PlexAuth interface {
	// StartPIN begins the PIN flow: id is the poll handle, code is the
	// 4-character code shown to the owner, authURL is the plex.tv page to open.
	StartPIN(ctx context.Context) (id, code, authURL string, err error)
	// PollPIN reports whether the owner has approved the PIN yet. When done
	// is true the implementation has recorded the owner account for Claim.
	PollPIN(ctx context.Context, id string) (done bool, err error)
	// Claim claims this server to the signed-in account.
	Claim(ctx context.Context) error
	// Status returns a JSON-able snapshot (claimed, owner, connections...).
	Status() any
}

// PlexFriendSyncer is an optional capability on PlexAuth for refreshing shared
// Plex users and their server access tokens on demand.
type PlexFriendSyncer interface {
	SyncFriends(ctx context.Context) error
}

// SignIn is a plex.tv PIN sign-in that, unlike PlexAuth, changes nothing on
// the server: it only yields the signed-in account's token so the admin UI
// can authenticate as that account.
type SignIn interface {
	// StartPIN creates a PIN; forwardURL is where plex.tv sends the browser
	// after approval.
	StartPIN(ctx context.Context, forwardURL string) (id, code, authURL string, err error)
	// PINToken returns the account token once approved, "" while pending.
	PINToken(ctx context.Context, id string) (string, error)
}

// Sessions issues and revokes owner console sessions (satisfied by
// *auth.Service). Sign-in hands the browser a session that only opens this
// API, never the owner's plex.tv account token.
type Sessions interface {
	// IssueOwnerSession exchanges an owner credential for a session. It
	// fails with core.ErrUnauthorized unless credential is the owner's.
	IssueOwnerSession(ctx context.Context, credential string) (token string, expires time.Time, err error)
	// RevokeOwnerSession ends a session; other tokens are ignored.
	RevokeOwnerSession(ctx context.Context, token string) error
}

// UserStore is the user-administration surface (satisfied by core.Auth
// implementations; only the two admin methods are required here).
type UserStore interface {
	Users(ctx context.Context) ([]core.User, error)
	UpdateUser(ctx context.Context, u core.User) error
}

// StatusFunc returns a JSON-able status blob for a dashboard card. It may
// return (any, error) shaped values via StatusFuncE; StatusFunc never fails.
type StatusFunc func(ctx context.Context) any

// StatusFuncE is a StatusFunc that can fail.
type StatusFuncE func(ctx context.Context) (any, error)

// Deps are the dependencies of the admin handler. The component
// dependencies (Plex sign-in, testers, traces, users, personalization,
// collections, catalogue helper, image proxy) are required. Only the
// tuning knobs, Logger/Logs, the dashboard status providers, Extensions,
// ValidateExtra and DiskUsage have defaults or may be left empty.
type Deps struct {
	ViewerCatalogRules ViewerCatalogRules
	Frontends          FrontendAdmin
	FavoritesMirror    FavoritesMirror
	NotInterested      NotInterestedPortal
	ViewerAlerts       ViewerAlertPreferences
	ActiveViewer       func(context.Context, int64) (*core.User, error)
	ViewerPortalURL    func(context.Context, *http.Request) (string, error)
	StremioPortalURL   func(context.Context, *http.Request) (string, error)
	CreateDLNAGuest    func(context.Context) (*core.Viewer, error)
	LocalIdentity      *identity.Local
	IdentityGrants     *identity.Grants
	IdentityLimiter    *identity.LoginLimiter
	// OwnerCredentialChanged runs after an owner local sign-in, setup and
	// any local password change (the app refreshes the owner's recovery
	// sign-in from it). Nil does nothing.
	OwnerCredentialChanged func(context.Context)
	PollLimiter            *identity.LoginLimiter
	QuickConnect           *identity.QuickConnect
	Invites                *identity.Invite
	RefreshViewers         func(context.Context) error
	OwnerConfigured        func(context.Context) bool
	LocalOwnerConfigured   func(context.Context) bool
	// Lifecycle backs pause, resume, sign-out, remove and merge for a
	// viewer (satisfied by *auth.Service). Nil answers those routes 503.
	Lifecycle        ViewerLifecycle
	AuthenticatePlex func(context.Context, string) (*core.User, error)
	LinkPlex         func(context.Context, int64, string) error

	// Extensions are owner-authenticated API subtrees keyed by relative path.
	Extensions map[string]http.Handler
	// PublicExtensions are API subtrees that authorise each request
	// themselves (the add-on settings relay takes a short-lived token).
	PublicExtensions map[string]http.Handler
	ValidateExtra    func(config.Config) []FieldError
	// Config is the owner settings store. Required.
	Config *config.Store
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Logs is the in-memory log ring served at /ether/api/logs. A fresh
	// 2000-entry buffer is created when nil (retrieve it via
	// Handler.Logs()); pass a shared one to tail server logs.
	Logs *LogBuffer
	// LogDir holds the on-disk logs offered by GET /api/logs/download;
	// empty offers the in-memory buffer instead.
	LogDir string
	// Support is the in-memory problem-capture recorder. Nil answers the
	// support routes with the same not-found result as an unknown path.
	Support *diagnostics.Recorder
	// Version is reported by /ether/api/bootstrap ("dev" when empty).
	Version string

	// PlexAuth drives wizard step 1 (plex.tv PIN sign-in and claim).
	PlexAuth PlexAuth

	// SignIn powers "Sign in with Plex" on the admin login screen.
	SignIn SignIn

	// IsOwnerToken reports whether a bearer token (an owner credential or
	// a console session) belongs to the owner.
	IsOwnerToken func(ctx context.Context, token string) bool

	// Sessions backs console sign-in and sign-out.
	Sessions Sessions

	// Provider/integration testers used by the wizard and settings pages.
	TestUsenet        func(ctx context.Context, p config.UsenetProvider) error
	TestIndexer       func(ctx context.Context, ix config.Indexer) error
	TestDebrid        func(ctx context.Context, p config.DebridProvider) error
	TestTMDB          func(ctx context.Context, key string) error
	TestOpenSubtitles func(ctx context.Context, cfg config.OpenSubtitles) error
	TestLLM           func(ctx context.Context, cfg config.LLM) error
	// LLMModels lists the models the provider in cfg offers.
	LLMModels func(ctx context.Context, cfg config.LLM) ([]LLMModel, error)
	TestTrakt func(ctx context.Context, cfg config.Trakt) error
	// TestIntroDB probes TheIntroDB with the posted key, or the stored key
	// when the post is empty or still redacted.
	TestIntroDB func(ctx context.Context, apiKey string) error

	// Dashboard status providers. Each non-nil provider appears on the
	// dashboard and at /ether/api/status/{name}.
	SessionsStatus  StatusFunc // "sessions" — now playing
	CacheStatus     StatusFunc // "cache" — usage; {used,budget} renders as a bar
	ProvidersStatus StatusFunc // "providers" — Usenet provider health
	IndexersStatus  StatusFunc // "indexers" — budgets remaining
	CatalogStatus   StatusFunc // "catalog" — catalogue sync/stats
	// PauseTierMigration pauses (true) or resumes the one-off library-tier
	// migration shown in the catalogue status.
	PauseTierMigration func(ctx context.Context, paused bool) error
	RemoteStatus       StatusFunc // "remote" — remote access
	HWAccelStatus      StatusFunc // "hwaccel" — transcoding hardware

	// CacheRestartRequired reports whether the saved folder/storage differs
	// from the running cache, including changes made during initial setup.
	CacheRestartRequired func() bool
	// SystemLoad is this machine's recent CPU, memory, network and GPU
	// load, served on its own route because only the Overview polls it.
	SystemLoad StatusFunc
	// ExtraStatus adds/overrides named status providers generically. The
	// "widget" entry is required: it backs GET /ether/api/status.
	ExtraStatus map[string]StatusFunc

	// DatabaseCheck runs an owner-requested integrity check. Optional for offline consoles.
	DatabaseCheck StatusFuncE
	// DatabaseCompactStatus reports what compacting the database would take
	// and any compaction running; DatabaseCompact starts one. Both optional.
	DatabaseCompactStatus StatusFuncE
	DatabaseCompact       StatusFuncE

	// ResolverTraces backs GET /ether/api/traces.
	ResolverTraces StatusFuncE

	// Users backs the user-administration and viewer endpoints.
	Users UserStore

	// ViewerDirectory lists stored identities for owner administration, including
	// imported or inactive viewers. It must never be used for authentication.
	// Nil preserves the active-user fallback for smaller integrations.
	ViewerDirectory func(context.Context) ([]ViewerRecord, error)

	// TestTimeout bounds each wizard test call (default 20s).
	TestTimeout time.Duration
	// StatusTimeout bounds each status provider call (default 5s).
	StatusTimeout time.Duration

	// DiskUsage overrides free-space reporting (tests). Nil → OS statfs.
	DiskUsage func(path string) (total, free int64, err error)

	// ---- Personalization engine (viewers, Home Designer, insights) ----

	// Personalization backs /api/viewers/*, /api/layout and
	// /api/insights/* (internal/personalize.Engine). Viewers are listed
	// from Users.
	Personalization Personalization

	// Collections backs /api/collections* (Collection Studio).
	Collections CollectionStudio
	// CatalogHelper backs /api/catalog/*.
	CatalogHelper CatalogHelper
	// TitleSchedules backs the catalogue drawer's episode and release tabs;
	// nil disables them.
	TitleSchedules    TitleSchedules
	CatalogVisibility func(context.Context, []core.LibraryExclusion, string) (any, error)
	// WaitingReleaseKeys are in-window movies still hidden from viewers.
	// Nil leaves the waiting catalog filter empty.
	WaitingReleaseKeys func(context.Context) []core.RatingKey
	// Images backs the TMDB image proxy at /api/image.
	Images ImageProxy
	// TitlePosters draws Ether's poster for a title with no artwork, at
	// /api/image/generated. Nil leaves such cards without a poster.
	TitlePosters interface {
		Title(context.Context, artwork.TitlePoster) ([]byte, string, error)
	}

	// PlexWatch is the per-viewer Plex account watch-state integration.
	// Nil hides the routes.
	PlexWatch PlexWatch

	// Prerolls backs /api/admin/prerolls*. Nil hides the routes.
	Prerolls Prerolls

	// PlexServers is watch sync with owner-run Plex Media Servers. Nil
	// hides the routes.
	PlexServers PlexServers
}

// Handler is the admin HTTP handler. Use NewHandler when you need access
// to the log buffer the handler created.
type Handler struct {
	deps           Deps
	log            *slog.Logger
	logs           *LogBuffer
	mux            *http.ServeMux
	version        string
	pins           *pinHandles
	portalPins     *pinHandles
	linkPins       *pinHandles
	authLimit      *authLimiter
	loginBodySlots chan struct{}
}

// NewHandler builds the admin handler. It panics when Deps.Config is nil.
func NewHandler(deps Deps) *Handler {
	if deps.Config == nil {
		panic("admin: Deps.Config is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Logs == nil {
		deps.Logs = NewLogBuffer(2000)
	}
	if deps.TestTimeout <= 0 {
		deps.TestTimeout = 20 * time.Second
	}
	if deps.StatusTimeout <= 0 {
		deps.StatusTimeout = 5 * time.Second
	}
	version := deps.Version
	if version == "" {
		version = "dev"
	}
	h := &Handler{deps: deps, log: deps.Logger, logs: deps.Logs, version: version, pins: newPINHandles(), portalPins: newPINHandles(), linkPins: newPINHandles(), authLimit: newAuthLimiter(), loginBodySlots: make(chan struct{}, 32)}
	h.mux = h.routes()
	return h
}

// Logs returns the log ring served by this handler.
func (h *Handler) Logs() *LogBuffer { return h.logs }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Accept both /ether/... and prefix-stripped paths.
	if p := r.URL.Path; len(p) >= 6 && p[:6] == "/ether" && (len(p) == 6 || p[6] == '/') {
		r2 := r.Clone(r.Context())
		r2.URL.Path = p[6:]
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		// Keep the escaped form in step, or StripPrefix rejects encoded
		// paths such as /api/plugins/studio%3Aid/views/overview.
		if raw := r.URL.RawPath; strings.HasPrefix(raw, "/ether/") {
			r2.URL.RawPath = raw[6:]
		} else {
			r2.URL.RawPath = ""
		}
		r = r2
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	h.frameHeaders(w.Header())
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()
	api := func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, h.requireAuth(fn))
	}
	// Public bootstrap: minimal, non-sensitive; the SPA needs it before auth.
	mux.HandleFunc("GET /api/bootstrap", h.handleBootstrap)
	mux.HandleFunc("GET /api/health", h.handleBootstrap)
	mux.HandleFunc("POST /api/auth/local", h.handleLocalOwnerLogin)
	mux.HandleFunc("POST /api/setup/local", h.handleLocalSetup)
	mux.HandleFunc("POST /api/me/login", h.handleLocalViewerLogin)
	mux.HandleFunc("GET /api/me", h.handleMe)
	mux.HandleFunc("GET /api/me/favorite-episode-alert", h.portalFavoriteEpisodeAlert)
	mux.HandleFunc("POST /api/me/favorite-episode-alert", h.portalFavoriteEpisodeAlert)
	mux.HandleFunc("GET /api/me/favorites-mirror", h.portalFavoritesMirror)
	mux.HandleFunc("POST /api/me/favorites-mirror", h.portalFavoritesMirror)
	mux.HandleFunc("GET /api/me/apps", h.portalConnectApps)
	mux.HandleFunc("POST /api/me/apps/{surface}", h.portalConnectApps)
	mux.HandleFunc("POST /api/me/link/plex", h.limitIdentityAuth(h.handlePlexLinkStart))
	mux.HandleFunc("GET /api/me/link/plex/{handle}", h.limitIdentityPoll(h.handlePlexLinkPoll))
	mux.HandleFunc("POST /api/me/invite", h.handleInviteCreate)
	mux.HandleFunc("POST /api/me/invite/redeem", h.handleInviteRedeem)
	mux.HandleFunc("GET /api/me/identities", h.handleMeIdentities)
	mux.HandleFunc("POST /api/me/password", h.handleMePassword)
	mux.HandleFunc("GET /api/me/devices", h.handleMeDevices)
	mux.HandleFunc("POST /api/me/devices/revoke", h.handleMeRevoke)
	mux.HandleFunc("POST /api/me/pin", h.limitIdentityAuth(h.handlePortalPIN))
	mux.HandleFunc("GET /api/me/pin/{handle}", h.limitIdentityPoll(h.handlePortalPINPoll))
	mux.HandleFunc("POST /api/me/quickconnect", h.handleApproveQuickConnect)
	// Public sign-in: yields a console session only to the browser that
	// started the PIN, and only if the account owns the server.
	mux.HandleFunc("POST /api/auth/pin", h.limitAuth(h.handleSignInStart))
	mux.HandleFunc("GET /api/auth/pin/{handle}", h.limitAuth(h.handleSignInPoll))
	mux.HandleFunc("POST /api/auth/session", h.limitAuth(h.handleSessionCreate))
	mux.HandleFunc("DELETE /api/auth/session", h.limitAuth(h.handleSessionDelete))

	api("POST /api/viewers/{id}/quickconnect", h.handleOwnerQuickConnect)
	api("GET /api/viewers/{id}/not-interested", h.ownerNotInterested)
	api("POST /api/viewers/{id}/not-interested/restore", h.ownerNotInterested)
	api("GET /api/viewers/{id}/favorite-episode-alert", h.ownerFavoriteEpisodeAlert)
	api("GET /api/viewers/{id}/favorites-mirror", h.ownerFavoritesMirror)
	api("POST /api/viewers/{id}/favorites-mirror", h.ownerFavoritesMirror)
	api("GET /api/viewers/{id}/apps", h.ownerConnectApps)
	api("POST /api/viewers/{id}/apps/{surface}", h.ownerConnectApps)
	api("POST /api/frontends/dlna/guest", h.ownerCreateDLNAGuest)
	api("GET /api/frontends", h.frontendList)
	api("GET /api/frontends/qr", h.frontendQR)
	api("GET /api/frontends/{id}/status", h.frontendStatus)
	api("GET /api/frontends/{id}/health", h.frontendHealth)
	api("GET /api/frontends/{id}/settings", h.frontendSettings)
	api("PUT /api/frontends/{id}/settings", h.frontendSettings)
	api("PUT /api/frontends/{id}/enabled", h.frontendToggle)
	api("GET /api/config", h.handleGetConfig)
	api("PUT /api/config", h.handlePutConfig)
	api("GET /api/config/{section}", h.handleGetSection)
	api("PUT /api/config/{section}", h.handlePutSection)
	api("PATCH /api/config/{section}", h.handlePutSection)

	api("GET /api/setup/status", h.handleSetupStatus)
	api("POST /api/setup/complete", h.handleSetupComplete)
	api("POST /api/setup/reopen", h.handleSetupReopen)

	api("POST /api/plex/pin", h.handlePINStart)
	api("GET /api/plex/pin/{id}", h.handlePINPoll)
	api("POST /api/plex/claim", h.handleClaim)
	api("POST /api/plex/friends/sync", h.handleSyncPlexFriends)
	api("GET /api/plex/status", h.handlePlexStatus)

	api("POST /api/test/usenet", h.handleTestUsenet)
	api("POST /api/test/indexer", h.handleTestIndexer)
	api("POST /api/test/debrid", h.handleTestDebrid)
	api("POST /api/test/tmdb", h.handleTestTMDB)
	api("POST /api/test/opensubtitles", h.handleTestOpenSubtitles)
	api("POST /api/test/llm", h.handleTestLLM)
	api("GET /api/llm/providers", h.handleLLMProviders)
	api("POST /api/llm/models", h.handleLLMModels)
	api("POST /api/test/trakt", h.handleTestTrakt)
	api("POST /api/test/introdb", h.handleTestIntroDB)
	api("POST /api/outbound/check", h.handleOutboundCheck)

	api("GET /api/dashboard", h.handleDashboard)
	api("GET /api/system/load", h.handleSystemLoad)
	api("GET /api/status", h.handleWidget)
	api("GET /api/status/{name}", h.handleStatus)
	api("POST /api/status/database/check", h.handleDatabaseCheck)
	api("GET /api/status/database/compact", h.handleDatabaseCompact)
	api("POST /api/status/database/compact", h.handleDatabaseCompact)
	api("GET /api/traces", h.handleTraces)
	api("GET /api/disk", h.handleDisk)

	api("GET /api/users/{id}/identities", h.ownerIdentityManagement)
	api("POST /api/users/{id}/password", h.ownerIdentityManagement)
	api("GET /api/users/{id}/devices", h.ownerIdentityManagement)
	api("POST /api/users/{id}/devices/revoke", h.ownerIdentityManagement)
	api("POST /api/users/{id}/invite", h.ownerIdentityManagement)
	api("POST /api/users/local", h.handleCreateLocalViewer)
	api("POST /api/users/invite", h.handleCreateInviteViewer)
	api("POST /api/users/{id}/pause", h.handlePauseViewer)
	api("POST /api/users/{id}/resume", h.handleResumeViewer)
	api("POST /api/users/{id}/signout", h.handleSignOutViewer)
	api("POST /api/users/{id}/merge", h.handleMergeViewer)
	api("DELETE /api/users/{id}", h.handleRemoveViewer)
	api("GET /api/users", h.handleListUsers)
	api("GET /api/users/{id}", h.handleGetUser)
	api("PUT /api/users/{id}", h.handleUpdateUser)
	api("PATCH /api/users/{id}", h.handleUpdateUser)

	api("GET /api/logs", h.handleLogs)
	api("GET /api/logs/download", h.handleLogsDownload)
	api("DELETE /api/logs", h.handleClearLogs)

	api("GET /api/support/devices", h.handleSupportDevices)
	api("POST /api/support/captures", h.handleSupportCreate)
	api("GET /api/support/captures/{id}", h.handleSupportGet)
	api("POST /api/support/captures/{id}/device", h.handleSupportDevice)
	api("POST /api/support/captures/{id}/finish", h.handleSupportFinish)
	api("DELETE /api/support/captures/{id}", h.handleSupportDiscard)
	api("GET /api/support/captures/{id}/archive", h.handleSupportArchive)

	for path, handler := range h.deps.Extensions {
		prefix := "/api/" + path
		serve := h.requireAuth(http.StripPrefix(prefix, handler).ServeHTTP)
		mux.Handle(prefix+"/", serve)
		// Serve the subtree root directly. ServeMux's slash redirect otherwise
		// loses the outer /ether prefix after the console strips it.
		mux.Handle(prefix, h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
			next := r.Clone(r.Context())
			u := *r.URL
			u.Path = prefix + "/"
			next.URL = &u
			serve(w, next)
		}))
	}
	for path, handler := range h.deps.PublicExtensions {
		prefix := "/api/" + path
		mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	}
	h.personalizationRoutes(api)
	h.collectionRoutes(mux, api)
	h.prerollRoutes(api)
	h.plexServerRoutes(api)

	mux.HandleFunc("/api/", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	}))
	mux.HandleFunc("/", h.handleSPA)
	return mux
}
