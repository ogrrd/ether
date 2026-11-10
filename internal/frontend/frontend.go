package frontend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// ConnectStep identifies a translated owner-console connection instruction.
type ConnectStep string

// CertifiedApp contains only verified native clients, with their connection steps.
type CertifiedApp struct {
	Name  string        `json:"name"`
	Steps []ConnectStep `json:"steps"`
}

type Descriptor struct {
	ID             Protocol       `json:"id"`
	Name           string         `json:"name"`
	Logo           string         `json:"logo,omitempty"`
	Version        string         `json:"version,omitempty"`
	DocsURL        string         `json:"docsUrl,omitempty"`
	DefaultPort    int            `json:"defaultPort,omitempty"`
	Clients        []string       `json:"clients,omitempty"`
	CertifiedApps  []CertifiedApp `json:"certifiedApps,omitempty"`
	SettingsSchema any            `json:"settingsSchema,omitempty"`
}
type ListenerSpec struct {
	Address    string `json:"address"`
	PathPrefix string `json:"pathPrefix,omitempty"`
	TLS        bool   `json:"tls"`
	// Shared means the app supplies this listener; the manager must not bind it.
	Shared bool `json:"shared,omitempty"`
}
type Status struct {
	Mounted          bool      `json:"mounted"`
	Addresses        []string  `json:"addresses"`
	ConnectURLs      []string  `json:"connectUrls,omitempty"`
	LAN              string    `json:"lan"`
	LANReason        string    `json:"lanReason,omitempty"`
	LANCheckedAt     time.Time `json:"lanCheckedAt,omitzero"`
	WAN              string    `json:"wan"`
	WANReason        string    `json:"wanReason,omitempty"`
	WANAddress       string    `json:"wanAddress,omitempty"`
	WANCheckedAt     time.Time `json:"wanCheckedAt,omitzero"`
	Clients24h       int       `json:"clients24h"`
	Clients24hKnown  bool      `json:"clients24hKnown"`
	LiveStreams      int       `json:"liveStreams"`
	LiveStreamsKnown bool      `json:"liveStreamsKnown"`
	TargetVersion    string    `json:"targetVersion"`
	Conformance      string    `json:"conformance"`
	Error            string    `json:"error,omitempty"`
	Warnings         []string  `json:"warnings,omitempty"`
}
type Announcer interface {
	Start(context.Context) error
	Close() error
}
type Frontend interface {
	Describe() Descriptor
	Mount(context.Context, Host) (Instance, error)
}

// SettingsValidator lets a frontend validate its own protocol-specific options.
// The host also checks common listener options before persistence.
type SettingsValidator interface{ ValidateSettings(Settings) error }

type Instance interface {
	Handler() http.Handler
	Listeners() []ListenerSpec
	Discovery() []Announcer
	Status(context.Context) Status
	Close(context.Context) error
}

// SurfaceIdentity is bound to one protocol by the host factory. Quick Connect
// approval requires a viewer obtained from a successfully authenticated session;
// frontends must not construct a viewer from untrusted request data.
type SurfaceIdentity interface {
	Resolve(context.Context, string) (*core.Viewer, error)
	// ViewerTimes revalidates the surface credential and allows only the same
	// viewer or an active owner to read another active viewer's timestamps.
	ViewerTimes(context.Context, string, int64) (ViewerTimes, error)
	PublicViewers(context.Context) ([]PublicViewer, error)
	SignIn(context.Context, string, string, core.Device) (string, error)
	IssueRefresh(context.Context, string, core.Device) (string, error)
	Refresh(context.Context, string, core.Device) (string, string, error)
	QuickConnectInitiate(context.Context, core.Device) (code, secret string, err error)
	QuickConnectStatus(context.Context, string) (approved bool, err error)
	QuickConnectDetails(context.Context, string) (QuickConnectDetails, error)
	QuickConnectPoll(context.Context, string) (string, error)
	QuickConnectApprove(context.Context, *core.Viewer, string) error
	Revoke(context.Context, string) error
	SetupComplete() bool
	Accept(context.Context, string) (*core.Viewer, CredentialAcceptance, error)
	ReceiptCurrent(context.Context, CredentialAcceptance) error
	IssueDelegation(context.Context, int64) (string, error)
	ActiveUser(context.Context, int64) (*core.User, error)
	Users(context.Context) ([]core.User, error)
}

type quietIdentityKey struct{}

// WithQuietIdentity tells credential resolution to skip activity writes.
// Sign-in uses it so the response is not waiting on viewer-times bookkeeping.
func WithQuietIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, quietIdentityKey{}, true)
}

// QuietIdentity reports whether activity writes should be skipped.
func QuietIdentity(ctx context.Context) bool {
	quiet, _ := ctx.Value(quietIdentityKey{}).(bool)
	return quiet
}

// ViewerTimes contains observed events; a zero time means unknown.
type ViewerTimes struct {
	LastLogin, LastActivity time.Time
}

// QuickConnectDetails is returned only to the holder of the polling secret.
type QuickConnectDetails struct {
	Approved     bool
	Code, Secret string
	Device       core.Device
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type clientIPKey struct{}

// WithClientIP supplies the verified network peer for authentication rate
// limits. A frontend must derive it from the transport and trusted proxies,
// never an untrusted forwarding header. Missing or invalid addresses fail closed.
func WithClientIP(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip.Unmap())
}

// ClientIP returns the verified peer carried by WithClientIP.
func ClientIP(ctx context.Context) (netip.Addr, bool) {
	ip, ok := ctx.Value(clientIPKey{}).(netip.Addr)
	return ip, ok && ip.IsValid()
}

// BindIdentity prevents a frontend from choosing a different authentication surface.
func BindIdentity(resolver core.Identity, surface Protocol) SurfaceIdentity {
	return boundIdentity{resolver: resolver, surface: surface}
}

type boundIdentity struct {
	resolver core.Identity
	surface  Protocol
}

func (b boundIdentity) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if b.resolver == nil || b.surface == "" {
		return nil, errors.New("identity surface unavailable")
	}
	return b.resolver.Resolve(ctx, string(b.surface), credential)
}

func (boundIdentity) ViewerTimes(context.Context, string, int64) (ViewerTimes, error) {
	return ViewerTimes{}, ErrIdentityUnavailable
}

func (boundIdentity) IssueRefresh(context.Context, string, core.Device) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundIdentity) Refresh(context.Context, string, core.Device) (string, string, error) {
	return "", "", ErrIdentityUnavailable
}

// ErrIdentityUnavailable means this host does not implement credential issuance.
var ErrIdentityUnavailable = errors.New("frontend identity operation unavailable")

// ErrRateLimited means the verified network peer has exceeded an identity limit.
var ErrRateLimited = errors.New("frontend identity rate limited")

// ErrQuickConnectPending means the code has not yet been approved.
var ErrQuickConnectPending = errors.New("frontend quick connect pending")

// BindIdentity alone binds resolution; app-hosted implementations additionally
// provide credential issuance and Quick Connect operations.
func (boundIdentity) PublicViewers(context.Context) ([]PublicViewer, error) {
	return nil, ErrIdentityUnavailable
}
func (boundIdentity) SignIn(context.Context, string, string, core.Device) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundIdentity) QuickConnectInitiate(context.Context, core.Device) (string, string, error) {
	return "", "", ErrIdentityUnavailable
}
func (boundIdentity) QuickConnectStatus(context.Context, string) (bool, error) {
	return false, ErrIdentityUnavailable
}
func (boundIdentity) QuickConnectDetails(context.Context, string) (QuickConnectDetails, error) {
	return QuickConnectDetails{}, ErrIdentityUnavailable
}
func (boundIdentity) QuickConnectPoll(context.Context, string) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundIdentity) QuickConnectApprove(context.Context, *core.Viewer, string) error {
	return ErrIdentityUnavailable
}
func (boundIdentity) Revoke(context.Context, string) error { return ErrIdentityUnavailable }
func (boundIdentity) SetupComplete() bool                  { return false }
func (b boundIdentity) Accept(ctx context.Context, token string) (*core.Viewer, CredentialAcceptance, error) {
	u, err := b.Resolve(ctx, token)
	return u, CredentialAcceptance{}, err
}
func (boundIdentity) ReceiptCurrent(context.Context, CredentialAcceptance) error {
	return core.ErrUnauthorized
}
func (boundIdentity) IssueDelegation(context.Context, int64) (string, error) {
	return "", core.ErrUnauthorized
}
func (boundIdentity) ActiveUser(context.Context, int64) (*core.User, error) {
	return nil, ErrIdentityUnavailable
}
func (boundIdentity) Users(context.Context) ([]core.User, error) {
	return nil, ErrIdentityUnavailable
}

// BindAuth adapts a core.Auth into a SurfaceIdentity for standalone servers and tests.
func BindAuth(auth core.Auth) SurfaceIdentity {
	if auth == nil {
		return nil
	}
	return boundAuth{auth: auth}
}

type boundAuth struct {
	auth core.Auth
}

func (b boundAuth) UnwrapAuth() core.Auth { return b.auth }

func (b boundAuth) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if b.auth == nil {
		return nil, core.ErrUnauthorized
	}
	return b.auth.Authenticate(ctx, credential)
}

func (b boundAuth) Accept(ctx context.Context, token string) (*core.Viewer, CredentialAcceptance, error) {
	if b.auth == nil {
		return nil, CredentialAcceptance{}, core.ErrUnauthorized
	}
	if acc, ok := b.auth.(CredentialAcceptor); ok {
		return acc.Accept(ctx, token)
	}
	u, err := b.auth.Authenticate(ctx, token)
	return u, CredentialAcceptance{}, err
}

func (b boundAuth) ReceiptCurrent(ctx context.Context, receipt CredentialAcceptance) error {
	if acc, ok := b.auth.(CredentialAcceptor); ok {
		return acc.ReceiptCurrent(ctx, receipt)
	}
	return core.ErrUnauthorized
}

func (b boundAuth) IssueDelegation(ctx context.Context, userID int64) (string, error) {
	if del, ok := b.auth.(DelegationAuth); ok {
		return del.IssueDelegation(ctx, userID)
	}
	return "", core.ErrUnauthorized
}

func (b boundAuth) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	if b.auth == nil {
		return nil, core.ErrUnauthorized
	}
	if a, ok := b.auth.(ActiveUserSource); ok {
		return a.ActiveUser(ctx, id)
	}
	users, err := b.auth.Users(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, core.ErrNotFound
}

func (b boundAuth) Users(ctx context.Context) ([]core.User, error) {
	if b.auth == nil {
		return nil, nil
	}
	return b.auth.Users(ctx)
}

func (boundAuth) ViewerTimes(context.Context, string, int64) (ViewerTimes, error) {
	return ViewerTimes{}, ErrIdentityUnavailable
}
func (boundAuth) PublicViewers(context.Context) ([]PublicViewer, error) {
	return nil, ErrIdentityUnavailable
}
func (boundAuth) SignIn(context.Context, string, string, core.Device) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundAuth) IssueRefresh(context.Context, string, core.Device) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundAuth) Refresh(context.Context, string, core.Device) (string, string, error) {
	return "", "", ErrIdentityUnavailable
}
func (boundAuth) QuickConnectInitiate(context.Context, core.Device) (string, string, error) {
	return "", "", ErrIdentityUnavailable
}
func (boundAuth) QuickConnectStatus(context.Context, string) (bool, error) {
	return false, ErrIdentityUnavailable
}
func (boundAuth) QuickConnectDetails(context.Context, string) (QuickConnectDetails, error) {
	return QuickConnectDetails{}, ErrIdentityUnavailable
}
func (boundAuth) QuickConnectPoll(context.Context, string) (string, error) {
	return "", ErrIdentityUnavailable
}
func (boundAuth) QuickConnectApprove(context.Context, *core.Viewer, string) error {
	return ErrIdentityUnavailable
}
func (boundAuth) Revoke(context.Context, string) error { return ErrIdentityUnavailable }
func (boundAuth) SetupComplete() bool                  { return false }

type Settings map[string]any

// AdvertisedName is the household name a frontend shows. The app fills
// serverName from the general server name; a frontend does not keep its own.
func AdvertisedName(settings Settings) string {
	if name, ok := settings["serverName"].(string); ok {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return "Ether"
}

// Subscriber delivers events across nodes until the context is cancelled.
type Subscriber interface {
	Subscribe(context.Context, EventScope) (<-chan Event, error)
}

// Images serves TMDB artwork via GET or HEAD with query parameters path and w.
// Frontends must authorize access to the item before calling it.
type Images interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}
type Host struct {
	// Support is an owner-initiated, bounded capture; nil disables recording.
	Support            *diagnostics.Recorder
	Experience         Experience
	ViewerCapabilities ViewerCapabilitiesFunc
	ViewerPolicy       ViewerPolicy
	Capabilities       Capabilities
	Preferences        Preferences
	TrackPreferences   TrackPreferenceService
	LiveTV             LiveTV
	VirtualPaths       VirtualPaths
	Sessions           Sessions
	LastPlayedSource   LastPlayedSourceReader
	SourceOffers       SourceOffers
	VersionHandles     VersionHandles
	// Editions lists locally probed copies of one title. It is empty when the
	// process has no local media manifest, and it must not search the network.
	Editions    Editions
	LazyStreams LazyStreams
	LANStreams  LANLazyStreams
	Identity    SurfaceIdentity
	IDs         ids.Codecs
	Events      Subscriber
	Tickets     Tickets
	// WatchParty is the shared Silo and Jellyfin room. Nil leaves both off.
	WatchParty  WatchParty
	Images      Images
	ImageGrants ImageGrants
	// Artwork serves the same composited poster bytes the other frontends
	// grant. DLNA has no credential, so it reads this after LANViewer.
	Artwork Artwork
	// ExtraStreams opens a playable trailer or extra. Nil means extras are not
	// configured; listing routes still authorize the parent and return nothing.
	ExtraStreams ExtraStreams
	// DirectProxy selects the proxy for trailer fetches. Nil follows the
	// process environment. The app wires the direct route so frontends do
	// not import the egress policy.
	DirectProxy func(*http.Request) (*url.URL, error)
	// Cinema lists preroll trailers of other movies. Nil means cinema intros
	// are not wired; the intros route returns an empty page after authorization.
	Cinema CinemaIntros
	// Prerolls lists the owner's pre-roll as an intro. Nil means pre-rolls
	// are off and intros are unchanged.
	Prerolls Prerolls
	// Subtitles searches and downloads a sidecar only after an explicit pick.
	// Nil or Enabled false tells clients the server will not fetch one.
	Subtitles SubtitleCatalog
	Config    func() Settings
	// MetadataLanguage is the catalog language (config catalog.language).
	// Empty skips that step of the no-list audio chain.
	MetadataLanguage func() string
	// AudioOrder is the shared no-choice language chain. Frontends call it
	// instead of importing the track-preference package. Nil keeps the
	// caller's own fallback.
	AudioOrder func(audio []string, server, original string, preferOriginal bool) []string
	// PickAudio returns a stream position in MediaInfo, or -1 when no audio
	// stream exists. Nil keeps the caller's own fallback.
	PickAudio func(MediaInfo, []string) int
	Log       *slog.Logger
	// ViewerPortalURL returns a reachable owner-selected viewer portal URL for this client.
	ViewerPortalURL func(context.Context, *http.Request) (string, error)
	// SecurePortalURL selects a verified reachable HTTPS portal address.
	SecurePortalURL func(context.Context, *http.Request) (string, error)
	// SurfaceURL returns a reachable, verified base for this frontend's routes.
	SurfaceURL func(context.Context, *http.Request) (string, error)
	// LANViewer resolves the owner-selected active DLNA viewer only after
	// trusted-proxy-aware network classification; other surfaces receive nil.
	LANViewer func(context.Context, *http.Request) (*Viewer, error)
	// ClientSeen reports an authenticated client's protocol-specific device ID.
	ClientSeen func(deviceID string)
	// ClientIP resolves the transport peer using the configured trusted proxies.
	ClientIP func(*http.Request) (netip.Addr, bool)
	// PageFreshness invalidates cached browse bodies. Nil disables the cache.
	PageFreshness func() PageFreshness
	// CacheDir is a node-local directory this frontend alone may fill with
	// files it can regenerate. It may be emptied between mounts. Empty
	// disables on-disk caches.
	CacheDir string
	// PersonalHomes keeps the last personalized library list so the first
	// UserViews after a restart can include it. Nil keeps that list in memory.
	PersonalHomes PersonalHomeStore
	// Plex supplies host services for the Plex frontend.
	Plex PlexHost
}

// PersonalHomeStore persists one viewer's personalized library list.
// The body is an opaque JSON document owned by the Media Browser frontend.
type PersonalHomeStore interface {
	LoadPersonalHome(ctx context.Context, protocol string, viewerID int64, config string) ([]byte, error)
	SavePersonalHome(ctx context.Context, protocol string, viewerID int64, config string, body []byte) error
	PersonalHomeViewers(ctx context.Context) ([]core.User, error)
}

// PageFreshness is every input of a cached browse response. A change drops
// the cached body. ArtBucket rotates image grants before they expire.
type PageFreshness struct {
	Catalog, Config, Watch, Favorite, Hidden, Listed, Images, Grants, Accounts, Safety uint64
	ArtBucket                                                                          int64
}

var ErrNotRegistered = errors.New("frontend not registered")
var ErrAlreadyMounted = errors.New("frontend already mounted")

// Registry serializes lifecycle transitions so a protocol has at most one
// live instance. The host factory must bind identity to the requested surface.
type Registry struct {
	mu      sync.RWMutex
	entries map[Protocol]*entry
	host    func(Protocol) Host
}
type entry struct {
	frontend   Frontend
	descriptor Descriptor
	instance   Instance
	transition chan struct{}
	generation uint64
}

func NewRegistry(host func(Protocol) Host) *Registry {
	return &Registry{entries: make(map[Protocol]*entry), host: host}
}
func (r *Registry) Register(f Frontend) error {
	if f == nil {
		return errors.New("nil frontend")
	}
	descriptor := f.Describe()
	id := descriptor.ID
	if id == "" {
		return errors.New("empty frontend id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		return fmt.Errorf("frontend %q already registered", id)
	}
	r.entries[id] = &entry{frontend: f, descriptor: cloneDescriptor(descriptor)}
	return nil
}

// ValidateSettings checks settings against the frontend's descriptor
// schema, then delegates protocol-specific settings to the registered
// frontend. The bool reports whether validation beyond the host's common
// checks applies (a schema or a protocol validator).
func (r *Registry) ValidateSettings(id Protocol, settings Settings) (bool, error) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return false, ErrNotRegistered
	}
	descriptor := e.descriptor
	validator, supported := e.frontend.(SettingsValidator)
	r.mu.Unlock()
	if schema, has := schemaOf(descriptor); has {
		supported = true
		if err := schema.validate(settings); err != nil {
			return true, err
		}
	}
	if validator == nil {
		return supported, nil
	}
	return true, validator.ValidateSettings(settings)
}

func (r *Registry) Mount(ctx context.Context, id Protocol) (Instance, error) {
	for {
		r.mu.Lock()
		e, ok := r.entries[id]
		if !ok {
			r.mu.Unlock()
			return nil, ErrNotRegistered
		}
		if wait := e.transition; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if e.instance != nil {
			r.mu.Unlock()
			return nil, ErrAlreadyMounted
		}
		if r.host == nil {
			r.mu.Unlock()
			return nil, errors.New("missing host factory")
		}
		e.generation++
		generation := e.generation
		e.transition = make(chan struct{})
		frontend, host := e.frontend, r.host
		r.mu.Unlock()
		instance, err := frontend.Mount(ctx, host(id))
		if err != nil && instance != nil {
			_ = instance.Close(ctx)
			instance = nil
		}
		if err == nil && instance == nil {
			err = errors.New("frontend returned nil instance")
		}
		r.mu.Lock()
		if current := r.entries[id]; current != e || e.generation != generation {
			r.mu.Unlock()
			if instance != nil {
				_ = instance.Close(ctx)
			}
			return nil, errors.New("frontend mount superseded")
		}
		if err == nil {
			e.instance = instance
		}
		close(e.transition)
		e.transition = nil
		r.mu.Unlock()
		return instance, err
	}
}
func (r *Registry) Unmount(ctx context.Context, id Protocol) error {
	for {
		r.mu.Lock()
		e, ok := r.entries[id]
		if !ok {
			r.mu.Unlock()
			return ErrNotRegistered
		}
		if wait := e.transition; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if e.instance == nil {
			r.mu.Unlock()
			return nil
		}
		e.generation++
		generation := e.generation
		e.transition = make(chan struct{})
		instance := e.instance
		r.mu.Unlock()
		err := instance.Close(ctx)
		r.mu.Lock()
		if current := r.entries[id]; current != e || e.generation != generation {
			r.mu.Unlock()
			return errors.New("frontend unmount superseded")
		}
		if err == nil {
			e.instance = nil
		}
		close(e.transition)
		e.transition = nil
		r.mu.Unlock()
		return err
	}
}
func (r *Registry) Instance(id Protocol) (Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.instance == nil {
		return nil, false
	}
	return e.instance, true
}
func (r *Registry) Descriptors() []Descriptor {
	r.mu.RLock()
	result := make([]Descriptor, 0, len(r.entries))
	for _, e := range r.entries {
		result = append(result, cloneDescriptor(e.descriptor))
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneDescriptor(d Descriptor) Descriptor {
	d.Clients = append([]string(nil), d.Clients...)
	d.CertifiedApps = append([]CertifiedApp(nil), d.CertifiedApps...)
	for i := range d.CertifiedApps {
		d.CertifiedApps[i].Steps = append([]ConnectStep(nil), d.CertifiedApps[i].Steps...)
	}
	if schema, ok := d.SettingsSchema.(SettingsSchema); ok {
		sections := append([]SettingsSection(nil), schema.Sections...)
		for i := range sections {
			fields := append([]SettingsField(nil), sections[i].Fields...)
			for j := range fields {
				fields[j].Options = append([]string(nil), fields[j].Options...)
				if fields[j].Min != nil {
					min := *fields[j].Min
					fields[j].Min = &min
				}
				if fields[j].Max != nil {
					max := *fields[j].Max
					fields[j].Max = &max
				}
			}
			sections[i].Fields = fields
		}
		schema.Sections = sections
		d.SettingsSchema = schema
	}
	return d
}
