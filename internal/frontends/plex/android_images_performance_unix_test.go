//go:build unix

package plex

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
)

// Opt-in, package-only handler measurement. Run identically at the before and
// after revisions. Every page has 50 distinct 300x450 posters and six workers.
func TestAndroidPosterPagePerformance(t *testing.T) {
	if os.Getenv("ETHER_ANDROID_POSTER_MEASURE") != "1" {
		t.Skip("opt-in performance measurement")
	}
	const posters, samples, workers = 50, 20, 6
	sources := make(map[string][]byte, posters)
	totalBytes := 0
	for i := 0; i < posters; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, 300, 450))
		seed := uint32(i + 1)
		for y := 0; y < 450; y++ {
			for x := 0; x < 300; x++ {
				seed = 1664525*seed + 1013904223
				n := int(seed >> 26)
				img.SetNRGBA(x, y, color.NRGBA{R: uint8((x + i*7 + n) % 256), G: uint8((y + i*13 + n) % 256), B: uint8((x/2 + y/2 + n) % 256), A: 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		sources[fmt.Sprintf("/poster-%d.png", i)] = buf.Bytes()
		totalBytes += buf.Len()
	}
	var fetches atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		data, ok := sources[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	defer upstream.Close()
	f := newFixture(func(d *testServerDeps) {
		d.ImageURL = func(path, size string) string { return upstream.URL + path }
		d.HTTPClient = upstream.Client()
	})
	defer f.srv.Close()
	targets := make([]string, posters)
	for i := 0; i < posters; i++ {
		title := fightClub
		title.Key = core.MovieKey(int64(900000 + i))
		title.PosterPath = fmt.Sprintf("/poster-%d.png", i)
		f.catalog.titles[title.Key] = title
		targets[i] = "/photo/:/transcode?width=300&height=450&minSize=1&upscale=1&format=jpeg&url=" + url.QueryEscape("/library/metadata/"+title.Key.String()+"/thumb/1")
	}
	page := func() {
		jobs := make(chan string, posters)
		for _, target := range targets {
			jobs <- target
		}
		close(jobs)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for target := range jobs {
					rec := f.get(t, target, "X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android")
					if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
						t.Errorf("image: %d %s", rec.Code, rec.Header())
					}
				}
			}()
		}
		wg.Wait()
	}
	cpu := func() time.Duration {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			t.Fatal(err)
		}
		return time.Duration(ru.Utime.Sec+ru.Stime.Sec)*time.Second + time.Duration(ru.Utime.Usec+ru.Stime.Usec)*time.Microsecond
	}
	for _, state := range []string{"cold", "warm"} {
		f.srv.images = artworkservice.NewRemoteCache(128<<20, upstream.Client())
		if state == "warm" {
			page()
		}
		fetchBefore := fetches.Load()
		var latencies []time.Duration
		var usedCPU time.Duration
		for i := 0; i < samples; i++ {
			if state == "cold" {
				f.srv.images = artworkservice.NewRemoteCache(128<<20, upstream.Client())
			}
			beforeCPU := cpu()
			start := time.Now()
			page()
			latencies = append(latencies, time.Since(start))
			usedCPU += cpu() - beforeCPU
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		fetchCount := fetches.Load() - fetchBefore
		if state == "warm" && fetchCount != 0 {
			t.Fatalf("warm upstream fetches: %d", fetchCount)
		}
		t.Logf("POSTER_PAGE state=%s posters=%d samples=%d workers=%d source_bytes=%d cpu_ms_per_image=%.4f page_p50_ms=%.3f page_p95_ms=%.3f upstream_fetches=%d", state, posters, samples, workers, totalBytes/50, float64(usedCPU)/float64(time.Millisecond*posters*samples), float64(latencies[samples/2])/float64(time.Millisecond), float64(latencies[(samples*95+99)/100-1])/float64(time.Millisecond), fetchCount)
	}
}
