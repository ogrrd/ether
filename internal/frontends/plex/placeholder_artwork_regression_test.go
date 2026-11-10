package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/overlays"
)

func TestPlaceholderArtworkAuthorization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("image"))
	}))
	defer upstream.Close()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "overlay_off", true: "overlay_on"}[enabled], func(t *testing.T) {
			f := newFixture(func(d *Deps) {
				d.ImageURL = func(path, size string) string { return upstream.URL + path }
				setDepsHTTPClient(d, upstream.Client())
			})
			defer f.srv.Close()
			ck := core.SmartKey(991)
			title := core.Title{Key: core.MovieKey(991123), Type: core.TypeMovie, Title: "Future", Released: fixedNow.Add(48 * time.Hour), PosterPath: "/future.jpg", BackdropPath: "/future-art.jpg"}
			f.catalog.titles[title.Key] = title
			c := &scopedFakeCollections{&fakeCollections{cols: map[core.RatingKey]core.Collection{ck: {Key: ck, Type: core.TypeMovie, Upcoming: true}}, items: map[core.RatingKey][]core.Title{ck: {title}}}}
			f.srv.d.Collections = c
			ov := overlays.New(discoveryDB(t), overlays.Options{Images: &discoveryImageSource{}})
			if err := ov.SaveSettings(context.Background(), overlays.Settings{Enabled: enabled, Localize: true}); err != nil {
				t.Fatal(err)
			}
			f.srv.d.Discovery = (&discovery.Features{Overlays: ov}).Frontend()
			md := f.srv.placeholderMetadata(context.Background(), &Request{User: &ownerUser, Language: "fr-CA"}, &title, ck)
			for _, path := range []string{md.Thumb, md.Art} {
				u, err := url.Parse(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(u.Path, "discoveryPoster") || len(u.Query()["collection"]) != 1 || u.Query().Get("collection") != ck.String() {
					t.Fatalf("invalid scoped image URL %q", path)
				}
				for _, target := range []string{path, "/photo/:/transcode?url=" + url.QueryEscape(path)} {
					if r := f.get(t, target); r.Code != http.StatusOK {
						t.Fatalf("%s: %d %s", target, r.Code, r.Body.String())
					}
					if r := f.get(t, target, "X-Plex-Token", friendToken); r.Code != http.StatusNotFound {
						t.Fatalf("forbidden %s: %d", target, r.Code)
					}
				}
			}
			canonical := "/library/metadata/" + title.Key.String() + "/thumb/1"
			if r := f.get(t, canonical); r.Code != http.StatusOK {
				t.Fatalf("canonical: %d", r.Code)
			}
			delete(c.cols, ck)
			delete(c.items, ck)
			for _, target := range []string{md.Thumb, "/photo/:/transcode?url=" + url.QueryEscape(md.Thumb), canonical} {
				if r := f.get(t, target); r.Code != http.StatusNotFound {
					t.Fatalf("deleted %s: %d", target, r.Code)
				}
			}
		})
	}
}
