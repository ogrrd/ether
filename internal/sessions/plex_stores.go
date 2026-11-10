package sessions

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (b *Broker) HasBroker() bool { return b != nil }

func (b *Broker) PlexEncoder() frontend.PlexEncoder {
	if b == nil || b.deps.DistributedHLS == nil {
		return nil
	}
	return b
}

func (b *Broker) Playback() frontend.Playback {
	if b == nil {
		return nil
	}
	return b.deps.Playback
}

func (b *Broker) Transcoder() frontend.Transcoder {
	if b == nil {
		return nil
	}
	return b.deps.Transcoder
}

func (b *Broker) Downloads() frontend.DownloadRemuxer {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deps.Downloads
}

func (b *Broker) SetDownloads(remuxer frontend.DownloadRemuxer) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.deps.Downloads = remuxer
	b.mu.Unlock()
}

func (b *Broker) LatestDecision(key core.RatingKey) (core.DecisionTrace, bool) {
	if b == nil || b.deps.DecisionTraces == nil {
		return core.DecisionTrace{}, false
	}
	return b.deps.DecisionTraces.LatestDecision(key)
}

func (b *Broker) DB() *sql.DB {
	if b == nil {
		return nil
	}
	return b.deps.DB
}

func (b *Broker) Cluster() frontend.ClusterNode {
	if b == nil {
		return nil
	}
	return b.deps.Cluster
}

func (b *Broker) SessionHeartbeat() time.Duration {
	if b == nil {
		return 0
	}
	return b.deps.SessionHeartbeat
}

func (b *Broker) PlaybackOwners() frontend.PlaybackOwners {
	if b == nil || b.owners == nil {
		return nil
	}
	return b.owners
}

func (b *Broker) LifecycleGates() frontend.Lifecycle {
	if b == nil || b.gates == nil {
		return nil
	}
	return b.gates
}

func (b *Broker) NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) frontend.DecisionArms {
	return NewDecisionArms(ctx, db, now, node, log, wg)
}

func (b *Broker) NewStartGuard() frontend.StartGuard { return &StartGuard{} }
func (b *Broker) NewPlayQueues() frontend.PlayQueues { return NewPlayQueues() }
func (b *Broker) NewDownloadQueueStore() frontend.DownloadQueueStore {
	return NewDownloadQueueStore()
}

func (b *Broker) NewVersionFlows(ctx context.Context, db *sql.DB) frontend.VersionFlows {
	return NewVersionFlows(ctx, db)
}

func (b *Broker) NewVariantIDs(db *sql.DB, now func() time.Time) frontend.VariantIDs {
	return NewVariantIDs(db, now)
}

func (b *Broker) NewDownloadJob(playback frontend.Playback, remuxer frontend.DownloadRemuxer) frontend.DownloadJob {
	return DownloadJob{Playback: playback, Remuxer: remuxer}
}

func (b *Broker) NewDownloadQueuePersistence(queue frontend.DownloadQueueStore, db *sql.DB, artifacts frontend.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) frontend.DownloadQueuePersistence {
	q, _ := queue.(*DownloadQueueStore)
	if q == nil {
		return DownloadQueuePersistence{}
	}
	return DownloadQueuePersistence{Queue: q, DB: db, Artifacts: artifacts, Log: log, Expiry: expiry, StorageID: storageID, Users: users}
}

func (b *Broker) PrepareSource(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error) {
	if b == nil {
		return nil, core.ErrNotAvailable
	}
	return (PlexSources{Playback: b.deps.Playback, Log: b.deps.Logger}).Prepare(ctx, viewer, key, caps, release, wait)
}

func (b *Broker) PartSource(viewer int64, client string, key core.RatingKey, release string) frontend.PlexPartSource {
	if b == nil {
		return frontend.PlexPartSource{}
	}
	return (PlexSources{Playback: b.deps.Playback, Log: b.deps.Logger}).Part(viewer, client, key, release)
}

func (b *Broker) ServePartSource(w http.ResponseWriter, r *http.Request, sourceID string) {
	if b == nil {
		http.NotFound(w, r)
		return
	}
	(PlexSources{Playback: b.deps.Playback, Log: b.deps.Logger}).ServePart(w, r, sourceID)
}

func (b *Broker) ObserveStart(ctx context.Context, source frontend.HLSSession, id string) (frontend.StartObservation, bool) {
	return ObserveStart(ctx, source, id)
}

func (b *Broker) ActivityStore() frontend.ActivityStore {
	if b == nil {
		return nil
	}
	return b
}

func (b *Broker) BindStream(ctx context.Context, access frontend.SessionAccess, binding frontend.PlexStreamBinding) (int64, error) {
	return b.BindPlexStream(ctx, access, binding)
}

func (b *Broker) ResolveStream(ctx context.Context, access frontend.SessionAccess, wireID int64) (frontend.PlexStreamBinding, error) {
	return b.ResolvePlexStream(ctx, access, wireID)
}

func (b *Broker) OpenSidecar(ctx context.Context, access frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	return b.OpenPlexSidecar(ctx, access, receipt, wireID)
}

// LocalRuntimeDeps configures a standalone SessionRuntime when no full
// session broker is mounted.
type LocalRuntimeDeps struct {
	Sessions         frontend.Sessions
	Encoder          frontend.PlexEncoder
	Logger           *slog.Logger
	Playback         core.Playback
	PlaybackFunc     func() core.Playback
	Transcoder       core.Transcoder
	TranscoderFunc   func() core.Transcoder
	Downloads        core.DownloadRemuxer
	DecisionTraces   core.DecisionTraceSource
	DB               *sql.DB
	Cluster          frontend.ClusterNode
	SessionHeartbeat time.Duration
	Coordination     *Coordination
}

// LocalRuntime implements frontend.Sessions and frontend.SessionRuntime for
// standalone servers, spike fixtures, and unit tests.
type LocalRuntime struct {
	mu     sync.Mutex
	deps   LocalRuntimeDeps
	owners *PlaybackOwners
	gates  *Lifecycle
}

// NewLocalRuntime constructs a SessionRuntime from local dependencies.
func NewLocalRuntime(d LocalRuntimeDeps) *LocalRuntime {
	coord := d.Coordination
	if coord == nil || coord.Owners == nil || coord.Gates == nil {
		coord = NewCoordination()
	}
	return &LocalRuntime{
		deps:   d,
		owners: coord.Owners,
		gates:  coord.Gates,
	}
}

func (r *LocalRuntime) FillDefaults(d LocalRuntimeDeps) {
	if r == nil {
		return
	}
	if r.deps.Playback == nil {
		r.deps.Playback = d.Playback
	}
	if r.deps.PlaybackFunc == nil {
		r.deps.PlaybackFunc = d.PlaybackFunc
	}
	if r.deps.Transcoder == nil {
		r.deps.Transcoder = d.Transcoder
	}
	if r.deps.TranscoderFunc == nil {
		r.deps.TranscoderFunc = d.TranscoderFunc
	}
	if r.deps.DB == nil {
		r.deps.DB = d.DB
	}
	if r.deps.Logger == nil {
		r.deps.Logger = d.Logger
	}
}

func (r *LocalRuntime) Deps() LocalRuntimeDeps {
	if r == nil {
		return LocalRuntimeDeps{}
	}
	return r.deps
}

func (r *LocalRuntime) HasBroker() bool { return r != nil && r.deps.Sessions != nil }

func (r *LocalRuntime) PlexEncoder() frontend.PlexEncoder {
	if r == nil {
		return nil
	}
	if r.deps.Encoder != nil {
		return r.deps.Encoder
	}
	if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
		return rt.PlexEncoder()
	}
	return nil
}

func (r *LocalRuntime) playback() core.Playback {
	if r == nil {
		return nil
	}
	if r.deps.PlaybackFunc != nil {
		if p := r.deps.PlaybackFunc(); p != nil {
			return p
		}
	}
	return r.deps.Playback
}

func (r *LocalRuntime) transcoder() core.Transcoder {
	if r == nil {
		return nil
	}
	if r.deps.TranscoderFunc != nil {
		if t := r.deps.TranscoderFunc(); t != nil {
			return t
		}
	}
	return r.deps.Transcoder
}

func (r *LocalRuntime) Playback() frontend.Playback     { return r.playback() }
func (r *LocalRuntime) Transcoder() frontend.Transcoder { return r.transcoder() }

func (r *LocalRuntime) Downloads() frontend.DownloadRemuxer {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deps.Downloads
}

func (r *LocalRuntime) SetDownloads(remuxer frontend.DownloadRemuxer) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.deps.Downloads = remuxer
	r.mu.Unlock()
}

func (r *LocalRuntime) LatestDecision(key core.RatingKey) (core.DecisionTrace, bool) {
	if r == nil || r.deps.DecisionTraces == nil {
		return core.DecisionTrace{}, false
	}
	return r.deps.DecisionTraces.LatestDecision(key)
}

func (r *LocalRuntime) DB() *sql.DB {
	if r == nil {
		return nil
	}
	return r.deps.DB
}

func (r *LocalRuntime) Cluster() frontend.ClusterNode {
	if r == nil {
		return nil
	}
	return r.deps.Cluster
}

func (r *LocalRuntime) SessionHeartbeat() time.Duration {
	if r == nil {
		return 0
	}
	return r.deps.SessionHeartbeat
}

func (r *LocalRuntime) PlaybackOwners() frontend.PlaybackOwners {
	if r == nil {
		return nil
	}
	return r.owners
}

func (r *LocalRuntime) LifecycleGates() frontend.Lifecycle {
	if r == nil {
		return nil
	}
	return r.gates
}

func (r *LocalRuntime) NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) frontend.DecisionArms {
	return NewDecisionArms(ctx, db, now, node, log, wg)
}

func (r *LocalRuntime) NewStartGuard() frontend.StartGuard { return &StartGuard{} }
func (r *LocalRuntime) NewPlayQueues() frontend.PlayQueues { return NewPlayQueues() }
func (r *LocalRuntime) NewDownloadQueueStore() frontend.DownloadQueueStore {
	return NewDownloadQueueStore()
}

func (r *LocalRuntime) NewVersionFlows(ctx context.Context, db *sql.DB) frontend.VersionFlows {
	return NewVersionFlows(ctx, db)
}

func (r *LocalRuntime) NewVariantIDs(db *sql.DB, now func() time.Time) frontend.VariantIDs {
	return NewVariantIDs(db, now)
}

func (r *LocalRuntime) NewDownloadJob(playback frontend.Playback, remuxer frontend.DownloadRemuxer) frontend.DownloadJob {
	return DownloadJob{Playback: playback, Remuxer: remuxer}
}

func (r *LocalRuntime) NewDownloadQueuePersistence(queue frontend.DownloadQueueStore, db *sql.DB, artifacts frontend.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) frontend.DownloadQueuePersistence {
	q, _ := queue.(*DownloadQueueStore)
	if q == nil {
		return DownloadQueuePersistence{}
	}
	return DownloadQueuePersistence{Queue: q, DB: db, Artifacts: artifacts, Log: log, Expiry: expiry, StorageID: storageID, Users: users}
}

func (r *LocalRuntime) PrepareSource(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error) {
	if r == nil {
		return nil, core.ErrNotAvailable
	}
	return (PlexSources{Playback: r.playback(), Log: r.deps.Logger}).Prepare(ctx, viewer, key, caps, release, wait)
}

func (r *LocalRuntime) PartSource(viewer int64, client string, key core.RatingKey, release string) frontend.PlexPartSource {
	if r == nil {
		return frontend.PlexPartSource{}
	}
	return (PlexSources{Playback: r.playback(), Log: r.deps.Logger}).Part(viewer, client, key, release)
}

func (r *LocalRuntime) ServePartSource(w http.ResponseWriter, req *http.Request, sourceID string) {
	if r == nil {
		http.NotFound(w, req)
		return
	}
	(PlexSources{Playback: r.playback(), Log: r.deps.Logger}).ServePart(w, req, sourceID)
}

func (r *LocalRuntime) ObserveStart(ctx context.Context, source frontend.HLSSession, id string) (frontend.StartObservation, bool) {
	return ObserveStart(ctx, source, id)
}

func (r *LocalRuntime) ActivityStore() frontend.ActivityStore {
	if r == nil || r.deps.Sessions == nil {
		return nil
	}
	if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
		return rt.ActivityStore()
	}
	return nil
}

func (r *LocalRuntime) BindStream(ctx context.Context, access frontend.SessionAccess, binding frontend.PlexStreamBinding) (int64, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.BindStream(ctx, access, binding)
		}
	}
	return 0, core.ErrNotAvailable
}

func (r *LocalRuntime) ResolveStream(ctx context.Context, access frontend.SessionAccess, wireID int64) (frontend.PlexStreamBinding, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.ResolveStream(ctx, access, wireID)
		}
	}
	return frontend.PlexStreamBinding{}, core.ErrNotFound
}

func (r *LocalRuntime) OpenSidecar(ctx context.Context, access frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.OpenSidecar(ctx, access, receipt, wireID)
		}
	}
	return nil, core.StreamInfo{}, core.ErrNotAvailable
}

func (r *LocalRuntime) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			rt.NotePlexEncoder(id, format, decision, offset)
		}
	}
}

func (r *LocalRuntime) PlexSessionForPlayback(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, playbackID, transcodeID string) (string, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.PlexSessionForPlayback(ctx, a, item, playbackID, transcodeID)
		}
	}
	return "", core.ErrNotAvailable
}

func (r *LocalRuntime) PlexSessionForSource(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, playID string) (string, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.PlexSessionForSource(ctx, a, item, playID)
		}
	}
	return "", core.ErrNotAvailable
}

func (r *LocalRuntime) PlexSessionSource(id string) (string, bool) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.PlexSessionSource(id)
		}
	}
	return "", false
}

func (r *LocalRuntime) ReportProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.ReportProgress(ctx, a, item, t)
		}
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) StopPlayback(ctx context.Context, playID string) bool {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.StopPlayback(ctx, playID)
		}
	}
	return false
}

func (r *LocalRuntime) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.StopProtocol(ctx, protocol)
		}
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) RecordAuthorizedSubtitleIntent(ctx context.Context, viewer int64, key core.RatingKey, manual bool) error {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
		}
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) ConfirmPlexAdmission(ctx context.Context, viewer *core.Viewer, remote bool) (*core.Viewer, error) {
	if r != nil && r.deps.Sessions != nil {
		if rt, ok := r.deps.Sessions.(frontend.SessionRuntime); ok {
			return rt.ConfirmPlexAdmission(ctx, viewer, remote)
		}
	}
	return viewer, nil
}

func (r *LocalRuntime) Plan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Plan(ctx, req)
	}
	return frontend.PlayPlan{}, core.ErrNotAvailable
}

func (r *LocalRuntime) Open(ctx context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Open(ctx, req)
	}
	return frontend.Session{}, core.ErrNotAvailable
}

func (r *LocalRuntime) StartStream(ctx context.Context, a frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.StartStream(ctx, a, id, req)
	}
	return "", core.ErrNotAvailable
}

func (r *LocalRuntime) SubtitleURL(ctx context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.SubtitleURL(ctx, a, id, index, format)
	}
	return "", core.ErrNotAvailable
}

func (r *LocalRuntime) Markers(ctx context.Context, a frontend.SessionAccess, id string) (frontend.SessionMarkers, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Markers(ctx, a, id)
	}
	return frontend.SessionMarkers{}, core.ErrNotAvailable
}

func (r *LocalRuntime) ServeMedia(w http.ResponseWriter, req *http.Request) {
	if r != nil && r.deps.Sessions != nil {
		r.deps.Sessions.ServeMedia(w, req)
		return
	}
	http.NotFound(w, req)
}

func (r *LocalRuntime) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Report(ctx, a, id, t)
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Stop(ctx, a, id)
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) Command(ctx context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Command(ctx, actor, id, cmd)
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) UpdateCapabilities(ctx context.Context, a frontend.SessionAccess, c frontend.SessionCapabilities) error {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.UpdateCapabilities(ctx, a, c)
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) Capabilities(ctx context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Capabilities(ctx, a)
	}
	return frontend.SessionCapabilities{}, core.ErrNotAvailable
}

func (r *LocalRuntime) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Sessions(ctx, a)
	}
	return nil, core.ErrNotAvailable
}

func (r *LocalRuntime) Recover(ctx context.Context, id, url string, at time.Duration) error {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Recover(ctx, id, url, at)
	}
	return core.ErrNotAvailable
}

func (r *LocalRuntime) TranscodeFor(ctx context.Context, id string) (core.HLSSession, bool) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.TranscodeFor(ctx, id)
	}
	return nil, false
}

func (r *LocalRuntime) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	if r != nil && r.deps.Sessions != nil {
		return r.deps.Sessions.Events(ctx, scope)
	}
	return nil, core.ErrNotAvailable
}

func (r *LocalRuntime) EncodeQueue(q *PlayQueue) (string, error) { return EncodeQueue(q) }
func (r *LocalRuntime) DecodeQueue(id, userID, rev, updated int64, data string) (*PlayQueue, error) {
	return DecodeQueue(id, userID, rev, updated, data)
}
