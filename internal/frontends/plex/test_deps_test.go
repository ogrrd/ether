package plex

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// testStores returns the real sessions-backed store factory for tests.
func testStores() frontend.PlexSessionStores { return sessions.NewPlexSessionStores() }

// testArtworkStores returns the real artwork-backed store factory for tests.
func testArtworkStores() frontend.PlexArtworkStores { return artworkservice.NewPlexArtworkStores() }

// testDeps fills the required host factories in d from the real
// sessions/experience/artwork implementations, leaving explicitly set fields alone.
func testDeps(d Deps) Deps {
	if d.SessionStores == nil {
		d.SessionStores = testStores()
	}
	if d.Experience == nil {
		d.Experience = experience.NewPlexExperience()
	}
	if d.ArtworkStores == nil {
		d.ArtworkStores = testArtworkStores()
	}
	return d
}

func testServer(d Deps) *Server {
	d = testDeps(d)
	return &Server{
		d:    d,
		host: d.Host,
		log:  slog.Default(),
	}
}

func updateHostSettings(cfgFn *func() frontend.Settings, mutate func(frontend.Settings)) {
	prev := *cfgFn
	*cfgFn = func() frontend.Settings {
		out := frontend.Settings{}
		if prev != nil {
			for k, v := range prev() {
				out[k] = v
			}
		}
		mutate(out)
		return out
	}
}

func setDepsConfig(d *Deps, fn func() config.Config) {
	if fn == nil {
		updateHostSettings(&d.Host.Config, func(s frontend.Settings) {
			delete(s, "hostConfig")
		})
		return
	}
	updateHostSettings(&d.Host.Config, func(s frontend.Settings) {
		c := fn()
		s["hostConfig"] = c
		s["serverName"] = c.Server.FriendlyName
		s["configFunc"] = fn
	})
}

func depsConfig(d *Deps) func() config.Config {
	if d == nil || d.Host.Config == nil {
		return nil
	}
	if fn, ok := d.Host.Config()["configFunc"].(func() config.Config); ok {
		return fn
	}
	return nil
}

func (s *Server) setTestConfig(fn func() config.Config) {
	if fn == nil {
		updateHostSettings(&s.host.Config, func(st frontend.Settings) {
			delete(st, "hostConfig")
			delete(st, "configFunc")
		})
		return
	}
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		c := fn()
		st["hostConfig"] = c
		st["serverName"] = c.Server.FriendlyName
		st["configFunc"] = fn
	})
}

func (s *Server) testConfig() func() config.Config {
	if s == nil || s.host.Config == nil {
		return nil
	}
	if fn, ok := s.host.Config()["configFunc"].(func() config.Config); ok {
		return fn
	}
	return nil
}

func setDepsNow(d *Deps, fn func() time.Time) {
	updateHostSettings(&d.Host.Config, func(st frontend.Settings) {
		st["now"] = fn
	})
}

func depsNow(d *Deps) func() time.Time {
	if d == nil || d.Host.Config == nil {
		return nil
	}
	if fn, ok := d.Host.Config()["now"].(func() time.Time); ok {
		return fn
	}
	return nil
}

func (s *Server) setTestNow(fn func() time.Time) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["now"] = fn
	})
}

func setDepsIdentity(d *Deps, id Identity) {
	updateHostSettings(&d.Host.Config, func(st frontend.Settings) {
		if id.MachineIdentifier != "" {
			st["machineIdentifier"] = id.MachineIdentifier
		}
		if id.FriendlyName != "" {
			st["friendlyName"] = id.FriendlyName
		}
		if id.Version != "" {
			st["version"] = id.Version
		}
		if id.Platform != "" {
			st["platform"] = id.Platform
		}
		if id.PlatformVersion != "" {
			st["platformVersion"] = id.PlatformVersion
		}
		st["myPlexSubscription"] = id.MyPlexSubscription
		st["ownerFeatures"] = id.OwnerFeatures
	})
}

func (s *Server) setTestIdentity(id Identity) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["machineIdentifier"] = id.MachineIdentifier
		st["friendlyName"] = id.FriendlyName
		st["version"] = id.Version
		st["platform"] = id.Platform
		st["platformVersion"] = id.PlatformVersion
		st["myPlexSubscription"] = id.MyPlexSubscription
		st["ownerFeatures"] = id.OwnerFeatures
	})
}

func (s *Server) setTestSubscription(sub bool) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["myPlexSubscription"] = sub
	})
}

func (s *Server) setTestPlatformVersion(v string) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["platformVersion"] = v
	})
}

func setDepsOwnerSubscription(d *Deps, fn func() (bool, []string)) {
	updateHostSettings(&d.Host.Config, func(st frontend.Settings) {
		st["ownerSubscription"] = fn
	})
}

func (s *Server) setTestOwnerSubscription(fn func() (bool, []string)) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["ownerSubscription"] = fn
	})
}

func setDepsRepoFileURL(d *Deps, fn func(string) string) {
	updateHostSettings(&d.Host.Config, func(st frontend.Settings) {
		st["repoFileURL"] = fn
	})
}

func (s *Server) setTestRepoFileURL(fn func(string) string) {
	updateHostSettings(&s.host.Config, func(st frontend.Settings) {
		st["repoFileURL"] = fn
	})
}

func setDepsPublicURL(d *Deps, fn func() string) {
	if fn == nil {
		d.Host.SurfaceURL = nil
		return
	}
	d.Host.SurfaceURL = func(context.Context, *http.Request) (string, error) {
		return fn(), nil
	}
}

func (s *Server) setTestPublicURL(fn func() string) {
	if fn == nil {
		s.host.SurfaceURL = nil
		return
	}
	s.host.SurfaceURL = func(context.Context, *http.Request) (string, error) {
		return fn(), nil
	}
}

func setDepsAuth(d *Deps, a core.Auth) {
	d.Host.Identity = frontend.BindAuth(a)
}

func (s *Server) setTestAuth(a core.Auth) {
	s.host.Identity = frontend.BindAuth(a)
}

func (s *Server) testAuth() *fakeAuth {
	if u, ok := s.host.Identity.(interface{ UnwrapAuth() core.Auth }); ok {
		fa, _ := u.UnwrapAuth().(*fakeAuth)
		return fa
	}
	return nil
}

func setDepsPolicy(d *Deps, p frontend.ViewerPolicy) {
	d.Host.ViewerPolicy = p
}

func (s *Server) setTestPolicy(p frontend.ViewerPolicy) {
	s.host.ViewerPolicy = p
}

func setDepsHTTPClient(d *Deps, c *http.Client) {
	if d.ArtworkStores == nil {
		d.ArtworkStores = testArtworkStores()
	}
	bytes := d.ImageCacheBytes
	if bytes <= 0 {
		bytes = 128 << 20
	}
	d.ArtworkImages = d.ArtworkStores.NewRemoteCache(bytes, c)
}

func setDepsDirectHTTP(d *Deps, c *http.Client) {
	if c == nil {
		d.Host.DirectProxy = nil
		return
	}
	if tr, ok := c.Transport.(*http.Transport); ok && tr != nil && tr.Proxy != nil {
		d.Host.DirectProxy = tr.Proxy
		return
	}
	d.Host.DirectProxy = func(*http.Request) (*url.URL, error) { return nil, nil }
}

func TestNewServerRequiresExperience(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without Experience did not panic")
		}
	}()
	NewServer(Deps{SessionStores: testStores(), ArtworkStores: testArtworkStores()})
}

func TestNewServerRequiresSessionStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without SessionStores did not panic")
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
	NewServer(Deps{SessionStores: testStores(), Experience: experience.NewPlexExperience()})
}
