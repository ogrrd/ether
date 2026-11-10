package plex

import (
	"context"
	"testing"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type testHomeCache struct {
	Revision       func(context.Context, *frontend.User) (frontend.HomeRevision, error)
	NoProviderWait func(context.Context) context.Context
	Layout         func() uint64
	Config         func() uint64
	Catalog        func() uint64
}

type testServerDeps struct {
	Deps
	Experience          frontend.Experience
	HomeCache           *testHomeCache
	Catalog             frontend.Catalog
	Collections         frontend.Collections
	State               frontend.State
	Playlists           frontend.Playlists
	Recommender         frontend.Recommender
	VibeSearch          frontend.VibeSearcher
	Search              frontend.SearchSources
	FavoriteShows       func(context.Context, *frontend.User) (map[frontend.RatingKey]bool, error)
	FilterAvailable     func(context.Context, []frontend.Title) []frontend.Title
	JudgedOnFacts       func(context.Context) func(frontend.Title) bool
	PrioritizeAvailable func(context.Context, []frontend.Title) []frontend.Title
	EpisodeOrder        frontend.EpisodeOrder
	Discovery           *frontend.DiscoveryFeatures
	Dashboard           frontend.Dashboard
	Resources           frontend.ResourceStatistics
	LibraryTitles       func() (movies, shows string)
}

func (d testServerDeps) experienceService() frontend.Experience {
	if d.Experience != nil {
		return d.Experience
	}
	var rev func(context.Context, *frontend.User) (frontend.HomeRevision, error)
	var noWait func(context.Context) context.Context
	if d.HomeCache != nil {
		rev = d.HomeCache.Revision
		noWait = d.HomeCache.NoProviderWait
	}
	return experience.NewDirectService(experience.ServiceDeps{
		Catalog:             d.Catalog,
		Collections:         d.Collections,
		State:               d.State,
		Playlists:           d.Playlists,
		Recommender:         d.Recommender,
		VibeSearch:          d.VibeSearch,
		SearchSources:       d.Search,
		FavoriteShows:       d.FavoriteShows,
		FilterAvailable:     d.FilterAvailable,
		JudgedOnFacts:       d.JudgedOnFacts,
		PrioritizeAvailable: d.PrioritizeAvailable,
		EpisodeOrder:        d.EpisodeOrder,
		Discovery:           d.Discovery,
		Dashboard:           d.Dashboard,
		Resources:           d.Resources,
		LibraryTitles:       d.LibraryTitles,
		HomeRevision:        rev,
		NoProviderWait:      noWait,
		Now:                 d.Now,
	})
}

func (d testServerDeps) pageFreshnessFunc() func() frontend.PageFreshness {
	if d.HomeCache == nil {
		return nil
	}
	hc := d.HomeCache
	return func() frontend.PageFreshness {
		var f frontend.PageFreshness
		if hc.Layout != nil {
			f.Layout = hc.Layout()
		}
		if hc.Config != nil {
			f.Config = hc.Config()
		}
		if hc.Catalog != nil {
			f.Catalog = hc.Catalog()
		}
		return f
	}
}

// testStores returns the real sessions-backed store factory for tests.
func testStores() frontend.PlexSessionStores { return sessions.NewPlexSessionStores() }

// testArtworkStores returns the real artwork-backed store factory for tests.
func testArtworkStores() frontend.PlexArtworkStores { return artworkservice.NewPlexArtworkStores() }

// testDeps fills the required host factories in d from the real
// sessions/artwork implementations, leaving explicitly set fields alone.
func testDeps(d Deps) Deps {
	if d.SessionStores == nil {
		d.SessionStores = testStores()
	}
	if d.ArtworkStores == nil {
		d.ArtworkStores = testArtworkStores()
	}
	return d
}

func newTestServer(d testServerDeps) *Server {
	srv := NewServer(testDeps(d.Deps))
	srv.attachHost(frontend.Host{
		Experience:    d.experienceService(),
		PageFreshness: d.pageFreshnessFunc(),
	})
	return srv
}

func (s *Server) currentTestDeps() testServerDeps {
	if s == nil {
		return testServerDeps{}
	}
	return testServerDeps{
		Deps:                s.d,
		Catalog:             s.catalog(),
		Collections:         s.collections(),
		State:               s.state(),
		Playlists:           s.playlists(),
		Recommender:         s.recommender(),
		VibeSearch:          s.vibeSearch(),
		Search:              s.searchSources(),
		FavoriteShows:       s.favoriteShows(),
		FilterAvailable:     s.filterAvailable(),
		JudgedOnFacts:       s.judgedOnFacts(),
		PrioritizeAvailable: s.prioritizeAvailable(),
		EpisodeOrder:        s.episodeOrder(),
		Discovery:           s.discovery(),
		Dashboard:           s.dashboard(),
		Resources:           s.resources(),
		LibraryTitles:       s.libraryTitles,
	}
}

func (s *Server) updateTestDeps(fn func(*testServerDeps)) {
	d := s.currentTestDeps()
	fn(&d)
	s.d = d.Deps
	s.attachHost(frontend.Host{
		Experience:    d.experienceService(),
		PageFreshness: d.pageFreshnessFunc(),
	})
}

func (s *Server) setCatalog(v frontend.Catalog) {
	s.updateTestDeps(func(d *testServerDeps) { d.Catalog = v })
}
func (s *Server) setCollections(v frontend.Collections) {
	s.updateTestDeps(func(d *testServerDeps) { d.Collections = v })
}
func (s *Server) setState(v frontend.State) {
	s.updateTestDeps(func(d *testServerDeps) { d.State = v })
}
func (s *Server) setPlaylists(v frontend.Playlists) {
	s.updateTestDeps(func(d *testServerDeps) { d.Playlists = v })
}
func (s *Server) setRecommender(v frontend.Recommender) {
	s.updateTestDeps(func(d *testServerDeps) { d.Recommender = v })
}
func (s *Server) setFilterAvailable(v func(context.Context, []frontend.Title) []frontend.Title) {
	s.updateTestDeps(func(d *testServerDeps) { d.FilterAvailable = v })
}
func (s *Server) setEpisodeOrder(v frontend.EpisodeOrder) {
	s.updateTestDeps(func(d *testServerDeps) { d.EpisodeOrder = v })
}
func (s *Server) setDiscovery(v *frontend.DiscoveryFeatures) {
	s.updateTestDeps(func(d *testServerDeps) { d.Discovery = v })
}
func (s *Server) setDashboard(v frontend.Dashboard) {
	s.updateTestDeps(func(d *testServerDeps) { d.Dashboard = v })
}
func (s *Server) setResources(v frontend.ResourceStatistics) {
	s.updateTestDeps(func(d *testServerDeps) { d.Resources = v })
}
func (s *Server) setLibraryTitles(v func() (string, string)) {
	s.updateTestDeps(func(d *testServerDeps) { d.LibraryTitles = v })
}

func TestNewServerRequiresSessionStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without SessionStores did not panic")
		}
	}()
	NewServer(Deps{ArtworkStores: testArtworkStores()})
}

func TestNewServerRequiresArtworkStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without ArtworkStores did not panic")
		}
	}()
	NewServer(Deps{SessionStores: testStores()})
}
