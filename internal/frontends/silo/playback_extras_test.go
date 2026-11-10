package silo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type skipPreferences struct{ values map[string]string }

func (skipPreferences) Get(context.Context, *core.Viewer, string, string) (json.RawMessage, error) {
	return nil, frontend.ErrPreferencesNotFound
}
func (skipPreferences) Put(context.Context, *core.Viewer, string, string, json.RawMessage) error {
	return nil
}
func (skipPreferences) Delete(context.Context, *core.Viewer, string, string) error { return nil }
func (p skipPreferences) GetVersioned(_ context.Context, _ *core.Viewer, _ string, key string) (json.RawMessage, int64, time.Time, error) {
	if raw, ok := p.values[key]; ok {
		return json.RawMessage(raw), 1, time.Unix(10, 0).UTC(), nil
	}
	return nil, 0, time.Time{}, frontend.ErrPreferencesNotFound
}
func (skipPreferences) PutVersioned(context.Context, *core.Viewer, string, string, json.RawMessage) (int64, time.Time, error) {
	return 0, time.Time{}, nil
}

func TestWatchMarkersStayOnTheirFileAndHonourIntroSkip(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	key := core.MovieKey(9)
	media := frontend.MediaInfo{Container: "mp4", Duration: time.Minute, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264", Height: 1080}, {Type: "audio", Codec: "aac", Channels: 2, Language: "en"}}}
	phase := "watch"
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		if phase == "seek" {
			return frontend.ItemDetail{Title: frontend.Title{Key: key}, Versions: []frontend.Version{{ID: "one", Markers: []frontend.Marker{{Type: "intro", Start: 0, End: 10 * time.Second}, {Type: "recap", Start: 10 * time.Second, End: 20 * time.Second}, {Type: "credits", Start: 50 * time.Second, End: 55 * time.Second}}}}}, nil
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: core.TypeMovie, Title: "Film", Released: time.Unix(10000, 0)}, Markers: []frontend.Marker{{Type: "intro", Start: 50 * time.Second, End: 55 * time.Second}}, Versions: []frontend.Version{
			{ID: "one", Media: media, Markers: []frontend.Marker{{Type: "intro", Start: time.Second, End: 10 * time.Second}, {Type: "credits", Start: 50 * time.Second, End: 55 * time.Second}}},
			{ID: "two", Media: media, Markers: []frontend.Marker{{Type: "recap", Start: 2 * time.Second, End: 8 * time.Second}}},
		}}, nil
	}}
	prefs := skipPreferences{values: map[string]string{"playback.intro_skip_mode": `"never"`}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp, Sessions: frontendtest.Sessions{}, SourceOffers: testSourceOffers{}, VersionHandles: testVersionHandles{}, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}, Preferences: prefs})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/watch/"+ids.RatingKeyGUID(key), nil)
	req.Header.Set("Authorization", "Bearer grant")
	req.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, `"kind":"intro"`) || !strings.Contains(body, `"kind":"credits"`) || !strings.Contains(body, `"kind":"recap"`) {
		t.Fatalf("markers %d %s", w.Code, body)
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		if err := doc.Validate(http.MethodGet, req.URL.Path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	lookups := 0
	previous := exp.ItemFunc
	exp.ItemFunc = func(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		lookups++
		phase = "seek"
		return previous(ctx, req)
	}
	handler := inst.(*instance)
	handler.host.Experience = exp
	ask := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	ask.Header.Set("X-Silo-Device-Id", "tv")
	handler.host.Preferences = skipPreferences{values: map[string]string{"playback.intro_skip_mode": `"ask"`}}
	if got := handler.skipIntroAtStart(ask, viewer, frontend.VersionScope{}, key, "", time.Second); got != time.Second || lookups != 0 {
		t.Fatalf("ask seek %s lookups %d", got, lookups)
	}
	handler.host.Preferences = skipPreferences{values: map[string]string{"playback.intro_skip_mode": `"always"`}}
	if got := handler.skipIntroAtStart(ask, viewer, frontend.VersionScope{}, key, "", time.Second); got != 10*time.Second || lookups != 1 {
		t.Fatalf("always seek %s lookups %d", got, lookups)
	}
	handler.host.Preferences = skipPreferences{values: map[string]string{"playback.auto_skip_recap": `true`, "playback.auto_skip_credits": `true`}}
	for position, want := range map[time.Duration]time.Duration{time.Second: time.Second, 11 * time.Second: 20 * time.Second, 51 * time.Second: 55 * time.Second} {
		if got := handler.skipIntroAtStart(ask, viewer, frontend.VersionScope{}, key, "", position); got != want {
			t.Fatalf("skip at %s = %s want %s", position, got, want)
		}
	}

}

type siloSubtitles struct {
	enabled   bool
	downloads int
}

func (s *siloSubtitles) Enabled() bool { return s.enabled }
func (s *siloSubtitles) Search(context.Context, *core.Viewer, core.Device, core.RatingKey, string) ([]frontend.SubtitleOffer, error) {
	return []frontend.SubtitleOffer{{ID: "42", Language: "en", Name: "Release", Format: "srt", Downloads: 3, Rating: 1.5, HearingImpaired: false}}, nil
}
func (s *siloSubtitles) Download(context.Context, *core.Viewer, core.Device, core.RatingKey, string) (int, error) {
	s.downloads++
	return 4, nil
}
func (*siloSubtitles) OpenFormat(string, string) (io.ReadCloser, string, error) {
	return nil, "", core.ErrNotFound
}
func (*siloSubtitles) OpenFormatFrom(string, string, string) (io.ReadCloser, string, error) {
	return nil, "", core.ErrNotFound
}
func (*siloSubtitles) CachedSidecars(context.Context, *frontend.Title, *frontend.User, []frontend.StreamInfo) []frontend.StreamInfo {
	return nil
}
func (*siloSubtitles) SearchSubtitles(context.Context, *frontend.Title, *frontend.User, string, string) ([]frontend.SubtitleSearchOffer, error) {
	return nil, nil
}
func (*siloSubtitles) DownloadSubtitle(context.Context, *frontend.Title, *frontend.User, int64) (frontend.StreamInfo, error) {
	return frontend.StreamInfo{}, core.ErrNotFound
}
func (*siloSubtitles) UploadSubtitle(context.Context, *frontend.Title, *frontend.User, string, string, string, []byte) (frontend.StreamInfo, error) {
	return frontend.StreamInfo{}, core.ErrNotFound
}
func (*siloSubtitles) DeleteSubtitle(context.Context, frontend.RatingKey, *frontend.User, frontend.StreamInfo) error {
	return nil
}

func TestSiloSubtitleSearchDoesNotDownloadUntilSelection(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	catalog := &siloSubtitles{enabled: true}
	exp := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key != 1000000000000001 {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: req.Key}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp, SourceOffers: testSourceOffers{}, Capabilities: frontendtest.Capabilities{}, Subtitles: catalog})
	if err != nil {
		t.Fatal(err)
	}
	search := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/subtitles/search", strings.NewReader(`{"media_file_id":"101","languages":["en"]}`))
	req.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(search, req)
	if search.Code != 200 || catalog.downloads != 0 || !strings.Contains(search.Body.String(), `"id":"42"`) || !strings.Contains(search.Body.String(), `"provider":"opensubtitles"`) {
		t.Fatalf("search %d %s", search.Code, search.Body.String())
	}
	status := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v2/subtitles/providers/status", nil)
	statusReq.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(status, statusReq)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"enabled":true`) || !strings.Contains(status.Body.String(), `"opensubtitles"`) {
		t.Fatalf("status %d %s", status.Code, status.Body.String())
	}
	download := httptest.NewRecorder()
	body := `{"media_file_id":"101","provider":"opensubtitles","subtitle_id":"42","language":"en","release_name":"Release","score":1.5,"hearing_impaired":false}`
	down := httptest.NewRequest(http.MethodPost, "/api/v2/subtitles/download", strings.NewReader(body))
	down.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(download, down)
	if download.Code != 200 || catalog.downloads != 1 || !strings.Contains(download.Body.String(), `"offset_ms":0`) {
		t.Fatalf("download %d downloads %d %s", download.Code, catalog.downloads, download.Body.String())
	}
	hidden := httptest.NewRecorder()
	miss := httptest.NewRequest(http.MethodPost, "/api/v2/subtitles/search", strings.NewReader(`{"media_file_id":"102","languages":["en"]}`))
	miss.Header.Set("Authorization", "Bearer grant")
	inst.Handler().ServeHTTP(hidden, miss)
	if hidden.Code != 404 || catalog.downloads != 1 {
		t.Fatalf("hidden %d downloads %d", hidden.Code, catalog.downloads)
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		for _, tc := range []struct {
			method, path string
			w            *httptest.ResponseRecorder
		}{
			{http.MethodPost, "/api/v2/subtitles/search", search},
			{http.MethodGet, "/api/v2/subtitles/providers/status", status},
			{http.MethodPost, "/api/v2/subtitles/download", download},
		} {
			if err := doc.Validate(tc.method, tc.path, tc.w.Code, tc.w.Header().Get("Content-Type"), tc.w.Body.Bytes()); err != nil {
				t.Fatalf("%s %s: %v\n%s", tc.method, tc.path, err, tc.w.Body.String())
			}
		}
	}
}
