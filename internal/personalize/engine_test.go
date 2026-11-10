package personalize

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

func hubIDsOf(hubs []core.Hub) []string {
	var out []string
	for _, h := range hubs {
		out = append(out, h.ID)
	}
	return out
}

func TestEngineServesGenericUntilBuilt(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Workers = 1 })
	ctx := context.Background()
	// New queues the generic model at startup; wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.e.mu.Lock()
		g := h.e.models[genericID]
		h.e.mu.Unlock()
		if g != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("generic model never built")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sciFiFan(h, 1)

	u := &core.User{ID: 1, Username: "alice"}
	start := time.Now()
	hubs, err := h.e.HomeHubsFor(ctx, u, hubContext(testNow, core.DeviceTV), 12)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("first request waited %v; it must be served from the generic model", time.Since(start))
	}
	for _, id := range hubIDsOf(hubs) {
		if id == "home.top10" {
			t.Fatal("personal row before the viewer's model exists")
		}
	}
	// The request queued the viewer's build; once done, Home is personal.
	h.e.mu.Lock()
	f := h.e.flights[1]
	h.e.mu.Unlock()
	if f != nil {
		<-f.done
	}
	hubs, _ = h.e.HomeHubsFor(ctx, u, hubContext(testNow, core.DeviceTV), 12)
	found := false
	for _, id := range hubIDsOf(hubs) {
		if id == "home.top10" {
			found = true
		}
	}
	if !found {
		t.Fatalf("personal Home after build = %v", hubIDsOf(hubs))
	}
}

func TestEngineHubsServedFromCache(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	// New queues the generic model independently of the viewer's synchronous
	// rebuild. Finish that startup work before measuring catalogue calls, or
	// its background reads are incorrectly attributed to cached Home requests.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := waitGenericReady(h.e, waitCtx); err != nil {
		t.Fatal(err)
	}
	sciFiFan(h, 1)
	u := core.User{ID: 1}
	h.rebuild(u)
	before := h.cat.callCount("similar") + h.cat.callCount("list") + h.cat.callCount("browse")
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := h.e.HomeHubsFor(ctx, &u, hubContext(testNow, core.DeviceTV), 12); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / 20
	after := h.cat.callCount("similar") + h.cat.callCount("list") + h.cat.callCount("browse")
	if after != before {
		t.Fatalf("serving Home hit the catalogue sources %d times", after-before)
	}
	if per > 50*time.Millisecond {
		t.Fatalf("Home assembly took %v per request", per)
	}

	// A stale model is still served immediately while a rebuild runs.
	h.clock.Set(testNow.Add(4 * time.Hour))
	h.e.mu.Lock()
	old := h.e.models[1]
	h.e.mu.Unlock()
	if _, err := h.e.HomeHubsFor(ctx, &u, hubContext(testNow.Add(4*time.Hour), core.DeviceTV), 12); err != nil {
		t.Fatal(err)
	}
	h.e.mu.Lock()
	f := h.e.flights[1]
	h.e.mu.Unlock()
	if f != nil {
		<-f.done
	}
	h.e.mu.Lock()
	fresh := h.e.models[1]
	h.e.mu.Unlock()
	if fresh == old || !fresh.builtAt.After(old.builtAt) {
		t.Fatal("stale model was not rebuilt in the background")
	}
}

// TestUnguardedHomeSampleCountsStartupBuild shows the cache measurement
// counting catalogue work that was only paused, then shows the same Home
// calls stay at zero once that startup build has been waited out.
func TestUnguardedHomeSampleCountsStartupBuild(t *testing.T) {
	hold := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	h := newHarness(t, func(o *Options) {
		cat := o.Catalog.(*fakeCatalog)
		cat.holdSources = hold
		cat.sourcesHeld = reached
	})
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("startup generic build did not reach a catalogue source")
	}
	paused := catalogueSources(h.cat)
	if paused != 0 {
		t.Fatalf("startup source calls were counted while held: %d", paused)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitGenericReady(h.e, ctx); err != nil {
		t.Fatal(err)
	}
	if got := catalogueSources(h.cat); got <= paused {
		t.Fatalf("paused startup build added no catalogue calls (%d -> %d); an unguarded sample would not have been contaminated", paused, got)
	}
	sciFiFan(h, 1)
	u := core.User{ID: 1}
	h.rebuild(u)
	before := catalogueSources(h.cat)
	for i := 0; i < 20; i++ {
		if _, err := h.e.HomeHubsFor(ctx, &u, hubContext(testNow, core.DeviceTV), 12); err != nil {
			t.Fatal(err)
		}
	}
	if after := catalogueSources(h.cat); after != before {
		t.Fatalf("Home after waiting for startup hit the catalogue sources %d times", after-before)
	}
}

func catalogueSources(c *fakeCatalog) int {
	return c.callCount("similar") + c.callCount("list") + c.callCount("browse")
}

// waitGenericReady blocks until the startup generic model is installed and
// its build flight is gone. The bound is ctx; it does not poll.
func waitGenericReady(e *Engine, ctx context.Context) error {
	e.mu.Lock()
	f := e.flights[genericID]
	ready := e.models[genericID] != nil && f == nil
	e.mu.Unlock()
	if ready {
		return nil
	}
	if f == nil {
		return errors.New("generic startup build is not running and no model is installed")
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return fmt.Errorf("generic startup build did not finish: %w", ctx.Err())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.models[genericID] == nil {
		return errors.New("generic startup build finished without a model")
	}
	return nil
}

func TestEngineRebuildsAfterStateChanges(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RebuildDebounce = 20 * time.Millisecond })
	ctx := context.Background()
	u := core.User{ID: 1}
	m1 := h.rebuild(u)
	if m1.taste.Confidence != 0 {
		t.Fatal("new viewer has taste")
	}
	sciFiFan(h, 1) // completions → stopped events → debounced rebuild
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.e.mu.Lock()
		m := h.e.models[1]
		h.e.mu.Unlock()
		if m != m1 && m.taste.Confidence > 0.3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("model not rebuilt after watch-state changes")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Timeline ticks while playing do not trigger rebuilds.
	h.e.mu.Lock()
	pending := len(h.e.debounce)
	h.e.mu.Unlock()
	h.e.handleEvent(ctx, state.Event{Kind: state.EventProgress, UserID: 1, Key: movieKey(1, 1), Fraction: 0.3, State: "playing", At: testNow})
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	if len(h.e.debounce) != pending {
		t.Fatal("a playing tick scheduled a rebuild")
	}
}

func TestPreviewMatchesHubs(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sciFiFan(h, 1)
	u := core.User{ID: 1}
	h.rebuild(u)
	for _, hc := range []core.HubContext{hubContext(testNow, core.DeviceTV), hubContext(testNow.Add(-12*time.Hour), core.DeviceMobile)} {
		p, err := h.e.PreviewHome(ctx, &u, hc, "", 12)
		if err != nil {
			t.Fatal(err)
		}
		hubs, _ := h.e.HomeHubsFor(ctx, &u, hc, 12)
		var fromPreview []core.Hub
		for _, r := range p.Rows {
			if r.Hidden || r.HubID == "home.playlists" {
				continue
			}
			fromPreview = append(fromPreview, r.Hub)
		}
		if len(fromPreview) != len(hubs) {
			t.Fatalf("preview rows %v vs hubs %v", hubIDsOf(fromPreview), hubIDsOf(hubs))
		}
		for i := range hubs {
			if hubs[i].ID != fromPreview[i].ID {
				t.Fatalf("row %d: preview %s vs hubs %s", i, fromPreview[i].ID, hubs[i].ID)
			}
			if hubs[i].ID == "home.continue" {
				continue
			}
			if len(hubs[i].Items) != len(fromPreview[i].Items) {
				t.Fatalf("row %s item counts differ", hubs[i].ID)
			}
			for j := range hubs[i].Items {
				if hubs[i].Items[j].Key != fromPreview[i].Items[j].Key {
					t.Fatalf("row %s item %d differs", hubs[i].ID, j)
				}
			}
		}
		if p.Context.Device != hc.Device {
			t.Fatalf("preview context = %+v", p.Context)
		}
		for _, r := range p.Rows {
			if r.Reason == "" || r.Source == "" || r.Kind == "" {
				t.Fatalf("row without explanation: %+v", r)
			}
			if r.Source == core.RowSourceEngine && len(r.Items) > 0 && len(r.ItemReasons) == 0 {
				t.Fatalf("row %s has no item reasons", r.HubID)
			}
		}
	}
	// A section opens with the top picks, then the front end's Continue
	// Watching when populated. Empty utility rows do not displace discovery.
	sec := hubContext(testNow, core.DeviceTV)
	sec.SectionID = 2
	p, _ := h.e.PreviewHome(ctx, &u, sec, "shows", 12)
	if len(p.Rows) < 3 || p.Rows[0].Kind != KindTrending || (p.Rows[1].Kind != KindTop10 && p.Rows[1].Kind != KindTop10Today) {
		t.Fatalf("section preview head = %v", kindsOf(p.Rows))
	}
	for i, r := range p.Rows {
		if r.HubID == "tv.recentlyadded.2" && !r.Hidden && i < recentlyAddedMinSlot-1 {
			t.Fatalf("recently added at row %d: %v", i+1, kindsOf(p.Rows))
		}
	}
	hubs, _ := h.e.SectionHubsFor(ctx, &u, core.TypeShow, sec, 12)
	if len(hubs) != len(p.Rows)-2 {
		t.Fatalf("section hubs %v vs preview %d rows", hubIDsOf(hubs), len(p.Rows))
	}
	for _, hb := range hubs {
		for _, it := range hb.Items {
			if it.Type == core.TypeMovie {
				t.Fatalf("movie %s in TV section row %s", it.Title, hb.ID)
			}
		}
	}
}

func TestOverridesAndLayoutPersist(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sciFiFan(h, 1)
	u := core.User{ID: 1}
	h.rebuild(u)
	_, err := h.e.SetTasteOverrides(ctx, &u, []core.TasteOverride{{Kind: "mood", Value: "x", Weight: 3}})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("invalid overrides: %v", err)
	}
	p, err := h.e.SetTasteOverrides(ctx, &u, []core.TasteOverride{{Kind: "genre", Value: "Science Fiction", Weight: -1}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Genres[0].Name != "Science Fiction" || p.Genres[0].Weight != -1 || len(p.Overrides) != 1 {
		t.Fatalf("profile after override = %+v", p.Genres)
	}
	hubs, _ := h.e.HomeHubsFor(ctx, &u, hubContext(testNow, core.DeviceTV), 12)
	for _, hb := range hubs {
		if hb.ID == "home.top10" {
			for _, it := range hb.Items {
				if overlap(normGenres(it.Genres), []string{"Science Fiction"}) {
					t.Fatalf("override ignored: %s in Top 10", it.Title)
				}
			}
		}
	}

	saved, err := h.e.SetHomeLayout(ctx, core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: KindTrending, Status: "off"}}}}})
	if err != nil || len(saved.Global) != 1 || saved.UpdatedAt.IsZero() {
		t.Fatalf("set layout = %+v %v", saved, err)
	}
	// A fresh engine over the same database loads it.
	e2, err := New(Options{DB: h.db, Catalog: h.cat, Logger: quietLogger(), Now: h.clock.Now, Location: time.UTC, RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	got, _ := e2.HomeLayout(ctx)
	if len(got.Global) != 1 || got.Global[0].Rows[0].Row != KindTrending || got.Global[0].Rows[0].Status != "off" {
		t.Fatalf("persisted layout = %+v", got)
	}
	os, _ := e2.store.overrides(ctx, 1)
	if len(os) != 1 {
		t.Fatalf("persisted overrides = %+v", os)
	}
	avail, _ := h.e.AvailableRows(ctx)
	if len(avail) < len(kinds) || avail[0].HubID != KindContinue {
		t.Fatalf("available rows = %+v", avail[:3])
	}
}

func TestInsightsAndActivity(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.users.users = []core.User{{ID: 1, Username: "alice"}, {ID: 2, Username: "bob"}}
	u := core.User{ID: 1}
	film := h.cat.titles[movieKey(6, 2)]
	h.e.RecordImpressions(&u, hubContext(testNow.Add(-time.Hour), core.DeviceTV), []core.Hub{{ID: "home.hiddengems", Title: "Hidden Gems for You", Items: []core.Title{film}}, {ID: "home.trending", Title: "Trending", Items: []core.Title{h.cat.titles[movieKey(1, 1)]}}})
	h.finish(1, film.Key, testNow.Add(-50*time.Minute))
	h.finish(2, movieKey(2, 2), testNow.AddDate(0, 0, -3))
	h.e.syncEvents(ctx)
	h.clock.Set(testNow.Add(2 * time.Hour))

	ins, err := h.e.RowInsights(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]core.RowInsight{}
	for _, r := range ins {
		byKind[r.HubID] = r
	}
	g := byKind[KindHiddenGems]
	if g.Impressions7d != 1 || g.Plays7d != 1 || g.PlayRate != 1 || g.Title != "Hidden Gems for You" {
		t.Fatalf("hidden gems insight = %+v", g)
	}
	if tr := byKind[KindTrending]; tr.Impressions7d != 1 || tr.Plays7d != 0 {
		t.Fatalf("trending insight = %+v", tr)
	}
	if ins[0].HubID != KindHiddenGems {
		t.Fatalf("insights order = %+v", ins)
	}

	days, err := h.e.Activity(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 30 || days[29].Date != "2026-10-14" {
		t.Fatalf("activity days = %d, last %s", len(days), days[len(days)-1].Date)
	}
	if days[29].Plays != 1 || days[29].UniqueViewers != 1 || days[29].Hours < 1.1 {
		t.Fatalf("today = %+v", days[29])
	}
	if days[26].Plays != 1 {
		t.Fatalf("three days ago = %+v", days[26])
	}

	stats, err := h.e.ViewerStats(ctx, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if stats[1].Plays30d != 1 || stats[1].HoursWatched30d < 1.1 || stats[1].LastActive.IsZero() || stats[3].Plays30d != 0 {
		t.Fatalf("viewer stats = %+v", stats)
	}
	recent, err := h.e.RecentActivity(ctx, 1, 10)
	if err != nil || len(recent) != 1 || !recent[0].Completed || recent[0].Progress != 1 || recent[0].Title.Key != film.Key {
		t.Fatalf("recent = %+v %v", recent, err)
	}
	partial := movieKey(3, 2)
	if _, err := h.st.StoreTimeline(ctx, 1, partial, 20*time.Minute, time.Hour, "playing", "partial", nil); err != nil {
		t.Fatal(err)
	}
	recent, err = h.e.RecentActivity(ctx, 1, 1)
	if err != nil || len(recent) != 1 || recent[0].Title.Key != partial || recent[0].Completed || recent[0].Progress != .333 {
		t.Fatalf("partial viewing must lead bounded activity: %+v %v", recent, err)
	}
	history, err := h.st.History(ctx, 1, time.Time{}, 10)
	if err != nil || len(history) != 2 || history[0].Key != film.Key || history[1].Key != film.Key {
		t.Fatalf("owner activity must not change Plex viewing history: %+v %v", history, err)
	}
	h.clock.Set(testNow.Add(3 * time.Hour))
	if _, err := h.st.StoreTimeline(ctx, 1, film.Key, 30*time.Minute, time.Hour, "playing", "rewatch", nil); err != nil {
		t.Fatal(err)
	}
	recent, err = h.e.RecentActivity(ctx, 1, 10)
	if err != nil || len(recent) != 2 || recent[0].Title.Key != film.Key || recent[0].Completed || recent[0].Progress != .5 || recent[1].Title.Key != partial {
		t.Fatalf("a partial rewatch must appear once as in progress: %+v %v", recent, err)
	}
}

func TestScoreTitles(t *testing.T) {
	h := newHarness(t, nil)
	sciFiFan(h, 1)
	u := core.User{ID: 1}
	titles := []core.Title{h.cat.titles[movieKey(0, 12)], h.cat.titles[movieKey(4, 12)]}
	scores, err := h.e.ScoreTitles(context.Background(), &u, titles)
	if err != nil {
		t.Fatal(err)
	}
	if scores[titles[0].Key] <= scores[titles[1].Key] {
		t.Fatalf("a sci-fi fan should score sci-fi higher: %v", scores)
	}
}

// The engine is what the admin API's viewers, layout and insights
// endpoints are wired to.
var _ admin.Personalization = (*Engine)(nil)
