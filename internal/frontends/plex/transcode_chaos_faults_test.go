//go:build transcodechaos

package plex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type plexChaosSource struct {
	data, changed []byte
	mode          string
	armed         atomic.Bool
	hits          atomic.Int64
}

type shortChaosWriter struct{ http.ResponseWriter }

func (w shortChaosWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), 4096)
		written, err := w.ResponseWriter.Write(p[:n])
		total += written
		p = p[written:]
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (s *plexChaosSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.armed.Load() {
		http.ServeContent(w, r, "media.mp4", time.Time{}, bytes.NewReader(s.data))
		return
	}
	n := s.hits.Add(1)
	if n == 1 {
		switch s.mode {
		case "503":
			http.Error(w, "injected error", 503)
			return
		case "416":
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(s.data)))
			http.Error(w, "injected error", 416)
			return
		case "slow":
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		case "stall", "drop", "mismatched-size", "changing":
			// Preserve real range status and headers, then interrupt the body.
			staged := httptest.NewRecorder()
			http.ServeContent(staged, r, "media.mp4", time.Time{}, bytes.NewReader(s.data))
			body := staged.Body.Bytes()
			cut := len(body) / 4
			if s.mode == "mismatched-size" {
				cut = len(body)
				staged.Header().Set("Content-Length", strconv.Itoa(len(body)+4096))
			}
			if s.mode == "stall" {
				for k, values := range staged.Header() {
					for _, v := range values {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(staged.Code)
				_, _ = w.Write(body[:cut])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-time.After(15 * time.Second):
				case <-r.Context().Done():
				}
				return
			}
			h, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported", 500)
				return
			}
			c, b, e := h.Hijack()
			if e != nil {
				return
			}
			defer c.Close()
			fmt.Fprintf(b, "HTTP/1.1 %d %s\r\n", staged.Code, http.StatusText(staged.Code))
			for k, values := range staged.Header() {
				for _, v := range values {
					fmt.Fprintf(b, "%s: %s\r\n", k, v)
				}
			}
			fmt.Fprint(b, "Connection: close\r\n\r\n")
			_, _ = b.Write(body[:cut])
			_ = b.Flush()
			return

		}
	}
	content := s.data
	if s.mode == "changing" && n > 1 {
		content = s.changed
	}
	if s.mode == "short-reads" {
		http.ServeContent(shortChaosWriter{w}, r, "media.mp4", time.Time{}, bytes.NewReader(content))
		return
	}
	http.ServeContent(w, r, "media.mp4", time.Time{}, bytes.NewReader(content))
}
func TestTranscodeChaosPlexFaults(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture.mp4")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-movflags", "+faststart", file)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"503", "416", "slow", "stall", "drop", "short-reads", "mismatched-size", "changing"} {
		t.Run(fault, func(t *testing.T) {
			for _, path := range []string{"hls", "dash", "start-mkv", "download"} {
				t.Run(path, func(t *testing.T) {
					changed := bytes.Clone(data)
					for i := len(changed) / 2; i < len(changed)/2+4096 && i < len(changed); i++ {
						changed[i] = 0xff
					}
					source := &plexChaosSource{data: data, changed: changed, mode: fault}
					server := httptest.NewServer(source)
					defer server.Close()
					m, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(t.TempDir(), "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(6*time.Second), transcode.WithStallTimeout(1500*time.Millisecond))
					if e != nil {
						t.Fatal(e)
					}
					defer m.Close()
					info, e := m.Probe(context.Background(), server.URL+"/fixture.mp4")
					if e != nil {
						t.Fatal(e)
					}
					source.armed.Store(true)
					check := func(b []byte, ext string) {
						t.Helper()
						if len(b) == 0 {
							t.Fatal("empty media")
						}
						p := filepath.Join(t.TempDir(), "out."+ext)
						if e := os.WriteFile(p, b, 0600); e != nil {
							t.Fatal(e)
						}
						chaosCmd(t, fp, "-v", "error", "-show_streams", p)
						chaosDecodeMedia(t, ff, fp, p, fault == "changing")
						if path == "start-mkv" || path == "download" {
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							out, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", p).Output()
							if err != nil {
								t.Fatal("cannot inspect output timeline")
							}
							maxTime := 0.0
							for _, line := range strings.Split(string(out), "\n") {
								v, e := strconv.ParseFloat(strings.TrimSpace(strings.Split(line, ",")[0]), 64)
								if e == nil && v > maxTime {
									maxTime = v
								}
							}
							if maxTime < 10 {
								t.Fatalf("output ended early at %.2fs", maxTime)
							}
						}
					}
					if path == "download" {
						f := newDownloadFixture(t, true)
						f.playback.info = *info
						f.srv.d.Playback = &chaosPlayback{f.playback, server.URL + "/fixture.mp4"}
						dl, e := m.NewDownloadRemuxer(config.Downloads{})
						if e != nil {
							t.Fatal(e)
						}
						f.srv.runtime.SetDownloads(dl)
						q := f.queue(t)
						rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(q)+"/add?keys=/library/metadata/"+fightClub.Key.String(), ownerToken, nil)
						if rec.Code != 200 {
							t.Fatal("download enqueue failed")
						}
						deadline := time.Now().Add(8 * time.Second)
						var item map[string]any
						for time.Now().Before(deadline) {
							items := f.items(t, q)
							if len(items) == 1 {
								item = items[0]
								if item["status"] == downloadAvailable {
									break
								}
								if item["status"] == downloadError {
									t.Fatal("conversion failed")
								}
							}
							time.Sleep(50 * time.Millisecond)
						}
						if item == nil || item["status"] != downloadAvailable {
							t.Fatal("conversion timed out")
						}
						id := int64(item["id"].(float64))
						rec = f.do(t, http.MethodGet, "/downloadQueue/"+itoa(q)+"/item/"+itoa(id)+"/media", ownerToken, nil)
						if rec.Code != 200 {
							t.Fatalf("media HTTP %d", rec.Code)
						}
						check(rec.Body.Bytes(), "mp4")
						f.do(t, http.MethodDelete, "/downloadQueue/"+itoa(q)+"/items/"+itoa(id), ownerToken, nil)
						if source.hits.Load() == 0 || (fault == "changing" && source.hits.Load() < 2) {
							t.Fatal("source fault was not reached")
						}
						return
					}
					f := newFixture()
					defer f.srv.Close()
					f.playback.info = *info
					f.srv.d.Playback = &chaosPlayback{f.playback, server.URL + "/fixture.mp4"}
					f.srv.d.Transcoder = m
					id := "fault-" + path
					query := "path=%2Flibrary%2Fmetadata%2F" + fightClub.Key.String() + "&session=" + id + "&directPlay=0&directStream=1"
					get := func(url string) []byte {
						t.Helper()
						requestHook(f, http.MethodGet, url, map[string]string{"X-Plex-Token": ownerToken, "X-Plex-Client-Identifier": "client-1"})
						ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						defer cancel()
						req := httptest.NewRequest(http.MethodGet, url, nil).WithContext(ctx)
						req.Header.Set("X-Plex-Token", ownerToken)
						req.Header.Set("X-Plex-Client-Identifier", "client-1")
						rec := httptest.NewRecorder()
						f.srv.ServeHTTP(rec, req)
						if ctx.Err() != nil {
							t.Fatal("client request exceeded its deadline")
						}
						if rec.Code != 200 {
							t.Fatalf("HTTP %d", rec.Code)
						}
						return rec.Body.Bytes()
					}
					defer f.get(t, "/video/:/transcode/universal/stop?session="+id)
					switch path {
					case "hls":
						get("/video/:/transcode/universal/start.m3u8?" + query + "&protocol=hls")
						check(get("/video/:/transcode/universal/session/"+id+"/base/00000.ts"), "ts")
					case "dash":
						get("/video/:/transcode/universal/start.mpd?" + query + "&protocol=dash")
						base := "/video/:/transcode/universal/session/" + id + "/0/"
						init := get(base + "header")
						check(append(init, get(base+"0.m4s")...), "mp4")
					case "start-mkv":
						check(get("/video/:/transcode/universal/start.mkv?"+query+"&protocol=http"), "mkv")
					}
					if source.hits.Load() == 0 || (fault == "changing" && source.hits.Load() < 2) {
						t.Fatal("source fault was not reached")
					}
				})
			}
		})
	}
}
