//go:build scorecard && !plexparity

package app

import (
	"context"
	"errors"
	"net/http"
	"time"

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
func (s scorecardSessionWrapper) TrickplayOffer(ctx context.Context, a frontend.SessionAccess, item core.RatingKey) (frontend.TrickplayOffer, bool) {
	tiles, ok := s.embeddedScorecardSessions.(frontend.TrickplayTiles)
	if !ok {
		return frontend.TrickplayOffer{}, false
	}
	return tiles.TrickplayOffer(ctx, a, item)
}
func (s scorecardSessionWrapper) TrickplaySheet(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, index int) ([]byte, error) {
	tiles, ok := s.embeddedScorecardSessions.(frontend.TrickplayTiles)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return tiles.TrickplaySheet(ctx, a, item, index)
}
func (s scorecardSessionWrapper) TrickplayAvailable(id string) bool {
	if tp, ok := s.embeddedScorecardSessions.(interface{ TrickplayAvailable(string) bool }); ok {
		return tp.TrickplayAvailable(id)
	}
	return false
}
func (s scorecardSessionWrapper) TrickplayBIF(id string) ([]byte, bool) {
	if tp, ok := s.embeddedScorecardSessions.(interface{ TrickplayBIF(string) ([]byte, bool) }); ok {
		return tp.TrickplayBIF(id)
	}
	return nil, false
}
func (s scorecardSessionWrapper) TrickplayFrame(id string, at time.Duration) ([]byte, bool) {
	if tp, ok := s.embeddedScorecardSessions.(interface{ TrickplayFrame(string, time.Duration) ([]byte, bool) }); ok {
		return tp.TrickplayFrame(id, at)
	}
	return nil, false
}
func (s scorecardSessionWrapper) CancelTrickplay(id string) {
	if canceler, ok := s.embeddedScorecardSessions.(frontend.TrickplayCanceler); ok {
		canceler.CancelTrickplay(id)
	}
}
func (s scorecardSessionWrapper) PlexSessionForPlayback(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playbackID, transcodeID string) (string, error) {
	lookup, ok := s.embeddedScorecardSessions.(interface {
		PlexSessionForPlayback(context.Context, frontend.SessionAccess, frontend.RatingKey, string, string) (string, error)
	})
	if !ok {
		return "", core.ErrNotAvailable
	}
	return lookup.PlexSessionForPlayback(ctx, a, item, playbackID, transcodeID)
}
func (s scorecardSessionWrapper) PlexSessionForSource(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playID string) (string, error) {
	lookup, ok := s.embeddedScorecardSessions.(interface {
		PlexSessionForSource(context.Context, frontend.SessionAccess, frontend.RatingKey, string) (string, error)
	})
	if !ok {
		return "", core.ErrNotAvailable
	}
	return lookup.PlexSessionForSource(ctx, a, item, playID)
}
func (s scorecardSessionWrapper) PlexSessionSource(id string) (string, bool) {
	source, ok := s.embeddedScorecardSessions.(interface{ PlexSessionSource(string) (string, bool) })
	if !ok {
		return "", false
	}
	return source.PlexSessionSource(id)
}
func (s scorecardSessionWrapper) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if note, ok := s.embeddedScorecardSessions.(interface {
		NotePlexEncoder(string, frontend.StreamFormat, core.Decision, time.Duration)
	}); ok {
		note.NotePlexEncoder(id, format, decision, offset)
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
	progress, ok := s.embeddedScorecardSessions.(interface {
		ReportProgress(context.Context, frontend.SessionAccess, core.RatingKey, frontend.Timeline) error
	})
	if !ok {
		return core.ErrNotAvailable
	}
	err := progress.ReportProgress(ctx, a, item, t)
	s.failure("report_progress", err)
	return err
}
func (s scorecardSessionWrapper) StopPlayback(ctx context.Context, playID string) bool {
	stop, ok := s.embeddedScorecardSessions.(interface {
		StopPlayback(context.Context, string) bool
	})
	if !ok {
		return false
	}
	return stop.StopPlayback(ctx, playID)
}
func (s scorecardSessionWrapper) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	stop, ok := s.embeddedScorecardSessions.(interface {
		StopProtocol(context.Context, frontend.Protocol) error
	})
	if !ok {
		return core.ErrNotAvailable
	}
	err := stop.StopProtocol(ctx, protocol)
	s.failure("stop_protocol", err)
	return err
}
