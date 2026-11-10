package plex

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// This is a protocol lifecycle stress test, not a claim that simulated
// headers exercise the actual proprietary players or their decoders.
func TestResilienceMixedClientLifecycles(t *testing.T) {
	cycles := 20
	if n, err := strconv.Atoi(os.Getenv("ETHER_RESILIENCE_CYCLES")); err == nil && n > 0 && n <= 1000 {
		cycles = n
	}
	f := newFixture(func(d *testServerDeps) { d.Hooks = Hooks{}; d.DisableEnrichment = true })
	defer f.srv.Close()
	var requests atomic.Int64
	profiles := []struct{ product, platform, accept string }{
		{"Plex Web", "Chrome", "application/json"},
		{"Plex for Apple TV", "tvOS", "application/xml"},
		{"Plex for iOS", "iOS", "application/xml"},
		{"Plex for Android", "Android", "application/json"},
		{"Plex for Roku", "Roku", "application/xml"},
		{"Plex for Windows", "Windows", "application/json"},
	}
	t.Run("clients", func(t *testing.T) {
		for i, profile := range profiles {
			t.Run(profile.platform, func(t *testing.T) {
				t.Parallel()
				client := fmt.Sprintf("resilience-%d", i)
				token, other := ownerToken, friendToken
				if i%2 != 0 {
					token, other = friendToken, ownerToken
				}
				headers := map[string]string{"X-Plex-Client-Identifier": client, "X-Plex-Token": token,
					"X-Plex-Product": profile.product, "X-Plex-Platform": profile.platform, "Accept": profile.accept}
				get := func(path string) *httptest.ResponseRecorder {
					t.Helper()
					requests.Add(1)
					r := f.request(t, http.MethodGet, path, headers)
					if r.Code != 200 {
						t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
					}
					return r
				}
				for cycle := range cycles {
					get("/library/metadata/" + breakingBad.Key.String() + "?includeOnDeck=1&includeChildren=1")
					qheaders := map[string]string{"Accept": "application/json", "X-Plex-Token": token, "X-Plex-Client-Identifier": client}
					requests.Add(1)
					qrec := f.request(t, http.MethodPost, "/playQueues?type=video&continuous=1&uri="+url.QueryEscape("/library/metadata/"+bbE1.Key.String()), qheaders)
					if qrec.Code != 200 {
						t.Fatalf("queue create: %d", qrec.Code)
					}
					q := decodeJSON(t, qrec)
					qid := fmt.Sprint(int64(q["playQueueID"].(float64)))
					item := int64(q["Metadata"].([]any)[0].(map[string]any)["playQueueItemID"].(float64))
					get("/playQueues/" + qid)
					requests.Add(1)
					if r := f.request(t, http.MethodGet, "/playQueues/"+qid, map[string]string{"X-Plex-Token": other}); r.Code != 404 {
						t.Fatalf("queue crossed viewers: %d", r.Code)
					}
					sid := fmt.Sprintf("%s-%d", client, cycle)
					query := "path=" + url.QueryEscape("/library/metadata/"+bbE1.Key.String()) + "&session=" + sid + "&directPlay=0&directStream=1&protocol=hls"
					get("/video/:/transcode/universal/decision?" + query)
					get("/video/:/transcode/universal/start.m3u8?" + query)
					get("/video/:/transcode/universal/start.m3u8?" + query) // duplicate retry
					for _, step := range []struct {
						state string
						ms    int
					}{{"playing", 1000}, {"paused", 120000}, {"playing", 30000}, {"stopped", 45000}, {"stopped", 45000}} {
						get(fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=%d&duration=3600000&playQueueItemID=%d", bbE1.Key, step.state, step.ms, item))
					}
					if _, ok := f.trans.Session(sid); ok {
						t.Fatal("encoder session survives stop")
					}
				}
			})
		}
	})
	if n := len(f.playback.Sessions()); n != 0 {
		t.Fatalf("%d playback sessions leaked", n)
	}
	if n := len(f.srv.sessions.list()); n != 0 {
		t.Fatalf("%d client sessions leaked", n)
	}
	f.srv.queues.Expire(fixedNow.Add(7*time.Hour), 6*time.Hour)
	remaining := f.srv.queues.Len()
	if remaining != 0 {
		t.Fatalf("%d abandoned queues leaked", remaining)
	}
	if !t.Failed() {
		t.Logf("%d profiles, %d cycles each, %d HTTP requests; all sessions stopped and expired queues removed", len(profiles), cycles, requests.Load())
	}
}
