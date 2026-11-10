package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// fakeSubtitles is Deps.Subtitles: cached sidecars per title, viewer picks,
// and canned search offers.
type fakeSubtitles struct {
	mu         sync.Mutex
	cached     map[core.RatingKey][]core.StreamInfo // automatic, one per language
	picks      map[core.RatingKey][]core.StreamInfo // viewer picks via search
	files      map[string]string                    // ExternalURL -> SRT
	offers     []SubtitleOffer
	searchErr  error
	searchLang string
	searchRel  string
}

func newFakeSubtitles() *fakeSubtitles {
	return &fakeSubtitles{cached: map[core.RatingKey][]core.StreamInfo{}, picks: map[core.RatingKey][]core.StreamInfo{}, files: map[string]string{}}
}

const sampleSRT = "1\n00:00:05,000 --> 00:00:07,000\nThey're coming to get you.\n\n"

func srtToVTT(s string) string {
	return "WEBVTT\n\n" + regexp.MustCompile(`(\d\d),(\d\d\d)`).ReplaceAllString(s, "$1.$2")
}

func sidecar(lang, url string) core.StreamInfo {
	return core.StreamInfo{Index: 9, Type: "subtitle", Codec: "srt", Language: lang, External: true, ExternalURL: url}
}

func (f *fakeSubtitles) cache(key core.RatingKey, st core.StreamInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cached[key] = append(f.cached[key], st)
	f.files[st.ExternalURL] = sampleSRT
}

func (f *fakeSubtitles) OpenFormat(id, format string) (io.ReadCloser, string, error) {
	f.mu.Lock()
	text, ok := f.files[id]
	f.mu.Unlock()
	if !ok {
		return nil, "", core.ErrNotFound
	}
	if format == "vtt" {
		return io.NopCloser(strings.NewReader(srtToVTT(text))), "vtt", nil
	}
	return io.NopCloser(strings.NewReader(text)), "srt", nil
}

// CachedSidecars mirrors subtitles.Service: picks always (unless listed),
// automatic tracks only for languages embedded does not cover.
func (f *fakeSubtitles) CachedSidecars(_ context.Context, t *core.Title, _ *core.User, embedded []core.StreamInfo) []core.StreamInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	present, langs := map[string]bool{}, map[string]bool{}
	for _, st := range embedded {
		present[st.ExternalURL] = st.External
		if st.Type == "subtitle" {
			langs[langtag.Normalize(st.Language)] = true
		}
	}
	var out []core.StreamInfo
	for _, st := range f.picks[t.Key] {
		if !present[st.ExternalURL] {
			out = append(out, st)
			langs[langtag.Normalize(st.Language)] = true
		}
	}
	for _, st := range f.cached[t.Key] {
		if !present[st.ExternalURL] && !langs[langtag.Normalize(st.Language)] {
			out = append(out, st)
		}
	}
	return out
}

func (f *fakeSubtitles) SearchSubtitles(_ context.Context, t *core.Title, _ *core.User, lang, release string) ([]SubtitleOffer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchLang, f.searchRel = lang, release
	return f.offers, f.searchErr
}

func (f *fakeSubtitles) DownloadSubtitle(_ context.Context, t *core.Title, _ *core.User, id int64) (core.StreamInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.offers {
		if o.ID == id {
			st := sidecar(o.Language, fmt.Sprintf("ether-subtitle:pick-%d", id))
			f.picks[t.Key] = append(f.picks[t.Key], st)
			f.files[st.ExternalURL] = sampleSRT
			return st, nil
		}
	}
	return core.StreamInfo{}, core.ErrNotFound
}

func (f *fakeSubtitles) UploadSubtitle(_ context.Context, t *core.Title, _ *core.User, filename, format, language string, body []byte) (core.StreamInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if language == "" {
		language = "en"
	}
	st := sidecar(language, "ether-subtitle:upload-"+filename)
	st.Title = filename
	st.Codec = format
	f.picks[t.Key] = append(f.picks[t.Key], st)
	f.files[st.ExternalURL] = string(body)
	return st, nil
}

var (
	// estimateInfo is what playback.Estimate gives before a session: a
	// video and an audio stream, no subtitle tracks.
	estimateInfo = core.MediaInfo{
		Container: "mkv", Duration: 139 * time.Minute, BitrateKbps: 7600, Estimated: true,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, Default: true},
		},
	}
	// probedInfo is the session's media: more embedded streams, and the
	// sidecar Enrich chose for this release (a different file than the one
	// cached before play).
	probedInfo = core.MediaInfo{
		Container: "mkv", Duration: 139 * time.Minute, Size: 8_000_000_000, BitrateKbps: 7600,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, Default: true},
			{Index: 2, Type: "audio", Codec: "aac", Language: "fr", Channels: 2},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "fr"},
			{Index: 4, Type: "subtitle", Codec: "srt", Language: "en", External: true, ExternalURL: "ether-subtitle:release-en", Title: "EN (OpenSubtitles: Fight.Club.1999.1080p)"},
		},
	}
)

func newSubtitleFixture(t *testing.T) (*fixture, *fakeSubtitles) {
	t.Helper()
	subs := newFakeSubtitles()
	f := newFixture(func(d *Deps) { d.Subtitles = subs })
	t.Cleanup(func() { f.srv.Close() })
	f.playback.info = estimateInfo
	subs.cache(fightClub.Key, sidecar("en", "ether-subtitle:cached-en"))
	subs.files["ether-subtitle:release-en"] = sampleSRT
	return f, subs
}

var streamTagRe = regexp.MustCompile(`<Stream [^>]*>`)

// subtitleTags returns the streamType="3" Stream elements of an XML body.
func selectedTag(tags []string) string {
	for _, tag := range tags {
		if strings.Contains(tag, `selected="1"`) {
			return tag
		}
	}
	return ""
}

func subtitleTags(body string) []string {
	var out []string
	for _, tag := range streamTagRe.FindAllString(body, -1) {
		if strings.Contains(tag, `streamType="3"`) {
			out = append(out, tag)
		}
	}
	return out
}

func externalID(key core.RatingKey, url string) int64 {
	return slotHash(key, "external/"+url)
}

func TestCachedSidecarListedBeforePlayback(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	id := externalID(fightClub.Key, "ether-subtitle:cached-en")

	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	subs := subtitleTags(rec.Body.String())
	want := fmt.Sprintf(`<Stream id="%d" streamType="3" canAutoSync="0" codec="srt" key="/library/streams/%d" language="English" languageTag="en" languageCode="eng" format="srt" displayTitle="English" extendedDisplayTitle="English (SRT External)" external="1" transient="0" />`, id, id)
	if len(subs) != 1 || subs[0] != want {
		t.Fatalf("detail subtitle streams:\n got %q\nwant %q", subs, want)
	}

	// The play queue item (what the player builds its menus from) lists
	// the same stream under the same id.
	uri := url.QueryEscape("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String())
	rec = f.request(t, http.MethodPost, "/playQueues?type=video&uri="+uri, nil)
	if got := subtitleTags(rec.Body.String()); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf(`id="%d"`, id)) || !strings.Contains(got[0], `external="1"`) {
		t.Fatalf("play queue subtitle streams: %q\n%s", got, rec.Body.String())
	}
}

func TestSidecarStreamServesRequestedFormat(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	f.get(t, "/library/metadata/"+fightClub.Key.String()) // registers stream ids
	id := externalID(fightClub.Key, "ether-subtitle:cached-en")
	for _, tc := range []struct {
		path, ctype, prefix string
	}{
		{fmt.Sprintf("/library/streams/%d", id), "application/x-subrip", "1\n00:00:05,000"},
		{fmt.Sprintf("/library/streams/%d?encoding=utf-8&format=srt", id), "application/x-subrip", "1\n00:00:05,000"},
		{fmt.Sprintf("/library/streams/%d?format=webvtt", id), "text/vtt", "WEBVTT\n\n1\n00:00:05.000"},
		{fmt.Sprintf("/library/streams/%d.vtt", id), "text/vtt", "WEBVTT"},
	} {
		rec := f.get(t, tc.path)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != tc.ctype || !strings.HasPrefix(rec.Body.String(), tc.prefix) {
			t.Fatalf("%s: %d %q %q", tc.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	if rec := f.get(t, "/library/streams/12345"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown stream: %d", rec.Code)
	}
}

func TestSidecarSelectionSurvivesProbeAndRemux(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	key := fightClub.Key.String()
	cachedID := externalID(fightClub.Key, "ether-subtitle:cached-en")
	f.get(t, "/library/metadata/"+key)
	f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/%s?subtitleStreamID=%d&allParts=1", key, cachedID), nil)
	if subs := subtitleTags(f.get(t, "/library/metadata/"+key).Body.String()); len(subs) != 1 || !strings.Contains(subs[0], `selected="1"`) {
		t.Fatalf("selection not shown: %q", subs)
	}

	// Play: the probed release has more streams and its own English
	// sidecar. The viewer's English choice follows it; text sidecars are
	// served to the client, never passed into the remux.
	f.playback.info = probedInfo
	android := []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android"}
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&directPlay=0&directStream=1&session=remux1&protocol=hls", android...)
	releaseID := externalID(fightClub.Key, "ether-subtitle:release-en")
	subs := subtitleTags(rec.Body.String())
	selected := selectedTag(subs)
	if selected == "" || !strings.Contains(selected, fmt.Sprintf(`id="%d"`, releaseID)) ||
		!strings.Contains(selected, fmt.Sprintf(`key="/library/streams/%d"`, releaseID)) || !strings.Contains(selected, `decision="copy" location="sidecar"`) {
		t.Fatalf("decision subtitle: %q\n%s", subs, rec.Body.String())
	}
	if len(subs) < 2 {
		t.Fatalf("decision dropped alternate subtitles: %q", subs)
	}
	if f.trans.lastOpts.SubtitleStream != -1 {
		t.Fatalf("sidecar reached the transcoder: %+v", f.trans.lastOpts)
	}

	// Off.
	f.request(t, http.MethodPut, "/library/parts/"+key+"?subtitleStreamID=0", nil)
	for _, tag := range subtitleTags(f.get(t, "/library/metadata/"+key).Body.String()) {
		if strings.Contains(tag, `selected="1"`) {
			t.Fatalf("subtitle still selected after off: %s", tag)
		}
	}
}

func TestSubtitleStreamIDOnDecisionPicksSidecar(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	f.playback.info = probedInfo
	key := fightClub.Key.String()
	f.get(t, "/library/metadata/"+key)
	releaseID := externalID(fightClub.Key, "ether-subtitle:release-en")
	// Direct play keeps every subtitle and marks the requested sidecar selected.
	android := []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android"}
	rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&subtitleStreamID=%d&session=dp1", key, releaseID), android...)
	subs := subtitleTags(rec.Body.String())
	selected := selectedTag(subs)
	if selected == "" || !strings.Contains(selected, fmt.Sprintf(`key="/library/streams/%d"`, releaseID)) {
		t.Fatalf("direct play subtitle: %q\n%s", subs, rec.Body.String())
	}
	if len(subs) < 2 {
		t.Fatalf("direct play dropped alternate subtitles: %q", subs)
	}
}

func TestTrackPreferenceAlwaysSelectsCachedSidecar(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefs := trackprefs.New(db)
	f.srv.d.TrackPreferences = prefs
	r := trackprefs.Default()
	r.SubtitleLanguages = []string{"en"}
	r.SubtitleMode = "always"
	if err := prefs.PutRules(context.Background(), 0, r); err != nil {
		t.Fatal(err)
	}
	subs := subtitleTags(f.get(t, "/library/metadata/"+fightClub.Key.String()).Body.String())
	if len(subs) != 1 || !strings.Contains(subs[0], `selected="1"`) {
		t.Fatalf("always mode did not select the sidecar: %q", subs)
	}
	// And at Play, without any explicit choice.
	f.playback.info = probedInfo
	android := []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android"}
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&session=always1", android...)
	got := subtitleTags(rec.Body.String())
	selected := selectedTag(got)
	if selected == "" || !strings.Contains(selected, `external="1"`) {
		t.Fatalf("always mode at play: %q", got)
	}
	if len(got) < 2 {
		t.Fatalf("always mode dropped alternate subtitles: %q", got)
	}
}

func TestSubtitleSearchAndPick(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	subs.offers = []SubtitleOffer{
		{ID: 102, Language: "en", Title: "Fight.Club.1999.SDH", Provider: "OpenSubtitles", Score: 90, HearingImpaired: true},
		{ID: 101, Language: "en", Title: "Fight.Club.1999.1080p.BluRay", Provider: "OpenSubtitles", Score: 80, PerfectMatch: true},
	}
	key := fightClub.Key.String()
	mc := decodeJSON(t, f.get(t, "/library/metadata/"+key+"/subtitles?language=en&hearingImpaired=0&forced=0", jsonAccept...))
	results, _ := mc["Stream"].([]any)
	if mc["size"].(float64) != 2 || len(results) != 2 || subs.searchLang != "en" {
		t.Fatalf("search: %v (lang %q)", mc, subs.searchLang)
	}
	first := results[0].(map[string]any)
	wantKey := first["key"].(string)
	if !strings.HasPrefix(wantKey, "/library/streams/") || first["sourceKey"] != "101" || first["score"] != "80" || first["transient"] != float64(1) {
		t.Fatalf("provider projection: %v", first)
	}
	if first["key"] != wantKey || first["streamType"].(float64) != 3 || first["codec"] != "srt" || first["languageCode"] != "eng" ||
		first["providerTitle"] != "OpenSubtitles" || first["perfectMatch"] != true || first["hearingImpaired"] != false ||
		first["title"] != "Fight.Club.1999.1080p.BluRay" || first["displayTitle"] != "English (SRT)" {
		t.Fatalf("first result (non-SDH preferred): %v", first)
	}
	mc = decodeJSON(t, f.get(t, "/library/metadata/"+key+"/subtitles?language=en&hearingImpaired=2", jsonAccept...))
	if mc["size"].(float64) != 1 {
		t.Fatalf("SDH-only filter: %v", mc)
	}

	// Picks are validated against the title's search results.
	if rec := f.request(t, http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape("/library/metadata/"+alien.Key.String()+"/subtitles/101"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign key: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape("/library/streams/999"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown offer: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape(wantKey), nil); rec.Code != http.StatusOK {
		t.Fatalf("pick: %d %s", rec.Code, rec.Body.String())
	}
	waitSubtitlePick(t, f, fightClub.Key)
	pickID := externalID(fightClub.Key, "ether-subtitle:pick-101")
	found := false
	for _, tag := range subtitleTags(f.get(t, "/library/metadata/"+key).Body.String()) {
		if strings.Contains(tag, fmt.Sprintf(`id="%d"`, pickID)) {
			found = true
			if !strings.Contains(tag, `selected="1"`) || !strings.Contains(tag, `external="1"`) {
				t.Fatalf("picked track not selected: %s", tag)
			}
		} else if strings.Contains(tag, `selected="1"`) {
			t.Fatalf("other track selected: %s", tag)
		}
	}
	if !found {
		t.Fatal("picked track not listed")
	}
	if rec := f.get(t, "/library/streams/"+strconv.FormatInt(pickID, 10)); rec.Code != http.StatusOK || rec.Body.String() != sampleSRT {
		t.Fatalf("picked track body: %d %q", rec.Code, rec.Body.String())
	}

	// A failing provider answers an empty container, never an error the
	// client would keep retrying.
	subs.searchErr = fmt.Errorf("provider down")
	rec := f.get(t, "/library/metadata/"+key+"/subtitles?language=en")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<MediaContainer size="0"`) {
		t.Fatalf("provider error: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, "/library/metadata/1000000000999999/subtitles?language=en"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown title: %d", rec.Code)
	}
}

// Issue #268: Plex Web's subtitle search modal spins until an "ended"
// subtitle.download ActivityNotification whose uuid matches the PUT
// response's X-Plex-Activity header arrives on its event connection. The
// event follows the response so the client has stored the id first.
func TestSubtitlePickEndsPlexActivity(t *testing.T) {
	saved := subtitleActivityDelay
	subtitleActivityDelay = 100 * time.Millisecond
	t.Cleanup(func() { subtitleActivityDelay = saved })
	f, subs := newSubtitleFixture(t)
	subs.offers = []SubtitleOffer{{ID: 101, Language: "en", Title: "Fight.Club.1999.1080p.BluRay", Provider: "OpenSubtitles", Score: 80}}
	sub, ok := f.srv.notifier.subscribe([]string{"activity"}, &ownerUser)
	if !ok {
		t.Fatal("subscribe")
	}
	defer f.srv.notifier.unsubscribe(sub)
	key := fightClub.Key.String()

	mc := decodeJSON(t, f.get(t, "/library/metadata/"+key+"/subtitles?language=en", jsonAccept...))
	offerKey := mc["Stream"].([]any)[0].(map[string]any)["key"].(string)
	rec := f.request(t, http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape(offerKey), nil)
	activity := rec.Header().Get("X-Plex-Activity")
	if rec.Code != http.StatusOK || activity == "" {
		t.Fatalf("pick: %d activity=%q", rec.Code, activity)
	}

	select {
	case msg := <-sub.ch:
		if strings.Contains(string(msg.data), `"event":"started"`) {
			select {
			case msg = <-sub.ch:
			case <-time.After(5 * time.Second):
				t.Fatal("no ended after started")
			}
		}
		var nc struct {
			NotificationContainer struct {
				Type                 string `json:"type"`
				ActivityNotification []struct {
					Event    string `json:"event"`
					UUID     string `json:"uuid"`
					Activity struct {
						UUID     string          `json:"uuid"`
						Type     string          `json:"type"`
						Response json.RawMessage `json:"Response"`
					} `json:"Activity"`
				} `json:"ActivityNotification"`
			} `json:"NotificationContainer"`
		}
		if err := json.Unmarshal(msg.data, &nc); err != nil {
			t.Fatalf("decode %s: %v", msg.data, err)
		}
		c := nc.NotificationContainer
		if msg.typ != "activity" || c.Type != "activity" || len(c.ActivityNotification) != 1 {
			t.Fatalf("notification %s", msg.data)
		}
		a := c.ActivityNotification[0]
		if a.Event != "ended" || a.UUID != activity || a.Activity.UUID != activity || a.Activity.Type != "subtitle.download" || strings.Contains(string(a.Activity.Response), "error") {
			t.Fatalf("activity %s, want ended subtitle.download %q without error", msg.data, activity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no subtitle.download activity ended; Plex Web keeps spinning")
	}

	// A failed pick answers an error status and ends nothing.
	if rec := f.request(t, http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape("/library/streams/999"), nil); rec.Code != http.StatusNotFound || rec.Header().Get("X-Plex-Activity") != "" {
		t.Fatalf("unknown offer: %d activity=%q", rec.Code, rec.Header().Get("X-Plex-Activity"))
	}
	select {
	case msg := <-sub.ch:
		t.Fatalf("failed pick published %s", msg.data)
	case <-time.After(3 * subtitleActivityDelay):
	}
}

func (f *fixture) requestBody(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	req.RemoteAddr = "192.168.1.50:51000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if _, ok := hdr["X-Plex-Token"]; !ok && !strings.Contains(target, "X-Plex-Token") {
		req.Header.Set("X-Plex-Token", ownerToken)
	}
	if _, ok := hdr["X-Plex-Client-Identifier"]; !ok {
		req.Header.Set("X-Plex-Client-Identifier", "client-1")
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestSubtitleUpload(t *testing.T) {
	// Status grammar measured on PMS 1.43.4 (recorded measurement,
	// cycle 11): empty body is 406 before title or
	// format; no title and a title with no extension are 500; format is
	// case-sensitive (SRT=400, srt=200); srt/ass/ssa/smi/txt/psb pass,
	// vtt is accepted here as an Ether extra.
	f, subs := newSubtitleFixture(t)
	key := fightClub.Key.String()
	body := []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.srt&format=srt", body, map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusForbidden {
		t.Fatalf("friend upload: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.srt&format=srt", body, nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("upload: %d %q", rec.Code, rec.Body.String())
	}
	uploadID := externalID(fightClub.Key, "ether-subtitle:upload-movie.en.srt")
	found := false
	for _, tag := range subtitleTags(f.get(t, "/library/metadata/"+key).Body.String()) {
		if strings.Contains(tag, fmt.Sprintf(`id="%d"`, uploadID)) {
			found = true
			if !strings.Contains(tag, `selected="1"`) || !strings.Contains(tag, `external="1"`) {
				t.Fatalf("uploaded track not selected: %s", tag)
			}
		}
	}
	if !found {
		t.Fatalf("uploaded track not listed; files=%v", subs.files)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.srt&format=SRT", body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("uppercase format: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.srt&format=srt", nil, nil); rec.Code != http.StatusNotAcceptable {
		t.Fatalf("empty body: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles", nil, nil); rec.Code != http.StatusNotAcceptable {
		t.Fatalf("empty body before title: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?format=srt", body, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing title: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie&format=srt", body, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("title without extension: %d", rec.Code)
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.psb&format=psb", body, nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("psb: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.requestBody(t, http.MethodPost, "/library/metadata/"+key+"/subtitles?title=movie.en.vtt&format=vtt", body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("vtt: %d", rec.Code)
	}
}

func TestSubtitleSearchWithoutServiceIsEmpty(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/subtitles?language=en")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<MediaContainer size="0"`) {
		t.Fatalf("search without service: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTranscodeSidecarSubtitlesFollowSessionOffset(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	f.playback.info = probedInfo
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	key := fightClub.Key.String()
	f.get(t, "/library/metadata/"+key)
	releaseID := externalID(fightClub.Key, "ether-subtitle:release-en")
	path := url.QueryEscape("/library/metadata/" + key)
	f.get(t, decisionPath(fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=%s&session=hls1&protocol=hls&offset=30&directPlay=0&directStream=1&subtitleStreamID=%d", path, releaseID)))
	rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=%s&session=hls1&protocol=hls&offset=30&directPlay=0&directStream=1&subtitleStreamID=%d", path, releaseID))
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, fmt.Sprintf("/video/:/transcode/universal/subtitles?path=%s&session=hls1&offset=30&directPlay=0&directStream=1&subtitleStreamID=%d", path, releaseID))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-subrip" || !strings.HasPrefix(rec.Body.String(), "1\n00:00:05,000") {
		t.Fatalf("sidecar: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// The session's own selection applies without an explicit id; WebVTT on request.
	rec = f.get(t, "/video/:/transcode/universal/subtitles?session=hls1&format=webvtt")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/vtt" || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("session sidecar: %d %q", rec.Code, rec.Body.String())
	}
	// Another device cannot read the session.
	rec = f.get(t, "/video/:/transcode/universal/subtitles?session=hls1", "X-Plex-Client-Identifier", "client-2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<MediaContainer size="0"`) {
		t.Fatalf("foreign client: %d %s", rec.Code, rec.Body.String())
	}
	// Subtitles off: nothing to serve.
	f.get(t, decisionPath(fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=%s&session=hls2&protocol=hls&directPlay=0&directStream=1&subtitleStreamID=0", path)))
	f.get(t, fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=%s&session=hls2&protocol=hls&directPlay=0&directStream=1&subtitleStreamID=0", path))
	rec = f.get(t, "/video/:/transcode/universal/subtitles?session=hls2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<MediaContainer size="0"`) {
		t.Fatalf("no subtitle: %d %s", rec.Code, rec.Body.String())
	}
}

const (
	englishSidecarSRT = "1\n00:02:16,000 --> 00:02:20,000\nThis blade has a dark past.\n\n"
	spanishSidecarSRT = "1\n00:02:16,000 --> 00:02:20,000\nEsta hoja tiene un pasado oscuro.\n\n"
)

var appleTV = []string{
	"X-Plex-Product", "Plex for Apple TV",
	"X-Plex-Platform", "tvOS",
	"X-Plex-Device", "Apple TV",
	"X-Plex-Client-Identifier", "apple-tv",
}

func headerMap(pairs ...string) map[string]string {
	h := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h[pairs[i]] = pairs[i+1]
	}
	return h
}

func streamAttr(tag, name string) string {
	m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return m[1]
}

// subtitleChoice is the stream id of a subtitle tag matching language and
// whether it is an external sidecar.
func subtitleChoice(tags []string, langCode string, external bool) string {
	for _, tag := range tags {
		if !strings.Contains(tag, `languageCode="`+langCode+`"`) {
			continue
		}
		isExternal := strings.Contains(tag, `external="1"`)
		if isExternal != external {
			continue
		}
		if id := streamAttr(tag, "id"); id != "" {
			return id
		}
	}
	return ""
}

func putSubtitle(t *testing.T, f *fixture, key, id string) {
	t.Helper()
	rec := f.request(t, http.MethodPut, "/library/parts/"+key+"?subtitleStreamID="+id+"&allParts=1", headerMap(appleTV...))
	if rec.Code != http.StatusOK {
		t.Fatalf("select subtitle %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

// TestExternalSubtitleSwitchOnResume is the Apple TV sequence: Off to English,
// play, stop, Spanish, resume. The detail chip follows the PUT. The next
// decision must name that Spanish SRT and its stream URL must serve the
// Spanish cues, including when the resumed probe already has some other
// Spanish track (so the cached-sidecar list would otherwise drop the file).
func TestExternalSubtitleSwitchOnResume(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	key := fightClub.Key
	keyS := key.String()
	subs.cache(key, sidecar("es", "ether-subtitle:cached-es"))
	subs.files["ether-subtitle:cached-en"] = englishSidecarSRT
	subs.files["ether-subtitle:cached-es"] = spanishSidecarSRT
	f.playback.info = estimateInfo

	meta := f.get(t, "/library/metadata/"+keyS, appleTV...)
	tags := subtitleTags(meta.Body.String())
	enID := subtitleChoice(tags, "eng", true)
	esID := subtitleChoice(tags, "spa", true)
	if enID == "" || esID == "" || enID == esID {
		t.Fatalf("detail sidecar ids en=%q es=%q tags=%q", enID, esID, tags)
	}

	putSubtitle(t, f, keyS, enID)
	play := "/video/:/transcode/universal/decision?path=/library/metadata/" + keyS + "&protocol=hls&directPlay=1&directStream=1&hasMDE=1&session=atv-en"
	rec := f.get(t, play, appleTV...)
	if rec.Code != http.StatusOK {
		t.Fatalf("english decision: %d %s", rec.Code, rec.Body.String())
	}
	assertSidecarDecision(t, f, rec.Body.String(), "eng", englishSidecarSRT)

	stop := f.get(t, "/:/timeline?ratingKey="+keyS+"&state=stopped&time=20000&duration=8340000", appleTV...)
	if stop.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", stop.Code, stop.Body.String())
	}
	putSubtitle(t, f, keyS, esID)
	again := subtitleTags(f.get(t, "/library/metadata/"+keyS, appleTV...).Body.String())
	chosen := selectedTag(again)
	if !strings.Contains(chosen, `id="`+esID+`"`) || !strings.Contains(chosen, `external="1"`) || !strings.Contains(chosen, `languageCode="spa"`) {
		t.Fatalf("detail chip did not persist Spanish: %q", again)
	}

	// The file probed on resume already contains a Spanish track. Listing
	// cached sidecars then skips the external SRT the viewer just chose.
	f.playback.mu.Lock()
	clear(f.playback.sessions)
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, Size: 800_000_000, BitrateKbps: 7600,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Title: "full"},
			{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es", Title: "full"},
		},
	}
	f.playback.mu.Unlock()

	resume := "/video/:/transcode/universal/decision?path=/library/metadata/" + keyS + "&protocol=hls&directPlay=1&directStream=1&hasMDE=1&session=atv-es&offset=135"
	rec = f.get(t, resume, appleTV...)
	if rec.Code != http.StatusOK {
		t.Fatalf("spanish decision: %d %s", rec.Code, rec.Body.String())
	}
	assertSidecarDecision(t, f, rec.Body.String(), "spa", spanishSidecarSRT)
	// The previous English binding still serves English, not the new file.
	enBody := f.get(t, "/library/streams/"+enID, appleTV...)
	if enBody.Code != http.StatusOK || enBody.Body.String() != englishSidecarSRT {
		t.Fatalf("english stream after switch: %d %q", enBody.Code, enBody.Body.String())
	}

	// Reverse: Spanish back to the English SRT, still against the probe that
	// already has an English subtitle.
	putSubtitle(t, f, keyS, enID)
	f.playback.mu.Lock()
	clear(f.playback.sessions)
	f.playback.mu.Unlock()
	back := "/video/:/transcode/universal/decision?path=/library/metadata/" + keyS + "&protocol=hls&directPlay=1&directStream=1&hasMDE=1&session=atv-en2&offset=135"
	rec = f.get(t, back, appleTV...)
	if rec.Code != http.StatusOK {
		t.Fatalf("english resume: %d %s", rec.Code, rec.Body.String())
	}
	assertSidecarDecision(t, f, rec.Body.String(), "eng", englishSidecarSRT)

	// The session subtitle endpoint follows the resumed choice as well.
	sub := f.get(t, "/video/:/transcode/universal/subtitles?session=atv-en2", appleTV...)
	if sub.Code != http.StatusOK || !strings.Contains(sub.Body.String(), "This blade has a dark past.") {
		t.Fatalf("session english sidecar: %d %q", sub.Code, sub.Body.String())
	}
}

// TestExplicitSubtitleWinsOverOffMode is the Apple TV detail chip: the
// viewer's saved mode is off, they PUT a Spanish SRT, and the next metadata
// response must keep that stream selected. Mode off is a default, not a
// veto of a per-title choice.
func TestExplicitSubtitleWinsOverOffMode(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	f.srv.d.TrackPreferences = prefs
	rules := trackprefs.Default()
	rules.SubtitleMode = "off"
	if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}
	key := fightClub.Key
	keyS := key.String()
	subs.cache(key, sidecar("es", "ether-subtitle:cached-es"))
	subs.files["ether-subtitle:cached-es"] = spanishSidecarSRT
	f.playback.info = estimateInfo
	meta := f.get(t, "/library/metadata/"+keyS, appleTV...)
	esID := subtitleChoice(subtitleTags(meta.Body.String()), "spa", true)
	if esID == "" {
		t.Fatalf("spanish srt missing from the menu: %s", meta.Body.String())
	}
	putSubtitle(t, f, keyS, esID)
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, Size: 800_000_000, BitrateKbps: 7600,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Title: "full"},
			{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es", Title: "full"},
		},
	}
	again := subtitleTags(f.get(t, "/library/metadata/"+keyS, appleTV...).Body.String())
	chosen := selectedTag(again)
	if !strings.Contains(chosen, `id="`+esID+`"`) || !strings.Contains(chosen, `external="1"`) || !strings.Contains(chosen, `languageCode="spa"`) {
		t.Fatalf("off mode cleared the explicit Spanish choice: %q", again)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM trackprefs_memory WHERE user_id=? AND kind='subtitle'`, ownerUser.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("explicit subtitle was not stored, trackprefs_memory rows=%d", n)
	}
}

func TestEmbeddedToExternalSubtitleSwitch(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	f.srv.d.Transcoder = &extractTranscoder{fakeTranscoder: f.trans, out: "WEBVTT\n\n00:02:16.000 --> 00:02:20.000\nEmbedded english\n"}
	key := fightClub.Key
	keyS := key.String()
	subs.cache(key, sidecar("es", "ether-subtitle:cached-es"))
	subs.files["ether-subtitle:cached-es"] = spanishSidecarSRT
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, Size: 800_000_000, BitrateKbps: 7600,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Title: "full"},
		},
	}

	meta := f.get(t, "/library/metadata/"+keyS, appleTV...)
	tags := subtitleTags(meta.Body.String())
	enID := subtitleChoice(tags, "eng", false)
	esID := subtitleChoice(tags, "spa", true)
	if enID == "" || esID == "" {
		t.Fatalf("embedded english or external spanish missing: %q", tags)
	}
	putSubtitle(t, f, keyS, enID)
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+keyS+"&protocol=hls&directPlay=0&directStream=1&session=emb-ext-1", appleTV...)
	if rec.Code != http.StatusOK {
		t.Fatalf("embedded decision: %d %s", rec.Code, rec.Body.String())
	}
	selected := selectedTag(subtitleTags(rec.Body.String()))
	if strings.Contains(selected, `external="1"`) || !strings.Contains(selected, `languageCode="eng"`) {
		t.Fatalf("embedded english not selected: %s", selected)
	}

	f.playback.mu.Lock()
	clear(f.playback.sessions)
	f.playback.info.Streams = append(f.playback.info.Streams, core.StreamInfo{Index: 4, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es", Title: "full"})
	f.playback.mu.Unlock()
	putSubtitle(t, f, keyS, esID)
	rec = f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+keyS+"&protocol=hls&directPlay=0&directStream=1&session=emb-ext-2", appleTV...)
	if rec.Code != http.StatusOK {
		t.Fatalf("external decision: %d %s", rec.Code, rec.Body.String())
	}
	assertSidecarDecision(t, f, rec.Body.String(), "spa", spanishSidecarSRT)
}

func assertSidecarDecision(t *testing.T, f *fixture, body, langCode, want string) {
	t.Helper()
	selected := selectedTag(subtitleTags(body))
	if selected == "" || !strings.Contains(selected, `languageCode="`+langCode+`"`) || !strings.Contains(selected, `external="1"`) {
		t.Fatalf("decision subtitle %s: %q\n%s", langCode, selected, body)
	}
	key := streamAttr(selected, "key")
	if !strings.HasPrefix(key, "/library/streams/") {
		t.Fatalf("sidecar key: %q", selected)
	}
	rec := f.get(t, key, appleTV...)
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("GET %s: %d %q", key, rec.Code, rec.Body.String())
	}
}

// TestWaysToWatchSubtitleReread is the Apple TV discover path: search,
// More Ways to Watch, then More › Subtitles. The detail is addressed by a
// plex:// guid and by Play Version part ids (the 8×10^15 block and the
// 9×10^15 ids the registry issues). The client PUTs with the plex.tv token
// and allParts=1, then re-reads the guid, the rating key, and the part id.
func TestWaysToWatchSubtitleReread(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	pb.info = estimateInfo
	subs := newFakeSubtitles()
	subs.cache(fightClub.Key, sidecar("es", "ether-subtitle:cached-es"))
	subs.files["ether-subtitle:cached-es"] = spanishSidecarSRT
	f.srv.d.Subtitles = subs

	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	rules := trackprefs.Default()
	rules.SubtitleMode = "off"
	if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}
	f.srv.d.TrackPreferences = prefs

	const guid = "plex://movie/5d77688123d5a3001f4ed3df"
	if _, err := db.Exec(`INSERT INTO plexguid_mappings(rating_key, guid, checked_at) VALUES(?,?,?)`, int64(fightClub.Key), guid, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	pg := plexguid.New(db, plexguid.Options{})
	t.Cleanup(func() { pg.Close() })
	f.srv.d.LibraryGUIDs = pg

	const plexToken = "plex-tv-user-token"
	f.srv.d.Auth.(*fakeAuth).users[plexToken] = ownerUser
	hdr := append([]string{"X-Plex-Token", plexToken}, appleTV...)

	key := fightClub.Key.String()
	meta := f.get(t, "/library/metadata/"+key, hdr...)
	if meta.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", meta.Code, meta.Body.String())
	}
	esID := subtitleChoice(subtitleTags(meta.Body.String()), "spa", true)
	if esID == "" {
		t.Fatalf("spanish srt missing from the discover detail: %s", meta.Body.String())
	}
	variant := variantPartRE.FindStringSubmatch(meta.Body.String())
	if variant == nil {
		t.Fatalf("variant part id missing:\n%s", meta.Body.String())
	}
	guidPath := "/library/metadata/" + url.PathEscape(guid)

	put := func(partID string, status int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/library/parts/"+partID+"?subtitleStreamID="+esID+"&allParts=1", nil)
		req.RemoteAddr = "192.168.1.50:51000"
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("PUT part %s: %d %s", partID, rec.Code, rec.Body.String())
		}
	}
	assertSpanish := func(path string) {
		t.Helper()
		rec := f.get(t, path, hdr...)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		chosen := selectedTag(subtitleTags(rec.Body.String()))
		if !strings.Contains(chosen, `id="`+esID+`"`) || !strings.Contains(chosen, `languageCode="spa"`) {
			t.Fatalf("GET %s did not mark Spanish selected: %q", path, chosen)
		}
	}

	// A different title's part must not take the stream.
	put(alien.Key.String(), http.StatusBadRequest)
	if chosen := selectedTag(subtitleTags(f.get(t, "/library/metadata/"+key, hdr...).Body.String())); chosen != "" {
		t.Fatalf("cross-title part stored a selection: %s", chosen)
	}

	// A valid stream cannot authorize an unregistered part.
	put("8000000000000001", http.StatusNotFound)

	// Registered variant part, then the Auto part. Re-read the part id itself
	// and the plex:// guid the discover row keeps.
	put(variant[1], http.StatusOK)
	assertSpanish("/library/metadata/" + key)
	assertSpanish("/library/metadata/" + variant[1])
	assertSpanish(guidPath)

	put(key, http.StatusOK)
	assertSpanish("/library/metadata/" + key)
	assertSpanish(guidPath)
}

// TestSubtitleChoiceSurvivesVariantReorder is the Apple TV More Ways to
// Watch flake: the viewer picks Spanish SRT, then the variant list reorders
// (a different release becomes Auto, so the same part id names a different
// release) and the Spanish track moves to a new index. A PGS track now sits
// in the old slot with the old stream index. The choice is the title plus
// language, codec, and source, so the re-read and the direct-play decision
// still select that SRT, at the same stream id.
func TestSubtitleChoiceSurvivesVariantReorder(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	initial := []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720},
		{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
		{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
		{Index: 3, Type: "subtitle", Codec: "srt", Language: "es"},
	}
	for i := range f.resolver.byKey[fightClub.Key] {
		if f.resolver.byKey[fightClub.Key][i].Auto {
			f.resolver.byKey[fightClub.Key][i].Info.Streams = initial
		}
	}
	pb.info.Streams = initial
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	rules := trackprefs.Default()
	rules.SubtitleMode = "off"
	if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}
	f.srv.d.TrackPreferences = prefs

	key := fightClub.Key.String()
	meta := f.get(t, "/library/metadata/"+key, appleTV...)
	if meta.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", meta.Code, meta.Body.String())
	}
	esID := ""
	for _, tag := range subtitleTags(meta.Body.String()) {
		if strings.Contains(tag, `languageCode="spa"`) && strings.Contains(tag, `codec="srt"`) && !strings.Contains(tag, `external="1"`) {
			esID = streamAttr(tag, "id")
		}
	}
	if esID == "" {
		t.Fatalf("spanish srt missing:\n%s", meta.Body.String())
	}
	partID := strconv.FormatInt(f.srv.variantIDs.idFor(fightClub.Key, "rel-4k"), 10)
	req := httptest.NewRequest(http.MethodPut, "/library/parts/"+partID+"?subtitleStreamID="+esID+"&allParts=1", nil)
	req.RemoteAddr = "192.168.1.50:51000"
	req.Header.Set("X-Plex-Token", ownerToken)
	for i := 0; i+1 < len(appleTV); i += 2 {
		req.Header.Set(appleTV[i], appleTV[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}

	// A warmer candidate becomes Auto, so the title-key part is a different
	// release, and the Spanish SRT is no longer at index 3.
	var next []core.ReleaseVariant
	for _, id := range []string{"rel-4k", "rel-compat", "rel-auto"} {
		for _, v := range f.resolver.byKey[fightClub.Key] {
			if v.Release.ID == id {
				v.Auto = id == "rel-4k"
				next = append(next, v)
			}
		}
	}
	moved := []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720},
		{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
		{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es"},
		{Index: 4, Type: "subtitle", Codec: "subrip", Language: "en"},
		{Index: 9, Type: "subtitle", Codec: "srt", Language: "es"},
	}
	for i := range next {
		if next[i].Auto {
			next[i].Info.Streams = moved
		}
	}
	f.resolver.byKey[fightClub.Key] = next
	pb.info.Streams = moved

	assertSpanishSRT := func(path string) {
		t.Helper()
		body := f.get(t, path, appleTV...).Body.String()
		chosen := selectedTag(subtitleTags(body))
		if !strings.Contains(chosen, `id="`+esID+`"`) || !strings.Contains(chosen, `languageCode="spa"`) || !strings.Contains(chosen, `codec="srt"`) {
			t.Fatalf("GET %s selected %q, want Spanish SRT id %s", path, chosen, esID)
		}
	}
	assertSpanishSRT("/library/metadata/" + key)
	assertSpanishSRT("/library/metadata/" + partID)

	dec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&mediaIndex=0&directPlay=1&directStream=1&protocol=hls&session=reorder-sub", appleTV...)
	if dec.Code != http.StatusOK {
		t.Fatalf("decision: %d %s", dec.Code, dec.Body.String())
	}
	tags := subtitleTags(dec.Body.String())
	chosen := selectedTag(tags)
	if chosen == "" && len(tags) > 0 {
		chosen = tags[0]
	}
	if !strings.Contains(chosen, `id="`+esID+`"`) || !strings.Contains(chosen, `codec="srt"`) {
		t.Fatalf("direct play dropped Spanish SRT: %q\n%s", chosen, dec.Body.String())
	}
}

func TestDetailDoesNotDownloadSidecars(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/library/metadata/"+breakingBad.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("show: %d", rec.Code)
	}
}

func waitSubtitlePick(t *testing.T, f *fixture, key core.RatingKey) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sel := f.srv.selections.get(ownerUser.ID, int64(key)); sel.subtitleStream != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("subtitle pick did not finish")
}

func TestSubtitleSearchGrammarAndLocalIdentities(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	subs.offers = []SubtitleOffer{{ID: 101, Language: "en"}}
	root := "/library/metadata/" + fightClub.Key.String() + "/subtitles"
	for _, tc := range []struct {
		query string
		code  int
	}{{"", 400}, {"?language=", 500}, {"?language=eng", 500}, {"?language=xx", 500}, {"?language=EN", 200}, {"?language=en-US", 200}, {"?language=de", 200}} {
		if rec := f.get(t, root+tc.query); rec.Code != tc.code {
			t.Fatalf("%s: %d want %d", tc.query, rec.Code, tc.code)
		}
	}
	a := decodeJSON(t, f.get(t, root+"?language=en", jsonAccept...))["Stream"].([]any)[0].(map[string]any)
	b := decodeJSON(t, f.get(t, root+"?language=en", jsonAccept...))["Stream"].([]any)[0].(map[string]any)
	if a["id"] == b["id"] || a["key"] == b["key"] {
		t.Fatalf("search reused local identity: %v / %v", a, b)
	}
	if rec := f.request(t, http.MethodPut, root+"?language=en", nil); rec.Code != 200 {
		t.Fatalf("PUT search: %d", rec.Code)
	}
}

type blockingSubtitleDownload struct {
	*fakeSubtitles
	entered  chan struct{}
	complete chan struct{}
}

func (b *blockingSubtitleDownload) DownloadSubtitle(ctx context.Context, t *core.Title, u *core.User, id int64) (core.StreamInfo, error) {
	close(b.entered)
	select {
	case <-b.complete:
		return b.fakeSubtitles.DownloadSubtitle(ctx, t, u, id)
	case <-ctx.Done():
		return core.StreamInfo{}, ctx.Err()
	}
}
func TestSubtitleApplyReturnsBeforeDownload(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	subs.offers = []SubtitleOffer{{ID: 101, Language: "en"}}
	blocking := &blockingSubtitleDownload{subs, make(chan struct{}), make(chan struct{})}
	f.srv.d.Subtitles = blocking
	defer close(blocking.complete)
	root := "/library/metadata/" + fightClub.Key.String() + "/subtitles"
	mc := decodeJSON(t, f.get(t, root+"?language=en", jsonAccept...))
	key := mc["Stream"].([]any)[0].(map[string]any)["key"].(string)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- f.request(t, http.MethodPut, root+"?key="+url.QueryEscape(key), nil) }()
	select {
	case rec := <-response:
		if rec.Code != 200 || rec.Header().Get("X-Plex-Activity") == "" || rec.Body.Len() != 0 {
			t.Fatalf("async response: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("apply waited for download")
	}
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	if sel := f.srv.selections.get(ownerUser.ID, int64(fightClub.Key)); sel.subtitleStream != nil {
		t.Fatal("track published before download")
	}
}

func TestSubtitleOfferBoundAndViewerScope(t *testing.T) {
	var offers subtitleOffers
	first := offers.add(fightClub.Key, ownerUser.ID, 101)
	if _, ok := offers.get(first, alien.Key, ownerUser.ID); ok {
		t.Fatal("foreign title offer accepted")
	}
	if _, ok := offers.get(first, fightClub.Key, ownerUser.ID+1); ok {
		t.Fatal("foreign viewer offer accepted")
	}
	for range maxSubtitleOffers {
		offers.add(fightClub.Key, ownerUser.ID, 101)
	}
	if _, ok := offers.get(first, fightClub.Key, ownerUser.ID); ok {
		t.Fatal("old offer not evicted")
	}
	if len(offers.rows) != maxSubtitleOffers {
		t.Fatalf("offer bound: %d", len(offers.rows))
	}
}
