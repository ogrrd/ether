package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// hookRecommender is a fakeRecommender that also implements the three
// optional Netflix-style capabilities, per viewer.
type hookRecommender struct {
	fakeRecommender
	mu      sync.Mutex
	picks   map[int64][]core.PostPlayPick // by user id
	headers map[int64]string              // micro-reason header by user id
	opens   []core.RatingKey
}

func (r *hookRecommender) PostPlay(_ context.Context, u *core.User, _ core.RatingKey, _ core.HubContext, _ int) ([]core.PostPlayPick, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.picks[u.ID], nil
}

func (r *hookRecommender) MicroReason(_ context.Context, u *core.User, _ *core.Title, _ time.Time) core.MicroReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return core.MicroReason{Header: r.headers[u.ID]}
}

func (r *hookRecommender) RecordDetailOpen(_ context.Context, _ *core.User, t *core.Title) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens = append(r.opens, t.Key)
}

func (r *hookRecommender) NotePlayback(*core.User, core.RatingKey) {}

func hookFixture(t *testing.T, on bool) (*fixture, *hookRecommender) {
	t.Helper()
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	rec := &hookRecommender{
		fakeRecommender: fakeRecommender{
			related: []core.Hub{{ID: "movie.similar", Title: "More Like This", Type: core.TypeMovie,
				Context: "hub.movie.similar", Items: []core.Title{alien, breakingBad}}},
		},
		picks: map[int64][]core.PostPlayPick{
			ownerUser.ID:  {{Title: alien, Score: 0.9}},
			friendUser.ID: {{Title: breakingBad, Score: 0.8}},
		},
		headers: map[int64]string{
			ownerUser.ID: "96% Match · Directed by Mara Ellison · Ends at 10:45 PM",
		},
	}
	f.srv.d.Recommender = rec
	base := f.srv.testConfig()
	f.srv.setTestConfig(func() config.Config {
		c := base()
		c.Personalize = config.Personalize{PostPlay: on, PostPlayPrebuffer: on, MicroReasons: on, SessionIntent: on}
		return c
	})
	return f, rec
}

type mcJSON struct {
	MediaContainer struct {
		Hub []struct {
			HubIdentifier string `json:"hubIdentifier"`
			Title         string `json:"title"`
			Metadata      []struct {
				RatingKey string `json:"ratingKey"`
				Title     string `json:"title"`
			} `json:"Metadata"`
		} `json:"Hub"`
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
			Summary   string `json:"summary"`
			Tagline   string `json:"tagline"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func decodeMC(t *testing.T, body []byte) mcJSON {
	t.Helper()
	var mc mcJSON
	if err := json.Unmarshal(body, &mc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	return mc
}

var jsonHdr = []string{"Accept", "application/json"}

// Movie post-play: the hook queue leads (the pre-warmed #1 first), the
// related row follows without repeating queue titles, and each viewer
// gets their own queue.
func TestPostplayHookQueue(t *testing.T) {
	f, _ := hookFixture(t, true)
	rec := f.get(t, "/hubs/metadata/"+fightClub.Key.String()+"/postplay", jsonHdr...)
	if rec.Code != http.StatusOK {
		t.Fatalf("postplay: %d %s", rec.Code, rec.Body)
	}
	mc := decodeMC(t, rec.Body.Bytes())
	if len(mc.MediaContainer.Hub) != 2 {
		t.Fatalf("hubs = %+v", mc.MediaContainer.Hub)
	}
	hook := mc.MediaContainer.Hub[0]
	if hook.HubIdentifier != "movie.postplay" || len(hook.Metadata) == 0 || hook.Metadata[0].RatingKey != alien.Key.String() {
		t.Errorf("first hub = %+v, want the hook queue led by the #1 pick", hook)
	}
	rel := mc.MediaContainer.Hub[1]
	for _, m := range rel.Metadata {
		if m.RatingKey == alien.Key.String() {
			t.Error("related row repeats a hook-queue title")
		}
	}
	// The friend's queue is theirs, not the owner's.
	rec = f.get(t, "/hubs/metadata/"+fightClub.Key.String()+"/postplay", "Accept", "application/json", "X-Plex-Token", friendToken)
	mc = decodeMC(t, rec.Body.Bytes())
	if len(mc.MediaContainer.Hub) == 0 || len(mc.MediaContainer.Hub[0].Metadata) == 0 ||
		mc.MediaContainer.Hub[0].Metadata[0].RatingKey != breakingBad.Key.String() {
		t.Errorf("friend queue = %+v", mc.MediaContainer.Hub)
	}
}

// Episode post-play: Up Next stays first, then the queue.
func TestPostplayEpisodeUpNextFirst(t *testing.T) {
	f, _ := hookFixture(t, true)
	f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
	rec := f.get(t, "/hubs/metadata/"+bbE1.Key.String()+"/postplay", jsonHdr...)
	if rec.Code != http.StatusOK {
		t.Fatalf("postplay: %d %s", rec.Code, rec.Body)
	}
	mc := decodeMC(t, rec.Body.Bytes())
	hubs := mc.MediaContainer.Hub
	if len(hubs) < 2 || hubs[0].HubIdentifier != "tv.upnext" {
		t.Fatalf("hubs = %+v, want Up Next first", hubs)
	}
	if hubs[0].Metadata[0].RatingKey != bbE2.Key.String() {
		t.Errorf("up next = %+v, want the next episode", hubs[0].Metadata)
	}
	if hubs[1].HubIdentifier != "tv.postplay" {
		t.Errorf("second hub = %q, want tv.postplay", hubs[1].HubIdentifier)
	}
}

// With the owner switch off the endpoint is the plain related behaviour.
func TestPostplayOptOut(t *testing.T) {
	f, _ := hookFixture(t, false)
	rec := f.get(t, "/hubs/metadata/"+fightClub.Key.String()+"/postplay", jsonHdr...)
	mc := decodeMC(t, rec.Body.Bytes())
	for _, h := range mc.MediaContainer.Hub {
		if strings.Contains(h.HubIdentifier, "postplay") {
			t.Errorf("hook hub %q served while opted out", h.HubIdentifier)
		}
	}
	if len(mc.MediaContainer.Hub) != 1 || mc.MediaContainer.Hub[0].HubIdentifier != "movie.similar" {
		t.Errorf("hubs = %+v, want related only", mc.MediaContainer.Hub)
	}
}

// The micro-reason header decorates the single-item detail page only,
// per viewer, and never mutates the shared title or leaks across users.
func TestDetailMicroReason(t *testing.T) {
	f, rec := hookFixture(t, true)
	r := f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonHdr...)
	if r.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", r.Code, r.Body)
	}
	mc := decodeMC(t, r.Body.Bytes())
	if len(mc.MediaContainer.Metadata) != 1 {
		t.Fatalf("metadata = %+v", mc.MediaContainer.Metadata)
	}
	md := mc.MediaContainer.Metadata[0]
	// Fjord Harbor has its own tagline, so the header prefixes the summary.
	if md.Tagline != fightClub.Tagline {
		t.Errorf("tagline rewritten: %q", md.Tagline)
	}
	if !strings.HasPrefix(md.Summary, "96% Match · Directed by Mara Ellison · Ends at 10:45 PM\n\n") ||
		!strings.HasSuffix(md.Summary, fightClub.Summary) {
		t.Errorf("summary = %q", md.Summary)
	}
	// The friend (no header) sees the pristine text: no cross-viewer leak
	// through the title cache.
	r2 := f.get(t, "/library/metadata/"+fightClub.Key.String(), "Accept", "application/json", "X-Plex-Token", friendToken)
	md2 := decodeMC(t, r2.Body.Bytes()).MediaContainer.Metadata[0]
	if md2.Summary != fightClub.Summary {
		t.Errorf("friend summary leaked the owner's header: %q", md2.Summary)
	}
	if got := f.catalog.titles[fightClub.Key]; got.Summary != fightClub.Summary || got.Tagline != fightClub.Tagline {
		t.Error("shared catalogue title was mutated")
	}
	// List responses are never decorated.
	r3 := f.get(t, "/hubs/metadata/"+fightClub.Key.String()+"/related", jsonHdr...)
	if strings.Contains(r3.Body.String(), "96% Match") {
		t.Error("hub response carries a micro-reason header")
	}
	// Back-to-back detail calls do not accumulate headers.
	r4 := f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonHdr...)
	if n := strings.Count(r4.Body.String(), "96% Match"); n != 1 {
		t.Errorf("header appears %d times", n)
	}
	// Intent: exactly the single-item opens were recorded (two owner
	// details + one friend detail; the hub call is not an open).
	rec.mu.Lock()
	opens := append([]core.RatingKey(nil), rec.opens...)
	rec.mu.Unlock()
	if len(opens) != 3 {
		t.Errorf("detail opens recorded = %v, want 3", opens)
	}
	for _, k := range opens {
		if k != fightClub.Key {
			t.Errorf("open for %v", k)
		}
	}
}

// A title with no tagline takes the header in the Tagline slot instead of
// the summary prefix.
func TestDetailMicroReasonTaglineSlot(t *testing.T) {
	f, _ := hookFixture(t, true)
	// Alien has no tagline/summary in the fixture.
	r := f.get(t, "/library/metadata/"+alien.Key.String(), jsonHdr...)
	md := decodeMC(t, r.Body.Bytes()).MediaContainer.Metadata[0]
	if md.Tagline != "96% Match · Directed by Mara Ellison · Ends at 10:45 PM" {
		t.Errorf("tagline = %q", md.Tagline)
	}
	if md.Summary != "" {
		t.Errorf("summary duplicated the header: %q", md.Summary)
	}
}

// Opted out: the detail page is untouched and no intent is recorded.
func TestDetailHooksOptOut(t *testing.T) {
	f, rec := hookFixture(t, false)
	r := f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonHdr...)
	md := decodeMC(t, r.Body.Bytes()).MediaContainer.Metadata[0]
	if md.Summary != fightClub.Summary || md.Tagline != fightClub.Tagline {
		t.Errorf("opted-out detail changed: %q / %q", md.Summary, md.Tagline)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.opens) != 0 {
		t.Errorf("intent recorded while opted out: %v", rec.opens)
	}
}

type limitedPostplayRecommender struct {
	fakeRecommender
	picks         []core.PostPlayPick
	items         []core.Title
	pickLimits    []int
	relatedLimits []int
}

func (r *limitedPostplayRecommender) PostPlay(_ context.Context, _ *core.User, _ core.RatingKey, _ core.HubContext, n int) ([]core.PostPlayPick, error) {
	r.pickLimits = append(r.pickLimits, n)
	return r.picks[:min(n, len(r.picks))], nil
}
func (r *limitedPostplayRecommender) Related(_ context.Context, _ *core.User, _ core.RatingKey, n int) ([]core.Hub, error) {
	r.relatedLimits = append(r.relatedLimits, n)
	return []core.Hub{{ID: "movie.similar", Items: r.items[:min(n, len(r.items))]}}, nil
}
func TestPostPlayBackfillsFilteredCandidates(t *testing.T) {
	f, _ := hookFixture(t, true)
	hidden := alien
	hidden.Adult = true
	visible := breakingBad
	r := &limitedPostplayRecommender{picks: []core.PostPlayPick{{Title: hidden}, {Title: visible}}, items: []core.Title{hidden, visible}}
	f.srv.d.Recommender = r
	pr := &Request{User: &ownerUser}
	seen := map[core.RatingKey]bool{}
	selection := f.srv.postPlaySelection(context.Background(), pr, 0, false)
	row := selection.Row(context.Background(), pr.User, 1, seen)
	if len(row.Items) != 1 || row.Items[0].Key != visible.Key || len(r.pickLimits) != 2 || seen[hidden.Key] {
		t.Fatalf("row %+v, limits %v, seen %v", row, r.pickLimits, seen)
	}
	mc := hubContainer()
	seen = map[core.RatingKey]bool{}
	hubs, err := selection.RelatedRows(context.Background(), pr.User, 1, seen)
	if err != nil {
		t.Fatal(err)
	}
	for _, hub := range hubs {
		mc.Hub = append(mc.Hub, f.srv.renderHub(context.Background(), pr, hubSpec{Hub: hub}, hubCtx{}))
	}
	if len(mc.Hub) != 1 || len(mc.Hub[0].Metadata) != 1 || len(r.relatedLimits) != 2 || seen[hidden.Key] || !seen[visible.Key] {
		t.Fatalf("related %+v, limits %v, seen %v", mc.Hub, r.relatedLimits, seen)
	}
}
