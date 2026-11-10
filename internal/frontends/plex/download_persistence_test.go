package plex

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDownloadQueueSurvivesRestart(t *testing.T) {
	f := newDownloadFixture(t, true)
	deps := f.srv.d
	f.srv.Close()
	deps.DB = store.OpenTest(t)
	f.srv = NewServer(testDeps(deps))
	qid := f.queue(t)
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?key="+fightClub.Key.String(), ownerToken, nil)
	if rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	it := f.waitStatus(t, qid, downloadAvailable)
	id := int64(it["id"].(float64))
	f.srv.Close()
	f.srv = NewServer(testDeps(deps))
	if got := f.queue(t); got != qid {
		t.Fatalf("queue changed: %d => %d", qid, got)
	}
	items := f.items(t, qid)
	if len(items) != 1 || items[0]["status"] != downloadAvailable || int64(items[0]["id"].(float64)) != id {
		t.Fatalf("restored items: %v", items)
	}
	path := "/downloadQueue/" + itoa(qid) + "/item/" + itoa(id) + "/media"
	rec = f.do(t, http.MethodGet, path, ownerToken, map[string]string{"Range": "bytes=10-19"})
	if rec.Code != 206 || rec.Body.String() != f.remux.payload[10:20] {
		t.Fatalf("media: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodGet, path, friendToken, nil); rec.Code != 403 {
		t.Fatalf("foreign: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodGet, path, ownerToken, map[string]string{"X-Plex-Client-Identifier": "other"}); rec.Code != 404 {
		t.Fatalf("foreign device: %d", rec.Code)
	}
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Hour)
	f.mu.Unlock()
	if rec := f.do(t, http.MethodGet, path, ownerToken, nil); rec.Code != 410 {
		t.Fatalf("expiry after restart: %d", rec.Code)
	}
}

func TestLegacySyncDispatch(t *testing.T) {
	f := newDownloadFixture(t, false)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := f.do(t, method, "/sync", ownerToken, nil)
		if rec.Code != 200 || decodeContainer(t, rec)["content"] != "plugins" {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/sync/items", "/sync/transcodeQueue", "/sync/zzz"} {
		if rec := f.do(t, http.MethodGet, path, ownerToken, nil); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
}

func TestDownloadFilenameDisposition(t *testing.T) {
	got := contentDisposition("a+b.c-~\r\n\".mkv")
	if !strings.Contains(got, "a%2Bb%2Ec-~%22%2Emkv") || !strings.Contains(got, `filename="a_b.c-~_.mkv"`) || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("disposition: %q", got)
	}
}

func TestDownloadQueuePreauthReadRefusal(t *testing.T) {
	f := newDownloadFixture(t, true)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rec := f.do(t, method, "/downloadQueue", "", nil); rec.Code != 404 {
			t.Fatalf("%s: %d", method, rec.Code)
		}
	}
	if rec := f.do(t, http.MethodPost, "/downloadQueue", "", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated create: %d", rec.Code)
	}
}

func TestOfflineTranscodeFollowsClaim(t *testing.T) {
	f := newDownloadFixture(t, false)
	deps := f.srv.d
	cfg := depsConfig(&deps)()
	cfg.Plex.Claimed = false
	cfg.Plex.Token = ""
	setDepsConfig(&deps, func() config.Config { return cfg })
	f.srv.Close()
	f.srv = NewServer(testDeps(deps))
	rec := f.do(t, http.MethodGet, "/", ownerToken, nil)
	if _, ok := decodeContainer(t, rec)["offlineTranscode"]; ok {
		t.Fatalf("unclaimed offlineTranscode: %s", rec.Body.String())
	}
}

func TestDownloadPendingRestartsWithFreshRun(t *testing.T) {
	f := newDownloadFixture(t, true)
	deps := f.srv.d
	f.srv.Close()
	deps.DB = store.OpenTest(t)
	f.remux.block = make(chan struct{})
	f.srv = NewServer(testDeps(deps))
	qid := f.queue(t)
	rec := f.do(t, http.MethodPost, "/downloadQueue/"+itoa(qid)+"/add?key="+fightClub.Key.String()+"&maxVideoBitrate=4000", ownerToken, nil)
	if rec.Code != 200 {
		t.Fatalf("add: %d", rec.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.remux.mu.Lock()
		n := len(f.remux.requests)
		f.remux.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first run never started")
		}
		time.Sleep(time.Millisecond)
	}
	f.srv.Close()
	close(f.remux.block)
	f.srv = NewServer(testDeps(deps))
	f.waitStatus(t, qid, downloadAvailable)
	f.remux.mu.Lock()
	reqs := append([]core.DownloadRemuxRequest(nil), f.remux.requests...)
	f.remux.mu.Unlock()
	if len(reqs) != 2 || reqs[0].InputURL == reqs[1].InputURL || reqs[1].Caps.MaxBitrateKbps != 4000 {
		t.Fatalf("resume requests: %+v", reqs)
	}
}

func TestClaimedOwnerLibraryRowSyncWithoutDownloads(t *testing.T) {
	f := newDownloadFixture(t, false)
	for _, token := range []string{ownerToken, friendToken} {
		rec := f.do(t, http.MethodGet, "/library/sections", token, nil)
		rows, _ := decodeContainer(t, rec)["Directory"].([]any)
		if len(rows) == 0 {
			t.Fatalf("sections: %s", rec.Body.String())
		}
		for _, row := range rows {
			if got := row.(map[string]any)["allowSync"]; got != (token == ownerToken) {
				t.Fatalf("%s row allowSync=%v", token, got)
			}
		}
	}
}
