package plex

import (
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// benchConfigStore is a configuration about the size of a real install's:
// a dozen indexers and a few Usenet providers.
func benchConfigStore(tb testing.TB) *config.Store {
	tb.Helper()
	store, err := config.Open(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		for i := range 12 {
			c.Indexers = append(c.Indexers, config.Indexer{ID: fmt.Sprintf("ix%d", i), Name: fmt.Sprintf("Indexer %d", i), URL: "https://indexer.example/api", APIKey: "0123456789abcdef", Enabled: true, MovieCategories: []int{2000, 2010, 2040}, TVCategories: []int{5000, 5040}})
		}
		for i := range 4 {
			c.Usenet.Providers = append(c.Usenet.Providers, config.UsenetProvider{ID: fmt.Sprintf("p%d", i), Name: "Provider", Host: "news.example", Port: 563, TLS: true, Username: "user", Password: "password", Connections: 50})
		}
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
	return store
}

// BenchmarkTitleVisible is the per-title section check a Home or list page
// makes for every title it shows (GitHub #203): it must not copy the
// configuration.
func BenchmarkTitleVisible(b *testing.B) {
	store := benchConfigStore(b)
	s := testServer(Deps{})
	s.setTestConfig(store.Read)
	s.sections = s.buildSections()
	u := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{1, 2}}
	t := &core.Title{Key: core.EpisodeKey(1399, 1, 1), Type: core.TypeEpisode}
	b.ReportAllocs()
	for b.Loop() {
		if !s.titleVisible(u, t) {
			b.Fatal("hidden")
		}
	}
}

// BenchmarkSectionTitle is the titled section lookup a converted item makes.
func BenchmarkSectionTitle(b *testing.B) {
	store := benchConfigStore(b)
	s := testServer(Deps{})
	s.setTestConfig(store.Read)
	s.sections = s.buildSections()
	b.ReportAllocs()
	for b.Loop() {
		if sec, ok := s.sectionFor(core.TypeEpisode); !ok || sec.Title != "Series" {
			b.Fatal(sec.Title)
		}
	}
}

// TestVisibilityReadsNoConfiguration keeps the per-title checks off the
// configuration: a page checks every title it shows (GitHub #203), and the
// checks need section ids only.
func TestVisibilityReadsNoConfiguration(t *testing.T) {
	store := benchConfigStore(t)
	reads := 0
	s := testServer(Deps{})
	s.setTestConfig(func() config.Config { reads++; return store.Read() })
	s.sections = s.buildSections()
	reads = 0
	restricted := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{2}}
	movie := &core.Title{Key: core.RatingKey(603), Type: core.TypeMovie}
	episode := &core.Title{Key: core.EpisodeKey(1399, 1, 1), Type: core.TypeEpisode}
	if s.titleVisible(restricted, movie) || !s.titleVisible(restricted, episode) {
		t.Fatal("section access changed")
	}
	if s.canSeeSection(restricted, 1) || !s.canSeeSection(restricted, 2) || !s.canSeeSection(nil, 1) || s.canSeeSection(nil, 9) {
		t.Fatal("canSeeSection changed")
	}
	if reads != 0 {
		t.Fatalf("visibility checks read the configuration %d times", reads)
	}
}

// BenchmarkLeafCountVisibility is the GitHub #203 Home: watched-episode
// counts for 15 series of 300 episodes each pass every episode through the
// viewer's visibility filter.
func BenchmarkLeafCountVisibility(b *testing.B) {
	store := benchConfigStore(b)
	s := testServer(Deps{})
	s.setTestConfig(store.Read)
	s.sections = s.buildSections()
	u := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{1, 2}}
	var eps []core.Title
	for show := range 15 {
		for e := range 300 {
			eps = append(eps, core.Title{Key: core.EpisodeKey(int64(1000+show), 1+e/20, 1+e%20), Type: core.TypeEpisode})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if len(s.visibleTitles(u, eps)) != len(eps) {
			b.Fatal("hidden")
		}
	}
}
