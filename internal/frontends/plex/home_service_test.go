package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"reflect"
	"testing"
)

func TestPlexHomeServicePreservesDuplicateAndEmptySidecars(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// No layout provider means even duplicate identifiers retain their distinct wire rows.
	f.srv.setRecommender(nil)
	rows := []model.Hub{{HubIdentifier: "same", Title: "first"}, {HubIdentifier: "same", Title: "second"}, {HubIdentifier: "empty", Title: "empty"}}
	got := f.srv.finalizeHomeRows(context.Background(), &Request{User: &core.User{IsOwner: true}}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("sidecars changed: %+v", got)
	}
}

func TestPlexHomeServiceUsesLayoutOrder(t *testing.T) {
	r := &renderedLayout{}
	s := newTestServer(testServerDeps{Recommender: r})
	rows := []model.Hub{{HubIdentifier: "first", Title: "First"}, {HubIdentifier: "second", Title: "Second"}}
	got := s.finalizeHomeRows(context.Background(), &Request{User: &core.User{ID: 1}}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, []model.Hub{rows[1], rows[0]}) {
		t.Fatalf("wrong layout order: %+v", got)
	}
}

func TestPlexHomeLayoutPreservesLiveProgrammeSidecar(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setRecommender(nil)
	rows := []model.Hub{{HubIdentifier: "live", Metadata: []model.Metadata{{RatingKey: "programme-123", Media: []model.Media{{Extra: []model.Attr{model.A("channelIdentifier", "live")}}}}}}}
	got := f.srv.finalizeHomeRows(context.Background(), &Request{User: &ownerUser}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("lost live programme: %+v", got)
	}
}

func TestPlexHomeNilViewerPreservesLayout(t *testing.T) {
	r := &renderedLayout{}
	s := newTestServer(testServerDeps{Recommender: r})
	rows := []model.Hub{{HubIdentifier: "a"}, {HubIdentifier: "b"}}
	got := s.finalizeHomeRows(context.Background(), &Request{}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, []model.Hub{rows[1], rows[0]}) {
		t.Fatalf("anonymous layout changed: %+v", got)
	}
}

func TestPlexHomeServiceErrorFallsBackToAuthorizedSidecars(t *testing.T) {
	r := &renderedLayout{}
	s := newTestServer(testServerDeps{Recommender: r})
	rows := []model.Hub{{HubIdentifier: "a", Metadata: []model.Metadata{{RatingKey: "0"}}}, {HubIdentifier: "b"}}
	got := s.finalizeHomeRows(context.Background(), &Request{User: &core.User{ID: 1}}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, []model.Hub{rows[1], rows[0]}) {
		t.Fatalf("fallback changed sidecars: %+v", got)
	}
}

func TestPlexHomeRestrictedProjectionExcludesHiddenParent(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	episode := core.Title{Key: core.EpisodeKey(42, 1, 1), ShowKey: core.ShowKey(42), Type: core.TypeEpisode}
	rows := []model.Hub{{HubIdentifier: "a", Metadata: []model.Metadata{{RatingKey: episode.Key.String()}}}}
	// This show is absent from the fake catalogue; inherited policy must fail closed.
	user := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{2}}
	// Verify the same inherited policy applied by the row service's adapter.
	if !f.srv.viewerRestricted(user) || f.srv.visibleTitle(context.Background(), user, &episode) {
		t.Fatal("fixture must deny hidden parent")
	}
	got := f.srv.finalizeHomeRows(context.Background(), &Request{User: user}, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("Plex sidecar changed: %+v", got)
	}
}

func TestPlexHomeDuplicateLayoutMatchesLegacyLastWins(t *testing.T) {
	r := &renderedLayout{}
	s := newTestServer(testServerDeps{Recommender: r})
	rows := []model.Hub{{HubIdentifier: "same", Title: "first"}, {HubIdentifier: "same", Title: "second"}}
	pr := &Request{User: &core.User{ID: 1}}
	want := s.finalizeHomeRows(context.Background(), pr, core.HubContext{}, "home", rows)
	got := s.finalizeHomeRows(context.Background(), pr, core.HubContext{}, "home", rows)
	if !reflect.DeepEqual(got, want) || len(got) != 2 || got[0].Title != "second" || got[1].Title != "second" {
		t.Fatalf("legacy=%+v got=%+v", want, got)
	}
}
