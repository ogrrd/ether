package plex

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

var errNotAvailable = core.ErrNotAvailable

func errWrap(op string, err error) error { return fmt.Errorf("%s: %w", op, err) }

func ws(k core.RatingKey, offsetMS int64, views int) core.WatchStatus {
	return core.WatchStatus{Key: k, ViewOffset: time.Duration(offsetMS) * time.Millisecond, ViewCount: views, LastViewedAt: fixedNow.Add(-time.Hour), UpdatedAt: fixedNow}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, rec.Body.String())
	}
	return v["MediaContainer"]
}

func TestAuth(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// No token.
	rec := f.request(t, http.MethodGet, "/library/sections", map[string]string{"X-Plex-Token": ""})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "401 Unauthorized") {
		t.Fatalf("no token: %d %s", rec.Code, rec.Body.String())
	}
	// Bad token.
	rec = f.request(t, http.MethodGet, "/library/sections", map[string]string{"X-Plex-Token": "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rec.Code)
	}
	// Token as query param.
	rec = f.request(t, http.MethodGet, "/library/sections?X-Plex-Token="+friendToken, map[string]string{"X-Plex-Token": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("query token: %d", rec.Code)
	}
	// /identity is public.
	rec = f.request(t, http.MethodGet, "/identity", map[string]string{"X-Plex-Token": ""})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `machineIdentifier="0123456789abcdef`) {
		t.Fatalf("identity: %d %s", rec.Code, rec.Body.String())
	}
	// Owner-only prefs.
	rec = f.request(t, http.MethodGet, "/:/prefs", map[string]string{"X-Plex-Token": friendToken})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("friend prefs: %d", rec.Code)
	}
	rec = f.get(t, "/:/prefs")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="FriendlyName"`) {
		t.Fatalf("owner prefs: %d", rec.Code)
	}
}

func TestClientSeenOnlyAfterAuthentication(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var seen []string
	f.srv.d.Hooks.OnClientSeen = func(id string) { seen = append(seen, id) }
	f.request(t, http.MethodGet, "/identity", map[string]string{"X-Plex-Token": "", "X-Plex-Client-Identifier": "public"})
	f.request(t, http.MethodGet, "/library/sections", map[string]string{"X-Plex-Token": "bad", "X-Plex-Client-Identifier": "unauthorized"})
	if len(seen) != 0 {
		t.Fatalf("observed unauthenticated clients: %v", seen)
	}
	f.request(t, http.MethodGet, "/library/sections", map[string]string{"X-Plex-Token": friendToken, "X-Plex-Client-Identifier": "device"})
	if len(seen) != 1 || seen[0] != "device" {
		t.Fatalf("seen = %v", seen)
	}
}

func TestSectionRestriction(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Auth.(*fakeAuth).users["kid"] = core.User{ID: 3, Username: "kid", AllowedLibraries: []core.LibraryID{1}}
	rec := f.request(t, http.MethodGet, "/library/sections", map[string]string{"X-Plex-Token": "kid"})
	if strings.Contains(rec.Body.String(), `title="TV Shows"`) {
		t.Fatalf("restricted section visible: %s", rec.Body.String())
	}
	rec = f.request(t, http.MethodGet, "/library/sections/2/all", map[string]string{"X-Plex-Token": "kid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("restricted section listing: %d", rec.Code)
	}
	rec = f.request(t, http.MethodGet, "/library/metadata/"+breakingBad.Key.String(), map[string]string{"X-Plex-Token": "kid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("restricted item: %d", rec.Code)
	}
}

func TestCORSAndHeaders(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.request(t, http.MethodOptions, "/library/sections", map[string]string{
		"Origin": "https://app.plex.tv", "Access-Control-Request-Headers": "x-plex-token,x-plex-client-identifier", "X-Plex-Token": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight: %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "https://app.plex.tv" || !strings.Contains(h.Get("Access-Control-Allow-Headers"), "x-plex-token") ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("preflight headers: %v", h)
	}
	rec = f.get(t, "/library/sections", "Origin", "https://app.plex.tv")
	if rec.Header().Get("X-Plex-Protocol") != "1.0" || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.plex.tv" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Fatalf("content type %q", ct)
	}
	rec = f.get(t, "/library/sections", jsonAccept...)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("json content type %q", ct)
	}
}

func TestGzip(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/media/providers", "Accept-Encoding", "gzip, deflate")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	if !strings.Contains(string(b), "<MediaProvider") {
		t.Fatalf("bad body: %s", b)
	}
}

func TestUnknownEndpoint(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/some/unknown/endpoint")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "<h1>404 Not Found</h1>") || rec.Header().Get("Content-Type") != "text/html" {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body.String())
	}
	// Endpoints Ether knowingly stubs still answer an empty container.
	rec = f.get(t, "/video/:/transcode/universal/subtitles")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<MediaContainer size=\"0\">\n</MediaContainer>") {
		t.Fatalf("stubbed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPaging(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/sections/1/all?type=1&sort=titleSort", jsonAccept[0], jsonAccept[1], "X-Plex-Container-Start", "1", "X-Plex-Container-Size", "1")
	mc := decodeJSON(t, rec)
	if mc["size"].(float64) != 1 || mc["totalSize"].(float64) != 2 || mc["offset"].(float64) != 1 {
		t.Fatalf("paging attrs: %v", mc)
	}
	if rec.Header().Get("X-Plex-Container-Total-Size") != "2" || rec.Header().Get("X-Plex-Container-Start") != "1" {
		t.Fatalf("paging headers: %v", rec.Header())
	}
	md := mc["Metadata"].([]any)[0].(map[string]any)
	if md["title"] != "Fjord Harbor" {
		t.Fatalf("second by title should be Fjord Harbor, got %v", md["title"])
	}
	// Size 0 learns the total.
	mc = decodeJSON(t, f.get(t, "/library/sections/1/all?X-Plex-Container-Start=0&X-Plex-Container-Size=0", jsonAccept...))
	if mc["size"].(float64) != 0 || mc["totalSize"].(float64) != 2 {
		t.Fatalf("size 0: %v", mc)
	}
}

func TestBrowseFilters(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.get(t, "/library/sections/1/all?type=1&genre=18&year=1999&sort=originallyAvailableAt:desc")
	q := f.catalog.lastQ
	if len(q.Filter.Genres) != 1 || q.Filter.Genres[0] != "Drama" || q.Filter.YearFrom != 1999 || q.Filter.YearTo != 1999 ||
		q.Sort != core.SortReleased || !q.Desc || q.Filter.Type != core.TypeMovie {
		t.Fatalf("query: %+v", q)
	}
	f.get(t, "/library/sections/1/all?decade=1980&year>>=1981&sort=titleSort")
	q = f.catalog.lastQ
	if q.Sort != core.SortTitle || q.Desc {
		t.Fatalf("sort: %+v", q)
	}
	// Unwatched filter uses watch state.
	f.state.set(ownerUser.ID, ws(alien.Key, 0, 1))
	mc := decodeJSON(t, f.get(t, "/library/sections/1/all?unwatched=1", jsonAccept...))
	md := mc["Metadata"].([]any)
	if len(md) != 1 || md[0].(map[string]any)["title"] != "Fjord Harbor" {
		t.Fatalf("unwatched: %v", md)
	}
}

func TestTimelineUpdatesState(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// Start a session through the decision.
	f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&session=t1")
	ps, ok := f.playback.SessionFor(ownerUser.ID, "client-1", fightClub.Key)
	if !ok {
		t.Fatal("missing prepared playback")
	}
	ps.BytesServed = 1000 // the viewer received media before reporting progress
	rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&key=/library/metadata/1000000000000550&state=playing&time=120000&duration=8340000&playQueueItemID=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d", rec.Code)
	}
	if len(f.state.progress) != 1 {
		t.Fatalf("progress calls: %v", f.state.progress)
	}
	p := f.state.progress[0]
	if p.user != ownerUser.ID || p.key != fightClub.Key || p.offset != 2*time.Minute || p.duration != 8340*time.Second || p.state != "playing" {
		t.Fatalf("progress: %+v", p)
	}
	select {
	case h := <-f.hooks:
		if h != "owner 1000000000000550 120000 playing" {
			t.Fatalf("hook: %s", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnTimeline not called")
	}
	select {
	case h := <-f.hooks:
		t.Fatalf("timeline hook ran again: %s", h)
	case <-time.After(50 * time.Millisecond):
	}
	if len(f.playback.touched) != 1 {
		t.Fatalf("session not touched: %v", f.playback.touched)
	}
	if len(f.srv.sessions.list()) != 1 {
		t.Fatal("session not tracked")
	}
	// Stopped ends the play session.
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=130000&duration=8340000")
	if len(f.playback.stopped) != 1 {
		t.Fatalf("playback not stopped: %v", f.playback.stopped)
	}
	if len(f.srv.sessions.list()) != 0 {
		t.Fatal("session still tracked")
	}
}

func TestScrobbleRate(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/:/scrobble?key=1000000000000550&identifier=com.plexapp.plugins.library")
	if rec.Code != http.StatusOK {
		t.Fatalf("scrobble %d", rec.Code)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "text/html" {
		t.Fatalf("scrobble body %q", rec.Body.String())
	}
	if st, _ := f.state.Get(context.Background(), ownerUser.ID, fightClub.Key); st.ViewCount != 1 {
		t.Fatal("not scrobbled")
	}
	f.get(t, "/:/unscrobble?key=1000000000000550&identifier=com.plexapp.plugins.library")
	if st, _ := f.state.Get(context.Background(), ownerUser.ID, fightClub.Key); st.ViewCount != 0 {
		t.Fatal("not unscrobbled")
	}
	// Season scrobble marks each episode.
	f.get(t, "/:/scrobble?key="+bbS1.Key.String()+"&identifier=com.plexapp.plugins.library")
	if len(f.state.scrobbed) != 3 {
		t.Fatalf("season scrobble: %v", f.state.scrobbed)
	}
	f.request(t, http.MethodPut, "/:/rate?key=1000000000000550&identifier=com.plexapp.plugins.library&rating=8", nil)
	if f.state.rated[fightClub.Key] != 8 {
		t.Fatalf("rate: %v", f.state.rated)
	}
	if rec := f.get(t, "/:/rate?key=1000000000000550&rating=x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad rating: %d", rec.Code)
	}
}

func TestDirectPlayRange(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", "Range", "bytes=10-19")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "0123456789" || rec.Header().Get("Content-Range") != "bytes 10-19/10000" {
		t.Fatalf("range body %q %v", rec.Body.String(), rec.Header())
	}
	if f.playback.prepared != 1 {
		t.Fatalf("prepared %d", f.playback.prepared)
	}
	// Second request reuses the session.
	f.get(t, "/library/parts/1000000000000550/file.mkv", "Range", "bytes=0-1")
	if f.playback.prepared != 1 {
		t.Fatalf("session not reused: %d", f.playback.prepared)
	}
	// Not available.
	f.playback.failures[alien.Key] = core.ErrNotAvailable
	if rec := f.get(t, "/library/parts/1000000000000348/1/file.mkv"); rec.Code != http.StatusNotFound || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("unavailable part: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	f.playback.failures[alien.Key] = core.ErrStreamLimit
	if rec := f.get(t, "/library/parts/1000000000000348/1/file.mkv"); rec.Code != http.StatusBadRequest {
		t.Fatalf("stream limit part: %d", rec.Code)
	}
}

func TestDecisionStreamLimit(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.failures[fightClub.Key] = core.ErrStreamLimit
	mc := decodeJSON(t, f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550", jsonAccept...))
	if mc["generalDecisionCode"].(float64) != 2000 || !strings.Contains(mc["generalDecisionText"].(string), "Stream limit") {
		t.Fatalf("decision: %v", mc)
	}
}

func TestDecisionOptions(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// Select the French track and the embedded English subtitle by id.
	frID := f.bindStream(fightClub.Key, 2, sampleInfo.Streams[2])
	subID := f.bindStream(fightClub.Key, 3, sampleInfo.Streams[3])
	f.get(t, "/library/metadata/1000000000000550") // registers stream ids
	f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/1000000000000550?audioStreamID=%d&subtitleStreamID=%d&allParts=1", frID, subID), nil)
	f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&directPlay=0&maxVideoBitrate=4000&videoResolution=1280x720&subtitles=burn&session=o1",
		"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
	o := f.trans.lastOpts
	if o.DirectPlay || !o.DirectStream || o.MaxBitrateKbps != 4000 || o.MaxHeight != 720 || o.AudioStream != 2 || o.SubtitleStream != 3 || !o.BurnSubtitles {
		t.Fatalf("opts: %+v", o)
	}
	if f.trans.lastCaps.MaxHeight != 720 || f.trans.lastCaps.Product != "Plex Web" {
		t.Fatalf("caps: %+v", f.trans.lastCaps)
	}
}

func TestStartHLS(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=abc-123&protocol=hls&offset=30.5&directStream=1"))
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=abc-123&protocol=hls&offset=30.5&directStream=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A copy advertises its peak segment rate, not the 7600 kbit/s
	// container average (no keyframe index here: 1.5x + 64).
	if !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=11464000,RESOLUTION=1920x1080") || !strings.Contains(body, "session/abc-123/base/index.m3u8?X-Ether-Stream-Token=") {
		t.Fatalf("master: %q", body)
	}
	if rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("ct %q", rec.Header().Get("Content-Type"))
	}
	h := f.trans.sessions["abc-123"]
	if h == nil || h.req.StartOffset != 30500*time.Millisecond || !strings.Contains(h.req.InputURL, "ps-1-client-1") || h.req.Decision.Kind != core.DecisionRemux {
		t.Fatalf("hls req: %+v", h)
	}
	rec = f.get(t, "/video/:/transcode/universal/session/abc-123/base/index.m3u8")
	if !strings.Contains(rec.Body.String(), "00000.ts") {
		t.Fatalf("playlist: %s", rec.Body.String())
	}
	rec = f.get(t, "/video/:/transcode/universal/session/abc-123/base/00042.ts")
	if rec.Body.String() != "segment-42" {
		t.Fatalf("segment: %s", rec.Body.String())
	}
	if rec := f.get(t, "/video/:/transcode/universal/session/nope/base/index.m3u8"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", rec.Code)
	}
	// Same offset reuses; a seek restarts.
	f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=abc-123&offset=30.5")
	if len(f.trans.stopped) != 0 {
		t.Fatalf("restarted without seek: %v", f.trans.stopped)
	}
	f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=abc-123&offset=600")
	if len(f.trans.stopped) != 1 || f.trans.sessions["abc-123"].req.StartOffset != 600*time.Second {
		t.Fatalf("seek restart: %v", f.trans.stopped)
	}
	f.get(t, "/video/:/transcode/universal/stop?session=abc-123")
	if _, ok := f.trans.sessions["abc-123"]; ok {
		t.Fatal("not stopped")
	}
}

func TestSegmentNumber(t *testing.T) {
	for in, want := range map[string]int{"00012.ts": 12, "12.ts": 12, "segment-7.m4s": 7, "0.ts": 0} {
		if n, ok := segmentNumber(in); !ok || n != want {
			t.Errorf("%s: %d %v", in, n, ok)
		}
	}
	if n, ok := segmentNumber("header"); !ok || n != -1 {
		t.Error("Matroska header should select initialization")
	}
}

func TestPlaylists(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/1000000000000550,1000000000000348"
	rec := f.request(t, http.MethodPost, "/playlists?type=video&title=Mine&smart=0&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"})
	mc := decodeJSON(t, rec)
	md := mc["Metadata"].([]any)[0].(map[string]any)
	if md["title"] != "Mine" || md["leafCount"].(float64) != 2 || md["type"] != "playlist" {
		t.Fatalf("create: %v", md)
	}
	key := md["ratingKey"].(string)
	// Add a show: expands to its episodes.
	f.request(t, http.MethodPut, "/playlists/"+key+"/items?uri="+urlEsc("library://x/item/%2Flibrary%2Fmetadata%2F"+breakingBad.Key.String()), nil)
	mc = decodeJSON(t, f.get(t, "/playlists/"+key+"/items", jsonAccept...))
	items := mc["Metadata"].([]any)
	if len(items) != 4 || items[3].(map[string]any)["playlistItemID"].(float64) != 4 {
		t.Fatalf("items: %v", items)
	}
	// Move item 1 after item 3.
	f.request(t, http.MethodPut, "/playlists/"+key+"/items/1/move?after=3", nil)
	pl, _ := f.pls.Get(context.Background(), ownerUser.ID, mustKey(key))
	if pl.Items[2] != fightClub.Key || pl.Items[0] != alien.Key {
		t.Fatalf("move: %v", pl.Items)
	}
	// Remove item 1.
	f.request(t, http.MethodDelete, "/playlists/"+key+"/items/1", nil)
	pl, _ = f.pls.Get(context.Background(), ownerUser.ID, mustKey(key))
	if len(pl.Items) != 3 || pl.Items[0] != bbE1.Key {
		t.Fatalf("remove: %v", pl.Items)
	}
	// Friends can't see the owner's playlist.
	if rec := f.request(t, http.MethodGet, "/playlists/"+key, map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusNotFound {
		t.Fatalf("friend sees playlist: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodDelete, "/playlists/"+key, nil); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("playlist delete: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if _, err := f.pls.Get(context.Background(), ownerUser.ID, mustKey(key)); err == nil {
		t.Fatal("not deleted")
	}
}

func mustKey(s string) core.RatingKey {
	k, err := core.ParseRatingKey(s)
	if err != nil {
		panic(err)
	}
	return k
}

func urlEsc(s string) string {
	r := strings.NewReplacer("%", "%25", ":", "%3A", "/", "%2F", ",", "%2C", "&", "%26", "?", "%3F")
	return r.Replace(s)
}

func TestPlayQueueOps(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/"+breakingBad.Key.String())+"&key="+urlEsc("/library/metadata/"+bbE2.Key.String()), map[string]string{"Accept": "application/json"})
	mc := decodeJSON(t, rec)
	if mc["playQueueTotalCount"].(float64) != 2 || mc["playQueueSelectedMetadataItemID"] != bbE2.Key.String() {
		t.Fatalf("pq: %v", mc)
	}
	id := fmt.Sprint(int64(mc["playQueueID"].(float64)))
	items := mc["Metadata"].([]any)
	first := int64(items[0].(map[string]any)["playQueueItemID"].(float64))
	// Add a movie next, then move the first item after it.
	rec = f.request(t, http.MethodPut, "/playQueues/"+id+"?uri="+urlEsc("/library/metadata/"+fightClub.Key.String())+"&next=1", map[string]string{"Accept": "application/json"})
	mc = decodeJSON(t, rec)
	if mc["playQueueTotalCount"].(float64) != 3 || mc["playQueueVersion"].(float64) != 2 {
		t.Fatalf("add: %v", mc)
	}
	rec = f.request(t, http.MethodDelete, fmt.Sprintf("/playQueues/%s/items/%d", id, first), map[string]string{"Accept": "application/json"})
	mc = decodeJSON(t, rec)
	if mc["playQueueTotalCount"].(float64) != 3 || mc["playQueueSelectedMetadataItemID"] != bbE2.Key.String() {
		t.Fatalf("remove: %v", mc)
	}
	// Other users can't read it.
	if rec := f.request(t, http.MethodGet, "/playQueues/"+id, map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusNotFound {
		t.Fatalf("friend pq: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodPut, "/playQueues/"+id+"/shuffle", nil); rec.Code != http.StatusOK {
		t.Fatalf("shuffle %d", rec.Code)
	}
	if rec := f.request(t, http.MethodPut, "/playQueues/"+id+"/unshuffle", nil); rec.Code != http.StatusOK {
		t.Fatalf("unshuffle %d", rec.Code)
	}
}

func TestURIPath(t *testing.T) {
	cases := map[string]string{
		"server://abc/com.plexapp.plugins.library/library/metadata/1,2":       "/library/metadata/1,2",
		"library://uuid/item/%2Flibrary%2Fmetadata%2F123":                     "/library/metadata/123",
		"library://uuid/directory/%2Flibrary%2Fsections%2F1%2Fall%3Ftype%3D1": "/library/sections/1/all?type=1",
		"/library/metadata/5": "/library/metadata/5",
	}
	for in, want := range cases {
		got, err := uriPath(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := uriPath("http://evil"); err == nil {
		t.Error("expected error")
	}
}

func TestPhotoTranscode(t *testing.T) {
	var hits atomic.Int32
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		fmt.Fprint(w, "IMG:"+r.URL.Path)
	}))
	defer tmdb.Close()
	f := newFixture()
	f.srv.Close()
	d := f.srv.currentTestDeps()
	d.ImageURL = func(path, size string) string { return tmdb.URL + "/t/p/" + size + path }
	d.HTTPClient = tmdb.Client()
	f.srv = newTestServer(d)
	defer f.srv.Close()

	rec := f.get(t, "/photo/:/transcode?width=300&height=450&minSize=1&upscale=1&url="+urlEsc("/library/metadata/1000000000000550/thumb/1759276800"))
	if rec.Code != http.StatusOK || rec.Body.String() != "IMG:/t/p/w300/poster550.jpg" {
		t.Fatalf("own thumb: %d %q", rec.Code, rec.Body.String())
	}
	// Absolute self URL, art.
	rec = f.get(t, "/photo/:/transcode?width=1920&url="+urlEsc("http://127.0.0.1:32400/library/metadata/1000000000000550/art/1"))
	if rec.Body.String() != "IMG:/t/p/original/backdrop550.jpg" {
		t.Fatalf("own art: %q", rec.Body.String())
	}
	// Absolute TMDB URL is resized.
	rec = f.get(t, "/photo/:/transcode?width=180&url="+urlEsc(tmdb.URL+"/t/p/original/brad.jpg"))
	if rec.Body.String() != "IMG:/t/p/w185/brad.jpg" {
		t.Fatalf("tmdb url: %q", rec.Body.String())
	}
	// Cached.
	before := hits.Load()
	f.get(t, "/photo/:/transcode?width=180&url="+urlEsc(tmdb.URL+"/t/p/original/brad.jpg"))
	if hits.Load() != before {
		t.Fatal("not cached")
	}
	// Not an open proxy.
	if rec := f.get(t, "/photo/:/transcode?url="+urlEsc("http://169.254.169.254/latest/meta-data")); rec.Code != http.StatusForbidden {
		t.Fatalf("open proxy: %d", rec.Code)
	}
	// Direct thumb and episode still fallback.
	rec = f.get(t, "/library/metadata/"+bbE1.Key.String()+"/thumb/1")
	if rec.Body.String() != "IMG:/t/p/original/pilot.jpg" {
		t.Fatalf("still: %q", rec.Body.String())
	}
	rec = f.get(t, "/library/metadata/"+bbE2.Key.String()+"/art/1")
	if rec.Body.String() != "IMG:/t/p/w1280/bbart.jpg" {
		t.Fatalf("episode art falls back to show: %q", rec.Body.String())
	}
	rec = f.get(t, "/library/collections/5000000000008091/thumb/1")
	if rec.Body.String() != "IMG:/t/p/w500/aliencol.jpg" {
		t.Fatalf("collection thumb: %q", rec.Body.String())
	}
	if rec := f.get(t, "/library/metadata/1000000000000348/art/1"); rec.Code != http.StatusNotFound {
		t.Fatalf("no art: %d", rec.Code)
	}
}

func TestTMDBSize(t *testing.T) {
	for w, want := range map[int]string{0: "original", 90: "w92", 185: "w185", 186: "w300", 500: "w500", 1280: "w1280", 1281: "original"} {
		if got := tmdbSize(w); got != want {
			t.Errorf("%d: %s want %s", w, got, want)
		}
	}
}

func TestWebsocketNotifications(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/:/websockets/notifications?filters=playing&X-Plex-Token=" + ownerToken
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://app.plex.tv"}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// Wait for the subscription to register.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.srv.notifier.mu.Lock()
		n := len(f.srv.notifier.subs)
		f.srv.notifier.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/:/timeline?ratingKey=1000000000000550&state=paused&time=5000&duration=10000", nil)
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-ws")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	typ, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("type %v", typ)
	}
	var v struct {
		NotificationContainer struct {
			Type                         string
			PlaySessionStateNotification []struct {
				RatingKey        string `json:"ratingKey"`
				State            string `json:"state"`
				ViewOffset       int64  `json:"viewOffset"`
				ClientIdentifier string `json:"clientIdentifier"`
			}
		}
	}
	if err := json.Unmarshal(msg, &v); err != nil {
		t.Fatalf("%v: %s", err, msg)
	}
	n := v.NotificationContainer
	if n.Type != "playing" || len(n.PlaySessionStateNotification) != 1 || n.PlaySessionStateNotification[0].State != "paused" ||
		n.PlaySessionStateNotification[0].ViewOffset != 5000 || n.PlaySessionStateNotification[0].ClientIdentifier != "client-ws" {
		t.Fatalf("notification: %s", msg)
	}
	// Unauthenticated websocket is refused.
	_, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/:/websockets/notifications", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ws: %v %v", err, resp)
	}
}

func TestEventSource(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/:/eventsource/notifications?filters=timeline", nil)
	req.Header.Set("X-Plex-Token", ownerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("ct %q", resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); !strings.HasPrefix(line, ":") {
		t.Fatalf("first line %q", line)
	}
	go f.srv.WatchStateChanged(ownerUser.ID, bbE1.Key)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "event: timeline") {
			data, _ := br.ReadString('\n')
			// PMS sends each entry bare on eventsource, not the websocket
			// NotificationContainer, and names the episode's season and show.
			var v struct {
				TimelineEntry *model.TimelineEntry
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(data), "data: ")), &v); err != nil || v.TimelineEntry == nil {
				t.Fatalf("data %q: %v", data, err)
			}
			e := v.TimelineEntry
			if e.ItemID != bbE1.Key.String() || e.ParentItemID != bbE1.Key.Parent().String() ||
				e.RootItemID != breakingBad.Key.String() || e.Type != 4 || e.State != 5 ||
				e.MetadataState != "" || e.Identifier != model.LibraryIdentifier {
				t.Fatalf("timeline entry %+v", e)
			}
			return
		}
	}
}

func TestMovieWatchRefreshTimelineNamesItself(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	sub, _ := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
	defer f.srv.notifier.unsubscribe(sub)
	f.srv.WatchStateChanged(ownerUser.ID, fightClub.Key)
	select {
	case msg := <-sub.ch:
		var body struct {
			NotificationContainer struct {
				TimelineEntry []model.TimelineEntry
			}
		}
		if err := json.Unmarshal(msg.data, &body); err != nil {
			t.Fatal(err)
		}
		entries := body.NotificationContainer.TimelineEntry
		if len(entries) != 1 {
			t.Fatalf("timeline %s", msg.data)
		}
		e := entries[0]
		if e.ItemID != fightClub.Key.String() || e.RootItemID != fightClub.Key.String() ||
			e.Type != 1 || e.State != 5 || e.MetadataState != "" {
			t.Fatalf("movie timeline %+v", e)
		}
	default:
		t.Fatal("movie watch refresh published no timeline entry")
	}
}

func TestOnDeckAndContinueWatching(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, ws(fightClub.Key, 1_000_000, 0))
	mc := decodeJSON(t, f.get(t, "/hubs/continueWatching", jsonAccept...))
	hub := mc["Hub"].([]any)[0].(map[string]any)
	md := hub["Metadata"].([]any)[0].(map[string]any)
	if hub["hubIdentifier"] != "continueWatching" || md["viewOffset"].(float64) != 1_000_000 {
		t.Fatalf("continue: %v", hub)
	}
	// Show on-deck: first unwatched episode after the last watched.
	f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
	mc = decodeJSON(t, f.get(t, "/library/metadata/"+breakingBad.Key.String()+"/onDeck", jsonAccept...))
	if mc["Metadata"].([]any)[0].(map[string]any)["ratingKey"] != bbE2.Key.String() {
		t.Fatalf("on deck: %v", mc)
	}
}

func TestTerminate(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000", "X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	if rec := f.request(t, http.MethodGet, "/status/sessions/terminate?sessionId=sx&reason=Bedtime", map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusForbidden {
		t.Fatalf("friend terminate: %d", rec.Code)
	}
	if rec := f.get(t, "/status/sessions/terminate?sessionId=sx&reason=Bedtime"); rec.Code != http.StatusOK {
		t.Fatalf("terminate: %d", rec.Code)
	}
	rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=2000&duration=10000", "X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	if !strings.Contains(rec.Body.String(), `terminationCode="2006" terminationText="Bedtime"`) {
		t.Fatalf("termination: %s", rec.Body.String())
	}
}

func TestHLSInitializationSegmentServedForNativePlayer(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=native-fmp4&directPlay=0&directStream=1"))
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=native-fmp4&directPlay=0&directStream=1")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	// AVPlayer's segment requests do not carry the original Plex headers.
	e, _ := f.srv.transcodes.get("native-fmp4")
	init := f.request(t, "GET", streamGrantURL("/video/:/transcode/universal/session/native-fmp4/base/init.mp4", e.streamToken), map[string]string{"X-Plex-Token": ""})
	if init.Code != 200 || init.Body.String() != "segment--1" {
		t.Fatalf("init was not routed to InitSegment: %d %s", init.Code, init.Body.String())
	}
}

// The largest valid PMS paging size must not overflow page arithmetic.
func TestPagingHugeContainerSize(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.popular", Title: "Popular", Type: core.TypeMovie, Items: []core.Title{fightClub, alien}}}}})
	f.cols.cols[core.CollectionKey(77)] = core.Collection{Key: core.CollectionKey(77), Title: "Two", Kind: "franchise", Type: core.TypeMovie, ChildCount: 2}
	f.cols.items[core.CollectionKey(77)] = []core.Title{fightClub, alien}
	page := "X-Plex-Container-Start=1&X-Plex-Container-Size=2147483647"
	for _, path := range []string{
		"/library/metadata/" + bbS1.Key.String() + "/children?" + page,
		"/library/metadata/" + bbS1.Key.String() + "/allLeaves?" + page,
		"/hubs/items/movie.popular?contentDirectoryID=1&" + page,
		"/library/collections/" + core.CollectionKey(77).String() + "/children?" + page,
	} {
		rec := f.get(t, path, jsonAccept...)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if mc := decodeJSON(t, rec); mc["size"].(float64) != 1 {
			t.Errorf("%s: size %v, want the one item after the offset", path, mc["size"])
		}
	}
}

func TestDurationParamRejectsNonFinite(t *testing.T) {
	for _, v := range []string{"NaN", "Inf", "+Inf", "-1", "1e300", "x", ""} {
		if d := msParam(v); d != 0 {
			t.Errorf("msParam(%q) = %v", v, d)
		}
		if d := durationParam(v, time.Second); d != 0 {
			t.Errorf("durationParam(%q) = %v", v, d)
		}
	}
	if d := msParam("1500.5"); d != 1500500*time.Microsecond {
		t.Errorf("msParam(1500.5) = %v", d)
	}
	if d := durationParam("90", time.Second); d != 90*time.Second {
		t.Errorf("durationParam(90) = %v", d)
	}
}

func TestRateNaNAndEmptyTerminateAreBadRequests(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	if rec := f.get(t, "/:/rate?key=1000000000000550&identifier=com.plexapp.plugins.library&rating=NaN"); rec.Code != http.StatusBadRequest {
		t.Errorf("rate NaN: %d", rec.Code)
	}
	// A session without an identifier must not be stopped by an empty id.
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000", "X-Plex-Token", friendToken)
	if rec := f.get(t, "/status/sessions/terminate?reason=Bedtime"); rec.Code != http.StatusBadRequest {
		t.Errorf("terminate without sessionId: %d", rec.Code)
	}
	rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=2000&duration=10000", "X-Plex-Token", friendToken)
	if strings.Contains(rec.Body.String(), "terminationCode") {
		t.Fatalf("an empty session id terminated a stream: %s", rec.Body.String())
	}
}

// A kill whose client never sends another timeline must not stay queued
// forever.
func TestTerminationsExpire(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setTermination("gone", "Bedtime")
	f.srv.setTermination("back", "Bedtime")
	f.srv.expireTerminations(fixedNow.Add(terminationTTL/2), terminationTTL)
	if reason, ok := f.srv.takeTermination("back"); !ok || reason != "Bedtime" {
		t.Fatalf("a fresh kill was dropped: %q %v", reason, ok)
	}
	f.srv.expireTerminations(fixedNow.Add(terminationTTL+time.Second), terminationTTL)
	f.srv.termMu.Lock()
	n := len(f.srv.terms)
	f.srv.termMu.Unlock()
	if n != 0 {
		t.Fatalf("%d stale kills kept", n)
	}
}

// Unshuffling a shuffled queue whose items were all removed must not panic.
func TestPlayQueueUnshuffleAfterRemovingEverything(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	js := map[string]string{"Accept": "application/json"}
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/"+bbS1.Key.String()), js)
	mc := decodeJSON(t, rec)
	id := fmt.Sprint(int64(mc["playQueueID"].(float64)))
	rec = f.request(t, http.MethodPut, "/playQueues/"+id+"/shuffle", js)
	if rec.Code != http.StatusOK {
		t.Fatalf("shuffle %d", rec.Code)
	}
	mc = decodeJSON(t, rec)
	for _, it := range mc["Metadata"].([]any) {
		itemID := int64(it.(map[string]any)["playQueueItemID"].(float64))
		if rec := f.request(t, http.MethodDelete, fmt.Sprintf("/playQueues/%s/items/%d", id, itemID), nil); rec.Code != http.StatusOK {
			t.Fatalf("remove %d: %d", itemID, rec.Code)
		}
	}
	if rec := f.request(t, http.MethodPut, "/playQueues/"+id+"/unshuffle", nil); rec.Code != http.StatusOK {
		t.Fatalf("unshuffle of an empty queue: %d", rec.Code)
	}
}

// Moving a playlist item after itself leaves the playlist unchanged.
func TestPlaylistMoveAfterItself(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	pl, err := f.pls.Create(context.Background(), ownerUser.ID, "Mine", []core.RatingKey{fightClub.Key, alien.Key})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"1", "2"} {
		rec := f.request(t, http.MethodPut, "/playlists/"+pl.Key.String()+"/items/"+item+"/move?after="+item, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("move %s after itself: %d %s", item, rec.Code, rec.Body.String())
		}
		got, _ := f.pls.Get(context.Background(), ownerUser.ID, pl.Key)
		if len(got.Items) != 2 || got.Items[0] != fightClub.Key || got.Items[1] != alien.Key {
			t.Fatalf("move %s after itself reordered: %v", item, got.Items)
		}
	}
}

func TestRedactQueryHidesCredentials(t *testing.T) {
	got := redactQuery("a=1&X-Plex-Token=secret&" + streamGrantParam + "=grant&" + readCapParam + "=read&x-plex-token=s2")
	if strings.Contains(got, "secret") || strings.Contains(got, "grant") || strings.Contains(got, "read") || strings.Contains(got, "s2") || !strings.Contains(got, "a=1") {
		t.Fatalf("redactQuery = %q", got)
	}
}

func TestAcceptsGzipQuality(t *testing.T) {
	for v, want := range map[string]bool{"gzip": true, "gzip;q=0.8": true, "gzip; q=0.5, br": true, "gzip;q=0": false, "gzip;q=0.0": false, "br, gzip;q=0": false, "identity": false} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", v)
		if got := acceptsGzip(r); got != want {
			t.Errorf("acceptsGzip(%q) = %v", v, got)
		}
	}
}
