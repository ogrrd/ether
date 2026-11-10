package plex

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	watchstate "github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestDeepShelfPagingAndRecentlyWatchedAgreement(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	st := watchstate.New(db, watchstate.Options{Now: func() time.Time { return fixedNow }})
	defer st.Close(ctx)
	for i := 0; i < 240; i++ {
		title := core.Title{Key: core.MovieKey(int64(80000 + i)), Type: core.TypeMovie, Title: fmt.Sprint("Shelf ", i), Released: fixedNow.Add(-time.Duration(i+1) * time.Hour)}
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
		count, offset := 0, time.Minute.Nanoseconds()
		if i == 0 {
			count = 1
			offset = 0
		}
		if i == 1 {
			count = 1
		}
		if _, err := db.Exec(`INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count,last_viewed_at,updated_at) VALUES(?,?,?,?,?,?)`, ownerUser.ID, int64(title.Key), offset, count, fixedNow.Add(-time.Duration(i)*time.Second).UnixNano(), fixedNow.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	show := core.ShowKey(80000)
	ep := core.EpisodeKey(80000, 1, 1)
	for _, title := range []core.Title{{Key: show, Type: core.TypeShow, Title: "TV", Released: fixedNow.Add(-time.Hour)}, {Key: ep, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Title: "TV episode", Released: fixedNow.Add(-time.Hour)}} {
		if err := c.UpsertTitle(ctx, title, title.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO state_watch(user_id,rating_key,view_offset_ns,last_viewed_at) VALUES(?,?,?,?)`, ownerUser.ID, int64(ep), time.Minute.Nanoseconds(), fixedNow.UnixNano()); err != nil {
		t.Fatal(err)
	}
	next := core.Title{Key: core.EpisodeKey(80000, 1, 2), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 2, Title: "Next", Released: fixedNow.Add(-time.Hour)}
	if err := c.UpsertTitle(ctx, next, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE state_watch SET view_offset_ns=0,view_count=1 WHERE user_id=? AND rating_key=?`, ownerUser.ID, int64(ep)); err != nil {
		t.Fatal(err)
	}
	f := newFixture(func(d *Deps) { d.Catalog = c; d.State = st })
	defer f.srv.Close()
	// On Deck is bounded at PMS's OnDeckLimit (40): it pages within the
	// bounded deck and reports the bounded total.
	for _, tc := range []struct {
		path               string
		start, total, size int
	}{{"/library/recentlyAdded", 220, 242, 7}, {"/library/onDeck", 30, 40, 7}, {"/library/onDeck", 36, 40, 4}, {"/library/onDeck", 220, 40, 0}} {
		mc := decodeJSON(t, f.get(t, tc.path, "Accept", "application/json", "X-Plex-Container-Start", fmt.Sprint(tc.start), "X-Plex-Container-Size", "7"))
		if mc["totalSize"] != float64(tc.total) || mc["offset"] != float64(tc.start) || mc["size"] != float64(tc.size) {
			t.Fatalf("%s at %d: %v", tc.path, tc.start, mc)
		}
	}
	auth := f.srv.testAuth()
	auth.users["movie-viewer"] = core.User{ID: ownerUser.ID, AllowedLibraries: []core.LibraryID{1}}
	auth.users["tv-viewer"] = core.User{ID: ownerUser.ID, AllowedLibraries: []core.LibraryID{2}}
	for _, tc := range []struct {
		token string
		want  int
	}{{"movie-viewer", 40}, {"tv-viewer", 1}} {
		mc := decodeJSON(t, f.get(t, "/library/onDeck", "Accept", "application/json", "X-Plex-Token", tc.token, "X-Plex-Container-Size", "500"))
		if mc["totalSize"] != float64(tc.want) || mc["size"] != float64(tc.want) {
			t.Fatalf("section policy %s: %v", tc.token, mc)
		}
	}
	mc := decodeJSON(t, f.get(t, "/hubs/items/movie.recentlyviewed.1", jsonAccept...))
	rows, _ := mc["Metadata"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["ratingKey"] != core.MovieKey(80000).String() {
		t.Fatal("rewatch/pristine included", mc)
	}
	deck := decodeJSON(t, f.get(t, "/library/onDeck", "Accept", "application/json", "X-Plex-Token", "tv-viewer"))
	deckRows := deck["Metadata"].([]any)
	if deckRows[0].(map[string]any)["lastViewedAt"] != float64(fixedNow.Unix()) {
		t.Fatal("next-up lost anchor clock", deck)
	}
	if err := st.Scrobble(ctx, ownerUser.ID, ep); err != nil {
		t.Fatal(err)
	}
	merged := decodeJSON(t, f.get(t, "/hubs/continueWatching/items", jsonAccept...))
	found := false
	for _, row := range merged["Metadata"].([]any) {
		item := row.(map[string]any)
		if item["ratingKey"] == next.Key.String() {
			found = true
			if item["includedAt"] != float64(fixedNow.Unix()) {
				t.Fatal("merged next-up lost inclusion clock", item)
			}
		}
	}
	if !found {
		t.Fatal("merged next-up absent", merged)
	}
	ctx = withRequest(ctx, &Request{User: &ownerUser})
	hs, err := f.srv.recentlyWatchedHub(ctx, &Request{User: &ownerUser}, Section{ID: 1, Type: core.TypeMovie}, 10)
	if err != nil {
		t.Fatal(err)
	}
	listing := decodeJSON(t, f.get(t, hs.key, jsonAccept...))
	keyRows, _ := listing["Metadata"].([]any)
	if len(hs.Items) != 1 || len(keyRows) != 1 || keyRows[0].(map[string]any)["ratingKey"] != hs.Items[0].Key.String() {
		t.Fatalf("shelf/key mismatch %+v %v", hs, listing)
	}
}

func TestPendingResumeFirstPageDoesNotWaitForSQLiteWriter(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertTitle(ctx, fightClub, true); err != nil {
		t.Fatal(err)
	}
	st := watchstate.New(db, watchstate.Options{PendingSQLite: true, Now: func() time.Time { return fixedNow }})
	defer st.Close(ctx)
	f := newFixture(func(d *Deps) { d.Catalog = c; d.State = st })
	defer f.srv.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := st.Progress(ctx, ownerUser.ID, fightClub.Key, 10*time.Minute, time.Hour, "playing"); err != nil {
		t.Fatal(err)
	}
	if !st.HasPendingWatch(ownerUser.ID) {
		t.Fatal("expected pending write")
	}
	mc := decodeJSON(t, f.get(t, "/library/onDeck", jsonAccept...))
	rows, _ := mc["Metadata"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["ratingKey"] != fightClub.Key.String() {
		t.Fatal("pending resume lost", mc)
	}
}
