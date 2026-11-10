package plex

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type collectionGridPolicy struct{ key core.RatingKey }
func (collectionGridPolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (p collectionGridPolicy) Allowed(_ *core.User, t *core.Title) bool { return t.Key != p.key }

type collectionGridSource struct {
	core.Collections
	calls int
	fail  bool
}

func (s *collectionGridSource) Items(ctx context.Context, u *core.User, key core.RatingKey, offset, limit int) (core.Page, error) {
	s.calls++
	if s.fail {
		return core.Page{}, errors.New("collection read failed")
	}
	return s.Collections.Items(ctx, u, key, offset, limit)
}

// Client continuation offsets count returned cards, not unfiltered membership.
// Exercise full pages, duplicates, changed discovery, current authorization,
// aliases, and a failed generation build through the real HTTP adapter.
func TestCollectionGridPagingInvariant(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	key := core.SmartKey(9821)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Grid", Type: core.TypeMovie}
	var expected []string
	for i := 0; i < 60; i++ {
		title := fightClub
		title.Key = core.MovieKey(int64(40000 + i))
		title.Title = fmt.Sprintf("Movie %02d", i)
		f.cols.items[key] = append(f.cols.items[key], title)
		f.catalog.titles[title.Key] = title
		if i < 10 || i > 12 {
			expected = append(expected, title.Key.String())
		}
	}
	f.cols.items[key] = append(f.cols.items[key], f.cols.items[key][2])
	source := &collectionGridSource{Collections: f.cols}
	f.srv.d.Collections = source
	f.srv.d.FilterAvailable = func(_ context.Context, in []core.Title) []core.Title {
		out := make([]core.Title, 0, len(in))
		for _, v := range in {
			if v.Key.TMDB() < 40010 || v.Key.TMDB() > 40012 {
				out = append(out, v)
			}
		}
		return out
	}
	get := func(prefix string, start, size int) model.MediaContainer {
		t.Helper()
		response := f.get(t, fmt.Sprintf("%s?X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", prefix, start, size), jsonAccept...)
		if response.Code != 200 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeShelfContainer(t, response.Body.Bytes())
	}
	path := "/library/collections/" + key.String() + "/children"
	source.fail = true
	if response := f.get(t, path, jsonAccept...); response.Code == 200 {
		t.Fatal("source error published an empty successful page")
	}
	source.fail = false
	var got []string
	for _, part := range []struct{ start, size int }{{0, 8}, {8, 24}, {32, 24}, {56, 24}} {
		page := get(path, part.start, part.size)
		if page.TotalSize == nil || *page.TotalSize != len(expected) {
			t.Fatalf("page=%+v", page)
		}
		want := min(part.size, len(expected)-part.start)
		if page.Size != want {
			t.Fatalf("start=%d size=%d want=%d", part.start, page.Size, want)
		}
		for _, v := range page.Metadata {
			got = append(got, v.RatingKey)
		}
		// Discovery changes cannot rewrite an already published browsing cut.
		f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("page union=%v want=%v", got, expected)
	}
	if source.calls != 2 {
		t.Fatalf("source rebuilt on continuation: %d calls", source.calls)
	}
	alias := get("/library/metadata/"+key.String()+"/children", 8, 24)
	if len(alias.Metadata) != 24 || alias.Metadata[0].RatingKey != expected[8] {
		t.Fatalf("alias shifted: %+v", alias)
	}
	f.srv.setTestPolicy(collectionGridPolicy{key: core.MovieKey(40020)})
	restricted := get(path, 0, 100)
	if restricted.TotalSize == nil || *restricted.TotalSize != len(expected)-1 {
		t.Fatalf("authorization not applied before count: %+v", restricted)
	}
	for _, item := range restricted.Metadata {
		if item.RatingKey == core.MovieKey(40020).String() {
			t.Fatal("retained generation bypassed current authorization")
		}
	}
}
