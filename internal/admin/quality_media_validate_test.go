package admin

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func mediaConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults(t.TempDir())
	cfg.Quality = config.Quality{
		Profiles: []core.QualityProfile{
			{Name: "Movies", MediaType: core.MediaMovie},
			{Name: "Shows", MediaType: core.MediaTV},
			// One name may serve both media as two profiles.
			{Name: "Any", MediaType: core.MediaMovie},
			{Name: "Any", MediaType: core.MediaTV},
		},
	}
	return cfg
}

func hasField(errs []FieldError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

func TestValidateConfigMediaScopes(t *testing.T) {
	cfg := mediaConfig(t)
	cfg.Quality.DefaultMovie = "Movies"
	cfg.Quality.DefaultTV = "Shows"
	cfg.Quality.RemoteMovie = "Any"
	cfg.Quality.RemoteTV = "Any"
	cfg.Quality.Default = "Movies"
	cfg.Quality.Remote = "Shows"
	cfg.Quality.Rules = []config.QualityRule{
		{MediaType: core.MediaMovie, Profile: "Movies"},
		{MediaType: core.MediaTV, Profile: "Shows"},
		{Profile: "Any"},
	}
	cfg.Quality.Sync = config.QualitySync{Enabled: true, Interval: config.Duration(24 * time.Hour)}
	if errs := ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("valid media config rejected: %v", errs)
	}

	// Legacy defaults may name a media-scoped profile; they stay valid.
	cfg.Quality.Default = "Shows"
	cfg.Quality.Remote = "Movies"
	if errs := ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("legacy defaults rejected: %v", errs)
	}

	tests := []struct {
		name   string
		mutate func(*config.Config)
		field  string
	}{
		{
			name:   "profile media type",
			mutate: func(c *config.Config) { c.Quality.Profiles[0].MediaType = "film" },
			field:  "quality.profiles[0].mediaType",
		},
		{
			name:   "profile without a media type",
			mutate: func(c *config.Config) { c.Quality.Profiles[0].MediaType = "" },
			field:  "quality.profiles[0].mediaType",
		},
		{
			name:   "duplicate name within one media",
			mutate: func(c *config.Config) { c.Quality.Profiles[3].Name = "Shows" },
			field:  "quality.profiles[3].name",
		},
		{
			name: "rule media type",
			mutate: func(c *config.Config) {
				c.Quality.Rules = []config.QualityRule{{MediaType: "shows", Profile: "Movies"}}
			},
			field: "quality.rules[0].mediaType",
		},
		{
			name:   "movie default names tv profile",
			mutate: func(c *config.Config) { c.Quality.DefaultMovie = "Shows" },
			field:  "quality.defaultMovie",
		},
		{
			name:   "tv default names movie profile",
			mutate: func(c *config.Config) { c.Quality.DefaultTV = "Movies" },
			field:  "quality.defaultTv",
		},
		{
			name:   "movie remote names tv profile",
			mutate: func(c *config.Config) { c.Quality.RemoteMovie = "Shows" },
			field:  "quality.remoteMovie",
		},
		{
			name:   "tv remote names movie profile",
			mutate: func(c *config.Config) { c.Quality.RemoteTV = "Movies" },
			field:  "quality.remoteTv",
		},
		{
			name:   "movie default names missing profile",
			mutate: func(c *config.Config) { c.Quality.DefaultMovie = "Missing" },
			field:  "quality.defaultMovie",
		},
		{
			name: "media default requires a configured profile",
			mutate: func(c *config.Config) {
				c.Quality.Profiles = nil
				c.Quality.DefaultMovie = "Missing"
			},
			field: "quality.defaultMovie",
		},
		{
			name:   "sync interval zero while enabled",
			mutate: func(c *config.Config) { c.Quality.Sync = config.QualitySync{Enabled: true} },
			field:  "quality.sync.interval",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mediaConfig(t)
			tc.mutate(&cfg)
			errs := ValidateConfig(cfg)
			if !hasField(errs, tc.field) {
				t.Fatalf("expected %s error, got %v", tc.field, errs)
			}
		})
	}
}

func TestValidateConfigSyncIntervalDisabled(t *testing.T) {
	cfg := mediaConfig(t)
	cfg.Quality.Sync = config.QualitySync{Enabled: false}
	if errs := ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("disabled sync with zero interval rejected: %v", errs)
	}
}

// Reproduces the v0.3.6 upgrade bug: both an install upgrading from <=0.3.5
// with custom/TRaSH rules and an install that already booted v0.3.6 with
// reset profiles + dangling rules[1..3] open into a valid Config that passes
// ValidateConfig on subsequent profile edits.
func TestValidateConfigAfterUpgradingDanglingQualityRules(t *testing.T) {
	for name, raw := range map[string]string{
		"from v0.3.5 custom profiles": `{"quality":{"default":"TRaSH UHD","rules":[` +
			`{"product":"Apple TV","profile":"4K"},` +
			`{"product":"Roku","profile":"TRaSH UHD","maxHeight":1080},` +
			`{"platform":"iOS","mediaType":"movie","profile":"TRaSH Remux"},` +
			`{"userId":7,"mediaType":"tv","profile":"TRaSH WEB"}` +
			`],"profiles":[{"name":"TRaSH UHD","mediaType":"movie","maxHeight":2160,"preferredSources":["remux"]}]}}`,
		"from wedged v0.3.6 install": `{"quality":{"default":"","rules":[` +
			`{"product":"Apple TV","profile":"4K"},` +
			`{"product":"Roku","profile":"TRaSH UHD","maxHeight":1080},` +
			`{"platform":"iOS","mediaType":"movie","profile":"TRaSH Remux"},` +
			`{"userId":7,"mediaType":"tv","profile":"TRaSH WEB"}` +
			`],"profiles":[` +
			`{"name":"4K","mediaType":"movie","maxHeight":2160,"minHeight":720},` +
			`{"name":"4K","mediaType":"tv","maxHeight":2160,"minHeight":720},` +
			`{"name":"1080p","mediaType":"movie","maxHeight":1080,"minHeight":480},` +
			`{"name":"1080p","mediaType":"tv","maxHeight":1080,"minHeight":480}` +
			`]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := config.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			cfg := store.Get()
			cfg.Quality.Profiles[0].MinHeight = 1080
			if errs := ValidateConfig(cfg); len(errs) != 0 {
				t.Fatalf("saving edited profile failed validation: %v", errs)
			}
		})
	}
}
