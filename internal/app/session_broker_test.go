package app

import (
	"context"
	"fmt"
	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSessionStateEventsCrossProtocolsWithoutPlexDuplicate(t *testing.T) {
	db := store.OpenTest(t)
	watch := state.New(db, state.Options{})
	defer watch.Close(context.Background())
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plex, err := events.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 2}, Protocol: "plex"})
	if err != nil {
		t.Fatal(err)
	}
	jf, err := events.Subscribe(ctx, frontend.EventScope{Viewer: &core.Viewer{ID: 2}, Protocol: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	unsub := subscribeSessionState(watch, events, nil)
	defer unsub()
	key := core.MovieKey(5)
	if err := watch.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: "plex", ClientID: "p"}), 2, key); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-jf:
		if ev.Item != key || ev.OriginProtocol != "plex" {
			t.Fatalf("jellyfin event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing Plex event")
	}
	select {
	case ev := <-jf:
		if ev.Type != "WatchStateChanged" {
			t.Fatalf("watch event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing Plex watch event")
	}
	select {
	case ev := <-plex:
		t.Fatalf("Plex received its own event: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
	if err := watch.Unscrobble(state.WithOrigin(ctx, state.Origin{Surface: "jellyfin", ClientID: "j"}), 2, key); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-plex:
		if ev.Item != key || ev.OriginProtocol != "jellyfin" {
			t.Fatalf("plex event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing Jellyfin event")
	}
	select {
	case ev := <-plex:
		if ev.Type != "WatchStateChanged" {
			t.Fatalf("watch event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing Jellyfin watch event")
	}
	select {
	case ev := <-jf:
		t.Fatalf("Jellyfin received its own event: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
}

type activityPlayback struct {
	mu      sync.Mutex
	next    int
	entries map[string]*core.PlaySession
}

func TestOwnerActivityHidesPreparedSources(t *testing.T) {
	p := &activityPlayback{entries: map[string]*core.PlaySession{
		"prepared": {ID: "prepared", PreparedOnly: true},
		"playing":  {ID: "playing"},
	}}
	rows := (ownerPlayback{Playback: p, app: &App{}}).Sessions()
	if len(rows) != 1 || rows[0].ID != "playing" {
		t.Fatalf("unexpected Activity rows: %+v", rows)
	}
}

func (p *activityPlayback) PrepareSurface(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if release != "" || exact {
		return nil, core.ErrNotAvailable
	}
	return p.Prepare(ctx, u, k, c)
}
func (p *activityPlayback) SessionForSurface(_ string, user int64, client string, key core.RatingKey, _ string, _ bool) (*core.PlaySession, bool) {
	return p.SessionFor(user, client, key)
}
func (p *activityPlayback) MarkSurfaceOpened(id string) bool   { _, ok := p.Session(id); return ok }
func (p *activityPlayback) MarkSurfacePrepared(id string) bool { _, ok := p.Session(id); return ok }

func (p *activityPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return core.MediaInfo{Estimated: true}
}
func (p *activityPlayback) Prepare(_ context.Context, u *core.User, k core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := fmt.Sprintf("source-%d", p.next)
	s := &core.PlaySession{ID: id, UserID: u.ID, ClientID: c.ClientID, Key: k, Client: c, Info: core.MediaInfo{Container: "mp4"}}
	p.entries[id] = s
	return s, nil
}
func (p *activityPlayback) Session(id string) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.entries[id]
	return s, ok
}
func (p *activityPlayback) SessionFor(user int64, client string, key core.RatingKey) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.entries {
		if s.UserID == user && s.ClientID == client && s.Key == key {
			return s, true
		}
	}
	return nil, false
}
func (*activityPlayback) ServeRange(http.ResponseWriter, *http.Request, string) {}
func (*activityPlayback) InputURL(string) string                                { return "http://loopback" }
func (*activityPlayback) Failover(context.Context, string, time.Duration) (*core.PlaySession, error) {
	return nil, core.ErrNotAvailable
}
func (p *activityPlayback) Stop(id string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.entries, id) }
func (p *activityPlayback) Sessions() []*core.PlaySession {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*core.PlaySession, 0, len(p.entries))
	for _, s := range p.entries {
		out = append(out, s)
	}
	return out
}

type activityProgress struct{}

func (activityProgress) Progress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) error {
	return nil
}

type activityTranscoder struct{}

func (activityTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return nil, core.ErrNotAvailable
}
func (activityTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionDirectPlay, Reason: "planned"}
}
func (activityTranscoder) StartHLS(context.Context, core.HLSRequest) (core.HLSSession, error) {
	return nil, core.ErrNotAvailable
}
func (activityTranscoder) Session(string) (core.HLSSession, bool) { return nil, false }
func (activityTranscoder) Stop(string)                            {}
func (activityTranscoder) HWAccel() core.HWAccel                  { return core.HWAccel{} }
func TestOwnerActivityIncludesAndStopsLegacyAndBrokerSessions(t *testing.T) {
	ctx := context.Background()
	p := &activityPlayback{entries: map[string]*core.PlaySession{}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	broker, err := sessions.NewBroker(sessions.BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: activityTranscoder{}, Progress: activityProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	plexSource, err := p.Prepare(ctx, &core.User{ID: 2}, core.MovieKey(1), core.ClientCaps{ClientID: "plex-tv", Protocol: "plex"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := sessions.PlanAdmission{Source: plexSource, Media: plexSource.Info, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}
	plexCtx := sessions.WithPlanAdmission(ctx, func(context.Context) (sessions.PlanAdmission, error) {
		return admitted, nil
	})
	plexViewer := &core.Viewer{ID: 2}
	plexDevice := core.Device{ID: "plex-tv"}
	plexPlan, err := broker.Plan(plexCtx, frontend.PlanRequest{Viewer: plexViewer, Device: plexDevice, Protocol: "plex", Item: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "plex-tv", Protocol: "plex"}, Intent: frontend.PlayIntent{ClientSessionID: "plex-1", Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if plexPlan.Decision.Reason != "planned" || plexPlan.Decision.Kind != core.DecisionDirectPlay {
		t.Fatalf("plan decision = %+v, want the transcoder decision", plexPlan.Decision)
	}
	if _, err = broker.Open(plexCtx, frontend.OpenRequest{Viewer: plexViewer, Device: plexDevice, Protocol: "plex", PlanID: plexPlan.ID}); err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	device := core.Device{ID: "jf-tv"}
	plan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(2), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	plexServer := plex.NewServer(plex.Deps{SessionStores: sessions.NewSessionStores(), Experience: experience.NewSelectors(), Playback: p, DisableEnrichment: true})
	defer plexServer.Close()
	a := &App{plexServer: plexServer, frontends: &frontendRuntime{broker: broker}}
	activity := (ownerPlayback{Playback: p, app: a}).Sessions()
	if len(activity) != 2 {
		t.Fatalf("activity has %d rows", len(activity))
	}
	protocols := map[core.Protocol]string{}
	for _, row := range activity {
		if protocols[row.Client.Protocol] != "" {
			t.Fatalf("duplicate protocol %q", row.Client.Protocol)
		}
		protocols[row.Client.Protocol] = row.ID
	}
	if protocols["plex"] != plexSource.ID || protocols["jellyfin"] == "" {
		t.Fatalf("protocol badges %+v", protocols)
	}
	for _, id := range []string{protocols["jellyfin"], protocols["plex"]} {
		if err := a.stopOwnerSession(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := (ownerPlayback{Playback: p, app: a}).Sessions(); len(got) != 0 {
		t.Fatalf("sessions remain: %+v", got)
	}
}
