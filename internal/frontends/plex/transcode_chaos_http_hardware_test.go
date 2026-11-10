//go:build transcodechaos

package plex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type interruptingWriter struct {
	http.ResponseWriter
	once   sync.Once
	bytes  int
	runner *liveChaosRunner
	err    chan error
}

func (w *interruptingWriter) Write(p []byte) (int, error) {
	n, e := w.ResponseWriter.Write(p)
	w.bytes += n
	if w.bytes >= 32768 {
		w.once.Do(func() { go func() { w.err <- w.runner.interrupt(true) }() })
	}
	return n, e
}
func (w *interruptingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func TestTranscodeChaosHTTPHardware(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-movflags", "+faststart", input)
	source := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer source.Close()
	runner := &liveChaosRunner{}
	m, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "videotoolbox", TempDir: filepath.Join(dir, "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithRunner(runner))
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if m.HWAccel().Name != "videotoolbox" {
		t.Skip("VideoToolbox unavailable")
	}
	info, e := m.Probe(context.Background(), source.URL+"/input.mp4")
	if e != nil {
		t.Fatal(e)
	}
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = *info
	f.srv.d.Playback = &chaosPlayback{f.playback, source.URL + "/input.mp4"}
	f.srv.d.Transcoder = m
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A resume point under a minute is never stored from a timeline (the PMS
	// rule) and a timeline before any delivered segment is suppressed, so
	// seed the prior resume point the abort must leave alone.
	if e := watch.Progress(ctx, ownerUser.ID, fightClub.Key, time.Second, 0, "playing"); e != nil {
		t.Fatal(e)
	}
	// The player asks its decision first; timelines count only for an armed
	// decision, so the playhead below belongs to this playback.
	requestHook(f, http.MethodGet, "/video/:/transcode/universal/start.mkv?path=%2Flibrary%2Fmetadata%2F"+fightClub.Key.String()+"&session=http-hardware&protocol=http&directPlay=0&directStream=0", map[string]string{"X-Plex-Token": ownerToken, "X-Plex-Client-Identifier": "client-1"})
	// Keep the client's last actual playhead, rather than the encoder head.
	f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=playing&time=1000&duration=12000", "X-Plex-Client-Identifier", "client-1")
	injected := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" { // a ranged reconnect resumes the same start; a client asks no new decision
			requestHook(f, r.Method, r.URL.RequestURI(), map[string]string{"X-Plex-Token": ownerToken, "X-Plex-Client-Identifier": r.Header.Get("X-Plex-Client-Identifier")})
		}
		if r.URL.Query().Get("session") == "http-hardware" && r.Header.Get("Range") == "" {
			f.srv.ServeHTTP(&interruptingWriter{ResponseWriter: w, runner: runner, err: injected}, r)
			return
		}
		f.srv.ServeHTTP(w, r)
	}))
	defer server.Close()
	path := "/video/:/transcode/universal/start.mkv?path=%2Flibrary%2Fmetadata%2F" + fightClub.Key.String() + "&session=http-hardware&protocol=http&directPlay=0&directStream=0"
	req, e := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	resp, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	data, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("expected incomplete HTTP transfer, got %v", readErr)
	}
	select {
	case err := <-injected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("hardware failure was not injected (status=%d, bytes=%d)", resp.StatusCode, len(data))
	}
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	output := filepath.Join(dir, "result.mkv")
	if e := os.WriteFile(output, data, 0600); e != nil {
		t.Fatal(e)
	}
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", output, "-f", "null", "-")
	out, e := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", output).Output()
	if e != nil {
		t.Fatal(e)
	}
	last := 0.0
	for _, line := range strings.Split(string(out), "\n") {
		if v, e := strconv.ParseFloat(strings.TrimSpace(strings.Split(line, ",")[0]), 64); e == nil && v > last {
			last = v
		}
	}
	if last <= 0 || last >= 11 {
		t.Fatalf("failure did not end the first response early at %.2fs", last)
	}
	rangeReq, e := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
	if e != nil {
		t.Fatal(e)
	}
	rangeReq.Header.Set("X-Plex-Token", ownerToken)
	rangeReq.Header.Set("X-Plex-Client-Identifier", "client-1")
	rangeReq.Header.Set("Range", "bytes=32768-")
	rangeResp, e := server.Client().Do(rangeReq)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range reconnect HTTP %d", rangeResp.StatusCode)
	}
	reconnect, e := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
	if e != nil {
		t.Fatal(e)
	}
	reconnect.Header.Set("X-Plex-Token", ownerToken)
	reconnect.Header.Set("X-Plex-Client-Identifier", "client-1")
	reconnected, e := server.Client().Do(reconnect)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, reconnected.Body)
	reconnected.Body.Close()
	if reconnected.StatusCode != http.StatusBadGateway {
		t.Fatalf("same-session reconnect HTTP %d", reconnected.StatusCode)
	}
	// Some clients stop their encoder before their terminal timeline arrives.
	f.get(t, "/video/:/transcode/universal/stop?session=http-hardware", "X-Plex-Client-Identifier", "client-1")
	// An exaggerated terminal timeline must not turn the abort into watched.
	f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=stopped&time=12000&duration=12000", "X-Plex-Client-Identifier", "client-1")
	watchStatus, e := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if e != nil || watchStatus.ViewCount != 0 || watchStatus.ViewOffset != time.Second {
		t.Fatalf("abort changed watched state or resume point: %+v, %v", watchStatus, e)
	}
	// Simulate a client explicitly reopening at its last delivered position.
	// A new response can carry a fresh software codec-private header.
	retry := httptest.NewRequest("GET", "/video/:/transcode/universal/start.mkv?path=%2Flibrary%2Fmetadata%2F"+fightClub.Key.String()+"&session=http-hardware-retry&protocol=http&directPlay=0&directStream=0&offset="+strconv.FormatFloat(last, 'f', 3, 64), nil).WithContext(ctx)
	retry.Header.Set("X-Plex-Token", ownerToken)
	retry.Header.Set("X-Plex-Client-Identifier", "client-1")
	recovered := httptest.NewRecorder()
	requestHook(f, retry.Method, retry.URL.RequestURI(), map[string]string{"X-Plex-Token": ownerToken, "X-Plex-Client-Identifier": "client-1"})
	f.srv.ServeHTTP(recovered, retry)
	if recovered.Code != 200 || !runner.softwareStarted() {
		t.Fatalf("retry did not use software (HTTP %d)", recovered.Code)
	}
	if e := os.WriteFile(output, recovered.Body.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", output, "-f", "null", "-")
}
