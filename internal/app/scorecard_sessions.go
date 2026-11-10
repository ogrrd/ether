//go:build scorecard && !plexparity

package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type embeddedScorecardSessions interface{ frontend.Sessions }
type scorecardSessionWrapper struct {
	embeddedScorecardSessions
	app *App
}

func scorecardSessions(a *App, s frontend.Sessions) frontend.Sessions {
	return scorecardSessionWrapper{embeddedScorecardSessions: s, app: a}
}
func scorecardErrorCategory(err error) string {
	for _, entry := range []struct {
		err  error
		name string
	}{{core.ErrNotAvailable, "not_available"}, {core.ErrNotFound, "not_found"}, {core.ErrUnauthorized, "unauthorized"}, {core.ErrStreamLimit, "stream_limit"}, {frontend.ErrTranscodeCapacity, "transcode_capacity"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"}} {
		if errors.Is(err, entry.err) {
			return entry.name
		}
	}
	return "other"
}
func (s scorecardSessionWrapper) failure(stage string, err error) {
	if err != nil {
		s.app.log.Info("scorecard session stage failure", "stage", stage, "category", scorecardErrorCategory(err))
	}
}
func (s scorecardSessionWrapper) HasBroker() bool {
	if rt := s.runtime(); rt != nil {
		return rt.HasBroker()
	}
	return s.embeddedScorecardSessions != nil
}
func (s scorecardSessionWrapper) PlexEncoder() frontend.PlexEncoder {
	if rt := s.runtime(); rt != nil && rt.PlexEncoder() != nil {
		return s
	}
	return nil
}
func (s scorecardSessionWrapper) Plan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	v, e := s.embeddedScorecardSessions.Plan(ctx, r)
	s.failure("plan", e)
	return v, e
}
func (s scorecardSessionWrapper) DetailPlan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if planner, ok := s.embeddedScorecardSessions.(frontend.DetailPlanner); ok {
		v, e := planner.DetailPlan(ctx, r)
		s.failure("detail_plan", e)
		return v, e
	}
	return frontend.PlayPlan{}, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) ResolveDetailSource(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.DetailSourceTarget, error) {
	if planner, ok := s.embeddedScorecardSessions.(frontend.DetailPlanner); ok {
		return planner.ResolveDetailSource(ctx, scope, handle)
	}
	return frontend.DetailSourceTarget{}, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) MatchDetailSession(ctx context.Context, scope frontend.VersionScope, handle, sessionID string) error {
	if planner, ok := s.embeddedScorecardSessions.(frontend.DetailPlanner); ok {
		return planner.MatchDetailSession(ctx, scope, handle, sessionID)
	}
	return core.ErrNotAvailable
}
func (s scorecardSessionWrapper) WarmDetail(ctx context.Context, r frontend.PlanRequest) bool {
	if warmer, ok := s.embeddedScorecardSessions.(frontend.DetailWarmer); ok {
		return warmer.WarmDetail(ctx, r)
	}
	return false
}
func (s scorecardSessionWrapper) Open(ctx context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	v, e := s.embeddedScorecardSessions.Open(ctx, r)
	s.failure("open", e)
	return v, e
}
func (s scorecardSessionWrapper) ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string) {
	media, ok := s.embeddedScorecardSessions.(interface {
		ServeSessionMedia(http.ResponseWriter, *http.Request, frontend.SessionAccess, string, string)
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	media.ServeSessionMedia(w, r, a, sessionID, resource)
}
func (s scorecardSessionWrapper) StartStream(ctx context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	v, e := s.embeddedScorecardSessions.StartStream(ctx, a, id, r)
	s.failure("start_stream", e)
	return v, e
}
func (s scorecardSessionWrapper) PlexSessionForPlayback(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playbackID, transcodeID string) (string, error) {
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionForPlayback(ctx, a, item, playbackID, transcodeID)
	}
	return "", core.ErrNotAvailable
}
func (s scorecardSessionWrapper) PlexSessionForSource(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playID string) (string, error) {
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionForSource(ctx, a, item, playID)
	}
	return "", core.ErrNotAvailable
}
func (s scorecardSessionWrapper) PlexSessionSource(id string) (string, bool) {
	if rt := s.runtime(); rt != nil {
		return rt.PlexSessionSource(id)
	}
	return "", false
}
func (s scorecardSessionWrapper) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if rt := s.runtime(); rt != nil {
		rt.NotePlexEncoder(id, format, decision, offset)
	}
}
func (s scorecardSessionWrapper) ReportItemProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	progress, ok := s.embeddedScorecardSessions.(frontend.ItemProgress)
	if !ok {
		return core.ErrNotAvailable
	}
	err := progress.ReportItemProgress(ctx, a, item, t)
	s.failure("item_progress", err)
	return err
}
func (s scorecardSessionWrapper) ReportProgress(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, t frontend.Timeline) error {
	if rt := s.runtime(); rt != nil {
		err := rt.ReportProgress(ctx, a, item, t)
		s.failure("report_progress", err)
		return err
	}
	return core.ErrNotAvailable
}
func (s scorecardSessionWrapper) StopPlayback(ctx context.Context, playID string) bool {
	if rt := s.runtime(); rt != nil {
		return rt.StopPlayback(ctx, playID)
	}
	return false
}
func (s scorecardSessionWrapper) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	if rt := s.runtime(); rt != nil {
		err := rt.StopProtocol(ctx, protocol)
		s.failure("stop_protocol", err)
		return err
	}
	return core.ErrNotAvailable
}
func (s scorecardSessionWrapper) RecordAuthorizedSubtitleIntent(ctx context.Context, viewer int64, key core.RatingKey, manual bool) error {
	if rt := s.runtime(); rt != nil {
		return rt.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
	}
	return core.ErrNotAvailable
}
func (s scorecardSessionWrapper) ConfirmPlexAdmission(ctx context.Context, viewer *core.Viewer, remote bool) (*core.Viewer, error) {
	if rt := s.runtime(); rt != nil {
		return rt.ConfirmPlexAdmission(ctx, viewer, remote)
	}
	return viewer, nil
}
func (s scorecardSessionWrapper) runtime() frontend.SessionRuntime {
	rt, _ := s.embeddedScorecardSessions.(frontend.SessionRuntime)
	return rt
}
func (s scorecardSessionWrapper) Playback() frontend.Playback {
	if rt := s.runtime(); rt != nil {
		return rt.Playback()
	}
	return nil
}
func (s scorecardSessionWrapper) Transcoder() frontend.Transcoder {
	if rt := s.runtime(); rt != nil {
		return rt.Transcoder()
	}
	return nil
}
func (s scorecardSessionWrapper) Downloads() frontend.DownloadRemuxer {
	if rt := s.runtime(); rt != nil {
		return rt.Downloads()
	}
	return nil
}
func (s scorecardSessionWrapper) SetDownloads(remuxer frontend.DownloadRemuxer) {
	if rt := s.runtime(); rt != nil {
		rt.SetDownloads(remuxer)
	}
}
func (s scorecardSessionWrapper) LatestDecision(key core.RatingKey) (core.DecisionTrace, bool) {
	if rt := s.runtime(); rt != nil {
		return rt.LatestDecision(key)
	}
	return core.DecisionTrace{}, false
}
func (s scorecardSessionWrapper) DB() *sql.DB {
	if rt := s.runtime(); rt != nil {
		return rt.DB()
	}
	return nil
}
func (s scorecardSessionWrapper) Cluster() frontend.ClusterNode {
	if rt := s.runtime(); rt != nil {
		return rt.Cluster()
	}
	return nil
}
func (s scorecardSessionWrapper) SessionHeartbeat() time.Duration {
	if rt := s.runtime(); rt != nil {
		return rt.SessionHeartbeat()
	}
	return 0
}
func (s scorecardSessionWrapper) PlaybackOwners() frontend.PlaybackOwners {
	if rt := s.runtime(); rt != nil {
		return rt.PlaybackOwners()
	}
	return nil
}
func (s scorecardSessionWrapper) LifecycleGates() frontend.Lifecycle {
	if rt := s.runtime(); rt != nil {
		return rt.LifecycleGates()
	}
	return nil
}
func (s scorecardSessionWrapper) NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) frontend.DecisionArms {
	if rt := s.runtime(); rt != nil {
		return rt.NewDecisionArms(ctx, db, now, node, log, wg)
	}
	return nil
}
func (s scorecardSessionWrapper) NewStartGuard() frontend.StartGuard {
	if rt := s.runtime(); rt != nil {
		return rt.NewStartGuard()
	}
	return nil
}
func (s scorecardSessionWrapper) NewPlayQueues() frontend.PlayQueues {
	if rt := s.runtime(); rt != nil {
		return rt.NewPlayQueues()
	}
	return nil
}
func (s scorecardSessionWrapper) NewVersionFlows(ctx context.Context, db *sql.DB) frontend.VersionFlows {
	if rt := s.runtime(); rt != nil {
		return rt.NewVersionFlows(ctx, db)
	}
	return nil
}
func (s scorecardSessionWrapper) NewDownloadQueueStore() frontend.DownloadQueueStore {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadQueueStore()
	}
	return nil
}
func (s scorecardSessionWrapper) NewVariantIDs(db *sql.DB, now func() time.Time) frontend.VariantIDs {
	if rt := s.runtime(); rt != nil {
		return rt.NewVariantIDs(db, now)
	}
	return nil
}
func (s scorecardSessionWrapper) NewDownloadJob(playback frontend.Playback, remuxer frontend.DownloadRemuxer) frontend.DownloadJob {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadJob(playback, remuxer)
	}
	return nil
}
func (s scorecardSessionWrapper) NewDownloadQueuePersistence(queue frontend.DownloadQueueStore, db *sql.DB, artifacts frontend.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) frontend.DownloadQueuePersistence {
	if rt := s.runtime(); rt != nil {
		return rt.NewDownloadQueuePersistence(queue, db, artifacts, log, expiry, storageID, users)
	}
	return nil
}
func (s scorecardSessionWrapper) PrepareSource(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error) {
	if rt := s.runtime(); rt != nil {
		return rt.PrepareSource(ctx, viewer, key, caps, release, wait)
	}
	return nil, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) PartSource(viewer int64, client string, key core.RatingKey, release string) frontend.PlexPartSource {
	if rt := s.runtime(); rt != nil {
		return rt.PartSource(viewer, client, key, release)
	}
	return frontend.PlexPartSource{}
}
func (s scorecardSessionWrapper) ServePartSource(w http.ResponseWriter, r *http.Request, sourceID string) {
	if rt := s.runtime(); rt != nil {
		rt.ServePartSource(w, r, sourceID)
		return
	}
	http.NotFound(w, r)
}
func (s scorecardSessionWrapper) ObserveStart(ctx context.Context, source frontend.HLSSession, id string) (frontend.StartObservation, bool) {
	if rt := s.runtime(); rt != nil {
		return rt.ObserveStart(ctx, source, id)
	}
	return nil, false
}
func (s scorecardSessionWrapper) ActivityStore() frontend.ActivityStore {
	if rt := s.runtime(); rt != nil {
		return rt.ActivityStore()
	}
	return nil
}
func (s scorecardSessionWrapper) BindStream(ctx context.Context, a frontend.SessionAccess, binding frontend.PlexStreamBinding) (int64, error) {
	if rt := s.runtime(); rt != nil {
		return rt.BindStream(ctx, a, binding)
	}
	return 0, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) ResolveStream(ctx context.Context, a frontend.SessionAccess, wireID int64) (frontend.PlexStreamBinding, error) {
	if rt := s.runtime(); rt != nil {
		return rt.ResolveStream(ctx, a, wireID)
	}
	return frontend.PlexStreamBinding{}, core.ErrNotFound
}
func (s scorecardSessionWrapper) OpenSidecar(ctx context.Context, a frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if rt := s.runtime(); rt != nil {
		return rt.OpenSidecar(ctx, a, receipt, wireID)
	}
	return nil, core.StreamInfo{}, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) StartPlexHLS(ctx context.Context, u *core.User, playID, id string, req core.HLSRequest) (core.HLSSession, error) {
	enc, ok := s.embeddedScorecardSessions.(frontend.PlexEncoder)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return enc.StartPlexHLS(ctx, u, playID, id, req)
}
func (s scorecardSessionWrapper) PlexHLSSession(id string) (core.HLSSession, bool) {
	enc, ok := s.embeddedScorecardSessions.(frontend.PlexEncoder)
	if !ok {
		return nil, false
	}
	return enc.PlexHLSSession(id)
}
func (s scorecardSessionWrapper) StopPlexHLS(id string) bool {
	enc, ok := s.embeddedScorecardSessions.(frontend.PlexEncoder)
	if !ok {
		return false
	}
	return enc.StopPlexHLS(id)
}
func (s scorecardSessionWrapper) ForwardPlexMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	fwd, ok := s.embeddedScorecardSessions.(frontend.PlexMediaForwarder)
	if !ok {
		return false
	}
	return fwd.ForwardPlexMedia(w, r, id)
}
func (s scorecardSessionWrapper) ForwardPlexGrantMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	fwd, ok := s.embeddedScorecardSessions.(frontend.PlexGrantMediaForwarder)
	if !ok {
		return false
	}
	return fwd.ForwardPlexGrantMedia(w, r, id)
}
func (s scorecardSessionWrapper) PlexHLSDistributed(id string) bool {
	dist, ok := s.embeddedScorecardSessions.(frontend.PlexBindingStore)
	if !ok {
		return false
	}
	return dist.PlexHLSDistributed(id)
}
func (s scorecardSessionWrapper) SavePlexBinding(ctx context.Context, id string, raw []byte) error {
	store, ok := s.embeddedScorecardSessions.(frontend.PlexBindingStore)
	if !ok {
		return core.ErrNotAvailable
	}
	return store.SavePlexBinding(ctx, id, raw)
}
func (s scorecardSessionWrapper) LoadPlexBinding(ctx context.Context, id string) ([]byte, error) {
	store, ok := s.embeddedScorecardSessions.(frontend.PlexBindingStore)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return store.LoadPlexBinding(ctx, id)
}
func (s scorecardSessionWrapper) DeletePlexBinding(ctx context.Context, id string) error {
	store, ok := s.embeddedScorecardSessions.(frontend.PlexBindingDeleter)
	if !ok {
		return core.ErrNotAvailable
	}
	return store.DeletePlexBinding(ctx, id)
}
