package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type fakePlayback struct {
	mu                         sync.Mutex
	sessions                   map[string]*core.PlaySession
	limit, prepares, estimates int
}

func (f *fakePlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.estimates++
	return core.MediaInfo{Estimated: true, Container: "mp4"}
}
func (f *fakePlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if release != "" || exact {
		return nil, core.ErrNotAvailable
	}
	return f.Prepare(ctx, u, key, c)
}
func (f *fakePlayback) SessionForSurface(_ string, user int64, client string, key core.RatingKey, _ string, _ bool) (*core.PlaySession, bool) {
	return f.SessionFor(user, client, key)
}
func (f *fakePlayback) MarkSurfaceOpened(id string) bool   { _, ok := f.Session(id); return ok }
func (f *fakePlayback) MarkSurfacePrepared(id string) bool { _, ok := f.Session(id); return ok }
func (f *fakePlayback) Prepare(_ context.Context, u *core.User, key core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	for _, p := range f.sessions {
		if p.UserID == u.ID && p.ClientID == c.ClientID && p.Key == key {
			return p, nil
		}
	}
	if len(f.sessions) >= f.limit {
		return nil, core.ErrStreamLimit
	}
	id := fmt.Sprintf("source-%d", f.prepares)
	p := &core.PlaySession{ID: id, UserID: u.ID, ClientID: c.ClientID, Key: key, Info: core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Type: "subtitle", ExternalURL: "https://upstream.invalid/?token=account-credential-secret"}}}}
	f.sessions[id] = p
	return p, nil
}
func (f *fakePlayback) Session(id string) (*core.PlaySession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.sessions[id]
	return p, ok
}
func (f *fakePlayback) SessionFor(user int64, client string, key core.RatingKey) (*core.PlaySession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.sessions {
		if p.UserID == user && p.ClientID == client && p.Key == key {
			return p, true
		}
	}
	return nil, false
}
func (f *fakePlayback) ServeRange(w http.ResponseWriter, _ *http.Request, _ string) {
	_, _ = w.Write([]byte("source bytes"))
}
func (f *fakePlayback) InputURL(id string) string { return "http://loopback/" + id }
func (f *fakePlayback) Failover(context.Context, string, time.Duration) (*core.PlaySession, error) {
	return nil, core.ErrNotAvailable
}
func (f *fakePlayback) Stop(id string) { f.mu.Lock(); defer f.mu.Unlock(); delete(f.sessions, id) }
func (f *fakePlayback) Sessions() []*core.PlaySession {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*core.PlaySession, 0, len(f.sessions))
	for _, p := range f.sessions {
		out = append(out, p)
	}
	return out
}

type fakeBrokerTranscoder struct{}

func (fakeBrokerTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return nil, core.ErrNotAvailable
}
func (fakeBrokerTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mp4"}
}
func (fakeBrokerTranscoder) StartHLS(context.Context, core.HLSRequest) (core.HLSSession, error) {
	return nil, core.ErrNotAvailable
}
func (fakeBrokerTranscoder) Session(string) (core.HLSSession, bool) { return nil, false }
func (fakeBrokerTranscoder) Stop(string)                            {}
func (fakeBrokerTranscoder) HWAccel() core.HWAccel                  { return core.HWAccel{} }

type selectionTranscoder struct {
	fakeBrokerTranscoder
	audio, subtitle int
}

func (s *selectionTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, opts core.DecideOptions) core.Decision {
	s.audio, s.subtitle = opts.AudioStream, opts.SubtitleStream
	return core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mp4"}
}
func TestBrokerAppliesTrackSelectionsBeforeDecision(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	tc := &selectionTranscoder{}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, SelectTracks: func(_ context.Context, _ *core.Viewer, _ *core.Title, _ core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
		if !audioSet || !subtitleSet {
			t.Fatal("explicit flags lost")
		}
		opts.AudioStream = 12
		opts.SubtitleStream = -1
		return opts, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	_, err = b.Plan(ctx, frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "d"}, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{AudioSet: true, SubtitleSet: true, Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil || tc.audio != 12 || tc.subtitle != -1 {
		t.Fatalf("selected audio=%d subtitle=%d err=%v", tc.audio, tc.subtitle, err)
	}
}

func TestBrokerUsesSharedPlaybackCoordination(t *testing.T) {
	ctx := context.Background()
	coord := NewCoordination()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Coordination: coord, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	device := core.Device{ID: "device"}
	unlock, err := coord.Gates.Acquire(ctx, fmt.Sprintf("broker/owner/1/%s", clientKey("jellyfin", device)))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = b.Plan(limited, frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("broker bypassed shared gate: %v", err)
	}
	unlock()
	plexUnlock, err := coord.Gates.Acquire(ctx, fmt.Sprintf("owner/1/%s", clientKey("jellyfin", device)))
	if err != nil {
		t.Fatal(err)
	}
	defer plexUnlock()
	_, err = b.Plan(ctx, frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatalf("Plex client identifier blocked another protocol: %v", err)
	}
}
func TestBrokerMultiProtocolAdmissionAndMedia(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 5}
	progress := &fakeProgress{}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	bus := NewEventBus(nil, active)
	defer bus.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{Lookup: lookup, Playback: p, Transcoder: fakeBrokerTranscoder{}, Progress: progress, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	key := core.MovieKey(7)
	estimate, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: core.Device{ID: "detail"}, Protocol: "plex", Item: key, Intent: frontend.PlayIntent{Estimate: true}})
	if err != nil || !estimate.Estimated || p.prepares != 0 {
		t.Fatalf("estimate %+v prepare=%d err=%v", estimate, p.prepares, err)
	}
	var opened []frontend.Session
	var access []frontend.SessionAccess
	protocols := []frontend.Protocol{"plex", "plex", "jellyfin", "jellyfin", "jellyfin"}
	for i, protocol := range protocols {
		device := core.Device{ID: fmt.Sprintf("device-%d", i), Client: "account-credential-secret"}
		req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: protocol, Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
		plan, e := broker.Plan(ctx, req)
		if e != nil {
			t.Fatal(e)
		}
		if _, err := broker.Open(ctx, frontend.OpenRequest{Protocol: protocol, PlanID: plan.ID, Viewer: &core.Viewer{ID: 3}, Device: device}); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("foreign open: %v", err)
		}
		if len(plan.Media.Streams) > 0 && plan.Media.Streams[0].ExternalURL != "" {
			t.Fatal("upstream credential leaked in plan")
		}
		s, e := broker.Open(ctx, frontend.OpenRequest{Protocol: protocol, PlanID: plan.ID, Viewer: viewer, Device: device})
		if e != nil {
			t.Fatal(e)
		}
		a := frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: device.ID}, Protocol: protocol}
		if err := broker.Stop(ctx, frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Device: device, Protocol: protocol}, s.ID); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("foreign stop: %v", err)
		}
		path, e := broker.StartStream(ctx, a, s.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(path, device.Client) || strings.Contains(s.MediaPath, device.Client) {
			t.Fatal("account credential leaked in media path")
		}
		rec := httptest.NewRecorder()
		broker.ServeMedia(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || rec.Body.String() != "source bytes" {
			t.Fatalf("media: %d %q", rec.Code, rec.Body.String())
		}
		wrongSurface := httptest.NewRecorder()
		BindSurface(broker, "emby").ServeMedia(wrongSurface, httptest.NewRequest(http.MethodGet, path, nil))
		if wrongSurface.Code != http.StatusNotFound {
			t.Fatalf("cross-surface media: %d", wrongSurface.Code)
		}
		bad := httptest.NewRecorder()
		broker.ServeMedia(bad, httptest.NewRequest(http.MethodGet, strings.Replace(path, "/file", "x/file", 1), nil))
		if bad.Code != 404 {
			t.Fatalf("bad grant accepted: %d", bad.Code)
		}
		opened = append(opened, s)
		access = append(access, a)
	}
	if got, err := broker.Sessions(ctx, frontend.SessionAccess{Viewer: viewer}); err != nil || len(got) != 5 {
		t.Fatalf("sessions %d %v", len(got), err)
	}
	if err := broker.Report(ctx, access[0], opened[0].ID, frontend.Timeline{State: "paused", Position: time.Minute, Duration: time.Hour}); err != nil || progress.calls != 1 {
		t.Fatalf("report calls=%d err=%v", progress.calls, err)
	}
	fifthDevice := core.Device{ID: "sixth"}
	if _, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: fifthDevice, Protocol: "jellyfin", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("sixth plan error: %v", err)
	}
	if err := broker.Stop(ctx, access[0], opened[0].ID); err != nil {
		t.Fatal(err)
	}
	fifth, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: fifthDevice, Protocol: "jellyfin", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatalf("released admission: %v", err)
	}
	if _, err := broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: fifthDevice, Protocol: "jellyfin", PlanID: fifth.ID}); err != nil {
		t.Fatal(err)
	}
}

type fakeMediaSession struct{ id, protocol string }

func (s *fakeMediaSession) ID() string { return s.id }
func (s *fakeMediaSession) ServePlaylist(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("#EXTM3U\n00000.ts\n"))
}
func (s *fakeMediaSession) ServeSegment(w http.ResponseWriter, _ *http.Request, n int) {
	_, _ = fmt.Fprintf(w, "segment-%d", n)
}
func (s *fakeMediaSession) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return nil
}
func (s *fakeMediaSession) LastAccess() time.Time { return time.Now() }
func (s *fakeMediaSession) Close() error          { return nil }
func (s *fakeMediaSession) ServeManifest(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprintf(w, "session/%s/0/header", s.id)
}
func (s *fakeMediaSession) ServeInit(w http.ResponseWriter, _ *http.Request, rep int) {
	_, _ = fmt.Fprintf(w, "init-%d", rep)
}
func (s *fakeMediaSession) ServeMedia(w http.ResponseWriter, _ *http.Request, rep, n int) {
	_, _ = fmt.Fprintf(w, "dash-%d-%d", rep, n)
}

type fakeMediaTranscoder struct{ sessions map[string]*fakeMediaSession }

func (f *fakeMediaTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return nil, core.ErrNotAvailable
}
func (f *fakeMediaTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionDirectPlay, Container: "mp4"}
}
func (f *fakeMediaTranscoder) StartHLS(_ context.Context, r core.HLSRequest) (core.HLSSession, error) {
	s := &fakeMediaSession{id: r.SessionID, protocol: r.Protocol}
	f.sessions[r.SessionID] = s
	return s, nil
}
func (f *fakeMediaTranscoder) Session(id string) (core.HLSSession, bool) {
	s, ok := f.sessions[id]
	if !ok {
		return nil, false
	}
	return s, true
}
func (f *fakeMediaTranscoder) Stop(id string)        { delete(f.sessions, id) }
func (f *fakeMediaTranscoder) HWAccel() core.HWAccel { return core.HWAccel{} }
func TestBrokerSegmentedMedia(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	tc := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	broker, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	v := &core.Viewer{ID: 2}
	device := core.Device{ID: "tv"}
	plan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := broker.Open(ctx, frontend.OpenRequest{Viewer: v, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: v, Device: device, Protocol: "jellyfin"}
	hls, err := broker.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		t.Fatal(err)
	}
	first := tc.sessions[s.ID]
	again, err := broker.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil || again != hls || tc.sessions[s.ID] != first {
		t.Fatalf("repeated HLS start restarted encoder: %q %v", again, err)
	}
	fetch := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		broker.ServeMedia(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	if got := fetch(hls); !strings.Contains(got, "base/index.m3u8") {
		t.Fatalf("master %q", got)
	}
	baseHLS := strings.TrimSuffix(hls, "master.m3u8") + "base/"
	if got := fetch(baseHLS + "index.m3u8"); !strings.Contains(got, "00000.ts") {
		t.Fatalf("playlist %q", got)
	}
	if got := fetch(baseHLS + "00000.ts"); got != "segment-0" {
		t.Fatalf("segment %q", got)
	}
	dash, err := broker.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamDASH})
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch(dash); !strings.Contains(got, "session/"+s.ID) {
		t.Fatalf("manifest %q", got)
	}
	base := strings.TrimSuffix(dash, "manifest.mpd") + "session/" + s.ID + "/0/"
	if got := fetch(base + "header"); got != "init-0" {
		t.Fatalf("init %q", got)
	}
	if got := fetch(base + "0.m4s"); got != "dash-0-0" {
		t.Fatalf("DASH segment %q", got)
	}
}

type noTranscodeFixture struct{ fakeBrokerTranscoder }

func (noTranscodeFixture) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionTranscode}
}
func TestPlanRejectsForbiddenTranscoding(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: noTranscodeFixture{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	allowed := false
	_, err = b.Plan(ctx, frontend.PlanRequest{Viewer: &core.Viewer{ID: 2}, Device: core.Device{ID: "tv"}, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{AllowTranscoding: &allowed}})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("forbidden transcode error %v", err)
	}
}

func TestBrokerSupersededReportCannotStopReplacement(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	progress := &fakeProgress{}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: fakeBrokerTranscoder{}, Progress: progress, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	v := &core.Viewer{ID: 2}
	d := core.Device{ID: "tv"}
	a := frontend.SessionAccess{Viewer: v, Device: d, Protocol: "jellyfin"}
	open := func() frontend.Session {
		t.Helper()
		p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: d, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
		if err != nil {
			t.Fatal(err)
		}
		s, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: d, Protocol: "jellyfin", PlanID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, current := open(), open()
	listed, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: v})
	if err != nil || len(listed) != 1 || listed[0].ID != current.ID {
		t.Fatalf("superseded sessions remain visible: %+v %v", listed, err)
	}
	staleMedia := httptest.NewRecorder()
	b.ServeMedia(staleMedia, httptest.NewRequest(http.MethodGet, old.MediaPath, nil))
	if staleMedia.Code != http.StatusNotFound {
		t.Fatalf("superseded grant still served: %d", staleMedia.Code)
	}
	if err := b.Report(ctx, frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Device: d, Protocol: "jellyfin"}, old.ID, frontend.Timeline{State: "stopped"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("foreign retired report: %v", err)
	}
	if err := b.Report(ctx, a, current.ID, frontend.Timeline{PlaybackID: old.ID, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if progress.calls != 0 || len(p.Sessions()) != 1 {
		t.Fatal("stale identifier affected replacement")
	}
	if err := b.Report(ctx, a, old.ID, frontend.Timeline{PlaybackID: old.ID, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if progress.calls != 0 || len(p.Sessions()) != 1 {
		t.Fatal("superseded session stopped replacement")
	}
	if err := b.Report(ctx, a, current.ID, frontend.Timeline{PlaybackID: current.ID, State: "playing"}); err != nil || progress.calls != 1 {
		t.Fatalf("current report %v, progress %d", err, progress.calls)
	}
	if err := b.Report(ctx, a, current.ID, frontend.Timeline{PlaybackID: current.ID, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, a, current.ID); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
}

type fakeLadderSession struct{ fakeMediaSession }

func (s *fakeLadderSession) Rungs() []core.HLSRung {
	return []core.HLSRung{{ID: "base", Bandwidth: 2000000, Width: 1280, Height: 720}, {ID: "low", Bandwidth: 500000, Width: 640, Height: 360}}
}
func (s *fakeLadderSession) ServeRungPlaylist(w http.ResponseWriter, _ *http.Request, rung string) {
	_, _ = fmt.Fprintf(w, "#EXTM3U\n# %s\n00000.ts\n", rung)
}
func (s *fakeLadderSession) ServeRungSegment(w http.ResponseWriter, _ *http.Request, rung string, n int) {
	_, _ = fmt.Fprintf(w, "%s-%d", rung, n)
}

type fakeLadderTranscoder struct {
	fakeMediaTranscoder
	ladder *fakeLadderSession
}

func (f *fakeLadderTranscoder) StartHLS(_ context.Context, r core.HLSRequest) (core.HLSSession, error) {
	f.ladder = &fakeLadderSession{fakeMediaSession: fakeMediaSession{id: r.SessionID}}
	return f.ladder, nil
}
func (f *fakeLadderTranscoder) Session(id string) (core.HLSSession, bool) {
	if f.ladder != nil && f.ladder.id == id {
		return f.ladder, true
	}
	return nil, false
}
func (f *fakeLadderTranscoder) Stop(id string) {
	if f.ladder != nil && f.ladder.id == id {
		f.ladder = nil
	}
}
func TestBrokerHLSLadder(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
	tc := &fakeLadderTranscoder{}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	device := core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	path, err := b.StartStream(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.ServeMedia(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	master := get(path)
	if master.Code != 200 || !strings.Contains(master.Body.String(), "base/index.m3u8") || !strings.Contains(master.Body.String(), "low/index.m3u8") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	base := strings.TrimSuffix(path, "master.m3u8")
	if got := get(base + "low/index.m3u8"); got.Code != 200 || !strings.Contains(got.Body.String(), "# low") {
		t.Fatalf("low playlist %d %q", got.Code, got.Body.String())
	}
	if got := get(base + "low/00000.ts"); got.Code != 200 || got.Body.String() != "low-0" {
		t.Fatalf("low segment %d %q", got.Code, got.Body.String())
	}
	if got := get(base + "unknown/00000.ts"); got.Code != 404 {
		t.Fatalf("unknown rung %d", got.Code)
	}
	head := httptest.NewRecorder()
	b.ServeMedia(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD %d %q", head.Code, head.Body.String())
	}
}

func TestReportNotifiesTimelineOnceForPlexAndJellyfin(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}
	var mu sync.Mutex
	var calls []string
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(v *core.Viewer, key core.RatingKey, _ time.Duration, _ time.Duration, state string) {
		mu.Lock()
		calls = append(calls, fmt.Sprintf("%d %s %s", v.ID, key, state))
		mu.Unlock()
	}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	open := func(protocol frontend.Protocol, device, session string, item core.RatingKey) string {
		t.Helper()
		dev := core.Device{ID: device}
		plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: protocol, Item: item, Caps: core.ClientCaps{ClientID: device}, Intent: frontend.PlayIntent{ClientSessionID: session, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
		if err != nil {
			t.Fatal(err)
		}
		opened, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: protocol, PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err = b.Report(ctx, frontend.SessionAccess{Viewer: viewer, Device: dev, Protocol: protocol}, opened.ID, frontend.Timeline{Position: time.Minute, Duration: time.Hour, State: "playing"}); err != nil {
			t.Fatal(err)
		}
		return opened.ID
	}
	open("jellyfin", "jf", "jf-1", core.MovieKey(2))
	ps, err := p.Prepare(ctx, &core.User{ID: 2}, core.MovieKey(1), core.ClientCaps{ClientID: "plex-tv"})
	if err != nil {
		t.Fatal(err)
	}
	plexCtx := WithPlanAdmission(ctx, func(context.Context) (PlanAdmission, error) {
		return PlanAdmission{Source: ps, Media: ps.Info, Caps: core.ClientCaps{ClientID: "plex-tv"}, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}, nil
	})
	dev := core.Device{ID: "plex-tv"}
	plan, err := b.Plan(plexCtx, frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: "plex", Item: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "plex-tv"}, Intent: frontend.PlayIntent{ClientSessionID: "plex-1", Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Open(plexCtx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: "plex", PlanID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: dev, Protocol: "plex"}
	if err = b.Report(ctx, access, "plex-1", frontend.Timeline{Position: time.Minute, Duration: time.Hour, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{"2 " + core.MovieKey(2).String() + " playing", "2 " + core.MovieKey(1).String() + " playing"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("timeline notifications = %v, want %v", got, want)
	}
}

type livenessFakePlayback struct {
	fakePlayback
	active  map[string]bool
	stopped map[string]bool
}

func (f *livenessFakePlayback) SessionActive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped[id] {
		return false
	}
	if a, ok := f.active[id]; ok {
		return a
	}
	_, ok := f.sessions[id]
	return ok
}

func (f *livenessFakePlayback) PlaybackProgress(_ int64, clientID string, key core.RatingKey, _ time.Duration, state string) {
	if state != "stopped" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, p := range f.sessions {
		if p.ClientID == clientID && p.Key == key {
			f.stopped[id] = true
		}
	}
}

func TestBrokerReportItemProgressStoppedAndIdleSessionsVisibility(t *testing.T) {
	ctx := context.Background()
	p := &livenessFakePlayback{
		fakePlayback: fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4},
		active:       map[string]bool{},
		stopped:      map[string]bool{},
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: true}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
		Playback:     p,
		Transcoder:   fakeBrokerTranscoder{},
		Progress:     &fakeProgress{},
		ActiveViewer: active,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 1, IsOwner: true}
	dev := core.Device{ID: "infuse-appletv"}
	access := frontend.SessionAccess{Viewer: viewer, Device: dev, Protocol: "jellyfin", Owner: true}

	plan, err := b.Plan(ctx, frontend.PlanRequest{
		Viewer:   viewer,
		Device:   dev,
		Protocol: "jellyfin",
		Item:     core.MovieKey(42),
		Caps:     core.ClientCaps{ClientID: "infuse-appletv"},
		Intent:   frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := b.Sessions(ctx, access)
	if err != nil || len(listed) != 1 || listed[0].ID != opened.ID {
		t.Fatalf("expected 1 active session, got %+v (%v)", listed, err)
	}

	// Infuse sends POST /Sessions/Playing/Stopped with its own client-generated PlaySessionId,
	// which routes to ReportItemProgress(State: "stopped").
	if err := b.ReportItemProgress(ctx, access, core.MovieKey(42), frontend.Timeline{
		PlaybackID: "infuse-client-uuid",
		Position:   15 * time.Second,
		Duration:   time.Hour,
		State:      "stopped",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err = b.Sessions(ctx, access)
	if err != nil || len(listed) != 0 {
		t.Fatalf("expected 0 sessions after ReportItemProgress(stopped), got %+v (%v)", listed, err)
	}

	// Open a second session that goes idle without sending Stop; /Sessions hides it once playback reports inactive.
	dev2 := core.Device{ID: "infuse-idle"}
	plan2, err := b.Plan(ctx, frontend.PlanRequest{
		Viewer:   viewer,
		Device:   dev2,
		Protocol: "jellyfin",
		Item:     core.MovieKey(43),
		Caps:     core.ClientCaps{ClientID: "infuse-idle"},
		Intent:   frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: dev2, Protocol: "jellyfin", PlanID: plan2.ID}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for id := range p.sessions {
		p.active[id] = false
	}
	p.mu.Unlock()
	listed, err = b.Sessions(ctx, access)
	if err != nil || len(listed) != 0 {
		t.Fatalf("expected 0 sessions when playback session is idle, got %+v (%v)", listed, err)
	}
}
