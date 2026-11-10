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

// BindSurface prevents an adapter from submitting another surface's identity.
func BindSurface(b frontend.Sessions, protocol frontend.Protocol) frontend.Sessions {
	return surface{broker: b, protocol: protocol}
}

type surface struct {
	broker   frontend.Sessions
	protocol frontend.Protocol
}

// BindEvents applies the originating surface filter to user-data events.
func BindEvents(events frontend.Subscriber, protocol frontend.Protocol) frontend.Subscriber {
	return surfaceEvents{events: events, protocol: protocol}
}

type surfaceEvents struct {
	events   frontend.Subscriber
	protocol frontend.Protocol
}

func (s surfaceEvents) Subscribe(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	scope.Protocol = s.protocol
	return s.events.Subscribe(ctx, scope)
}

func (s surface) HasBroker() bool {
	if rt := s.runtime(); rt != nil {
		return rt.HasBroker()
	}
	return s.broker != nil
}

func (s surface) PlexEncoder() frontend.PlexEncoder {
	if s.protocol != "plex" {
		return nil
	}
	if rt := s.runtime(); rt != nil && rt.PlexEncoder() != nil {
		return s
	}
	return nil
}

func (s surface) Plan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if r.Protocol != s.protocol {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	return s.broker.Plan(ctx, r)
}
func (s surface) DetailPlan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if r.Protocol != s.protocol {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.DetailPlan(ctx, r)
	}
	return frontend.PlayPlan{}, core.ErrNotAvailable
}
func (s surface) ResolveDetailSource(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.DetailSourceTarget, error) {
	if scope.Protocol != s.protocol {
		return frontend.DetailSourceTarget{}, core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.ResolveDetailSource(ctx, scope, handle)
	}
	return frontend.DetailSourceTarget{}, core.ErrNotAvailable
}

// WarmDetail forwards a detail-page warm-up for this surface only.
func (s surface) WarmDetail(ctx context.Context, r frontend.PlanRequest) bool {
	if r.Protocol != s.protocol {
		return false
	}
	if warmer, ok := s.broker.(frontend.DetailWarmer); ok {
		return warmer.WarmDetail(ctx, r)
	}
	return false
}
func (s surface) MatchDetailSession(ctx context.Context, scope frontend.VersionScope, handle, sessionID string) error {
	if scope.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.MatchDetailSession(ctx, scope, handle, sessionID)
	}
	return core.ErrNotAvailable
}
func (s surface) Open(ctx context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	if r.Protocol != s.protocol {
		return frontend.Session{}, core.ErrUnauthorized
	}
	return s.broker.Open(ctx, r)
}
func (s surface) StartStream(ctx context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	if a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	return s.broker.StartStream(ctx, a, id, r)
}
func (s surface) SubtitleURL(ctx context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	if a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	return s.broker.SubtitleURL(ctx, a, id, index, format)
}
func (s surface) HasAuthoredASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, index int) bool {
	if a.Protocol != s.protocol {
		return false
	}
	artifacts, ok := s.broker.(frontend.SessionSubtitleArtifacts)
	if !ok {
		return false
	}
	return artifacts.HasAuthoredASSSubtitle(ctx, a, id, index)
}
func (s surface) ServeASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, index int, syncMS int, requireAuthored bool) (frontend.SubtitleASSArtifact, error) {
	if a.Protocol != s.protocol {
		return frontend.SubtitleASSArtifact{}, core.ErrUnauthorized
	}
	artifacts, ok := s.broker.(frontend.SessionSubtitleArtifacts)
	if !ok {
		return frontend.SubtitleASSArtifact{}, core.ErrNotAvailable
	}
	return artifacts.ServeASSSubtitle(ctx, a, id, index, syncMS, requireAuthored)
}
func (s surface) SubtitleFontBundle(ctx context.Context, a frontend.SessionAccess, id string, index int) ([]frontend.SubtitleFontBundleItem, error) {
	if a.Protocol != s.protocol {
		return nil, core.ErrUnauthorized
	}
	artifacts, ok := s.broker.(frontend.SessionSubtitleArtifacts)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return artifacts.SubtitleFontBundle(ctx, a, id, index)
}
func (s surface) SubtitleFontAttachment(ctx context.Context, a frontend.SessionAccess, id string, index int, fontIndex int) (frontend.SubtitleFontAttachment, error) {
	if a.Protocol != s.protocol {
		return frontend.SubtitleFontAttachment{}, core.ErrUnauthorized
	}
	artifacts, ok := s.broker.(frontend.SessionSubtitleArtifacts)
	if !ok {
		return frontend.SubtitleFontAttachment{}, core.ErrNotAvailable
	}
	return artifacts.SubtitleFontAttachment(ctx, a, id, index, fontIndex)
}
func (s surface) Markers(ctx context.Context, a frontend.SessionAccess, id string) (frontend.SessionMarkers, error) {
	if a.Protocol != s.protocol {
		return frontend.SessionMarkers{}, core.ErrUnauthorized
	}
	return s.broker.Markers(ctx, a, id)
}
func (s surface) ServeMedia(w http.ResponseWriter, r *http.Request) {
	if scoped, ok := s.broker.(interface {
		ServeMediaFor(frontend.Protocol, http.ResponseWriter, *http.Request)
	}); ok {
		scoped.ServeMediaFor(s.protocol, w, r)
		return
	}
	http.NotFound(w, r)
}
func (s surface) ServeMediaAccess(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess) {
	if a.Protocol != s.protocol {
		http.NotFound(w, r)
		return
	}
	if scoped, ok := s.broker.(frontend.ScopedMedia); ok {
		scoped.ServeMediaAccess(w, r, a)
		return
	}
	http.NotFound(w, r)
}

func (s surface) ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string) {
	if a.Protocol != s.protocol {
		http.NotFound(w, r)
		return
	}
	media, ok := s.broker.(interface {
		ServeSessionMedia(http.ResponseWriter, *http.Request, frontend.SessionAccess, string, string)
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	media.ServeSessionMedia(w, r, a, sessionID, resource)
}
func (s surface) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.Report(ctx, a, id, t)
}

// UnderlyingBroker unwraps a surface binding to the broker that stores
// Plex activities. A non-broker Sessions value has none.
func UnderlyingBroker(s frontend.Sessions) *Broker {
	switch v := s.(type) {
	case *Broker:
		return v
	case surface:
		b, _ := v.broker.(*Broker)
		return b
	default:
		return nil
	}
}

func (s surface) StopPlayback(ctx context.Context, playID string) bool {
	if rt := s.runtime(); rt != nil {
		return rt.StopPlayback(ctx, playID)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return false
	}
	return b.StopPlayback(ctx, playID)
}

func (s surface) ReportItemProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.ReportItemProgress(ctx, a, item, t)
}

func (s surface) ReportProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.ReportProgress(ctx, a, item, t)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.ReportProgress(ctx, a, item, t)
}

func (s surface) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.Stop(ctx, a, id)
}
func (s surface) Command(ctx context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	return s.broker.Command(ctx, actor, id, cmd)
}
func (s surface) UpdateCapabilities(ctx context.Context, a frontend.SessionAccess, c frontend.SessionCapabilities) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.UpdateCapabilities(ctx, a, c)
}
func (s surface) Capabilities(ctx context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	if a.Protocol != s.protocol {
		return frontend.SessionCapabilities{}, core.ErrUnauthorized
	}
	return s.broker.Capabilities(ctx, a)
}
func (s surface) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	if a.Protocol != s.protocol {
		return nil, core.ErrUnauthorized
	}
	return s.broker.Sessions(ctx, a)
}
func (s surface) Recover(ctx context.Context, id, url string, at time.Duration) error {
	return s.broker.Recover(ctx, id, url, at)
}
func (s surface) TranscodeFor(ctx context.Context, id string) (core.HLSSession, bool) {
	return s.broker.TranscodeFor(ctx, id)
}
func (s surface) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	scope.Protocol = s.protocol
	return s.broker.Events(ctx, scope)
}

func (s surface) PlexSessionForPlayback(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playbackID, transcodeID string) (string, error) {
	if s.protocol != "plex" || a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionForPlayback(ctx, a, item, playbackID, transcodeID)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return "", core.ErrNotAvailable
	}
	return b.PlexSessionForPlayback(ctx, a, item, playbackID, transcodeID)
}

func (s surface) PlexSessionForSource(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playID string) (string, error) {
	if s.protocol != "plex" || a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionForSource(ctx, a, item, playID)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return "", core.ErrNotAvailable
	}
	return b.PlexSessionForSource(ctx, a, item, playID)
}

func (s surface) PlexSessionSource(id string) (string, bool) {
	if s.protocol != "plex" {
		return "", false
	}
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionSource(id)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return "", false
	}
	return b.PlexSessionSource(id)
}

func (s surface) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if s.protocol != "plex" {
		return
	}
	if rt := s.runtime(); rt != nil {
		rt.NotePlexEncoder(id, format, decision, offset)
		return
	}
	if b, ok := s.broker.(*Broker); ok {
		b.NotePlexEncoder(id, format, decision, offset)
	}
}

func (s surface) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	if protocol != s.protocol {
		return core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.StopProtocol(ctx, protocol)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.StopProtocol(ctx, protocol)
}

func (s surface) BindPlexStream(ctx context.Context, a frontend.SessionAccess, binding PlexStreamBinding) (int64, error) {
	if a.Protocol != s.protocol {
		return 0, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		BindPlexStream(context.Context, frontend.SessionAccess, PlexStreamBinding) (int64, error)
	})
	if !ok {
		return 0, core.ErrNotAvailable
	}
	return api.BindPlexStream(ctx, a, binding)
}

func (s surface) ResolvePlexStream(ctx context.Context, a frontend.SessionAccess, wireID int64) (PlexStreamBinding, error) {
	if a.Protocol != s.protocol {
		return PlexStreamBinding{}, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		ResolvePlexStream(context.Context, frontend.SessionAccess, int64) (PlexStreamBinding, error)
	})
	if !ok {
		return PlexStreamBinding{}, core.ErrNotAvailable
	}
	return api.ResolvePlexStream(ctx, a, wireID)
}

func (s surface) OpenPlexSidecar(ctx context.Context, a frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if a.Protocol != s.protocol {
		return nil, core.StreamInfo{}, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		OpenPlexSidecar(context.Context, frontend.SessionAccess, auth.Acceptance, int64) (io.ReadCloser, core.StreamInfo, error)
	})
	if !ok {
		return nil, core.StreamInfo{}, core.ErrNotAvailable
	}
	return api.OpenPlexSidecar(ctx, a, receipt, wireID)
}

func (s surface) ReportSample(ctx context.Context, a frontend.SessionAccess, id string, sample frontend.ProgressSample) (frontend.ReportOutcome, error) {
	if a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	reporter, ok := s.broker.(frontend.SampleReporter)
	if !ok {
		return "", core.ErrNotAvailable
	}
	return reporter.ReportSample(ctx, a, id, sample)
}

// RecordAuthorizedSubtitleIntent forwards to the underlying broker.
func (s surface) RecordAuthorizedSubtitleIntent(ctx context.Context, viewer int64, key core.RatingKey, manual bool) error {
	if rt := s.runtime(); rt != nil {
		return rt.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
}

func (s surface) ConfirmPlexAdmission(ctx context.Context, viewer *core.Viewer, remote bool) (*core.Viewer, error) {
	if s.protocol != "plex" {
		return nil, core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.ConfirmPlexAdmission(ctx, viewer, remote)
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return viewer, nil
	}
	return b.ConfirmPlexAdmission(ctx, viewer, remote)
}

func (s surface) runtime() frontend.SessionRuntime {
	rt, _ := s.broker.(frontend.SessionRuntime)
	return rt
}

func (s surface) Playback() frontend.Playback {
	if rt := s.runtime(); rt != nil {
		return rt.Playback()
	}
	return nil
}

func (s surface) Transcoder() frontend.Transcoder {
	if rt := s.runtime(); rt != nil {
		return rt.Transcoder()
	}
	return nil
}

func (s surface) Downloads() frontend.DownloadRemuxer {
	if rt := s.runtime(); rt != nil {
		return rt.Downloads()
	}
	return nil
}

func (s surface) SetDownloads(remuxer frontend.DownloadRemuxer) {
	if rt := s.runtime(); rt != nil {
		rt.SetDownloads(remuxer)
	}
}

func (s surface) LatestDecision(key core.RatingKey) (core.DecisionTrace, bool) {
	if rt := s.runtime(); rt != nil {
		return rt.LatestDecision(key)
	}
	return core.DecisionTrace{}, false
}

func (s surface) DB() *sql.DB {
	if rt := s.runtime(); rt != nil {
		return rt.DB()
	}
	return nil
}

func (s surface) Cluster() frontend.ClusterNode {
	if rt := s.runtime(); rt != nil {
		return rt.Cluster()
	}
	return nil
}

func (s surface) SessionHeartbeat() time.Duration {
	if rt := s.runtime(); rt != nil {
		return rt.SessionHeartbeat()
	}
	return 0
}

func (s surface) PlaybackOwners() frontend.PlaybackOwners {
	if rt := s.runtime(); rt != nil {
		return rt.PlaybackOwners()
	}
	return nil
}

func (s surface) LifecycleGates() frontend.Lifecycle {
	if rt := s.runtime(); rt != nil {
		return rt.LifecycleGates()
	}
	return nil
}

func (s surface) NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) frontend.DecisionArms {
	if rt := s.runtime(); rt != nil {
		return rt.NewDecisionArms(ctx, db, now, node, log, wg)
	}
	return NewDecisionArms(ctx, db, now, node, log, wg)
}

func (s surface) NewStartGuard() frontend.StartGuard {
	if rt := s.runtime(); rt != nil {
		return rt.NewStartGuard()
	}
	return &StartGuard{}
}

func (s surface) NewPlayQueues() frontend.PlayQueues {
	if rt := s.runtime(); rt != nil {
		return rt.NewPlayQueues()
	}
	return NewPlayQueues()
}

func (s surface) NewVersionFlows(ctx context.Context, db *sql.DB) frontend.VersionFlows {
	if rt := s.runtime(); rt != nil {
		return rt.NewVersionFlows(ctx, db)
	}
	return NewVersionFlows(ctx, db)
}

func (s surface) NewDownloadQueueStore() frontend.DownloadQueueStore {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadQueueStore()
	}
	return NewDownloadQueueStore()
}

func (s surface) NewVariantIDs(db *sql.DB, now func() time.Time) frontend.VariantIDs {
	if rt := s.runtime(); rt != nil {
		return rt.NewVariantIDs(db, now)
	}
	return NewVariantIDs(db, now)
}

func (s surface) NewDownloadJob(playback frontend.Playback, remuxer frontend.DownloadRemuxer) frontend.DownloadJob {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadJob(playback, remuxer)
	}
	return DownloadJob{Playback: playback, Remuxer: remuxer}
}

func (s surface) NewDownloadQueuePersistence(queue frontend.DownloadQueueStore, db *sql.DB, artifacts frontend.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) frontend.DownloadQueuePersistence {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadQueuePersistence(queue, db, artifacts, log, expiry, storageID, users)
	}
	q, _ := queue.(*DownloadQueueStore)
	if q == nil {
		return DownloadQueuePersistence{}
	}
	return DownloadQueuePersistence{Queue: q, DB: db, Artifacts: artifacts, Log: log, Expiry: expiry, StorageID: storageID, Users: users}
}

func (s surface) PrepareSource(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error) {
	if rt := s.runtime(); rt != nil {
		return rt.PrepareSource(ctx, viewer, key, caps, release, wait)
	}
	return nil, core.ErrNotAvailable
}

func (s surface) PartSource(viewer int64, client string, key core.RatingKey, release string) frontend.PlexPartSource {
	if rt := s.runtime(); rt != nil {
		return rt.PartSource(viewer, client, key, release)
	}
	return frontend.PlexPartSource{}
}

func (s surface) ServePartSource(w http.ResponseWriter, r *http.Request, sourceID string) {
	if rt := s.runtime(); rt != nil {
		rt.ServePartSource(w, r, sourceID)
		return
	}
	http.NotFound(w, r)
}

func (s surface) ObserveStart(ctx context.Context, source frontend.HLSSession, id string) (frontend.StartObservation, bool) {
	if rt := s.runtime(); rt != nil {
		return rt.ObserveStart(ctx, source, id)
	}
	return ObserveStart(ctx, source, id)
}

func (s surface) ActivityStore() frontend.ActivityStore {
	if rt := s.runtime(); rt != nil {
		return rt.ActivityStore()
	}
	return nil
}

func (s surface) BindStream(ctx context.Context, a frontend.SessionAccess, binding frontend.PlexStreamBinding) (int64, error) {
	if a.Protocol != s.protocol {
		return 0, core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.BindStream(ctx, a, binding)
	}
	return s.BindPlexStream(ctx, a, binding)
}

func (s surface) ResolveStream(ctx context.Context, a frontend.SessionAccess, wireID int64) (frontend.PlexStreamBinding, error) {
	if a.Protocol != s.protocol {
		return frontend.PlexStreamBinding{}, core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.ResolveStream(ctx, a, wireID)
	}
	return s.ResolvePlexStream(ctx, a, wireID)
}

func (s surface) OpenSidecar(ctx context.Context, a frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if a.Protocol != s.protocol {
		return nil, core.StreamInfo{}, core.ErrUnauthorized
	}
	if rt := s.runtime(); rt != nil {
		return rt.OpenSidecar(ctx, a, receipt, wireID)
	}
	return s.OpenPlexSidecar(ctx, a, receipt, wireID)
}

func (s surface) StartPlexHLS(ctx context.Context, u *core.User, playID, id string, req core.HLSRequest) (core.HLSSession, error) {
	if s.protocol != "plex" {
		return nil, core.ErrUnauthorized
	}
	enc, ok := s.broker.(frontend.PlexEncoder)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return enc.StartPlexHLS(ctx, u, playID, id, req)
}

func (s surface) PlexHLSSession(id string) (core.HLSSession, bool) {
	if s.protocol != "plex" {
		return nil, false
	}
	enc, ok := s.broker.(frontend.PlexEncoder)
	if !ok {
		return nil, false
	}
	return enc.PlexHLSSession(id)
}

func (s surface) StopPlexHLS(id string) bool {
	if s.protocol != "plex" {
		return false
	}
	enc, ok := s.broker.(frontend.PlexEncoder)
	if !ok {
		return false
	}
	return enc.StopPlexHLS(id)
}

func (s surface) ForwardPlexMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.protocol != "plex" {
		return false
	}
	fwd, ok := s.broker.(frontend.PlexMediaForwarder)
	if !ok {
		return false
	}
	return fwd.ForwardPlexMedia(w, r, id)
}

func (s surface) ForwardPlexGrantMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.protocol != "plex" {
		return false
	}
	fwd, ok := s.broker.(frontend.PlexGrantMediaForwarder)
	if !ok {
		return false
	}
	return fwd.ForwardPlexGrantMedia(w, r, id)
}

func (s surface) PlexHLSDistributed(id string) bool {
	if s.protocol != "plex" {
		return false
	}
	dist, ok := s.broker.(frontend.PlexBindingStore)
	if !ok {
		return false
	}
	return dist.PlexHLSDistributed(id)
}

func (s surface) SavePlexBinding(ctx context.Context, id string, raw []byte) error {
	if s.protocol != "plex" {
		return core.ErrUnauthorized
	}
	store, ok := s.broker.(frontend.PlexBindingStore)
	if !ok {
		return core.ErrNotAvailable
	}
	return store.SavePlexBinding(ctx, id, raw)
}

func (s surface) LoadPlexBinding(ctx context.Context, id string) ([]byte, error) {
	if s.protocol != "plex" {
		return nil, core.ErrUnauthorized
	}
	store, ok := s.broker.(frontend.PlexBindingStore)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return store.LoadPlexBinding(ctx, id)
}

func (s surface) DeletePlexBinding(ctx context.Context, id string) error {
	if s.protocol != "plex" {
		return core.ErrUnauthorized
	}
	store, ok := s.broker.(frontend.PlexBindingDeleter)
	if !ok {
		return core.ErrNotAvailable
	}
	return store.DeletePlexBinding(ctx, id)
}
