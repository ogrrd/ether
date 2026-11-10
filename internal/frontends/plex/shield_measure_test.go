package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
)

type shieldMeasureIndexer struct{ searches atomic.Int64 }

func (*shieldMeasureIndexer) Name() string { return "offline-shield" }
func (*shieldMeasureIndexer) FetchNZB(context.Context, core.Release) ([]byte, error) {
	return nil, fmt.Errorf("torrent must use debrid indexer")
}
func (i *shieldMeasureIndexer) Search(ctx context.Context, q core.SearchRequest) ([]core.Release, error) {
	i.searches.Add(1)
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	name := strings.ReplaceAll(q.Title, " ", ".") + fmt.Sprintf(".%d.1080p.WEB-DL.DDP5.1.H264-LAB", q.Year)
	return []core.Release{{ID: "shield-measure", GUID: "shield-measure", Indexer: i.Name(), Name: name, Size: 4 << 30, Protocol: core.ProtocolTorrent, InfoHash: strings.Repeat("a", 40), MagnetURL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40), Info: releaseparse.Parse(name), PubDate: time.Now().Add(-24 * time.Hour)}}, nil
}

type shieldMeasureRig struct {
	f          *fixture
	mgr        *playback.Manager
	rs         *resolver.Service
	engine     *debrid.Engine
	upstream   *httptest.Server
	idx        *shieldMeasureIndexer
	api, media atomic.Int64
	probeBlock <-chan struct{}
}

func newShieldMeasureRig(t *testing.T, mode string, cached bool) *shieldMeasureRig {
	t.Helper()
	x := &shieldMeasureRig{idx: &shieldMeasureIndexer{}}
	var base string
	x.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media" {
			x.media.Add(1)
			time.Sleep(5 * time.Millisecond)
			http.ServeContent(w, r, "troy.mkv", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("x"), 65536)))
			return
		}
		x.api.Add(1)
		time.Sleep(time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/torrents":
			fmt.Fprintf(w, `[{"id":"lab","hash":"%s","status":"downloaded"}]`, strings.Repeat("a", 40))
		case "/torrents/info/lab":
			fmt.Fprintf(w, `{"id":"lab","status":"downloaded","files":[{"id":1,"path":"/Fight.Club.1999.mkv","bytes":65536,"selected":1}],"links":["https://host.invalid/file"]}`)
		case "/unrestrict/link":
			fmt.Fprintf(w, `{"download":%q,"filesize":65536}`, base+"/media")
		default:
			http.Error(w, "unexpected fixture request: "+r.URL.Path, 500)
		}
	}))
	base = x.upstream.URL
	cfg := config.Defaults(t.TempDir())
	cfg.Quality.Default = "lab"
	cfg.Quality.Remote = ""
	cfg.Quality.Rules = nil
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "lab", MaxHeight: 2160}}
	switch mode {
	case "detail-off":
		cfg.Cache.DetailWarmup = "off"
	case "speculation-off":
		cfg.Cache.DisableSpeculation = true
	case "cache-off":
		cfg.Cache.Mode = "off"
	}
	cfg.Debrid = config.Debrid{CachedOnly: true, Providers: []config.DebridProvider{{ID: "lab", Kind: config.DebridRealDebrid, APIKey: "offline", Enabled: true}}}
	svc := debrid.NewService(cfg.Debrid, debrid.Options{ProviderOptions: debrid.ProviderOptions{BaseURL: base, HTTPClient: x.upstream.Client()}})
	x.engine = debrid.NewEngine(svc, debrid.EngineOptions{HTTPClient: x.upstream.Client(), HedgeDelay: -1})
	x.rs = resolver.New(debrid.NewIndexer(x.idx, svc), nil, nil, cfg, resolver.Options{})
	x.f = newFixture(func(d *Deps) {
		setDepsConfig(d, func() config.Config { return cfg })
		d.DisableEnrichment = true
		d.Hooks = Hooks{}
	})
	var err error
	x.mgr, err = playback.New(playback.Deps{Catalog: x.f.catalog, State: x.f.state, Resolver: x.rs, Engine: x.engine, Transcoder: x.f.trans, SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return x.rs.ProfileFor(c, u) }, NativeProbe: func(ctx context.Context, src core.MediaSource) (*core.MediaInfo, error) {
		if x.probeBlock != nil {
			select {
			case <-x.probeBlock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if _, err := src.ReadAt(make([]byte, 64), 0); err != nil {
			return nil, err
		}
		info := core.CloneMediaInfo(sampleInfo)
		info.Size = src.Size()
		return &info, nil
	}}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		x.Close()
		t.Fatal(err)
	}
	x.f.srv.d.Playback = x.mgr
	x.f.srv.d.Variants = x.rs
	if cached {
		done := x.rs.RefillSearchCache(&fightClub)
		if done == nil {
			t.Fatal("seed search skipped")
		}
		<-done
	}
	x.idx.searches.Store(0)
	x.api.Store(0)
	x.media.Store(0)
	return x
}
func (x *shieldMeasureRig) Close() {
	if x.f != nil {
		x.f.srv.Close()
	}
	if x.mgr != nil {
		x.mgr.Close()
	}
	if x.rs != nil {
		x.rs.Close()
	}
	if x.engine != nil {
		x.engine.Close()
	}
	if x.upstream != nil {
		x.upstream.Close()
	}
}
func (x *shieldMeasureRig) view(t *testing.T) (time.Duration, int64, int64, int64) {
	t.Helper()
	s, a, m := x.idx.searches.Load(), x.api.Load(), x.media.Load()
	start := time.Now()
	r := x.f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "SHIELD Android TV")
	elapsed := time.Since(start)
	if r.Code != 200 {
		t.Fatalf("detail: %d %s", r.Code, r.Body.String())
	}
	return elapsed, x.idx.searches.Load() - s, x.api.Load() - a, x.media.Load() - m
}

// Opt-in offline measurement of the real HTTP metadata, resolver, playback,
// Real-Debrid provider and range engine. No account or external service is used.
// Raw samples belong in thread storage; the fixture is the lasting method.
func TestShieldDetailMeasurement(t *testing.T) {
	if os.Getenv("ETHER_191_MEASURE") == "" {
		t.Skip("opt-in measurement")
	}
	for _, mode := range []string{"on", "detail-off", "speculation-off", "cache-off"} {
		for _, cached := range []bool{false, true} {
			var cold, warm []time.Duration
			var cs, ca, cm, ws, wa, wm int64
			for n := 0; n < 20; n++ {
				func() {
					x := newShieldMeasureRig(t, mode, cached)
					defer x.Close()
					d, s, a, m := x.view(t)
					cold = append(cold, d)
					cs += s
					ca += a
					cm += m
					d, s, a, m = x.view(t)
					warm = append(warm, d)
					ws += s
					wa += a
					wm += m
				}()
			}
			sort.Slice(cold, func(i, j int) bool { return cold[i] < cold[j] })
			sort.Slice(warm, func(i, j int) bool { return warm[i] < warm[j] })
			row := map[string]any{"setting": mode, "search_cached": cached, "samples": 20, "cold_p50_ms": float64(cold[9]) / 1e6, "cold_p95_ms": float64(cold[18]) / 1e6, "warm_p50_ms": float64(warm[9]) / 1e6, "warm_p95_ms": float64(warm[18]) / 1e6, "cold_searches_per_view": float64(cs) / 20, "cold_debrid_api_per_view": float64(ca) / 20, "cold_debrid_media_per_view": float64(cm) / 20, "warm_searches_per_view": float64(ws) / 20, "warm_debrid_api_per_view": float64(wa) / 20, "warm_debrid_media_per_view": float64(wm) / 20}
			b, _ := json.Marshal(row)
			t.Log(string(b))
		}
	}
}

func TestShieldDisabledDetailDoesNoSourceIO(t *testing.T) {
	for _, mode := range []string{"detail-off", "speculation-off", "cache-off"} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cached=%v", mode, cached), func(t *testing.T) {
				x := newShieldMeasureRig(t, mode, cached)
				x.view(t)
				x.view(t)
				x.Close() // include background work before counting
				if s, a, m := x.idx.searches.Load(), x.api.Load(), x.media.Load(); s != 0 || a != 0 || m != 0 {
					t.Fatalf("disabled detail IO: search=%d api=%d media=%d", s, a, m)
				}
			})
		}
	}
}

func TestShieldSlowMetadataKeepsNormalResponseAndPublishesLater(t *testing.T) {
	x := newShieldMeasureRig(t, "on", true)
	defer x.Close()
	gate := make(chan struct{})
	x.probeBlock = gate
	// Make a version lookup wait its entire context allowance. It must share
	// the probe budget, never add the usual three seconds after the probe.
	x.f.srv.d.Variants = &shieldSlowVariants{VariantResolver: x.rs}
	d, _, _, _ := x.view(t)
	if d < 3800*time.Millisecond || d > 5500*time.Millisecond {
		t.Fatalf("slow detail wait %s, want about four seconds total", d)
	}
	close(gate)
	caps := (&Request{User: &ownerUser, ClientID: "client-1", Product: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV"}).ClientCaps(0)
	if _, ok := x.mgr.WaitWarmProbe(t.Context(), &ownerUser, caps, fightClub.Key, time.Second); !ok {
		t.Fatal("request timeout canceled background probe")
	}
	x.f.srv.d.Variants = x.rs
	r := x.f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android")
	if r.Code != 200 || len(subtitleTags(r.Body.String())) != 2 {
		t.Fatalf("later detail lost completed probe: %d %s", r.Code, r.Body.String())
	}
}

type shieldSlowVariants struct{ core.VariantResolver }

func (v *shieldSlowVariants) Variants(ctx context.Context, r core.ResolveRequest, n int) ([]core.ReleaseVariant, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
