package plex

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/store"
)

var _ core.ArtworkAccessPolicy = (*safety.Service)(nil)

type unknownPolicy struct{}
func (unknownPolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (unknownPolicy) Allowed(*core.User, *core.Title) bool { return true }

func TestPlexOwnerAndRestrictedSearchParity(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	policy, err := safety.New(context.Background(), store.OpenTest(t))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.setTestPolicy(policy)
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	owner := &core.User{ID: 1, IsOwner: true}
	restricted := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{1}}
	for _, tc := range []struct {
		name string
		user *core.User
		want int
	}{{"owner", owner, 1}, {"restricted", restricted, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.srv.runSearch(context.Background(), &Request{User: tc.user}, fightClub.Title, 0, 10)
			if err != nil || len(got) != tc.want {
				t.Fatalf("search = %+v, %v; want %d", got, err, tc.want)
			}
		})
	}
	// A custom policy lacking the restriction interface must not disable
	// filtering for other viewers, but the owner retains the legacy path.
	f.srv.setTestPolicy(unknownPolicy{})
	if f.srv.viewerRestricted(owner) || !f.srv.viewerRestricted(&core.User{ID: 3}) {
		t.Fatal("custom policy classification")
	}
}

func TestPlexRecentOwnerKeepsUnrestrictedCandidates(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	sec, ok := f.srv.sectionFor(core.TypeMovie)
	if !ok {
		t.Fatal("missing movie section")
	}
	owner := &core.User{ID: 1, IsOwner: true}
	restricted := &core.User{ID: 2, AllowedLibraries: []core.LibraryID{core.LibraryID(sec.ID)}}
	got, err := f.srv.recentlyAddedHub(context.Background(), &Request{User: owner}, sec, 1)
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("owner row = %+v, %v", got, err)
	}
	again, err := f.srv.recentlyAddedHub(context.Background(), &Request{User: owner}, sec, 1)
	if err != nil || len(again.Items) != 1 || again.Items[0].Key != got.Items[0].Key || again.More != got.More {
		t.Fatalf("owner row changed: %+v then %+v, %v", got, again, err)
	}
	hidden, err := f.srv.recentlyAddedHub(context.Background(), &Request{User: restricted}, sec, 1)
	if err != nil || len(hidden.Items) != 0 {
		t.Fatalf("restricted row = %+v, %v", hidden, err)
	}
}
