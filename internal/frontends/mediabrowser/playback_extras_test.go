package mediabrowser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type extraCatalog struct {
	opens int
	url   string
}

func (c *extraCatalog) Lookup(key core.RatingKey) (frontend.Extra, bool) {
	if key != core.ExtraKey(4) {
		return frontend.Extra{}, false
	}
	return frontend.Extra{Key: key, ParentKey: core.MovieKey(9), Title: "Preview", Kind: "trailer"}, true
}

func (c *extraCatalog) Open(context.Context, core.RatingKey) (string, string, map[string]string, error) {
	c.opens++
	return "mp4", c.url, map[string]string{"Authorization": "secret"}, nil
}
func (*extraCatalog) Enabled() bool { return true }
func (*extraCatalog) List(context.Context, *core.Title, string) ([]frontend.ExtraClip, error) {
	return nil, nil
}
func (*extraCatalog) Clip(core.RatingKey) (frontend.ExtraClip, bool) {
	return frontend.ExtraClip{}, false
}
func (*extraCatalog) Resolve(context.Context, frontend.ExtraClip) (frontend.ResolvedExtra, error) {
	return frontend.ResolvedExtra{}, core.ErrNotFound
}
func (*extraCatalog) PrewarmBytes() int64 { return 0 }

type cinemaCatalog struct{ clips []frontend.Extra }

func (c cinemaCatalog) Trailers(context.Context, *core.Viewer, core.Device, core.RatingKey, int) ([]frontend.Extra, error) {
	return c.clips, nil
}

type subtitleCatalog struct {
	enabled   bool
	downloads int
	searches  int
	lastKey   core.RatingKey
}

func (c *subtitleCatalog) Enabled() bool { return c.enabled }
func (c *subtitleCatalog) Search(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey, language string) ([]frontend.SubtitleOffer, error) {
	c.searches++
	c.lastKey = key
	if language != "en" {
		return nil, nil
	}
	return []frontend.SubtitleOffer{{
		ID: "42", Language: "en", Name: "Release", Provider: "OpenSubtitles", Format: "srt",
		Author: "Ada", Downloads: 4, Rating: 8.5, HearingImpaired: true, HashMatch: true,
	}}, nil
}
func (c *subtitleCatalog) Download(context.Context, *core.Viewer, core.Device, core.RatingKey, string) (int, error) {
	c.downloads++
	return 6, nil
}
func (*subtitleCatalog) OpenFormat(string, string) (io.ReadCloser, string, error) {
	return nil, "", core.ErrNotFound
}
func (*subtitleCatalog) OpenFormatFrom(string, string, string) (io.ReadCloser, string, error) {
	return nil, "", core.ErrNotFound
}
func (*subtitleCatalog) CachedSidecars(context.Context, *frontend.Title, *frontend.User, []frontend.StreamInfo) []frontend.StreamInfo {
	return nil
}
func (*subtitleCatalog) SearchSubtitles(context.Context, *frontend.Title, *frontend.User, string, string) ([]frontend.SubtitleSearchOffer, error) {
	return nil, nil
}
func (*subtitleCatalog) DownloadSubtitle(context.Context, *frontend.Title, *frontend.User, int64) (frontend.StreamInfo, error) {
	return frontend.StreamInfo{}, core.ErrNotFound
}
func (*subtitleCatalog) UploadSubtitle(context.Context, *frontend.Title, *frontend.User, string, string, string, []byte) (frontend.StreamInfo, error) {
	return frontend.StreamInfo{}, core.ErrNotFound
}
func (*subtitleCatalog) DeleteSubtitle(context.Context, frontend.RatingKey, *frontend.User, frontend.StreamInfo) error {
	return nil
}

func TestLocalTrailersAndFeaturesFollowAuthorizedExtras(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	movie := core.MovieKey(9)
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, frontend.ErrNotFound
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != viewer || req.Key != movie {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: movie, Type: core.TypeMovie, Title: "Film"}, Extras: []frontend.Extra{
			{Key: core.ExtraKey(4), ParentKey: movie, Title: "Preview", Kind: "trailer", Source: "YouTube", SourceID: "clip"},
			{Key: core.ExtraKey(5), ParentKey: movie, Title: "Making of", Kind: "featurette"},
		}}, nil
	}}
	streams := &extraCatalog{url: "http://127.0.0.1/trailer.mp4"}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, ExtraStreams: streams, Cinema: cinemaCatalog{clips: []frontend.Extra{{Key: core.ExtraKey(4), ParentKey: movie, Title: "Preview", Kind: "trailer"}}}})
		if err != nil {
			t.Fatal(err)
		}
		userID, _ := ids.GUID(ids.User, viewer.ID)
		prefix := "/Items/" + ids.RatingKeyGUID(movie)
		if tc.name == "emby" {
			prefix = "/Users/" + userID + prefix
		}
		local := callExtras(inst, prefix+"/LocalTrailers")
		if local.Code != 200 || !strings.Contains(local.Body.String(), "Preview") || strings.Contains(local.Body.String(), "Making of") {
			t.Fatalf("%s trailers %d %s", tc.name, local.Code, local.Body.String())
		}
		features := callExtras(inst, prefix+"/SpecialFeatures")
		if features.Code != 200 || !strings.Contains(features.Body.String(), "Making of") || strings.Contains(features.Body.String(), "Preview") {
			t.Fatalf("%s features %d %s", tc.name, features.Code, features.Body.String())
		}
		intros := callExtras(inst, prefix+"/Intros")
		if intros.Code != 200 || !strings.Contains(intros.Body.String(), "Preview") || !strings.Contains(intros.Body.String(), `"TotalRecordCount":1`) {
			t.Fatalf("%s intros %d %s", tc.name, intros.Code, intros.Body.String())
		}
		hidden := callExtras(inst, strings.Replace(prefix, ids.RatingKeyGUID(movie), ids.RatingKeyGUID(core.MovieKey(3)), 1)+"/LocalTrailers")
		if hidden.Code != 404 {
			t.Fatalf("%s hidden %d", tc.name, hidden.Code)
		}
		validateSpec(t, tc.name, "GET", prefix+"/LocalTrailers", local)
		validateSpec(t, tc.name, "GET", prefix+"/SpecialFeatures", features)
		if tc.name == "jellyfin" || strings.Contains(prefix, "/Users/") {
			validateSpec(t, tc.name, "GET", prefix+"/Intros", intros)
		}
	}
	dto := map[string]any{}
	(&instance{protocol: "emby"}).embyItemDetailDTO(dto, frontend.ItemDetail{Extras: []frontend.Extra{{Kind: "trailer", Source: "YouTube", SourceID: "clip", Title: "Preview"}}})
	remote, _ := dto["RemoteTrailers"].([]any)
	if len(remote) != 1 || remote[0].(map[string]string)["Url"] != "https://www.youtube.com/watch?v=clip" {
		t.Fatalf("remote trailers %#v", dto["RemoteTrailers"])
	}
}

func TestExtraStreamRefusesPrivateTargetsAndHiddenParents(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	allowParent := true
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if !allowParent || req.Key != core.MovieKey(9) {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: req.Key}}, nil
	}}
	streams := &extraCatalog{url: "http://127.0.0.1/trailer.mp4"}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, ExtraStreams: streams, Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/Videos/" + ids.RatingKeyGUID(core.ExtraKey(4)) + "/stream.mp4"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Emby-Token", "valid")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || streams.opens != 1 || strings.Contains(w.Body.String(), "127.0.0.1") || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("private trailer %d opens %d %s", w.Code, streams.opens, w.Body.String())
	}
	streams.opens = 0
	allowParent = false
	hidden := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/Videos/"+ids.RatingKeyGUID(core.ExtraKey(4))+"/stream", nil)
	req.Header.Set("X-Emby-Token", "valid")
	inst.Handler().ServeHTTP(hidden, req)
	if hidden.Code != http.StatusNotFound || streams.opens != 0 {
		t.Fatalf("hidden parent %d opens %d", hidden.Code, streams.opens)
	}
	allowParent = true
	opened := streams.opens
	info := httptest.NewRecorder()
	estimate := httptest.NewRequest(http.MethodPost, "/Items/"+ids.RatingKeyGUID(core.ExtraKey(4))+"/PlaybackInfo?IsPlayback=false", strings.NewReader(`{}`))
	estimate.Header.Set("X-Emby-Token", "valid")
	inst.Handler().ServeHTTP(info, estimate)
	if info.Code != 200 || streams.opens != opened || !strings.Contains(info.Body.String(), `"SupportsDirectPlay":true`) {
		t.Fatalf("estimate %d opens %d %s", info.Code, streams.opens, info.Body.String())
	}
}

func TestRemoteSubtitleSearchDownloadsOnlyTheSelection(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	movie := core.MovieKey(9)
	catalog := &subtitleCatalog{enabled: true}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key != movie {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: movie, Title: "Film"}}, nil
	}}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		catalog.searches, catalog.downloads = 0, 0
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Subtitles: catalog})
		if err != nil {
			t.Fatal(err)
		}
		path := "/Items/" + ids.RatingKeyGUID(movie) + "/RemoteSearch/Subtitles/en"
		if tc.name == "emby" {
			path += "?MediaSourceId=source"
		}
		found := callExtras(inst, path)
		if found.Code != 200 || catalog.downloads != 0 || catalog.lastKey != movie || !strings.Contains(found.Body.String(), `"Id":"42"`) {
			t.Fatalf("%s search %d downloads %d %s", tc.name, found.Code, catalog.downloads, found.Body.String())
		}
		if tc.name == "jellyfin" && strings.Contains(found.Body.String(), `"Language"`) {
			t.Fatalf("jellyfin language field %s", found.Body.String())
		}
		if tc.name == "emby" && !strings.Contains(found.Body.String(), `"IsHearingImpaired":true`) {
			t.Fatalf("emby subtitle %s", found.Body.String())
		}
		downloadPath := "/Items/" + ids.RatingKeyGUID(movie) + "/RemoteSearch/Subtitles/42"
		if tc.name == "emby" {
			downloadPath += "?MediaSourceId=source"
		}
		saved := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, downloadPath, nil)
		req.Header.Set("X-Emby-Token", "valid")
		inst.Handler().ServeHTTP(saved, req)
		want := http.StatusNoContent
		if tc.name == "emby" {
			want = http.StatusOK
		}
		if saved.Code != want || catalog.downloads != 1 {
			t.Fatalf("%s download %d downloads %d %s", tc.name, saved.Code, catalog.downloads, saved.Body.String())
		}
		if tc.name == "emby" && !strings.Contains(saved.Body.String(), `"NewIndex":6`) {
			t.Fatalf("index %s", saved.Body.String())
		}
	}
	policy := (&instance{protocol: "emby", host: frontend.Host{Subtitles: catalog}}).userDTO(httptest.NewRequest("GET", "/", nil), viewer)["Policy"].(map[string]any)
	if policy["EnableSubtitleDownloading"] != true {
		t.Fatal(policy["EnableSubtitleDownloading"])
	}
}

func TestPublicExtraURLRejectsPrivateTargets(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/x", "http://10.1.1.1/x", "http://169.254.1.1/x", "file:///tmp/x", "https://user:pass@example.com/x", "http://localhost/x"} {
		if publicExtraURL(raw) {
			t.Fatal(raw)
		}
	}
	if !publicExtraURL("https://example.com/trailer.mp4") {
		t.Fatal("public trailer")
	}
}

func callExtras(inst frontend.Instance, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Emby-Token", "valid")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	return w
}

func validateSpec(t *testing.T, name, method, path string, w *httptest.ResponseRecorder) {
	t.Helper()
	doc := spectest.Document(t, name)
	if doc == nil {
		return
	}
	if err := doc.Validate(method, path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatalf("%s %s %s: %v\n%s", name, method, path, err, w.Body.String())
	}
}
