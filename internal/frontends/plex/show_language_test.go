package plex

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

type countingShowCatalog struct {
	*fakeCatalog
	gets, batches int
}

func (c *countingShowCatalog) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	c.gets++
	return c.fakeCatalog.Get(ctx, k)
}

func (c *countingShowCatalog) ResolveShowParents(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	c.batches++
	out := map[core.RatingKey]core.Title{}
	for _, k := range keys {
		if t, err := c.fakeCatalog.Get(ctx, k); err == nil {
			out[k] = *t
		}
	}
	return out, nil
}

// A list of episodes from distinct shows resolves their series' languages
// in one batch for track selection, not one catalogue read per show; the
// languages are the ones Get returns.
func TestRenderResolvesEpisodeShowLanguagesInOneBatch(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	cat := &countingShowCatalog{fakeCatalog: f.catalog}
	f.srv.setCatalog(cat)
	f.srv.d.TrackPreferences = trackprefs.New(store.OpenTest(t))
	var eps []core.Title
	want := map[core.RatingKey]string{}
	for i, lang := range []string{"ja", "fr", "ko"} {
		id := int64(9100 + i)
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", Language: lang}
		cat.titles[show.Key] = show
		eps = append(eps, core.Title{Key: core.EpisodeKey(id, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1})
		want[show.Key] = lang
	}
	pr := &Request{User: &core.User{ID: 7}}
	ctx := reqmemo.With(context.Background())
	f.srv.preloadShowLanguages(ctx, pr, eps, renderOpts{media: true, detailEpisodes: true})
	for show, lang := range want {
		if got := f.srv.showLanguage(ctx, show); got != lang {
			t.Fatalf("show %v language %q, want %q", show, got, lang)
		}
	}
	if cat.batches != 1 || cat.gets != 0 {
		t.Fatalf("%d batches and %d single reads, want one batch", cat.batches, cat.gets)
	}
	// A render that selects no streams does not resolve shows.
	f.srv.preloadShowLanguages(reqmemo.With(context.Background()), pr, eps, renderOpts{media: true})
	if cat.batches != 1 {
		t.Fatal("a stream-less render resolved shows")
	}
	// Outside a read request each lookup is a plain Get.
	if got := f.srv.showLanguage(context.Background(), core.ShowKey(9100)); got != "ja" || cat.gets != 1 {
		t.Fatalf("plain lookup %q after %d reads", got, cat.gets)
	}
}
