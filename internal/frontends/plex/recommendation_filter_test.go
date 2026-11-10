package plex

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type recommendationFilterFake struct {
	fakeRecommender
	denied map[core.RatingKey]bool
	err    error
}

func (f *recommendationFilterFake) FilterRecommendations(_ context.Context, _ *core.User, items []core.Title) ([]core.Title, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []core.Title
	for _, item := range items {
		if !f.denied[item.Key] {
			out = append(out, item)
		}
	}
	return out, nil
}

func TestRecommendationCandidatesBackfillsAndPreservesEmptyParity(t *testing.T) {
	ctx := context.Background()
	f := &recommendationFilterFake{}
	s := newTestServer(testServerDeps{Recommender: f})
	items := []core.Title{{Key: core.MovieKey(1)}, {Key: core.MovieKey(2)}, {Key: core.MovieKey(3)}, {Key: core.MovieKey(4)}}
	var calls []int
	load := func(_ context.Context, n int) ([]core.Title, error) {
		calls = append(calls, n)
		return items[:min(n, len(items))], nil
	}
	got, err := s.recommendationCandidates(ctx, &core.User{ID: 1}, 2, load)
	if err != nil || !reflect.DeepEqual(got, items[:2]) || !reflect.DeepEqual(calls, []int{2}) {
		t.Fatalf("empty-set parity: %+v %v %v", got, calls, err)
	}
	f.denied = map[core.RatingKey]bool{items[0].Key: true}
	calls = nil
	got, err = s.recommendationCandidates(ctx, &core.User{ID: 1}, 2, load)
	if err != nil || !reflect.DeepEqual(got, items[1:3]) || !reflect.DeepEqual(calls, []int{2, 4}) {
		t.Fatalf("backfill: %+v %v %v", got, calls, err)
	}
	f.err = errors.New("unavailable")
	if got, err = s.recommendationCandidates(ctx, &core.User{ID: 1}, 2, load); err == nil || got != nil {
		t.Fatalf("failed open: %+v %v", got, err)
	}
	f.err = nil
	f.denied = nil
	got, err = s.recommendationCandidates(ctx, &core.User{ID: 1}, 2, func(context.Context, int) ([]core.Title, error) { return items, nil })
	if err != nil || !reflect.DeepEqual(got, items) {
		t.Fatalf("overfull legacy source changed: %+v %v", got, err)
	}
}

func TestNotInterestedSimilarAndCinemaTrailers(t *testing.T) {
	f, _, _ := extrasFixture(t)
	filter := &recommendationFilterFake{}
	f.srv.setRecommender(filter)
	path := "/library/metadata/" + fightClub.Key.String() + "/similar"
	before := f.get(t, path, "Accept", "application/json")
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), alien.Key.String()) {
		t.Fatalf("fixture missing similar title: %d %s", before.Code, before.Body.String())
	}
	without := f.srv.cinemaTrailerKeys(context.Background(), &Request{User: &ownerUser}, fightClub.Key, 1)
	if len(without) != 1 || without[0] != alienTrailer.Key {
		t.Fatalf("fixture missing trailer: %+v", without)
	}
	filter.denied = map[core.RatingKey]bool{alien.Key: true}
	after := f.get(t, path, "Accept", "application/json")
	if after.Code != http.StatusOK || strings.Contains(after.Body.String(), alien.Key.String()) {
		t.Fatalf("similar leaked dismissed item: %d %s", after.Code, after.Body.String())
	}
	if got := f.srv.cinemaTrailerKeys(context.Background(), &Request{User: &ownerUser}, fightClub.Key, 1); len(got) != 0 {
		t.Fatalf("cinema trailer leaked dismissed feature: %+v", got)
	}
	filter.denied = nil
	restored := f.get(t, path, "Accept", "application/json")
	if restored.Code != before.Code || restored.Body.String() != before.Body.String() {
		t.Fatal("undo changed legacy Similar response")
	}
}
