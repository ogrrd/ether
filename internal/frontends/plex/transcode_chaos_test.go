//go:build transcodechaos

package plex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var (
	armedMu sync.Mutex
	armed   = map[*fixture]map[string]bool{}
)

// Real Plex clients ask /decision before start.* and Ether only starts a
// session that a decision armed. The chaos suites predate that contract, so
// every fixture start.* request is preceded by the same client's decision.
func init() {
	requestHook = func(f *fixture, method, target string, hdr map[string]string) {
		i := strings.Index(target, "/video/:/transcode/universal/start")
		if method != http.MethodGet || i < 0 || hdr["Range"] != "" {
			return
		}
		q := ""
		if j := strings.IndexByte(target, '?'); j >= 0 {
			q = target[j:]
		}
		// One decision per player session, as a client asks it once; a
		// reconnect to the same session must not be re-armed.
		session := ""
		if u, err := url.Parse(target); err == nil {
			session = u.Query().Get("session")
		}
		armedMu.Lock()
		if armed[f] == nil {
			armed[f] = map[string]bool{}
		}
		done := armed[f][session]
		armed[f][session] = true
		armedMu.Unlock()
		if done {
			return
		}
		req := httptest.NewRequest(http.MethodGet, target[:i]+"/video/:/transcode/universal/decision"+q, nil)
		req.RemoteAddr = "192.168.1.50:51000"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if _, ok := hdr["X-Plex-Token"]; !ok && !strings.Contains(target, "X-Plex-Token") {
			req.Header.Set("X-Plex-Token", ownerToken)
		}
		if _, ok := hdr["X-Plex-Client-Identifier"]; !ok {
			req.Header.Set("X-Plex-Client-Identifier", "client-1")
		}
		f.srv.ServeHTTP(httptest.NewRecorder(), req)
	}
}

type chaosPlayback struct {
	*fakePlayback
	url string
}

func (p *chaosPlayback) InputURL(string) string { return p.url }

// TestTranscodeChaosPlex is explicitly opt-in: it uses the regular Plex
// fixture and actual handlers, a real transcode manager, and loopback media.
func TestTranscodeChaosPlex(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "fixture.mp4")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-movflags", "+faststart", input)
	source := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer source.Close()
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "transcodes")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	info, err := manager.Probe(context.Background(), source.URL+"/fixture.mp4")
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, data []byte, ext string) {
		t.Helper()
		if len(data) == 0 {
			t.Fatal("empty media")
		}
		path := filepath.Join(t.TempDir(), "output."+ext)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		chaosCmd(t, fp, "-v", "error", "-show_streams", path)
		chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	}
	for _, path := range []string{"hls-remux", "dash", "start-mkv"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.playback.info = *info
			f.srv.d.Playback = &chaosPlayback{f.playback, source.URL + "/fixture.mp4"}
			f.srv.d.Transcoder = manager
			id := "chaos-" + path
			query := "path=%2Flibrary%2Fmetadata%2F" + fightClub.Key.String() + "&session=" + id + "&directPlay=0&directStream=1"
			get := func(url string) []byte {
				t.Helper()
				rec := f.get(t, url)
				if rec.Code != 200 {
					t.Fatalf("%s: HTTP %d %s", url, rec.Code, rec.Body.String())
				}
				return rec.Body.Bytes()
			}
			switch path {
			case "hls-remux":
				playlist := get("/video/:/transcode/universal/start.m3u8?" + query + "&protocol=hls")
				if !strings.Contains(string(playlist), "#EXTM3U") {
					t.Fatal("missing HLS playlist")
				}
				check(t, get("/video/:/transcode/universal/session/"+id+"/base/00000.ts"), "ts")
			case "dash":
				manifest := get("/video/:/transcode/universal/start.mpd?" + query + "&protocol=dash")
				if !strings.Contains(string(manifest), "<MPD") {
					t.Fatal("missing MPD")
				}
				base := "/video/:/transcode/universal/session/" + id + "/0/"
				init := get(base + "header")
				media := get(base + "0.m4s")
				check(t, append(init, media...), "mp4")
			case "start-mkv":
				check(t, get("/video/:/transcode/universal/start.mkv?"+query+"&protocol=http"), "mkv")
			}
			get("/video/:/transcode/universal/ping?session=" + id)
			get("/video/:/transcode/universal/stop?session=" + id)
		})
	}
	t.Run("download", func(t *testing.T) {
		f := newDownloadFixture(t, true)
		f.playback.info = *info
		f.srv.d.Playback = &chaosPlayback{f.playback, source.URL + "/fixture.mp4"}
		dl, err := manager.NewDownloadRemuxer(config.Downloads{})
		if err != nil {
			t.Fatal(err)
		}
		f.srv.runtime.SetDownloads(dl)
		q := f.queue(t)
		rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(q)+"/add?keys=/library/metadata/"+fightClub.Key.String(), ownerToken, nil)
		if rec.Code != 200 {
			t.Fatalf("add HTTP %d", rec.Code)
		}
		item := f.waitStatus(t, q, downloadAvailable)
		id := int64(item["id"].(float64))
		rec = f.do(t, http.MethodGet, "/downloadQueue/"+itoa(q)+"/item/"+itoa(id)+"/media", ownerToken, nil)
		if rec.Code != 200 {
			t.Fatalf("media HTTP %d", rec.Code)
		}
		check(t, rec.Body.Bytes(), "mp4")
		rec = f.do(t, http.MethodDelete, "/downloadQueue/"+itoa(q)+"/items/"+itoa(id), ownerToken, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete HTTP %d", rec.Code)
		}
	})
}
func chaosCmd(t *testing.T, exe string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := mediaproc.CommandContext(ctx, 2*time.Minute, exe, args...).CombinedOutput()
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w: %s", filepath.Base(exe), err, b))
	}
}

// A damaged source may have concealed errors after a playable start. In that
// case do not equate -xerror with a player failure: require an independently
// decoded opening frame, then decode the whole output with ordinary tolerance.
func chaosDecodeMedia(t *testing.T, ff, fp, path string, damaged bool) {
	t.Helper()
	if !damaged {
		chaosCmd(t, ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "quiet", "-select_streams", "v:0", "-read_intervals", "%+#16", "-show_frames", "-show_entries", "frame=key_frame", "-of", "csv=p=0", path).Output()
	if err != nil || !strings.Contains(string(out), "1") {
		t.Fatal("no decodable opening keyframe")
	}
	chaosCmd(t, ff, "-v", "error", "-i", path, "-f", "null", "-")
}
