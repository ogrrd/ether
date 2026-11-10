package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// libraryProvider returns the com.plexapp.plugins.library provider and its
// container from /media/providers.
func libraryProvider(t *testing.T, rec *httptest.ResponseRecorder) (map[string]any, map[string]any) {
	t.Helper()
	mc := decodeContainer(t, rec)
	providers, _ := mc["MediaProvider"].([]any)
	for _, p := range providers {
		if m := p.(map[string]any); m["identifier"] == model.LibraryIdentifier {
			return mc, m
		}
	}
	t.Fatalf("no library provider in %v", mc)
	return nil, nil
}

func hasDownloadSubscribe(provider map[string]any) bool {
	features, _ := provider["Feature"].([]any)
	for _, f := range features {
		if m := f.(map[string]any); m["type"] == "subscribe" && m["flavor"] == "download" {
			return true
		}
	}
	return false
}

// Plex Web's showSyncDownload (Plex for Mac/Windows/HTPC) needs the library
// provider's subscribe feature with flavor "download", server.allowSync, and
// a guid on the item. All three follow the viewer's download permission.
func TestDesktopDownloadGate(t *testing.T) {
	desktop := map[string]string{"X-Plex-Product": "Plex for Mac", "X-Plex-Client-Identifier": "mac-1"}

	f := newDownloadFixture(t, true)
	mc, lib := libraryProvider(t, f.do(t, http.MethodGet, "/media/providers", ownerToken, desktop))
	if !hasDownloadSubscribe(lib) || mc["allowSync"] != true {
		t.Fatalf("owner: subscribe=%v allowSync=%v", hasDownloadSubscribe(lib), mc["allowSync"])
	}
	if root := decodeContainer(t, f.do(t, http.MethodGet, "/", ownerToken, desktop)); root["allowSync"] != true {
		t.Fatalf("owner root allowSync = %v", root["allowSync"])
	}
	md := decodeContainer(t, f.do(t, http.MethodGet, "/library/metadata/"+fightClub.Key.String(), ownerToken, desktop))
	if item := md["Metadata"].([]any)[0].(map[string]any); item["guid"] == nil || item["remoteMedia"] != nil {
		t.Fatalf("item guid=%v remoteMedia=%v", item["guid"], item["remoteMedia"])
	}

	// A friend without download permission sees neither.
	mc, lib = libraryProvider(t, f.do(t, http.MethodGet, "/media/providers", friendToken, desktop))
	if hasDownloadSubscribe(lib) || mc["allowSync"] != false {
		t.Fatalf("friend: subscribe=%v allowSync=%v", hasDownloadSubscribe(lib), mc["allowSync"])
	}
	if root := decodeContainer(t, f.do(t, http.MethodGet, "/", friendToken, desktop)); root["allowSync"] != false {
		t.Fatalf("friend root allowSync = %v", root["allowSync"])
	}

	// Downloads off: no feature for anyone.
	off := newDownloadFixture(t, false)
	mc, lib = libraryProvider(t, off.do(t, http.MethodGet, "/media/providers", ownerToken, desktop))
	if hasDownloadSubscribe(lib) || mc["allowSync"] != false {
		t.Fatalf("disabled: subscribe=%v allowSync=%v", hasDownloadSubscribe(lib), mc["allowSync"])
	}
}

// /transcode/sessions/{id} is the key every TranscodeSession advertises; a
// pulling server polls it. Viewers see only their own sessions.
func TestTranscodeSessionStatus(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.transcodes.put(&transcodeEntry{id: "pull-1", userID: friendUser.ID, clientID: "embedded-1", key: fightClub.Key, info: sampleInfo, updatedAt: fixedNow})

	for _, tc := range []struct {
		token string
		want  int
	}{{friendToken, http.StatusOK}, {ownerToken, http.StatusOK}} {
		rec := f.get(t, "/transcode/sessions/pull-1", "X-Plex-Token", tc.token, "Accept", "application/json")
		if rec.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.token, rec.Code, rec.Body.String())
		}
		sess := decodeContainer(t, rec)["TranscodeSession"].([]any)[0].(map[string]any)
		if sess["key"] != "/transcode/sessions/pull-1" {
			t.Fatalf("session = %v", sess)
		}
	}
	f.srv.transcodes.put(&transcodeEntry{id: "own-1", userID: ownerUser.ID, clientID: "c", key: fightClub.Key, info: sampleInfo, updatedAt: fixedNow})
	if rec := f.get(t, "/transcode/sessions/own-1", "X-Plex-Token", friendToken); rec.Code != http.StatusNotFound {
		t.Fatalf("friend read owner's session: %d", rec.Code)
	}
	if rec := f.get(t, "/transcode/sessions/missing"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing session: %d", rec.Code)
	}
}

// slowChecker answers each key with its verdict (missing: live) or error (no
// verdict); it records the peak number of concurrent checks.
type slowChecker struct {
	*fakePlayback
	dead           map[core.RatingKey]bool
	errs           map[core.RatingKey]error
	mu             sync.Mutex
	inflight, peak int
	calls          int
}

func (c *slowChecker) CheckFileAvailability(ctx context.Context, _ *core.User, key core.RatingKey, _ core.ClientCaps, _ string) (bool, error) {
	c.mu.Lock()
	c.calls++
	c.inflight++
	c.peak = max(c.peak, c.inflight)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.inflight--; c.mu.Unlock() }()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if err := c.errs[key]; err != nil {
		return false, err
	}
	return !c.dead[key], nil
}

// Every Part of every version carries exists/accessible: checks run with
// bounded concurrency inside one budget, and a Part without a verdict keeps
// its offered (available) state.
func TestFileAvailabilityAnnotatesEveryPart(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	defer f.srv.Close()
	// Titles 1, 5, 9 are dead; 2, 6, 10 (busy check slots) and 3, 7, 11
	// (nothing found yet) get no verdict and keep the offered state.
	checker := &slowChecker{fakePlayback: newFakePlayback(sampleInfo), dead: map[core.RatingKey]bool{}, errs: map[core.RatingKey]error{}}
	for n := int64(1); n <= 12; n++ {
		switch n % 4 {
		case 1:
			checker.dead[core.MovieKey(n)] = true
		case 2:
			checker.errs[core.MovieKey(n)] = core.ErrSourceUnavailable
		case 3:
			checker.errs[core.MovieKey(n)] = core.ErrNotAvailable
		}
	}
	f.srv.d.Playback = checker

	mc := &model.MediaContainer{}
	for n := int64(1); n <= 12; n++ {
		id := int64(core.MovieKey(n))
		// Every title twice: one check answers both Parts.
		mc.Metadata = append(mc.Metadata, model.Metadata{Media: []model.Media{{Part: []model.Part{{ID: id}}}, {Part: []model.Part{{ID: id}}}}})
	}
	req := httptest.NewRequest(http.MethodGet, "/library/metadata/x?checkFiles=1", nil)
	req = req.WithContext(withRequest(req.Context(), &Request{User: &ownerUser, ClientID: "c"}))
	f.srv.projectFileAvailability(req, mc)

	if checker.peak > fileCheckParallel || checker.peak < 2 {
		t.Fatalf("peak concurrency = %d, want 2..%d", checker.peak, fileCheckParallel)
	}
	if checker.calls != 12 {
		t.Fatalf("checks = %d, want one per title", checker.calls)
	}
	for n, md := range mc.Metadata {
		want := (n+1)%4 != 1
		for _, m := range md.Media {
			p := m.Part[0]
			if p.Exists == nil || p.Accessible == nil || *p.Exists != want || *p.Accessible != want {
				t.Fatalf("title %d part exists=%v accessible=%v, want %v", n+1, p.Exists, p.Accessible, want)
			}
		}
	}
}
