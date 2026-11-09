package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// savedByOlderRelease is q as a release that predates per-media profiles
// (v0.3.4) stores it: it drops every field it does not know.
func savedByOlderRelease(t *testing.T, q Quality) map[string]any {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"defaultMovie", "defaultTv", "remoteMovie", "remoteTv"} {
		delete(doc, k)
	}
	for _, p := range doc["profiles"].([]any) {
		delete(p.(map[string]any), "mediaType")
	}
	return doc
}

func asDoc(t *testing.T, q Quality) map[string]any {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// assertSettled checks the invariant every load must leave: each profile
// serves exactly one media, a name is unique per media, and every stored
// default and client rule names a profile serving its media.
func assertSettled(t *testing.T, label string, q Quality) {
	t.Helper()
	type key struct{ media, name string }
	seen := map[key]bool{}
	for i, p := range q.Profiles {
		if !core.IsProfileMedia(p.MediaType) {
			t.Fatalf("%s: profile %d %q has media %q", label, i, p.Name, p.MediaType)
		}
		if seen[key{p.MediaType, p.Name}] {
			t.Fatalf("%s: two %s profiles named %q: %+v", label, p.MediaType, p.Name, q.Profiles)
		}
		seen[key{p.MediaType, p.Name}] = true
	}
	for _, m := range core.ProfileMediaTypes {
		home, away := q.DefaultMovie, q.RemoteMovie
		if m == core.MediaTV {
			home, away = q.DefaultTV, q.RemoteTV
		}
		for field, name := range map[string]string{"home": home, "away": away} {
			if name != "" && !seen[key{m, name}] {
				t.Fatalf("%s: %s %s default %q serves no %s profile", label, m, field, name, m)
			}
		}
	}
	if len(q.Profiles) > 0 {
		for field, name := range map[string]string{"default": q.Default, "remote": q.Remote} {
			if name != "" && !seen[key{core.MediaMovie, name}] && !seen[key{core.MediaTV, name}] {
				t.Fatalf("%s: %s %q names no profile", label, field, name)
			}
		}
		for i, r := range q.Rules {
			if r.Profile == "" || (r.MediaType != "" && !core.IsProfileMedia(r.MediaType)) {
				continue
			}
			if _, ok := core.FindProfile(q.Profiles, r.Profile, r.MediaType); !ok {
				t.Fatalf("%s: rule %d (%q) profile %q serves no %q profile", label, i, r.Product, r.Profile, r.MediaType)
			}
		}
	}
}

func profileList(q Quality) []string {
	var out []string
	for _, p := range q.Profiles {
		out = append(out, p.MediaType+"/"+p.Name)
	}
	return out
}

// Upgrading from v0.3.4, in any of the shapes an install can be in, and
// then loading the settings any number of times (config.json on a single
// server, the shared settings on a cluster, a peer that predates per-media
// profiles saving over them) always leaves one movie and one TV profile
// per original, no duplicates, and defaults and rules that resolve.
func TestQualityProfilesStaySettledAcrossLoads(t *testing.T) {
	builtins := func() []core.QualityProfile {
		var out []core.QualityProfile
		for _, p := range DefaultQuality().Profiles {
			if p.MediaType == core.MediaMovie {
				out = append(out, p)
			}
		}
		return out
	}
	customized := builtins()
	customized[0].MinHeight = 1080
	customized[0].PreferredLanguages = []string{"en"}
	customized = append(customized, core.QualityProfile{Name: "Kids", MediaType: core.MediaMovie, MaxHeight: 1080, MinHeight: 480})
	movieOnly := builtins()[:2]
	tvCopy := slices.Clone(movieOnly)
	for i := range tvCopy {
		tvCopy[i].MediaType = core.MediaTV
	}
	customMovie := slices.Clone(movieOnly)
	customMovie[0].MinHeight = 1080
	interleaved := []core.QualityProfile{customMovie[0], tvCopy[0], customMovie[1], tvCopy[1]}
	fresh := DefaultQuality()
	fresh.DefaultMovie, fresh.DefaultTV = "4K", "4K"
	doubled := slices.Concat(interleaved, interleaved) // the split repeated over a stripped list
	dangling := DefaultQuality()
	dangling.Default, dangling.DefaultMovie, dangling.RemoteTV = "Gone", "Gone", "Gone"
	dangling.Rules = []QualityRule{
		{Product: "Apple TV", Profile: "4K"},
		{Product: "Roku", Profile: "TRaSH UHD", MaxHeight: 1080, ConvertSurroundAudio: true},
		{Platform: "iOS", MediaType: core.MediaMovie, Profile: "TRaSH Remux"},
		{UserID: 7, MediaType: core.MediaTV, Profile: "TRaSH WEB"},
	}
	divergentRules := DefaultQuality()
	divergentRules.DefaultMovie = "4K"
	divergentRules.DefaultTV = "1080p"
	divergentRules.Rules = []QualityRule{
		{Product: "Fire TV", Profile: "Removed Imported", MaxHeight: 1080},
	}

	all := []string{"movie/4K", "movie/1080p", "movie/720p", "tv/4K", "tv/1080p", "tv/720p"}
	cases := []struct {
		name    string
		quality map[string]any
		want    []string // per-media profile names, in order, sorted by media
		check   func(t *testing.T, q Quality)
	}{
		{name: "v0.3.4 built-ins", quality: savedByOlderRelease(t, Quality{Profiles: builtins(), Default: "4K"}), want: all},
		{
			name:    "v0.3.4 customized built-ins and owner profiles",
			quality: savedByOlderRelease(t, Quality{Profiles: customized, Default: "1080p", Remote: "720p", Rules: []QualityRule{{Product: "Roku", Profile: "Kids"}}}),
			want:    []string{"movie/4K", "movie/1080p", "movie/720p", "movie/Kids", "tv/4K", "tv/1080p", "tv/720p", "tv/Kids"},
			check: func(t *testing.T, q Quality) {
				for _, m := range core.ProfileMediaTypes {
					p, _ := core.FindProfile(q.Profiles, "4K", m)
					if p.MinHeight != 1080 || len(p.PreferredLanguages) != 1 {
						t.Fatalf("%s 4K lost its customization: %+v", m, p)
					}
				}
				if q.Default != "1080p" || q.Remote != "720p" || len(q.Rules) != 1 {
					t.Fatalf("defaults or rules changed: %+v", q)
				}
			},
		},
		{name: "v0.3.5 fresh install saved by v0.3.4", quality: savedByOlderRelease(t, fresh), want: all},
		{
			name:    "v0.3.5 split with a customized movie copy saved by v0.3.4",
			quality: savedByOlderRelease(t, Quality{Profiles: interleaved, Default: "4K"}),
			want:    []string{"movie/4K", "movie/1080p", "tv/4K", "tv/1080p"},
			check: func(t *testing.T, q Quality) {
				movie, _ := core.FindProfile(q.Profiles, "4K", core.MediaMovie)
				tv, _ := core.FindProfile(q.Profiles, "4K", core.MediaTV)
				if movie.MinHeight != 1080 || tv.MinHeight != 720 {
					t.Fatalf("copies swapped media: movie min %d, tv min %d", movie.MinHeight, tv.MinHeight)
				}
			},
		},
		{name: "v0.3.5 duplicated split", quality: asDoc(t, Quality{Profiles: doubled, Default: "4K"}), want: []string{"movie/4K", "movie/1080p", "tv/4K", "tv/1080p"}},
		{
			name:    "duplicates with different settings",
			quality: asDoc(t, Quality{Profiles: []core.QualityProfile{movieOnly[0], customMovie[0], tvCopy[0]}, Default: "4K"}),
			want:    []string{"movie/4K", "movie/4K (2)", "tv/4K"},
			check: func(t *testing.T, q Quality) {
				if p, _ := core.FindProfile(q.Profiles, "4K (2)", core.MediaMovie); p.MinHeight != 1080 {
					t.Fatalf("customized duplicate lost: %+v", q.Profiles)
				}
			},
		},
		{
			name:    "defaults and client rules naming missing profiles (including v0.3.6 wedged installs)",
			quality: asDoc(t, dangling),
			want:    all,
			check: func(t *testing.T, q Quality) {
				if q.Default != "" || q.DefaultMovie != "4K" || q.RemoteTV != "" {
					t.Fatalf("defaults not repointed: %+v", q)
				}
				wantRules := []QualityRule{
					{Product: "Apple TV", Profile: "4K"},
					{Product: "Roku", Profile: "4K", MaxHeight: 1080, ConvertSurroundAudio: true},
					{Platform: "iOS", MediaType: core.MediaMovie, Profile: "4K"},
					{UserID: 7, MediaType: core.MediaTV, Profile: "4K"},
				}
				if !slices.Equal(q.Rules, wantRules) {
					t.Fatalf("rules not repointed: got %+v, want %+v", q.Rules, wantRules)
				}
			},
		},
		{
			name:    "media-neutral rule naming missing profile splits when movie and show defaults differ",
			quality: asDoc(t, divergentRules),
			want:    all,
			check: func(t *testing.T, q Quality) {
				wantRules := []QualityRule{
					{Product: "Fire TV", MediaType: core.MediaMovie, Profile: "4K", MaxHeight: 1080},
					{Product: "Fire TV", MediaType: core.MediaTV, Profile: "1080p", MaxHeight: 1080},
				}
				if !slices.Equal(q.Rules, wantRules) {
					t.Fatalf("divergent rules not split: got %+v, want %+v", q.Rules, wantRules)
				}
			},
		},
	}
	sortByMedia := func(names []string) []string {
		out := slices.Clone(names)
		slices.SortStableFunc(out, func(a, b string) int {
			aTV, bTV := strings.HasPrefix(a, "tv/"), strings.HasPrefix(b, "tv/")
			switch {
			case aTV == bTV:
				return 0
			case bTV:
				return -1
			}
			return 1
		})
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// After an older release saved over the profiles, a name it
			// listed once may come back for both media (it erased which one
			// the profile served); nothing is lost or duplicated.
			verify := func(label string, q Quality, exact bool) {
				t.Helper()
				assertSettled(t, label, q)
				got := sortByMedia(profileList(q))
				if exact && !slices.Equal(got, tc.want) {
					t.Fatalf("%s: profiles %v, want %v", label, got, tc.want)
				}
				for _, w := range tc.want {
					if !slices.Contains(got, w) {
						t.Fatalf("%s: profiles %v lost %s", label, got, w)
					}
				}
				if exact && tc.check != nil {
					tc.check(t, q)
				}
			}

			// Single server: config.json, opened again and again.
			dir := t.TempDir()
			raw, _ := json.Marshal(map[string]any{"quality": tc.quality})
			if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var first []string
			for i := range 3 {
				s, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				verify("open", s.Get().Quality, true)
				if i == 0 {
					first = profileList(s.Get().Quality)
				} else if got := profileList(s.Get().Quality); !slices.Equal(got, first) {
					t.Fatalf("open %d changed the profiles: %v, then %v", i+1, first, got)
				}
			}

			// Cluster: the shared document holds the same section.
			node, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			seal := func(quality map[string]any) string {
				var shared map[string]any
				if err := json.Unmarshal(SharedJSON(node.Get()), &shared); err != nil {
					t.Fatal(err)
				}
				shared["quality"] = quality
				b, _ := json.Marshal(shared)
				sealed, err := node.sealShared(b)
				if err != nil {
					t.Fatal(err)
				}
				return sealed
			}
			backend := &memShared{found: true, doc: SharedDoc{Doc: seal(tc.quality), Version: 1, KeyFingerprint: node.KeyFingerprint()}}
			if err := node.AttachShared(context.Background(), backend, SharedOptions{PollInterval: -1}); err != nil {
				t.Fatal(err)
			}
			verify("attach", node.Get().Quality, true)

			// A peer that predates per-media profiles saves over them.
			backend.mu.Lock()
			backend.doc.Doc = seal(savedByOlderRelease(t, node.Get().Quality))
			backend.doc.Version++
			backend.mu.Unlock()
			if err := node.RefreshShared(context.Background()); err != nil {
				t.Fatal(err)
			}
			verify("refresh", node.Get().Quality, false)
			refreshed := profileList(node.Get().Quality)
			// The next save by this node stores the settled profiles.
			if err := node.Update(func(c *Config) error { c.Quality.Sync.Enabled = !c.Quality.Sync.Enabled; return nil }); err != nil {
				t.Fatal(err)
			}
			node.Close()

			// Another node joins, and this node restarts: same profiles.
			for _, label := range []string{"second node", "restart"} {
				dir := t.TempDir()
				if label == "restart" {
					dir = filepath.Dir(node.Path())
				}
				peer, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				peer.credentialKey = node.credentialKey
				if err := peer.AttachShared(context.Background(), backend, SharedOptions{PollInterval: -1}); err != nil {
					t.Fatal(err)
				}
				verify(label, peer.Get().Quality, false)
				if got := profileList(peer.Get().Quality); !slices.Equal(got, refreshed) {
					t.Fatalf("%s: profiles %v, want the stored %v", label, got, refreshed)
				}
				peer.Close()
			}
		})
	}
}
