package sessions

import (
	"context"
	"crypto/rand"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func validPlexTranscodeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func (b *Broker) planPlexAdmission(ctx context.Context, req frontend.PlanRequest, admit func(context.Context) (PlanAdmission, error)) (frontend.PlayPlan, error) {
	if !validPlexTranscodeID(req.Intent.ClientSessionID) || req.Device.ID == "" || req.Item == 0 || req.Viewer == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	viewerCopy := *req.Viewer
	req.Viewer = &viewerCopy
	if req.Caps.ClientID == "" {
		req.Caps.ClientID = req.Device.ID
	}
	confirmed, err := b.confirm(ctx, req.Viewer, req.Caps)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	req.Viewer = confirmed
	if !b.reservePlan(req.Viewer.ID) {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	defer b.releasePlanReservation(req.Viewer.ID)
	unlock, err := b.gates.Acquire(ctx, "broker/owner/"+strconv.FormatInt(req.Viewer.ID, 10)+"/"+req.Caps.ClientID)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	defer unlock()
	admitted, err := admit(ctx)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	ps := admitted.Source
	if ps == nil || ps.ID == "" || ps.UserID != req.Viewer.ID || ps.Key != req.Item || ps.ClientID != req.Caps.ClientID {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.acquirePlanSource(ps.ID, true)
	stored := false
	defer func() {
		if !stored {
			b.releasePlanSource(ps.ID, false)
		}
	}()
	id := rand.Text()
	media := admitted.Media
	if len(media.Streams) == 0 && media.Container == "" {
		media = ps.Info
	}
	caps := req.Caps
	opts := req.Intent.Options
	if admitted.Caps.ClientID != "" {
		caps = admitted.Caps
		opts = admitted.Options
	}
	// A part-only client has not passed through the decision endpoint's
	// policy clamping. Apply the same current ceilings before Decide and
	// storing the plan, so Open does not mistake an omitted limit for a
	// policy violation. Open still rechecks for policy changes.
	limit := b.deps.BitrateLimit(req.Viewer)
	if caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if remote := b.deps.RemoteBitrateLimit(); remote > 0 && (limit == 0 || remote < limit) {
			limit = remote
		}
	}
	if limit > 0 {
		if caps.MaxBitrateKbps == 0 || caps.MaxBitrateKbps > limit {
			caps.MaxBitrateKbps = limit
		}
		if opts.MaxBitrateKbps == 0 || opts.MaxBitrateKbps > limit {
			opts.MaxBitrateKbps = limit
		}
	}
	protocol := admitted.Protocol
	if protocol == "" {
		protocol = string(req.Protocol)
	}
	preferRemux := admitted.PreferRemux || req.Intent.PreferRemux
	dec, err := Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{
		Session: ps, Caps: caps, Options: opts, PreferRemux: preferRemux,
		Protocol: protocol, RemoteMaxBitrateKbps: admitted.RemoteMaxBitrateKbps,
	})
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	req.Caps = caps
	req.Intent.Options = opts
	req.Intent.PreferRemux = preferRemux
	etag := sourceRevision(ps)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.plans[id] = brokerPlan{request: req, source: ps, decision: dec, created: b.deps.Now(), etag: etag, external: true}
	stored = true
	b.mu.Unlock()
	return frontend.PlayPlan{ID: id, Media: publicMedia(media), Decision: dec, ETag: etag}, nil
}

// reusePlexSession updates the broker session already published for a Plex
// client transcode id. The plan lease is consumed when ok is true.
func (b *Broker) reusePlexSession(_ context.Context, v *core.Viewer, req frontend.OpenRequest, p brokerPlan, ps *core.PlaySession, id string) (frontend.Session, bool, error) {
	b.mu.Lock()
	old := b.sessions[id]
	if old == nil {
		b.mu.Unlock()
		return frontend.Session{}, false, nil
	}
	if old.public.Protocol != "plex" || old.public.ViewerID != v.ID || old.public.Device.ID != req.Device.ID {
		b.mu.Unlock()
		return frontend.Session{}, false, core.ErrUnauthorized
	}
	var stopID string
	var stop bool
	if old.playID == ps.ID {
		b.mu.Unlock()
		b.releasePlanSource(ps.ID, p.pinned)
	} else {
		stop = b.releaseSessionSourceLocked(old.playID)
		stopID = old.playID
		old.playID = ps.ID
		b.transferPlanSourceLocked(ps.ID)
		b.mu.Unlock()
		if stop && stopID != "" && stopID != ps.ID {
			b.deps.Playback.Stop(stopID)
		}
	}
	b.mu.Lock()
	current := b.sessions[id]
	if current == nil || current.public.ViewerID != v.ID {
		b.mu.Unlock()
		return frontend.Session{}, true, core.ErrNotAvailable
	}
	current.playID = ps.ID
	current.playbackID = p.request.Intent.PlaybackID
	current.decision = p.decision
	current.caps = p.request.Caps
	current.opts = p.request.Intent.Options
	current.clientID = p.request.Caps.ClientID
	current.sourceETag = p.etag
	b.bindings.Put(&Binding[frontend.PlayIntent]{ID: id, UserID: v.ID, ClientID: current.clientID, Key: current.public.Item, PlayID: ps.ID, PlaybackID: current.playbackID, Requested: p.request.Intent, Decision: p.decision, Info: ps.Info, Caps: current.caps, Opts: current.opts, UpdatedAt: b.deps.Now()})
	b.owners.BindFor("broker", v.ID, current.clientID, current.public.Item, current.playbackID, ps.ID, true, b.deps.Now())
	pub := current.public
	b.mu.Unlock()
	return pub, true, nil
}

// PlexSessionForPlayback resolves the broker session for a Plex timeline.
// A client transcode id wins. Otherwise the playback header selects the
// newest match. An empty header matches only when exactly one session exists.
func (b *Broker) PlexSessionForPlayback(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playbackID, transcodeID string) (string, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || a.Protocol != "plex" || a.Device.ID == "" || a.Owner || item == 0 {
		return "", core.ErrUnauthorized
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", core.ErrNotAvailable
	}
	if transcodeID != "" {
		if s := b.sessions[transcodeID]; s != nil && plexViewerSession(s, viewer.ID, a.Device.ID, item) {
			return transcodeID, nil
		}
	}
	var ids []string
	newest, newestAt := "", time.Time{}
	for id, s := range b.sessions {
		if !plexViewerSession(s, viewer.ID, a.Device.ID, item) {
			continue
		}
		if playbackID != "" && s.playbackID != playbackID {
			continue
		}
		ids = append(ids, id)
		if newest == "" || s.public.StartedAt.After(newestAt) {
			newest, newestAt = id, s.public.StartedAt
		}
	}
	if playbackID == "" && len(ids) != 1 {
		return "", core.ErrNotFound
	}
	if newest == "" {
		return "", core.ErrNotFound
	}
	return newest, nil
}

// PlexSessionForSource is the broker session that owns a prepared source for
// this viewer and item. The newest match wins when more than one remains.
func (b *Broker) PlexSessionForSource(ctx context.Context, a frontend.SessionAccess, item frontend.RatingKey, playID string) (string, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil || a.Protocol != "plex" || a.Device.ID == "" || a.Owner || item == 0 || playID == "" {
		return "", core.ErrUnauthorized
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", core.ErrNotAvailable
	}
	newest, newestAt := "", time.Time{}
	for id, s := range b.sessions {
		if !plexViewerSession(s, viewer.ID, a.Device.ID, item) || s.playID != playID {
			continue
		}
		if newest == "" || s.public.StartedAt.After(newestAt) {
			newest, newestAt = id, s.public.StartedAt
		}
	}
	if newest == "" {
		return "", core.ErrNotFound
	}
	return newest, nil
}

// PlexSessionSource is the playback source owned by a published Plex session.
func (b *Broker) PlexSessionSource(id string) (string, bool) {
	if b == nil || id == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[id]
	if s == nil || s.public.Protocol != "plex" || s.playID == "" {
		return "", false
	}
	return s.playID, true
}

func plexViewerSession(s *brokerSession, viewer int64, device string, item core.RatingKey) bool {
	return s != nil && s.public.Protocol == "plex" && s.public.ViewerID == viewer && s.public.Device.ID == device && s.public.Item == item
}

// NotePlexEncoder records that the Plex adapter started ffmpeg under the
// client transcode id, which is also the broker session id.
func (b *Broker) NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration) {
	if b == nil || !validPlexTranscodeID(id) {
		return
	}
	b.mu.Lock()
	s := b.sessions[id]
	if s == nil || s.public.Protocol != "plex" {
		b.mu.Unlock()
		return
	}
	s.format = format
	s.streamPosition = offset
	s.decision = decision
	playID := s.playID
	if entry, ok := b.bindings.Get(id); ok {
		entry.Started = true
		entry.Decision = decision
		entry.Offset = offset
		entry.UpdatedAt = b.deps.Now()
		b.bindings.Put(entry)
	}
	b.mu.Unlock()
	b.recordPlaybackDecision(playID, decision)
}

// StopProtocol releases every local session for one mounted surface.
func (b *Broker) StopProtocol(ctx context.Context, protocol frontend.Protocol) error {
	if b == nil || protocol == "" {
		return core.ErrNotAvailable
	}
	b.mu.Lock()
	var sessionIDs, planIDs []string
	for id, s := range b.sessions {
		if s.public.Protocol == protocol {
			sessionIDs = append(sessionIDs, id)
		}
	}
	for id, plan := range b.plans {
		if plan.request.Protocol == protocol {
			planIDs = append(planIDs, id)
		}
	}
	b.mu.Unlock()
	var err error
	for _, id := range sessionIDs {
		b.mu.Lock()
		s := b.sessions[id]
		var copy brokerSession
		if s != nil {
			copy = *s
		}
		b.mu.Unlock()
		if s == nil {
			continue
		}
		if stopErr := b.stopLocked(ctx, &copy); stopErr != nil && err == nil {
			err = stopErr
		}
	}
	for _, id := range planIDs {
		b.dropPlexPlan(id)
	}
	return err
}

func (b *Broker) dropPlexPlan(id string) {
	b.mu.Lock()
	plan, ok := b.plans[id]
	if ok {
		delete(b.plans, id)
	}
	b.mu.Unlock()
	if ok && plan.source != nil {
		b.releasePlanSource(plan.source.ID, plan.pinned)
	}
}

// PlanAdmission and its context helpers live in internal/frontend
// (host_sessions_experience.go).
type PlanAdmission = frontend.PlanAdmission

var WithPlanAdmission = frontend.WithPlanAdmission
