package plextv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/store"
)

// ConfigStore is the part of *config.Store the registrar uses.
type ConfigStore interface {
	Get() config.Config
	Update(fn func(*config.Config) error) error
}

// UserSync receives the owner and the friend list (implemented by
// internal/auth).
type UserSync interface {
	SetOwner(ctx context.Context, owner Account, serverToken string) error
	SyncFriends(ctx context.Context, friends []Friend) error
}

// WANDetector finds the public IP (remote.WANDetector).
type WANDetector interface {
	Detect(ctx context.Context) (net.IP, error)
}

// PortMapper keeps the remote-access port forwarded (remote.PortMapper).
type PortMapper interface {
	Ensure(ctx context.Context, internal, external int) (remote.MappingStatus, error)
	Status() remote.MappingStatus
	Close(ctx context.Context)
}

// Status is the plex.tv / remote-access state for the admin dashboard.
type Status struct {
	Claimed           bool   `json:"claimed"`
	OwnerUsername     string `json:"ownerUsername,omitempty"`
	MachineIdentifier string `json:"machineIdentifier"`

	LANAddresses []string            `json:"lanAddresses"`
	PublicIP     string              `json:"publicIp,omitempty"`
	PublicIPErr  string              `json:"publicIpError,omitempty"`
	Connections  []remote.Connection `json:"connections"`

	PublishedURIs    []string  `json:"publishedUris"`
	LastPublish      time.Time `json:"lastPublish,omitempty"`
	LastPublishError string    `json:"lastPublishError,omitempty"`
	RegisterError    string    `json:"registerError,omitempty"` // servers.xml (non-fatal)

	// Listed reports whether plex.tv's resources list contains this server;
	// ResourceConnections is what clients will be offered.
	Listed              bool                 `json:"listed"`
	ResourceConnections []ResourceConnection `json:"resourceConnections,omitempty"`
	PublicAddress       string               `json:"publicAddress,omitempty"` // as plex.tv sees us
	PlexPass            bool                 `json:"plexPass"`                // owner subscription active
	OwnerFeatures       []string             `json:"ownerFeatures"`           // granted by plex.tv

	RemoteAccess bool                 `json:"remoteAccess"`
	Reachability *remote.Reachability `json:"reachability,omitempty"`
	UPnP         remote.MappingStatus `json:"upnp"`

	Cert      CertInfo `json:"cert"`
	CertError string   `json:"certError,omitempty"`

	// Presence is the plex.tv pubsub stream that marks the server online.
	Presence PresenceStatus `json:"presence"`
	// PlexReachable is plex.tv's last notifyConnectivity verdict for our
	// published address (nil until one arrives).
	PlexReachable          *bool     `json:"plexReachable,omitempty"`
	PlexReachableCheckedAt time.Time `json:"plexReachableCheckedAt,omitzero"`
	// Recent authenticated inbound traffic is external evidence even when
	// plex.tv's independent connectivity check fails. Never persist client IPs.
	RemoteClientLastSeen  time.Time `json:"remoteClientLastSeen,omitzero"`
	RemoteClientReachable bool      `json:"remoteClientReachable"`
	Friends               int       `json:"friends"`
	LastFriendSync        time.Time `json:"lastFriendSync,omitempty"`
	FriendSyncError       string    `json:"friendSyncError,omitempty"`
	// TokenRejected reports that plex.tv answered the server token with
	// 401/403 (publishing or friend sync): typically the device record
	// still belongs to an account the server was claimed by before.
	TokenRejected bool `json:"tokenRejected,omitempty"`
}

// RegistrarOptions configures a Registrar. Only Config is required; nil
// collaborators get production defaults or are disabled.
type RegistrarOptions struct {
	Config ConfigStore
	// Client carries the server identity; built from Config when nil.
	Client  *Client
	BaseURL string // for the default client (tests)
	Version string // PMS version reported to plex.tv
	Users   UserSync
	WAN     WANDetector
	LAN     func() ([]net.IP, error)
	Mapper  PortMapper // nil creates a remote.PortMapper
	Reach   *remote.ReachabilityChecker
	Certs   *CertManager // nil disables certificate management

	// Intervals (defaults in parentheses).
	Tick                       time.Duration // loop granularity (15s)
	AddressInterval            time.Duration // LAN/WAN re-check (30s)
	PublishInterval            time.Duration // unconditional re-publish (1h)
	FriendSyncInterval         time.Duration // access token sync (30m; pubsub events trigger immediate sync)
	UnknownTokenSyncMinSpacing time.Duration // minimum spacing between unknown-token friend re-reads (15s)
	MappingInterval            time.Duration // UPnP/NAT-PMP renew check (5m)
	CertInterval               time.Duration // certificate expiry check (1w, observed when not near expiry)
	ReachabilityInterval       time.Duration // remote self-test (1h, observed)
	OwnerInterval              time.Duration // owner account / Plex Pass re-check (6h)

	// PeerURIs, when set (clustered servers), returns the other live
	// nodes' connection URIs in publication order. PublishNow appends them,
	// de-duplicated, after this node's own URIs so plex.tv lists the union.
	// It is called on every publish and must not block on the network.
	PeerURIs func() []string

	// NodeMapping moves port mapping out of the publishing loop: the owner
	// runs RunPortMapping on every node (each cluster node is reached
	// through its own port forward, plex.publicPort being node-local), and
	// Stop leaves the mapping in place. Off, the loop maps the port and
	// Stop removes the mapping (a standalone server).
	NodeMapping bool

	// PublishPlexDirect also publishes the https plex.direct URIs. Off by
	// default: PMS publishes plain http URIs and plex.tv synthesises the
	// plex.direct https variants for clients asking with includeHttps=1.
	PublishPlexDirect bool

	Logger *slog.Logger
	Now    func() time.Time
}

// Registrar runs Ether's plex.tv presence: claim, connection publishing,
// friend sync, certificate renewal, port mapping and reachability.
type Registrar struct {
	claimMu sync.Mutex // serializes claim and device release
	o       RegistrarOptions
	client  *Client
	log     *slog.Logger

	wake               chan struct{}
	mapWake            chan struct{} // RunPortMapping
	presenceWake       chan struct{}
	publicationMu      sync.Mutex
	publicationRunMu   sync.Mutex
	publicationCancel  context.CancelFunc
	publicationEnabled bool

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	addrMu      sync.Mutex // serialises address refreshes
	addrRefresh time.Time  // last refreshAddresses

	mu                   sync.Mutex
	st                   Status
	lastLAN              []string
	lastWAN              string
	next                 map[string]time.Time
	lastUnknownTokenSync time.Time
	pubFail              int
	friendFail           int    // consecutive non-unauthorized friend sync failures
	friendContention     int    // consecutive SQLite write-contention failures in SyncFriendsNow
	pubGeneration        uint64 // advances whenever fresh connections must be published
	forcePub             bool
	mappingOn            bool
	trustAt              time.Time
	trustFP              string
	asyncID              string // outstanding connectivity request
	libraries            []LibrarySection

	presence *Presence

	// claimedToken is the server token while claimed; a switch to
	// unclaimed releases it (releaseClaim).
	claimedToken string

	// reachTargets is the URI set the last self-check probed.
	reachTargets []string
}

// rejectedRetry spaces out friend syncs while plex.tv rejects the token.
const rejectedRetry = 15 * time.Minute

// friendContentionRetry retries a friend sync promptly when another writer
// held SQLite across the write gate (e.g. post-upgrade index builds or WAL
// maintenance) instead of waiting the full 30-minute sync interval.
const friendContentionRetry = 15 * time.Second

// friendContentionWarnThreshold is how many consecutive write-contention
// attempts must fail before FriendSyncError surfaces a dashboard warning.
const friendContentionWarnThreshold = 3

const (
	taskAddress = "address"
	taskPublish = "publish"
	taskFriends = "friends"
	taskMapping = "mapping"
	taskCert    = "cert"
	taskReach   = "reach"
	taskOwner   = "owner"
)

// NewRegistrar validates options and returns a registrar; call Start.
func NewRegistrar(o RegistrarOptions) (*Registrar, error) {
	if o.Config == nil {
		return nil, errors.New("plextv: registrar needs Config")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Tick, 15*time.Second)
	def(&o.AddressInterval, 30*time.Second)
	def(&o.PublishInterval, time.Hour)
	def(&o.FriendSyncInterval, 30*time.Minute)
	def(&o.UnknownTokenSyncMinSpacing, 15*time.Second)
	def(&o.MappingInterval, 5*time.Minute)
	def(&o.CertInterval, 7*24*time.Hour)
	def(&o.ReachabilityInterval, time.Hour)
	def(&o.OwnerInterval, 6*time.Hour)
	if o.Version == "" {
		o.Version = DefaultServerVersion
	}
	client := o.Client
	if client == nil {
		var err error
		client, err = New(Options{BaseURL: o.BaseURL, Identity: ServerIdentity(o.Config.Get(), o.Version), Logger: o.Logger})
		if err != nil {
			return nil, err
		}
	}
	if o.WAN == nil {
		o.WAN = remote.NewWANDetector(nil, o.Logger)
	}
	if o.LAN == nil {
		o.LAN = func() ([]net.IP, error) { return remote.LANIPs(remote.LANOptions{}) }
	}
	if o.Mapper == nil {
		o.Mapper = remote.NewPortMapper(remote.PortMapperOptions{Description: "Ether", Logger: o.Logger})
	}
	if o.Reach == nil {
		o.Reach = remote.NewReachabilityChecker(nil)
	}
	r := &Registrar{
		o:                  o,
		client:             client,
		log:                o.Logger.With("component", "registrar"),
		wake:               make(chan struct{}, 1),
		mapWake:            make(chan struct{}, 1),
		presenceWake:       make(chan struct{}, 1),
		publicationEnabled: plexAppsEnabled(o.Config.Get()),
		next:               map[string]time.Time{},
	}
	if c := o.Config.Get(); c.Plex.Claimed {
		r.claimedToken = c.Plex.Token
	}
	if sub, ok := o.Config.(interface{ Subscribe(func(config.Config)) }); ok {
		sub.Subscribe(r.onConfig)
	}
	r.presence = NewPresence(PresenceOptions{
		Client: client,
		Credentials: func() (int64, string, string, bool) {
			c := r.o.Config.Get()
			return c.Plex.OwnerPlexID, c.Server.MachineIdentifier, c.Plex.Token,
				c.Plex.Token != "" && c.Plex.OwnerPlexID != 0 && c.Server.MachineIdentifier != ""
		},
		OnMessage: r.onPubsub,
	})
	r.refreshConfigStatus(o.Config.Get())
	return r, nil
}

// Client returns the server-identity plex.tv client.
func (r *Registrar) Client() *Client { return r.client }

// SignInClient returns a client with the setup wizard's identity for PIN
// sign-in (CreatePIN, AuthURL, WaitPIN).
func (r *Registrar) SignInClient() *Client {
	return r.client.WithIdentity(AppIdentity(r.o.Config.Get(), ""))
}

// Start runs the background loop until ctx ends or Stop is called.
func (r *Registrar) Start(ctx context.Context) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.cancel != nil {
		return errors.New("plextv: registrar already started")
	}
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	loopDone := make(chan struct{})
	go r.loop(ctx, loopDone)
	presenceDone := make(chan struct{})
	go func() { defer close(presenceDone); r.runPresence(ctx) }()
	go func() { <-loopDone; <-presenceDone; close(done) }()
	return nil
}

func plexAppsEnabled(c config.Config) bool {
	return c.Frontends == nil || c.Frontends["plex"].Enabled
}

// Presence is publication, not account or membership synchronisation.
func (r *Registrar) runPresence(ctx context.Context) {
	var cancel context.CancelFunc
	var done chan struct{}
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stop()
	for {
		if plexAppsEnabled(r.o.Config.Get()) && cancel == nil {
			child, c := context.WithCancel(ctx)
			cancel = c
			done = make(chan struct{})
			go func() { defer close(done); r.presence.Run(child) }()
		} else if !plexAppsEnabled(r.o.Config.Get()) {
			stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-r.presenceWake:
		}
	}
}

// Stop ends the loop and removes any port mapping.
func (r *Registrar) Stop() {
	r.runMu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	if !r.o.NodeMapping {
		r.closeMapping()
	}
}

func (r *Registrar) closeMapping() {
	ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	r.o.Mapper.Close(ctx)
	r.mu.Lock()
	r.st.UPnP = r.o.Mapper.Status()
	r.mappingOn = false
	r.mu.Unlock()
}

// RunPortMapping keeps this node's port mapping (UPnP IGD or NAT-PMP)
// alive until ctx ends, then removes it. Used with NodeMapping, on every
// cluster node whether or not it leads.
func (r *Registrar) RunPortMapping(ctx context.Context) {
	t := time.NewTicker(r.o.Tick)
	defer t.Stop()
	for {
		if r.due(taskMapping, r.o.MappingInterval) {
			prev := r.o.Mapper.Status()
			r.ensureMapping(ctx, r.o.Config.Get())
			if cur := r.o.Mapper.Status(); cur.Active != prev.Active || cur.ExternalPort != prev.ExternalPort || cur.ExternalIP != prev.ExternalIP {
				// The WAN URI this node heartbeats may have changed:
				// refresh addresses on the next NodeURIs.
				r.addrMu.Lock()
				r.addrRefresh = time.Time{}
				r.addrMu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			r.closeMapping()
			return
		case <-t.C:
		case <-r.mapWake:
		}
	}
}

// Trigger asks the loop to re-check addresses and re-publish now (e.g. after
// a config change).
func (r *Registrar) Trigger() {
	r.mu.Lock()
	r.requestPublishLocked()
	r.next[taskAddress] = time.Time{}
	r.next[taskMapping] = time.Time{}
	if r.st.FriendSyncError != "" && !r.st.TokenRejected {
		r.next[taskFriends] = time.Time{}
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	select {
	case r.mapWake <- struct{}{}:
	default:
	}
}

// ListenerReady wakes the loop once this server serves TLS. A trust check
// that ran before the listener existed (every restart) is then confirmed or
// cleared at once instead of at the next tick.
func (r *Registrar) ListenerReady() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// RequestPublish asks the running loop to publish connections now without
// re-checking addresses (e.g. after cluster membership changed).
func (r *Registrar) RequestPublish() {
	r.mu.Lock()
	r.requestPublishLocked()
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// SetLibrarySections connects account publishing to the actual PMS library
// identities. Call during front-end construction and whenever sections change.
func (r *Registrar) SetLibrarySections(sections []LibrarySection) {
	r.mu.Lock()
	r.libraries = slices.Clone(sections)
	r.requestPublishLocked()
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// requestPublishLocked marks a new publication request. The generation prevents
// an older in-flight publication from consuming an update it never observed.
func (r *Registrar) requestPublishLocked() {
	r.pubGeneration++
	r.forcePub = true
}

// Status returns a snapshot for the admin dashboard.
func (r *Registrar) Status() Status {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	age := r.o.Now().Sub(st.RemoteClientLastSeen)
	st.RemoteClientReachable = !st.RemoteClientLastSeen.IsZero() && age >= 0 && age < 5*time.Minute
	st.LANAddresses = slices.Clone(st.LANAddresses)
	st.Connections = slices.Clone(st.Connections)
	st.PublishedURIs = slices.Clone(st.PublishedURIs)
	st.ResourceConnections = slices.Clone(st.ResourceConnections)
	st.OwnerFeatures = slices.Clone(st.OwnerFeatures)
	st.UPnP = r.o.Mapper.Status()
	if r.o.Certs != nil {
		st.Cert = r.o.Certs.Info()
	}
	st.Presence = r.presence.Status()
	return st
}

// WANCheck is the away-from-home verdict the console shows. Reason names
// the evidence: "outside_client" (an authenticated client connected from
// outside within five minutes), "plex_tv" (plex.tv's connectivity check),
// "self_check" (this server fetched its own /identity through the published
// address), "self_check_failed", "wrong_identity" (the address answers as
// another server) or "not_checked". Address is the published route the
// verdict is about, when it is a specific one.
type WANCheck struct {
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Address   string    `json:"address,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// WANState is WANCheck().State.
func (st Status) WANState() string { return st.WANCheck().State }

// WANCheck never resurrects an older failed check after newer client
// evidence expires, and never calls a route reachable without a positive
// observation. The self-check is a real request through the published
// address, but it starts inside the network (NAT loopback, split DNS): its
// failure is "unknown", not "unreachable", unless another server answered.
func (st Status) WANCheck() WANCheck {
	if st.RemoteClientReachable {
		return WANCheck{State: "reachable", Reason: "outside_client", CheckedAt: st.RemoteClientLastSeen}
	}
	if st.PlexReachable != nil && *st.PlexReachable {
		return WANCheck{State: "reachable", Reason: "plex_tv", CheckedAt: st.PlexReachableCheckedAt}
	}
	// A plex.tv verdict does not identify which published route it tested.
	// A reverse proxy can work while the direct public port is closed, so a
	// published outside custom route is judged by its own self-check.
	var custom []string
	for _, c := range st.Connections {
		if c.Custom && !c.Local && c.URI != "" && slices.Contains(st.PublishedURIs, c.URI) && !slices.Contains(custom, c.URI) {
			custom = append(custom, c.URI)
		}
	}
	if len(custom) > 0 {
		if check, ok := st.selfCheck(custom); ok {
			return check
		}
		return WANCheck{State: "unknown", Reason: "not_checked", Address: custom[0]}
	}
	if st.PlexReachable != nil && (st.RemoteClientLastSeen.IsZero() || !st.PlexReachableCheckedAt.Before(st.RemoteClientLastSeen)) {
		return WANCheck{State: "unreachable", Reason: "plex_tv", CheckedAt: st.PlexReachableCheckedAt}
	}
	// No custom route: every self-probe target is the direct public route.
	if st.Reachability != nil {
		var uris []string
		for _, pr := range st.Reachability.Results {
			uris = append(uris, pr.URI)
		}
		if check, ok := st.selfCheck(uris); ok && check.State == "reachable" {
			return check
		}
	}
	return WANCheck{State: "unknown"}
}

// selfCheck is the latest self-probe verdict among uris: a success first,
// then another server answering, then a plain failure.
func (st Status) selfCheck(uris []string) (WANCheck, bool) {
	if st.Reachability == nil {
		return WANCheck{}, false
	}
	var wrong, failed string
	for _, pr := range st.Reachability.Results {
		if pr.URI == "" || !slices.Contains(uris, pr.URI) {
			continue
		}
		switch {
		case pr.OK:
			return WANCheck{State: "reachable", Reason: "self_check", Address: pr.URI, CheckedAt: st.Reachability.Checked}, true
		case strings.Contains(pr.Error, "different server"):
			if wrong == "" {
				wrong = pr.URI
			}
		case failed == "":
			failed = pr.URI
		}
	}
	if wrong != "" {
		return WANCheck{State: "unreachable", Reason: "wrong_identity", Address: wrong, CheckedAt: st.Reachability.Checked}, true
	}
	if failed != "" {
		return WANCheck{State: "unknown", Reason: "self_check_failed", Address: failed, CheckedAt: st.Reachability.Checked}, true
	}
	return WANCheck{}, false
}

// ObserveRemoteClient is called only after Plex authentication. It performs
// no database or network work. Exclude local/VPN peers and the server's own
// public address, which may be a NAT loopback rather than an outside client.
func (r *Registrar) ObserveRemoteClient(address string) {
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || remote.IsCGNAT(ip) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if own := net.ParseIP(r.st.PublicIP); own != nil && own.Equal(ip) {
		return
	}
	r.st.RemoteClientLastSeen = r.o.Now()
}

func (r *Registrar) refreshConfigStatus(cfg config.Config) {
	r.mu.Lock()
	r.st.Claimed = cfg.Plex.Claimed && cfg.Plex.Token != ""
	r.st.OwnerUsername = cfg.Plex.OwnerUsername
	r.st.MachineIdentifier = cfg.Server.MachineIdentifier
	r.st.RemoteAccess = cfg.Plex.RemoteAccess
	r.mu.Unlock()
}

func (r *Registrar) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(r.o.Tick)
	defer t.Stop()
	for {
		r.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
	}
}

// due reports whether task should run now, and schedules its next run.
func (r *Registrar) due(task string, every time.Duration) bool {
	now := r.o.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Compare wall time: publishing and friend refresh must account for
	// time spent asleep, just like stored expiry dates.
	if now.Round(0).Before(r.next[task].Round(0)) {
		return false
	}
	r.next[task] = now.Add(every)
	return true
}

func (r *Registrar) schedule(task string, at time.Time) {
	r.mu.Lock()
	r.next[task] = at
	r.mu.Unlock()
}

func (r *Registrar) runDue(ctx context.Context) {
	cfg := r.o.Config.Get()
	r.refreshConfigStatus(cfg)
	if !r.o.NodeMapping && r.due(taskMapping, r.o.MappingInterval) {
		r.ensureMapping(ctx, cfg)
	}
	if r.due(taskAddress, r.o.AddressInterval) {
		r.refreshAddresses(ctx, cfg)
	}
	if ctx.Err() != nil {
		return
	}
	if r.o.Certs != nil && r.due(taskCert, r.o.CertInterval) {
		_ = r.RenewCertificate(ctx, false)
		r.scheduleCertCheck()
	}
	if r.o.Certs != nil {
		r.refreshCertTrust(ctx)
	}
	claimed := cfg.Plex.Claimed && cfg.Plex.Token != ""
	if !claimed || !plexAppsEnabled(cfg) {
		return
	}
	r.mu.Lock()
	force := r.forcePub
	generation := r.pubGeneration
	r.mu.Unlock()
	if force || r.due(taskPublish, r.o.PublishInterval) {
		if err := r.PublishNow(ctx); err != nil && ctx.Err() == nil && plexAppsEnabled(r.o.Config.Get()) {
			r.mu.Lock()
			r.pubFail++
			backoff := time.Minute << min(r.pubFail-1, 5) // 1m .. 32m
			if r.pubGeneration == generation {
				r.forcePub = false // retry this request on the back-off schedule
			}
			r.next[taskPublish] = r.o.Now().Add(backoff)
			r.mu.Unlock()
		}
	}
	if r.due(taskFriends, r.o.FriendSyncInterval) {
		_ = r.SyncFriendsNow(ctx)
	}
	// Plex Pass bought, renewed or lapsed while running: Live TV and the
	// owner features clients see follow without a restart.
	if r.due(taskOwner, r.o.OwnerInterval) {
		if err := r.RefreshOwner(ctx); err != nil && ctx.Err() == nil {
			r.log.Debug("plex.tv owner refresh failed", "err", err)
			retry := 10 * time.Minute
			if store.IsWriteContention(err) {
				retry = min(friendContentionRetry, r.o.OwnerInterval)
			}
			r.schedule(taskOwner, r.o.Now().Add(retry))
		}
	}
	// Custom URLs are published with remote access off too; check them, and
	// re-check at once when the public routes change (a new public host).
	if cfg.Plex.RemoteAccess || len(cfg.Plex.CustomURLs) > 0 {
		uris := r.reachURIs(cfg)
		r.mu.Lock()
		if !slices.Equal(uris, r.reachTargets) {
			r.next[taskReach] = time.Time{}
		}
		r.mu.Unlock()
		if r.due(taskReach, r.o.ReachabilityInterval) {
			r.CheckReachability(ctx)
		}
	}
}

func (r *Registrar) ensureMapping(ctx context.Context, cfg config.Config) {
	want := plexAppsEnabled(cfg) && cfg.Plex.RemoteAccess && cfg.Plex.UseUPnP
	r.mu.Lock()
	on := r.mappingOn
	r.mu.Unlock()
	if !want {
		if on {
			r.o.Mapper.Close(ctx)
			r.mu.Lock()
			r.mappingOn = false
			r.requestPublishLocked()
			r.mu.Unlock()
		}
		return
	}
	prev := r.o.Mapper.Status()
	st, err := r.o.Mapper.Ensure(ctx, serverPort(cfg), publicPort(cfg))
	r.mu.Lock()
	r.mappingOn = true
	if st.Active && (st.ExternalPort != prev.ExternalPort || !prev.Active) {
		r.requestPublishLocked()
	}
	// A gateway may grant (or an adopted entry may have) a lease shorter
	// than MappingInterval: check again when the mapper wants to renew,
	// or the mapping lapses between checks.
	if st.Active && !st.RenewAt.IsZero() && st.RenewAt.Before(r.next[taskMapping]) {
		r.next[taskMapping] = st.RenewAt
	}
	r.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		r.log.Debug("port mapping failed", "err", err)
	}
}

func serverPort(cfg config.Config) int {
	if cfg.Server.Port > 0 {
		return cfg.Server.Port
	}
	return 32400
}

// publicPort is the WAN port: the manual forward, else the server port.
func publicPort(cfg config.Config) int {
	if cfg.Plex.PublicPort > 0 {
		return cfg.Plex.PublicPort
	}
	return serverPort(cfg)
}

// wanPort is the port remote clients reach this server on. While the
// gateway holds a mapping it is the external port actually mapped: a
// NAT-PMP gateway may grant a different port than the one requested
// (RFC 6886 3.3, e.g. when another host already holds it), and publishing
// the requested port would then advertise a dead address. Otherwise it is
// publicPort.
func (r *Registrar) wanPort(cfg config.Config) int {
	if cfg.Plex.RemoteAccess && cfg.Plex.UseUPnP {
		if m := r.o.Mapper.Status(); m.Active && m.ExternalPort > 0 {
			return m.ExternalPort
		}
	}
	return publicPort(cfg)
}

func (r *Registrar) refreshAddresses(ctx context.Context, cfg config.Config) {
	r.addrMu.Lock()
	defer r.addrMu.Unlock()
	r.addrRefresh = r.o.Now()
	var lan []string
	if ips, err := r.o.LAN(); err == nil {
		for _, ip := range ips {
			lan = append(lan, ip.String())
		}
	} else {
		r.log.Warn("lan address enumeration failed", "err", err)
	}
	wan, wanErr := "", ""
	if plexAppsEnabled(cfg) && cfg.Plex.RemoteAccess {
		switch {
		case strings.TrimSpace(cfg.Plex.PublicHost) != "":
			wan = strings.TrimSpace(cfg.Plex.PublicHost)
		default:
			if m := r.o.Mapper.Status(); m.Active && m.ExternalIP != "" && remote.IsPublicIP(net.ParseIP(m.ExternalIP)) {
				wan = m.ExternalIP
			} else if ip, err := r.o.WAN.Detect(ctx); err == nil {
				wan = ip.String()
			} else {
				wanErr = err.Error()
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(lan, r.lastLAN) || wan != r.lastWAN {
		r.st.RemoteClientLastSeen = time.Time{}
		if r.lastLAN != nil || r.lastWAN != "" {
			r.log.Info("network addresses changed", "lan", lan, "wan", wan)
		}
		r.requestPublishLocked()
	}
	r.lastLAN, r.lastWAN = lan, wan
	r.st.LANAddresses = lan
	r.st.PublicIP = wan
	r.st.PublicIPErr = wanErr
}

// connections builds the connection list from the latest addresses.
func (r *Registrar) connections(cfg config.Config) []remote.Connection {
	r.mu.Lock()
	lanS, wanS := slices.Clone(r.lastLAN), r.lastWAN
	r.mu.Unlock()
	var lan []net.IP
	for _, s := range lanS {
		if ip := net.ParseIP(s); ip != nil {
			lan = append(lan, ip)
		}
	}
	var wan net.IP
	extraURLs := slices.Clone(cfg.Plex.CustomURLs)
	if wanS != "" {
		if ip := net.ParseIP(wanS); ip != nil {
			wan = ip
		} else {
			customCert := false
			if r.o.Certs != nil {
				info := r.o.Certs.Info()
				customCert = info.Present && info.Custom && !info.SelfSigned
			}
			extraURLs = append(extraURLs, publicHostURLs(wanS, r.wanPort(cfg), cfg.Plex.SecureConnections, customCert)...)
		}
	}
	hash := cfg.Plex.CertHash
	if r.o.Certs != nil {
		if info := r.o.Certs.Info(); info.Present && !info.SelfSigned && info.Hash != "" {
			hash = info.Hash
		} else {
			hash = ""
		}
	}
	return remote.BuildConnections(remote.BuildOptions{
		LAN: lan, WAN: wan, LocalPort: serverPort(cfg), PublicPort: r.wanPort(cfg),
		CertHash: hash, Secure: cfg.Plex.SecureConnections, CustomURLs: extraURLs,
	})
}

// publicHostURLs turns the owner's public host name into connection URLs.
// A typed scheme is honoured exactly (case-insensitively, surrounding space
// ignored), so https://plex.example.com is never downgraded. A bare DNS name
// (optionally host:port) reaches Ether's own listener, whose certificate
// only names plex.direct hosts: https is used when a custom certificate is
// installed, the port is 443, or secure connections are required; plain
// http is added unless secure connections are required or the port is 443.
func publicHostURLs(host string, port int, secure string, customCert bool) []string {
	host = strings.TrimSpace(host)
	if scheme, rest, ok := strings.Cut(host, "://"); ok {
		scheme = strings.ToLower(scheme)
		if scheme != "http" && scheme != "https" {
			return nil
		}
		return []string{scheme + "://" + strings.TrimSuffix(rest, "/")}
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			host, port = h, n
		}
	}
	host = strings.TrimSuffix(host, "/")
	var out []string
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	if secure != remote.SecureDisabled && (customCert || port == 443 || secure == remote.SecureRequired) {
		if port == 443 {
			out = append(out, "https://"+host)
		} else {
			out = append(out, "https://"+hostPort)
		}
	}
	if secure != remote.SecureRequired && port != 443 {
		out = append(out, "http://"+hostPort)
	}
	return out
}

// publishable is the URI list sent to plex.tv. Like PMS, every address goes
// out as plain http://<ip>:<port>, whatever the secure-connections mode:
// plex.tv matches those IPs against localAddresses to mark LAN connections
// local, and synthesises the https plex.direct variants (https only when
// httpsRequired is set). A published plex.direct hostname is stored as a
// non-local address, so clients would list a LAN server as Remote.
// CGNAT (100.64.0.0/10, including Tailscale) addresses are never published to
// plex.tv because plex.tv rejects the whole device update when one is present.
func publishable(cs []remote.Connection, includePlexDirect bool) []string {
	var out []string
	add := func(uri string) {
		if !remote.IsPublishableURI(uri) {
			return
		}
		if !slices.Contains(out, uri) {
			out = append(out, uri)
		}
	}
	for _, c := range cs {
		if ip := net.ParseIP(c.Address); ip != nil && remote.IsCGNAT(ip) {
			continue
		}
		if c.Protocol == "https" && !c.Custom {
			if c.Address != "" && c.Port > 0 {
				add("http://" + net.JoinHostPort(c.Address, strconv.Itoa(c.Port)))
			}
			if !includePlexDirect {
				continue
			}
		}
		add(c.URI)
	}
	return out
}

// publishSet is this node's connections and the URIs it publishes from
// them.
func (r *Registrar) publishSet(cfg config.Config) ([]remote.Connection, []string) {
	conns := r.connections(cfg)
	published := conns
	if !cfg.Plex.RemoteAccess {
		// Only LAN and custom URLs when remote access is off.
		published = nil
		for _, c := range conns {
			if c.Local || c.Custom {
				published = append(published, c)
			}
		}
	}
	return conns, publishable(published, r.o.PublishPlexDirect)
}

// NodeURIs returns the URIs this node would publish for itself, refreshing
// its LAN/WAN addresses first when they are older than AddressInterval
// (the loop keeps them fresh while it runs). A clustered node heartbeats
// these so the leader can publish the union.
func (r *Registrar) NodeURIs(ctx context.Context) []string {
	cfg := r.o.Config.Get()
	r.addrMu.Lock()
	stale := r.addrRefresh.IsZero() || r.o.Now().Sub(r.addrRefresh) >= r.o.AddressInterval
	r.addrMu.Unlock()
	if stale {
		r.refreshAddresses(ctx, cfg)
	}
	_, uris := r.publishSet(cfg)
	return uris
}

// peerLANAddrs is the LAN IP addresses in the other cluster nodes' URIs
// (PeerURIs), for servers.xml's localAddresses: plex.tv then knows every
// node's LAN address, not only the leader's.
func (r *Registrar) peerLANAddrs() []string {
	if r.o.PeerURIs == nil {
		return nil
	}
	var out []string
	for _, raw := range r.o.PeerURIs() {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil {
			ip, _, _ = remote.ParsePlexDirectHost(u.Hostname())
		}
		if ip == nil || !remote.UsableLANIP(ip) || remote.IsPublicIP(ip) {
			continue
		}
		if s := ip.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// mergeURIs appends extra to own, skipping duplicates.
func mergeURIs(own, extra []string) []string {
	out := slices.Clone(own)
	for _, u := range extra {
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// PublishNow publishes the current connections to plex.tv and verifies the
// server appears in the resources list.
func (r *Registrar) PublishNow(ctx context.Context) error {
	r.publicationRunMu.Lock()
	defer r.publicationRunMu.Unlock()
	r.publicationMu.Lock()
	if !r.publicationEnabled {
		r.publicationMu.Unlock()
		return errors.New("plextv: Plex apps are disabled")
	}
	ctx, cancel := context.WithCancel(ctx)
	r.publicationCancel = cancel
	r.publicationMu.Unlock()
	defer func() {
		cancel()
		r.publicationMu.Lock()
		r.publicationCancel = nil
		r.publicationMu.Unlock()
	}()
	r.mu.Lock()
	generation := r.pubGeneration
	libraries := slices.Clone(r.libraries)
	r.mu.Unlock()
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	r.mu.Lock()
	haveAddrs := r.lastLAN != nil || r.lastWAN != ""
	r.mu.Unlock()
	if !haveAddrs {
		r.refreshAddresses(ctx, cfg)
	}
	conns, uris := r.publishSet(cfg)
	if r.o.PeerURIs != nil {
		for _, peerURI := range r.o.PeerURIs() {
			if remote.IsPublishableURI(peerURI) {
				uris = mergeURIs(uris, []string{peerURI})
			}
		}
	}
	if len(uris) == 0 {
		err := errors.New("plextv: no connection addresses to publish")
		r.mu.Lock()
		r.st.Connections = conns
		r.st.LastPublish = r.o.Now()
		r.st.LastPublishError = err.Error()
		r.mu.Unlock()
		return err
	}
	now := r.o.Now()
	flags := &PublishFlags{HTTPSRequired: cfg.Plex.SecureConnections == "required"}
	if cfg.Plex.SecureConnections != "disabled" {
		if r.o.Certs != nil {
			ci := r.o.Certs.Info()
			flags.HTTPSEnabled = ci.Present && !ci.SelfSigned
		}
		if !flags.HTTPSEnabled {
			for _, u := range uris {
				if strings.HasPrefix(u, "https://") {
					flags.HTTPSEnabled = true
					break
				}
			}
		}
	}
	err := r.client.PublishConnectionsWith(ctx, cfg.Plex.Token, uris, flags)

	r.mu.Lock()
	r.st.Connections = conns
	r.st.LastPublish = now
	if err != nil {
		r.st.LastPublishError = err.Error()
		r.st.TokenRejected = errors.Is(err, core.ErrUnauthorized)
		r.mu.Unlock()
		r.log.Warn("publish connections failed", "err", err)
		return err
	}
	r.st.PublishedURIs = uris
	r.st.LastPublishError = ""
	r.st.TokenRejected = false
	r.pubFail = 0
	if r.pubGeneration == generation {
		r.forcePub = false
	}
	r.next[taskPublish] = now.Add(r.o.PublishInterval)
	lan, wan := slices.Clone(r.lastLAN), r.lastWAN
	r.mu.Unlock()
	r.log.Info("published connections", "count", len(uris))

	mapping := "unknown"
	if cfg.Plex.RemoteAccess {
		if m := r.o.Mapper.Status(); m.Active || cfg.Plex.PublicPort > 0 {
			mapping = "mapped"
		}
	} else {
		mapping = "unmapped"
	}
	var localAddrs []string
	for _, addr := range lan {
		if ip := net.ParseIP(addr); ip != nil && !remote.IsCGNAT(ip) {
			localAddrs = append(localAddrs, addr)
		}
	}
	reg := ServerRegistration{
		Name: cfg.Server.FriendlyName, Version: r.o.Version, LocalAddresses: mergeURIs(localAddrs, r.peerLANAddrs()), MappingState: mapping,
		PrivatePort: serverPort(cfg),
		Libraries:   libraries,
	}
	if len(localAddrs) > 0 {
		reg.PrivateAddress = localAddrs[0]
	}
	if cfg.Plex.RemoteAccess {
		reg.Port = r.wanPort(cfg)
		if ip := net.ParseIP(wan); ip != nil && !remote.IsCGNAT(ip) {
			reg.Address = wan
		}
	}
	regErr := r.client.RegisterServer(ctx, cfg.Plex.Token, reg)
	if regErr == nil && cfg.Plex.RemoteAccess {
		var id string
		id, regErr = r.client.ReportConnectivity(ctx, cfg.Plex.Token)
		if regErr == nil {
			r.mu.Lock()
			r.asyncID = id
			r.mu.Unlock()
		}
	}
	if regErr != nil && ctx.Err() == nil {
		r.log.Debug("servers.xml registration failed (non-fatal)", "err", regErr)
	}
	res, ferr := r.client.FindServer(ctx, cfg.Plex.Token, cfg.Server.MachineIdentifier)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.RegisterError = ""
	if regErr != nil {
		r.st.RegisterError = regErr.Error()
	}
	if ferr == nil {
		r.st.Listed = true
		r.st.ResourceConnections = res.Connections
		r.st.PublicAddress = res.PublicAddress
	} else if ctx.Err() == nil {
		r.st.Listed = false
		r.st.ResourceConnections = nil
	}
	return nil
}

// SyncFriendsNow refreshes friends and their access tokens.
func (r *Registrar) SyncFriendsNow(ctx context.Context) error {
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	friends, err := r.client.Friends(ctx, cfg.Plex.Token)
	fromUsers := false
	if err == nil && r.o.Users != nil {
		fromUsers = true
		err = r.o.Users.SyncFriends(ctx, friends)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.LastFriendSync = r.o.Now()
	if err != nil {
		switch {
		case errors.Is(err, core.ErrUnauthorized):
			// A rejected token stays rejected until the owner re-claims:
			// retry rarely instead of every minute.
			r.friendFail = 0
			r.friendContention = 0
			r.st.FriendSyncError = err.Error()
			r.st.TokenRejected = true
			r.next[taskFriends] = r.o.Now().Add(rejectedRetry)
		case fromUsers && store.IsWriteContention(err):
			r.friendContention++
			if r.friendContention >= friendContentionWarnThreshold {
				r.st.FriendSyncError = err.Error()
			}
			if ctx.Err() == nil {
				r.next[taskFriends] = r.o.Now().Add(min(friendContentionRetry, r.o.FriendSyncInterval))
			}
		default:
			r.friendContention = 0
			r.friendFail++
			r.st.FriendSyncError = err.Error()
			if ctx.Err() == nil {
				backoff := min(time.Minute<<min(r.friendFail-1, 4), r.o.FriendSyncInterval) // 1m .. 16m
				r.next[taskFriends] = r.o.Now().Add(backoff)
			}
		}
		r.log.Warn("friend sync failed", "err", err)
		return err
	}
	n := 0
	for _, f := range friends {
		if !f.Owned {
			n++
		}
	}
	r.friendFail = 0
	r.friendContention = 0
	r.st.Friends = n
	r.st.FriendSyncError = ""
	if ctx.Err() == nil {
		r.next[taskFriends] = r.o.Now().Add(r.o.FriendSyncInterval)
	}
	return nil
}

// SyncFriendsOnUnknownToken re-reads friends and access tokens when an
// unrecognised token arrives, rate-limited by UnknownTokenSyncMinSpacing.
func (r *Registrar) SyncFriendsOnUnknownToken(ctx context.Context) error {
	if !plexAppsEnabled(r.o.Config.Get()) {
		return nil
	}
	now := r.o.Now()
	r.mu.Lock()
	if !r.lastUnknownTokenSync.IsZero() && now.Round(0).Sub(r.lastUnknownTokenSync.Round(0)) < r.o.UnknownTokenSyncMinSpacing {
		r.mu.Unlock()
		return nil
	}
	r.lastUnknownTokenSync = now
	r.mu.Unlock()
	return r.SyncFriendsNow(ctx)
}

// CheckReachability probes the WAN connections and records the result.
func (r *Registrar) CheckReachability(ctx context.Context) remote.Reachability {
	cfg := r.o.Config.Get()
	uris := r.reachURIs(cfg)
	res := r.o.Reach.Check(ctx, uris, cfg.Server.MachineIdentifier)
	r.mu.Lock()
	r.st.Reachability = &res
	r.reachTargets = uris
	r.mu.Unlock()
	return res
}

// RequestReachabilityCheck asks the loop to run the self-check now (the
// owner's "run checks again").
func (r *Registrar) RequestReachabilityCheck() {
	r.mu.Lock()
	r.next[taskReach] = time.Time{}
	if !r.st.TokenRejected {
		r.next[taskFriends] = time.Time{}
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// reachURIs is what the self-check probes: the public plex.direct name and
// every outside custom route (the first direct WAN URI when neither exists).
func (r *Registrar) reachURIs(cfg config.Config) []string {
	var uris []string
	// PMS checks its own public plex.direct name on the mapped public port,
	// even when the local listener uses a different port.
	r.mu.Lock()
	wan := r.lastWAN
	r.mu.Unlock()
	if ip := net.ParseIP(wan); ip != nil && cfg.Plex.CertHash != "" {
		hash := cfg.Plex.CertHash
		if r.o.Certs != nil {
			if ci := r.o.Certs.Info(); ci.Present && !ci.SelfSigned && ci.Hash != "" {
				hash = ci.Hash
			}
		}
		if hash != "" {
			uris = append(uris, "https://"+net.JoinHostPort(remote.PlexDirectHost(ip, hash), fmt.Sprint(r.wanPort(cfg))))
		}
	}
	for _, c := range r.connections(cfg) {
		if !c.Local && (len(uris) == 0 || c.Custom) && !slices.Contains(uris, c.URI) {
			uris = append(uris, c.URI)
		}
	}
	return uris
}

// RenewCertificate runs the certificate manager; force bypasses the
// failure back-off (admin "retry now").
func (r *Registrar) RenewCertificate(ctx context.Context, force bool) error {
	if r.o.Certs == nil {
		return errors.New("plextv: certificate management disabled")
	}
	cfg := r.o.Config.Get()
	r.mu.Lock()
	hosts := slices.Clone(r.lastLAN)
	r.mu.Unlock()
	before := r.o.Certs.Info()
	token := cfg.Plex.Token
	if !force && !plexAppsEnabled(cfg) {
		token = ""
	}
	info, err := r.o.Certs.Ensure(ctx, token, hosts, force)
	r.mu.Lock()
	r.st.CertError = ""
	if err != nil {
		r.st.CertError = err.Error()
	}
	r.mu.Unlock()
	if info.Present && (info.Fingerprint != before.Fingerprint || cfg.Plex.CertFile != info.CertFile || cfg.Plex.CertHash != info.Hash) {
		uerr := r.o.Config.Update(func(c *config.Config) error {
			c.Plex.CertFile, c.Plex.KeyFile = info.CertFile, info.KeyFile
			if !info.SelfSigned {
				c.Plex.CertHash = info.Hash
			} else {
				c.Plex.CertHash = ""
			}
			return nil
		})
		if uerr != nil {
			r.log.Warn("persist certificate paths", "err", uerr)
		}
		r.mu.Lock()
		r.requestPublishLocked()
		r.mu.Unlock()
	}
	r.refreshCertTrust(ctx)
	return err
}

// refreshCertTrust re-checks the installed plex.direct certificate on the
// certificate schedule, at the next tick after a first failed handshake (the
// startup check runs before the listener serves), a minute after a confirmed
// one, and again after a reissue. Status polls only read the stored result.
func (r *Registrar) refreshCertTrust(ctx context.Context) {
	if r.o.Certs == nil || ctx.Err() != nil {
		return
	}
	info := r.o.Certs.Info()
	if !info.Present || info.SelfSigned || r.certTrustFresh(info) {
		return
	}
	cfg := r.o.Config.Get()
	identity := cfg.Plex.CertHash
	if identity == "" {
		identity = info.Hash
	}
	addr := localPlexDirectDial(r.connections(cfg))
	host, port, _ := net.SplitHostPort(addr)
	// The listener can be checked even when no LAN HTTPS URI is advertised
	// (for example, LAN discovery failed or secure connections are disabled).
	// Use a name covered by the installed wildcard for SNI in that case.
	if host == "" && info.Hash != "" {
		host = "x." + info.Hash + ".plex.direct"
	}
	// Ether's own listener first (its bind address when it has one, else
	// loopback), then the LAN address the plex.direct name spells out, dialled
	// by IP. The name is only the TLS server name, so neither attempt needs
	// DNS: routers with rebinding protection and containers without hairpin
	// DNS cannot fail a working certificate (#251, #279). The name itself is
	// last, for setups where only it routes.
	selfHost := "127.0.0.1"
	if b := strings.TrimSpace(cfg.Server.BindAddress); b != "" {
		if ip := net.ParseIP(b); ip != nil && !ip.IsUnspecified() {
			selfHost = ip.String()
		}
	}
	dials := []string{net.JoinHostPort(selfHost, strconv.Itoa(serverPort(cfg)))}
	if ip, _, ok := remote.ParsePlexDirectHost(host); ok && ip != nil && port != "" {
		dials = append(dials, net.JoinHostPort(ip.String(), port))
	}
	dials = append(dials, addr)
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = r.o.Certs.VerifyTrust(vctx, identity, host, dials...)
	r.mu.Lock()
	r.trustAt = r.o.Now()
	r.trustFP = info.Fingerprint
	r.mu.Unlock()
}

func (r *Registrar) certTrustFresh(info CertInfo) bool {
	if r.o.Certs.trustPending(info.Fingerprint) {
		// One failed handshake is confirmed or cleared at the next tick
		// (Tick), so a restart never leaves a false warning behind.
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if info.Fingerprint == "" || info.Fingerprint != r.trustFP || r.trustAt.IsZero() {
		return false
	}
	wait := r.o.CertInterval
	if wait <= 0 {
		wait = 7 * 24 * time.Hour
	}
	if !info.Verified && info.VerifyReason == CertReasonHandshake {
		wait = time.Minute
	}
	return r.o.Now().Sub(r.trustAt) < wait
}

// scheduleCertCheck brings the next certificate check forward from
// CertInterval to when a failed plex.tv request may be retried, or to when
// the certificate enters its renewal window. Without it a request that
// failed once (a fresh install left on the self-signed fallback, or a
// renewal near expiry) waited a whole CertInterval instead of the
// manager's RetryAfter.
func (r *Registrar) scheduleCertCheck() {
	now := r.o.Now()
	next := now.Add(r.o.CertInterval)
	earlier := func(at time.Time) {
		if at.Before(next) {
			next = at
		}
	}
	info := r.o.Certs.Info()
	r.mu.Lock()
	certError := r.st.CertError
	r.mu.Unlock()
	if certError != "" && info.NextAttempt.IsZero() {
		earlier(now.Add(5 * time.Minute))
	}
	switch {
	case info.Custom:
	case !info.NextAttempt.IsZero():
		if info.NextAttempt.After(now) {
			earlier(info.NextAttempt)
		} else {
			earlier(now.Add(time.Minute))
		}
	case info.Present && !info.SelfSigned:
		at := info.NotAfter.Add(-r.o.Certs.renewBefore(info))
		if !at.After(now) {
			// Due but not requested by this node (e.g. a cluster
			// follower waiting for the leader): look again hourly.
			at = now.Add(time.Hour)
		}
		earlier(at)
	}
	r.schedule(taskCert, next)
}

// Claim claims the server for the account behind userToken (from PIN
// sign-in), persists the server token and owner, informs UserSync and
// triggers publishing.
func (r *Registrar) Claim(ctx context.Context, userToken string) (*ClaimResult, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	// Persist before the remote operation so a crash after claim cannot lose
	// the need to bind a managed certificate to the new device record.
	if r.o.Certs != nil {
		if err := r.o.Certs.RequireReissue(ctx); err != nil {
			return nil, fmt.Errorf("plextv: prepare certificate for claim: %w", err)
		}
	}
	res, err := r.client.Claim(ctx, userToken)
	if err != nil {
		return nil, err
	}
	if err := r.o.Config.Update(func(c *config.Config) error {
		c.Plex.Token = res.ServerToken
		c.Plex.OwnerPlexID = res.Owner.ID
		c.Plex.OwnerUsername = res.Owner.Username
		c.Plex.Claimed = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("plextv: persist claim: %w", err)
	}
	if r.o.Users != nil {
		if err := r.o.Users.SetOwner(ctx, res.Owner, res.ServerToken); err != nil {
			return nil, fmt.Errorf("plextv: set owner: %w", err)
		}
	}
	r.mu.Lock()
	r.claimedToken = res.ServerToken
	r.st.TokenRejected = false
	r.st.PlexPass = res.Owner.Subscription.Active
	r.st.OwnerFeatures = ownerFeatures(nil, res.Owner.Subscription.Features)
	r.next[taskFriends] = time.Time{}
	r.next[taskCert] = time.Time{}
	// The claim reads the account's subscription flag; the owner refresh
	// also consults the subscriptions list (lifetime passes), so run it on
	// the next tick.
	r.next[taskOwner] = time.Time{}
	r.mu.Unlock()
	featureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	features, featureErr := r.client.Features(featureCtx, res.ServerToken)
	cancel()
	if featureErr != nil {
		// Claim succeeded. Keep the legacy names and let the immediate owner
		// refresh retry this auxiliary request instead of undoing the claim.
		r.log.Warn("plex.tv owner features unavailable after claim", "err", featureErr)
	} else {
		r.mu.Lock()
		// The owner may have unclaimed or switched accounts during the fetch.
		if r.claimedToken == res.ServerToken {
			r.st.OwnerFeatures = ownerFeatures(features, res.Owner.Subscription.Features)
		}
		r.mu.Unlock()
	}
	r.refreshConfigStatus(r.o.Config.Get())
	r.log.Info("server claimed", "owner", res.Owner.Username)
	r.Trigger()
	return res, nil
}

// Unclaim signs the server out of plex.tv: its device record is removed
// from the owner's account (best effort), so another account can claim it,
// and the server token is forgotten.
func (r *Registrar) Unclaim(ctx context.Context) error {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if r.o.Certs != nil {
		if err := r.o.Certs.RequireReissue(ctx); err != nil {
			return fmt.Errorf("plextv: prepare certificate for unclaim: %w", err)
		}
	}
	token := r.o.Config.Get().Plex.Token
	r.mu.Lock()
	r.claimedToken = "" // released here, not again by onConfig
	r.mu.Unlock()
	if err := r.o.Config.Update(func(c *config.Config) error {
		c.Plex.Claimed = false
		return nil
	}); err != nil {
		return err
	}
	r.releaseClaim(ctx, token)
	return nil
}

// onConfig notices the owner switching Claimed off (Settings) and releases
// the claim. Without the release the old account keeps the device record
// and a later claim by another account is refused by plex.tv with 401s.
func (r *Registrar) onConfig(c config.Config) {
	enabled := plexAppsEnabled(c)
	r.publicationMu.Lock()
	changed := r.publicationEnabled != enabled
	r.publicationEnabled = enabled
	if !enabled && r.publicationCancel != nil {
		r.publicationCancel()
	}
	r.publicationMu.Unlock()
	if changed {
		select {
		case r.presenceWake <- struct{}{}:
		default:
		}
		if enabled {
			r.mu.Lock()
			r.next[taskCert] = time.Time{}
			r.next[taskFriends] = time.Time{}
			r.next[taskOwner] = time.Time{}
			r.mu.Unlock()
		}
		r.Trigger()
	}
	r.mu.Lock()
	prev := r.claimedToken
	if c.Plex.Claimed {
		r.claimedToken = c.Plex.Token
		r.mu.Unlock()
		return
	}
	r.claimedToken = ""
	r.mu.Unlock()
	if prev != "" {
		go func() {
			r.claimMu.Lock()
			defer r.claimMu.Unlock()
			// A newer claim supersedes this queued release.
			if r.o.Config.Get().Plex.Claimed {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if r.o.Certs != nil {
				if err := r.o.Certs.RequireReissue(ctx); err != nil {
					r.log.Warn("prepare certificate for unclaim", "err", err)
					return
				}
			}
			r.releaseClaim(ctx, prev)
		}()
	}
}

// releaseClaim removes this server's device record from the account behind
// token, then forgets token if it is still the stored one (a sign-in that
// happened meanwhile keeps its new token) and clears the plex.tv status.
func (r *Registrar) releaseClaim(ctx context.Context, token string) {
	r.mu.Lock()
	r.claimedToken = ""
	r.mu.Unlock()
	if token != "" {
		switch removed, err := r.client.RemoveDevice(ctx, token); {
		case err != nil:
			r.log.Warn("could not remove this server from the previous Plex account; remove it under Authorized Devices on plex.tv before claiming with another account", "err", err)
		case removed:
			r.log.Info("server removed from the previous Plex account")
		}
	}
	if err := r.o.Config.Update(func(c *config.Config) error {
		if !c.Plex.Claimed && c.Plex.Token == token {
			c.Plex.Token = ""
		}
		return nil
	}); err != nil {
		r.log.Warn("forget server token", "err", err)
	}
	r.mu.Lock()
	r.st.Listed = false
	r.st.PlexPass = false
	r.st.OwnerFeatures = nil
	r.st.PublishedURIs = nil
	r.st.ResourceConnections = nil
	r.friendFail = 0
	r.friendContention = 0
	r.st.LastPublishError = ""
	r.st.FriendSyncError = ""
	r.st.TokenRejected = false
	r.mu.Unlock()
	r.refreshConfigStatus(r.o.Config.Get())
}

// RefreshOwner re-reads the owner's account (Plex Pass state) and passes it
// to UserSync; call at startup.
func (r *Registrar) RefreshOwner(ctx context.Context) error {
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	acct, err := r.client.User(ctx, cfg.Plex.Token)
	if err != nil {
		return err
	}
	features, err := r.client.Features(ctx, cfg.Plex.Token)
	if err != nil {
		// An incomplete refresh must not erase a previously verified UUID
		// snapshot. runDue retries in ten minutes instead of six hours.
		return err
	}
	pass := acct.Subscription.Active
	if subs, serr := r.client.UserSubscriptions(ctx, cfg.Plex.Token); serr == nil {
		for _, s := range subs {
			if s.PlexID == acct.ID {
				if s.PlexPassKnown {
					pass = s.PlexPass
				}
				break
			}
		}
	}
	r.mu.Lock()
	if r.claimedToken != cfg.Plex.Token {
		r.mu.Unlock()
		return errors.New("plextv: owner changed during refresh")
	}
	r.st.PlexPass = pass
	r.st.OwnerFeatures = ownerFeatures(features, acct.Subscription.Features)
	r.next[taskOwner] = r.o.Now().Add(r.o.OwnerInterval)
	r.mu.Unlock()
	if r.o.Users != nil {
		return r.o.Users.SetOwner(ctx, *acct, cfg.Plex.Token)
	}
	return nil
}

// onPubsub handles events pushed over the presence stream.
func (r *Registrar) onPubsub(m PubsubMessage) {
	switch m.Command {
	case "notifyConnectivity":
		r.mu.Lock()
		// Like PMS, only trust the answer to our outstanding request.
		if r.asyncID == "" || m.AsyncIdentifier != r.asyncID {
			r.mu.Unlock()
			return
		}
		ok := m.Reachable()
		r.st.PlexReachable = &ok
		r.st.PlexReachableCheckedAt = r.o.Now()
		r.asyncID = ""
		r.mu.Unlock()
		attrs := []any{"reachable", ok, "direction", "inbound"}
		if m.Address != "" {
			attrs = append(attrs, "address", m.Address)
		}
		if m.Port != "" {
			attrs = append(attrs, "port", m.Port)
		}
		r.log.Info("plex.tv connectivity check", attrs...)
	case "refreshCertificate":
		r.schedule(taskCert, time.Time{})
		r.Trigger()
	case "notifyResetToken", "notifyAccountChange", "notifySharingChange", "notifyHomeChange":
		r.schedule(taskFriends, time.Time{})
		r.Trigger()
	default:
		r.log.Debug("pubsub event", "command", m.Command)
	}
}
