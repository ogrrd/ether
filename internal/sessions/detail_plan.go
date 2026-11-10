package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const detailPlanBudget = 4 * time.Second
const detailPlanTTL = 30 * time.Second
const maxDetailPlans = 128

type detailFlight struct {
	done     chan struct{}
	plan     frontend.PlayPlan
	err      error
	sourceID string
	expires  time.Time
}

// DetailPlan is for a single explicitly requested playable item, never a
// listing. The flight is scoped to the authenticated request and bounded.
// An informational result cannot be opened as a session by the caller.
func (b *Broker) DetailPlan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, detailPlanBudget)
	defer cancel()
	if req.Viewer == nil || req.Device.ID == "" || req.Protocol == "" || req.Item == 0 || req.Channel != "" || req.Intent.Hints != nil {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	// Explicit source tracks require a generation pin through Open. Detail
	// planning does not create an openable session, so it cannot promise them.
	if req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil {
		return frontend.PlayPlan{}, frontend.ErrTrackSourceUnavailable
	}
	viewer, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		if ctx.Err() != nil {
			return frontend.PlayPlan{}, ctx.Err()
		}
		return frontend.PlayPlan{}, err
	}
	viewer, err = b.confirm(ctx, viewer, req.Caps)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	title, err := lookup(ctx, viewer, req.Device, req.Item)
	if ctx.Err() != nil {
		return frontend.PlayPlan{}, ctx.Err()
	}
	if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return frontend.PlayPlan{}, core.ErrNotFound
	}
	if req.Intent.ExactVersion {
		if req.Version == "" || b.deps.DB == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		item, _, err := h.resolveRecord(ctx, frontend.VersionScope{Viewer: viewer, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.Version)
		if err != nil || item != req.Item {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
	}
	req = copyRequest(req)
	req.Viewer = viewer
	req.Intent.Estimate = false
	// Incorporate live bitrate policy into the cache key, so a policy change
	// cannot reuse a decision produced under a different limit.
	if req.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (req.Caps.MaxBitrateKbps == 0 || limit < req.Caps.MaxBitrateKbps) {
			req.Caps.MaxBitrateKbps = limit
		}
	}
	if limit := b.deps.BitrateLimit(viewer); limit > 0 {
		if req.Caps.MaxBitrateKbps == 0 || limit < req.Caps.MaxBitrateKbps {
			req.Caps.MaxBitrateKbps = limit
		}
		if req.Intent.Options.MaxBitrateKbps == 0 || limit < req.Intent.Options.MaxBitrateKbps {
			req.Intent.Options.MaxBitrateKbps = limit
		}
	}
	data, err := json.Marshal(req)
	if err != nil || len(data) > 65536 {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	sum := sha256.Sum256(data)
	key := string(sum[:])
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if b.detailPlans == nil {
		b.detailPlans = make(map[string]*detailFlight)
	}
	for k, entry := range b.detailPlans {
		select {
		case <-entry.done:
			if !entry.expires.After(b.deps.Now()) {
				delete(b.detailPlans, k)
			}
		default:
		}
	}
	if entry := b.detailPlans[key]; entry != nil {
		waiting := false
		select {
		case <-entry.done:
		default:
			waiting = true
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return frontend.PlayPlan{}, ctx.Err()
		case <-entry.done:
		}
		if entry.err != nil {
			// The leader's own cancellation says nothing about this caller:
			// its failed entry is already gone, so plan afresh.
			if ctx.Err() == nil && (errors.Is(entry.err, context.Canceled) || errors.Is(entry.err, context.DeadlineExceeded)) {
				return b.DetailPlan(ctx, req)
			}
			return frontend.PlayPlan{}, entry.err
		}
		if source, ok := b.deps.Playback.Session(entry.sourceID); ok && source != nil {
			if reflect.DeepEqual(publicMedia(source.Info), entry.plan.Media) && ((entry.plan.ETag != "" && sourceRevision(source) == entry.plan.ETag) || (waiting && entry.plan.ETag == "")) {
				return b.detailDecision(ctx, req, viewer, title, source, entry.plan)
			}
		}
		// Unknown or changed sources are re-planned, never advertised as cached.
		b.mu.Lock()
		if b.detailPlans[key] == entry {
			delete(b.detailPlans, key)
		}
		b.mu.Unlock()
		return b.DetailPlan(ctx, req)
	}
	if len(b.detailPlans) >= maxDetailPlans {
		b.mu.Unlock()
		return frontend.PlayPlan{}, &frontend.SessionBusyError{RetryAfter: time.Second}
	}
	entry := &detailFlight{done: make(chan struct{})}
	b.detailPlans[key] = entry
	b.mu.Unlock()
	p, err := b.Plan(ctx, req)
	if err == nil && p.ETag != "" && b.deps.DB != nil {
		b.mu.Lock()
		planned, ok := b.plans[p.ID]
		b.mu.Unlock()
		if ok && planned.source != nil {
			p.DetailSource, err = b.issueDetailSource(ctx, req, planned.source, p.ETag)
		} else {
			err = core.ErrNotAvailable
		}
	}
	if p.ID != "" && (err != nil || p.Estimated || p.Media.Estimated || ctx.Err() != nil) {
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
		if !b.dropPlan(cleanup, p.ID, true) {
			b.mu.Lock()
			if planned, ok := b.plans[p.ID]; ok {
				planned.created = b.deps.Now().Add(-planLifetime - time.Nanosecond)
				b.plans[p.ID] = planned
			}
			b.mu.Unlock()
		}
		cancelCleanup()
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err == nil {
			err = core.ErrNotAvailable
		}
	}
	b.mu.Lock()
	entry.err = err
	if err == nil && !p.Estimated && !p.Media.Estimated {
		if planned, ok := b.plans[p.ID]; ok && planned.source != nil {
			entry.sourceID = planned.source.ID
			entry.expires = b.deps.Now().Add(detailPlanTTL)
			p.ID = ""
			entry.plan = p
		} else {
			entry.err = core.ErrNotAvailable
		}
	} else if err == nil {
		entry.err = core.ErrNotAvailable
	}
	if entry.err != nil {
		delete(b.detailPlans, key)
	}
	close(entry.done)
	b.mu.Unlock()
	if entry.err != nil {
		return frontend.PlayPlan{}, entry.err
	}
	p = entry.plan
	p.Media = publicMedia(p.Media)
	p.Decision.AudioFilters = slices.Clone(p.Decision.AudioFilters)
	return p, nil
}

// Re-evaluate mutable decision and preference policy even for an unchanged
// source. A cached source must not freeze a viewer's quality or track rules.
func (b *Broker) detailDecision(ctx context.Context, req frontend.PlanRequest, viewer *core.Viewer, title *core.Title, source *core.PlaySession, p frontend.PlayPlan) (frontend.PlayPlan, error) {
	req.Caps.Protocol = req.Protocol
	req.Caps.ClientID = clientKey(req.Protocol, req.Device)
	applySubtitleProfiles(&req.Caps)
	var err error
	if b.deps.SelectScopedTracks != nil {
		req.Intent.Options, err = b.deps.SelectScopedTracks(ctx, viewer, req.Device, title, source.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
	} else if b.deps.SelectTracks != nil {
		req.Intent.Options, err = b.deps.SelectTracks(ctx, viewer, title, source.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
	}
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	p.Decision, err = b.decideProfile(source, req)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding && decisionEncodes(p.Decision, source.Info) {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if (p.Decision.Kind == core.DecisionDirectPlay && !req.Intent.Options.DirectPlay) || (p.Decision.Kind == core.DecisionRemux && !req.Intent.Options.DirectStream) {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	p.DirectPlay, p.DirectStream, p.Transcoding = b.planAlternatives(source, req)
	p.HLS, p.HLSContainer = b.hlsPlanOutput(source, req, p.Decision)
	p.Media = publicMedia(source.Info)
	p.Decision.AudioFilters = slices.Clone(p.Decision.AudioFilters)
	return p, nil
}
