package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type subtitlePrefs struct {
	frontendtest.TrackPreferences
	value frontend.TrackPreferences
}

func (s *subtitlePrefs) Get(context.Context, *frontend.Viewer) (frontend.TrackPreferences, error) {
	return s.value, nil
}
func (s *subtitlePrefs) SetSubtitle(_ context.Context, _ *frontend.Viewer, languages []string, mode string) error {
	s.value.SubtitleLanguages = append([]string(nil), languages...)
	s.value.SubtitleMode = mode
	return nil
}

func TestJellyfinAndEmbyShareSubtitlePreferences(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "ada", SubtitleLanguages: []string{"eng"}}
	prefs := &subtitlePrefs{value: frontend.TrackPreferences{SubtitleMode: "off", SubtitleLanguages: []string{"fr", "de"}}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	mount := func(protocol string) frontend.Instance {
		t.Helper()
		var inst frontend.Instance
		var err error
		host := frontend.Host{Identity: ident, TrackPreferences: prefs, Sessions: frontendtest.Sessions{}}
		if protocol == "emby" {
			inst, err = (Emby{MachineID: "test"}).Mount(context.Background(), host)
		} else {
			inst, err = (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
		}
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	call := func(inst frontend.Instance, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	for _, protocol := range []string{"jellyfin", "emby"} {
		prefs.value = frontend.TrackPreferences{SubtitleMode: "foreign-only", SubtitleLanguages: []string{"fr", "de"}}
		inst := mount(protocol)
		got := call(inst, http.MethodGet, "/Users/Me", "")
		var user struct {
			Configuration map[string]any
		}
		if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &user) != nil || user.Configuration["SubtitleMode"] != "Smart" || user.Configuration["SubtitleLanguagePreference"] != "fr" {
			t.Fatalf("%s read %d %s", protocol, got.Code, got.Body.String())
		}
		id, err := ids.GUID(ids.User, viewer.ID)
		if err != nil {
			t.Fatal(err)
		}
		saved := call(inst, http.MethodPost, "/Users/"+id+"/Configuration", `{"SubtitleMode":"Always","SubtitleLanguagePreference":"fr"}`)
		if saved.Code != 204 || prefs.value.SubtitleMode != "always" || len(prefs.value.SubtitleLanguages) != 2 || prefs.value.SubtitleLanguages[1] != "de" {
			t.Fatalf("%s echo %d %+v", protocol, saved.Code, prefs.value)
		}
		saved = call(inst, http.MethodPost, "/Users/"+id+"/Configuration", `{"SubtitleMode":"None","SubtitleLanguagePreference":"en, es"}`)
		if saved.Code != 204 || prefs.value.SubtitleMode != "off" || len(prefs.value.SubtitleLanguages) != 2 || prefs.value.SubtitleLanguages[0] != "en" || prefs.value.SubtitleLanguages[1] != "es" {
			t.Fatalf("%s write %d %+v", protocol, saved.Code, prefs.value)
		}
		again := call(inst, http.MethodGet, "/Users/Me", "")
		if json.Unmarshal(again.Body.Bytes(), &user) != nil || user.Configuration["SubtitleMode"] != "None" || user.Configuration["SubtitleLanguagePreference"] != "en" {
			t.Fatalf("%s reread %s", protocol, again.Body.String())
		}
	}
	forced, ok := jellyfinSubtitleMode("forced-only")
	back, known := etherSubtitleMode("OnlyForced")
	if !ok || !known || forced != "OnlyForced" || back != "forced-only" {
		t.Fatalf("forced mapping %s %s", forced, back)
	}
}

func TestJellyfinRejectsUnknownSubtitleMode(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "ada"}
	prefs := &subtitlePrefs{value: frontend.TrackPreferences{SubtitleMode: "off"}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, TrackPreferences: prefs, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Users/"+id+"/Configuration", strings.NewReader(`{"SubtitleMode":"Sometimes"}`))
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 400 || prefs.value.SubtitleMode != "off" {
		t.Fatalf("%d %s %+v", w.Code, w.Body.String(), prefs.value)
	}
}
