package silo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type settingsTracks struct {
	frontendtest.TrackPreferences
	tracks          frontend.TrackPreferences
	bitrate, height int
	quality         string
}

func TestSettingScopesRequireAuthorizedLibraryAndSeries(t *testing.T) {
	v := &frontend.Viewer{ID: 7}
	series := frontend.RatingKey(2000000000000001)
	other := frontend.RatingKey(2000000000000002)
	i := &instance{host: frontend.Host{Experience: frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "3", Name: "Shows"}}, nil
		},
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			if req.Viewer != v || req.Key != series {
				return frontend.ItemDetail{}, errors.New("hidden")
			}
			return frontend.ItemDetail{}, nil
		},
	}}}
	request := func(query string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v2/settings/values/effective?"+query, nil)
		r.Header.Set("X-Silo-Device-Id", "tv")
		return r
	}
	if id, ok := i.settingScope(request("library_id=3"), v, "profile_library"); !ok || id != "3" {
		t.Fatal("authorized library rejected")
	}
	if _, ok := i.settingScope(request("library_id=4"), v, "profile_library"); ok {
		t.Fatal("unauthorized library accepted")
	}
	if id, ok := i.settingScope(request("series_id="+ids.RatingKeyGUID(series)), v, "profile_series"); !ok || id != ids.RatingKeyGUID(series) {
		t.Fatal("authorized series rejected")
	}
	if _, ok := i.settingScope(request("series_id="+ids.RatingKeyGUID(other)), v, "profile_series"); ok {
		t.Fatal("unauthorized series accepted")
	}
	if _, ok := i.settingScope(request("profile_id=8&library_id=3"), v, "profile_library"); ok {
		t.Fatal("cross-profile scope accepted")
	}
}

type versionedTestPreferences struct {
	frontendtest.Preferences
	revisions map[string]int64
}

func (p *versionedTestPreferences) GetVersioned(ctx context.Context, v *frontend.Viewer, c, k string) (json.RawMessage, int64, time.Time, error) {
	raw, err := p.Get(ctx, v, c, k)
	return raw, p.revisions[c+"/"+k], time.Unix(100, 0), err
}
func (p *versionedTestPreferences) PutVersioned(ctx context.Context, v *frontend.Viewer, c, k string, raw json.RawMessage) (int64, time.Time, error) {
	if err := p.Put(ctx, v, c, k, raw); err != nil {
		return 0, time.Time{}, err
	}
	p.revisions[c+"/"+k]++
	return p.revisions[c+"/"+k], time.Unix(100, 0), nil
}

func (s *settingsTracks) Get(context.Context, *frontend.Viewer) (frontend.TrackPreferences, error) {
	return s.tracks, nil
}
func (s *settingsTracks) SetSubtitle(_ context.Context, _ *frontend.Viewer, langs []string, mode string) error {
	s.tracks.SubtitleLanguages = langs
	s.tracks.SubtitleMode = mode
	return nil
}
func (s *settingsTracks) GetScoped(context.Context, *frontend.Viewer, frontend.TrackScope) (frontend.TrackPreferences, int, int, string, map[string]string, error) {
	return s.tracks, s.bitrate, s.height, s.quality, nil, nil
}
func (s *settingsTracks) SetAudio(_ context.Context, _ *frontend.Viewer, langs []string) error {
	s.tracks.AudioLanguages = langs
	return nil
}
func (s *settingsTracks) SetSubtitleLanguage(_ context.Context, _ *frontend.Viewer, langs []string) error {
	s.tracks.SubtitleLanguages = langs
	return nil
}
func (s *settingsTracks) SetSubtitleMode(_ context.Context, _ *frontend.Viewer, mode string) error {
	s.tracks.SubtitleMode = mode
	return nil
}
func (s *settingsTracks) SetAppearance(_ context.Context, _ *frontend.Viewer, raw json.RawMessage) error {
	s.tracks.SubtitleAppearance = append(json.RawMessage(nil), raw...)
	return nil
}
func (s *settingsTracks) SetBitrate(_ context.Context, _ *frontend.Viewer, n int) error {
	s.bitrate = n
	return nil
}
func (s *settingsTracks) SetHeight(_ context.Context, _ *frontend.Viewer, n int, quality string) error {
	s.height = n
	s.quality = quality
	return nil
}
func (s *settingsTracks) SetScoped(_ context.Context, _ *frontend.Viewer, _, _ string, p frontend.TrackPreferencePatch) error {
	if p.AudioLanguages != nil {
		s.tracks.AudioLanguages = *p.AudioLanguages
	}
	if p.SubtitleLanguages != nil {
		s.tracks.SubtitleLanguages = *p.SubtitleLanguages
	}
	if p.SubtitleMode != nil {
		s.tracks.SubtitleMode = *p.SubtitleMode
	}
	if p.SubtitleAppearance != nil {
		s.tracks.SubtitleAppearance = append([]byte(nil), *p.SubtitleAppearance...)
	}
	if p.MaxBitrateKbps != nil {
		s.bitrate = *p.MaxBitrateKbps
	}
	if p.MaxHeight != nil {
		s.height = *p.MaxHeight
		if p.PreferredQuality != nil {
			s.quality = *p.PreferredQuality
		}
	}
	return nil
}
func (s *settingsTracks) DeleteScoped(context.Context, *frontend.Viewer, string, string, string) error {
	return nil
}

func TestRevisionEightSettingsRoundTrip(t *testing.T) {
	v := &frontend.Viewer{ID: 7}
	records := map[string]json.RawMessage{}
	base := frontendtest.Preferences{
		GetFunc: func(_ context.Context, _ *frontend.Viewer, c, k string) (json.RawMessage, error) {
			b, ok := records[c+"/"+k]
			if !ok {
				return nil, frontend.ErrPreferencesNotFound
			}
			return b, nil
		},
		PutFunc: func(_ context.Context, _ *frontend.Viewer, c, k string, b json.RawMessage) error {
			records[c+"/"+k] = append([]byte(nil), b...)
			return nil
		},
		DeleteFunc: func(_ context.Context, _ *frontend.Viewer, c, k string) error { delete(records, c+"/"+k); return nil },
	}
	prefs := &versionedTestPreferences{Preferences: base, revisions: map[string]int64{}}
	tracks := &settingsTracks{tracks: frontend.TrackPreferences{SubtitleMode: "off"}}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Preferences: prefs, TrackPreferences: tracks}
	i := &instance{host: host}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	routeReq := httptest.NewRequest("GET", "/api/v2/settings/contract/capabilities", nil)
	routeReq.Header.Set("Authorization", "Bearer grant")
	routeResponse := httptest.NewRecorder()
	inst.Handler().ServeHTTP(routeResponse, routeReq)
	if routeResponse.Code != http.StatusOK {
		t.Fatalf("capability route: %d %s", routeResponse.Code, routeResponse.Body.String())
	}
	overlayReq := httptest.NewRequest("GET", "/api/v2/settings/overlay-config", nil)
	overlayReq.Header.Set("Authorization", "Bearer grant")
	overlayResponse := httptest.NewRecorder()
	inst.Handler().ServeHTTP(overlayResponse, overlayReq)
	if overlayResponse.Code != http.StatusOK || !strings.Contains(overlayResponse.Body.String(), `"quick_actions_default":"both"`) {
		t.Fatalf("overlay defaults: %d %s", overlayResponse.Code, overlayResponse.Body.String())
	}
	request := func(method, path, body string, handler http.HandlerFunc, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		r.Header.Set("X-Silo-Client-Family", "tv")
		r.SetPathValue("key", key)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	w := request("GET", "/api/v2/settings/contract/capabilities", "", i.settingsCapabilities, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"manifest_revision":24`) || !strings.Contains(w.Body.String(), `"supports_batched_effective":true`) {
		t.Fatalf("capabilities: %d %s", w.Code, w.Body.String())
	}
	var document *openapi.Document
	if doc := spectest.Document(t, "silo"); doc != nil {
		if e := doc.Validate("GET", "/api/v2/settings/contract/capabilities", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Fatal(e)
		}
		document = doc
	}
	for key, d := range settingDefinitions {
		scope := ""
		for _, s := range d.scopes {
			if s == "profile" || s == "profile_device" || s == "profile_client" {
				scope = s
				break
			}
		}
		if scope == "" {
			continue
		} // client_local has no server write
		value := d.defaultJSON
		switch d.kind {
		case "boolean":
			if value == "true" {
				value = "false"
			} else {
				value = "true"
			}
		case "language_tag":
			value = `"fr"`
		case "integer":
			if key == "playback.max_bitrate_kbps" {
				value = "1000"
			} else if d.hasMinimum {
				value = strconv.FormatFloat(d.minimum, 'f', -1, 64)
			}
		case "enum":
			if d.numeric {
				for _, choice := range d.choices {
					if choice != value {
						value = choice
						break
					}
				}
			} else if key == "playback.preferred_quality" {
				value = `"original"`
			} else {
				for _, choice := range d.choices {
					candidate, _ := json.Marshal(choice)
					if string(candidate) != value {
						value = string(candidate)
						break
					}
				}
			}
		}
		body := `{"value":` + value + `}`
		w = request("PUT", "/api/v2/settings/values/"+key+"?scope="+scope, body, i.settingWrite, key)
		if w.Code != 200 {
			t.Errorf("%s write: %d %s", key, w.Code, w.Body.String())
			continue
		}
		if document != nil && key == "ui.theme" {
			if e := document.Validate("PUT", "/api/v2/settings/values/ui.theme", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Errorf("setting receipt spec: %v", e)
			}
		}
		w = request("GET", "/api/v2/settings/values/effective?keys="+key, "", i.settingValues, "")
		if w.Code != 200 {
			t.Errorf("%s read: %d %s", key, w.Code, w.Body.String())
			continue
		}
		if document != nil && key == "ui.theme" {
			if e := document.Validate("GET", "/api/v2/settings/values/effective", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Errorf("effective settings spec: %v", e)
			}
		}
		var result struct {
			Items []struct {
				Value json.RawMessage `json:"value"`
			} `json:"items"`
		}
		if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 1 {
			t.Errorf("%s response: %s", key, w.Body.String())
			continue
		}
		var got, want any
		_ = json.Unmarshal(result.Items[0].Value, &got)
		_ = json.Unmarshal([]byte(value), &want)
		g, _ := json.Marshal(got)
		a, _ := json.Marshal(want)
		if string(g) != string(a) {
			t.Errorf("%s got %s want %s", key, g, a)
		}
	}
	for _, change := range []struct{ scope, value string }{{"profile", "midnight-cinema"}, {"profile_device", "cobalt-studio"}} {
		w = request("PUT", "/api/v2/settings/values/ui.theme?scope="+change.scope, `{"value":"`+change.value+`"}`, i.settingWrite, "ui.theme")
		if w.Code != http.StatusOK {
			t.Fatalf("theme %s: %d %s", change.scope, w.Code, w.Body.String())
		}
	}
	w = request("GET", "/api/v2/settings/values/effective?keys=ui.theme", "", i.settingValues, "")
	if !strings.Contains(w.Body.String(), `"value":"cobalt-studio"`) || !strings.Contains(w.Body.String(), `"source":"profile_device"`) {
		t.Fatalf("device precedence: %s", w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/v2/settings/values/effective?keys=ui.theme", nil)
	r.Header.Set("Authorization", "Bearer grant")
	r.Header.Set("X-Silo-Device-Id", "other")
	r.Header.Set("X-Silo-Client-Family", "tv")
	w = httptest.NewRecorder()
	i.settingValues(w, r)
	if !strings.Contains(w.Body.String(), `"value":"midnight-cinema"`) || !strings.Contains(w.Body.String(), `"source":"profile"`) {
		t.Fatalf("profile fallback: %s", w.Body.String())
	}
	w = request("GET", "/api/v2/settings/values/effective?keys=ui.theme&keys=playback.audio_language", "", i.settingValues, "")
	var batch struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &batch) != nil || len(batch.Items) != 2 || batch.Items[0].Key != "ui.theme" || batch.Items[1].Key != "playback.audio_language" {
		t.Fatalf("batch: %d %s", w.Code, w.Body.String())
	}
}
