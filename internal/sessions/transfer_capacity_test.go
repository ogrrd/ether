package sessions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

// One viewer playing a few titles for more than an hour must never be refused
// a part read or a transcode start, and the admission tickets stay bounded.
func TestViewerHourOfPlaybackReusesTransferTickets(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 8}
	viewer := &core.Viewer{ID: 42}
	device := core.Device{ID: "plex-web"}
	start := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := start
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		clock = clock.Add(d)
		clockMu.Unlock()
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	b, err := NewBroker(BrokerDeps{
		DB: db, Events: bus, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{},
		ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Now:          now,
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k, Released: start.Add(-time.Hour)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	titles := []core.RatingKey{core.MovieKey(1), core.MovieKey(2), core.MovieKey(3)}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "plex"}
	tickets := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND viewer_id=?`, brokerTransferNamespace, viewer.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	open := func(item core.RatingKey, sessionID string) string {
		t.Helper()
		ps, err := playback.Prepare(ctx, &core.User{ID: viewer.ID}, item, core.ClientCaps{ClientID: device.ID})
		if err != nil {
			t.Fatal(err)
		}
		plexCtx := WithPlanAdmission(ctx, func(context.Context) (PlanAdmission, error) {
			return PlanAdmission{Source: ps, Media: ps.Info, Caps: core.ClientCaps{ClientID: device.ID}, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}, nil
		})
		plan, err := b.Plan(plexCtx, frontend.PlanRequest{
			Viewer: viewer, Device: device, Protocol: "plex", Item: item, Caps: core.ClientCaps{ClientID: device.ID},
			Intent: frontend.PlayIntent{ClientSessionID: sessionID, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		opened, err := b.Open(plexCtx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "plex", PlanID: plan.ID})
		if err != nil {
			t.Fatalf("open %s: %v", sessionID, err)
		}
		return opened.ID
	}
	const rounds = 600
	maxTickets := 0
	for i := 0; i < rounds; i++ {
		item := titles[i%len(titles)]
		open(item, fmt.Sprintf("part-%d", i))
		current := open(item, fmt.Sprintf("transcode-%d", i))
		if n := tickets(); n > maxTickets {
			maxTickets = n
		}
		if maxTickets > 16 {
			t.Fatalf("ticket count %d after %d part reads and transcode starts", maxTickets, (i+1)*2)
		}
		if err := b.Stop(ctx, access, current); err != nil {
			t.Fatal(err)
		}
		advance(7 * time.Second)
	}
	if plays := rounds * 2; plays < 1000 {
		t.Fatalf("plays = %d", plays)
	}
	if now().Sub(start) < time.Hour {
		t.Fatalf("clock advanced %s", now().Sub(start))
	}
	if maxTickets > 16 {
		t.Fatalf("ticket count peaked at %d", maxTickets)
	}
	if n := tickets(); n > 2 {
		t.Fatalf("tickets after the hour = %d, peak %d", n, maxTickets)
	}
}

func TestTransferCapRefusesOnlyDistinctLiveStreams(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	b, err := NewBroker(BrokerDeps{
		DB: db, Events: bus, Playback: playback, Progress: &fakeProgress{}, ActiveViewer: active,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	const viewer int64 = 7
	for i := 0; i < transferViewerCap; i++ {
		device := fmt.Sprintf("dev-%d", i)
		item := core.MovieKey(int64(i + 1))
		playID := fmt.Sprintf("play-%d", i)
		sessionID := fmt.Sprintf("sess-%d", i)
		h, err := b.admitSessionBarrier(ctx, viewer, "plex", device, item, playID, sessionID)
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		b.mu.Lock()
		b.sessions[sessionID] = &brokerSession{
			public:          frontend.Session{ID: sessionID, ViewerID: viewer, Device: core.Device{ID: device}, Protocol: "plex", Item: item, StartedAt: time.Unix(int64(i), 0)},
			transferBarrier: h, playID: playID,
		}
		b.mu.Unlock()
	}
	_, err = b.admitSessionBarrier(ctx, viewer, "plex", "dev-overflow", core.MovieKey(9000), "play-overflow", "sess-overflow")
	var cap *capability.CapacityError
	if !errors.As(err, &cap) || cap.Scope != "viewer" {
		t.Fatalf("live cap err=%v", err)
	}
	b.mu.Lock()
	delete(b.sessions, "sess-0")
	b.mu.Unlock()
	if _, err = b.admitSessionBarrier(ctx, viewer, "plex", "dev-next", core.MovieKey(9001), "play-next", "sess-next"); err != nil {
		t.Fatalf("evicting the oldest idle ticket: %v", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND viewer_id=?`, brokerTransferNamespace, viewer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > transferViewerCap {
		t.Fatalf("tickets = %d, cap %d", n, transferViewerCap)
	}
}
