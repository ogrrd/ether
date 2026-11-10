package plex

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type scopedFakeCollections struct{ *fakeCollections }

type denyTitlePolicy struct{}
func (denyTitlePolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (denyTitlePolicy) Allowed(_ *core.User, t *core.Title) bool { return t.Title != "Blocked" }

func (c *scopedFakeCollections) AllowsPlaceholders(_ context.Context, u *core.User, collection core.RatingKey, _ string) bool {
	_, ok := c.cols[collection]
	return ok && u != nil && u.IsOwner
}

func TestUpcomingPagingAppliesPolicyBeforeCounting(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, denyTitlePolicy{}) })
	defer f.srv.Close()
	ck := core.SmartKey(88)
	items := []core.Title{
		{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "First", Released: fixedNow.Add(time.Hour)},
		{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "Blocked", Released: fixedNow.Add(time.Hour)},
		{Key: core.MovieKey(203), Type: core.TypeMovie, Title: "Last", Released: fixedNow.Add(time.Hour)},
	}
	f.srv.d.Collections = &scopedFakeCollections{&fakeCollections{cols: map[core.RatingKey]core.Collection{ck: {Key: ck, Type: core.TypeMovie, Upcoming: true}}, items: map[core.RatingKey][]core.Title{ck: items}}}
	r := f.get(t, "/library/collections/"+ck.String()+"/children?X-Plex-Container-Start=1&X-Plex-Container-Size=1")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `totalSize="2"`) || !strings.Contains(r.Body.String(), `title="Last"`) || strings.Contains(r.Body.String(), "Blocked") {
		t.Fatalf("page: %d %s", r.Code, r.Body.String())
	}
}
func (c *scopedFakeCollections) Placeholder(_ context.Context, u *core.User, collection, key core.RatingKey, _ string) (*core.Title, error) {
	if u == nil || !u.IsOwner {
		return nil, core.ErrNotFound
	}
	for _, item := range c.items[collection] {
		if item.Key == key {
			return &item, nil
		}
	}
	return nil, core.ErrNotFound
}
func (c *scopedFakeCollections) FindPlaceholder(ctx context.Context, u *core.User, key core.RatingKey, device string) (*core.Title, core.RatingKey, error) {
	for ck := range c.cols {
		if t, err := c.Placeholder(ctx, u, ck, key, device); err == nil {
			return t, ck, nil
		}
	}
	return nil, 0, core.ErrNotFound
}

func TestUpcomingCollectionScopedDetailsAndViewerPolicy(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	k := core.SmartKey(77)
	title := core.Title{Key: core.MovieKey(123456), Type: core.TypeMovie, Title: "Future Film", Released: fixedNow.Add(48 * time.Hour), ContentRating: "R", PosterPath: "/future.jpg"}
	f.catalog.titles[title.Key] = title
	c := &scopedFakeCollections{&fakeCollections{cols: map[core.RatingKey]core.Collection{k: {Key: k, Title: "Look Ahead", Type: core.TypeMovie, Upcoming: true}}, items: map[core.RatingKey][]core.Title{k: {title}}}}
	f.srv.d.Collections = c
	base := "/library/metadata/" + title.Key.String()
	if r := f.get(t, base); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Scheduled release") {
		t.Fatalf("canonical: %d %s", r.Code, r.Body.String())
	}
	path := base + "?collection=" + k.String()
	if r := f.get(t, path); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Future Film") || !strings.Contains(r.Body.String(), "Scheduled release") {
		t.Fatalf("scoped: %d %s", r.Code, r.Body.String())
	}
	if r := f.get(t, "/library/collections/"+k.String()+"/children"); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "collection="+k.String()) {
		t.Fatalf("children: %d %s", r.Code, r.Body.String())
	}
	if r := f.get(t, path, "X-Plex-Token", friendToken); r.Code != http.StatusNotFound {
		t.Fatalf("unauthorized: %d %s", r.Code, r.Body.String())
	}
	decision := "/video/:/transcode/universal/decision?path=" + url.QueryEscape(path)
	if r := f.get(t, decision); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "has not been released") {
		t.Fatalf("upcoming decision: %d %s", r.Code, r.Body.String())
	}
	if r := f.get(t, base+"?collection="+core.SmartKey(78).String()); r.Code != http.StatusNotFound {
		t.Fatalf("wrong collection: %d", r.Code)
	}
	art := base + "/thumb/1?collection=" + k.String()
	if r := f.get(t, art); r.Code != http.StatusOK {
		t.Fatalf("scoped artwork: %d", r.Code)
	}
	if r := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(art)); r.Code != http.StatusOK {
		t.Fatalf("scoped photo: %d", r.Code)
	}
	if r := f.get(t, base+"/thumb/1"); r.Code != http.StatusOK {
		t.Fatalf("canonical artwork: %d", r.Code)
	}
	if r := f.get(t, art, "X-Plex-Token", friendToken); r.Code != http.StatusNotFound {
		t.Fatalf("unauthorized artwork: %d", r.Code)
	}
}
