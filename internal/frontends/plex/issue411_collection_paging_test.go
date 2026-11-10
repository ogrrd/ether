package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type placeholderCollectionsFake struct {
	*fakeCollections
}

func (p placeholderCollectionsFake) AllowsPlaceholders(context.Context, *core.User, core.RatingKey, string) bool {
	return true
}

func (p placeholderCollectionsFake) Placeholder(_ context.Context, _ *core.User, col, key core.RatingKey, _ string) (*core.Title, error) {
	for _, t := range p.items[col] {
		if t.Key == key {
			cp := t
			return &cp, nil
		}
	}
	return nil, core.ErrNotFound
}

func (p placeholderCollectionsFake) FindPlaceholder(_ context.Context, _ *core.User, key core.RatingKey, _ string) (*core.Title, core.RatingKey, error) {
	for col, list := range p.items {
		for _, t := range list {
			if t.Key == key {
				cp := t
				return &cp, col, nil
			}
		}
	}
	return nil, 0, core.ErrNotFound
}

// Issue #411: a mixed collection (15 shows + 32 movies) promoted to the Movies
// section shelf must expose all 32 movies when paged via its pinned etherShelf
// HubKey, even when the shelf hub itself carried fewer lead movie items.
func TestMixedCollectionShelfPagesAll32Movies(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	baseKey := core.SmartKey(411)
	movieSectionKey := core.SectionCollectionKey(baseKey, core.TypeMovie)

	var movies []core.Title
	var wantKeys []string
	for i := 1; i <= 32; i++ {
		m := fightClub
		m.Key = core.MovieKey(int64(41100 + i))
		m.Title = fmt.Sprintf("Movie %02d", i)
		movies = append(movies, m)
		f.catalog.titles[m.Key] = m
		wantKeys = append(wantKeys, m.Key.String())
	}
	for i := 1; i <= 15; i++ {
		sh := breakingBad
		sh.Key = core.ShowKey(int64(41200 + i))
		sh.Title = fmt.Sprintf("Show %02d", i)
		f.catalog.titles[sh.Key] = sh
	}

	f.cols.cols[movieSectionKey] = core.Collection{
		Key:        movieSectionKey,
		Title:      "Mixed MDBList",
		Kind:       "smart",
		Type:       core.TypeMovie,
		ChildCount: len(movies),
	}
	f.cols.items[movieSectionKey] = movies

	// Shelf carries only the first 9 movies (as happened when 15 shows consumed
	// the 24-item row cap) with More=true.
	rec := &fakeRecommender{
		section: map[core.MediaType][]core.Hub{
			core.TypeMovie: {{
				ID:    "collection.411",
				Key:   movieSectionKey,
				Title: "Mixed MDBList",
				Type:  core.TypeMovie,
				Items: movies[:9],
				More:  true,
			}},
		},
	}
	f.srv.d.Recommender = rec

	headers := map[string]string{"Accept": "application/json"}
	get := func(path string) model.MediaContainer {
		t.Helper()
		resp := f.request(t, "GET", path, headers)
		if resp.Code != 200 {
			t.Fatalf("%s: %d %s", path, resp.Code, resp.Body.String())
		}
		return decodeShelfContainer(t, resp.Body.Bytes())
	}

	var shelf model.Hub
	for _, h := range get("/hubs/sections/1").Hub {
		if h.HubIdentifier == "collection.411" {
			shelf = h
			break
		}
	}
	if shelf.Key == "" || !shelf.More {
		t.Fatalf("expected pinned shelf with More=true; got key=%q more=%v size=%d", shelf.Key, shelf.More, shelf.Size)
	}

	var gotAll []string
	for _, start := range []int{0, 12, 24} {
		u, err := url.Parse(shelf.Key)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("X-Plex-Container-Start", fmt.Sprint(start))
		q.Set("X-Plex-Container-Size", "12")
		u.RawQuery = q.Encode()
		mc := get(u.String())
		if mc.TotalSize == nil || *mc.TotalSize != 32 {
			t.Fatalf("start=%d: totalSize=%v, want 32", start, mc.TotalSize)
		}
		wantPageLen := min(12, 32-start)
		if mc.Size != wantPageLen || len(mc.Metadata) != wantPageLen {
			t.Fatalf("start=%d: size=%d len=%d, want %d", start, mc.Size, len(mc.Metadata), wantPageLen)
		}
		for _, md := range mc.Metadata {
			gotAll = append(gotAll, md.RatingKey)
		}
	}
	if !reflect.DeepEqual(gotAll, wantKeys) {
		t.Fatalf("paged movies mismatch:\ngot  %v\nwant %v", gotAll, wantKeys)
	}
}

func TestUpcomingCollectionAndRecentlyAddedPageOverOpenableItems(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	blockedKey := core.MovieKey(51002)
	f.srv.d.FilterAvailable = func(_ context.Context, titles []core.Title) []core.Title {
		out := make([]core.Title, 0, len(titles))
		for _, item := range titles {
			if item.Key != blockedKey {
				out = append(out, item)
			}
		}
		return out
	}

	m1 := fightClub
	m1.Key = core.MovieKey(51001)
	m1.Released = fixedNow.Add(-2 * time.Hour)
	mBlocked := fightClub
	mBlocked.Key = blockedKey
	mBlocked.Released = fixedNow.Add(-3 * time.Hour)
	m2 := fightClub
	m2.Key = core.MovieKey(51003)
	m2.Released = fixedNow.Add(-4 * time.Hour)
	mFuture := fightClub
	mFuture.Key = core.MovieKey(51004)
	mFuture.Released = fixedNow.Add(48 * time.Hour)

	for _, item := range []core.Title{m1, mBlocked, m2, mFuture} {
		f.catalog.titles[item.Key] = item
	}

	ck := core.SmartKey(510)
	f.cols.cols[ck] = core.Collection{
		Key:        ck,
		Title:      "Upcoming Mixed",
		Kind:       "smart",
		Type:       core.TypeMovie,
		Upcoming:   true,
		ChildCount: 4,
	}
	f.cols.items[ck] = []core.Title{m1, mBlocked, m2, mFuture}
	f.srv.d.Collections = placeholderCollectionsFake{f.cols}

	headers := map[string]string{"Accept": "application/json"}
	get := func(path string) model.MediaContainer {
		t.Helper()
		resp := f.request(t, "GET", path, headers)
		if resp.Code != 200 {
			t.Fatalf("%s: %d %s", path, resp.Code, resp.Body.String())
		}
		return decodeShelfContainer(t, resp.Body.Bytes())
	}

	metaResp := f.request(t, "GET", "/library/collections/"+ck.String(), headers)
	if metaResp.Code != 200 {
		t.Fatalf("collection meta: %d %s", metaResp.Code, metaResp.Body.String())
	}
	var metaBody struct {
		MediaContainer struct {
			Metadata []struct {
				ChildCount *int `json:"childCount"`
			}
		}
	}
	if err := json.Unmarshal(metaResp.Body.Bytes(), &metaBody); err != nil {
		t.Fatal(err)
	}
	if len(metaBody.MediaContainer.Metadata) != 1 || metaBody.MediaContainer.Metadata[0].ChildCount == nil || *metaBody.MediaContainer.Metadata[0].ChildCount != 3 {
		t.Fatalf("upcoming collection ChildCount = %+v, want 3", metaBody.MediaContainer.Metadata)
	}

	p1 := get(fmt.Sprintf("/library/collections/%s/children?X-Plex-Container-Start=0&X-Plex-Container-Size=2", ck.String()))
	p2 := get(fmt.Sprintf("/library/collections/%s/children?X-Plex-Container-Start=2&X-Plex-Container-Size=2", ck.String()))
	if p1.TotalSize == nil || *p1.TotalSize != 3 || len(p1.Metadata) != 2 || len(p2.Metadata) != 1 {
		t.Fatalf("upcoming pages: p1=%d/%v p2=%d/%v, want 2/3 and 1/3", len(p1.Metadata), p1.TotalSize, len(p2.Metadata), p2.TotalSize)
	}
	if p1.Metadata[0].RatingKey != m1.Key.String() || p1.Metadata[1].RatingKey != m2.Key.String() || p2.Metadata[0].RatingKey != mFuture.Key.String() {
		t.Fatalf("unexpected upcoming order: [%s %s %s]", p1.Metadata[0].RatingKey, p1.Metadata[1].RatingKey, p2.Metadata[0].RatingKey)
	}

	// Fallback /library/recentlyAdded path (fakeCatalog does not implement RecentPage):
	// mBlocked is second newest, so slicing before openableTitles used to return only 1 item on Size=2.
	m1.AddedAt = fixedNow.Add(-1 * time.Minute)
	mBlocked.AddedAt = fixedNow.Add(-2 * time.Minute)
	m2.AddedAt = fixedNow.Add(-3 * time.Minute)
	f.catalog.titles = map[core.RatingKey]core.Title{
		m1.Key:       m1,
		mBlocked.Key: mBlocked,
		m2.Key:       m2,
	}
	ra := get("/library/recentlyAdded?type=1&X-Plex-Container-Start=0&X-Plex-Container-Size=2")
	if ra.TotalSize == nil || *ra.TotalSize != 2 || len(ra.Metadata) != 2 {
		t.Fatalf("recentlyAdded: len=%d total=%v, want 2/2", len(ra.Metadata), ra.TotalSize)
	}
}
