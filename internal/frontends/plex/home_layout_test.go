package plex

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type failingNeutralLayout struct{ core.Recommender }

func (f failingNeutralLayout) FinalizeNeutralHome(context.Context, *core.User, core.HomeSituation, string, []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	return nil, errors.New("layout unavailable")
}

func TestNeutralPlexHomeLayoutFailurePreservesRows(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(failingNeutralLayout{})
	rows := []model.Hub{{HubIdentifier: "a", Metadata: []model.Metadata{{Title: "original"}}}}
	got := f.srv.finalizeHomeRows(context.Background(), &Request{User: &ownerUser}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("layout failure changed wire rows: %+v", got)
	}
}

type renderedLayout struct {
	core.Recommender
	core.HomeLayoutSource
	got []core.Hub
}

type neutralRenderedLayout struct {
	*renderedLayout
	got []core.NeutralHomeRow
}

func (r *neutralRenderedLayout) FinalizeNeutralHome(_ context.Context, _ *core.User, _ core.HomeSituation, _ string, rows []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	r.got = rows
	return []core.NeutralHomeRow{rows[1], rows[0]}, nil
}

func TestNeutralPlexHomeLayoutMatchesLegacySidecars(t *testing.T) {
	legacy := &renderedLayout{}
	neutral := &neutralRenderedLayout{renderedLayout: &renderedLayout{}}
	rows := []model.Hub{{HubIdentifier: "a", Size: 1, Metadata: []model.Metadata{{Title: "A"}}}, {HubIdentifier: "b", Size: 2, Metadata: []model.Metadata{{Title: "B"}}}}
	pr := &Request{User: &core.User{ID: 1}}
	old := (newTestServer(testServerDeps{Recommender: legacy})).finalizeHomeRows(context.Background(), pr, core.HubContext{}, "home", rows)
	got := (newTestServer(testServerDeps{Recommender: neutral})).finalizeHomeRows(context.Background(), pr, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, old) || len(neutral.got) != 2 || neutral.got[1].ItemCount != 2 {
		t.Fatalf("legacy=%+v neutral=%+v input=%+v", old, got, neutral.got)
	}
}

func (r *renderedLayout) FinalizeHome(_ context.Context, _ *core.User, _ core.HubContext, _ string, rows []core.Hub) ([]core.Hub, error) {
	r.got = rows
	return []core.Hub{rows[1], rows[0]}, nil
}
func TestFinalLayoutReceivesRenderedAvailability(t *testing.T) {
	r := &renderedLayout{}
	s := newTestServer(testServerDeps{Recommender: r})
	rows := []model.Hub{
		{HubIdentifier: "movie.inprogress.1", Size: 0},
		{HubIdentifier: "movie.top10", Size: 10, Key: "/original/key", Metadata: []model.Metadata{{Title: "Original metadata"}}},
	}
	got := s.finalizeHomeRows(context.Background(), &Request{User: &core.User{ID: 1}}, core.HubContext{}, "movies", rows)
	if r.got[0].ItemCount != 0 || r.got[1].ItemCount != 10 {
		t.Fatalf("availability: %+v", r.got)
	}
	if !reflect.DeepEqual(got, []model.Hub{rows[1], rows[0]}) {
		t.Fatal("layout changed original Plex metadata")
	}
	if !reflect.DeepEqual(stableHomeHubs(got), []model.Hub{rows[1]}) {
		t.Fatal("stable home rows kept an empty row")
	}
}

func TestHomeRowAssemblyStableForSameCandidates(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(&fakeRecommender{home: []core.Hub{
		{ID: "home.top10.today", Title: "Top 10", Type: core.TypeMovie, Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.shortontime", Title: "Short on Time", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}},
	}})
	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		first := hubIDs(t, f.get(t, path, "Accept", "application/json").Body.Bytes())
		second := hubIDs(t, f.get(t, path, "Accept", "application/json").Body.Bytes())
		// An empty Continue Watching row is omitted, same as main's stable home rows.
		want := []string{"home.top10.today", "home.shortontime"}
		if !reflect.DeepEqual(first, want) {
			t.Fatalf("%s: unexpected row order: %v", path, first)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("%s: row order changed: %v != %v", path, first, second)
		}
	}
}

func TestHeroReadyHomeHubsOmitsColdAndArtlessHero(t *testing.T) {
	artless := model.Metadata{Title: "No Art", Thumb: "/library/metadata/1/thumb/1", RatingKey: "1"}
	ready := model.Metadata{Title: "Ready", Thumb: "/library/metadata/2/thumb/1", Art: "/library/metadata/2/art/1", RatingKey: "2"}
	because := model.Hub{HubIdentifier: "home.because", Title: "Because You Watched", Style: "shelf", Size: 1, HubKey: "/library/metadata/2", Metadata: []model.Metadata{ready}}
	short := model.Hub{HubIdentifier: "home.shortontime", Title: "Short on Time", Style: "shelf", Size: 1, Metadata: []model.Metadata{ready}}

	cold := heroReadyHomeHubs(stableHomeHubs([]model.Hub{
		{HubIdentifier: "home.continue", Title: "Continue Watching", Style: "hero", Size: 0},
		{HubIdentifier: "home.trending", Title: "Trending Now", Style: "shelf", Size: 1, Metadata: []model.Metadata{artless}},
		because,
		short,
	}))
	if len(cold) != 2 || cold[0].HubIdentifier != "home.because" || cold[0].Style != "hero" || cold[0].Size != 1 || cold[0].Metadata[0].Art == "" {
		t.Fatalf("cold hero = %+v", cold)
	}
	if cold[1].HubIdentifier != "home.shortontime" || cold[1].Style != "shelf" {
		t.Fatalf("cold shelves = %+v", cold)
	}

	mixed := heroReadyHomeHubs([]model.Hub{{
		HubIdentifier: "home.trending", Title: "Trending Now", Style: "shelf", Size: 2,
		HubKey: "/library/metadata/1,2", Metadata: []model.Metadata{artless, ready},
	}})
	if len(mixed) != 1 || mixed[0].Style != "hero" || mixed[0].Size != 1 || mixed[0].HubKey != "/library/metadata/2" || mixed[0].Metadata[0].RatingKey != "2" {
		t.Fatalf("mixed trending = %+v", mixed)
	}

	readyHero := model.Hub{HubIdentifier: "home.continue", Title: "Continue Watching", Style: "hero", Size: 1, Metadata: []model.Metadata{ready}}
	readyTrend := model.Hub{HubIdentifier: "home.trending", Title: "Trending Now", Style: "shelf", Size: 1, Metadata: []model.Metadata{ready}}
	got := heroReadyHomeHubs([]model.Hub{readyHero, readyTrend})
	if !reflect.DeepEqual(got, []model.Hub{readyHero, readyTrend}) {
		t.Fatalf("ready state changed: %+v", got)
	}

	if rows := heroReadyHomeHubs(stableHomeHubs(nil)); len(rows) != 0 {
		t.Fatalf("empty home = %+v", rows)
	}
}
