package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPlaylistsHomeHub(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	if strings.Contains(f.get(t, "/hubs").Body.String(), "home.playlists") {
		t.Fatal("no playlists hub without playlists")
	}
	pl, _ := f.pls.Create(context.Background(), ownerUser.ID, "Heist Night", []core.RatingKey{fightClub.Key})
	body := f.get(t, "/hubs").Body.String()
	for _, want := range []string{`hubIdentifier="home.playlists"`, `context="hub.home.playlists"`, `type="playlist"`, `hubKey="/library/metadata/` + pl.Key.String() + `"`, `<Playlist ratingKey="` + pl.Key.String() + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("hubs lack %s:\n%s", want, body)
		}
	}
	if strings.Contains(f.get(t, "/hubs?contentDirectoryID=1").Body.String(), "home.playlists") {
		t.Error("section-filtered home hubs must not include playlists")
	}
	// The playlist's duration appears once its items' runtimes are known.
	body = f.getWarm(t, "/playlists/"+pl.Key.String()).Body.String()
	if !strings.Contains(body, `duration="8340000"`) {
		t.Errorf("playlist duration missing: %s", body)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"The 'Burbs":             "the-burbs",
		"Coyote vs. Acme":        "coyote-vs-acme",
		"9-1-1":                  "9-1-1",
		"2 Fast 2 Furious":       "2-fast-2-furious",
		"Fast & Furious":         "fast-and-furious",
		"Amélie":                 "amelie",
		"Léon: The Professional": "leon-the-professional",
		"  Spaced  Out!! ":       "spaced-out",
		"ドラゴンボール":                "",
		"WALL·E":                 "wall-e",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBGQueueDedupesAndDrops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	q := newBGQueue(ctx, &wg, 1, 1)
	release := make(chan struct{})
	var runs atomic.Int32
	block := func(context.Context) { runs.Add(1); <-release }
	if !q.submit("a", block) {
		t.Fatal("first submit refused")
	}
	if q.submit("a", block) {
		t.Fatal("duplicate key queued")
	}
	// The worker holds "a"; "b" fills the one-slot queue, "c" is dropped.
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !q.submit("b", func(context.Context) { runs.Add(1) }) {
		t.Fatal("b should fit the queue")
	}
	if q.submit("c", func(context.Context) {}) {
		t.Fatal("c should be dropped when the queue is full")
	}
	close(release)
	q.wait()
	if runs.Load() != 2 {
		t.Fatalf("runs = %d, want 2", runs.Load())
	}
	// A panicking job doesn't kill the worker.
	q.submit("p", func(context.Context) { panic("boom") })
	q.wait()
	done := make(chan struct{})
	q.submit("d", func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker died after a panic")
	}
	cancel()
	q.wait() // must not hang after shutdown
	wg.Wait()
	if q.submit("e", func(context.Context) {}) {
		t.Fatal("submit after shutdown")
	}
}

func TestUltraBlurFromImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	fill := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, c)
			}
		}
	}
	fill(0, 0, 20, 10, color.RGBA{255, 0, 0, 255})       // top left: red
	fill(20, 0, 40, 10, color.RGBA{0, 0, 255, 255})      // top right: blue
	fill(20, 10, 40, 20, color.RGBA{128, 128, 128, 255}) // bottom right: grey
	fill(0, 10, 20, 20, color.RGBA{255, 255, 255, 255})  // bottom left: white
	u, err := ultraBlurFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	rgb := func(hex string) (int, int, int) {
		var r, g, b int
		if _, err := fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b); err != nil || len(hex) != 6 {
			t.Fatalf("bad hex %q", hex)
		}
		return r, g, b
	}
	if r, g, b := rgb(u.TopLeft); !(r > g && r > b && r < 160) {
		t.Errorf("top left should be a dark red, got %s", u.TopLeft)
	}
	if r, g, b := rgb(u.TopRight); !(b > r && b > g && b < 160) {
		t.Errorf("top right should be a dark blue, got %s", u.TopRight)
	}
	if r, g, b := rgb(u.BottomRight); !(r == g && g == b) {
		t.Errorf("grey stays neutral, got %s", u.BottomRight)
	}
	if r, _, _ := rgb(u.BottomLeft); r > 125 {
		t.Errorf("white is darkened into the UltraBlur range, got %s", u.BottomLeft)
	}
	if _, err := ultraBlurFromImage(image.NewRGBA(image.Rect(0, 0, 1, 1))); err == nil {
		t.Error("1x1 image should be rejected")
	}
}

// TestUltraBlurComputedLazilyAndPersisted: the first listing has no colours
// and doesn't wait; once computed they appear; a new server on the same
// database serves them without refetching the backdrop.
func TestUltraBlurComputedLazilyAndPersisted(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := newFixture()
	f.srv.Close()
	d := f.srv.d
	d.DB = db
	f.srv = NewServer(testDeps(d))

	first := f.get(t, "/library/metadata/1000000000000550", jsonAccept...)
	if strings.Contains(first.Body.String(), "UltraBlurColors") {
		t.Fatal("colours must not be computed inline")
	}
	f.srv.waitBackground()
	second := f.get(t, "/library/metadata/1000000000000550", jsonAccept...)
	var v struct {
		MediaContainer struct {
			Metadata []struct {
				UltraBlurColors *model.UltraBlurColors
			}
		}
	}
	if err := json.Unmarshal(second.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	u := v.MediaContainer.Metadata[0].UltraBlurColors
	if u == nil || len(u.TopLeft) != 6 || len(u.BottomLeft) != 6 {
		t.Fatalf("UltraBlurColors object expected: %s", second.Body.String())
	}
	fetched := f.art.count("/t/p/w300/backdrop550.jpg")
	if fetched != 1 {
		t.Fatalf("backdrop fetched %d times, want 1", fetched)
	}
	f.srv.Close()

	// Restart: the colours come from plex_ultrablur on the first render.
	f.srv = NewServer(testDeps(d))
	defer f.srv.Close()
	rec := f.get(t, "/library/sections/1/all", jsonAccept...)
	if !strings.Contains(rec.Body.String(), `"topLeft":"`+u.TopLeft+`"`) {
		t.Fatalf("persisted colours missing after restart: %s", rec.Body.String())
	}
	if n := f.art.count("/t/p/w300/backdrop550.jpg"); n != fetched {
		t.Fatalf("backdrop refetched after restart (%d)", n)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM plex_ultrablur WHERE key = ?`, int64(fightClub.Key)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("plex_ultrablur rows = %d, %v", n, err)
	}
}

// blockingCDN never answers until the request's context ends.
type blockingCDN struct{ started chan struct{} }

func (b *blockingCDN) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestCloseDoesNotWaitForSlowArtwork(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	cdn := &blockingCDN{started: make(chan struct{}, 1)}
	d := f.srv.d
	setDepsHTTPClient(&d, &http.Client{Transport: cdn})
	f.srv = NewServer(testDeps(d))
	f.get(t, "/library/metadata/1000000000000550")
	select {
	case <-cdn.started:
	case <-time.After(5 * time.Second):
		t.Fatal("UltraBlur fetch never started")
	}
	done := make(chan struct{})
	go func() { f.srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an in-flight artwork fetch")
	}
}

func TestUltraBlurFailureIsRetriedLater(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	missing := fightClub
	missing.BackdropPath = "/missing.jpg"
	f.catalog.titles[missing.Key] = missing
	for i := 0; i < 3; i++ {
		rec := f.get(t, "/library/metadata/1000000000000550")
		if strings.Contains(rec.Body.String(), "UltraBlurColors") {
			t.Fatal("no colours for a missing backdrop")
		}
		f.srv.waitBackground()
	}
	if n := f.art.count("/t/p/w300/missing.jpg"); n != 1 {
		t.Fatalf("failed backdrop fetched %d times within the retry window, want 1", n)
	}
}

// listCatalog is a catalogue whose lists are summaries (no credits) while
// Get returns the detail, like TMDB list endpoints vs. details.
type listCatalog struct {
	*fakeCatalog
	mu   sync.Mutex
	gets map[core.RatingKey]int
	fail bool
}

func (c *listCatalog) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	c.mu.Lock()
	c.gets[k]++
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return nil, errors.New("tmdb down")
	}
	return c.fakeCatalog.Get(ctx, k)
}

func (c *listCatalog) count(k core.RatingKey) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets[k]
}

func TestHubItemsEnrichedFromDetail(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	lc := &listCatalog{fakeCatalog: f.catalog, gets: map[core.RatingKey]int{}}
	d := f.srv.d
	d.Catalog = lc
	summary := fightClub
	summary.Cast, summary.Directors, summary.Writers, summary.Studios, summary.ContentRating, summary.Tagline = nil, nil, nil, nil, "", ""
	d.Recommender = &fakeRecommender{home: []core.Hub{{ID: "trending", Title: "Trending", Type: core.TypeMovie, Items: []core.Title{summary}}}}
	f.srv = NewServer(testDeps(d))
	defer f.srv.Close()

	rec := f.get(t, "/hubs")
	body := rec.Body.String()
	if strings.Contains(body, "Theo Varga") || strings.Contains(body, `studio="`) {
		t.Fatalf("first render must not block on detail: %s", body)
	}
	f.srv.waitBackground()
	body = f.get(t, "/hubs").Body.String()
	for _, want := range []string{`studio="Northlight Pictures"`, `contentRating="R"`, `tagline="Quiet waters run deep."`, `<Director tag="Mara Ellison" />`, `<Role tag="Theo Varga" />`} {
		if !strings.Contains(body, want) {
			t.Errorf("enriched hub lacks %s:\n%s", want, body)
		}
	}
	f.get(t, "/hubs")
	f.srv.waitBackground()
	if n := lc.count(fightClub.Key); n != 1 {
		t.Fatalf("detail fetched %d times, want 1 (cached)", n)
	}
}

func TestEnrichFailureKeepsSummaryAndRetriesLater(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	lc := &listCatalog{fakeCatalog: f.catalog, gets: map[core.RatingKey]int{}, fail: true}
	d := f.srv.d
	d.Catalog = lc
	summary := alien
	d.Recommender = &fakeRecommender{home: []core.Hub{{ID: "x", Title: "X", Type: core.TypeMovie, Items: []core.Title{summary}}}}
	now := fixedNow
	var mu sync.Mutex
	setDepsNow(&d, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	f.srv = NewServer(testDeps(d))
	defer f.srv.Close()
	for i := 0; i < 3; i++ {
		if rec := f.get(t, "/hubs"); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		f.srv.waitBackground()
	}
	if n := lc.count(alien.Key); n != 1 {
		t.Fatalf("failed fetch retried %d times inside the retry window", n)
	}
	mu.Lock()
	now = now.Add(titleCacheRetry + time.Minute)
	mu.Unlock()
	f.get(t, "/hubs")
	f.srv.waitBackground()
	if n := lc.count(alien.Key); n != 2 {
		t.Fatalf("fetch not retried after the window: %d", n)
	}
}

func TestEpisodeImagesUseShowArtwork(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	show := breakingBad
	show.LogoPath = "/bblogo.png"
	show.Studios = []string{"Sony Pictures Television"}
	show.ContentRating = "TV-MA"
	f.catalog.titles[show.Key] = show
	target := "/library/metadata/" + bbS1.Key.String() + "/children"
	body := f.getWarm(t, target).Body.String()
	for _, want := range []string{
		`grandparentContentRating="TV-MA"`, `grandparentStudio="Sony Pictures Television"`, `parentTitle=""`,
		`<Image alt="Pilot" type="coverPoster" url="/library/metadata/2000000000001396/thumb/`,
		`<Image alt="Pilot" type="snapshot" url="/library/metadata/4000013960010001/thumb/`,
		`<Image alt="Pilot" type="background" url="/library/metadata/2000000000001396/art/`,
		`<Image alt="Pilot" type="clearLogo" url="/library/metadata/2000000000001396/clearLogo/`,
		`grandparentSlug="boundary-bay"`, `contentRating="TV-MA"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("season children lack %s:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{`<Genre`, ` year=`, `titleSort=`, `UltraBlurColors`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("episodes must not carry %s:\n%s", unwanted, body)
		}
	}
	seasons := f.getWarm(t, "/library/metadata/"+show.Key.String()+"/children").Body.String()
	for _, want := range []string{`parentSlug="boundary-bay"`, `parentStudio="Sony Pictures Television"`, `parentYear="2008"`, `type="clearLogo"`} {
		if !strings.Contains(seasons, want) {
			t.Errorf("seasons lack %s:\n%s", want, seasons)
		}
	}
}

func TestSectionPrefsAndEmptyHubSearch(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/sections/2/prefs", jsonAccept...)
	var v struct {
		MediaContainer struct {
			Size    int
			Setting []map[string]any
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	ids := map[string]map[string]any{}
	for _, s := range v.MediaContainer.Setting {
		ids[s["id"].(string)] = s
	}
	for _, id := range []string{"hidden", "episodeSort", "showOrdering", "flattenSeasons", "collectionMode"} {
		s, ok := ids[id]
		if !ok {
			t.Fatalf("show prefs lack %s: %s", id, rec.Body.String())
		}
		if _, ok := s["value"].(string); !ok {
			t.Errorf("%s value must be a string like PMS: %#v", id, s["value"])
		}
	}
	if v.MediaContainer.Size != len(v.MediaContainer.Setting) {
		t.Errorf("size %d for %d settings", v.MediaContainer.Size, len(v.MediaContainer.Setting))
	}
	movie := f.get(t, "/library/sections/1/prefs").Body.String()
	if !strings.Contains(movie, `id="enableCinemaTrailers"`) || strings.Contains(movie, "episodeSort") {
		t.Errorf("movie prefs: %s", movie)
	}
	rec = f.get(t, "/hubs/search?query=")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "400 Bad Request") {
		t.Fatalf("empty hub search: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRelatedHubsOmitSectionAndPromoted(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Recommender.(*fakeRecommender).related = []core.Hub{{ID: "similar", Title: "More Like This", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}}}
	body := f.get(t, "/library/metadata/1000000000000550?includeRelated=1").Body.String()
	i := strings.Index(body, "<Related>")
	if i < 0 {
		t.Fatalf("no Related: %s", body)
	}
	rel := body[i:]
	if strings.Contains(rel, "librarySectionID") || strings.Contains(rel, `promoted=`) {
		t.Errorf("related hub items must omit librarySection* and promoted: %s", rel)
	}
}

func TestMDEDecision(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
	base := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&protocol=hls&directStream=1&session=mde1"
	body := f.get(t, base+"&hasMDE=1&directPlay=1").Body.String()
	for _, want := range []string{`mdeDecisionCode="1000"`, `mdeDecisionText="Direct play OK."`, `decision="directplay"`, `location="direct"`} {
		if !strings.Contains(body, want) {
			t.Errorf("MDE direct decision lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "generalDecisionCode") {
		t.Errorf("MDE direct decision carries general codes:\n%s", body)
	}
	// The stored decision is Ether's own: a stream request transcodes.
	if e, ok := f.srv.transcodes.get("mde1"); !ok || e.decision.Kind != core.DecisionTranscode {
		t.Fatalf("stored decision: %+v", e)
	}
	body = f.get(t, base+"&hasMDE=1&directPlay=0").Body.String()
	if !strings.Contains(body, `generalDecisionCode="1001"`) || !strings.Contains(body, `decision="transcode"`) {
		t.Errorf("MDE stream decision:\n%s", body)
	}
	f.playback.failures[fightClub.Key] = errWrap("resolve", errNotAvailable)
	body = f.get(t, base+"&hasMDE=1&directPlay=1&session=mde2").Body.String()
	if !strings.Contains(body, `mdeDecisionCode="2000"`) || !strings.Contains(body, `generalDecisionCode="2000"`) {
		t.Errorf("MDE failure must carry mde and general error codes:\n%s", body)
	}
}

func TestStartHLSRedecidesForMDEStream(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// Direct play for the decision (as an MDE client is told), then the
	// client asks for a stream without direct play.
	f.get(t, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&hasMDE=1&directPlay=1&session=hls1")
	f.trans.mu.Lock()
	f.trans.decision = core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
	f.trans.mu.Unlock()
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&directPlay=0&directStream=1&session=hls1")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	e, ok := f.srv.transcodes.get("hls1")
	if !ok || e.decision.Kind != core.DecisionTranscode {
		t.Fatalf("start.m3u8 should re-decide without direct play: %+v", e.decision)
	}
}

// Usenet decisions prefer a remux; compatible HTTP and local files retain
// direct play. A debrid file needing conversion must not be reported as copy.
func TestMDEOnDemandPrefersCopyRemux(t *testing.T) {
	for _, tc := range []struct {
		name, id, protocol string
		requireRemux       bool
		convert            bool
		want               core.DecisionKind
	}{
		{name: "usenet", id: "usenet-release", want: core.DecisionRemux},
		{name: "local", id: "local:550", want: core.DecisionDirectPlay},
		{name: "debrid", id: "debrid-release", protocol: core.ProtocolTorrent, want: core.DecisionDirectPlay},
		{name: "debrid failover", id: "debrid-release", protocol: core.ProtocolTorrent, requireRemux: true, want: core.DecisionRemux},
		{name: "debrid conversion", id: "debrid-release", protocol: core.ProtocolTorrent, convert: true, want: core.DecisionTranscode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			user := &core.User{ID: 1, IsOwner: true}
			caps := core.ClientCaps{ClientID: "remux-client"}
			ps, err := f.playback.Prepare(context.Background(), user, fightClub.Key, caps)
			if err != nil {
				t.Fatal(err)
			}
			ps.Resolution = &core.Resolution{Release: core.Release{ID: tc.id, Protocol: tc.protocol}}
			ps.RequiresRemux = tc.requireRemux
			if tc.convert {
				f.trans.decision = core.Decision{Kind: core.DecisionTranscode, Container: "mpegts", VideoCodec: "h264", AudioCodec: "aac"}
			}
			pr := &Request{User: user, ClientID: caps.ClientID}
			p := playRequest{key: fightClub.Key, caps: caps, mde: true, sessionID: "copy-remux", protocol: "hls", opts: core.DecideOptions{DirectPlay: true, DirectStream: true}}
			title, session, dec, err := f.srv.prepare(context.Background(), pr, &p)
			if err != nil {
				t.Fatal(err)
			}
			if dec.Kind != tc.want || (!tc.convert && (!dec.VideoCopy || !dec.AudioCopy)) {
				t.Fatalf("decision: %+v", dec)
			}
			mc := f.srv.decisionContainer(context.Background(), pr, title, session, dec, p)
			part := mc.Metadata[0].Media[0].Part[0]
			if got := part.Decision == "directplay"; got != (tc.want == core.DecisionDirectPlay) {
				t.Fatalf("MDE response overrides decision: %s", part.Decision)
			}
		})
	}
}

func TestHEVCDecisionReportsMP4ForFragmentedHLS(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	info := f.playback.info
	info.Streams = append([]core.StreamInfo(nil), info.Streams...)
	info.Container = "mkv"
	for i := range info.Streams {
		if info.Streams[i].Type == "video" {
			info.Streams[i].Codec = "hevc"
			info.Streams[i].BitDepth = 10
		}
	}
	ps := &core.PlaySession{Info: info}
	m := f.srv.decisionMedia(&fightClub, ps, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}, playRequest{protocol: "hls"}, &ownerUser)
	if m.Container != "mp4" || len(m.Part) != 1 || m.Part[0].Container != "mp4" {
		t.Fatalf("HEVC HLS advertised as MPEG-TS: %+v", m)
	}
}

// Plex Web decides with its own engine (hasMDE=1) from metadata that, for
// an on-demand title not yet probed, is an estimate: it saw AAC, the file
// has AC3. Ether's own decision must convert the audio, so the answer is
// not "Direct play OK", even with directStream=0 (#246). A compatible
// file is still direct play.
func TestMDEOnDemandIncompatibleAudioIsNotDirectPlay(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  core.Decision
		want bool // answered as MDE direct play
	}{
		{"audio conversion", core.Decision{Kind: core.DecisionTranscode, VideoCopy: true, AudioCopy: false, Container: "mp4", AudioCodec: "aac"}, false},
		{"full conversion", core.Decision{Kind: core.DecisionTranscode, Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}, false},
		{"compatible", core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			user := &core.User{ID: 1, IsOwner: true}
			caps := core.ClientCaps{ClientID: "web-mde", Product: "Plex Web", Platform: "Chrome"}
			ps, err := f.playback.Prepare(context.Background(), user, fightClub.Key, caps)
			if err != nil {
				t.Fatal(err)
			}
			ps.Resolution = &core.Resolution{Release: core.Release{ID: "usenet-release"}}
			pr := &Request{User: user, ClientID: caps.ClientID}
			p := playRequest{key: fightClub.Key, caps: caps, mde: true, sessionID: "web-mde", protocol: "dash", opts: core.DecideOptions{DirectPlay: true, DirectStream: false}}
			mc := f.srv.decisionContainer(context.Background(), pr, &fightClub, ps, tc.dec, p)
			part := mc.Metadata[0].Media[0].Part[0]
			if got := part.Decision == "directplay"; got != tc.want {
				t.Fatalf("part decision %q, want direct play %v", part.Decision, tc.want)
			}
			mde := false
			for _, a := range mc.Extra {
				if a.Name == "mdeDecisionCode" && fmt.Sprint(a.Value) == "1000" {
					mde = true
				}
			}
			if mde != tc.want {
				t.Fatalf("mdeDecisionCode 1000 = %v, want %v", mde, tc.want)
			}
		})
	}
}
