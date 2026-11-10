package plex

import (
	"testing"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// testStores returns the real sessions-backed store factory for tests.
func testStores() *sessions.LocalRuntime {
	return sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})
}

// testArtworkStores returns the real artwork-backed store factory for tests.
func testArtworkStores() frontend.PlexArtworkStores { return artworkservice.NewPlexArtworkStores() }

func withTestEncoder(enc frontend.PlexEncoder) func(*Deps) {
	return func(d *Deps) {
		d.Sessions = sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{Encoder: enc})
	}
}

// testDeps fills the required host factories in d from the real
// sessions/experience/artwork implementations, leaving explicitly set fields alone.
func testDeps(d Deps) Deps {
	if d.Sessions == nil {
		d.Sessions = sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{
			Playback:   d.Playback,
			Transcoder: d.Transcoder,
			DB:         d.DB,
			Logger:     d.Logger,
		})
	} else if lr, ok := d.Sessions.(*sessions.LocalRuntime); ok {
		lr.FillDefaults(sessions.LocalRuntimeDeps{
			Playback:   d.Playback,
			Transcoder: d.Transcoder,
			DB:         d.DB,
			Logger:     d.Logger,
		})
	}
	if d.Experience == nil {
		d.Experience = experience.NewPlexExperience()
	}
	if d.ArtworkStores == nil {
		d.ArtworkStores = testArtworkStores()
	}
	return d
}

func TestNewServerRequiresExperience(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without Experience did not panic")
		}
	}()
	NewServer(Deps{Sessions: testStores(), ArtworkStores: testArtworkStores()})
}

func TestNewServerRequiresSessionStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without Sessions did not panic")
		}
	}()
	NewServer(Deps{Experience: experience.NewPlexExperience(), ArtworkStores: testArtworkStores()})
}

func TestNewServerRequiresArtworkStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without ArtworkStores did not panic")
		}
	}()
	NewServer(Deps{Sessions: testStores(), Experience: experience.NewPlexExperience()})
}
