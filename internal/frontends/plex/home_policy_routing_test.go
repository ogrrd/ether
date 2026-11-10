package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type policyRoutingRecommender struct {
	fakeRecommender
	legacy, policy int
	items          []core.Title
}

func (r *policyRoutingRecommender) HomeCandidatesFor(context.Context, *core.User, core.HubContext, int) ([]core.Hub, error) {
	r.legacy++
	return []core.Hub{{Items: r.items[:2]}}, nil
}
func (r *policyRoutingRecommender) SectionCandidatesFor(ctx context.Context, u *core.User, _ core.MediaType, hc core.HubContext, n int) ([]core.Hub, error) {
	return r.HomeCandidatesFor(ctx, u, hc, n)
}
func (r *policyRoutingRecommender) HomeCandidatesWithPolicy(ctx context.Context, u *core.User, _ core.HubContext, n int, allow func(context.Context, *core.User, *core.Title) bool) ([]core.Hub, error) {
	r.policy++
	var items []core.Title
	for _, item := range r.items {
		if allow(ctx, u, &item) {
			items = append(items, item)
		}
		if len(items) == n {
			break
		}
	}
	return []core.Hub{{Items: items}}, nil
}
func (r *policyRoutingRecommender) SectionCandidatesWithPolicy(ctx context.Context, u *core.User, _ core.MediaType, hc core.HubContext, n int, allow func(context.Context, *core.User, *core.Title) bool) ([]core.Hub, error) {
	return r.HomeCandidatesWithPolicy(ctx, u, hc, n, allow)
}

func TestRestrictedHomeAndSectionUsePolicyBeforeLimit(t *testing.T) {
	ctx := context.Background()
	r := &policyRoutingRecommender{items: []core.Title{{Key: core.MovieKey(1), Adult: true}, {Key: core.MovieKey(2), ContentRating: "PG"}, {Key: core.MovieKey(3), ContentRating: "PG"}}}
	s := testServer(Deps{Recommender: r})
	for _, user := range []*core.User{{ID: 1, MaxContentRating: "PG"}, {ID: 2}} {
		if user.ID == 2 {
			s.setTestPolicy(unknownPolicy{})
		}
		for _, section := range []bool{false, true} {
			var got []core.Hub
			var err error
			if section {
				got, err = s.recommendSection(ctx, &Request{User: user}, core.TypeMovie, core.HubContext{}, 2)
			} else {
				got, err = s.recommendHome(ctx, &Request{User: user}, core.HubContext{}, 2)
			}
			if err != nil || len(got) != 1 || len(got[0].Items) != 2 || got[0].Items[0].Key != core.MovieKey(2) || got[0].Items[1].Key != core.MovieKey(3) {
				t.Fatalf("restricted section=%v: %+v %v", section, got, err)
			}
		}
	}
	if r.policy != 4 || r.legacy != 0 {
		t.Fatalf("wrong restricted routing: %+v", r)
	}
	for _, u := range []*core.User{{ID: 3, IsOwner: true}, {ID: 4}} {
		if !u.IsOwner {
			s.setTestPolicy(nil)
		}
		if _, err := s.recommendHome(ctx, &Request{User: u}, core.HubContext{}, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.recommendSection(ctx, &Request{User: u}, core.TypeMovie, core.HubContext{}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if r.legacy != 4 || r.policy != 4 {
		t.Fatalf("unrestricted routing changed: %+v", r)
	}
}

func TestRestrictedLegacyRecommenderFailsClosed(t *testing.T) {
	ctx := context.Background()
	episode := core.Title{Key: core.EpisodeKey(42, 1, 1), ShowKey: core.ShowKey(42), Type: core.TypeEpisode}
	unavailable := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}
	visible := core.Title{Key: core.MovieKey(5), Type: core.TypeMovie}
	items := []core.Title{episode, unavailable, visible}
	r := &fakeRecommender{home: []core.Hub{{ID: "home.test", Items: items}}, section: map[core.MediaType][]core.Hub{core.TypeMovie: {{ID: "movie.test", Items: items}}}}
	s := testServer(Deps{Recommender: r, FilterAvailable: func(_ context.Context, titles []core.Title) []core.Title {
		out := make([]core.Title, 0, len(titles))
		for _, item := range titles {
			if item.Key != unavailable.Key {
				out = append(out, item)
			}
		}
		return out
	}})
	s.setTestPolicy(unknownPolicy{})
	for _, section := range []bool{false, true} {
		var rows []core.Hub
		var err error
		if section {
			rows, err = s.recommendSection(ctx, &Request{User: &core.User{ID: 1}}, core.TypeMovie, core.HubContext{}, 3)
		} else {
			rows, err = s.recommendHome(ctx, &Request{User: &core.User{ID: 1}}, core.HubContext{}, 3)
		}
		if err != nil || len(rows) != 1 || len(rows[0].Items) != 1 || rows[0].Items[0].Key != visible.Key {
			t.Fatalf("custom source leaked item: %+v %v", rows, err)
		}
	}
	rows, err := s.recommendHome(ctx, &Request{User: &core.User{ID: 2, IsOwner: true}}, core.HubContext{}, 3)
	if err != nil || len(rows) != 1 || len(rows[0].Items) != 3 || rows[0].Items[0].Key != episode.Key {
		t.Fatalf("owner source changed: %+v %v", rows, err)
	}
	if len(r.home[0].Items) != 3 {
		t.Fatal("fallback mutated source hub")
	}
}

// Exercise the actual Plex Home handler, including candidate filtering before
// rendering and deduplication of producer IDs, rather than only the predicate.
func TestRestrictedPlexHomeRouteFiltersCandidatesAndDuplicateRows(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	blockedShow := breakingBad
	blockedShow.Adult = true
	f.catalog.titles[blockedShow.Key] = blockedShow
	blockedTitle := alien
	blockedTitle.Adult = true
	f.catalog.titles[blockedTitle.Key] = blockedTitle
	candidates := []core.Title{bbE1, blockedTitle, fightClub}
	f.srv.d.Recommender = &fakeRecommender{home: []core.Hub{{ID: "home.policy", Title: "Visible", Promoted: true, Items: candidates}, {ID: "home.policy", Title: "Duplicate", Promoted: true, Items: candidates}}}
	f.srv.testAuth().users["restricted-home"] = core.User{ID: 88, AllowedLibraries: []core.LibraryID{1, 2}}
	rec := f.request(t, http.MethodGet, "/hubs?excludeContinueWatching=1", map[string]string{"Accept": "application/json", "X-Plex-Token": "restricted-home"})
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var response struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range response.MediaContainer.Hub {
		if row.HubIdentifier != "home.policy" {
			continue
		}
		count++
		if len(row.Metadata) != 1 || row.Metadata[0].RatingKey != fightClub.Key.String() {
			t.Fatalf("restricted row leaked or lost candidates: %+v", row.Metadata)
		}
	}
	if count != 1 {
		t.Fatalf("duplicate row count=%d response=%s", count, rec.Body.String())
	}
}
