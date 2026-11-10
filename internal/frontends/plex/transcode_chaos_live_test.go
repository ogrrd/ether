//go:build transcodechaos

package plex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type liveChaosWriter struct {
	mu  sync.Mutex
	dst io.Writer
}

func (w *liveChaosWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dst == nil {
		return len(p), nil
	}
	return w.dst.Write(p)
}

type liveChaosProcess struct {
	transcode.Process
	injected atomic.Bool
	stderr   *liveChaosWriter
}

func (p *liveChaosProcess) Err() error {
	if p.injected.Load() {
		return errors.New("injected hardware encoder failure")
	}
	return p.Process.Err()
}

type liveChaosRunner struct {
	mu       sync.Mutex
	p        *liveChaosProcess
	hardware bool
	software bool
}

func (r *liveChaosRunner) Run(ctx context.Context, c transcode.Command) error {
	return (transcode.ExecRunner{}).Run(ctx, c)
}
func (r *liveChaosRunner) Start(c transcode.Command) (transcode.Process, error) {
	hw, sw := false, false
	for _, a := range c.Args {
		if strings.Contains(a, "h264_videotoolbox") {
			hw = true
		}
		if a == "libx264" {
			sw = true
		}
	}
	for i, a := range c.Args {
		if a == "-i" {
			c.Args = append(c.Args[:i], append([]string{"-re"}, c.Args[i:]...)...)
			break
		}
	}
	w := &liveChaosWriter{dst: c.Stderr}
	c.Stderr = w
	p, e := (transcode.ExecRunner{}).Start(c)
	if e != nil {
		return nil, e
	}
	wrapped := &liveChaosProcess{Process: p, stderr: w}
	r.mu.Lock()
	r.p = wrapped
	r.hardware = r.hardware || hw
	r.software = r.software || sw
	r.mu.Unlock()
	return wrapped, nil
}
func (r *liveChaosRunner) interrupt(injectHardware bool) error {
	r.mu.Lock()
	p := r.p
	hw := r.hardware
	r.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no live encoder")
	}
	if injectHardware && !hw {
		return fmt.Errorf("hardware encoder was not started")
	}
	select {
	case <-p.Done():
		return fmt.Errorf("encoder ended before injection")
	default:
	}
	if injectHardware {
		p.injected.Store(true)
		fmt.Fprintln(p.stderr, "videotoolbox encoder injected failure")
	}
	p.Process.Stop()
	return nil
}
func (r *liveChaosRunner) softwareStarted() bool  { r.mu.Lock(); defer r.mu.Unlock(); return r.software }
func TestTranscodeChaosLiveRelay(t *testing.T)    { runLiveChaos(t, false, false, false) }
func TestTranscodeChaosLiveHardware(t *testing.T) { runLiveChaos(t, true, false, false) }
func TestTranscodeChaosAppleRelay(t *testing.T)   { runLiveChaos(t, false, true, false) }
func TestTranscodeChaosDASHIFRelay(t *testing.T)  { runLiveChaos(t, false, false, true) }
func runLiveChaos(t *testing.T, hardware, apple, dashif bool) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "live.ts")
	if apple {
		sample := filepath.Join(os.Getenv("CHAOS_CORPUS"), "corpus-apple-bipbop.ts.media")
		data, err := os.ReadFile(sample)
		if err != nil {
			t.Skip("Apple public sample not available in corpus")
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		playlist := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:11\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:9.9766,\n#EXT-X-BYTERANGE:326744@0\nlive.ts\n#EXTINF:9.9766,\n#EXT-X-BYTERANGE:326368@326744\nlive.ts\n#EXTINF:10.01,\n#EXT-X-BYTERANGE:327120@653112\nlive.ts\n#EXT-X-ENDLIST\n"
		if err := os.WriteFile(filepath.Join(dir, "live.m3u8"), []byte(playlist), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "15", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-f", "mpegts", file)
	}
	if dashif {
		chaosDASHIF(t, dir)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) / 2 / 188 * 188; !apple && !dashif && i < len(data)/2/188*188+30*188 && i < len(data); i++ {
		if i%188 != 0 {
			data[i] = 0xff
		}
	}
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer upstream.Close()
	runner := &liveChaosRunner{}
	hw := "none"
	if hardware {
		hw = "videotoolbox"
	}
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: hw, TempDir: filepath.Join(dir, "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithRunner(runner), transcode.WithSegmentTimeout(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if hardware && m.HWAccel().Name != "videotoolbox" {
		t.Skip("VideoToolbox unavailable")
	}
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := livetv.New(livetv.Options{Transcoder: m, DB: db, Now: func() time.Time { return fixedNow }, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	input := "/live.ts"
	if dashif {
		input = "/live.mpd"
	}
	if apple {
		input = "/live.m3u8"
	}
	_, err = service.ImportSource(context.Background(), livetv.Source{Name: "Fixture", Playlist: fmt.Sprintf("#EXTM3U\n#EXTINF:-1 tvg-id=\"fixture\" tvg-chno=\"7\",Fixture\n%s%s\n", upstream.URL, input)})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := service.Channels(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatal("missing fixture channel")
	}
	channel := channels[0]
	channel.Transcode = true
	if _, err = service.SaveChannel(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.LiveTV = service.Frontend()
	f.srv.setTestSubscription(true)
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/"+channel.ID+"/tune", map[string]string{"Accept": "application/json"})
	if tune.Code != 200 {
		t.Fatalf("tune HTTP %d", tune.Code)
	}
	metadata := decodeJSON(t, tune)["Metadata"].([]any)[0].(map[string]any)
	session := metadata["key"].(string)
	playlist := f.get(t, session+"/0/index.m3u8")
	if playlist.Code != 200 {
		t.Fatalf("playlist HTTP %d", playlist.Code)
	}
	var media string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.Contains(line, "/relay/0.ts") {
			media = line
			break
		}
	}
	if media == "" {
		t.Fatal("first live relay segment missing")
	}
	first := f.get(t, media)
	if first.Code != 200 || first.Body.Len() == 0 {
		t.Fatalf("first live segment HTTP %d", first.Code)
	}
	path := filepath.Join(dir, "first.ts")
	if err = os.WriteFile(path, first.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	chaosCmd(t, fp, "-v", "error", "-show_streams", path)
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	if err = runner.interrupt(hardware); err != nil {
		t.Fatal(err)
	}
	// A watcher requests the next segment after the interruption. Its URI uses
	// the same relay authorization as the first and is never printed or logged.
	next := strings.Replace(media, "/relay/0.ts", "/relay/1.ts", 1)
	r := f.get(t, next)
	if r.Code != 200 || r.Body.Len() == 0 {
		t.Fatalf("live continuation HTTP %d", r.Code)
	}
	path = filepath.Join(dir, "next.ts")
	if err = os.WriteFile(path, r.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	chaosCmd(t, fp, "-v", "error", "-show_streams", path)
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	if hardware && !runner.softwareStarted() {
		t.Fatal("live recovery restarted hardware instead of falling back to software")
	}
	updated := f.get(t, session+"/0/index.m3u8")
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), "/relay/1.ts") || !strings.Contains(updated.Body.String(), "#EXT-X-DISCONTINUITY") {
		t.Fatal("live playlist did not advance with a discontinuity")
	}
}
