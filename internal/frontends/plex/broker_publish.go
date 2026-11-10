package plex

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Server) bindHost(host frontend.Host) {
	if s == nil {
		return
	}
	if host.Sessions != nil {
		s.usePlaybackBroker(host.Sessions)
	}
	if host.LazyStreams != nil {
		s.hostLazyStreams = host.LazyStreams
	}
	if host.LANStreams != nil {
		s.hostLANStreams = host.LANStreams
	}
	if host.Tickets != nil {
		s.hostTickets = host.Tickets
	}
	if host.Events != nil {
		s.hostEvents = host.Events
	}
}

func (s *Server) unmountRuntime(b frontend.Sessions) {
	if s == nil {
		return
	}
	if b != nil && s.playbackBroker == b {
		s.usePlaybackBroker(nil)
	}
	s.releaseMountedPlayback()
}

// usePlaybackBroker attaches the shared session broker. Mounted Plex playback
// publishes through it. Timeline progress and stops are recorded only there.
func (s *Server) usePlaybackBroker(b frontend.Sessions) {
	if s == nil {
		return
	}
	if b == nil {
		s.playbackBroker = nil
		s.sessions.bind(nil)
		return
	}
	if rt, ok := b.(frontend.SessionRuntime); ok {
		s.runtime = rt
		if rt.HasBroker() {
			s.playbackBroker = b
			s.streamRuntime = rt
		} else {
			s.playbackBroker = nil
		}
		s.sessions.bind(rt.ActivityStore())
		if enc := rt.PlexEncoder(); enc != nil {
			s.usePlexEncoder(enc)
		}
		return
	}
	s.playbackBroker = b
	s.sessions.bind(nil)
}

func (s *Server) brokerRuntime() frontend.SessionRuntime {
	if s == nil || s.playbackBroker == nil {
		return nil
	}
	rt, _ := s.playbackBroker.(frontend.SessionRuntime)
	return rt
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
			Plex: &frontend.PlexHints{
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
func (s *Server) openPlexPlan(ctx context.Context, pr *Request, p *playRequest, admit func(context.Context) (frontend.PlexAdmission, error)) (frontend.Decision, error) {
	if s == nil || s.playbackBroker == nil || pr == nil || pr.User == nil || p == nil || p.sessionID == "" || admit == nil {
		return frontend.Decision{}, frontend.ErrNotAvailable
	}
	ctx = frontend.WithPlexAdmission(ctx, admit)
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
	return s.openPlexPlan(ctx, pr, p, func(context.Context) (frontend.PlexAdmission, error) {
		return frontend.PlexAdmission{
			Source: ps, Media: ps.Info, Caps: p.caps, Options: p.opts, PreferRemux: p.mde, Protocol: p.protocol,
			RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps,
		}, nil
	})
}

func (s *Server) notePlexEncoder(id string, format frontend.StreamFormat, dec frontend.Decision, offset time.Duration) {
	if rt := s.brokerRuntime(); rt != nil {
		rt.NotePlexEncoder(id, format, dec, offset)
	}
}

func (s *Server) reportBrokerTimeline(ctx context.Context, pr *Request, key frontend.RatingKey, offset, duration time.Duration, state, transcodeID string) bool {
	rt := s.brokerRuntime()
	if rt == nil || pr == nil || pr.User == nil || key == 0 {
		return false
	}
	access := s.plexAccess(pr)
	timeline := frontend.Timeline{PlaybackID: pr.SessionID, Position: offset, Duration: duration, State: state}
	id, err := rt.PlexSessionForPlayback(ctx, access, key, pr.SessionID, transcodeID)
	if (err != nil || id == "") && s.playback() != nil {
		if ps, ok := s.playback().SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil {
			if sid, serr := rt.PlexSessionForSource(ctx, access, key, ps.ID); serr == nil && sid != "" {
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
	return rt.ReportProgress(ctx, access, key, timeline) == nil
}

// adoptBrokerTimeline publishes a source the client is already playing when
// no broker session exists for this viewer and item. A mismatched playback
// header does not open a second session.
func (s *Server) adoptBrokerTimeline(ctx context.Context, pr *Request, key frontend.RatingKey) string {
	if s.playback() == nil || pr == nil || pr.User == nil {
		return ""
	}
	if rt := s.brokerRuntime(); rt != nil {
		if id, err := rt.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); err == nil && id != "" {
			return ""
		}
	}
	ps, ok := s.playback().SessionFor(pr.User.ID, pr.ClientID, key)
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

func (s *Server) recordManualSubtitle(ctx context.Context, viewer int64, key frontend.RatingKey, manual bool) error {
	if rt := s.brokerRuntime(); rt != nil {
		return rt.RecordAuthorizedSubtitleIntent(ctx, viewer, key, manual)
	}
	return nil
}
