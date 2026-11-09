package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// An install upgraded from a release before the Radarr/Sonarr-standard
// ranking starts on exactly the profiles a fresh install has, repoints any
// client rules that named removed custom or imported profiles while keeping
// their matchers and limits, and keeps a standard profile the owner made.
func TestPreStandardProfilesResetToDefaults(t *testing.T) {
	open := func(doc string) Config {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return st.Get()
	}
	old := open(`{"resolver":{"customFormatScores":{"x":5}},"quality":{"default":"Mine","rules":[{"product":"Apple TV","profile":"4K"},{"product":"Roku","profile":"Mine","maxHeight":1080,"convertSurroundAudio":true},{"platform":"iOS","mediaType":"movie","profile":"TRaSH UHD"},{"userId":9,"mediaType":"tv","profile":"TRaSH HD"}],"profiles":[{"name":"Mine","mediaType":"movie","maxHeight":1080,"minHeight":480,"preferredSources":["webdl"],"preferredCodecs":["h264"],"preferredAudio":["aac"],"ranking":["resolution"]}]}}`)
	if want := DefaultQuality().Profiles; !reflect.DeepEqual(old.Quality.Profiles, want) {
		t.Fatalf("profiles %+v, want the defaults", old.Quality.Profiles)
	}
	if old.Quality.Default == "Mine" {
		t.Fatal("default still names the removed profile")
	}
	wantRules := []QualityRule{
		{Product: "Apple TV", Profile: "4K"},
		{Product: "Roku", Profile: "4K", MaxHeight: 1080, ConvertSurroundAudio: true},
		{Platform: "iOS", MediaType: "movie", Profile: "4K"},
		{UserID: 9, MediaType: "tv", Profile: "4K"},
	}
	if !slices.Equal(old.Quality.Rules, wantRules) {
		t.Fatalf("rules %+v, want %+v", old.Quality.Rules, wantRules)
	}

	shared, _, err := mergeShared(Defaults(t.TempDir()), []byte(`{"quality":{"rules":[{"product":"Shield","profile":"Mine","maxHeight":2160}],"profiles":[{"name":"Mine","mediaType":"tv","preferredSources":["webdl"]}]}}`))
	if err != nil || !reflect.DeepEqual(shared.Quality.Profiles, DefaultQuality().Profiles) {
		t.Fatalf("shared profiles %+v (%v), want the defaults", shared.Quality.Profiles, err)
	}
	if len(shared.Quality.Rules) != 1 || shared.Quality.Rules[0].Profile != "4K" || shared.Quality.Rules[0].MaxHeight != 2160 {
		t.Fatalf("shared rules %+v, want repointed 4K with maxHeight 2160", shared.Quality.Rules)
	}

	b, _ := json.Marshal(DefaultQuality().Profiles[1])
	var mine map[string]any
	_ = json.Unmarshal(b, &mine)
	mine["name"] = "Mine"
	doc, _ := json.Marshal(map[string]any{"quality": map[string]any{"default": "Mine", "profiles": []any{mine}}})
	kept := open(string(doc))
	if len(kept.Quality.Profiles) != 1 || kept.Quality.Profiles[0].Name != "Mine" || kept.Quality.Default != "Mine" {
		t.Fatalf("standard profile not kept: %+v", kept.Quality)
	}
}
