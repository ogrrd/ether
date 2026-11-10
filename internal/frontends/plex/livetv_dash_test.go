package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVirtualChannelDASHPlaysThroughPlexWeb(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// The broadcast goroutine reads the clock while the test advances it.
	var clock atomic.Int64
	clock.Store(time.Unix(7, 0).UnixNano())
	pb := newFakePlayback(core.MediaInfo{Duration: time.Hour, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720, BitrateKbps: 4000},
		{Index: 1, Type: "audio", Codec: "aac", BitrateKbps: 128},
	}})
	tc := &trackedTranscoder{fakeTranscoder: newFakeTranscoder(core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"})}
	service, err := livetv.New(livetv.Options{Transcoder: tc, Playback: pb, DB: db, Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	channel := livetv.Channel{Name: "Catalogue", Number: "4", Kind: "virtual", Enabled: true, Epoch: time.Unix(0, 0).UTC(), Items: []livetv.Item{
		{Key: core.MovieKey(1), Title: "First", DurationMS: 12000, Rating: "G"},
		{Key: core.MovieKey(2), Title: "Second", DurationMS: 12000, Rating: "PG"},
	}}
	if _, err = service.SaveChannel(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	f.srv.d.LiveTV = service.Frontend()
	f.srv.setTestSubscription(true)

	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/4/tune", map[string]string{"Accept": "application/json", "X-Plex-Product": "Plex Web", "X-Plex-Platform": "Chrome"})
	if tune.Code != 200 {
		t.Fatalf("tune %d %s", tune.Code, tune.Body.String())
	}
	metadata := decodeJSON(t, tune)["Metadata"].([]any)[0].(map[string]any)
	sessionPath := metadata["key"].(string)

	mpd := f.request(t, "GET", "/video/:/transcode/universal/start.mpd?path="+url.QueryEscape(sessionPath)+"&protocol=dash", map[string]string{"X-Plex-Product": "Plex Web"})
	if mpd.Code != 200 || !strings.Contains(mpd.Body.String(), `type="dynamic"`) || strings.Contains(mpd.Body.String(), "mediaPresentationDuration") {
		t.Fatalf("dash %d %s", mpd.Code, mpd.Body.String())
	}
	manifest := mpd.Body.String()
	for _, want := range []string{`minimumUpdatePeriod="PT2S"`, `timeShiftBufferDepth="PT1800S"`, `avc1.640028`, `mp4a.40.2`, `width="1280"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %s\n%s", want, manifest)
		}
	}

	attr := strings.Split(manifest, `media="`)
	if len(attr) < 2 {
		t.Fatalf("no media template:\n%s", manifest)
	}
	media := strings.SplitN(attr[1], `"`, 2)[0]
	media = strings.ReplaceAll(media, "$RepresentationID$", "0")
	media = strings.ReplaceAll(media, "$Number$", "0")
	seg := f.request(t, "GET", media, map[string]string{"X-Plex-Token": ""})
	if seg.Code != 200 || !strings.Contains(seg.Body.String(), "media-") {
		t.Fatalf("segment %d %q", seg.Code, seg.Body.String())
	}
	rolled := strings.Replace(media, "/0.m4s", "/2.m4s", 1)
	if !strings.Contains(rolled, "/2.m4s") {
		t.Fatalf("could not address the next programme from %s", media)
	}
	nextSeg := f.request(t, "GET", rolled, map[string]string{"X-Plex-Token": ""})
	if nextSeg.Code != 200 || nextSeg.Body.String() == seg.Body.String() {
		t.Fatalf("rollover segment %d %q (join %q)", nextSeg.Code, nextSeg.Body.String(), seg.Body.String())
	}

	clock.Store(time.Unix(40, 0).UnixNano())
	later := f.request(t, "GET", "/video/:/transcode/universal/start.mpd?path="+url.QueryEscape(sessionPath)+"&protocol=dash", nil)
	if later.Code != 200 || !strings.Contains(later.Body.String(), `type="dynamic"`) {
		t.Fatalf("manifest after the programme boundary: %d %s", later.Code, later.Body.String())
	}
	stop := f.request(t, "DELETE", sessionPath, nil)
	if stop.Code != 200 {
		t.Fatalf("stop %d %s", stop.Code, stop.Body.String())
	}
	for _, s := range tc.sessions {
		if !s.closed {
			t.Fatalf("transcoder %s left open", s.id)
		}
	}
	if len(service.ViewingSessions()) != 0 {
		t.Fatal("live session leaked")
	}
}

type trackedSession struct {
	id     string
	req    core.HLSRequest
	closed bool
}

func (s *trackedSession) ID() string                                       { return s.id }
func (s *trackedSession) LastAccess() time.Time                            { return time.Time{} }
func (s *trackedSession) Close() error                                     { s.closed = true; return nil }
func (s *trackedSession) ServePlaylist(http.ResponseWriter, *http.Request) {}
func (s *trackedSession) ServeSegment(http.ResponseWriter, *http.Request, int) {
}
func (s *trackedSession) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return nil
}
func (s *trackedSession) ServeManifest(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<MPD session=%q/>`, s.id)
}
func (s *trackedSession) ServeInit(w http.ResponseWriter, _ *http.Request, rep int) {
	fmt.Fprintf(w, "init-%s-%d", s.id, rep)
}
func (s *trackedSession) ServeMedia(w http.ResponseWriter, _ *http.Request, rep, n int) {
	fmt.Fprintf(w, "media-%s-%d-%d", s.id, rep, n)
}

type trackedTranscoder struct {
	*fakeTranscoder
	requests []core.HLSRequest
	sessions []*trackedSession
}

func (t *trackedTranscoder) StartHLS(_ context.Context, req core.HLSRequest) (core.HLSSession, error) {
	t.requests = append(t.requests, req)
	s := &trackedSession{id: req.SessionID, req: req}
	t.sessions = append(t.sessions, s)
	return s, nil
}
