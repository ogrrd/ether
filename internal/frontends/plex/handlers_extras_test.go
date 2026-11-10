package plex

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// fakeExtrasProvider is an in-memory ExtrasProvider for front-end tests.
type fakeExtrasProvider struct {
	enabled  bool
	byParent map[core.RatingKey][]frontend.ExtraClip
	byKey    map[core.RatingKey]frontend.ExtraClip
	resolved frontend.ResolvedExtra
}

func newFakeExtras(list ...frontend.ExtraClip) *fakeExtrasProvider {
	f := &fakeExtrasProvider{enabled: true, byParent: map[core.RatingKey][]frontend.ExtraClip{}, byKey: map[core.RatingKey]frontend.ExtraClip{}}
	for _, e := range list {
		f.byParent[e.ParentKey] = append(f.byParent[e.ParentKey], e)
		f.byKey[e.Key] = e
	}
	return f
}

func (f *fakeExtrasProvider) Enabled() bool { return f.enabled }
func (f *fakeExtrasProvider) List(_ context.Context, parent *core.Title, _ string) ([]frontend.ExtraClip, error) {
	if !f.enabled {
		return nil, nil
	}
	return append([]frontend.ExtraClip(nil), f.byParent[parent.Key]...), nil
}
func (f *fakeExtrasProvider) Lookup(k core.RatingKey) (frontend.ExtraClip, bool) {
	e, ok := f.byKey[k]
	return e, ok
}
func (f *fakeExtrasProvider) Resolve(_ context.Context, _ frontend.ExtraClip) (frontend.ResolvedExtra, error) {
	return f.resolved, nil
}
func (f *fakeExtrasProvider) PrewarmBytes() int64 { return 48 << 20 }

type prewarmCall struct {
	userID int64
	key    core.RatingKey
	opts   core.PrewarmOptions
}

type warmingPlayback struct {
	*fakePlayback
	mu    sync.Mutex
	calls []prewarmCall
}

func (p *warmingPlayback) Prewarm(_ context.Context, u *core.User, k core.RatingKey, _ core.ClientCaps, o core.PrewarmOptions) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, prewarmCall{userID: u.ID, key: k, opts: o})
	return true
}

func (p *warmingPlayback) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

var (
	fightClubTrailer = frontend.ExtraClip{
		Key: core.ExtraKey(101), ParentKey: fightClub.Key, ParentTitle: fightClub.Title,
		Title: "Fjord Harbor Trailer", Subtype: "trailer", ExtraType: 1,
		Site: "YouTube", SiteKey: "vKQi3bBA1y8", Language: "en", Official: true,
		Size: 1080, Duration: 2 * time.Minute, Source: frontend.ExtraSourceCommand,
	}
	fightClubFeaturette = frontend.ExtraClip{
		Key: core.ExtraKey(102), ParentKey: fightClub.Key, ParentTitle: fightClub.Title,
		Title: "Making Fjord Harbor", Subtype: "featurette", ExtraType: 6,
		Site: "Vimeo", SiteKey: "123456789", Language: "en", Source: frontend.ExtraSourceCommand,
	}
	alienTrailer = frontend.ExtraClip{
		Key: core.ExtraKey(103), ParentKey: alien.Key, ParentTitle: alien.Title,
		Title: "Alien Trailer", Subtype: "trailer", ExtraType: 1,
		Site: "YouTube", SiteKey: "LjLamj-b0I8", Language: "en", Official: true,
		Size: 720, Duration: time.Minute, Source: frontend.ExtraSourceCommand,
	}
)

func extrasFixture(t *testing.T) (*fixture, *fakeExtrasProvider, *warmingPlayback) {
	t.Helper()
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	ex := newFakeExtras(fightClubTrailer, fightClubFeaturette, alienTrailer)
	ex.resolved = frontend.ResolvedExtra{URL: "https://cdn.example/trailer.mp4", Container: "mp4"}
	wp := &warmingPlayback{fakePlayback: f.playback}
	f.srv.d.Extras = ex
	f.srv.d.Playback = wp
	return f, ex, wp
}

func TestExtrasListAndDetailXML(t *testing.T) {
	f, ex, _ := extrasFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/extras")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("extras: %d %s", rec.Code, body)
	}
	for _, want := range []string{
		`type="clip"`, `subtype="trailer"`, `extraType="1"`, `subtype="featurette"`, `extraType="6"`,
		`ratingKey="` + fightClubTrailer.Key.String() + `"`, "<Media", "<Part",
		`parentRatingKey="` + fightClub.Key.String() + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("extras body missing %q\n%s", want, body)
		}
	}
	// Detail page embeds the same extras and a primary extra key.
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String()+"?includeExtras=1")
	body = rec.Body.String()
	if !strings.Contains(body, `<Extras size="2">`) || !strings.Contains(body, `primaryExtraKey="/library/metadata/`+fightClubTrailer.Key.String()+`"`) {
		t.Fatalf("detail extras\n%s", body)
	}
	// extraTypes filter.
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String()+"?includeExtras=1&extraTypes=1")
	if body = rec.Body.String(); !strings.Contains(body, `<Extras size="1">`) || strings.Contains(body, "Making Fjord Harbor") {
		t.Fatalf("extraTypes filter\n%s", body)
	}
	// The extra itself resolves as metadata.
	rec = f.get(t, "/library/metadata/"+fightClubTrailer.Key.String())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `subtype="trailer"`) {
		t.Fatalf("extra metadata: %d %s", rec.Code, rec.Body.String())
	}
	// Hidden when no source is configured.
	ex.enabled = false
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String()+"?includeExtras=1")
	if !strings.Contains(rec.Body.String(), `<Extras size="0"`) {
		t.Fatalf("disabled extras advertised\n%s", rec.Body.String())
	}
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String()+"/extras")
	if strings.Contains(rec.Body.String(), "Fjord Harbor Trailer") {
		t.Fatalf("disabled extras listed\n%s", rec.Body.String())
	}
}

func TestExtrasRespectRestrictions(t *testing.T) {
	f, _, _ := extrasFixture(t)
	f.srv.testAuth().users["kid"] = core.User{ID: 3, Username: "kid", AllowedLibraries: []core.LibraryID{2}}
	rec := f.request(t, http.MethodGet, "/library/metadata/"+fightClub.Key.String()+"/extras", map[string]string{"X-Plex-Token": "kid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("restricted extras: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.request(t, http.MethodGet, "/library/metadata/"+fightClubTrailer.Key.String(), map[string]string{"X-Plex-Token": "kid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("restricted extra: %d", rec.Code)
	}
}

func TestExtraPlaybackDecisionStartPrewarm(t *testing.T) {
	f, ex, wp := extrasFixture(t)
	path := "/library/metadata/" + fightClubTrailer.Key.String()
	rec := f.get(t, "/video/:/transcode/universal/decision?path="+urlEsc(path)+"&session=ex1&protocol=hls&directPlay=1&directStream=1")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "directPlayDecisionCode") || !strings.Contains(body, `ratingKey="`+fightClubTrailer.Key.String()+`"`) {
		t.Fatalf("decision: %d\n%s", rec.Code, body)
	}
	if wp.count() != 1 {
		t.Fatalf("prewarm calls %d", wp.count())
	}
	call := wp.calls[0]
	if call.key != fightClub.Key || call.opts.Reason != core.PrewarmTrailer || call.opts.PrefetchBytes != 48<<20 || !call.opts.DuringPlayback {
		t.Fatalf("prewarm %+v", call)
	}
	// HLS start reuses the decision session and feeds ffmpeg the
	// resolved URL, not the Usenet playback manager.
	rec = f.get(t, "/video/:/transcode/universal/start.m3u8?path="+urlEsc(path)+"&session=ex1&protocol=hls&directPlay=0&directStream=1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "session/ex1/base/index.m3u8") {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	f.trans.mu.Lock()
	hs := f.trans.sessions["ex1"]
	f.trans.mu.Unlock()
	if hs == nil || hs.req.InputURL != ex.resolved.URL {
		t.Fatalf("transcode input %+v", hs)
	}
	if f.playback.prepared != 0 {
		t.Fatalf("extra went through Playback.Prepare %d times", f.playback.prepared)
	}
	if wp.count() != 1 {
		t.Fatalf("prewarm not deduplicated: %d", wp.count())
	}
	// Segments flow through the existing transcode session route.
	rec = f.get(t, "/video/:/transcode/universal/session/ex1/base/00000.ts")
	if rec.Code != http.StatusOK || rec.Body.String() != "segment-0" {
		t.Fatalf("segment: %d %q", rec.Code, rec.Body.String())
	}
	// Stop cleans the session up like other transcodes.
	_ = f.request(t, http.MethodGet, "/video/:/transcode/universal/stop?session=ex1", nil)
	f.trans.mu.Lock()
	stopped := append([]string(nil), f.trans.stopped...)
	f.trans.mu.Unlock()
	if len(stopped) == 0 || stopped[len(stopped)-1] != "ex1" {
		t.Fatalf("stop: %v", stopped)
	}
	// Unknown extra keys 404 at the hook, never reach prepare.
	rec = f.get(t, "/video/:/transcode/universal/decision?path="+urlEsc("/library/metadata/"+core.ExtraKey(999).String())+"&session=ex2")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown extra: %d", rec.Code)
	}
}

func TestExtraStartHTTP(t *testing.T) {
	f, ex, _ := extrasFixture(t)
	path := "/library/metadata/" + fightClubTrailer.Key.String()
	rec := f.get(t, "/video/:/transcode/universal/start?path="+urlEsc(path)+"&session=ex-http&protocol=http&directPlay=0&directStream=1")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/x-matroska" || !strings.Contains(rec.Body.String(), "mkv-ex-http") {
		t.Fatalf("http start: %d %s", rec.Code, rec.Body.String())
	}
	f.trans.mu.Lock()
	hs := f.trans.sessions["ex-http"]
	f.trans.mu.Unlock()
	if hs == nil || hs.req.Protocol != core.ProtocolHTTP || hs.req.InputURL != ex.resolved.URL {
		t.Fatalf("http extra session %+v", hs)
	}
}

func TestExtraPartProxyRange(t *testing.T) {
	f, ex, _ := extrasFixture(t)
	data := []byte("0123456789abcdef")
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "trailer.mp4", fixedNow, bytes.NewReader(data))
	}))
	defer media.Close()
	ex.resolved = frontend.ResolvedExtra{URL: media.URL + "/trailer.mp4", Container: "mp4"}
	rec := f.request(t, http.MethodGet, "/library/parts/"+fightClubTrailer.Key.String()+"/1/file.mp4", map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" {
		t.Fatalf("range proxy: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") == "" {
		t.Fatalf("no content-range: %v", rec.Header())
	}
}

func TestCinemaTrailersPlayQueue(t *testing.T) {
	f, _, wp := extrasFixture(t)
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String()
	rec := f.request(t, http.MethodPost,
		"/playQueues?type=video&extrasPrefixCount=1&uri="+urlEsc(uri)+"&key="+urlEsc("/library/metadata/"+fightClub.Key.String()),
		map[string]string{"Accept": "application/json"})
	mc := decodeJSON(t, rec)
	if mc["playQueueTotalCount"].(float64) != 2 {
		t.Fatalf("cinema queue count: %v", mc)
	}
	if mc["playQueueSelectedMetadataItemID"] != alienTrailer.Key.String() {
		t.Fatalf("selected should be the trailer: %v", mc["playQueueSelectedMetadataItemID"])
	}
	items := mc["Metadata"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["ratingKey"] != alienTrailer.Key.String() ||
		items[1].(map[string]any)["ratingKey"] != fightClub.Key.String() {
		t.Fatalf("queue order: %v", items)
	}
	if items[0].(map[string]any)["type"] != "clip" {
		t.Fatalf("trailer item type: %v", items[0])
	}
	// Creating the queue already warms the feature.
	if wp.count() != 1 || wp.calls[0].key != fightClub.Key || wp.calls[0].opts.Reason != core.PrewarmTrailer {
		t.Fatalf("queue prewarm: %+v", wp.calls)
	}
	// Without extrasPrefixCount the queue is just the feature.
	rec = f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"})
	mc = decodeJSON(t, rec)
	if mc["playQueueTotalCount"].(float64) != 1 {
		t.Fatalf("plain queue: %v", mc)
	}
}
