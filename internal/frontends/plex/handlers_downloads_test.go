package plex

import (
	"context"
	"encoding/json"
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
	"github.com/braedonsaunders/ether/internal/core"
)

// fakeRemuxer writes a small temp file per download, like the real
// copy remux. block (when non-nil) parks the remux until closed or the
// context ends.
type fakeRemuxer struct {
	dir     string
	payload string
	err     error
	block   chan struct{}

	mu       sync.Mutex
	requests []core.DownloadRemuxRequest
	released []string
}

func (f *fakeRemuxer) RemuxDownload(ctx context.Context, req core.DownloadRemuxRequest, progress func(float64)) (string, int64, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	if f.err != nil {
		return "", 0, f.err
	}
	if progress != nil {
		progress(0.5)
	}
	p := filepath.Join(f.dir, "dl-fake.mp4")
	if err := os.WriteFile(p, []byte(f.payload), 0o644); err != nil {
		return "", 0, err
	}
	return p, int64(len(f.payload)), nil
}

func (f *fakeRemuxer) ReleaseDownload(p string) {
	f.mu.Lock()
	f.released = append(f.released, p)
	f.mu.Unlock()
	_ = os.Remove(p)
}

type downloadFixture struct {
	srv      *Server
	remux    *fakeRemuxer
	playback *fakePlayback
	auth     *fakeAuth
	now      time.Time
	mu       sync.Mutex
}

func newDownloadFixture(t *testing.T, downloadsOn bool) *downloadFixture {
	t.Helper()
	cat := &fakeCatalog{
		titles:   map[core.RatingKey]core.Title{},
		children: map[core.RatingKey][]core.RatingKey{bbS1.Key: {bbE1.Key, bbE2.Key}},
		facets:   map[string][]core.FacetValue{},
	}
	for _, title := range []core.Title{fightClub, alien, breakingBad, bbS1, bbE1, bbE2} {
		cat.titles[title.Key] = title
	}
	f := &downloadFixture{
		remux:    &fakeRemuxer{dir: t.TempDir(), payload: strings.Repeat("0123456789", 100)},
		playback: newFakePlayback(sampleInfo),
		auth:     &fakeAuth{users: map[string]core.User{ownerToken: ownerUser, friendToken: friendUser}},
		now:      fixedNow,
	}
	cfg := func() config.Config {
		c := config.Config{}
		c.Server.Port = 32400
		c.Server.FriendlyName = "Ether Test"
		c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
		c.Plex.Claimed = true
		c.Downloads = config.Downloads{
			Enabled: downloadsOn, MaxConcurrentPerUser: 1,
			Expiry: config.Duration(time.Hour), MaxHeight: 1080,
		}
		return c
	}
	d := Deps{
		Catalog:           cat,
		State:             newFakeState(),
		Playback:          f.playback,
		Downloads:         f.remux,
		DisableEnrichment: true,
	}
	setDepsAuth(&d, f.auth)
	setDepsConfig(&d, cfg)
	setDepsIdentity(&d, Identity{Version: "1.43.4.10903-e5521bd8c", Platform: "Linux"})
	setDepsNow(&d, func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	})
	f.srv = NewServer(testDeps(d))
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *downloadFixture) do(t *testing.T, method, target, token string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "192.168.1.50:51000"
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("X-Plex-Client-Identifier", "phone-1")
	req.Header.Set("X-Plex-Product", "Plex for iOS")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func decodeContainer(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	mc, _ := body["MediaContainer"].(map[string]any)
	if mc == nil {
		t.Fatalf("no MediaContainer in %s", rec.Body.String())
	}
	return mc
}

func (f *downloadFixture) queue(t *testing.T) int64 {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/downloadQueue", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create queue: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeContainer(t, rec)
	qs, _ := mc["DownloadQueue"].([]any)
	if len(qs) != 1 {
		t.Fatalf("DownloadQueue = %v", mc["DownloadQueue"])
	}
	return int64(qs[0].(map[string]any)["id"].(float64))
}

func (f *downloadFixture) items(t *testing.T, queueID int64) []map[string]any {
	t.Helper()
	rec := f.do(t, http.MethodGet, "/downloadQueue/"+itoa(queueID)+"/items", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("items: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeContainer(t, rec)
	raw, _ := mc["DownloadQueueItem"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(map[string]any))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// waitStatus polls until the single item reaches want (or fails).
func (f *downloadFixture) waitStatus(t *testing.T, queueID int64, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		items := f.items(t, queueID)
		if len(items) == 1 && items[0]["status"] == want {
			return items[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("items = %v, want status %q", items, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDownloadQueueLifecycle(t *testing.T) {
	f := newDownloadFixture(t, true)
	qid := f.queue(t)

	key := "/library/metadata/" + fightClub.Key.String()
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key+"&videoResolution=1280x720&maxVideoBitrate=4000", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeContainer(t, rec)
	added, _ := mc["AddedQueueItems"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["key"] != key {
		t.Fatalf("AddedQueueItems = %v", mc["AddedQueueItems"])
	}
	itemID := int64(added[0].(map[string]any)["id"].(float64))

	it := f.waitStatus(t, qid, downloadAvailable)
	if it["key"] != key {
		t.Fatalf("item key = %v", it["key"])
	}
	dr, _ := it["DecisionResult"].(map[string]any)
	if dr == nil || dr["generalDecisionCode"].(float64) != 1001 {
		t.Fatalf("DecisionResult = %v", it["DecisionResult"])
	}
	ts, _ := it["TranscodeSession"].(map[string]any)
	if ts == nil || ts["complete"] != true || ts["videoDecision"] != "copy" || ts["progress"].(float64) != 100 {
		t.Fatalf("TranscodeSession = %v", it["TranscodeSession"])
	}

	// The remux ran off a dedicated download playback session.
	f.remux.mu.Lock()
	reqs := f.remux.requests
	f.remux.mu.Unlock()
	if len(reqs) != 1 || !strings.Contains(reqs[0].InputURL, "phone-1/download") {
		t.Fatalf("remux requests = %+v", reqs)
	}
	if reqs[0].ViewerID == 0 || !reqs[0].Caps.Download {
		t.Fatalf("convert fields missing: %+v", reqs[0])
	}
	if reqs[0].Options.BurnSubtitles {
		t.Fatalf("default add burned subtitles: %+v", reqs[0].Options)
	}
	f.playback.mu.Lock()
	stopped := len(f.playback.stopped) > 0
	f.playback.mu.Unlock()
	if !stopped {
		t.Fatal("playback session not stopped after remux")
	}

	// Decision endpoint: conversion (copy) OK with the item's metadata.
	rec = f.do(t, http.MethodGet, "/downloadQueue/"+itoa(qid)+"/item/"+itoa(itemID)+"/decision", ownerToken, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1001") {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}

	// Media: full + ranged fetches of the exact bytes.
	media := "/downloadQueue/" + itoa(qid) + "/item/" + itoa(itemID) + "/media"
	rec = f.do(t, http.MethodGet, media, ownerToken, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != f.remux.payload {
		t.Fatalf("media: %d len=%d", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "Fjord Harbor.mp4") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	rec = f.do(t, http.MethodGet, media, ownerToken, map[string]string{"Range": "bytes=10-19"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != f.remux.payload[10:20] {
		t.Fatalf("range media: %d %q", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 10-19/") {
		t.Fatalf("Content-Range = %q", cr)
	}

	// Re-adding the same title with the same parameters dedupes.
	rec = f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key+"&videoResolution=1280x720&maxVideoBitrate=4000", ownerToken, nil)
	mc = decodeContainer(t, rec)
	added, _ = mc["AddedQueueItems"].([]any)
	if len(added) != 1 || int64(added[0].(map[string]any)["id"].(float64)) != itemID {
		t.Fatalf("dedupe add = %v", mc["AddedQueueItems"])
	}

	// Delete releases the temp file and the item is gone.
	rec = f.do(t, http.MethodDelete, "/downloadQueue/"+itoa(qid)+"/items/"+itoa(itemID), ownerToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := f.items(t, qid); len(got) != 0 {
		t.Fatalf("items after delete = %v", got)
	}
	f.remux.mu.Lock()
	released := f.remux.released
	f.remux.mu.Unlock()
	if len(released) != 1 {
		t.Fatalf("released = %v", released)
	}
	rec = f.do(t, http.MethodGet, media, ownerToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("media after delete: %d", rec.Code)
	}
}

func TestDownloadQueueBurnsWhenRequested(t *testing.T) {
	f := newDownloadFixture(t, true)
	qid := f.queue(t)
	key := "/library/metadata/" + fightClub.Key.String()
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key+"&subtitles=burn", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	f.waitStatus(t, qid, downloadAvailable)
	f.remux.mu.Lock()
	reqs := f.remux.requests
	f.remux.mu.Unlock()
	if len(reqs) != 1 || !reqs[0].Options.BurnSubtitles {
		t.Fatalf("burn request not passed: %+v", reqs)
	}
}

func TestDownloadQueueProcessing503AndSeasonExpand(t *testing.T) {
	f := newDownloadFixture(t, true)
	f.remux.block = make(chan struct{})
	qid := f.queue(t)

	// A season key expands to its episodes.
	key := "/library/metadata/" + bbS1.Key.String()
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key, ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add season: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeContainer(t, rec)
	if added, _ := mc["AddedQueueItems"].([]any); len(added) != 2 {
		t.Fatalf("season add = %v, want 2 episodes", mc["AddedQueueItems"])
	}

	// Media is 503 + Retry-After while the remux is parked; the second
	// episode waits behind the per-user cap of 1.
	items := f.items(t, qid)
	first := int64(items[0]["id"].(float64))
	rec = f.do(t, http.MethodGet, "/downloadQueue/"+itoa(qid)+"/item/"+itoa(first)+"/media", ownerToken, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "-1" {
		t.Fatalf("media while processing: %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Queue status reflects activity.
	rec = f.do(t, http.MethodGet, "/downloadQueue/"+itoa(qid), ownerToken, nil)
	mc = decodeContainer(t, rec)
	qs, _ := mc["DownloadQueue"].([]any)
	if st := qs[0].(map[string]any)["status"]; st != downloadDeciding && st != downloadProcessing && st != downloadWaiting {
		t.Fatalf("queue status = %v", st)
	}

	close(f.remux.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		items = f.items(t, qid)
		done := true
		for _, it := range items {
			if it["status"] != downloadAvailable {
				done = false
			}
		}
		if done && len(items) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("items never finished: %v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDownloadQueueErrorAndRestart(t *testing.T) {
	f := newDownloadFixture(t, true)
	f.remux.err = core.ErrDownloadNeedsTranscode
	qid := f.queue(t)
	key := "/library/metadata/" + fightClub.Key.String()
	f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key, ownerToken, nil)
	it := f.waitStatus(t, qid, downloadError)
	if it["error"] == "" || it["error"] == nil {
		t.Fatalf("error item = %v", it)
	}
	itemID := int64(it["id"].(float64))

	// Restart reprocesses; with the codec problem gone it completes.
	f.remux.err = nil
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/items/"+itoa(itemID)+"/restart", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart: %d", rec.Code)
	}
	f.waitStatus(t, qid, downloadAvailable)
}

func TestDownloadExpiry(t *testing.T) {
	f := newDownloadFixture(t, true)
	qid := f.queue(t)
	key := "/library/metadata/" + fightClub.Key.String()
	f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key, ownerToken, nil)
	it := f.waitStatus(t, qid, downloadAvailable)
	itemID := int64(it["id"].(float64))

	f.mu.Lock()
	f.now = f.now.Add(2 * time.Hour) // past the 1h retention
	f.mu.Unlock()
	f.waitStatus(t, qid, downloadExpired)
	rec := f.do(t, http.MethodGet, "/downloadQueue/"+itoa(qid)+"/item/"+itoa(itemID)+"/media", ownerToken, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("expired media: %d", rec.Code)
	}
	f.remux.mu.Lock()
	released := len(f.remux.released)
	f.remux.mu.Unlock()
	if released == 0 {
		t.Fatal("expired file not released")
	}
}

func TestDownloadsPermissionAndAllowSync(t *testing.T) {
	f := newDownloadFixture(t, true)

	// Owner sees allowSync on the root and sections; friend does not
	// (AllowFriends off) and cannot use the queue.
	rec := f.do(t, http.MethodGet, "/", ownerToken, nil)
	if mc := decodeContainer(t, rec); mc["allowSync"] != true {
		t.Fatalf("root allowSync = %v", mc["allowSync"])
	}
	if mc := decodeContainer(t, rec); mc["sync"] != true || mc["offlineTranscode"] != float64(1) {
		t.Fatalf("root download capabilities = sync:%v offlineTranscode:%v", mc["sync"], mc["offlineTranscode"])
	}
	rec = f.do(t, http.MethodGet, "/library/sections", ownerToken, nil)
	if !strings.Contains(rec.Body.String(), `"allowSync":true`) {
		t.Fatalf("sections missing allowSync true: %s", rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/library/sections", friendToken, nil)
	if strings.Contains(rec.Body.String(), `"allowSync":true`) {
		t.Fatalf("friend sees allowSync: %s", rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/downloadQueue", friendToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("friend create queue: %d", rec.Code)
	}
	// Item detail for the owner carries allowSync on the playable item.
	rec = f.do(t, http.MethodGet, "/library/metadata/"+fightClub.Key.String(), ownerToken, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"allowSync":true`) {
		t.Fatalf("metadata allowSync: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDownloadsDisabledKeepsAllowSyncOff(t *testing.T) {
	f := newDownloadFixture(t, false)
	rec := f.do(t, http.MethodGet, "/", ownerToken, nil)
	if mc := decodeContainer(t, rec); mc["allowSync"] != false {
		t.Fatalf("root allowSync = %v, want false", mc["allowSync"])
	}
	if mc := decodeContainer(t, rec); mc["sync"] != false || mc["offlineTranscode"] != float64(1) {
		t.Fatalf("disabled root download capabilities = sync:%v offlineTranscode:%v", mc["sync"], mc["offlineTranscode"])
	}
	rec = f.do(t, http.MethodPost, "/downloadQueue", ownerToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled create queue: %d", rec.Code)
	}
	rec = f.do(t, http.MethodGet, "/library/metadata/"+fightClub.Key.String(), ownerToken, nil)
	if strings.Contains(rec.Body.String(), `"allowSync":true`) {
		t.Fatalf("disabled metadata has allowSync: %s", rec.Body.String())
	}
}

func TestDownloadQueueIsPerClient(t *testing.T) {
	f := newDownloadFixture(t, true)
	qid := f.queue(t)
	// Another client of the same user does not see this queue.
	rec := f.do(t, http.MethodGet, "/downloadQueue/"+itoa(qid), ownerToken,
		map[string]string{"X-Plex-Client-Identifier": "tablet-9"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other client queue: %d", rec.Code)
	}
	// An unknown title is a clean 404 at add time.
	rec = f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys=/library/metadata/"+core.MovieKey(424242).String(), ownerToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("add unknown: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDownloadMediaRechecksTitleVisibility(t *testing.T) {
	f := newDownloadFixture(t, true)
	qid := f.queue(t)
	key := "/library/metadata/" + fightClub.Key.String()
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key, ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	it := f.waitStatus(t, qid, downloadAvailable)
	media := "/downloadQueue/" + itoa(qid) + "/item/" + itoa(int64(it["id"].(float64))) + "/media"
	if rec = f.do(t, http.MethodGet, media, ownerToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("media before restriction: %d", rec.Code)
	}
	// The viewer is restricted to PG-13 after the R-rated file was made.
	restricted := ownerUser
	restricted.MaxContentRating = "PG-13"
	f.auth.users[ownerToken] = restricted
	if rec = f.do(t, http.MethodGet, media, ownerToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("media after restriction: %d, want 404", rec.Code)
	}
}

type blockAllKeyword string
func (blockAllKeyword) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (b blockAllKeyword) Allowed(_ *core.User, t *core.Title) bool {
	for _, k := range t.Keywords {
		if k == string(b) {
			return false
		}
	}
	return true
}

// An episode of a show blocked by owner policy is not queued for download
// (episodes do not carry the show's keywords).
func TestDownloadQueueRejectsEpisodeOfBlockedShow(t *testing.T) {
	f := newDownloadFixture(t, true)
	f.srv.setTestPolicy(blockAllKeyword("blocked"))
	show := breakingBad
	show.Keywords = []string{"blocked"}
	f.srv.d.Catalog.(*fakeCatalog).titles[show.Key] = show
	qid := f.queue(t)
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+bbE1.Key.String(), ownerToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("add blocked episode: %d %s", rec.Code, rec.Body.String())
	}
	if items := f.items(t, qid); len(items) != 0 {
		t.Fatalf("blocked episode queued: %v", items)
	}
}

// A restart must not leave the new job on the old job's playback session:
// the old job's deferred Stop would cut the new remux's input.
func TestDownloadRestartGetsOwnPlaybackSession(t *testing.T) {
	f := newDownloadFixture(t, true)
	f.remux.block = make(chan struct{})
	qid := f.queue(t)
	key := "/library/metadata/" + fightClub.Key.String()
	f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?keys="+key, ownerToken, nil)
	reqCount := func() int {
		f.remux.mu.Lock()
		defer f.remux.mu.Unlock()
		return len(f.remux.requests)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("first remux", func() bool { return reqCount() == 1 })
	itemID := int64(f.items(t, qid)[0]["id"].(float64))
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/items/"+itoa(itemID)+"/restart", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart: %d", rec.Code)
	}
	waitFor("second remux", func() bool { return reqCount() == 2 })
	f.remux.mu.Lock()
	first := strings.TrimPrefix(f.remux.requests[0].InputURL, "http://127.0.0.1:1/source/")
	second := strings.TrimPrefix(f.remux.requests[1].InputURL, "http://127.0.0.1:1/source/")
	f.remux.mu.Unlock()
	if first == second {
		t.Fatalf("restarted job reuses playback session %q", first)
	}
	waitFor("old session stop", func() bool {
		f.playback.mu.Lock()
		defer f.playback.mu.Unlock()
		for _, id := range f.playback.stopped {
			if id == first {
				return true
			}
		}
		return false
	})
	if _, ok := f.playback.Session(second); !ok {
		t.Fatalf("new job's session %q was stopped by the old job", second)
	}
	close(f.remux.block)
	f.waitStatus(t, qid, downloadAvailable)
}
