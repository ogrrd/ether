package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type fakeInputLease struct{ sealed bool }

func (l *fakeInputLease) ClaimInput(func(error)) error { return nil }
func (l *fakeInputLease) HeadersForInput(string) (map[string]string, error) {
	return nil, nil
}
func (l *fakeInputLease) SealInput() { l.sealed = true }
func (l *fakeInputLease) InputDone() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (l *fakeInputLease) InputErr() error { return nil }

type leasePlayback struct {
	*fakePlayback
	lease *fakeInputLease
}

func (p *leasePlayback) SourceRetirement(string) (core.SourceRetirement, bool) {
	return scriptOutcome{done: make(chan struct{})}, true
}
func (p *leasePlayback) PrepareSurfaceSource(context.Context, *core.User, core.RatingKey, core.ClientCaps, string, bool) (*core.PlaySession, core.SourceRetirement, error) {
	return nil, nil, core.ErrNotAvailable
}
func (p *leasePlayback) RetireSource(string, core.SourceRetirement) error { return nil }
func (p *leasePlayback) ReserveEncoderInput(context.Context, string, core.SourceRetirement) (core.EncoderInputLease, error) {
	p.lease = &fakeInputLease{}
	return p.lease, nil
}

type leaseRecordingTranscoder struct {
	*fakeMediaTranscoder
	last core.HLSRequest
}

func (f *leaseRecordingTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	f.last = r
	return f.fakeMediaTranscoder.StartHLS(ctx, r)
}
func (leaseRecordingTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
}

// Issue #410: local Plex encoder starts never carried an input lease, so
// every such session ended retirement as "encoder input producer
// retirement is unverified", warning on close and failing settlement.
// The broker must mint the same lease for Plex starts as for its own.
func TestStartPlexHLSMintsInputLease(t *testing.T) {
	ctx := context.Background()
	p := &leasePlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	tc := &leaseRecordingTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	broker, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	ps, err := p.Prepare(ctx, &core.User{ID: 2}, core.MovieKey(1), core.ClientCaps{ClientID: "plex-tv"})
	if err != nil {
		t.Fatal(err)
	}
	plexCtx := WithPlanAdmission(ctx, func(context.Context) (PlanAdmission, error) {
		return PlanAdmission{Source: ps, Media: ps.Info, Caps: core.ClientCaps{ClientID: "plex-tv"}, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}, nil
	})
	dev := core.Device{ID: "plex-tv"}
	plan, err := broker.Plan(plexCtx, frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: "plex", Item: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "plex-tv"}, Intent: frontend.PlayIntent{ClientSessionID: "plex-1", Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = broker.Open(plexCtx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: "plex", PlanID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	q := core.HLSRequest{SessionID: "plex-1", ReleaseID: ps.SourceReleaseID(), Info: ps.Info, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}}
	hs, err := broker.StartSegmentedHLS(ctx, viewer, ps.ID, "plex-1", q)
	if err != nil || hs == nil {
		t.Fatalf("local Plex encoder: %v", err)
	}
	if tc.last.InputLease == nil {
		t.Fatal("Plex local encoder started without an input lease; retirement will be unverified")
	}
	if p.lease == nil || tc.last.InputLease != core.EncoderInputLease(p.lease) {
		t.Fatal("encoder did not receive the lease minted for its source")
	}
}
