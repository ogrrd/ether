package plex

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// fakeVariantResolver serves a fixed variant list per title.
type fakeVariantResolver struct {
	mu    sync.Mutex
	byKey map[core.RatingKey][]core.ReleaseVariant
	calls int
	// lastMax is the cap the server asked for on the latest call.
	lastMax int
	// lastPlayed is the latest call's ResolveRequest.PlayedRelease.
	lastPlayed string
}

func (r *fakeVariantResolver) Variants(_ context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastMax = max
	r.lastPlayed = req.PlayedRelease
	vs := r.byKey[req.Title.Key]
	if max > 0 && len(vs) > max {
		vs = vs[:max]
	}
	return append([]core.ReleaseVariant(nil), vs...), nil
}

// fakeVariantPlayback adds PrepareVariant to the shared fake playback.
type fakeVariantPlayback struct {
	*fakePlayback
	variantCalls []string
	resumed      map[resumedFactKey]resumedFact
}

func (p *fakeVariantPlayback) PrepareVariant(_ context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps, releaseID string) (*core.PlaySession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.variantCalls = append(p.variantCalls, releaseID)
	id := fmt.Sprintf("psv-%d-%s-%d-%s", u.ID, caps.ClientID, k, releaseID)
	if ps, ok := p.sessions[id]; ok {
		return ps, nil
	}
	ps := &core.PlaySession{
		ID: id, UserID: u.ID, ClientID: caps.ClientID, Key: k, Info: p.info, StartedAt: fixedNow,
		Resolution: &core.Resolution{Title: k, Release: core.Release{ID: releaseID}},
	}
	p.sessions[id] = ps
	if u != nil && u.ID > 0 && releaseID != "" && !p.info.Estimated && len(p.info.Streams) > 0 {
		if p.resumed == nil {
			p.resumed = map[resumedFactKey]resumedFact{}
		}
		cp := p.info
		cp.Streams = append([]core.StreamInfo(nil), p.info.Streams...)
		p.resumed[resumedFactKey{u.ID, k}] = resumedFact{info: cp, release: releaseID}
	}
	return ps, nil
}

// resumedFact is the test double for playback.Manager.ResumedTracks: one
// probe per viewer and title. It does not apply profile, source, or block
// policy; the playback package tests those gates.
type resumedFactKey struct {
	user int64
	key  core.RatingKey
}

type resumedFact struct {
	info    core.MediaInfo
	release string
}

func (p *fakeVariantPlayback) ResumedTracks(_ context.Context, user *core.User, key core.RatingKey, _ core.ClientCaps) (core.MediaInfo, string, bool) {
	if user == nil || user.ID <= 0 {
		return core.MediaInfo{}, "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.resumed[resumedFactKey{user.ID, key}]
	if !ok || e.release == "" || e.info.Estimated || len(e.info.Streams) == 0 {
		return core.MediaInfo{}, "", false
	}
	return e.info, e.release, true
}

func mkVariant(id, label, edition string, auto bool, height int, vcodec, acodec string, dv bool, kbps int) core.ReleaseVariant {
	width := map[int]int{2160: 3840, 1080: 1920, 720: 1280}[height]
	video := core.StreamInfo{Index: 0, Type: "video", Codec: vcodec, Width: width, Height: height, FrameRate: 23.976, Default: true, DolbyVision: dv}
	if dv {
		video.ColorTransfer = "smpte2084"
		video.BitDepth = 10
	}
	info := core.MediaInfo{
		Container: "mkv", Duration: 139 * time.Minute, Size: 8_000_000_000, BitrateKbps: kbps, Estimated: true,
		Streams: []core.StreamInfo{video, {Index: 1, Type: "audio", Codec: acodec, Language: "en", Channels: 6, SampleRate: 48000, Default: true}},
	}
	return core.ReleaseVariant{ID: id, Label: label, Edition: edition, Auto: auto, Release: core.Release{ID: id}, Info: info}
}

func testVariantLists() map[core.RatingKey][]core.ReleaseVariant {
	return map[core.RatingKey][]core.ReleaseVariant{
		fightClub.Key: {
			mkVariant("rel-auto", "Auto / Best for Device", "", true, 1080, "h264", "eac3", false, 7600),
			mkVariant("rel-4k", "4K Dolby Vision Remux", "", false, 2160, "hevc", "truehd", true, 45000),
			mkVariant("rel-compat", "720p Blu-ray (High Compatibility)", "", false, 720, "h264", "aac", false, 4000),
		},
		alien.Key: {
			mkVariant("rel-dc", "Auto / Best for Device", "Director's Cut", true, 1080, "h264", "ac3", false, 8000),
			mkVariant("rel-alien4k", "4K HDR10 Blu-ray", "", false, 2160, "hevc", "dtshd", false, 30000),
		},
	}
}

type variantFixture struct {
	srv      *Server
	resolver *fakeVariantResolver
	playback core.Playback
}

func newVariantFixture(t *testing.T, playback core.Playback) *variantFixture {
	t.Helper()
	cat := &fakeCatalog{
		titles:   map[core.RatingKey]core.Title{fightClub.Key: fightClub, alien.Key: alien},
		children: map[core.RatingKey][]core.RatingKey{},
		facets:   map[string][]core.FacetValue{},
	}
	vr := &fakeVariantResolver{byKey: testVariantLists()}
	d := Deps{
		Catalog:           cat,
		State:             newFakeState(),
		Playback:          playback,
		Transcoder:        newFakeTranscoder(core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}),
		Variants:          vr,
		DisableEnrichment: true,
	}
	setDepsAuth(&d, &fakeAuth{users: map[string]core.User{ownerToken: ownerUser, friendToken: friendUser}})
	setDepsConfig(&d, func() config.Config {
		c := config.Config{}
		c.Server.Port = 32400
		c.Server.FriendlyName = "Ether Test"
		c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
		c.Transcode.RemoteMaxBitrateKbps = 8000
		return c
	})
	setDepsIdentity(&d, Identity{Version: "1.43.4.10903-e5521bd8c", Platform: "Linux"})
	setDepsNow(&d, func() time.Time { return fixedNow })
	srv := NewServer(testDeps(d))
	t.Cleanup(func() { srv.Close() })
	mountTestBroker(srv, func() time.Time { return fixedNow })
	return &variantFixture{srv: srv, resolver: vr, playback: playback}
}

func newVariantPlaybackFixture(t *testing.T) (*variantFixture, *fakeVariantPlayback) {
	t.Helper()
	pb := &fakeVariantPlayback{fakePlayback: newFakePlayback(sampleInfo)}
	return newVariantFixture(t, pb), pb
}

func (f *variantFixture) get(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.168.1.50:51000"
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

var variantPartRE = regexp.MustCompile(`/library/parts/(9[0-9]{15})/[0-9]+/file\.mkv`)

func TestDetailRendersPlayVersionMedia(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, "<Media "); n != 3 {
		t.Fatalf("media count %d, want 3\n%s", n, body)
	}
	// Auto keeps the historical ids and part key.
	autoID := fightClub.Key.String()
	if !strings.Contains(body, `id="`+autoID+`"`) || !strings.Contains(body, "/library/parts/"+autoID+"/") {
		t.Errorf("auto ids missing\n%s", body)
	}
	// Nothing is resumed, so Auto is the selected Media. A client that
	// honors selected starts there.
	mediaTags := regexp.MustCompile(`<Media\b[^>]*>`).FindAllString(body, -1)
	selectedMedia := 0
	selectedAuto := false
	for _, tag := range mediaTags {
		if !strings.Contains(tag, `selected="1"`) {
			continue
		}
		selectedMedia++
		if strings.Contains(tag, `id="`+autoID+`"`) {
			selectedAuto = true
		}
	}
	if selectedMedia != 1 || !selectedAuto {
		t.Fatalf("selected media=%d auto=%v\n%s", selectedMedia, selectedAuto, body)
	}
	// The picker labels.
	if !strings.Contains(body, `title="Auto / Best for Device"`) ||
		!strings.Contains(body, `title="4K Dolby Vision Remux"`) ||
		!strings.Contains(body, "High Compatibility") {
		t.Errorf("variant labels missing\n%s", body)
	}
	// Per-variant quality attributes.
	if !strings.Contains(body, `videoResolution="4k"`) || !strings.Contains(body, `videoCodec="hevc"`) ||
		!strings.Contains(body, `audioCodec="truehd"`) {
		t.Errorf("variant quality attrs missing\n%s", body)
	}
	// Variant part ids are compact JSON-safe integers in the 8e15 block.
	m := variantPartRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no variant part key in\n%s", body)
	}
	vid, _ := strconv.ParseInt(m[1], 10, 64)
	if vid < 8_000_000_000_000_000 || vid >= 1<<53 {
		t.Errorf("variant id %d out of safe range", vid)
	}
	// Every version carries its estimated video and audio stream, as PMS
	// lists streams on every Media (#267).
	if n := strings.Count(body, "<Stream "); n != 6 {
		t.Errorf("stream count %d, want 6 (2 per version)", n)
	}
	// No edition on this item.
	if strings.Contains(body, "editionTitle") {
		t.Errorf("unexpected editionTitle\n%s", body)
	}
	// JSON carries the same versions and labels.
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"4K Dolby Vision Remux"`) {
		t.Errorf("json variant label missing: %d %s", rec.Code, rec.Body.String())
	}
	// The rendered order is remembered for mediaIndex.
	ids, ok := f.srv.playIDs.releases(playIdentityKey{userID: ownerUser.ID, clientID: "client-1", key: fightClub.Key}, fixedNow)
	if !ok || len(ids) != 3 || ids[0] != "rel-auto" || ids[1] != "rel-4k" || ids[2] != "rel-compat" {
		t.Errorf("rendered set %v ok=%v", ids, ok)
	}
}

func TestDetailEditionTitleFromAutoVariant(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+alien.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `editionTitle="Director&#39;s Cut"`) {
		t.Errorf("editionTitle missing\n%s", rec.Body.String())
	}
}

func TestListResponsesStaySingleMedia(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/sections/1/all?type=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "<Media "); n != 2 {
		t.Errorf("list media count %d, want 2 (one per movie)", n)
	}
	if f.resolver.calls != 0 {
		t.Errorf("list rendering consulted variants %d times", f.resolver.calls)
	}
}

func TestMediaIndexSelectsVariant(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	f.get(t, "/library/metadata/"+fightClub.Key.String()) // renders + remembers
	base := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String()

	rec := f.get(t, base+"&mediaIndex=1&session=v1")
	if rec.Code != http.StatusOK {
		t.Fatalf("decision status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
	if pb.prepared != 0 {
		t.Errorf("plain Prepare used %d times for a variant pick", pb.prepared)
	}

	// Auto stays Auto.
	rec = f.get(t, base+"&mediaIndex=0&session=v2")
	if rec.Code != http.StatusOK || pb.prepared != 1 || len(pb.variantCalls) != 1 {
		t.Fatalf("auto decision code=%d prepared=%d variantCalls=%v", rec.Code, pb.prepared, pb.variantCalls)
	}
	// An out-of-range index refuses rather than silently playing another version.
	rec = f.get(t, base+"&mediaIndex=9&session=v3")
	if rec.Code != http.StatusBadRequest || len(pb.variantCalls) != 1 {
		t.Fatalf("oob decision code=%d variantCalls=%v", rec.Code, pb.variantCalls)
	}
}

func TestMediaIndexWithoutIssuedListRefusesUnknownVersion(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	// No issued list: a stored position must not select a new candidate.
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&mediaIndex=2&session=v9")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 0 || pb.prepared != 0 {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
}

func TestVariantPartDirectPlay(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	partKey := variantPartRE.FindString(rec.Body.String())
	if partKey == "" {
		t.Fatalf("no variant part key")
	}
	rec = f.get(t, partKey, "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("part status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
	// The Auto part keeps working and never reaches PrepareVariant.
	rec = f.get(t, "/library/parts/"+fightClub.Key.String()+"/1/file.mkv", "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent && rec.Code != http.StatusOK {
		t.Fatalf("auto part status %d", rec.Code)
	}
	if len(pb.variantCalls) != 1 {
		t.Errorf("auto part triggered PrepareVariant: %v", pb.variantCalls)
	}
	// Unknown part ids are 404, not a title.
	rec = f.get(t, "/library/parts/8500000000000000/1/file.mkv")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown variant part status %d", rec.Code)
	}
}

func TestVariantFallsBackWithoutVariantPlayback(t *testing.T) {
	plain := newFakePlayback(sampleInfo)
	f := newVariantFixture(t, plain)
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&mediaIndex=1&session=v5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if plain.prepared != 1 {
		t.Errorf("fallback prepared=%d, want 1 (normal Prepare)", plain.prepared)
	}
}

func TestVariantIDRegistryStablePersisted(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r1 := newVariantIDRegistry(testStores(), db)
	id := r1.idFor(fightClub.Key, "rel-4k")
	if id < 8_000_000_000_000_000 || id >= 1<<53 {
		t.Fatalf("id %d out of range", id)
	}
	if r1.idFor(fightClub.Key, "rel-4k") != id {
		t.Fatal("id not stable within registry")
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM variants_ids WHERE media_id=?`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("persisted rows = %d (err %v), want 1", rows, err)
	}
	// A fresh registry over the same DB (restart) resolves the same id.
	r2 := newVariantIDRegistry(testStores(), db)
	if got := r2.idFor(fightClub.Key, "rel-4k"); got != id {
		t.Fatalf("persisted id %d != %d", got, id)
	}
	ref, ok := r2.lookup(id)
	if !ok || ref.key != fightClub.Key || ref.releaseID != "rel-4k" {
		t.Fatalf("lookup %+v ok=%v", ref, ok)
	}
}

func TestVariantIDCollisionProbeAndPartTitle(t *testing.T) {
	db := store.OpenTest(t)
	r := newVariantIDRegistry(testStores(), db)
	h := variantHash(fightClub.Key, "rel-x")
	squatter := variantRef{key: alien.Key, releaseID: "other"}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO variants_ids(media_id,rating_key,release_id,last_seen) VALUES(?,?,?,?)`, h, int64(squatter.key), squatter.releaseID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	got := r.idFor(fightClub.Key, "rel-x")
	if got == h {
		t.Fatal("collision not probed")
	}
	if ref, ok := r.lookup(got); !ok || ref.releaseID != "rel-x" || ref.key != fightClub.Key {
		t.Fatalf("probed lookup %+v ok=%v", ref, ok)
	}
	if ref, ok := r.lookup(h); !ok || ref != squatter {
		t.Fatalf("squatter displaced: %+v", ref)
	}

	srv := &Server{variantIDs: r}
	if k, rel, ok := srv.partTitle(int64(fightClub.Key)); !ok || k != fightClub.Key || rel != "" {
		t.Errorf("auto partTitle %v %q %v", k, rel, ok)
	}
	if k, rel, ok := srv.partTitle(got); !ok || k != fightClub.Key || rel != "rel-x" {
		t.Errorf("variant partTitle %v %q %v", k, rel, ok)
	}
	if _, _, ok := srv.partTitle(8_500_000_000_000_000); ok {
		t.Error("unregistered variant id resolved")
	}
	if _, _, ok := srv.partTitle(12345); ok {
		t.Error("garbage part id resolved")
	}
}

func assertVIDAAXMLIdentity(t *testing.T, body string) {
	t.Helper()
	var doc struct {
		Videos []struct {
			Media []struct {
				ID    string `xml:"id,attr"`
				Parts []struct {
					ID string `xml:"id,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Videos) != 1 || len(doc.Videos[0].Media) != 1 {
		t.Fatalf("unexpected media: %s", body)
	}
	m := doc.Videos[0].Media[0]
	if m.ID != fightClub.Key.String() || len(m.Parts) != 1 || m.Parts[0].ID != fightClub.Key.String() {
		t.Fatalf("media/part identity: %s", body)
	}
}

func TestVIDAAMetadataStaysSingleMediaAfterVariantsResolve(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	variants := f.resolver.byKey[fightClub.Key]
	f.resolver.byKey[fightClub.Key] = nil
	path := "/library/metadata/" + fightClub.Key.String()
	headers := []string{"X-Plex-Product", "Plex for VIDAA"}
	cold := f.get(t, path, headers...)
	if cold.Code != http.StatusOK || strings.Count(cold.Body.String(), "<Media ") != 1 {
		t.Fatalf("cold metadata: %d %s", cold.Code, cold.Body.String())
	}
	assertVIDAAXMLIdentity(t, cold.Body.String())
	f.resolver.byKey[fightClub.Key] = variants
	warm := f.get(t, path, headers...)
	if warm.Code != http.StatusOK || strings.Count(warm.Body.String(), "<Media ") != 1 {
		t.Fatalf("warm VIDAA metadata should retain one media: %d %s", warm.Code, warm.Body.String())
	}
	assertVIDAAXMLIdentity(t, warm.Body.String())
	for _, product := range []string{"plex for vidaa", "  Plex for VIDAA  "} {
		r := f.get(t, path, "X-Plex-Product", product)
		assertVIDAAXMLIdentity(t, r.Body.String())
	}
	jr := f.get(t, path, "X-Plex-Product", "Plex for VIDAA", "Accept", "application/json")
	var j map[string]any
	if err := json.Unmarshal(jr.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	mc := j["MediaContainer"].(map[string]any)
	v := mc["Metadata"].([]any)[0].(map[string]any)
	media := v["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("JSON media: %s", jr.Body.String())
	}
	jm := media[0].(map[string]any)
	parts := jm["Part"].([]any)
	if jm["id"] != float64(fightClub.Key) || len(parts) != 1 || parts[0].(map[string]any)["id"] != float64(fightClub.Key) {
		t.Fatalf("JSON media/part identity: %s", jr.Body.String())
	}
	other := f.get(t, path, "X-Plex-Product", "Plex Web")
	if n := strings.Count(other.Body.String(), "<Media "); other.Code != http.StatusOK || n != 3 {
		t.Fatalf("other client should retain all variants: %d media=%d", other.Code, n)
	}
}

func TestVIDAAPlaybackMetadataLifecycle(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	path := "/library/metadata/" + fightClub.Key.String()
	headers := []string{"X-Plex-Product", "Plex for VIDAA"}
	check := func(stage string) {
		t.Helper()
		r := f.get(t, path, headers...)
		if r.Code != 200 {
			t.Fatalf("%s metadata: %d %s", stage, r.Code, r.Body.String())
		}
		assertVIDAAXMLIdentity(t, r.Body.String())
	}
	check("warm")
	q := httptest.NewRequest(http.MethodPost, "/playQueues?type=video&uri="+urlEsc(path), nil)
	q.Header.Set("X-Plex-Token", ownerToken)
	q.Header.Set("X-Plex-Client-Identifier", "client-1")
	q.Header.Set("X-Plex-Product", "Plex for VIDAA")
	qr := httptest.NewRecorder()
	f.srv.ServeHTTP(qr, q)
	if qr.Code != 200 {
		t.Fatalf("queue: %d %s", qr.Code, qr.Body.String())
	}
	assertVIDAAXMLIdentity(t, qr.Body.String())
	query := "path=" + urlEsc(path) + "&session=vidaa&mediaIndex=0&directPlay=0&protocol=hls"
	for _, endpoint := range []string{"decision?", "start.m3u8?"} {
		r := f.get(t, "/video/:/transcode/universal/"+endpoint+query, headers...)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", endpoint, r.Code, r.Body.String())
		}
		if endpoint == "decision?" {
			assertVIDAAXMLIdentity(t, r.Body.String())
		}
	}
	if pb.prepared == 0 || len(pb.variantCalls) != 0 {
		t.Fatalf("auto preparation: prepared=%d variants=%v", pb.prepared, pb.variantCalls)
	}
	ps, ok := pb.SessionFor(ownerUser.ID, "client-1", fightClub.Key)
	if !ok || len(ps.Info.Streams) == 0 || ps.Info.Streams[0].Width != sampleInfo.Streams[0].Width || ps.Info.Streams[0].Height != sampleInfo.Streams[0].Height || ps.Info.Streams[0].Codec != sampleInfo.Streams[0].Codec {
		t.Fatalf("prepared source: %+v", ps)
	}
	check("active")
	r := f.get(t, "/video/:/transcode/universal/start.m3u8?"+query+"&videoBitrate=2000&videoResolution=1280x720", headers...)
	if r.Code != 200 {
		t.Fatalf("quality switch: %d %s", r.Code, r.Body.String())
	}
	if r = f.get(t, "/video/:/transcode/universal/stop?session=vidaa", headers...); r.Code != 200 {
		t.Fatalf("stop: %d", r.Code)
	}
	check("stopped")
	if r = f.get(t, path, "X-Plex-Token", "invalid", "X-Plex-Product", "Plex for VIDAA"); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized metadata: %d", r.Code)
	}
}

// ETHE-5 C: same-quality alternatives are separate Play Versions. Auto stays
// the first Media, each alternative prepares its own release by mediaIndex or
// part id, and plain Play still prepares Auto.
func TestSameQualityAlternativesAreSeparateVersions(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	alt := func(id, label string) core.ReleaseVariant {
		return mkVariant(id, label, "", false, 1080, "h264", "eac3", false, 7000)
	}
	f.resolver.byKey[fightClub.Key] = []core.ReleaseVariant{
		mkVariant("rel-auto", "Auto / Best for Device", "", true, 1080, "h264", "eac3", false, 7600),
		alt("rel-alt1", "1080p WEB-DL · DD+ 5.1 · 2.3 GB · FLUX"),
		alt("rel-alt2", "1080p WEB-DL · AAC 2.0 · 1.6 GB · EDITH"),
		mkVariant("rel-4k", "4K Dolby Vision Remux", "", false, 2160, "hevc", "truehd", true, 45000),
		mkVariant("rel-compat", "720p Blu-ray (High Compatibility)", "", false, 720, "h264", "aac", false, 4000),
	}
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	body := rec.Body.String()
	if n := strings.Count(body, "<Media "); n != 5 {
		t.Fatalf("media count %d, want 5\n%s", n, body)
	}
	if f.resolver.lastMax != core.MaxVariants {
		t.Errorf("render asked for %d versions, want core.MaxVariants=%d", f.resolver.lastMax, core.MaxVariants)
	}
	auto, a1, a2 := strings.Index(body, `title="Auto / Best`), strings.Index(body, "FLUX"), strings.Index(body, "EDITH")
	if auto < 0 || !(auto < a1 && a1 < a2) {
		t.Errorf("labels out of order auto=%d alt1=%d alt2=%d", auto, a1, a2)
	}
	if !strings.Contains(body, `id="`+fightClub.Key.String()+`"`) {
		t.Errorf("auto keeps the rating-key id")
	}

	// A client that addresses the alternative's part key reaches the same release.
	parts := regexp.MustCompile(`/library/parts/(9[0-9]{15})/[0-9]+/file\.mkv`).FindAllString(body, -1)
	if len(parts) != 4 {
		t.Fatalf("variant part keys %v", parts)
	}
	before := len(pb.variantCalls)
	if rec := f.get(t, parts[0], "Range", "bytes=0-9"); rec.Code != http.StatusPartialContent {
		t.Fatalf("part status %d", rec.Code)
	}
	if pb.variantCalls[before] != "rel-alt1" {
		t.Errorf("first variant part prepared %v", pb.variantCalls)
	}

	base := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String()
	for i, want := range []string{"rel-alt1", "rel-alt2"} {
		before := len(pb.variantCalls)
		if rec := f.get(t, base+fmt.Sprintf("&mediaIndex=%d&session=alt%d", i+1, i)); rec.Code != http.StatusOK {
			t.Fatalf("decision %d status %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if len(pb.variantCalls) != before+1 || pb.variantCalls[before] != want {
			t.Fatalf("mediaIndex %d prepared %v, want %s", i+1, pb.variantCalls, want)
		}
	}
	// Index 0 is Auto, and so is a decision without a selector, which plays
	// the first Media as v0.3.4 did.
	n, prepared := len(pb.variantCalls), pb.prepared
	if rec := f.get(t, base+"&session=plain"); rec.Code != http.StatusOK {
		t.Fatalf("missing selector: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, base+"&mediaIndex=0&session=plain0"); rec.Code != http.StatusOK {
		t.Fatalf("Auto selector: %d %s", rec.Code, rec.Body.String())
	}
	if pb.prepared != prepared+2 || len(pb.variantCalls) != n {
		t.Errorf("plain play prepared=%d variantCalls=%v", pb.prepared, pb.variantCalls)
	}
}

// The detail page names Auto after what this viewer actually played: the
// resumed release goes to the resolver with the variants request (#188).
func TestDetailPassesPlayedReleaseToVariants(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	if f.resolver.lastPlayed != "" {
		t.Fatalf("played release %q before any play", f.resolver.lastPlayed)
	}
	info := pb.info
	info.Estimated = false
	pb.mu.Lock()
	pb.resumed = map[resumedFactKey]resumedFact{{ownerUser.ID, fightClub.Key}: {info: info, release: "rel-played"}}
	pb.mu.Unlock()
	if rec := f.get(t, "/library/metadata/"+fightClub.Key.String()); rec.Code != http.StatusOK {
		t.Fatalf("detail %d", rec.Code)
	}
	if f.resolver.lastPlayed != "rel-played" {
		t.Fatalf("played release %q, want rel-played", f.resolver.lastPlayed)
	}
}

// A played file that is SD AAC must not leave Auto titled as the 1080p
// E-AC-3 estimate the detail showed before playback.
func TestAutoLabelFollowsPlayedProbe(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	probe := core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 720, Height: 480, Default: true},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true},
	}}
	pb.mu.Lock()
	pb.resumed = map[resumedFactKey]resumedFact{{ownerUser.ID, fightClub.Key}: {info: probe, release: "rel-sd"}}
	pb.mu.Unlock()
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("detail %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `title="Auto · SD · AAC"`) {
		t.Fatalf("auto title did not follow the played file:\n%s", body)
	}
	if !strings.Contains(body, `videoResolution="480"`) {
		t.Fatalf("played media still hides the SD probe:\n%s", body)
	}
}

// Scope-ratio 1080p (1920x800), probe-corrected SD fallback preserving
// release metadata, and Auto matching the second-row release's label and
// bitrate/attributes (Send Help 2026 regression).
func TestAutoScope1080pAndSecondRowReleaseConsistency(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)

	row1Rel := core.Release{
		ID:   "rel-row1",
		Name: "Send.Help.2026.1080p.WEB-DL.DDP5.1.H.264-FLUX",
		Info: core.ReleaseInfo{
			Title: "Send Help", Year: 2026, Height: 1080,
			Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "eac3",
			Channels: "5.1", Group: "FLUX",
		},
	}
	scopeRel := core.Release{
		ID:   "rel-scope",
		Name: "Send.Help.2026.1080p.AMZN.WEB-DL.DDP5.1.Atmos.H.264-SCOPE",
		Info: core.ReleaseInfo{
			Title: "Send Help", Year: 2026, Height: 1080,
			Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "eac3",
			Atmos: true, Channels: "5.1", Group: "SCOPE",
		},
	}
	rbbRel := core.Release{
		ID:   "rel-rbb",
		Name: "Send.Help.2026.BDRip.x264-rbb",
		Info: core.ReleaseInfo{
			Title: "Send Help", Year: 2026, Height: 0,
			Source: core.SourceBluRay, VideoCodec: "h264", Group: "rbb",
		},
	}

	vAuto := mkVariant(row1Rel.ID, "Auto · 1080p WEB-DL · DD+ 5.1 · FLUX", "", true, 1080, "h264", "eac3", false, 9400)
	vAuto.Release = row1Rel
	vRow2 := mkVariant(scopeRel.ID, "1080p WEB-DL · DD+ Atmos 5.1 · 6.8 GB · SCOPE", "", false, 1080, "h264", "eac3", false, 8200)
	vRow2.Release = scopeRel
	v720 := mkVariant("rel-720", "720p WEB-DL", "", false, 720, "h264", "aac", false, 3800)
	vSD := mkVariant(rbbRel.ID, "SD Blu-ray · rbb", "", false, 480, "h264", "aac", false, 1100)
	vSD.Release = rbbRel

	f.resolver.byKey[fightClub.Key] = []core.ReleaseVariant{vAuto, vRow2, v720, vSD}

	// 1. Scope-aspect 1920x800 probe when Auto picked the second row (SCOPE):
	// Auto must read "Auto · 1080p WEB-DL · DD+ Atmos 5.1 · SCOPE" (never "800p · EAC3"),
	// videoResolution="1080", and carry the chosen second row's bitrate (8200 kbps).
	scopeProbe := core.MediaInfo{
		Container:   "mkv",
		Duration:    139 * time.Minute,
		Size:        6_800_000_000,
		BitrateKbps: 9400, // probe container header disagrees with release size estimate
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 800, FrameRate: 23.976, Default: true},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, SampleRate: 48000, Default: true},
		},
	}
	pb.mu.Lock()
	pb.resumed = map[resumedFactKey]resumedFact{{ownerUser.ID, fightClub.Key}: {info: scopeProbe, release: scopeRel.ID}}
	pb.mu.Unlock()

	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("detail %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "800p") {
		t.Fatalf("scope 1920x800 mislabelled as 800p:\n%s", body)
	}
	wantAutoTitle := `title="Auto · 1080p WEB-DL · DD+ Atmos 5.1 · SCOPE"`
	if !strings.Contains(body, wantAutoTitle) {
		t.Fatalf("auto title did not match second-row release (%s):\n%s", wantAutoTitle, body)
	}
	mediaTags := regexp.MustCompile(`<Media\b[^>]*>`).FindAllString(body, -1)
	if len(mediaTags) != 4 {
		t.Fatalf("media count %d, want 4:\n%s", len(mediaTags), body)
	}
	for _, idx := range []int{0, 1} {
		if !strings.Contains(mediaTags[idx], `bitrate="8200"`) || !strings.Contains(mediaTags[idx], `videoResolution="1080"`) || !strings.Contains(mediaTags[idx], `width="1920"`) || !strings.Contains(mediaTags[idx], `height="800"`) {
			t.Fatalf("media[%d] attributes out of sync with chosen release: %s", idx, mediaTags[idx])
		}
	}
	if !strings.Contains(mediaTags[0], `selected="1"`) || strings.Contains(mediaTags[1], `selected="1"`) {
		t.Fatalf("expected only Auto media[0] selected:\nmedia[0]=%s\nmedia[1]=%s", mediaTags[0], mediaTags[1])
	}

	// 2. Probe-corrected SD fallback on a parsed 1080p release preserves
	// release source and group while correcting resolution and audio codec.
	sdProbe := core.MediaInfo{
		Container:   "mkv",
		BitrateKbps: 1100,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 720, Height: 300, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true},
		},
	}
	f.resolver.byKey[fightClub.Key] = []core.ReleaseVariant{vAuto, vRow2, v720, vSD}
	pb.mu.Lock()
	pb.resumed = map[resumedFactKey]resumedFact{{ownerUser.ID, fightClub.Key}: {info: sdProbe, release: "rel-fallback-sd"}}
	pb.mu.Unlock()

	rec = f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("detail sd fallback %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, `title="Auto · SD WEB-DL · AAC · FLUX"`) {
		t.Fatalf("probe-corrected SD fallback dropped release source/group:\n%s", body)
	}
	if !strings.Contains(body, `videoResolution="480"`) {
		t.Fatalf("scope SD 720x300 should classify as 480 by width:\n%s", body)
	}
}

// A detail page opened before the title's search finished shows one
// version. Once discovery finishes, an ordinary detail gains the picker
// immediately. Only explicit pre-play/queue flows retain a version list.
// Refs #282: the native detail cache needs a terminal PMS metadata event,
// not a state-5 frame that still carries metadataState="created". Check the
// actual version-discovery path and both transport projections, rather than
// merely checking that a subsequent manual fetch contains more versions.
func TestDetailPageRefreshesWhenVersionsArrive(t *testing.T) {
	saved := variantWatchSchedule
	variantWatchSchedule = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { variantWatchSchedule = saved })
	for _, client := range []struct{ name, product, platform string }{
		{"Shield", "Plex for Android (TV)", "Android"},
		{"iOS", "Plex for iOS", "iOS"},
	} {
		t.Run(client.name, func(t *testing.T) {
			f, _ := newVariantPlaybackFixture(t)
			full := f.resolver.byKey[fightClub.Key]
			f.resolver.mu.Lock()
			f.resolver.byKey[fightClub.Key] = full[:1]
			f.resolver.mu.Unlock()
			// Subscribe before opening the page, as an already-connected native app.
			sub, _ := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
			defer f.srv.notifier.unsubscribe(sub)
			headers := []string{"X-Plex-Product", client.product, "X-Plex-Platform", client.platform}
			path := "/library/metadata/" + fightClub.Key.String()
			rec := f.get(t, path, headers...)
			if n := strings.Count(rec.Body.String(), "<Media "); n != 1 {
				t.Fatalf("media count %d before the search, want 1", n)
			}
			f.resolver.mu.Lock()
			f.resolver.byKey[fightClub.Key] = full
			f.resolver.mu.Unlock()
			select {
			case msg := <-sub.ch:
				// PMS terminal state 5 omits metadataState and mediaState. itemID
				// and rootItemID are the server ratingKey, never the plex:// GUID.
				entry := fmt.Sprintf(`{"identifier":"com.plexapp.plugins.library","sectionID":"1","itemID":"%s","rootItemID":"%s","type":1,"title":"","state":5,"updatedAt":%d}`, fightClub.Key, fightClub.Key, fixedNow.Unix())
				want := `{"NotificationContainer":{"type":"timeline","size":1,"TimelineEntry":[` + entry + `]}}`
				if msg.typ != "timeline" || string(msg.data) != want {
					t.Fatalf("native detail refresh:\n got %s\nwant %s", msg.data, want)
				}
				wantSSE := `{"TimelineEntry":` + entry + `}`
				if len(msg.sse) != 1 || string(msg.sse[0]) != wantSSE {
					t.Fatalf("eventsource refresh: %q, want %s", msg.sse, wantSSE)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no refresh after the versions arrived")
			}
			if n := strings.Count(f.get(t, path, headers...).Body.String(), "<Media "); n != 3 {
				t.Fatalf("media count %d after refill without a play flow, want 3", n)
			}
		})
	}
}

// Issue #267: Plex Web chooses the version itself (highest playable
// quality, not Media@selected), keeps that Media index for the whole
// session, and reads Media[index].Part[0].Stream when its player settings
// panel opens. It adds a "None" subtitle track only to a Part with a video
// stream; without one the panel reads the id of an undefined subtitle and
// the player ends with "unexpected error during playback". A re-fetch
// during the session (subtitle download, timeline refresh) must not shrink
// the Media list under that index.
func TestPlexWebVersionKeepsTracksAndIndexDuringPlayback(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	path := "/library/metadata/" + fightClub.Key.String()
	web := []string{"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "Accept", "application/json"}
	type part struct {
		ID     int64 `json:"id"`
		Stream []struct {
			StreamType int  `json:"streamType"`
			Selected   bool `json:"selected"`
		} `json:"Stream"`
	}
	type media struct {
		ID       int64  `json:"id"`
		Selected bool   `json:"selected"`
		Part     []part `json:"Part"`
	}
	fetch := func(stage string) []media {
		t.Helper()
		rec := f.get(t, path, web...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", stage, rec.Code)
		}
		var body struct {
			MediaContainer struct {
				Metadata []struct {
					Media []media `json:"Media"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.MediaContainer.Metadata) != 1 {
			t.Fatalf("%s: %v %s", stage, err, rec.Body.String())
		}
		ms := body.MediaContainer.Metadata[0].Media
		for i, m := range ms {
			video, selectedAudio := 0, 0
			for _, st := range m.Part[0].Stream {
				if st.StreamType == 1 {
					video++
				}
				if st.StreamType == 2 && st.Selected {
					selectedAudio++
				}
			}
			if video != 1 || selectedAudio != 1 {
				t.Errorf("%s: Media[%d] video=%d selectedAudio=%d, want 1/1 (Plex Web reads the playing Part's tracks)", stage, i, video, selectedAudio)
			}
		}
		return ms
	}
	before := fetch("before playback")
	if len(before) != 3 {
		t.Fatalf("before playback: %d media, want 3", len(before))
	}

	// Plex Web plays the 4K version by its Media index.
	rec := f.get(t, "/video/:/transcode/universal/decision?path="+urlEsc(path)+"&mediaIndex=1&partIndex=0&session=web267&directPlay=0&directStream=1&protocol=dash", web[:4]...)
	if rec.Code != http.StatusOK || len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("decision: %d variants=%v %s", rec.Code, pb.variantCalls, rec.Body.String())
	}

	during := fetch("during playback")
	if len(during) != len(before) {
		t.Fatalf("during playback: %d media, want the %d Plex Web indexed", len(during), len(before))
	}
	for i := range before {
		if during[i].ID != before[i].ID {
			t.Errorf("Media[%d] id %d -> %d during playback", i, before[i].ID, during[i].ID)
		}
	}
	if !during[1].Selected || during[0].Selected || during[2].Selected {
		t.Errorf("playing version not the selected Media: %+v", during)
	}
	audio, subs := 0, 0
	for _, st := range during[1].Part[0].Stream {
		switch st.StreamType {
		case 2:
			audio++
		case 3:
			subs++
		}
	}
	if audio != 2 || subs != 2 {
		t.Errorf("playing version tracks audio=%d subs=%d, want the session probe's 2/2", audio, subs)
	}
}

// Native clients also keep their chosen version index during playback.
func TestNonWebClientsSeeThePlayingVersionDuringPlayback(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	path := "/library/metadata/" + fightClub.Key.String()
	atv := []string{"X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS", "Accept", "application/json"}
	count := func() int {
		t.Helper()
		var body struct {
			MediaContainer struct {
				Metadata []struct {
					Media []json.RawMessage `json:"Media"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		rec := f.get(t, path, atv...)
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.MediaContainer.Metadata) != 1 {
			t.Fatalf("%v %s", err, rec.Body.String())
		}
		return len(body.MediaContainer.Metadata[0].Media)
	}
	if n := count(); n != 3 {
		t.Fatalf("before playback: %d media, want 3", n)
	}
	rec := f.get(t, "/video/:/transcode/universal/decision?path="+urlEsc(path)+"&mediaIndex=1&partIndex=0&session=atv267&directPlay=0&directStream=1&protocol=hls", atv[:4]...)
	if rec.Code != http.StatusOK || len(pb.variantCalls) != 1 {
		t.Fatalf("decision: %d %v", rec.Code, pb.variantCalls)
	}
	if n := count(); n != 3 {
		t.Fatalf("during playback: %d media, want the full version list", n)
	}
}
