package sessions

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// BindSurface prevents an adapter from submitting another surface's identity.
func BindSurface(b frontend.Sessions, protocol frontend.Protocol) frontend.Sessions {
	return surface{broker: b, protocol: protocol}
}

// BindStreams is BindSurface for the Plex stream methods. The returned
// value rejects any access whose protocol is not plex.
func BindStreams(b *Broker) StreamBindings {
	return surface{broker: b, protocol: "plex"}
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
	b, ok := s.broker.(*Broker)
	if !ok {
		return "", false
	}
	return b.PlexSessionSource(id)
}

func (s surface) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if b, ok := s.broker.(*Broker); ok {
		b.NotePlexEncoder(id, format, decision, offset)
	}
}

func (s surface) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	if protocol != s.protocol {
		return core.ErrUnauthorized
	}
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.StopProtocol(ctx, protocol)
}

func (s surface) BindStream(ctx context.Context, a frontend.SessionAccess, binding StreamBinding) (int64, error) {
	if a.Protocol != s.protocol {
		return 0, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		BindStream(context.Context, frontend.SessionAccess, StreamBinding) (int64, error)
	})
	if !ok {
		return 0, core.ErrNotAvailable
	}
	return api.BindStream(ctx, a, binding)
}

func (s surface) ResolveStream(ctx context.Context, a frontend.SessionAccess, wireID int64) (StreamBinding, error) {
	if a.Protocol != s.protocol {
		return StreamBinding{}, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		ResolveStream(context.Context, frontend.SessionAccess, int64) (StreamBinding, error)
	})
	if !ok {
		return StreamBinding{}, core.ErrNotAvailable
	}
	return api.ResolveStream(ctx, a, wireID)
}

func (s surface) OpenSidecar(ctx context.Context, a frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if a.Protocol != s.protocol {
		return nil, core.StreamInfo{}, core.ErrUnauthorized
	}
	api, ok := s.broker.(interface {
		OpenSidecar(context.Context, frontend.SessionAccess, auth.Acceptance, int64) (io.ReadCloser, core.StreamInfo, error)
	})
	if !ok {
		return nil, core.StreamInfo{}, core.ErrNotAvailable
	}
	return api.OpenSidecar(ctx, a, receipt, wireID)
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

// StreamBindings lives in internal/frontend (host_sessions_experience.go).
type StreamBindings = frontend.StreamBindings

// PlexActivities reaches the broker's Plex activity store.
func (b *Broker) PlexActivities() frontend.PlaybackActivityStore {
	if b == nil {
		return nil
	}
	return b
}

// PlexActivities reaches the underlying broker's Plex activity store.
func (s surface) PlexActivities() frontend.PlaybackActivityStore {
	if src, ok := s.broker.(frontend.PlaybackActivitySource); ok {
		return src.PlexActivities()
	}
	return nil
}

// RecordAuthorizedSubtitleIntent forwards to the underlying broker.
func (s surface) RecordAuthorizedSubtitleIntent(ctx context.Context, viewer int64, key core.RatingKey, manual bool) error {
	b, ok := s.broker.(*Broker)
	if !ok {
		return core.ErrNotAvailable
	}
	return b.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
}
