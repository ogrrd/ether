package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/trickplay"
)

// BrokerDeps are the explicit dependencies shared by all protocol surfaces.
type BrokerDeps struct {
	// OnActualPlaying runs in a bounded worker after authenticated playing telemetry.
	OnActualPlaying func(context.Context, *Broker, PlayingSubtitleSession)
	// DistributedHLS is opt-in; unsupported plans stay local before reservation.
	DistributedHLS DistributedHLS
	Coordination   *Coordination
	// MaxPendingStarts bounds active or waiting neutral stream starts.
	// Zero selects the default; the broker rejects values above 4096.
	MaxPendingStarts int
	DB               *sql.DB
	Cluster          clusterbus.Node
	Live             LiveBackend
	Variants         func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error)
	SelectTracks     func(context.Context, *core.Viewer, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	// SelectScopedTracks receives the authenticated device as well. If present,
	// it replaces SelectTracks and must preserve the unscoped behavior.
	SelectScopedTracks func(context.Context, *core.Viewer, core.Device, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	Lookup             func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	// MediaLookup checks current title policy using local facts only; it must
	// never resolve a release or fetch remote metadata on a segment request.
	// If nil, Lookup must satisfy the same requirement.
	MediaLookup   func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	Playback      core.Playback
	Transcoder    core.Transcoder
	Progress      ProgressWriter
	SourceHistory SourceHistoryWriter
	ActiveViewer  func(context.Context, int64) (*core.Viewer, error)
	CanPlay       func(*core.Viewer, bool, time.Time) bool
	// FreshPlay re-reads fenced membership and safety policy. A returned viewer
	// replaces the cached one for the rest of the call. ErrPolicyUnconfirmed
	// keeps the cached CanPlay decision for an unrestricted viewer.
	FreshPlay func(context.Context, *core.Viewer, bool, time.Time) (*core.Viewer, error)
	// ReceiptCurrent rechecks the acceptance Plex stored for this request.
	ReceiptCurrent     func(context.Context, auth.Acceptance) error
	BitrateLimit       func(*core.Viewer) int
	RemoteBitrateLimit func() int
	OnTimeline         func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string)
	Events             *EventBus
	OnEventError       func(error)
	// OnProfileDiagnostic is an optional fixture hook. Values are closed enums
	// only; the callback receives no media, identity or client-supplied data.
	OnProfileDiagnostic func(ProfileDiagnostic)
	// SourceMarkers returns a complete marker set for the exact source revision.
	SourceMarkers    func(context.Context, *core.Viewer, core.RatingKey, string, core.MediaInfo) ([]frontend.Marker, bool, error)
	OpenSubtitle     func(string, string) (io.ReadCloser, string, error)
	OpenSubtitleFrom func(string, string, string) (io.ReadCloser, string, error)
	ExtractSubtitle  func(context.Context, string, int, string, io.Writer) error
	// Trickplay is borrowed from the runtime owner (internal/trickplay/owner.go).
	// It is the same provider Plex uses. The broker releases its own leases;
	// only the runtime owner may Close the provider.
	Trickplay core.TrickplayProvider
	Now       func() time.Time
}

type brokerPlan struct {
	request  frontend.PlanRequest
	source   *core.PlaySession
	decision core.Decision
	created  time.Time
	// requested is when the play request that made this plan arrived.
	requested     time.Time
	etag          string
	live          bool
	exactRelease  string
	versionHandle string
	opening       bool
	pinned        bool
	// external is a Plex admission: the adapter already prepared the source.
	external bool
}
type brokerSession struct {
	observedSourceRevision string

	manualSubtitle        bool
	autoSubtitleIdentity  *int
	autoSubtitleQueued    bool
	autoSubtitleEpoch     uint64
	autoSubtitleCancel    context.CancelFunc
	distributed, restored bool
	distributedSaved      time.Time
	trickplay             *trickplay.Holder
	public                frontend.Session
	grant                 string
	playID                string
	clientID              string
	playbackID            string
	caps                  core.ClientCaps
	opts                  core.DecideOptions
	decision              core.Decision
	allowTranscoding      *bool
	format                frontend.StreamFormat
	streamPosition        time.Duration
	segmentDuration       time.Duration
	live                  bool
	sequence              uint64
	historyEpoch          int64
	historySequence       int64
	versionHandle         string
	exactRelease          string
	sourcePinned          bool
	sourceETag            string
	// transferBarrier is this session's admission ticket. A later open of the
	// same title, part and device reuses it. Stop drops it once no live
	// session still holds it; an unverified retirement leaves it until the
	// next admission evicts that idle ticket instead of refusing the viewer.
	// lazyHolds protect a source until its backend retirement succeeds.
	transferBarrier *capability.Header
	lazyHolds       []capability.Header
	// servingReads is one tracker per in-flight media request. Stop and
	// supersede wait until those producers release; the signal does not
	// itself free a source or capability claim.
	servingReads []*core.ReadRetirement
	// deliveryToken is this session's admission order. A later session can
	// replace the durable authority; an older token cannot.
	deliveryToken      int64
	deliveryGeneration int64
	deliverySession    string
}

type retiredSession struct {
	viewerID int64
	protocol frontend.Protocol
	deviceID string
	item     core.RatingKey
	until    time.Time
}

// Broker coordinates playback admission for every protocol, including Plex.
type Broker struct {
	autoSubtitleWG      sync.WaitGroup
	autoSubtitleRunning int
	sampleLimit         int
	deps                BrokerDeps
	startSlots          chan struct{}
	shutdownTimeout     time.Duration
	mu                  sync.Mutex
	plans               map[string]brokerPlan
	sessions            map[string]*brokerSession
	plexActivities      map[string]*PlaybackActivity
	plexActivityNext    int
	retired             map[string]retiredSession
	lazySessions        map[string]lazySession
	lazyCleanup         map[string]lazyCleanupEntry
	lazyCleanupRunning  bool
	lazyCleanupWait     time.Duration
	lazyCleanupReserved int
	lazyHeartbeatCount  int
	lazyTransfers       map[string]map[*lazyTransfer]context.CancelFunc
	lazyCleanupCancel   context.CancelFunc
	lazyCleanupDone     chan struct{}
	devices             map[deviceKey]frontend.SessionCapabilities
	playCapsByDevice    map[deviceKey]rememberedPlayCaps
	owners              *PlaybackOwners
	gates               *Lifecycle
	bindings            *BindingTable[frontend.PlayIntent]
	closed              bool
	pendingPlans        int
	pendingByViewer     map[int64]int
	sourceLeases        map[string]*sourceLease
	planSweepStop       chan struct{}
	planSweepDone       chan struct{}
	planSweepEvery      time.Duration
	detailPlans         map[string]*detailFlight
	cluster             *brokerCluster
	plexStreams         *StreamTable
	historyQueue        chan sourceHistorySample
	historyCancel       context.CancelFunc
	historyDone         chan struct{}
	holdMu              sync.Mutex
	holds               map[string]*capabilityHold
}

func NewBroker(d BrokerDeps) (*Broker, error) {
	if d.Lookup == nil || d.Playback == nil || d.ActiveViewer == nil || d.Events == nil || d.CanPlay == nil || d.BitrateLimit == nil || d.Progress == nil || d.OnTimeline == nil {
		return nil, errors.New("sessions: missing required dependency")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	coord := d.Coordination
	if coord == nil {
		coord = NewCoordination()
	}
	if coord.Owners == nil || coord.Gates == nil {
		return nil, errors.New("sessions: invalid coordination")
	}
	limit := d.MaxPendingStarts
	if limit <= 0 {
		limit = 128
	}
	if limit > 4096 {
		return nil, errors.New("sessions: max pending starts exceeds 4096")
	}
	b := &Broker{sampleLimit: 100000, deps: d, startSlots: make(chan struct{}, limit), shutdownTimeout: 10 * time.Second, lazyCleanupWait: 5 * time.Second, planSweepEvery: 5 * time.Second, plans: make(map[string]brokerPlan), pendingByViewer: make(map[int64]int), sourceLeases: make(map[string]*sourceLease), sessions: make(map[string]*brokerSession), plexActivities: make(map[string]*PlaybackActivity), retired: make(map[string]retiredSession), devices: make(map[deviceKey]frontend.SessionCapabilities), playCapsByDevice: make(map[deviceKey]rememberedPlayCaps), owners: coord.Owners, gates: coord.Gates, bindings: NewBindingTable[frontend.PlayIntent](), plexStreams: NewStreamTable()}
	if d, ok := b.deps.DistributedHLS.(interface {
		BindDelivery(func(http.ResponseWriter, *http.Request, string)) error
	}); ok {
		if err := d.BindDelivery(func(w http.ResponseWriter, r *http.Request, p string) { b.ServeMediaFor(frontend.Protocol(p), w, r) }); err != nil {
			return nil, err
		}
	}
	if d.DB != nil && d.Cluster != nil {
		var err error
		b.cluster, err = startBrokerCluster(b, d.DB, d.Cluster)
		if err != nil {
			return nil, err
		}
	}

	if d.SourceHistory != nil {
		ctx, cancel := context.WithCancel(context.Background())
		b.historyCancel = cancel
		b.historyQueue = make(chan sourceHistorySample, 256)
		b.historyDone = make(chan struct{})
		go func() { defer close(b.historyDone); b.runSourceHistory(ctx) }()
	}

	if remote, ok := b.deps.DistributedHLS.(neutralControl); ok {
		if err := remote.BindNeutralControl(b.neutralControl); err != nil {
			return nil, err
		}
	}
	return b, nil
}
func (b *Broker) viewer(ctx context.Context, v *core.Viewer) (*core.Viewer, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	active, err := b.deps.ActiveViewer(ctx, v.ID)
	if err != nil || active == nil || active.ID != v.ID {
		return nil, core.ErrUnauthorized
	}
	return active, nil
}
func (b *Broker) permitted(v *core.Viewer, caps core.ClientCaps) bool {
	return b.deps.CanPlay == nil || b.deps.CanPlay(v, caps.Remote, b.deps.Now())
}

// confirm applies a fenced playback policy when one is installed. The returned
// viewer is the one later lookups must use.
func (b *Broker) confirm(ctx context.Context, v *core.Viewer, caps core.ClientCaps) (*core.Viewer, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	at := time.Now()
	if b.deps.Now != nil {
		at = b.deps.Now()
	}
	if b.deps.FreshPlay != nil {
		fresh, err := b.deps.FreshPlay(ctx, v, caps.Remote, at)
		if err == nil && fresh != nil && fresh.ID == v.ID {
			return fresh, nil
		}
		if err != nil && !errors.Is(err, core.ErrPolicyUnconfirmed) {
			if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, core.ErrUnauthorized
		}
	}
	if !b.permitted(v, caps) {
		return nil, core.ErrUnauthorized
	}
	return v, nil
}

// ConfirmAdmission is the fenced policy check a Plex plan uses.
// planPlexAdmission already calls confirm with the request caps. This is
// the same check when the caller has only the remote bit.
func (b *Broker) ConfirmAdmission(ctx context.Context, v *core.Viewer, remote bool) (*core.Viewer, error) {
	return b.confirm(ctx, v, core.ClientCaps{Remote: remote})
}

func (b *Broker) publish(ctx context.Context, event frontend.Event) {
	if err := b.deps.Events.Publish(ctx, event); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}
func clientKey(protocol frontend.Protocol, device core.Device) string {
	return fmt.Sprintf("%d:%s%s", len(protocol), protocol, device.ID)
}
func copyRequest(r frontend.PlanRequest) frontend.PlanRequest {
	if r.Intent.AudioOrdinal != nil {
		ordinal := *r.Intent.AudioOrdinal
		r.Intent.AudioOrdinal = &ordinal
	}
	if r.Intent.SubtitleStreamIndex != nil {
		index := *r.Intent.SubtitleStreamIndex
		r.Intent.SubtitleStreamIndex = &index
	}
	if r.Intent.AllowTranscoding != nil {
		allowed := *r.Intent.AllowTranscoding
		r.Intent.AllowTranscoding = &allowed
	}
	r.Caps.VideoCodecs = slices.Clone(r.Caps.VideoCodecs)
	r.Caps.AudioCodecs = slices.Clone(r.Caps.AudioCodecs)
	r.Caps.TranscodeAudioCodecs = slices.Clone(r.Caps.TranscodeAudioCodecs)
	r.Caps.Containers = slices.Clone(r.Caps.Containers)
	r.Caps.HLSContainers = slices.Clone(r.Caps.HLSContainers)
	r.Caps.SubtitleCodecs = slices.Clone(r.Caps.SubtitleCodecs)
	r.Caps.DVProfiles = slices.Clone(r.Caps.DVProfiles)
	r.Caps.VideoLevels = maps.Clone(r.Caps.VideoLevels)
	r.Caps.VideoMaxHeights = maps.Clone(r.Caps.VideoMaxHeights)
	r.Caps.VideoProfiles = maps.Clone(r.Caps.VideoProfiles)
	for k, v := range r.Caps.VideoProfiles {
		r.Caps.VideoProfiles[k] = slices.Clone(v)
	}
	r.Caps.Profiles = cloneClientProfiles(r.Caps.Profiles)
	r.Intent.Options.AudioFilters = slices.Clone(r.Intent.Options.AudioFilters)
	if r.Intent.Hints != nil {
		p := *r.Intent.Hints
		if p.SubtitleStream != nil {
			s := copyStream(*p.SubtitleStream)
			p.SubtitleStream = &s
		}
		r.Intent.Hints = &p
	}
	return r
}
func copyStream(stream core.StreamInfo) core.StreamInfo {
	for _, field := range []**bool{&stream.DefaultDisposition, &stream.ForcedDisposition, &stream.HearingImpaired, &stream.Original, &stream.VisualImpaired, &stream.Commentary, &stream.Dub, &stream.HDR10Plus, &stream.DolbyVisionBLPresent, &stream.DolbyVisionELPresent, &stream.DolbyVisionRPUPresent, &stream.ProbedInterlaced, &stream.DolbyVisionConfigurationPresent, &stream.DolbyVisionBLCompatibilityIDPresent} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**int{&stream.NALLengthSize, &stream.ReferenceFrames, &stream.BitsPerRawSample, &stream.ProbedLevel, &stream.ProbedDolbyVisionProfile, &stream.ProbedDolbyVisionLevel} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**float64{&stream.AverageFrameRate, &stream.ReferenceFrameRate} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	return stream
}
func publicMedia(info core.MediaInfo) core.MediaInfo {
	info.Streams = slices.Clone(info.Streams)
	for i := range info.Streams {
		info.Streams[i] = copyStream(info.Streams[i])
		info.Streams[i].ExternalURL = ""
	}
	info.Chapters = slices.Clone(info.Chapters)
	info.Keyframes = slices.Clone(info.Keyframes)
	return info
}
func (b *Broker) Plan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	requested := b.deps.Now()
	req = copyRequest(req)
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Protocol == "" || req.Device.ID == "" || (req.Item == 0) == (req.Channel == "") {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if req.Protocol == "silo" && req.Intent.ClientSessionID != "" && !canonicalUUID(req.Intent.ClientSessionID) {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if req.Protocol == "plex" {
		if admit, ok := frontend.PlanAdmissionFrom(ctx); ok {
			return b.planPlexAdmission(ctx, req, admit)
		}
	}
	// Plex-specific version and stream identifiers remain on the existing Plex
	// path until its adapter can translate them into neutral selections.
	if req.Intent.Hints != nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	confirmed, err := b.confirm(ctx, v, req.Caps)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	v = confirmed
	viewerCopy := *v
	req.Viewer = &viewerCopy
	if req.Intent.DeclaredCaps && req.Item != 0 {
		b.rememberPlayCaps(v, req.Protocol, req.Device, req.Item, req.Caps)
	}
	req.Caps = b.playCaps(v, req.Protocol, req.Device, req.Caps)
	if b.deps.BitrateLimit != nil {
		if limit := b.deps.BitrateLimit(v); limit > 0 && (req.Intent.Options.MaxBitrateKbps == 0 || limit < req.Intent.Options.MaxBitrateKbps) {
			req.Intent.Options.MaxBitrateKbps = limit
		}
	}
	if req.Channel != "" {
		if req.Version != "" || req.Intent.ExactVersion || req.ExpectedSourceETag != "" || req.DetailSource != "" {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		return b.planLive(ctx, req, v)
	}
	title, err := b.deps.Lookup(ctx, v, req.Device, req.Item)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if title == nil || title.Key != req.Item {
		return frontend.PlayPlan{}, core.ErrNotFound
	}
	if !title.Released.IsZero() && title.Released.After(b.deps.Now()) {
		return frontend.PlayPlan{}, core.ErrUnreleased
	}
	var detail *detailSourceRecord
	if req.DetailSource != "" {
		if req.Version != "" || req.Intent.ExactVersion {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		record, err := b.resolveDetailSource(ctx, frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.DetailSource)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if record.item != req.Item || (req.ExpectedSourceETag != "" && req.ExpectedSourceETag != record.revision) {
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		req.ExpectedSourceETag = record.revision
		detail = &record
	}
	if req.Intent.Estimate && (req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil) {
		return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
	}
	if req.Intent.Estimate && req.Intent.ExactVersion {
		info := b.deps.Playback.Estimate(ctx, title, req.Caps)
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	versionHandle := ""
	var chosen *core.ReleaseVariant
	if req.Intent.ExactVersion && req.ResolvedRelease != "" {
		if req.Version == "" || b.deps.DB == nil || req.ResolvedRelease == "" || len(req.ResolvedRelease) > 512 {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		item, release, err := h.lookupRow(ctx, frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.Version)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if item != req.Item || release != req.ResolvedRelease {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
		versionHandle, req.Version = normalizeVersionHandle(req.Version), release
		chosen = &core.ReleaseVariant{ID: release}
	} else if req.Intent.ExactVersion {
		if req.Version == "" || b.deps.DB == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		scope := frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}
		item, release, err := h.resolveRecord(ctx, scope, req.Version)
		if errors.Is(err, core.ErrNotFound) {
			// An id names one release. When this viewer is no longer offered
			// it (the release went away), the title still plays: the
			// automatic pick, never the stale release itself.
			if parent, _, lerr := h.lookupRow(ctx, scope, req.Version); lerr == nil && parent == req.Item {
				item, release, err = parent, "", nil
				req.Intent.ExactVersion = false
			}
		}
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if item != req.Item {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
		versionHandle, req.Version = normalizeVersionHandle(req.Version), release
	}
	if chosen == nil && req.Version != "" {
		if b.deps.Variants == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		variants, err := b.deps.Variants(ctx, v, title, req.Caps)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		for j := range variants {
			if variants[j].ID == req.Version {
				chosen = &variants[j]
				break
			}
		}
		if chosen == nil && versionHandle != "" && len(variants) > 0 {
			chosen = &core.ReleaseVariant{ID: req.Version, Auto: true, Info: variants[0].Info}
		}
		if chosen == nil {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
	}
	if req.Intent.Estimate {
		var info core.MediaInfo
		if chosen != nil {
			info = chosen.Info
		} else {
			info = b.deps.Playback.Estimate(ctx, title, req.Caps)
		}
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	if !b.reservePlan(v.ID) {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	defer b.releasePlanReservation(v.ID)
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", v.ID, req.Caps.ClientID))
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	defer unlock()
	playback, ok := b.deps.Playback.(core.SurfaceScopedPlayback)
	if !ok {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	release := ""
	if chosen != nil {
		release = chosen.ID
	}
	prepareExact := req.Intent.ExactVersion
	rememberedFallback := false
	if remembered, rememberErr := b.rememberedDeliveryRelease(ctx, v.ID, string(req.Protocol), req.Device.ID, req.Item, req.Intent.ClientSessionID); rememberErr != nil {
		return frontend.PlayPlan{}, rememberErr
	} else if remembered != "" && release == "" && !prepareExact {
		release, prepareExact, rememberedFallback = remembered, true, true
	}
	previous, existed := playback.SessionForSurface(string(req.Protocol), v.ID, req.Caps.ClientID, req.Item, release, prepareExact)
	if detail != nil {
		if cached := b.localDetailSource(v.ID, req.Device.ID, req.Protocol, req.Item, *detail); cached != nil {
			previous, existed = cached, true
		}
	}
	useCachedDetail := detail != nil && existed && previous != nil && sourceRevision(previous) == detail.revision && previous.Resolution != nil && previous.Resolution.Release.ID == detail.release
	if detail != nil && !useCachedDetail {
		release, prepareExact, rememberedFallback = detail.release, true, false
		previous, existed = playback.SessionForSurface(string(req.Protocol), v.ID, req.Caps.ClientID, req.Item, release, prepareExact)
	}
	if req.ExpectedSourceETag != "" && detail == nil && (!existed || previous == nil || sourceRevision(previous) != req.ExpectedSourceETag) {
		return frontend.PlayPlan{}, frontend.ErrSourceGone
	}
	var ps *core.PlaySession
	prepStarted := time.Now()
	if useCachedDetail {
		if liveness, ok := b.deps.Playback.(interface{ SessionActive(string) bool }); ok && !liveness.SessionActive(previous.ID) {
			ps, err = playback.PrepareSurface(ctx, v, req.Item, req.Caps, release, prepareExact)
		} else {
			ps = previous
		}
	} else {
		ps, err = playback.PrepareSurface(ctx, v, req.Item, req.Caps, release, prepareExact)
		// If the remembered release (or a non-detail MediaSourceId handle when
		// no ExpectedSourceETag was pinned) is no longer available or changed,
		// fall back cleanly to the next healthy candidate instead of failing
		// replay with "not available yet: playback: source revision changed".
		if err != nil && detail == nil && req.ExpectedSourceETag == "" && (rememberedFallback || (chosen != nil && chosen.Auto) || errors.Is(err, core.ErrSourceChanged)) {
			if fallbackPS, fallbackErr := playback.PrepareSurface(ctx, v, req.Item, req.Caps, "", false); fallbackErr == nil && fallbackPS != nil {
				ps, err = fallbackPS, nil
				prepareExact = false
				req.Intent.ExactVersion = false
				if ps.Resolution != nil && ps.Resolution.Release.ID != "" {
					req.Version = ps.Resolution.Release.ID
					release = ps.Resolution.Release.ID
				}
			}
		}
	}
	if os.Getenv("ETHER_TRACE_PLAYBACK") != "" {
		slog.Info("playback trace: broker.Plan PrepareSurface",
			"item", req.Item,
			"protocol", req.Protocol,
			"release_req", release,
			"exact", prepareExact,
			"cached_detail", useCachedDetail,
			"existed", existed,
			"prepare_ms", time.Since(prepStarted).Milliseconds(),
			"err", err,
		)
	}
	if err != nil {
		if detail != nil && (errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrSourceChanged)) {
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		return frontend.PlayPlan{}, err
	}
	if ps == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if req.Intent.ExactVersion && (ps.Resolution == nil || ps.Resolution.Release.ID != req.Version) {
		if !existed || previous == nil || previous.ID != ps.ID {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if detail != nil && (ps.Resolution == nil || ps.Resolution.Release.ID != detail.release) {
		if !existed || previous == nil || previous.ID != ps.ID {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, frontend.ErrSourceGone
	}
	existed = existed && previous != nil && previous.ID == ps.ID
	b.acquirePlanSource(ps.ID, !existed)
	stored := false
	pinned := false
	defer func() {
		if !stored {
			b.releasePlanSource(ps.ID, pinned)
		}
	}()
	if req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil {
		if ps.Info.Estimated {
			return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
		}
		if req.ExpectedSourceETag == "" {
			req.ExpectedSourceETag = sourceRevision(ps)
			if req.ExpectedSourceETag == "" {
				return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
			}
		}
	}
	if req.ExpectedSourceETag != "" {
		pins, ok := b.deps.Playback.(core.PlaybackRevisionPin)
		if !ok {
			if req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil {
				return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
			}
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		if err := pins.PinSourceRevision(ctx, ps.ID, req.ExpectedSourceETag); err != nil {
			if ctx.Err() != nil {
				return frontend.PlayPlan{}, ctx.Err()
			}
			if errors.Is(err, core.ErrSourceChanged) {
				return frontend.PlayPlan{}, frontend.ErrSourceGone
			}
			return frontend.PlayPlan{}, err
		}
		pinned = true
		current, ok := b.deps.Playback.Session(ps.ID)
		if !ok || current == nil || sourceRevision(current) != req.ExpectedSourceETag {
			return frontend.PlayPlan{}, frontend.ErrSourceGone
		}
		ps = current
	}
	if err := bindSourceTracks(&req.Intent, ps.Info); err != nil {
		return frontend.PlayPlan{}, err
	}
	if b.deps.SelectScopedTracks != nil || b.deps.SelectTracks != nil {
		var selected core.DecideOptions
		var err error
		if b.deps.SelectScopedTracks != nil {
			selected, err = b.deps.SelectScopedTracks(ctx, v, req.Device, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		} else {
			selected, err = b.deps.SelectTracks(ctx, v, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		}
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		req.Intent.Options = selected
	}
	dec, err := b.decideProfile(ps, req)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding && decisionEncodes(dec, ps.Info) {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if (dec.Kind == core.DecisionDirectPlay && !req.Intent.Options.DirectPlay) || (dec.Kind == core.DecisionRemux && !req.Intent.Options.DirectStream) {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	id := rand.Text()
	etag := sourceRevision(ps)
	if req.Intent.SubtitleSet || (req.Intent.Hints != nil && req.Intent.Hints.ManualSubtitle) {
		if err := b.RecordAuthorizedSubtitleIntent(ctx, v.ID, req.Item, true); err != nil {
			return frontend.PlayPlan{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	exactRelease := ""
	if req.Intent.ExactVersion {
		exactRelease = req.Version
	} else if versionHandle != "" && ps.Resolution != nil && ps.Resolution.Release.ID != "" {
		exactRelease = ps.Resolution.Release.ID
	}
	if detail != nil {
		exactRelease = detail.release
	}
	b.plans[id] = brokerPlan{request: req, source: ps, decision: dec, created: b.deps.Now(), requested: requested, etag: etag, exactRelease: exactRelease, versionHandle: versionHandle, pinned: pinned}
	stored = true
	b.mu.Unlock()
	direct, remux, transcoding := b.planAlternatives(ps, req)
	hls, hlsContainer := b.hlsPlanOutput(ps, req, dec)
	return frontend.PlayPlan{ID: id, Media: publicMedia(ps.Info), Decision: dec, DirectPlay: direct, DirectStream: remux, Transcoding: transcoding, HLS: hls, HLSContainer: hlsContainer, ETag: etag}, nil
}
func (b *Broker) Open(ctx context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	opened := b.deps.Now()
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.Session{}, err
	}
	b.mu.Lock()
	p, ok := b.plans[req.PlanID]
	if b.closed {
		ok = false
	}
	if ok && (p.opening || p.request.Viewer.ID != v.ID || p.request.Protocol != req.Protocol || req.Device.ID == "" || p.request.Device.ID != req.Device.ID) {
		ok = false
	}
	if ok {
		p.opening = true
		b.plans[req.PlanID] = p
	}
	b.mu.Unlock()
	if !ok {
		return frontend.Session{}, core.ErrUnauthorized
	}
	gateCtx, cancelGate := context.WithTimeout(ctx, 10*time.Second)
	unlock, err := b.gates.Acquire(gateCtx, fmt.Sprintf("broker/owner/%d/%s", v.ID, p.request.Caps.ClientID))
	cancelGate()
	if err != nil {
		// Keep the scoped reference for the bounded sweeper; it must acquire
		// the owner gate before releasing a potentially shared source.
		b.mu.Lock()
		if current, exists := b.plans[req.PlanID]; exists && current.opening {
			current.opening = false
			current.created = b.deps.Now().Add(-planLifetime - time.Nanosecond)
			b.plans[req.PlanID] = current
		}
		b.mu.Unlock()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return frontend.Session{}, &frontend.SessionBusyError{RetryAfter: time.Second}
		}
		return frontend.Session{}, err
	}
	defer unlock()
	b.mu.Lock()
	current, exists := b.plans[req.PlanID]
	if exists && current.opening {
		delete(b.plans, req.PlanID)
	}
	b.mu.Unlock()
	if !exists || !current.opening {
		return frontend.Session{}, core.ErrNotAvailable
	}
	transferred := false
	defer func() {
		if !transferred && p.source != nil {
			b.releasePlanSource(p.source.ID, p.pinned)
		}
	}()
	if b.deps.Now().Sub(p.created) > planLifetime {
		return frontend.Session{}, core.ErrUnauthorized
	}
	v, err = b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.Session{}, core.ErrUnauthorized
	}
	v, err = b.confirm(ctx, v, p.request.Caps)
	if err != nil {
		return frontend.Session{}, err
	}
	if limit := b.deps.BitrateLimit(v); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
		return frontend.Session{}, core.ErrUnauthorized
	}
	if p.request.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
			return frontend.Session{}, core.ErrUnauthorized
		}
	}
	if p.live {
		return b.openLive(ctx, req, p, v)
	}
	title, err := b.deps.Lookup(ctx, v, req.Device, p.request.Item)
	if err != nil {
		return frontend.Session{}, err
	}
	if title == nil || title.Key != p.request.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return frontend.Session{}, core.ErrNotFound
	}
	if remote, ok := b.deps.Playback.(interface {
		RevalidateSession(context.Context, string) error
	}); ok {
		if err := remote.RevalidateSession(ctx, p.source.ID); err != nil {
			return frontend.Session{}, err
		}
	}
	ps, ok := b.deps.Playback.Session(p.source.ID)
	if !ok || ps == nil || ps.UserID != v.ID || ps.ClientID != p.request.Caps.ClientID || ps.Key != p.request.Item {
		return frontend.Session{}, core.ErrNotAvailable
	}
	if p.exactRelease != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != p.exactRelease) {
		return frontend.Session{}, core.ErrNotAvailable
	}
	// A plan's tag must not describe a source that failed over before Open.
	// Ask the frontend to re-plan instead of handing it a stale revision.
	if p.etag != "" && sourceRevision(ps) != p.etag {
		if p.pinned {
			return frontend.Session{}, frontend.ErrSourceGone
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	if remote, ok := b.deps.Playback.(interface {
		OpenRemoteSource(context.Context, string) error
	}); ok {
		if err := remote.OpenRemoteSource(ctx, p.source.ID); err != nil {
			return frontend.Session{}, err
		}
	}
	id, err := sessionID(req.Protocol)
	if err != nil {
		return frontend.Session{}, err
	}
	if binding, ok := b.deps.Playback.(interface {
		BindClientSession(context.Context, string, string) error
	}); ok {
		if err := binding.BindClientSession(ctx, id, ps.ID); err != nil {
			return frontend.Session{}, err
		}
	} else if req.Protocol == "jellyfin" || req.Protocol == "emby" {
		if binding, ok := b.deps.DistributedHLS.(interface {
			BindLocalClientSession(context.Context, string, *core.PlaySession) error
		}); ok {
			if err := binding.BindLocalClientSession(ctx, id, ps); err != nil {
				return frontend.Session{}, err
			}
		}
	}
	if req.Protocol == "plex" && p.external {
		if !validPlexTranscodeID(p.request.Intent.ClientSessionID) {
			return frontend.Session{}, core.ErrNotAvailable
		}
		id = p.request.Intent.ClientSessionID
		if reused, ok, reuseErr := b.reusePlexSession(ctx, v, req, p, ps, id); ok || reuseErr != nil {
			if ok {
				transferred = true
			}
			return reused, reuseErr
		}
	}
	grant := rand.Text()
	playbackID := p.request.Intent.PlaybackID
	if playbackID == "" && req.Protocol != "plex" {
		playbackID = id
	}
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/file", Protocol: req.Protocol, ViewerID: v.ID, Device: p.request.Device, Item: p.request.Item, StartedAt: b.deps.Now()}
	if req.Protocol == "silo" {
		session.ClientSessionID = p.request.Intent.ClientSessionID
	}
	if err := b.enrichCapabilities(ctx, &session); err != nil {
		return frontend.Session{}, err
	}
	deliverySession := p.request.Intent.ClientSessionID
	if deliverySession == "" {
		deliverySession = id
	}
	s := &brokerSession{autoSubtitleIdentity: new(int), manualSubtitle: p.request.Intent.SubtitleSet || (p.request.Intent.Hints != nil && p.request.Intent.Hints.ManualSubtitle), public: copySession(session), grant: grant, playID: ps.ID, clientID: p.request.Caps.ClientID, playbackID: playbackID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, allowTranscoding: p.request.Intent.AllowTranscoding, versionHandle: p.versionHandle, exactRelease: p.exactRelease, sourcePinned: p.pinned, sourceETag: p.etag, deliverySession: deliverySession}
	barrier, barrierErr := b.admitSessionBarrier(ctx, v.ID, string(req.Protocol), p.request.Device.ID, p.request.Item, ps.ID, id)
	if barrierErr != nil {
		if errors.Is(barrierErr, errTransferCommitUnknown) {
			// The durable row may exist. Keep the prepared source; do not Stop it.
			transferred = true
		}
		return frontend.Session{}, barrierErr
	}
	s.transferBarrier = barrier
	if b.cluster != nil {
		if err := b.cluster.store.put(ctx, session, ps.ID, *v, b.deps.Now()); err != nil {
			return frontend.Session{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, session.ID)
		}
		if s.transferBarrier != nil {
			transferred = true
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	if !p.external {
		if scoped, ok := b.deps.Playback.(core.SurfaceScopedPlayback); !ok || !scoped.MarkSurfaceOpened(ps.ID) {
			b.mu.Unlock()
			if b.cluster != nil {
				_ = b.cluster.store.remove(ctx, session.ID)
			}
			return frontend.Session{}, core.ErrNotAvailable
		}
	} else if scoped, ok := b.deps.Playback.(core.SurfaceScopedPlayback); ok && !scoped.MarkSurfaceOpened(ps.ID) {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, session.ID)
		}
		if s.transferBarrier != nil {
			transferred = true
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	b.bindings.Put(&Binding[frontend.PlayIntent]{ID: id, UserID: v.ID, ClientID: s.clientID, Key: session.Item, PlayID: ps.ID, PlaybackID: s.playbackID, Requested: p.request.Intent, Decision: p.decision, Info: ps.Info, Caps: s.caps, Opts: s.opts, UpdatedAt: b.deps.Now()})
	b.owners.BindFor("broker", v.ID, s.clientID, session.Item, s.playbackID, ps.ID, true, b.deps.Now())
	superseded := b.collectSupersededLocked(s)
	b.sessions[id] = s
	b.transferPlanSourceLocked(ps.ID)
	transferred = true
	b.mu.Unlock()
	if noter, ok := b.deps.Playback.(interface{ NotePlayRequest(string, time.Time) }); ok {
		// The play request is the plan's (Jellyfin PlaybackInfo plans and
		// opens in one request); a plan made earlier counts from this open.
		at := p.requested
		if at.IsZero() {
			at = opened
		}
		noter.NotePlayRequest(ps.ID, at)
	}
	if b.deps.SourceHistory != nil && session.Protocol != "plex" {
		// The epoch and the opened-file facts are durable bookkeeping. The
		// session URL does not wait for that write. Start after the session
		// is visible so the writer can find the row it updates.
		go b.publishOpenedSource(s)
	}
	b.finishSuperseded(ctx, superseded, ps.ID)
	if b.cluster != nil {
		b.cluster.ownershipChanged(ctx)
	}
	b.publish(ctx, sessionEvent("SessionStarted", session, nil))
	return session, nil
}
func (b *Broker) access(ctx context.Context, a frontend.SessionAccess, id string) (*brokerSession, *core.Viewer, error) {
	return b.accessSession(ctx, a, id, true)
}

func (b *Broker) accessSession(ctx context.Context, a frontend.SessionAccess, id string, enforcePlaybackPolicy bool) (*brokerSession, *core.Viewer, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, nil, core.ErrNotAvailable
	}
	s, ok := b.sessions[id]
	if ok {
		copy := *s
		s = &copy
	}
	b.mu.Unlock()
	if !ok {
		if err := b.restoreNeutralBinding(ctx, id, &a, "", string(a.Protocol)); err != nil {
			return nil, nil, err
		}
		b.mu.Lock()
		s = b.sessions[id]
		if s != nil {
			copy := *s
			s = &copy
		}
		b.mu.Unlock()
		if s == nil {
			return nil, nil, core.ErrNotFound
		}
	}
	if !b.neutralCurrent(ctx, s) {
		return nil, nil, core.ErrNotAvailable
	}
	owner := a.Owner && viewer.IsOwner && !viewer.Delegated
	if !owner && (s.public.ViewerID != viewer.ID || s.public.Protocol != a.Protocol || a.Device.ID == "" || s.public.Device.ID != a.Device.ID) {
		return nil, nil, core.ErrUnauthorized
	}
	if a.Owner && !owner {
		return nil, nil, core.ErrUnauthorized
	}
	if enforcePlaybackPolicy && !owner {
		confirmed, err := b.confirm(ctx, viewer, s.caps)
		if err != nil {
			return nil, nil, err
		}
		viewer = confirmed
	}
	return s, viewer, nil
}
func (b *Broker) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	return b.deps.Events.Subscribe(ctx, scope)
}
func (b *Broker) Recover(ctx context.Context, id, url string, at time.Duration) error {
	return Recover(ctx, b.bindings, b.deps.Playback, id, url, at)
}

// HasBinding reports whether this broker owns an encoder ID.
func (b *Broker) HasBinding(id string) bool { _, ok := b.bindings.Get(id); return ok }
func (b *Broker) TranscodeFor(_ context.Context, id string) (core.HLSSession, bool) {
	return TranscodeFor(b.bindings, b.deps.Transcoder, id)
}
func (b *Broker) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return nil, core.ErrUnauthorized
	}
	if err = b.restoreNeutralScope(ctx, a); err != nil {
		return nil, err
	}
	b.mu.Lock()
	liveness, checkLiveness := b.deps.Playback.(interface{ SessionActive(string) bool })
	var out []frontend.Session
	for _, s := range b.sessions {
		if checkLiveness && !s.live && s.playID != "" && !liveness.SessionActive(s.playID) {
			continue
		}
		if owner || s.public.ViewerID == v.ID {
			out = append(out, s.public)
		}
	}
	b.mu.Unlock()
	if b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range remote {
			if owner || r.session.ViewerID == v.ID {
				out = append(out, r.session)
			}
		}
	}
	visible := out[:0]
	for i := range out {
		if b.cluster != nil && out[i].Protocol != "plex" {
			active, err := b.cluster.store.current(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			if !active {
				continue
			}
		}
		if err := b.enrichCapabilities(ctx, &out[i]); err != nil {
			return nil, err
		}
		visible = append(visible, out[i])
	}
	return visible, nil
}
func (b *Broker) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if _, handled, err := b.forwardNeutralControl(ctx, a, id, "report", t, frontend.StreamRequest{}); handled {
		return err
	}
	s, _, err := b.accessSession(ctx, a, id, t.State != "stopped")
	if errors.Is(err, core.ErrNotFound) {
		item, retired, retiredErr := b.retiredAccess(ctx, a, id)
		if retiredErr != nil {
			return retiredErr
		}
		if retired {
			// Static playback opens a second session and retires the one
			// PlaybackInfo minted. Infuse keeps reporting that first id.
			// A bare stop still does not rewind a resume point.
			if item != 0 && (a.Protocol == "jellyfin" || a.Protocol == "emby") && (t.Position > 0 || t.Duration > 0) {
				if err := b.recordItemTimeline(ctx, a, id, item, t); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if errors.Is(err, core.ErrNotFound) && b.cluster != nil {
		return b.cluster.forwardReport(ctx, a, id, t)
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, v, err := b.accessSession(ctx, a, id, t.State != "stopped")
	if err != nil {
		return err
	}
	if v.ID != s.public.ViewerID {
		v, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
		if err != nil || v == nil {
			return core.ErrUnauthorized
		}
	}
	// A revoked session may still stop and release resources, but cannot write
	// further progress after its title or channel becomes inaccessible.
	if !b.currentMediaPolicy(ctx, v, s) {
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return core.ErrUnauthorized
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		active, err := b.cluster.store.active(ctx, s.public.ID)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
	}
	report, sequenced := ctx.Value(sampleReportKey{}).(*sampleReport)
	accepted, rollbackSequence := true, func() {}
	if !sequenced {
		accepted, rollbackSequence, err = b.acceptSequence(ctx, s, t.Sequence)
	}
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	if err := b.enrichCapabilities(ctx, &s.public); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
	if !s.live {
		reportedID := t.PlaybackID
		if reportedID == s.public.ID {
			reportedID = s.playbackID
		}
		if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, reportedID) {
			rollbackSequence()
			if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
				return b.stopLocked(ctx, s)
			}
			return nil
		}
	}
	if s.live {
		b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return nil
	}
	if err := b.observeSubtitleSelection(ctx, s, t); err != nil {
		rollbackSequence()
		return err
	}
	if t.Duration <= 0 {
		if source, ok := b.deps.Playback.Session(s.playID); ok && source != nil && source.Info.Duration > 0 {
			t.Duration = source.Info.Duration
		}
	}
	// Keep active broker ownership alive when Plex's shared v39 history
	// expires idle entries. Never refresh a superseded binding.
	if !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		b.owners.BindFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID, s.playID, false, b.deps.Now())
	}
	originSession := s.public.ID
	if s.public.Protocol == "plex" && s.playbackID != "" {
		originSession = s.playbackID
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.public.Protocol, SessionID: originSession, ClientID: s.public.Device.ID})
	if sequenced {
		err = b.deps.Progress.(receiptProgress).ProgressWithReceipt(ctx, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State, b.sampleReceipt(ctx, s, report))
	} else {
		err = RecordProgress(ctx, b.deps.Progress, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State)
	}
	if err != nil {
		rollbackSequence()
		if sequenced && errors.Is(err, errSampleNotApplied) && report.outcome == frontend.ReportReplayed && report.sample.Stop {
			return b.stopLocked(ctx, s)
		}
		return err
	}
	if sequenced {
		report.outcome = frontend.ReportApplied
	}
	if s.format != "" {
		b.recordSourceHistory(ctx, s)
	}
	if toucher, ok := b.deps.Playback.(interface{ Touch(string) }); ok && (t.State == "playing" || t.State == "paused") {
		toucher.Touch(s.playID)
	}
	NotifyTranscodeActivity(b.deps.Transcoder, s.public.ID, t.State)
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(s.public.ViewerID, s.clientID, s.public.Item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(v, s.public.Item, t.Position, t.Duration, t.State)
	}
	b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
	if t.State == "stopped" {
		return b.stopLocked(ctx, s)
	}
	if err = b.saveNeutralBinding(ctx, id, true); err != nil {
		return err
	}
	b.ensureTrickplay(ctx, s, t.Position)
	if t.State == "playing" {
		b.enqueueSubtitleWork(s)
	}
	return nil
}

// recordItemTimeline writes a Jellyfin or Emby watch update without stopping
// the playback that replaced this session.
func (b *Broker) recordItemTimeline(ctx context.Context, a frontend.SessionAccess, sessionID string, item core.RatingKey, t frontend.Timeline) error {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || viewer == nil {
		return core.ErrUnauthorized
	}
	origin := t.PlaybackID
	if origin == "" {
		origin = sessionID
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: a.Protocol, SessionID: origin, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, clientKey(a.Protocol, a.Device), item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	return nil
}

// ReportItemProgress records a Jellyfin or Emby timeline that names an item
// instead of a broker session.
func (b *Broker) ReportItemProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if a.Protocol != "jellyfin" && a.Protocol != "emby" {
		return core.ErrUnauthorized
	}
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || a.Device.ID == "" || item == 0 {
		return core.ErrUnauthorized
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	if lookup == nil {
		return core.ErrNotAvailable
	}
	title, err := lookup(ctx, viewer, a.Device, item)
	if err != nil || title == nil || title.Key != item {
		if err != nil {
			return err
		}
		return core.ErrNotFound
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: a.Protocol, SessionID: t.PlaybackID, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, clientKey(a.Protocol, a.Device), item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	if t.State == "stopped" {
		clientID := clientKey(a.Protocol, a.Device)
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", viewer.ID, clientID))
		if err != nil {
			return err
		}
		defer unlock()
		b.mu.Lock()
		var toStop []*brokerSession
		for _, s := range b.sessions {
			if s.public.ViewerID == viewer.ID && s.public.Protocol == a.Protocol && s.public.Device.ID == a.Device.ID && s.public.Item == item {
				toStop = append(toStop, s)
			}
		}
		b.mu.Unlock()
		for _, s := range toStop {
			if err := b.stopLocked(ctx, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReportProgress records a Plex timeline that has no playback source.
// It does not prepare or stop one.
func (b *Broker) ReportProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || a.Protocol != "plex" || a.Device.ID == "" || item == 0 {
		return core.ErrUnauthorized
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: "plex", SessionID: t.PlaybackID, ClientID: a.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, viewer.ID, item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(viewer.ID, a.Device.ID, item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(viewer, item, t.Position, t.Duration, t.State)
	}
	return nil
}
func (b *Broker) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	if _, handled, err := b.forwardNeutralControl(ctx, a, id, "stop", frontend.Timeline{}, frontend.StreamRequest{}); handled {
		return err
	}
	s, _, err := b.accessSession(ctx, a, id, false)
	if errors.Is(err, core.ErrNotFound) {
		if b.cluster == nil {
			return nil
		}
		v, err := b.viewer(ctx, a.Viewer)
		if err != nil {
			return err
		}
		owner := a.Owner && v.IsOwner && !v.Delegated
		if a.Owner && !owner {
			return core.ErrUnauthorized
		}
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return err
		}
		for _, r := range remote {
			if r.session.ID != id {
				continue
			}
			if !owner && (r.session.ViewerID != v.ID || r.session.Protocol != a.Protocol || a.Device.ID == "" || r.session.Device.ID != a.Device.ID) {
				return core.ErrUnauthorized
			}
			payload, err := json.Marshal(remoteStop{Node: r.node, ID: id, ActorID: v.ID, DeviceID: a.Device.ID, Protocol: a.Protocol, Owner: owner})
			if err != nil {
				return err
			}
			return b.cluster.node.Publish(ctx, brokerStopTopic, payload)
		}
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, viewer, err := b.accessSession(ctx, a, id, false)
	if err != nil {
		return err
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err == nil && viewer != nil && !s.live && s.format != "" && !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) && b.currentMediaPolicy(ctx, viewer, s) {
		b.recordSourceHistory(ctx, s)
	}
	return b.stopLocked(ctx, s)
}
func (b *Broker) stopLocked(ctx context.Context, s *brokerSession) error {
	if s.autoSubtitleCancel != nil {
		s.autoSubtitleCancel()
	}
	if s.distributed {
		if storage, ok := b.deps.DistributedHLS.(neutralBindingStore); ok {
			if err := storage.DeleteNeutralBinding(ctx, s.public.ID); err != nil {
				return err
			}
		}
	}
	b.mu.Lock()
	current, ok := b.sessions[s.public.ID]
	var hold *trickplay.Holder
	if ok {
		hold = current.trickplay
	}
	stopSource := false
	var reads []*core.ReadRetirement
	if ok {
		s = current
		reads = slices.Clone(s.servingReads)
		delete(b.sessions, s.public.ID)
		b.rememberRetiredLocked(s)
		if !s.live {
			stopSource = b.releaseSessionSourceLocked(s.playID)
		}
	}
	transfers := b.lazyTransfers[s.public.ID]
	delete(b.lazyTransfers, s.public.ID)
	b.mu.Unlock()
	waitReadRetirement(reads)
	if hold != nil {
		// Plex applies its own idle rule after the stop, so other sources of
		// the same release keep their thumbnails.
		hold.Close(s.public.Protocol != "plex")
	}
	for _, cancel := range transfers {
		cancel()
	}
	if !ok {
		return nil
	}
	if s.sourcePinned {
		if pins, ok := b.deps.Playback.(core.PlaybackRevisionPin); ok {
			pins.UnpinSourceRevision(s.playID)
		}
	}
	if b.cluster != nil {
		if err := b.cluster.store.remove(ctx, s.public.ID); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
		b.cluster.announceRetired(ctx, s)
	}
	b.cleanupSampleReceipt(ctx, s)

	b.bindings.Remove(s.public.ID)
	if s.live {
		err := b.deps.Live.Stop(ctx, &core.Viewer{ID: s.public.ViewerID}, s.playID)
		// The broker already authenticated and removed this exact session.
		// A live idle reaper may have joined its backend before the next switch;
		// absence is successful retirement, not a failure to start the new item.
		if errors.Is(err, core.ErrNotFound) {
			err = nil
		}
		b.settleRetirement(context.WithoutCancel(ctx), s, err == nil)
		b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
		return err
	}
	// A newer binding owns this client and title. Stop the encoder for this
	// session, and leave the source with that binding. This is the broker
	// switch's stop rule; retirement must not drop it.
	if stopSource && b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		stopSource = false
	}
	if err := b.retireBackends(ctx, s, stopSource); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
	b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
	return nil
}

// StopPlayback stops every local broker session fed by a source ID.
func (b *Broker) StopPlayback(ctx context.Context, playID string) bool {
	b.mu.Lock()
	var entries []brokerSession
	for _, s := range b.sessions {
		if s.playID == playID {
			entries = append(entries, *s)
		}
	}
	b.mu.Unlock()
	stopped := true
	for i := range entries {
		s := &entries[i]
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
		if err == nil {
			cmd := frontend.Command{Kind: frontend.CommandStop}
			b.publish(ctx, frontend.Event{Type: "SessionCommand", ViewerID: s.public.ViewerID, SessionID: s.public.ID, TargetProtocol: s.public.Protocol, TargetDeviceID: s.public.Device.ID, Command: &cmd})
			_ = b.stopLocked(ctx, s)
			unlock()
		} else {
			stopped = false
		}
	}
	if len(entries) == 0 && b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
			return false
		}
		for _, r := range remote {
			if r.sourceID != "" && r.sourceID == playID {
				payload, err := json.Marshal(remoteStop{Node: r.node, SourceID: playID})
				if err == nil {
					err = b.cluster.node.Publish(ctx, brokerStopTopic, payload)
				}
				if err != nil && b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
				return err == nil
			}
		}
	}
	return len(entries) > 0 && stopped
}

// Close stops local broker sessions and prevents new plans or opens.
func (b *Broker) Close(ctx context.Context) {
	b.mu.Lock()
	if b.closed && len(b.sessions) == 0 && len(b.plans) == 0 {
		b.mu.Unlock()
		return
	}
	firstClose := !b.closed
	b.closed = true
	if b.historyCancel != nil {
		b.historyCancel()
	}
	var transferCancels []context.CancelFunc
	for _, transfers := range b.lazyTransfers {
		for _, cancel := range transfers {
			transferCancels = append(transferCancels, cancel)
		}
	}
	b.lazyTransfers = nil
	lazyCancel, lazyDone := b.lazyCleanupCancel, b.lazyCleanupDone
	var ids, planIDs []string
	seen := make(map[string]bool)
	for _, s := range b.sessions {
		if s.autoSubtitleCancel != nil {
			s.autoSubtitleCancel()
		}
		if !seen[s.playID] {
			ids = append(ids, s.playID)
			seen[s.playID] = true
		}
	}
	for id := range b.plans {
		planIDs = append(planIDs, id)
	}
	b.mu.Unlock()
	for _, cancel := range transferCancels {
		cancel()
	}
	b.autoSubtitleWG.Wait()
	// Shutdown must still release sessions when its caller has already been
	// canceled; bound the cleanup so an unresponsive gate cannot block forever.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.shutdownTimeout)
	defer cancel()
	if b.historyDone != nil {
		drainCtx, drainCancel := context.WithTimeout(cleanupCtx, 150*time.Millisecond)
		select {
		case <-b.historyDone:
		case <-drainCtx.Done():
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(errors.New("sessions: source history writer did not stop after cancellation"))
			}
		}
		drainCancel()
	}
	if lazyCancel != nil {
		lazyCancel()
	}
	if lazyDone != nil {
		select {
		case <-lazyDone:
		case <-cleanupCtx.Done():
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(fmt.Errorf("sessions: lazy cleanup worker did not stop during shutdown"))
			}
		}
	}
	// The bounded plan sweeper stays alive until outstanding owner gates and
	// in-flight opens release their references, even after this budget expires.
	for _, id := range ids {
		if !b.StopPlayback(cleanupCtx, id) && b.deps.OnEventError != nil {
			b.deps.OnEventError(fmt.Errorf("sessions: could not stop playback %s during shutdown", id))
		}
	}
	for _, id := range planIDs {
		b.dropPlan(cleanupCtx, id, true)
	}
	if firstClose && b.cluster != nil {
		b.cluster.close()
	}
}
