package plex

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
)

type versionMeasureIndexer struct {
	searches atomic.Int64
	fetches  atomic.Int64
}

func (i *versionMeasureIndexer) Name() string { return "offline" }
func (i *versionMeasureIndexer) FetchNZB(context.Context, core.Release) ([]byte, error) {
	i.fetches.Add(1)
	return []byte("offline"), nil
}
func (i *versionMeasureIndexer) Search(_ context.Context, q core.SearchRequest) ([]core.Release, error) {
	i.searches.Add(1)
	var out []core.Release
	for n := 0; n < 24; n++ {
		height := []int{1080, 720, 2160}[n%3]
		name := fmt.Sprintf("%s.%d.%dp.WEB-DL.DDP5.1.H.264-GROUP%d", strings.ReplaceAll(q.Title, " ", "."), q.Year, height, n)
		out = append(out, core.Release{ID: fmt.Sprintf("%s-%d", q.Title, n), Name: name, Size: 8 << 30, Info: releaseparse.Parse(name), NZBURL: "offline"})
	}
	return out, nil
}

type versionMeasureResolver struct {
	*resolver.Service
	calls atomic.Int64
}

func (v *versionMeasureResolver) Variants(ctx context.Context, r core.ResolveRequest, n int) ([]core.ReleaseVariant, error) {
	v.calls.Add(1)
	return v.Service.Variants(ctx, r, n)
}

// Opt-in measurement, with the production cached resolver and offline indexer.
func TestVersionBrowseMeasurement(t *testing.T) {
	if os.Getenv("ETHER_339_MEASURE") == "" {
		t.Skip("opt-in")
	}
	cfg := config.Defaults(t.TempDir())
	cfg.Quality.Default = "measure"
	cfg.Quality.Remote = ""
	cfg.Quality.Rules = nil
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "measure", MaxHeight: 2160}}
	idx := &versionMeasureIndexer{}
	rs := resolver.New(idx, nil, nil, cfg, resolver.Options{Now: func() time.Time { return fixedNow }})
	defer rs.Close()
	vr := &versionMeasureResolver{Service: rs}
	f := newFixture(func(d *Deps) {
		d.Config = func() config.Config { return cfg }
		d.DisableEnrichment = true
		d.Hooks = Hooks{}
	})
	f.srv.setVariants(vr)
	defer f.srv.Close()
	titles := make([]core.Title, 200)
	f.catalog.titles = map[core.RatingKey]core.Title{}
	for i := range titles {
		x := fightClub
		x.Key = core.MovieKey(int64(90000 + i))
		x.Title = fmt.Sprintf("Fixture Film %03d", i)
		x.IDs = core.IDs{TMDB: int64(90000 + i)}
		titles[i] = x
		f.catalog.titles[x.Key] = x
		if i%5 != 0 {
			done := rs.RefillSearchCache(&x)
			if done == nil {
				t.Fatal("seed not admitted")
			}
			<-done
		}
	}
	hubs := []core.Hub{}
	for i := 0; i < 5; i++ {
		hubs = append(hubs, core.Hub{ID: fmt.Sprintf("movie.measure.%d", i), Title: "Measured shelf", Type: core.TypeMovie, Items: titles[i*40 : (i+1)*40]})
	}
	f.srv.d.Recommender = &fakeRecommender{home: hubs, section: map[core.MediaType][]core.Hub{core.TypeMovie: hubs}}
	caps := (&Request{User: &ownerUser, ClientID: "client-1", Product: "Plex for LG"}).ClientCaps(cfg.Transcode.RemoteMaxBitrateKbps)
	if vs, e := rs.Variants(context.Background(), core.ResolveRequest{Title: &titles[1], User: &ownerUser, Client: caps}, core.MaxVariants); e != nil || len(vs) < 2 {
		t.Fatalf("fixture variants=%d err=%v", len(vs), e)
	}
	for _, path := range []string{"/hubs/sections/1?count=40", "/hubs?count=40", "/library/sections/1/all?type=1&X-Plex-Container-Size=50"} {
		f.srv.playIDs = newPlayIdentities()
		calls := vr.calls.Load()
		searches := idx.searches.Load()
		fetches := idx.fetches.Load()
		run := func() (time.Duration, int) {
			start := time.Now()
			r := f.get(t, path, "Accept", "application/json", "X-Plex-Product", "Plex for LG")
			if r.Code != 200 {
				t.Fatalf("%s: %d", path, r.Code)
			}
			return time.Since(start), r.Body.Len()
		}
		cold, size := run()
		times := make([]time.Duration, 15)
		for j := range times {
			times[j], _ = run()
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s cold=%s warm_p50=%s warm_p95=%s bytes=%d variant_calls=%d searches=%d descriptor_fetches=%d", path, cold, times[7], times[14], size, vr.calls.Load()-calls, idx.searches.Load()-searches, idx.fetches.Load()-fetches)
	}
}
