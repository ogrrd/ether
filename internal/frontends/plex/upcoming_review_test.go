package plex

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestReviewFutureRevocationAndNoWarm(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ck := core.SmartKey(188)
	title := core.Title{Key: core.MovieKey(188123), Type: core.TypeMovie, Title: "Future review", Released: fixedNow.Add(48 * time.Hour), PosterPath: "/future.jpg"}
	f.catalog.titles[title.Key] = title
	c := &scopedFakeCollections{&fakeCollections{cols: map[core.RatingKey]core.Collection{ck: {Key: ck, Type: core.TypeMovie, Upcoming: true}}, items: map[core.RatingKey][]core.Title{ck: {title}}}}
	f.srv.setCollections(c)
	playback := f.srv.d.Playback.(*fakePlayback)
	warmer := &detailWarmer{Playback: playback}
	f.srv.d.Playback = warmer
	base := "/library/metadata/" + title.Key.String()
	scoped := base + "?collection=" + ck.String()
	for _, path := range []string{base, scoped} {
		r := f.get(t, path)
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Scheduled release") {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
	r := f.get(t, "/video/:/transcode/universal/decision?path="+url.QueryEscape(scoped))
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "has not been released") {
		t.Fatalf("decision: %d %s", r.Code, r.Body.String())
	}
	if playback.prepared != 0 {
		t.Fatalf("future decision called playback Prepare %d times", playback.prepared)
	}
	if warmer.calls != 0 {
		t.Fatalf("future detail caused %d PreResolve calls", warmer.calls)
	}
	for _, path := range []string{base, scoped, base + "/thumb/1", base + "/thumb/1?collection=" + ck.String()} {
		if r := f.get(t, path, "X-Plex-Token", friendToken); r.Code != http.StatusNotFound {
			t.Fatalf("wrong viewer %s: %d", path, r.Code)
		}
	}
	delete(c.cols, ck)
	delete(c.items, ck)
	for _, path := range []string{base, scoped, base + "/thumb/1", base + "/thumb/1?collection=" + ck.String()} {
		if r := f.get(t, path); r.Code != http.StatusNotFound {
			t.Fatalf("revoked %s: %d", path, r.Code)
		}
	}
}
