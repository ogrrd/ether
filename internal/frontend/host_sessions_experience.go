package frontend

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// This file holds the session/experience host contracts protocol adapters
// (Plex included) use instead of importing internal/sessions or
// internal/experience. Implementations of the pure policy helpers live here;
// stateful services stay in their domain packages and are wired by the app.

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy interface {
	Allowed(*core.User, *core.Title) bool
}

// TitleAllowed applies the mandatory content guards and the owner's policy.
func TitleAllowed(policy AccessPolicy, viewer *core.User, title *core.Title) bool {
	if title == nil || title.Adult {
		return false
	}
	if viewer != nil && contentrating.Exceeds(title.ContentRating, viewer.MaxContentRating) {
		return false
	}
	return policy == nil || policy.Allowed(viewer, title)
}

// SectionAllowed applies a viewer's library restrictions to an existing section.
// An empty restriction list means all sections, as it does for an owner.
func SectionAllowed(viewer *core.User, id int) bool {
	if viewer == nil || viewer.IsOwner || len(viewer.AllowedLibraries) == 0 {
		return true
	}
	for _, allowed := range viewer.AllowedLibraries {
		if allowed == core.LibraryID(id) {
			return true
		}
	}
	return false
}

// Visibility applies content, library and inherited show restrictions. The
// section lookup returns false for media types with no configured library.
type Visibility struct {
	Policy          AccessPolicy
	SectionFor      func(core.MediaType) (int, bool)
	GetTitle        func(context.Context, core.RatingKey) (*core.Title, error)
	FilterAvailable func(context.Context, []core.Title) []core.Title
	// JudgedOnFacts returns, for one request, whether FilterAvailable judges
	// a title on its full facts rather than its row. Nil with FilterAvailable
	// set means every title may be.
	JudgedOnFacts func(context.Context) func(core.Title) bool
	Sessions      func() []*core.PlaySession
}

// TitleFacts is what the access policy reads of a title beyond its row for
// viewer (core.TitleFacts). A policy that does not say may read any of them.
func (v Visibility) TitleFacts(viewer *core.User) core.TitleFacts {
	if v.Policy == nil {
		return 0
	}
	if p, ok := v.Policy.(interface {
		TitleFacts(*core.User) core.TitleFacts
	}); ok {
		return p.TitleFacts(viewer)
	}
	return core.VisibilityFacts
}

// FactJudge is JudgedOnFacts for one request: nil when no title is,
// false when the visibility cannot tell.
func (v Visibility) FactJudge(ctx context.Context) (func(core.Title) bool, bool) {
	if v.FilterAvailable == nil {
		return nil, true
	}
	if v.JudgedOnFacts == nil {
		return nil, false
	}
	return v.JudgedOnFacts(ctx), true
}

func (v Visibility) TitleAllowed(viewer *core.User, title *core.Title) bool {
	if !TitleAllowed(v.Policy, viewer, title) {
		return false
	}
	if v.SectionFor == nil {
		return true
	}
	id, ok := v.SectionFor(title.Type)
	return !ok || SectionAllowed(viewer, id)
}

// Playable checks inherited show policy without requiring discovery evidence.
func (v Visibility) Playable(ctx context.Context, viewer *core.User, title *core.Title) bool {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if !v.TitleAllowed(viewer, title) {
		return false
	}
	if title.ShowKey == 0 {
		return true
	}
	if v.GetTitle == nil {
		return false
	}
	show, err := v.GetTitle(ctx, title.ShowKey)
	return err == nil && v.TitleAllowed(viewer, show)
}

// Visible includes availability in addition to the playback policy.
func (v Visibility) Visible(ctx context.Context, viewer *core.User, title *core.Title) bool {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if !v.TitleAllowed(viewer, title) {
		return false
	}
	if len(v.Available(ctx, viewer, []core.Title{*title})) == 0 {
		return false
	}
	return v.Playable(ctx, viewer, title)
}

// Available filters discovery evidence, retaining a viewer's open sources.
// Callers must separately enforce title policy.
func (v Visibility) Available(ctx context.Context, viewer *core.User, titles []core.Title) []core.Title {
	if policy, ok := v.Policy.(interface {
		DiscoveryAllowed(*core.User, *core.Title) bool
	}); ok && !core.ExplicitSearch(ctx) {
		out := make([]core.Title, 0, len(titles))
		for _, title := range titles {
			if policy.DiscoveryAllowed(viewer, &title) {
				out = append(out, title)
			}
		}
		titles = out
	}

	ctx = core.WithCatalogViewer(ctx, viewer)
	if v.FilterAvailable == nil {
		return titles
	}
	filtered := v.FilterAvailable(core.WithCatalogViewer(ctx, viewer), titles)
	if len(filtered) == len(titles) || viewer == nil || v.Sessions == nil {
		return filtered
	}
	keep := map[core.RatingKey]bool{}
	for _, title := range filtered {
		keep[title.Key] = true
	}
	for _, session := range v.Sessions() {
		if session != nil && session.UserID == viewer.ID {
			keep[session.Key] = true
		}
	}
	out := make([]core.Title, 0, len(titles))
	for _, title := range titles {
		if keep[title.Key] {
			out = append(out, title)
		}
	}
	return out
}

// CollectionAllowed checks the library restriction for a collection.
func (v Visibility) CollectionAllowed(viewer *core.User, c *core.Collection) bool {
	if c.Type != core.TypeMovie && c.Type != core.TypeShow {
		return true
	}
	if v.SectionFor == nil {
		return true
	}
	id, ok := v.SectionFor(c.Type)
	return !ok || SectionAllowed(viewer, id)
}

// UpcomingItems retains the existing placeholder discovery rules for a page.
func (v Visibility) UpcomingItems(ctx context.Context, viewer *core.User, items []core.Title, now func() time.Time) []core.Title {
	ctx = core.WithCatalogViewer(ctx, viewer)
	nowVal := time.Now()
	if now != nil {
		nowVal = now()
	}
	allowed := make([]bool, len(items))
	future := make([]bool, len(items))
	var released []core.Title
	for i := range items {
		title := &items[i]
		if !v.Playable(ctx, viewer, title) || title.Released.IsZero() {
			continue
		}
		allowed[i] = true
		if title.IsFutureRelease(nowVal) {
			future[i] = true
		} else {
			released = append(released, *title)
		}
	}
	availSet := make(map[core.RatingKey]bool, len(released))
	if len(released) > 0 {
		for _, t := range v.Available(ctx, viewer, released) {
			availSet[t.Key] = true
		}
	}
	seen := make(map[core.RatingKey]bool, len(items))
	out := make([]core.Title, 0, len(items))
	for i, title := range items {
		if !allowed[i] || seen[title.Key] {
			continue
		}
		if !future[i] && !availSet[title.Key] {
			continue
		}
		seen[title.Key] = true
		out = append(out, title)
	}
	return out
}

// TitlesAllowed filters the title/section guard without discovery filtering.
func (v Visibility) TitlesAllowed(viewer *core.User, titles []core.Title) []core.Title {
	out := make([]core.Title, 0, len(titles))
	for i := range titles {
		if v.TitleAllowed(viewer, &titles[i]) {
			out = append(out, titles[i])
		}
	}
	return out
}

// MaySetViewerRating is the owner-console rule for changing a household rating cap.
// Delegated sessions and non-owners cannot write it.
func MaySetViewerRating(actor *core.Viewer, active *core.User) bool {
	return actor != nil && !actor.Delegated && active != nil && active.IsOwner
}

// ViewerRestricted chooses inherited visibility for limited or unknown policies.
func ViewerRestricted(policy AccessPolicy, u *core.User) bool {
	if u == nil {
		return true
	}
	if u.IsOwner {
		return false
	}
	if u.MaxContentRating != "" || len(u.AllowedLibraries) > 0 {
		return true
	}
	if policy == nil {
		return false
	}
	if p, ok := policy.(core.ArtworkAccessPolicy); ok {
		return p.Restricted(u)
	}
	return true
}

// HomeRevision stamps a viewer's cached Home. Viewer changes the Home must
// show on the very next request: the latest watch-state write (Continue
// Watching, watched badges, ratings), playlists, watchlist, favorites and
// "not interested" exclusions. Policy is the shared viewer-policy epoch:
// it also moves on catalogue and guide changes, so only restricted viewers
// treat it as a hard input.
type HomeRevision struct {
	Viewer string
	Policy int64
}

// ApplyMetadataDisplay applies a cached Plex metadata overlay's display
// fields to title.
func ApplyMetadataDisplay(title *core.Title, overlay MetadataOverlay) {
	plexmeta.ApplyDisplay(title, overlay)
}

// SaferMetadataRating is the stricter of a catalogue and overlay rating.
func SaferMetadataRating(catalogue, overlay string) string {
	return plexmeta.SaferRating(catalogue, overlay)
}

// MetadataOverlayRatingAllowed reports whether a cached metadata overlay's
// rating stays within viewer's content-rating cap when combined with the
// catalogue rating.
func MetadataOverlayRatingAllowed(viewer *core.User, catalogue, overlay string) bool {
	if viewer == nil || viewer.MaxContentRating == "" || overlay == "" {
		return true
	}
	return !contentrating.Exceeds(SaferMetadataRating(catalogue, overlay), viewer.MaxContentRating)
}

// DownloadsAllowed is shared capability admission; handlers still enforce it.
func DownloadsAllowed(viewer *core.User, ready, allowFriends bool, policy AccessPolicy) bool {
	if viewer == nil || !ready {
		return false
	}
	if viewer.IsOwner {
		return true
	}
	if p, ok := policy.(core.DownloadAccessPolicy); ok {
		return p.AllowDownloads(viewer)
	}
	return allowFriends
}

// WireID is the positive int32-range id Plex clients already parse.
// Offer rows use it directly and are not stream bindings.
func WireID(key core.RatingKey, slot string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(int64(key), 10) + "/" + slot))
	return int64(h.Sum64()%2_000_000_000) + 1
}

type Binding[R any] struct {
	ID          string // client "session" param
	StreamToken string // independent random capability; never a client-chosen ID
	PlayID      string // core.PlaySession.ID
	PlaybackID  string // explicit X-Plex-Session-IDentifier owning this encoder
	UserID      int64
	ClientID    string
	Key         core.RatingKey
	Requested   R // original Decision/start inputs, before policy refinement
	Decision    core.Decision
	Info        core.MediaInfo
	Subtitle    int // selected Subtitle's position in Info, -1 = none
	Caps        core.ClientCaps
	Opts        core.DecideOptions
	Offset      time.Duration
	Started     bool // StartHLS ran
	UpdatedAt   time.Time
	// A failed Decision for this binding (FailKey, FailErr at FailedAt):
	// the start request that follows it answers with the same error at
	// once instead of resolving again. One-shot and short-lived.
	FailKey  core.RatingKey
	FailErr  error
	FailedAt time.Time
}

type QueueItem struct {
	ID    int64
	Key   core.RatingKey
	Added bool // explicit Up Next addition, distinct from original queue members
}

type PlayQueue struct {
	ID               int64
	UserID           int64
	ClientID         string
	Items            []QueueItem
	Original         []QueueItem // order before shuffle
	Selected         int         // index into items
	Shuffled         bool
	SourceURI        string
	PlaylistID       core.RatingKey
	PlaylistTitle    string
	Version          int
	LastAdded        int64
	UpdatedAt        time.Time
	SuppressItem     int64
	SuppressRows     bool // mutation response only
	SuppressMetadata bool
	ForceOffsetZero  bool
	LiveSession      string // live queues resolve through LiveTV, never the catalogue

	Rev      int64     // clustered: stored row revision
	LoadedAt time.Time // clustered: when the cached copy was read or written
}

// PlayQueuePersistence connects shared queue transactions to node invalidation
// and outage reporting. Configure it before serving requests.
type PlayQueuePersistence struct {
	DB           *sql.DB
	Counter      func(context.Context, string, int) (int64, error)
	Invalidate   func(int64)
	Warn         func(string, error)
	ExpireResult func(error)
}

type DownloadItem struct {
	ID      int64
	QueueID int64
	Key     core.RatingKey
	Title   string // display Title, for the served filename

	Caps core.ClientCaps // as requested at add time (Download set at Run)
	User *core.User

	Status   string
	Error    string
	Progress float64 // 0..1
	Started  bool
	Cancel   context.CancelFunc
	// Run numbers each start of the Item, so a restarted job gets its own
	// playback session instead of sharing (and later stopping) the
	// previous Run's.
	Run int64

	Path       string
	Size       int64
	ReadyAt    time.Time
	VideoCodec string
	AudioCodec string
	Duration   time.Duration
	Bandwidth  int // requested maxVideoBitrate, kbps
	Burn       bool
}

type DownloadQueue struct {
	ID       int64
	UserID   int64
	ClientID string
	Items    []*DownloadItem
}

type DownloadStart struct {
	Item    *DownloadItem
	Context context.Context
	Run     int64
}

type DownloadRequest struct {
	Viewer    *core.User
	Item      core.RatingKey
	Caps      core.ClientCaps
	Run       int64
	NoRelease func(error) bool
	Prepared  func(*core.PlaySession) (core.DecideOptions, error)
	Progress  func(float64)
}

type DownloadPreparationError struct{ Err error }

type VariantRef struct {
	Key       core.RatingKey
	ReleaseID string
}

// StartHintKey names one remembered fallback. A title Key is the Dolby
// Vision HDR10-base hint for that title; a zero Key is client-wide: this
// viewer's client stalled on surround audio copied into segmented output.
type StartHintKey struct {
	UserID   int64
	ClientID string
	Key      core.RatingKey
}

// StreamBinding is one Plex-visible stream id. WireID is zero on bind;
// BindStream assigns it. SessionID is empty for library metadata and is
// the broker session id after Open.
type StreamBinding struct {
	WireID    int64
	SessionID string
	Key       core.RatingKey
	Index     int
	Info      core.StreamInfo
}

// PlanAdmission is the source and the version and track selections for one
// Plex plan. Plan makes the direct, remux, or transcode decision from them.
// Open publishes that decision. The adapter renders it and does not decide again.
type PlanAdmission struct {
	Source               *core.PlaySession
	Media                core.MediaInfo
	Caps                 core.ClientCaps
	Options              core.DecideOptions
	PreferRemux          bool
	Protocol             string
	RemoteMaxBitrateKbps int
}

// DecisionOwner identifies the authenticated public player, independently of
// its encoder delivery handle.
type DecisionOwner struct {
	ID                  string
	UserID              int64
	ClientID, SessionID string
}

// VersionFlowKey scopes issued versions to one viewer, client and title.
type VersionFlowKey struct {
	UserID   int64
	ClientID string
	Key      core.RatingKey
}

type VersionSnapshot struct {
	Variants []core.ReleaseVariant
	Frozen   bool
	Until    time.Time
}

// PlaybackActivity is one Plex client's playback as the session list shows it.
// The broker is the only store. Cluster rows for these activities use
// protocol plex.
type PlaybackActivity struct {
	ID                                                     string
	DecisionSeen                                           bool
	PlayingSeen                                            bool
	SessionKey                                             int
	User                                                   core.User
	ClientID, Product, Version, Platform, PlatformVersion  string
	Device, DeviceName, Model, Vendor, SessionID, RemoteIP string
	ProfileName                                            string
	Remote                                                 bool
	Key                                                    core.RatingKey
	State                                                  string
	Offset, Duration                                       time.Duration
	PlayQueueItemID                                        int64
	TranscodeID                                            string
	StartedAt, UpdatedAt                                   time.Time
}

// StartObservation is the evidence used by stalled-start detection.
type StartObservation interface {
	core.PlaybackPosition
	Decision() core.Decision
	Info() core.MediaInfo
}

type PartSource struct {
	Session               *core.PlaySession
	Matches               bool
	PreviousID            string
	AnonymousContinuation bool
}

func (q *PlayQueue) Snapshot() PlayQueue {
	cp := *q
	cp.Items = append([]QueueItem(nil), q.Items...)
	cp.Original = append([]QueueItem(nil), q.Original...)
	return cp
}

// writePlayQueue renders a queue window around the selected item.
func (q *PlayQueue) ReanchorAdded() {
	if q.LastAdded == 0 {
		return
	}
	for i := max(0, q.Selected+1); i < len(q.Items) && q.Items[i].Added; i++ {
		q.LastAdded = q.Items[i].ID
	}
}

func (q *DownloadQueue) Item(ID int64) *DownloadItem {
	for _, it := range q.Items {
		if it.ID == ID {
			return it
		}
	}
	return nil
}

func (e *DownloadPreparationError) Error() string { return e.Err.Error() }
func (e *DownloadPreparationError) Unwrap() error { return e.Err }

// Selections and BindingTable are generic and cannot be constructed
// through the SessionStores factory (Go interfaces have no generic
// methods); their small generic implementations live here and sessions
// aliases them.

// Selections stores per-viewer track choices. The adapter owns the value
// shape and translates wire stream identifiers before updating a choice.
type Selections[T any] struct {
	mu     sync.Mutex
	values map[[2]int64]T
}

func NewSelections[T any]() *Selections[T] { return &Selections[T]{values: map[[2]int64]T{}} }
func (s *Selections[T]) Get(viewer, item int64) T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[[2]int64{viewer, item}]
}
func (s *Selections[T]) Update(viewer, item int64, fn func(*T)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) > 100_000 {
		s.values = map[[2]int64]T{}
	}
	key := [2]int64{viewer, item}
	value := s.values[key]
	fn(&value)
	s.values[key] = value
}

// decisionFailureTTL bounds how long a failed Decision answers the start
// request that follows it; after that a start resolves afresh.
const decisionFailureTTL = 15 * time.Second

type BindingTable[R any] struct {
	mu sync.Mutex
	m  map[string]*Binding[R]
}

func NewBindingTable[R any]() *BindingTable[R] { return &BindingTable[R]{m: map[string]*Binding[R]{}} }

func (t *BindingTable[R]) Put(e *Binding[R]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.m[e.ID]; ok {
		if old.UserID != e.UserID || old.ClientID != e.ClientID {
			return
		}
		e.StreamToken = old.StreamToken
	}
	if e.StreamToken == "" {
		e.StreamToken = crand.Text()
	}
	cp := *e
	t.m[e.ID] = &cp
}

// Claim reserves a client-chosen ID before any playback/transcoder work. A
// different viewer or device cannot replace, reuse, stop, or probe that binding.
func (t *BindingTable[R]) Claim(ID string, UserID int64, ClientID string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[ID]; ok {
		return e.UserID == UserID && e.ClientID == ClientID
	}
	t.m[ID] = &Binding[R]{ID: ID, UserID: UserID, ClientID: ClientID, StreamToken: crand.Text(), UpdatedAt: now}
	return true
}

func (t *BindingTable[R]) Get(ID string) (*Binding[R], bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// Fail records a failed Decision on the binding the Decision claimed. A
// binding owned by another viewer or device is left alone.
func (t *BindingTable[R]) Fail(ID string, UserID int64, ClientID string, Key core.RatingKey, err error, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok || e.UserID != UserID || e.ClientID != ClientID || err == nil {
		return
	}
	e.FailKey, e.FailErr, e.FailedAt, e.UpdatedAt = Key, err, now, now
}

// TakeFailure returns and clears a recent failed Decision for this viewer's
// binding and title.
func (t *BindingTable[R]) TakeFailure(ID string, UserID int64, Key core.RatingKey, now time.Time) (error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[ID]
	if !ok || e.FailErr == nil {
		return nil, false
	}
	err, match := e.FailErr, e.UserID == UserID && e.FailKey == Key && now.Sub(e.FailedAt) < decisionFailureTTL
	if e.UserID == UserID {
		e.FailKey, e.FailErr, e.FailedAt = 0, nil, time.Time{}
	}
	if !match {
		return nil, false
	}
	return err, true
}

func (t *BindingTable[R]) Touch(ID string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[ID]; ok {
		e.UpdatedAt = now
	}
}

func (t *BindingTable[R]) Remove(ID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, ID)
}

// ForClient lists the transcode sessions of a client for a title.
func (t *BindingTable[R]) ForClient(UserID int64, ClientID string, Key core.RatingKey) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for ID, e := range t.m {
		if e.UserID == UserID && e.ClientID == ClientID && (Key == 0 || e.Key == Key) {
			out = append(out, ID)
		}
	}
	return out
}

func (t *BindingTable[R]) Expire(now time.Time, ttl time.Duration) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var gone []string
	for ID, e := range t.m {
		if now.Sub(e.UpdatedAt) > ttl {
			delete(t.m, ID)
			gone = append(gone, ID)
		}
	}
	return gone
}

// IDs lists every binding this table still holds.
func (t *BindingTable[R]) IDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.m))
	for id := range t.m {
		ids = append(ids, id)
	}
	return ids
}

// ByPlayID returns the transcode session ids fed by a playback session.
func (t *BindingTable[R]) ByPlayID(PlayID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for ID, e := range t.m {
		if e.PlayID == PlayID && e.Started {
			ids = append(ids, ID)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// Session store host contracts. Implementations live in internal/sessions;
// the app wires them through SessionStores. Value types shared across
// the boundary are defined in this file; sessions aliases them.

// PlaybackOwners is the v39 playback owner history.
type PlaybackOwners interface {
	Bind(user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time)
	BindFor(surface string, user int64, client string, key core.RatingKey, id, playID string, decision bool, now time.Time)
	Stale(user int64, client string, key core.RatingKey, id string) bool
	StaleFor(surface string, user int64, client string, key core.RatingKey, id string) bool
	Expire(now time.Time, ttl time.Duration)
}

// Lifecycle serializes playback lifecycle operations per key.
type Lifecycle interface {
	Acquire(ctx context.Context, key string) (func(), error)
	References(key string) int
}

// Coordination holds the owner history and lifecycle gates shared by every
// protocol adapter in one Ether process.
type Coordination interface {
	PlaybackOwners() PlaybackOwners
	LifecycleGates() Lifecycle
}

// DecisionArms records armed playback decisions per owner and title.
type DecisionArms interface {
	Expire(now time.Time)
	Arm(pr DecisionOwner, key core.RatingKey)
	Armed(pr DecisionOwner, key core.RatingKey) bool
	Disarm(pr DecisionOwner, key core.RatingKey)
	Refresh(now time.Time)
	ClearOwn()
}

// StartGuard watches stalled playback starts and start hints.
type StartGuard interface {
	Observe(tid, state string, offset, ahead time.Duration, now time.Time) bool
	Forget(tid string)
	Remember(k StartHintKey, now time.Time)
	BaseOnly(k StartHintKey, now time.Time) bool
	Expire(now time.Time)
}

// PlayQueues is the play-queue store.
type PlayQueues interface {
	Preallocate(keys []core.RatingKey) ([]QueueItem, error)
	CreateWith(userID int64, clientID string, keys []core.RatingKey, now time.Time, fn func(q *PlayQueue)) (PlayQueue, error)
	With(id, userID int64, now time.Time, fn func(q *PlayQueue)) (PlayQueue, bool)
	Get(id, userID int64) (PlayQueue, bool)
	SelectItem(userID, itemID int64)
	Expire(now time.Time, ttl time.Duration)
	AddItems(q *PlayQueue, keys []core.RatingKey, pre []QueueItem, next bool)
	Forget(id int64)
	DropCache()
	Reshuffle(q *PlayQueue, shuffled bool)
	Take(id, userID int64, clientID string, now time.Time) (PlayQueue, bool)
	Configure(now func() time.Time, persistence *PlayQueuePersistence)
	Persistent() bool
	Len() int
	FollowingPlayable(userID int64, key core.RatingKey, playable func(core.RatingKey) bool) core.RatingKey
	TimelineQueue(user int64, client string, key core.RatingKey, raw string, present bool) core.RatingKey
}

// VersionFlows stores version-picker flow snapshots.
type VersionFlows interface {
	Call(ctx context.Context, k VersionFlowKey, vs []core.ReleaseVariant, mode int, now time.Time, ttl time.Duration) (VersionSnapshot, error)
	Run()
}

// DownloadQueueStore is the download-queue store.
type DownloadQueueStore interface {
	QueueFor(UserID int64, ClientID string, create bool) *DownloadQueue
	QueueByID(ID, UserID int64, ClientID string) *DownloadQueue
	Add(q *DownloadQueue, u *core.User, t core.Title, caps core.ClientCaps, burn bool) *DownloadItem
	StartReady(ctx context.Context, perUser int, allowed func(*core.User) bool) []DownloadStart
	Expire(now time.Time, expiry time.Duration) []string
	StopForRestart()
	Remove(q *DownloadQueue, ids []int64) []*DownloadItem
	Restart(q *DownloadQueue, ids []int64, release func(string))
	CancelItem(it *DownloadItem) string
	Fail(ctx context.Context, it *DownloadItem, message string)
	Lock()
	Unlock()
	WakeDownloads()
	AllItems() []*DownloadItem
	Prepared(ctx context.Context, it *DownloadItem, info core.MediaInfo) bool
	Progress(ctx context.Context, it *DownloadItem, fraction float64)
	Complete(ctx context.Context, it *DownloadItem, path string, size int64, now time.Time) bool
}

// VariantIDs assigns stable ids to release variants.
type VariantIDs interface {
	IDFor(key core.RatingKey, releaseID string) int64
	Lookup(id int64) (VariantRef, bool)
	Start(ctx context.Context, wg *sync.WaitGroup)
	Flush(ctx context.Context) error
	Prune(ctx context.Context, liveIDs []int64, liveRefs []VariantRef) (int64, error)
}

// StreamTable is the Plex visible-id map.
type StreamTable interface {
	Bind(binding StreamBinding) (int64, error)
	Lookup(id int64) (StreamBinding, bool)
}

// StreamBindings is the visible-id and sidecar surface Plex calls.
type StreamBindings interface {
	BindStream(context.Context, SessionAccess, StreamBinding) (int64, error)
	ResolveStream(context.Context, SessionAccess, int64) (StreamBinding, error)
	OpenSidecar(context.Context, SessionAccess, auth.Acceptance, int64) (io.ReadCloser, core.StreamInfo, error)
}

// PartSources prepares and serves Plex direct-play part sources.
type PartSources interface {
	Prepare(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error)
	Part(viewer int64, client string, key core.RatingKey, release string) PartSource
	ServePart(w http.ResponseWriter, r *http.Request, sourceID string)
}

// DownloadJob runs one download preparation.
type DownloadJob interface {
	Run(ctx context.Context, q DownloadRequest) (string, int64, error)
}

// DownloadQueuePersistence persists the download queue.
type DownloadQueuePersistence interface {
	Retain(path string, ready time.Time)
	Save() error
	Load(ctx context.Context)
	Run(ctx context.Context)
}

// Estimator is the non-resolving playback surface for detail pages.
type Estimator interface {
	Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo
	SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool)
}

// Estimate returns an existing source's media information or a virtual
// estimate. It never prepares or resolves a release.
func Estimate(ctx context.Context, playback Estimator, viewer *core.Viewer, clientID string, title *core.Title, caps func() core.ClientCaps) (core.MediaInfo, *core.PlaySession) {
	if playback == nil {
		return core.MediaInfo{Estimated: true, Duration: title.Runtime}, nil
	}
	if viewer != nil {
		if ps, ok := playback.SessionFor(viewer.ID, clientID, title.Key); ok && ps != nil {
			return ps.Info, ps
		}
	}
	return playback.Estimate(ctx, title, caps()), nil
}

// PlaybackActivityStore is the broker's Plex session-list store.
type PlaybackActivityStore interface {
	HasActivity(id string) bool
	EnsureActivity(id string, key int)
	UpsertActivity(id string, fn func(*PlaybackActivity)) PlaybackActivity
	RemoveActivity(id string) (PlaybackActivity, bool)
	ListActivities() []PlaybackActivity
	ExpireActivities(now time.Time, ttl time.Duration) []PlaybackActivity
}

// PlaybackActivitySource reaches the Plex activity store from a session broker
// or its surface binding.
type PlaybackActivitySource interface {
	PlexActivities() PlaybackActivityStore
}

// SessionStores constructs the session stores Plex owns and provides
// the package-level session helpers Plex calls. Implemented in
// internal/sessions, wired by the app (and spike).
type SessionStores interface {
	NewCoordination() Coordination
	NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) DecisionArms
	NewStartGuard() StartGuard
	NewPlayQueues() PlayQueues
	NewVersionFlows(ctx context.Context, db *sql.DB) VersionFlows
	NewDownloadQueueStore() DownloadQueueStore
	NewVariantIDs(db *sql.DB, now func() time.Time) VariantIDs
	NewStreamTable() StreamTable
	NewDownloadJob(playback core.Playback, remuxer core.DownloadRemuxer) DownloadJob
	NewDownloadQueuePersistence(queue DownloadQueueStore, db *sql.DB, artifacts core.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) DownloadQueuePersistence
	NewPartSources(playback core.Playback, log *slog.Logger) PartSources
	ObserveStart(ctx context.Context, source core.HLSSession, id string) (StartObservation, bool)
	EncodeQueue(q *PlayQueue) (string, error)
	DecodeQueue(id, userID, rev, updated int64, data string) (*PlayQueue, error)
	ShuffleQueue(p *PlayQueue)
	UnshuffleQueue(p *PlayQueue)
	QueueItemIndex(items []QueueItem, id int64) int
	VariantHash(key core.RatingKey, releaseID string) int64
	UnderlyingActivityStore(s Sessions) PlaybackActivityStore
}

// PlanAdmission is the source and the version and track selections for one
// Plex plan. Plan makes the direct, remux, or transcode decision from them.
// Open publishes that decision. The adapter renders it and does not decide again.
type plexAdmissionKey struct{}

// WithPlanAdmission attaches the prepared source for one Plex Plan call.
func WithPlanAdmission(ctx context.Context, admit func(context.Context) (PlanAdmission, error)) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, plexAdmissionKey{}, admit)
}

// PlanAdmissionFrom returns the admission callback WithPlanAdmission attached.
func PlanAdmissionFrom(ctx context.Context) (func(context.Context) (PlanAdmission, error), bool) {
	admit, ok := ctx.Value(plexAdmissionKey{}).(func(context.Context) (PlanAdmission, error))
	return admit, ok && admit != nil
}

type plexServe struct {
	remote bool
	format string
}

type plexServeKey struct{}

// WithStreamServe carries the request's remote bit and subtitle format into
// ResolveStream and OpenSidecar without changing their signatures.
func WithStreamServe(ctx context.Context, remote bool, format string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, plexServeKey{}, plexServe{remote: remote, format: format})
}

// StreamServeFrom returns the remote bit and subtitle format WithStreamServe attached.
func StreamServeFrom(ctx context.Context) (remote bool, format string) {
	if ctx == nil {
		return false, ""
	}
	v, _ := ctx.Value(plexServeKey{}).(plexServe)
	return v.remote, v.format
}

// HLSDecision applies the v39 start-time decision rules. A client may reject
// direct play after its initial decision; in that case reconsider is called
// before selecting the segmented output. forcedStream, when supplied, applies
// adapter output constraints only when a still-direct decision is forced to HLS.
func HLSDecision(dec core.Decision, allowDirect bool, reconsider func() core.Decision, forcedStream func(core.Decision) core.Decision) core.Decision {
	if dec.Kind == core.DecisionDirectPlay && !allowDirect {
		dec = reconsider()
	}
	if dec.Kind == core.DecisionDirectPlay {
		dec.Kind, dec.VideoCopy, dec.AudioCopy, dec.Container = core.DecisionRemux, true, true, "mpegts"
		if forcedStream != nil {
			dec = forcedStream(dec)
		}
	}
	if dec.Container == "" {
		dec.Container = "mpegts"
	}
	return dec
}

// NotifyTranscodeActivity forwards timeline state to an active output. It is
// deliberately a no-op for transcoders and outputs without this capability.
func NotifyTranscodeActivity(transcoder core.Transcoder, id, state string) {
	if transcoder == nil {
		return
	}
	if output, ok := transcoder.Session(id); ok {
		if activity, ok := output.(core.SessionActivity); ok {
			activity.PlaybackState(state)
		}
	}
}

// Version flow modes for VersionFlows.Call.
const (
	VersionRead = iota
	VersionOffer
	VersionFreeze
	VersionEnd
	VersionTouch
)

// MaxPlayQueues bounds the play-queue store.
const MaxPlayQueues = 2000

// VersionPreplayTTL bounds a version flow's preplay snapshot.
const VersionPreplayTTL = 90 * time.Second

const (
	VariantIDBase  = 9_000_000_000_000_000
	VariantIDSpace = 7_199_254_740_990 // ids stay below 2^53
)

// VariantHash is the stable hash slot for a variant id.
func VariantHash(key core.RatingKey, releaseID string) int64 {
	h := fnv.New64a()
	h.Write([]byte(strconv.FormatInt(int64(key), 10) + "/" + releaseID))
	return VariantIDBase + int64(h.Sum64()%VariantIDSpace) + 1
}

// PostPlayRow is a viewer-scoped set of ranked recommendations.
// The presentation and protocol-specific identifiers belong to the
type PostPlayRow struct {
	Purpose core.RowPurpose
	Items   []core.Title
}

// DiscoveryCandidate contains a source row's neutral purpose and item candidates.
type DiscoveryCandidate struct {
	Index   int
	Purpose core.RowPurpose
	Items   []core.Title
}

// SearchSources are the parts of search every frontend shares: the
// catalogue (with the owner's availability rules applied), natural-language
// search, the viewer's affinity and collection readings. The app builds one
// set; each frontend adds only its own visibility, so a query finds the same
// titles in the same order on Plex, Jellyfin, Emby and the rest.
type SearchSources struct {
	Catalog  func(context.Context, string, []core.MediaType, int) ([]core.Title, error)
	Natural  func(context.Context, *core.User, string, int) ([]core.Title, bool)
	Familiar func(context.Context, *core.User, []core.RatingKey) map[core.RatingKey]int
	Vibe     VibeSource
	// Details reads in full the titles a candidate search returned partial
	// (core.SearchCandidates). Nil leaves every search reading its results
	// in full.
	Details func(context.Context, []core.Title) ([]core.Title, error)
}

// SearchResults retains whether an optional collection source handled the query.
// A handled empty result must not fall back to keyword search.
type SearchResults struct {
	Collection       *core.Collection
	Items            []core.Title
	CollectionSearch bool
}

// IssuedTrackIdentity is the selection resolvePlaySelection already holds
// after the Plex adapter has accepted a stream ID from the item-scoped
// issued registry. Positions are indexes into the full MediaInfo.Streams
// slice from that issuance. They are not StreamInfo.Index values and they
// are not audio-only ordinals.
//
// TranslatePlexTrackIdentity does not look up stream IDs, does not see the
// item, and does not authorize a caller-built StreamInfo. Copy Audio and
// Subtitle only from a registry reference whose item is the one being played.
//
// AudioSet must equal AudioPosition >= 0, which is how the play resolver
// derives audio set-ness. Pass AudioPosition -1 and AudioSet false when the
// client omitted audio. The zero value is not an omission.
//
// SubtitleSet false is omission. SubtitleSet true with a nil subtitle and
// position -1 is explicit subtitle-off. A nil subtitle with SubtitleSet true
// and any other position, including an out-of-range or wrong-type slot, is a
// legacy position-only choice and is preserved.
//
// The translator does not retain either pointer and does not pin a source.
type IssuedTrackIdentity struct {
	Audio            *core.StreamInfo
	AudioPosition    int
	AudioSet         bool
	Subtitle         *core.StreamInfo
	SubtitlePosition int
	SubtitleSet      bool
}

// TrackSelection holds positions in MediaInfo.Streams, not StreamInfo.Index.
// Set flags distinguish an omitted selection from position zero, and an
// explicit subtitle-off selection (-1) from an omitted subtitle choice.
type TrackSelection struct {
	Audio, Subtitle       int
	AudioSet, SubtitleSet bool
}

// TrackPreferenceSource supplies persisted viewer rules and show choices.
type TrackPreferenceSource interface {
	Select(context.Context, int64, core.RatingKey, core.MediaInfo, string) (trackprefs.Selection, error)
	GetRules(context.Context, int64) (trackprefs.Rules, error)
}

type ExtrasSource interface {
	Enabled() bool
	List(context.Context, *core.Title, string) ([]ExtraClip, error)
}

// FamiliarSource is a watch-state store that can report a viewer's
// history with titles (state.State).
type FamiliarSource interface {
	Familiar(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]int, error)
}

// VibeSource returns a viewer-scoped ephemeral collection and candidate items.
type VibeSource interface {
	VibeCollection(context.Context, *core.User, string) (*core.Collection, []core.Title, bool, error)
}

// HomeCandidate carries neutral row placement information. Index identifies
// the producer's candidate without exposing its wire representation.
type HomeCandidate struct {
	Index       int
	Purpose     core.RowPurpose
	Promoted    bool
	InLibraries bool
}

// HomeAssembly owns row inclusion, deduplication and ordering while adapters
// supply rendered projections and preserve their exact producer sidecars.
// Report records layout facts at paint time. Layout receives those facts and
// must not read the wire value again.
type HomeAssembly[P any] struct {
	Render      func(core.Hub) P
	Identity    func(P) string
	Report      func(P) (core.Hub, core.NeutralHomeRow)
	Continue    func() (core.Hub, P)
	Playlists   func() (P, bool)
	Discovery   func() []core.Hub
	Live        func() (P, bool)
	InLibraries func(core.Hub) bool
	Keep        func(P) bool
	Layout      func([]P, []core.Hub, []core.NeutralHomeRow) []P
}

func (a HomeAssembly[P]) Rows(hubs []core.Hub, includeContinue, promotedOnly, includePlaylists bool, purpose func(core.Hub) core.RowPurpose) ([]P, map[string]core.Hub) {
	var out []P
	var layoutHubs []core.Hub
	var neutral []core.NeutralHomeRow
	served := map[string]core.Hub{}
	appendRow := func(row P, source core.Hub, key string, record bool) {
		out = append(out, row)
		var hub core.Hub
		var fact core.NeutralHomeRow
		if a.Report != nil {
			hub, fact = a.Report(row)
		}
		layoutHubs = append(layoutHubs, hub)
		neutral = append(neutral, fact)
		if record {
			served[key] = source
		}
	}
	candidates := make([]HomeCandidate, len(hubs))
	for i, hub := range hubs {
		candidates[i] = HomeCandidate{Index: i, Purpose: purpose(hub), Promoted: hub.Promoted, InLibraries: a.InLibraries(hub)}
	}
	var projected P
	AssembleHome(candidates, includeContinue, promotedOnly, "home.continue", func() {
		hub, row := a.Continue()
		appendRow(row, hub, a.Identity(row), true)
	}, func(candidate HomeCandidate) string {
		projected = a.Render(hubs[candidate.Index])
		return a.Identity(projected)
	}, func(candidate HomeCandidate) {
		appendRow(projected, hubs[candidate.Index], a.Identity(projected), true)
	})
	if includePlaylists {
		if row, ok := a.Playlists(); ok {
			appendRow(row, core.Hub{}, "", false)
		}
	}
	for _, hub := range a.Discovery() {
		appendRow(a.Render(hub), hub, hub.ID, true)
	}
	if row, ok := a.Live(); ok {
		appendRow(row, core.Hub{}, "", false)
	}
	if a.Keep != nil {
		kept := make([]P, 0, len(out))
		keptHubs := make([]core.Hub, 0, len(out))
		keptNeutral := make([]core.NeutralHomeRow, 0, len(out))
		for i, row := range out {
			if !a.Keep(row) {
				continue
			}
			kept = append(kept, row)
			keptHubs = append(keptHubs, layoutHubs[i])
			keptNeutral = append(keptNeutral, neutral[i])
		}
		out, layoutHubs, neutral = kept, keptHubs, keptNeutral
	}
	if a.Layout != nil {
		out = a.Layout(out, layoutHubs, neutral)
	}
	return out, served
}

// AssemblePostPlay retains next-up, personal picks and related row ordering.
// Rendering runs at each original boundary, before fetching the next source.
func AssemblePostPlay[P any](ctx context.Context, selection PostPlaySelection, viewer *core.User, key core.RatingKey, count int,
	next func() (core.RatingKey, P, bool), picks func(PostPlayRow) (P, bool), render func(core.Hub) P) ([]P, error) {
	var out []P
	seen := map[core.RatingKey]bool{key: true}
	if key, row, ok := next(); ok {
		out = append(out, row)
		seen[key] = true
	}
	if row := selection.Row(ctx, viewer, count, seen); len(row.Items) > 0 {
		if projected, ok := picks(row); ok {
			out = append(out, projected)
		}
	}
	related, err := selection.RelatedRows(ctx, viewer, count, seen)
	if err != nil {
		return out, err
	}
	for _, row := range related {
		out = append(out, render(row))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Experience host contracts. Selector implementations live in
// internal/experience; adapters build them through Selectors
// constructors from the option value types below and call the selector
// interfaces. Generic home/postplay assembly orchestrates only these
// contracts, so it lives here like the generic session tables.

// Recent loads recently added and recently viewed rows.
type Recent interface {
	Added(ctx context.Context, viewer *core.User, kind core.MediaType, count int, browse func(context.Context, core.BrowseQuery) (core.Page, error)) ([]core.Title, bool, error)
	Viewed(ctx context.Context, viewer *core.User, libraryID int, history func(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error), titles func(context.Context, []core.RatingKey) []core.Title, libraryFor func(core.MediaType) (int, bool)) ([]core.Title, error)
}

// RecentOptions configures a Recent selector.
type RecentOptions struct {
	Visibility   Visibility
	Unrestricted func(*core.User) bool
}

// DetailRelated loads detail-page related rows.
type DetailRelated interface {
	SourceVisible(ctx context.Context, viewer *core.User, key core.RatingKey) bool
	Rows(ctx context.Context, viewer *core.User, key core.RatingKey, count int) ([]core.Hub, error)
}

// DetailRelatedOptions configures a DetailRelated selector.
type DetailRelatedOptions struct {
	Source     core.Recommender
	Visibility Visibility
	Get        func(context.Context, core.RatingKey) (*core.Title, error)
}

// TimelineStore persists one Plex timeline report through the host writer.
type TimelineStore interface {
	Store(ctx context.Context, user int64, key core.RatingKey, offset, duration time.Duration, status, queue string, offline *int) (context.Context, *TimelinePersistenceResult, error)
}

// TimelineStoreOptions configures a TimelineStore.
type TimelineStoreOptions struct {
	Writer          TimelineWriter
	Now             func() time.Time
	RestoreContinue func(context.Context, int64, core.RatingKey) error
}

// EpisodeOrderGrouped is the grouped-episode-order capability selectors use.
type EpisodeOrderGrouped interface {
	Grouped(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
}

// EpisodeBatch loads episode batches with watch state.
type EpisodeBatch interface {
	Load(ctx context.Context, viewer *core.User, ts []core.Title, selectionOnly bool) (map[core.RatingKey][]core.Title, map[core.RatingKey]core.WatchStatus)
}

// EpisodeBatchOptions configures an EpisodeBatch.
type EpisodeBatchOptions struct {
	Catalog      core.Catalog
	State        core.State
	EpisodeOrder EpisodeOrderGrouped
}

// ContinueDeck selects on-deck episodes per show.
type ContinueDeck interface {
	Prepare(ctx context.Context, shows []core.RatingKey)
	Select(ctx context.Context, show core.RatingKey) (*core.Title, map[core.RatingKey]core.WatchStatus)
}

// ContinueDeckOptions configures a ContinueDeck.
type ContinueDeckOptions struct {
	Load        func(context.Context, []core.RatingKey) (map[core.RatingKey][]core.Title, map[core.RatingKey]core.WatchStatus)
	Order       func(context.Context, core.RatingKey, []core.Title) ([]core.Title, error)
	Hydrate     func(context.Context, []core.RatingKey) []core.Title
	Unavailable func(context.Context, *core.Title) bool
	Fallback    func(context.Context, core.RatingKey, []core.Title, bool) (*core.Title, map[core.RatingKey]core.WatchStatus)
}

// Continue loads continue-watching and on-deck rows.
type Continue interface {
	WithoutFilter() Continue
	InProgress(ctx context.Context, viewer *core.User, t core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus)
	Row(ctx context.Context, viewer *core.User, kind core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus)
	Shelf(ctx context.Context, viewer *core.User, kind core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus, bool)
}

// ContinueOptions configures a Continue selector.
type ContinueOptions struct {
	ShowOnDeck         func(context.Context, core.RatingKey) (*core.Title, map[core.RatingKey]core.WatchStatus)
	PrepareShowOnDeck  func(context.Context, []core.RatingKey)
	PrefetchTitles     func(context.Context, []core.RatingKey)
	Filter             func(context.Context, *core.User, []core.Title) []core.Title
	State              core.State
	EpisodeOrder       EpisodeOrderGrouped
	Now                func() time.Time
	Grandchildren      func(context.Context, core.RatingKey) ([]core.Title, error)
	NextEpisode        func(context.Context, core.RatingKey) (*core.Title, error)
	NextEpisodes       func(context.Context, []core.RatingKey) (map[core.RatingKey]*core.Title, error)
	Visible            func(context.Context, *core.User, *core.Title) bool
	SameDayUnavailable func(context.Context, *core.Title) bool
	GetTitles          func(context.Context, []core.RatingKey) []core.Title
	SectionAllowed     func(*core.User, core.MediaType) bool
	OnError            func(error)
	FavoriteShows      func(context.Context, *core.User) (map[core.RatingKey]bool, error)
	Unrestricted       func(*core.User) bool
}

// RowSelection filters one hub row's items for a viewer.
type RowSelection interface {
	Items(ctx context.Context, viewer *core.User, row core.Hub) []core.Title
}

// RowSelectionOptions configures a RowSelection.
type RowSelectionOptions struct {
	Visibility          Visibility
	PrioritizeAvailable func(context.Context, []core.Title) []core.Title
	SameDayUnavailable  func(context.Context, *core.Title) bool
}

// HomeSelection loads home rows from a recommender.
type HomeSelection interface {
	Rows(ctx context.Context, viewer *core.User, hc core.HubContext, typ core.MediaType, count int) ([]core.Hub, error)
	Filter(ctx context.Context, viewer *core.User, hubs []core.Hub) []core.Hub
}

// HomeSelectionOptions configures a HomeSelection.
type HomeSelectionOptions struct {
	Source     core.Recommender
	Visibility Visibility
	Restricted bool
}

// ScopedCollection reads collections through viewer policy.
type ScopedCollection interface {
	Items(ctx context.Context, viewer *core.User, key core.RatingKey, deviceClass string) ([]core.Title, error)
	Get(ctx context.Context, viewer *core.User, key core.RatingKey, device string, count bool) (*core.Collection, error)
	Counts(ctx context.Context, viewer *core.User, collections []core.Collection, device string) []core.Collection
	Page(ctx context.Context, viewer *core.User, collection *core.Collection, key core.RatingKey, device string, offset, size int) (core.Page, error)
	Placeholder(ctx context.Context, viewer *core.User, key core.RatingKey, requested *core.RatingKey, device string) (*core.Title, core.RatingKey, error)
}

// ScopedCollectionOptions configures a ScopedCollection.
type ScopedCollectionOptions struct {
	Collections  core.Collections
	Visibility   Visibility
	Now          func() time.Time
	Unrestricted func(*core.User) bool
	TitleAllowed func(*core.User, *core.Title) bool
	Openable     func(context.Context, *core.User, []core.Title) []core.Title
}

// PlaylistSelection reads and expands playlists.
type PlaylistSelection interface {
	Get(ctx context.Context, viewer *core.User, key core.RatingKey) (*core.Playlist, error)
	Expand(ctx context.Context, viewer *core.User, key core.RatingKey) ([]core.RatingKey, error)
}

// PlaylistSelectionOptions configures a PlaylistSelection.
type PlaylistSelectionOptions struct {
	Playlists     core.Playlists
	Collections   core.Collections
	Visibility    Visibility
	Children      func(context.Context, core.RatingKey) ([]core.Title, error)
	Grandchildren func(context.Context, core.RatingKey) ([]core.Title, error)
}

// PlaylistExpandLimit bounds playlist expansion.
const PlaylistExpandLimit = 500

// PostPlaySelection loads post-play picks and related rows.
type PostPlaySelection interface {
	Row(ctx context.Context, viewer *core.User, count int, seen map[core.RatingKey]bool) PostPlayRow
	RelatedRows(ctx context.Context, viewer *core.User, count int, seen map[core.RatingKey]bool) ([]core.Hub, error)
}

// PostPlaySelectionOptions configures a PostPlaySelection.
type PostPlaySelectionOptions struct {
	Visibility   Visibility
	Unrestricted bool
	Picks        func(context.Context, int) []core.PostPlayPick
	Related      func(context.Context, int) ([]core.Hub, error)
}

// DiscoverySelection loads discovery candidates through visibility.
type DiscoverySelection interface {
	Load(ctx context.Context, viewer *core.User, limit int, load func(context.Context, int) []DiscoveryCandidate) []DiscoveryCandidate
}

// DiscoverySelectionOptions configures a DiscoverySelection.
type DiscoverySelectionOptions struct {
	Visibility   Visibility
	Unrestricted func(*core.User) bool
}

// TrackPreferenceSelector applies persisted track choices beneath explicit
// client choices. (TrackPreferences in this package is the values struct.)
type TrackPreferenceSelector interface {
	Select(ctx context.Context, viewer *core.User, key core.RatingKey, info core.MediaInfo, original string, selected TrackSelection) (TrackSelection, error)
	AudioFilters(ctx context.Context, viewer *core.User, info core.MediaInfo, selected TrackSelection) ([]string, error)
	SelectScoped(ctx context.Context, viewer *core.User, key core.RatingKey, info core.MediaInfo, original string, scope TrackScope, selected TrackSelection, scopedError func(error)) (TrackSelection, error)
	ScopedLimits(ctx context.Context, viewer *core.User, scope TrackScope, opts core.DecideOptions) (core.DecideOptions, error)
}

// TrackPreferenceSelectorOptions configures a TrackPreferenceSelector.
type TrackPreferenceSelectorOptions struct {
	Source   TrackPreferenceSource
	GetTitle func(context.Context, core.RatingKey) (*core.Title, error)
}

// Search is the shared search seen through one frontend's visibility.
type Search interface {
	Find(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error)
	Resolve(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int, collections bool) (SearchResults, error)
	Keyword(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error)
}

// Selectors constructs the experience selectors Plex uses and
// provides its package-level operations. Implemented in
// internal/experience, wired by the app (and spike).
type Selectors interface {
	NewRecent(RecentOptions) Recent
	NewDetailRelated(DetailRelatedOptions) DetailRelated
	NewTimelineStore(TimelineStoreOptions) TimelineStore
	NewEpisodeBatch(EpisodeBatchOptions) EpisodeBatch
	NewContinueDeck(ContinueDeckOptions) ContinueDeck
	NewContinue(ContinueOptions) Continue
	NewRowSelection(RowSelectionOptions) RowSelection
	NewHomeSelection(HomeSelectionOptions) HomeSelection
	NewScopedCollection(ScopedCollectionOptions) ScopedCollection
	NewPlaylistSelection(PlaylistSelectionOptions) PlaylistSelection
	NewPostPlaySelection(PostPlaySelectionOptions) PostPlaySelection
	NewDiscoverySelection(DiscoverySelectionOptions) DiscoverySelection
	NewTrackPreferenceSelector(TrackPreferenceSelectorOptions) TrackPreferenceSelector
	SearchFor(src SearchSources, vis Visibility, unrestricted func(*core.User) bool) Search
	StatisticsDefaults(dashboard Dashboard, resources ResourceStatistics, now func() time.Time) (Dashboard, ResourceStatistics)
	MergeShareFilter(filter, share core.Filter) (core.Filter, bool)
	ArrangeHome(ctx context.Context, source core.Recommender, viewer *core.User, hc core.HubContext, section string, rows []core.Hub, candidates []core.NeutralHomeRow) ([]int, error)
	DetailExtras(ctx context.Context, source ExtrasSource, parent *core.Title, language string, wanted map[int]bool) ([]ExtraClip, error)
	DiscoverySearch(source *DiscoveryFeatures) func(context.Context, *core.User, string, int) ([]core.Title, bool)
	FamiliarFrom(st any) func(context.Context, *core.User, []core.RatingKey) map[core.RatingKey]int
	ContinueFilter(state core.State, personal func(context.Context, *core.User, []core.Title) []core.Title) func(context.Context, *core.User, []core.Title) []core.Title
	SameDayUnavailable(ctx context.Context, title *core.Title, now time.Time, filter func(context.Context, []core.Title) []core.Title, source core.AvailabilitySource) bool
	SeasonNextEpisode(eps []core.Title, st map[core.RatingKey]core.WatchStatus, now time.Time, unavailable func(*core.Title) bool) *core.Title
	ShowOnDeck(eps []core.Title, st map[core.RatingKey]core.WatchStatus, unavailable func(*core.Title) bool) *core.Title
	RequestDatabaseObservation(ctx context.Context) (int64, time.Duration)
	PlaybackMarkerRanges(ctx context.Context, source MarkerRanges, key core.RatingKey, info core.MediaInfo) ([]MarkerRange, error)
}

// AssembleHome places the continue row at its ranked position and deduplicates
// rendered identifiers. The renderer returns the stable identifier it actually
// used; this can differ from the producer's candidate identifier.
func AssembleHome(candidates []HomeCandidate, includeContinue, promotedOnly bool, continueID string, renderContinue func(), render func(HomeCandidate) string, accept func(HomeCandidate)) {
	hasContinue := false
	for _, row := range candidates {
		if row.Purpose == core.RowPurpose("continue") {
			hasContinue = true
			break
		}
	}
	placed := false
	place := func() {
		if includeContinue && !placed {
			placed = true
			renderContinue()
		}
	}
	if !hasContinue {
		place()
	}
	seen := map[string]bool{continueID: true}
	for _, row := range candidates {
		if row.Purpose == core.RowPurpose("continue") {
			place()
			continue
		}
		if (promotedOnly && !row.Promoted) || !row.InLibraries {
			continue
		}
		id := render(row)
		if seen[id] {
			continue
		}
		seen[id] = true
		accept(row)
	}
}

// homePurpose translates producer identifiers at the Plex boundary; no wire
// context or hub identifier is included in the neutral candidate.
func HomePurpose(h core.Hub) core.RowPurpose {
	id := strings.ToLower(h.ID)
	switch {
	case id == "home.continue":
		return "continue"
	case strings.Contains(id, "upnext") || strings.Contains(id, "ondeck"):
		return "nextUp"
	case strings.Contains(id, "watchlist"):
		return "watchlist"
	case h.Key != 0 || strings.Contains(id, "collection"):
		return "collection"
	case strings.Contains(id, "recentlyreleased") || strings.Contains(id, "recently.released"):
		return "recentlyReleased"
	case strings.Contains(id, "livetv"):
		return "live"
	default:
		return "recommendation"
	}
}

// PickAudio selects an audio stream position, or -1 when none exists.
func PickAudio(info core.MediaInfo, langs []string) int {
	for _, lang := range langs {
		// Within one wanted language the best tier wins (exact before
		// generic-want×regional before regional-want×generic); preference
		// order across languages still dominates.
		best, bestTier := -1, 0
		for i, st := range info.Streams {
			if st.Type != "audio" {
				continue
			}
			if tier := langtag.MatchTier(lang, st.Language); tier > bestTier {
				best, bestTier = i, tier
			}
		}
		if best >= 0 {
			return best
		}
	}
	first := -1
	for i, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		if st.Default {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}

// ResolveSubtitle maps a stored choice onto a stream position, or -1.
// The choice is language, codec, and source (sidecar URL, or embedded
// versus external). A matching track at a new index beats whatever now
// sits in the old slot: variant lists reorder, and that slot is not the
// track the viewer picked.
func ResolveSubtitle(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	if want == nil {
		if pos >= 0 && pos < len(info.Streams) && info.Streams[pos].Type == "subtitle" {
			return pos
		}
		return -1
	}
	if want.Language == "" && want.ExternalURL == "" {
		if pos >= 0 && pos < len(info.Streams) && info.Streams[pos].Type == "subtitle" && info.Streams[pos].Index == want.Index {
			return pos
		}
		return -1
	}
	best, bestScore := -1, 0
	for i, st := range info.Streams {
		if st.Type != "subtitle" {
			continue
		}
		score := subtitleIdentityScore(*want, st)
		if score == 0 {
			continue
		}
		if i == pos {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// subtitleIdentityScore ranks how well st is the chosen subtitle. A sidecar
// URL is the source. Otherwise language is required, and the same codec and
// the same source kind (embedded or external) outrank a forced-flag or
// position tie.
func subtitleIdentityScore(want, st core.StreamInfo) int {
	if want.ExternalURL != "" && st.External && st.ExternalURL == want.ExternalURL {
		return 1000
	}
	if !sameTrackLanguage(st.Language, want.Language) {
		return 0
	}
	n := 10
	if subtitleCodec(want.Codec) != "" && subtitleCodec(st.Codec) == subtitleCodec(want.Codec) {
		n += 40
	}
	if st.External == want.External {
		n += 20
	}
	if st.Forced == want.Forced {
		n += 4
	}
	return n
}

func subtitleCodec(c string) string {
	c = strings.ToLower(c)
	if c == "subrip" {
		return "srt"
	}
	return c
}

func sameTrackLanguage(a, b string) bool { return a != "" && b != "" && langtag.MatchTier(a, b) > 0 }

// ResolveAudio maps an issued audio identity onto a position in the current
// MediaInfo.Streams. Callers needing an encoder index must translate that
// position through the selected StreamInfo.Index. The caller must supply a
// non-nil issued stream identity.
func ResolveAudio(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	best, bestScore := -1, -1
	for i, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		if want.Language == "" {
			if i == pos && sameAudioIdentity(*want, st) {
				return i
			}
			continue
		}
		tier := langtag.MatchTier(want.Language, st.Language)
		if tier == 0 {
			continue
		}
		score := tier * 32
		if st.Codec == want.Codec {
			score += 8
		}
		if st.Channels == want.Channels {
			score += 4
		}
		if st.Title == want.Title {
			score += 2
		}
		if i == pos {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

func sameAudioIdentity(a, b core.StreamInfo) bool {
	return a.Type == b.Type && a.Codec == b.Codec && a.Language == b.Language &&
		a.Channels == b.Channels && a.Title == b.Title && a.Forced == b.Forced &&
		a.External == b.External && a.ExternalURL == b.ExternalURL
}

// UnsupportedPlexTrackIdentityError means the issued selection is not a shape
// plex resolvePlaySelection produces. Callers must handle it themselves.
// The returned TrackSelection is empty and must not be applied.
// Every value of this type matches errors.Is, whatever Reason is set.
type UnsupportedPlexTrackIdentityError struct {
	Reason string
}

func (e UnsupportedPlexTrackIdentityError) Error() string {
	if e.Reason == "" {
		return "experience: unsupported plex track identity"
	}
	return "experience: unsupported plex track identity: " + e.Reason
}

func (UnsupportedPlexTrackIdentityError) Is(target error) bool {
	switch target.(type) {
	case UnsupportedPlexTrackIdentityError, *UnsupportedPlexTrackIdentityError:
		return true
	default:
		return false
	}
}

// TranslatePlexTrackIdentity maps an issued Plex selection onto the current
// probed MediaInfo the way plex resolvePlaySelection does, before viewer
// preferences and before applySelections.
//
// A nil audio identity leaves AudioPosition unchanged, including when it is
// negative, out of range, or not an audio stream. Preferences still see a
// nonnegative legacy position as set. A non-nil audio identity is passed to
// ResolveAudio only when that position is nonnegative. An unmatched result
// is position -1 and AudioSet false so preferences can run. The identity's
// Type is not rechecked.
//
// A nil subtitle identity leaves SubtitleSet and SubtitlePosition unchanged.
// SubtitleSet false becomes position -1. A non-nil subtitle identity is
// passed to ResolveSubtitle only when SubtitleSet is true. An unmatched
// result is position -1 and SubtitleSet false. Type is not rechecked.
//
// The result positions are indexes in probed.Streams. Encoder indexes still
// come from the selected StreamInfo.Index.
func TranslatePlexTrackIdentity(issued IssuedTrackIdentity, probed core.MediaInfo) (TrackSelection, error) {
	if issued.AudioSet != (issued.AudioPosition >= 0) {
		return TrackSelection{}, UnsupportedPlexTrackIdentityError{Reason: "audio set flag does not match a nonnegative position"}
	}
	out := TrackSelection{
		Audio:       issued.AudioPosition,
		AudioSet:    issued.AudioPosition >= 0,
		Subtitle:    issued.SubtitlePosition,
		SubtitleSet: issued.SubtitleSet,
	}
	if out.AudioSet && issued.Audio != nil {
		out.Audio = ResolveAudio(issued.Audio, issued.AudioPosition, probed)
		out.AudioSet = out.Audio >= 0
	}
	if !out.SubtitleSet {
		out.Subtitle = -1
		return out, nil
	}
	if issued.Subtitle == nil {
		return out, nil
	}
	if pos := ResolveSubtitle(issued.Subtitle, issued.SubtitlePosition, probed); pos >= 0 {
		out.Subtitle = pos
	} else {
		out.Subtitle, out.SubtitleSet = -1, false
	}
	return out, nil
}
