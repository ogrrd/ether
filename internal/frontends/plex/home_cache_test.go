package plex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// buildCountingRecommender titles its first row with the build number, so a
// response shows which build produced it.
type buildCountingRecommender struct {
	*fakeRecommender
	n atomic.Int64
}

func (c *buildCountingRecommender) HomeHubs(ctx context.Context, u *core.User, n int) ([]core.Hub, error) {
	hubs := append([]core.Hub(nil), c.fakeRecommender.home...)
	hubs[0].Title = fmt.Sprintf("Trending build %d", c.n.Add(1))
	return hubs, nil
}

// TestHomeCacheRevisionContract pins what may be served from a viewer's
// cached Home: another client of the same kind is served the stored screen;
// a viewer revision change (watch state, playlists, exclusions) is rebuilt
// on the request; the shared policy epoch only refreshes behind the response
// for an unrestricted viewer, but is rebuilt on the request for a restricted
// one.
func TestHomeCacheRevisionContract(t *testing.T) {
	var mu sync.Mutex
	revs := map[int64]experience.HomeRevision{}
	setRev := func(id int64, fn func(*experience.HomeRevision)) {
		mu.Lock()
		r := revs[id]
		fn(&r)
		revs[id] = r
		mu.Unlock()
	}
	restricted := core.User{ID: 3, PlexID: 3003, Username: "kid", Title: "Kid", MaxContentRating: "NC-17"}
	var rec *buildCountingRecommender
	f := newFixture(func(d *Deps) {
		rec = &buildCountingRecommender{fakeRecommender: d.Recommender.(*fakeRecommender)}
		d.Recommender = rec
		setDepsAuth(d, &fakeAuth{users: map[string]core.User{ownerToken: ownerUser, friendToken: friendUser, "kid-token": restricted}})
		d.HomeCache = &HomeCacheSource{Revision: func(_ context.Context, u *core.User) (experience.HomeRevision, error) {
			mu.Lock()
			defer mu.Unlock()
			return revs[u.ID], nil
		}}
	})
	defer f.srv.Close()
	home := func(token, client string) string {
		t.Helper()
		r := f.get(t, "/hubs/promoted?count=12", "Accept", "application/json", "X-Plex-Token", token, "X-Plex-Client-Identifier", client, "X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS")
		if r.Code != 200 {
			t.Fatalf("status %d", r.Code)
		}
		body := r.Body.String()
		if !strings.Contains(body, "Trending build ") {
			t.Fatalf("no recommender row: %s", body)
		}
		return body
	}
	first := home(ownerToken, "living-room")
	if again := home(ownerToken, "bedroom"); again != first {
		t.Fatal("a second client of the same kind rebuilt the viewer's Home")
	}
	setRev(ownerUser.ID, func(r *experience.HomeRevision) { r.Policy++ })
	if soft := home(ownerToken, "bedroom"); soft != first {
		t.Fatal("an unrestricted viewer waited for a rebuild on a shared policy change")
	}
	setRev(ownerUser.ID, func(r *experience.HomeRevision) { r.Viewer = "watched-more" })
	if hard := home(ownerToken, "bedroom"); hard == first {
		t.Fatal("a viewer revision change was served from the cache")
	}

	kid := home("kid-token", "kid-tv")
	if home("kid-token", "kid-tv") != kid {
		t.Fatal("an unchanged restricted viewer rebuilt")
	}
	setRev(restricted.ID, func(r *experience.HomeRevision) { r.Policy++ })
	if home("kid-token", "kid-tv") == kid {
		t.Fatal("a restricted viewer was served a screen from before a policy change")
	}
}
