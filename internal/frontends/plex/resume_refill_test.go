package plex

// Intent: ETHE-18 asks that an admitted remembered-release warm-up let a resumed episode,
// without an owner pin, regain Play Versions after background refill completion.
// Taskmaster identity/key comes from the report. Release descriptors, media
// facts, engine and auth are explicit offline stand-ins, not production evidence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

type etheLabIndexer struct {
	releases []core.Release
	searches atomic.Int32
	fetches  atomic.Int32
	started  chan struct{}
	release  chan struct{}
	failure  error
}

func (i *etheLabIndexer) Name() string { return "offline-resume-lab" }
func (i *etheLabIndexer) Search(ctx context.Context, _ core.SearchRequest) ([]core.Release, error) {
	i.searches.Add(1)
	if i.started != nil {
		i.started <- struct{}{}
	}
	if i.release != nil {
		select {
		case <-i.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if i.failure != nil {
		return nil, i.failure
	}
	return append([]core.Release(nil), i.releases...), nil
}
func (i *etheLabIndexer) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	i.fetches.Add(1)
	return []byte("offline-descriptor-" + r.ID), nil
}

type etheLabEngine struct {
	issue94Engine
	finished chan context.Context
}

func (e *etheLabEngine) Prefetch(ctx context.Context, _ string, _ []byte, _, _ int64) error {
	e.finished <- ctx
	return nil
}

// Observe the real non-blocking scheduling seam without starting work from a test.
type observedRefillResolver struct {
	*resolver.Service
	scheduled chan (<-chan struct{})
}

func (r *observedRefillResolver) RefillSearchCache(title *core.Title) <-chan struct{} {
	done := r.Service.RefillSearchCache(title)
	r.scheduled <- done
	return done
}

type etheLab struct {
	f        *fixture
	cfg      config.Config
	title    core.Title
	idx      *etheLabIndexer
	engine   *etheLabEngine
	rs       *resolver.Service
	observed *observedRefillResolver
	mgr      *playback.Manager
	db       *sql.DB
	caps     core.ClientCaps
	dir      string
}

func newEtheLab(t *testing.T, resumed bool, remembered string, n int) *etheLab {
	t.Helper()
	x := &etheLab{dir: t.TempDir()}
	x.cfg = config.Defaults(x.dir)
	x.cfg.Resolver.SameQualityAlternatives = n
	x.cfg.Quality.Default = "lab1080"
	x.cfg.Quality.Profiles = []core.QualityProfile{{Name: "lab1080", MaxHeight: 1080}}
	x.cfg.Quality.Rules = nil
	x.cfg.Quality.Remote = ""
	x.title = core.Title{Key: core.RatingKey(4000634040030005), Type: core.TypeEpisode, ShowTitle: "Taskmaster", Title: "Taskmaster S03E05", Year: 2016, Season: 3, Episode: 5, Runtime: 45 * time.Minute, Language: "en"}
	x.idx = &etheLabIndexer{}
	for i, group := range []string{"FLUX", "EDITH", "NTb", "OTHER", "REMEMBERED"} {
		name := fmt.Sprintf("Taskmaster.S03E05.1080p.WEB-DL.DDP5.1.H.264-%s", group)
		x.idx.releases = append(x.idx.releases, core.Release{ID: fmt.Sprintf("offline-%d", i), GUID: fmt.Sprintf("offline-%d", i), Indexer: "offline", Name: name, Size: int64(1500+i*100) << 20, PubDate: fixedNow.Add(-24 * time.Hour), NZBURL: "offline", Info: releaseparse.Parse(name)})
	}
	x.engine = &etheLabEngine{finished: make(chan context.Context, 8)}
	x.f = newFixture(func(d *Deps) {
		d.Config = func() config.Config { return x.cfg }
		d.DisableEnrichment = true
		d.Hooks = Hooks{}
	})
	x.f.catalog.titles[x.title.Key] = x.title
	x.f.state.set(ownerUser.ID, core.WatchStatus{Key: x.title.Key, ViewOffset: 10 * time.Minute})
	var err error
	x.db, err = store.Open(context.Background(), x.dir)
	if err != nil {
		t.Fatal(err)
	}
	x.caps = (&Request{User: &ownerUser, ClientID: "client-1", Product: "Plex Web"}).ClientCaps(x.cfg.Transcode.RemoteMaxBitrateKbps)
	x.start(t)
	if resumed {
		ps, err := x.mgr.PrepareVariant(context.Background(), &ownerUser, x.title.Key, x.caps, remembered)
		if err != nil {
			t.Fatal(err)
		}
		if ps.Resolution.Release.ID != remembered {
			t.Fatalf("seed release = %s", ps.Resolution.Release.ID)
		}
		// A clean local shutdown/reopen preserves the resume row and probe, not caches.
		x.f.srv.Close()
		x.mgr.Close()
		x.rs.Close()
		x.db.Close()
		x.db, err = store.Open(context.Background(), x.dir)
		if err != nil {
			t.Fatal(err)
		}
		x.f = newFixture(func(d *Deps) {
			d.Config = func() config.Config { return x.cfg }
			d.DisableEnrichment = true
			d.Hooks = Hooks{}
		})
		x.f.catalog.titles[x.title.Key] = x.title
		x.f.state.set(ownerUser.ID, core.WatchStatus{Key: x.title.Key, ViewOffset: 10 * time.Minute})
		x.start(t)
	}
	x.idx.searches.Store(0)
	x.idx.fetches.Store(0)
	t.Cleanup(func() { x.f.srv.Close(); x.mgr.Close(); x.rs.Close(); x.db.Close() })
	return x
}
func (x *etheLab) start(t *testing.T) {
	t.Helper()
	// A nil resolver DB ensures there are no owner pins or remembered candidates.
	x.rs = resolver.New(x.idx, nil, nil, x.cfg, resolver.Options{Now: func() time.Time { return fixedNow }})
	x.observed = &observedRefillResolver{Service: x.rs, scheduled: make(chan (<-chan struct{}), 16)}
	var err error
	x.mgr, err = playback.New(playback.Deps{Catalog: x.f.catalog, State: x.f.state, Resolver: x.observed, Engine: x.engine, Transcoder: x.f.trans, SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return x.rs.ProfileFor(c, u) }, DB: x.db, NativeProbe: func(_ context.Context, s core.MediaSource) (*core.MediaInfo, error) {
		info := sampleInfo
		info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
		info.Duration = x.title.Runtime
		info.Size = s.Size()
		return &info, nil
	}}, x.cfg, playback.Options{DataDir: x.dir, Now: func() time.Time { return fixedNow }, PinInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	x.f.srv.d.Playback = x.mgr
	x.f.srv.setVariants(x.observed)
}
func (x *etheLab) detail(t *testing.T, label string) []map[string]any {
	t.Helper()
	r := x.f.get(t, "/library/metadata/"+x.title.Key.String(), "X-Plex-Product", "Plex Web", "Accept", "application/json")
	if r.Code != 200 {
		t.Fatalf("detail %s status %d: %s", label, r.Code, r.Body.String())
	}
	var response struct {
		MediaContainer struct {
			Metadata []struct {
				Media []map[string]any `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.MediaContainer.Metadata) != 1 {
		t.Fatal("missing item")
	}
	media := response.MediaContainer.Metadata[0].Media
	t.Logf("%s: Media=%d searches=%d descriptorFetches=%d", label, len(media), x.idx.searches.Load(), x.idx.fetches.Load())
	if dir := os.Getenv("ETHE_LAB_EVIDENCE"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "-")+"-"+label+".json"), r.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return media
}
func (x *etheLab) waitWarm(t *testing.T) {
	t.Helper()
	select {
	case ctx := <-x.engine.finished:
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("warm did not finish")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prewarm never reached offline prefetch")
	}
}
func (x *etheLab) scheduledRefill(t *testing.T) <-chan struct{} {
	t.Helper()
	select {
	case done := <-x.observed.scheduled:
		return done
	case <-time.After(5 * time.Second):
		t.Fatal("remembered detail warm never requested refill")
		return nil
	}
}
func waitResumeRefill(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		t.Fatal("expected admitted refill")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refill did not complete")
	}
}

func TestResumedDetailColdRestartRefill(t *testing.T) {
	x := newEtheLab(t, true, "offline-1", 3)
	if _, id, ok := x.mgr.ResumedTracks(context.Background(), &ownerUser, x.title.Key, x.caps); !ok || id != "offline-1" {
		t.Fatalf("resume probe missing: %s %t", id, ok)
	}
	if _, ok := x.rs.PinFor(x.title.Key); ok {
		t.Fatal("unexpected owner pin")
	}
	x.detail(t, "first")
	x.waitWarm(t)
	waitResumeRefill(t, x.scheduledRefill(t))
	second := x.detail(t, "second")
	if len(second) != 4 {
		t.Fatalf("reported intent failed: second page Media=%d, want Auto + 3 alternatives; searches=%d", len(second), x.idx.searches.Load())
	}
	if x.idx.searches.Load() != 1 || x.idx.fetches.Load() != 0 {
		t.Fatal("refill must search only once")
	}
	ids, ok := x.f.srv.playIDs.releases(playIdentityKey{ownerUser.ID, "client-1", x.title.Key}, fixedNow)
	if !ok {
		t.Fatal("render order missing")
	}
	for i, id := range ids {
		if id == "offline-1" {
			if second[i]["selected"] != true {
				t.Fatal("offered remembered release not selected")
			}
			return
		}
	}
	t.Fatalf("remembered candidate missing: %v", ids)
}

func TestResumedDetailNoResumeControl(t *testing.T) {
	x := newEtheLab(t, false, "", 3)
	x.detail(t, "first")
	x.waitWarm(t)
	waitResumeRefill(t, x.scheduledRefill(t))
	if media := x.detail(t, "second"); len(media) != 4 {
		t.Fatalf("no-resume control Media=%d want 4", len(media))
	}
	if x.idx.searches.Load() != 1 {
		t.Fatal("repeated detail caused an extra search")
	}
}

func TestResumedDetailRefillDoesNotWaitOrChangeSource(t *testing.T) {
	x := newEtheLab(t, true, "offline-1", 3)
	x.idx.started = make(chan struct{}, 1)
	x.idx.release = make(chan struct{})
	x.detail(t, "first")
	x.waitWarm(t) // source open/probe/prefetch finishes while search stays blocked
	done := x.scheduledRefill(t)
	select {
	case <-x.idx.started:
	case <-time.After(5 * time.Second):
		t.Fatal("search never started")
	}
	select {
	case <-done:
		t.Fatal("warm completion canceled independent search")
	default:
	}
	if len(x.detail(t, "search-blocked")) != 1 {
		t.Fatal("blocked search unexpectedly filled cache")
	}
	// Another client of the same viewer is independently admitted, but joins the
	// episode search. Its canceled request must not cancel either background path.
	caps := x.caps
	caps.ClientID = "client-2"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !x.mgr.Prewarm(ctx, &ownerUser, x.title.Key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail}) {
		t.Fatal("second client warm refused")
	}
	x.waitWarm(t)
	if duplicate := x.scheduledRefill(t); duplicate != done {
		t.Fatal("clients did not share refill")
	}
	// Resolver owns refill, so even manager closure must not strand or cancel it.
	x.mgr.Close()
	select {
	case <-done:
		t.Fatal("manager/request completion canceled resolver refill")
	default:
	}
	close(x.idx.release)
	waitResumeRefill(t, done)
	media := x.detail(t, "search-completed")
	if len(media) != 4 || x.idx.searches.Load() != 1 || x.idx.fetches.Load() != 0 {
		t.Fatalf("background refill media=%d searches=%d fetches=%d", len(media), x.idx.searches.Load(), x.idx.fetches.Load())
	}
	rp, err := x.mgr.ResumePoint(context.Background(), ownerUser.ID, x.title.Key)
	if err != nil || rp == nil || rp.Release.ID != "offline-1" {
		t.Fatal("search changed remembered release")
	}
	if len(x.mgr.Sessions()) != 0 {
		t.Fatal("background search opened a playback session")
	}
}

func TestResumedDetailAlternativeLimitsAfterRefill(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 2, 3} {
		name := fmt.Sprint(n)
		if n < 0 {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			limit := n
			if n < 0 {
				limit = config.Defaults(t.TempDir()).Resolver.SameQualityAlternatives
			}
			x := newEtheLab(t, true, "offline-4", limit)
			x.detail(t, "first")
			x.waitWarm(t)
			waitResumeRefill(t, x.scheduledRefill(t))
			media := x.detail(t, "refilled")
			if len(media) != 1+limit {
				t.Fatalf("setting=%d Media=%d want %d", limit, len(media), 1+limit)
			}
			// ETHE-18 explicitly preserves Auto's fallback, not an extra row when
			// the remembered release lies outside this bounded picker.
			if limit > 0 && media[0]["selected"] != true {
				t.Fatal("outside-picker resume must select Auto when a picker exists")
			}
		})
	}
}

func TestResumedDetailRefillFailureKeepsMedia(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"empty", nil}, {"error", errors.New("offline indexer unavailable")}, {"budget", core.ErrBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newEtheLab(t, true, "offline-1", 3)
			x.idx.failure = tc.err
			x.idx.releases = nil
			x.detail(t, "first")
			x.waitWarm(t)
			waitResumeRefill(t, x.scheduledRefill(t))
			media := x.detail(t, tc.name)
			if len(media) != 1 || media[0]["id"] != float64(x.title.Key) {
				t.Fatal("unsuccessful search must retain the single Auto Media")
			}
			remembered, releaseID, ok := x.mgr.ResumedTracks(t.Context(), &ownerUser, x.title.Key, x.caps)
			if !ok || releaseID != "offline-1" {
				t.Fatal("unsuccessful search changed remembered source")
			}
			parts := media[0]["Part"].([]any)
			// Persisted resume facts intentionally strip viewer-specific sidecars.
			if len(parts) != 1 || len(parts[0].(map[string]any)["Stream"].([]any)) != len(remembered.Streams) {
				t.Fatal("unsuccessful search lost remembered tracks")
			}
			if x.idx.searches.Load() != 1 || x.idx.fetches.Load() != 0 {
				t.Fatal("unexpected retry or descriptor work")
			}
		})
	}
}
