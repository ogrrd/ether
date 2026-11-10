package plex

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCreditNameQueryGrammar(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, raw := range []string{"actor=Zo%C3%AB+North,22&actor=Pat+West", "director!=Missing,22"} {
		q, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		br := f.srv.parseBrowse(context.Background(), Section{}, core.TypeMovie, nil, q)
		if len(br.q.Filter.Credits) == 0 {
			t.Fatal("name filter dropped")
		}
		cf := br.q.Filter.Credits[0]
		if len(cf.Names) != 1 || len(cf.People) != 1 {
			t.Fatalf("mixed alternatives: %+v", cf)
		}
		if q.Has("actor") && len(br.q.Filter.Credits) != 2 {
			t.Fatal("repeated constraints must remain separate")
		}
		if q.Has("director!") && (!cf.Exclude || cf.Kind != "director") {
			t.Fatalf("negated director: %+v", cf)
		}
	}
}

func TestCreditNameHTTPPaging(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := catalog.New(store.OpenTest(t), nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"Zoë North", "Pat West", "Zoë North"} {
		title := core.Title{Key: core.MovieKey(int64(i + 1)), Type: core.TypeMovie, IDs: core.IDs{TMDB: int64(i + 1)}, Title: []string{"One", "Two", "Three"}[i], Released: now.AddDate(-1, 0, 0), Cast: []core.Person{{TMDB: int64(i + 11), Name: name}}}
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(func(d *testServerDeps) { d.Catalog = c })
	defer f.srv.Close()
	for _, tc := range []struct {
		query       string
		size, total int
	}{
		{"actor=ZO%C3%AB+NORTH", 1, 2},
		{"actor=ZO%C3%8B+NORTH", 0, 0},
		{"actor=Missing", 0, 0},
		{"actor!=Missing", 1, 3},
		{"actor=Missing,pat+west", 1, 1},
		{"actor=Zo%C3%AB+North&actor=Pat+West", 0, 0},
	} {
		rec := f.get(t, "/library/sections/1/all?"+tc.query+"&X-Plex-Container-Start=0&X-Plex-Container-Size=1", jsonAccept...)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", tc.query, rec.Code)
		}
		mc := decodeJSON(t, rec)
		if mc["size"] != float64(tc.size) || mc["totalSize"] != float64(tc.total) {
			t.Errorf("%s: size=%v total=%v", tc.query, mc["size"], mc["totalSize"])
		}
	}
}
