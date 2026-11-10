package plex

import (
	"testing"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// testStores returns the real sessions-backed store factory for tests.
func testStores() frontend.SessionStores { return sessions.NewSessionStores() }

// testDeps fills the required host factories in d from the real
// sessions/experience implementations, leaving explicitly set fields alone.
func testDeps(d Deps) Deps {
	if d.SessionStores == nil {
		d.SessionStores = testStores()
	}
	if d.Experience == nil {
		d.Experience = experience.NewSelectors()
	}
	return d
}

func (s *Server) setImageURL(fn frontend.ImageURLFunc) {
	if fn == nil {
		fn = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
	}
	s.imageURL = fn
}

func (s *Server) setGeneratedPosters(g frontend.TitlePosterRenderer) {
	s.generatedPosters = g
}

func (s *Server) setLiveTV(ltv frontend.LiveTV) {
	s.liveTV = ltv
}

func (s *Server) setSupport(rec frontend.DiagnosticRecorder) {
	s.support = rec
}

func (s *Server) setClientSeen(fn func(string)) {
	s.clientSeen = fn
}

func (s *Server) setRemoteClientSeen(fn func(string)) {
	s.remoteClientSeen = fn
}

func TestNewServerRequiresExperience(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without Experience did not panic")
		}
	}()
	NewServer(Deps{SessionStores: testStores()})
}

func TestNewServerRequiresSessionStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without SessionStores did not panic")
		}
	}()
	NewServer(Deps{Experience: experience.NewSelectors()})
}
