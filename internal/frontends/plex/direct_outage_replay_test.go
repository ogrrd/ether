package plex

import (
	"bytes"
	"sync/atomic"

	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"testing"
	"time"
)

// Exercise the actual Plex part handler after its playback generation moves.
func TestPartRangeAfterFailover(t *testing.T) {
	f, m := issue94Fixture(t, true, "open")
	path := "/library/parts/1000000000000550/1759276800/file.mkv"
	headers := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "play-id", "Range", "bytes=0-9"}
	if r := f.get(t, path, headers...); r.Code != 206 {
		t.Fatalf("initial part status %d", r.Code)
	}
	ps, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
	if !ok {
		t.Fatal("missing session")
	}
	original := ps.Resolution.Release.ID
	if _, err := m.Failover(context.Background(), ps.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	ps, ok = m.Session(ps.ID)
	if !ok || ps.Resolution.Release.ID == original {
		t.Fatal("no replacement generation")
	}
	r := f.get(t, path, headers...)
	// A same-size replacement still has different container bytes.
	if r.Code != 409 {
		t.Fatalf("replacement part status=%d length=%d", r.Code, r.Body.Len())
	}
}

type partOutageSource struct {
	*bytes.Reader
	id      string
	blocked *atomic.Bool
}

func (s *partOutageSource) Size() int64           { return int64(s.Len()) }
func (s *partOutageSource) Name() string          { return s.id + ".mkv" }
func (s *partOutageSource) ReleaseID() string     { return s.id }
func (s *partOutageSource) Close() error          { return nil }
func (s *partOutageSource) RetryDirectPlay() bool { return true }
func (s *partOutageSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if s.id == "hd" && s.blocked.Load() {
		return 0, core.ErrSourceUnavailable
	}
	return s.Reader.ReadAt(p, off)
}

type partOutageEngine struct{ blocked atomic.Bool }

func (e *partOutageEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	size := 65536
	if id == "sd" {
		size = 80000
	}
	return &partOutageSource{bytes.NewReader(bytes.Repeat([]byte("x"), size)), id, &e.blocked}, nil
}
func (*partOutageEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

func TestPartRangeOutageEndsInRestart(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	cfg.Quality.Default = "test"
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "test", MaxHeight: 1080}}
	cfg.Quality.Rules = nil
	cfg.Quality.Remote = ""
	releases := []core.Release{
		{ID: "hd", GUID: "hd", Indexer: "offline", Name: "Fjord.Harbor.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, PubDate: time.Now().Add(-24 * time.Hour), NZBURL: "offline"},
		{ID: "sd", GUID: "sd", Indexer: "offline", Name: "Fjord.Harbor.1999.720p.WEB-DL.H264-GRP", Size: 2000 << 20, PubDate: time.Now().Add(-24 * time.Hour), NZBURL: "offline"},
	}
	rs := resolver.New(&issue94Indexer{releases}, nil, nil, cfg, resolver.Options{})
	defer rs.Close()
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true; d.Hooks = Hooks{} })
	tr := &issue94Transcoder{newFakeTranscoder(core.Decision{})}
	e := &partOutageEngine{}
	mgr, err := playback.New(playback.Deps{Catalog: f.catalog, Resolver: rs, Engine: e, Transcoder: tr, SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return rs.ProfileFor(c, u) }, NativeProbe: func(_ context.Context, _ core.MediaSource) (*core.MediaInfo, error) {
		info := sampleInfo
		return &info, nil
	}}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.d.Playback = mgr
	f.srv.d.Transcoder = tr
	defer f.srv.Close()
	defer mgr.Close()
	path := "/library/parts/1000000000000550/1759276800/file.mkv"
	headers := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "play-id", "Range", "bytes=0-9"}
	if r := f.get(t, path, headers...); r.Code != 206 {
		t.Fatalf("initial status %d", r.Code)
	}
	ps, ok := mgr.SessionFor(ownerUser.ID, "phone", fightClub.Key)
	if !ok || ps.Resolution.Release.ID != "hd" {
		t.Fatal("no initial source")
	}
	e.blocked.Store(true)
	headers[len(headers)-1] = "bytes=32000-32009"
	if r := f.get(t, path, headers...); r.Code != 503 {
		t.Fatalf("outage status %d", r.Code)
	}
	// Bounded recovery gets one grace window on the original release before
	// it restarts (25s production timer per window; see
	// internal/playback/stall_edge_recovery_test.go), so the remux restart
	// lands after the second window.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := mgr.Session(ps.ID)
		if ok && state.RequiresRemux {
			r := f.get(t, path, headers...)
			if r.Code != 409 {
				t.Fatalf("restart status %d", r.Code)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("part remained on unavailable source")
}
