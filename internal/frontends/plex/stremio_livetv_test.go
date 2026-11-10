package plex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/remote"
	"github.com/braedonsaunders/ether/internal/store"
)

// A Stremio `tv` add-on installed as a plugin feeds Plex's Live TV grid
// through the add-on channel source, with its native (dated catalog) guide.
func TestStremioAddonGuideOnPlexGrid(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/secret/manifest.json":
			fmt.Fprint(w, `{"id":"tv.example","version":"1.0.0","name":"Example TV","resources":["catalog","stream"],"types":["tv"],"catalogs":[{"type":"tv","id":"live","extra":[{"name":"date"}]}],"behaviorHints":{"epgProvider":true}}`)
		case strings.Contains(r.URL.Path, "date=") && !strings.Contains(r.URL.Path, "skip="):
			fmt.Fprint(w, `{"metasDetailed":[{"id":"c","name":"Test channel","videos":[{"id":"p","title":"Test broadcast","startTime":"2026-10-18T11:00:00Z","endTime":"2026-10-18T13:00:00Z"}]}]}`)
		case strings.Contains(r.URL.Path, "skip="):
			fmt.Fprint(w, `{"metasDetailed":[]}`)
		case strings.Contains(r.URL.Path, "/catalog/"):
			fmt.Fprint(w, `{"metas":[{"id":"c","name":"Test channel"}]}`)
		case strings.Contains(r.URL.Path, "/stream/"):
			fmt.Fprint(w, `{"streams":[{"url":"https://example.test/live"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := livetv.New(livetv.Options{DB: db, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := plugins.New(plugins.Deps{DB: db, Registry: extend.New(), Tools: tools.NewCatalogue(), Logger: log, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	mgr.RegisterRuntime("remote", (&remote.Factory{TrustedLAN: remote.GrantsLAN(mgr.GrantsOf), Logger: log}).RuntimeFactory())
	adapters.Attach(mgr, nil, nil, s)
	rec, err := mgr.InstallAddon(ctx, plugins.AddonInstall{Link: upstream.URL + "/secret/manifest.json", Roles: []plugins.Capability{plugins.CapLiveTV}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportSource(ctx, livetv.Source{Name: "Test", Kind: extend.PluginID(rec.ID, plugins.StremioChannelsKey)}); err != nil {
		t.Fatal(err)
	}
	channels, err := s.Channels(ctx)
	if err != nil || len(channels) != 1 {
		t.Fatalf("channels=%+v err=%v", channels, err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.LiveTV = s.Frontend()
	f.srv.setTestSubscription(true)
	res := f.request(t, "GET", liveProviderPath+"/grid?date=2026-10-18&channelGridKey="+channels[0].ID, map[string]string{"Accept": "application/json"})
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Test broadcast") || strings.Contains(res.Body.String(), "secret") {
		t.Fatalf("grid: %d %s", res.Code, res.Body.String())
	}
}

func TestMatchedChannelLogoUsesArtworkProxy(t *testing.T) {
	c := livetv.ToFrontendChannel(livetv.Channel{ID: "channel", Logo: "https://example.test/logo.png", LogoMatched: true})
	if got := liveLogo(c); got != "/livetv/channels/channel/logo.png" {
		t.Fatalf("matched logo path = %q", got)
	}
	c.LogoMatched = false
	if got := liveLogo(c); got != "/livetv/channels/channel/logo.png" {
		t.Fatalf("original logo path changed: %q", got)
	}
}
