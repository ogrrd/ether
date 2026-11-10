package plex

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// UsePlaybackBroker attaches the shared session broker. Mounted Plex playback
// publishes through it. Timeline progress and stops are recorded only there.
func (s *Server) UsePlaybackBroker(b frontend.Sessions) {
	if s != nil {
		s.playbackBroker = b
		s.sessions.bind(s.d.SessionStores.UnderlyingActivityStore(b))
	}
}

func (s *Server) plexPlanRequest(pr *Request, p *playRequest) frontend.PlanRequest {
	device := frontend.Device{ID: pr.ClientID, Client: pr.Product, Product: pr.Product, Version: pr.Version, Platform: pr.Platform, DeviceName: pr.Device, Language: pr.Language}
	caps := p.caps
	if caps.ClientID == "" {
		caps.ClientID = pr.ClientID
	}
	return frontend.PlanRequest{
		Viewer: pr.User, Device: device, Protocol: "plex", Item: p.key, Caps: caps,
		Intent: frontend.PlayIntent{
			Options: p.opts, PreferRemux: p.mde, PlaybackID: pr.SessionID, ClientSessionID: p.sessionID,
			Hints: &frontend.PlayHints{
				ManualSubtitle: p.manualSubtitle,
				Collection:     p.collection, MediaIndex: p.mediaIndex, PartReleaseID: p.partReleaseID,
				AudioSelection: p.audioSel, SubtitleSelection: p.subSel, SubtitleSet: p.subSet, SubtitleStream: p.subStream,
			},
		},
	}
}

// replayPlay is a repeated start of a decision the plan already made.
// The stored caps and options are the inputs of that decision.
func replayPlay(p playRequest, entry *transcodeEntry) *playRequest {
	if entry == nil {
		return &p
	}
	p.caps = entry.caps
	p.opts = entry.opts
	p.protocol = entry.requested.protocol
	p.mde = entry.requested.mde
	return &p
}

func (s *Server) plexAccess(pr *Request) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: pr.User, Device: frontend.Device{ID: pr.ClientID}, Protocol: "plex"}
}

// openPlexPlan runs version and track resolution inside Plan, then publishes
// the decision Plan made. The adapter renders that decision.
func (s *Server) openPlexPlan(ctx context.Context, pr *Request, p *playRequest, admit func(context.Context) (frontend.PlanAdmission, error)) (frontend.Decision, error) {
	if s == nil || s.playbackBroker == nil || pr == nil || pr.User == nil || p == nil || p.sessionID == "" || admit == nil {
		return frontend.Decision{}, frontend.ErrNotAvailable
	}
	ctx = frontend.WithPlanAdmission(ctx, admit)
	plan, err := s.playbackBroker.Plan(ctx, s.plexPlanRequest(pr, p))
	if err != nil {
		return frontend.Decision{}, fmt.Errorf("Plex broker plan: %w", err)
	}
	_, err = s.playbackBroker.Open(ctx, frontend.OpenRequest{Protocol: "plex", PlanID: plan.ID, Viewer: pr.User, Device: frontend.Device{ID: pr.ClientID, Client: pr.Product, Product: pr.Product, Version: pr.Version, Platform: pr.Platform, DeviceName: pr.Device, Language: pr.Language}})
	if err != nil {
		return frontend.Decision{}, fmt.Errorf("Plex broker open: %w", err)
	}
	return plan.Decision, nil
}

// publishBrokerPlayback plans a source that is already open.
func (s *Server) publishBrokerPlayback(ctx context.Context, pr *Request, p *playRequest, ps *frontend.PlaySession) (frontend.Decision, error) {
	if ps == nil {
		return frontend.Decision{}, frontend.ErrNotAvailable
	}
	return s.openPlexPlan(ctx, pr, p, func(context.Context) (frontend.PlanAdmission, error) {
		return frontend.PlanAdmission{
			Source: ps, Media: ps.Info, Caps: p.caps, Options: p.opts, PreferRemux: p.mde, Protocol: p.protocol,
			RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps,
		}, nil
	})
}

type plexEncoderNote interface {
	NotePlexEncoder(id string, format frontend.StreamFormat, decision frontend.Decision, offset time.Duration)
}

func (s *Server) notePlexEncoder(id string, format frontend.StreamFormat, dec frontend.Decision, offset time.Duration) {
	if s == nil || s.playbackBroker == nil {
		return
	}
	if note, ok := s.playbackBroker.(plexEncoderNote); ok {
		note.NotePlexEncoder(id, format, dec, offset)
	}
}

type plexPlaybackLookup interface {
	PlexSessionForPlayback(context.Context, frontend.SessionAccess, frontend.RatingKey, string, string) (string, error)
	PlexSessionForSource(context.Context, frontend.SessionAccess, frontend.RatingKey, string) (string, error)
	PlexSessionSource(string) (string, bool)
}

func (s *Server) reportBrokerTimeline(ctx context.Context, pr *Request, key frontend.RatingKey, offset, duration time.Duration, state, transcodeID string) bool {
	if s == nil || s.playbackBroker == nil || pr == nil || pr.User == nil || key == 0 {
		return false
	}
	lookup, ok := s.playbackBroker.(plexPlaybackLookup)
	if !ok {
		return false
	}
	access := s.plexAccess(pr)
	timeline := frontend.Timeline{PlaybackID: pr.SessionID, Position: offset, Duration: duration, State: state}
	id, err := lookup.PlexSessionForPlayback(ctx, access, key, pr.SessionID, transcodeID)
	if (err != nil || id == "") && s.d.Playback != nil {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil {
			if sid, serr := lookup.PlexSessionForSource(ctx, access, key, ps.ID); serr == nil && sid != "" {
				id = sid
			}
		}
	}
	if id == "" {
		id = s.adoptBrokerTimeline(ctx, pr, key)
	}
	if id != "" {
		return s.playbackBroker.Report(ctx, access, id, timeline) == nil
	}
	if progress, ok := s.playbackBroker.(interface {
		ReportProgress(context.Context, frontend.SessionAccess, frontend.RatingKey, frontend.Timeline) error
	}); ok {
		return progress.ReportProgress(ctx, access, key, timeline) == nil
	}
	return false
}

// adoptBrokerTimeline publishes a source the client is already playing when
// no broker session exists for this viewer and item. A mismatched playback
// header does not open a second session.
func (s *Server) adoptBrokerTimeline(ctx context.Context, pr *Request, key frontend.RatingKey) string {
	if s.d.Playback == nil || pr == nil || pr.User == nil {
		return ""
	}
	if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok {
		if id, err := lookup.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); err == nil && id != "" {
			return ""
		}
	}
	ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key)
	if !ok || ps == nil {
		return ""
	}
	sessionID := pr.SessionID
	if sessionID == "" {
		sessionID = pr.ClientID + "-" + key.String()
	}
	sessionID = sanitizeSessionID(sessionID)
	if sessionID == "" {
		return ""
	}
	caps := s.declaredCaps(pr)
	if caps.ClientID == "" {
		caps.ClientID = pr.ClientID
	}
	play := playRequest{key: key, caps: caps, sessionID: sessionID, audioSel: -1, subSel: -1}
	play.opts.DirectPlay = true
	play.opts.DirectStream = true
	if _, err := s.publishBrokerPlayback(ctx, pr, &play, ps); err != nil {
		s.log.Info("timeline broker adopt failed", "key", key, "err", err)
		return ""
	}
	return sessionID
}

type plexProtocolStop interface {
	StopProtocol(context.Context, frontend.Protocol) error
}

func (s *Server) recordManualSubtitle(ctx context.Context, viewer int64, key frontend.RatingKey, manual bool) error {
	if b, ok := s.playbackBroker.(interface {
		RecordAuthorizedSubtitleIntent(context.Context, int64, frontend.RatingKey, bool) error
	}); ok {
		return b.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
	}
	return nil
}
