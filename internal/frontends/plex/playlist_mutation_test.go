package plex

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Exercise persisted member identity across duplicates, independent playlists,
// movement, deletion, and a new view of the store, through the HTTP boundary.
func TestPlaylistPersistedMemberIdentity(t *testing.T) {
	db := store.OpenTest(t)
	playlists := state.New(db, state.Options{}).Playlists()
	f := newFixture(func(d *testServerDeps) { d.Playlists = playlists })
	defer f.srv.Close()
	ctx := context.Background()
	p, err := playlists.Create(ctx, ownerUser.ID, "First", []core.RatingKey{fightClub.Key, alien.Key, fightClub.Key})
	if err != nil {
		t.Fatal(err)
	}
	second, err := playlists.Create(ctx, ownerUser.ID, "Second", []core.RatingKey{alien.Key})
	if err != nil {
		t.Fatal(err)
	}
	if second.ItemIDs[0] <= p.ItemIDs[2] {
		t.Fatalf("identities not global: %v %v", p.ItemIDs, second.ItemIDs)
	}
	path := "/playlists/" + p.Key.String()
	rec := f.request(t, http.MethodPut, path+"/items/"+strconv.FormatInt(p.ItemIDs[0], 10)+"/move?after="+strconv.FormatInt(p.ItemIDs[2], 10), nil)
	if rec.Code != 200 {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	persisted, err := state.New(db, state.Options{}).Playlists().Get(ctx, ownerUser.ID, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ItemIDs[2] != p.ItemIDs[0] || persisted.ItemIDs[1] != p.ItemIDs[2] {
		t.Fatalf("identity lost on move: %v", persisted.ItemIDs)
	}
	rec = f.request(t, http.MethodDelete, path+"/items/"+strconv.FormatInt(p.ItemIDs[2], 10), nil)
	if rec.Code != 200 {
		t.Fatalf("delete %d", rec.Code)
	}
	persisted, _ = playlists.Get(ctx, ownerUser.ID, p.Key)
	if len(persisted.Items) != 2 || persisted.ItemIDs[1] != p.ItemIDs[0] {
		t.Fatalf("wrong occurrence deleted: %+v", persisted)
	}
	rec = f.request(t, http.MethodPut, path+"?title=&summary.value=Saved", nil)
	if rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	persisted, _ = playlists.Get(ctx, ownerUser.ID, p.Key)
	if persisted.Title != "" || persisted.Summary != "Saved" {
		t.Fatalf("edit lost: %+v", persisted)
	}
	source := "server://" + f.srv.identity().MachineIdentifier + "/com.plexapp.plugins.library/library/metadata/" + alien.Key.String() + "," + alien.Key.String()
	rec = f.request(t, http.MethodPut, path+"/items?uri="+url.QueryEscape(source), map[string]string{"Accept": "application/json"})
	mc := decodeJSON(t, rec)
	if mc["leafCountAdded"] != float64(0) || mc["leafCountRequested"] != float64(2) {
		t.Fatalf("dedup counts: %v", mc)
	}
	rec = f.request(t, http.MethodDelete, path+"/items", nil)
	if rec.Code != 200 {
		t.Fatalf("clear: %d", rec.Code)
	}
	persisted, _ = playlists.Get(ctx, ownerUser.ID, p.Key)
	if len(persisted.Items) != 0 {
		t.Fatalf("clear: %+v", persisted)
	}
}
