package silo

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type testTracks struct {
	frontendtest.TrackPreferences
	value frontend.TrackPreferences
}

func (t *testTracks) Get(context.Context, *frontend.Viewer) (frontend.TrackPreferences, error) {
	return t.value, nil
}
func (t *testTracks) SetSubtitle(_ context.Context, _ *frontend.Viewer, languages []string, mode string) error {
	t.value.SubtitleLanguages = languages
	t.value.SubtitleMode = mode
	return nil
}
func TestProfilePreferencesRoundTrip(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "alice", CreatedAt: time.Unix(10000, 0), UpdatedAt: time.Unix(11000, 0)}
	var raw json.RawMessage
	prefs := frontendtest.Preferences{GetFunc: func(context.Context, *frontend.Viewer, string, string) (json.RawMessage, error) {
		if raw == nil {
			return nil, frontend.ErrPreferencesNotFound
		}
		return raw, nil
	}, PutFunc: func(_ context.Context, _ *frontend.Viewer, _, _ string, b json.RawMessage) error {
		raw = append([]byte(nil), b...)
		return nil
	}}
	tracks := &testTracks{value: frontend.TrackPreferences{SubtitleMode: "off"}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Preferences: prefs, TrackPreferences: tracks, Experience: frontendtest.Experience{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/api/v2/profiles", "")
	if w.Code != 200 {
		t.Fatalf("profiles %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"show_forced_subtitles":true`) {
		t.Fatalf("forced subtitle default: %s", w.Body.String())
	}
	if doc := spectest.Document(t, "silo"); doc != nil {
		if e := doc.Validate("GET", "/api/v2/profiles", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Errorf("profile spec: %v", e)
		}
	}
	w = call("PATCH", "/api/v2/profiles/7", `{"subtitle_language":"fr","subtitle_mode":"always","auto_skip_intro":true}`)
	if w.Code != 200 || tracks.value.SubtitleMode != "always" || len(tracks.value.SubtitleLanguages) != 1 || tracks.value.SubtitleLanguages[0] != "fr" || !strings.Contains(w.Body.String(), `"auto_skip_intro":true`) {
		t.Fatalf("patch %d %s %+v", w.Code, w.Body.String(), tracks.value)
	}
	w = call("GET", "/api/v2/profiles", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"subtitle_language":"fr"`) {
		t.Fatalf("readback %d %s", w.Code, w.Body.String())
	}
}

func TestProfileQualityUsesSharedPlaybackPreference(t *testing.T) {
	v := &frontend.Viewer{ID: 7, Username: "alice", CreatedAt: time.Unix(10000, 0), UpdatedAt: time.Unix(11000, 0)}
	var raw json.RawMessage
	prefs := frontendtest.Preferences{
		GetFunc: func(context.Context, *frontend.Viewer, string, string) (json.RawMessage, error) {
			if raw == nil {
				return nil, frontend.ErrPreferencesNotFound
			}
			return raw, nil
		},
		PutFunc: func(_ context.Context, _ *frontend.Viewer, _, _ string, b json.RawMessage) error {
			raw = append([]byte(nil), b...)
			return nil
		},
	}
	tracks := &settingsTracks{tracks: frontend.TrackPreferences{SubtitleMode: "off"}}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Preferences: prefs, TrackPreferences: tracks, Experience: frontendtest.Experience{}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("PATCH", "/api/v2/profiles/7", `{"max_playback_quality":"1080p"}`)
	if w.Code != 200 || tracks.height != 1080 || !strings.Contains(w.Body.String(), `"max_playback_quality":"1080p"`) {
		t.Fatalf("write: %d %s height=%d", w.Code, w.Body.String(), tracks.height)
	}
	tracks.height = 2160 // Another frontend changes the shared rule.
	w = request("GET", "/api/v2/profiles", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"max_playback_quality":"2160p"`) {
		t.Fatalf("shared read: %d %s", w.Code, w.Body.String())
	}
	w = request("PATCH", "/api/v2/profiles/7", `{"max_playback_quality":null}`)
	if w.Code != 200 || tracks.height != 0 {
		t.Fatalf("clear: %d %s height=%d", w.Code, w.Body.String(), tracks.height)
	}
}

func TestProfileAndSettingsSharePreferences(t *testing.T) {
	v := &frontend.Viewer{ID: 7, Username: "alice", CreatedAt: time.Unix(10000, 0), UpdatedAt: time.Unix(11000, 0)}
	records := map[string]json.RawMessage{}
	base := frontendtest.Preferences{
		GetFunc: func(_ context.Context, _ *frontend.Viewer, client, key string) (json.RawMessage, error) {
			if raw, ok := records[client+"/"+key]; ok {
				return raw, nil
			}
			return nil, frontend.ErrPreferencesNotFound
		},
		PutFunc: func(_ context.Context, _ *frontend.Viewer, client, key string, raw json.RawMessage) error {
			records[client+"/"+key] = append([]byte(nil), raw...)
			return nil
		},
		DeleteFunc: func(_ context.Context, _ *frontend.Viewer, client, key string) error {
			delete(records, client+"/"+key)
			return nil
		},
	}
	prefs := &versionedTestPreferences{Preferences: base, revisions: map[string]int64{}}
	tracks := &settingsTracks{tracks: frontend.TrackPreferences{SubtitleMode: "off"}}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Preferences: prefs, TrackPreferences: tracks, Experience: frontendtest.Experience{}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("PATCH", "/api/v2/profiles/7", `{"auto_skip_intro":true,"language":"fr"}`)
	if w.Code != 200 || len(tracks.tracks.AudioLanguages) != 1 || tracks.tracks.AudioLanguages[0] != "fr" {
		t.Fatalf("profile write: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/api/v2/settings/values/effective?keys=playback.auto_skip_intro&keys=playback.audio_language", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":true`) || !strings.Contains(w.Body.String(), `"value":"fr"`) {
		t.Fatalf("settings read: %d %s", w.Code, w.Body.String())
	}
	w = request("PUT", "/api/v2/settings/values/playback.auto_skip_intro?scope=profile", `{"value":false}`)
	if w.Code != 200 {
		t.Fatalf("setting write: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/api/v2/profiles", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"auto_skip_intro":false`) || !strings.Contains(w.Body.String(), `"language":"fr"`) {
		t.Fatalf("profile read: %d %s", w.Code, w.Body.String())
	}
	w = request("DELETE", "/api/v2/settings/values/playback.auto_skip_intro?scope=profile", "")
	if w.Code != 204 {
		t.Fatalf("setting delete: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/api/v2/profiles", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"auto_skip_intro":false`) {
		t.Fatalf("deleted setting restored stale value: %d %s", w.Code, w.Body.String())
	}
}
