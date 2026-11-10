package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
)

const hostileArtworkSVG = `<svg xmlns="http://www.w3.org/2000/svg"><script>fetch('/ether/api/config',{headers:{'X-Plex-Token':localStorage.getItem('ether-admin-token')}})</script></svg>`

func assertPassiveArtwork(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("artwork can execute on the owner origin: CSP=%q", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q", got)
	}
}

func TestRemoteArtworkCannotExecuteOnOwnerOrigin(t *testing.T) {
	for _, tc := range []struct{ name, ct, body string }{
		{"svg", "image/svg+xml", hostileArtworkSVG},
		{"html", "text/html", "<!doctype html><script>localStorage.getItem('ether-admin-token')</script>"},
		{"jpeg", "image/jpeg", "image bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			f := newFixture(func(d *Deps) {
				d.ImageURL = func(path, size string) string { return upstream.URL + "/" + size + path }
				setDepsHTTPClient(d, upstream.Client())
			})
			defer f.srv.Close()
			for _, path := range []string{
				"/library/metadata/" + alien.Key.String() + "/thumb",
				"/photo/:/transcode?url=" + urlEsc(upstream.URL+"/original/logo.svg"),
			} {
				sep := "?"
				if strings.Contains(path, "?") {
					sep = "&"
				}
				// A link carries the attacker's ordinary viewer token, with no
				// owner credential required to navigate to the active document.
				rec := f.request(t, http.MethodGet, path+sep+"X-Plex-Token="+friendToken, map[string]string{"X-Plex-Token": ""})
				if rec.Code != http.StatusOK || rec.Body.String() != tc.body {
					t.Fatalf("artwork %s: %d %q", path, rec.Code, rec.Body.String())
				}
				assertPassiveArtwork(t, rec)
			}
		})
	}
}

type customURLCollections struct {
	*fakeCollections
	images *artwork.Images
	url    string
}

func (a *customURLCollections) CollectionArtwork(ctx context.Context, _ *core.User, _ core.RatingKey, _ string, _ int) ([]byte, string, bool, error) {
	data, ct, err := a.images.GetURL(ctx, a.url)
	return data, ct, err == nil, err
}

func TestCustomCollectionArtworkCannotExecuteOnOwnerOrigin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(hostileArtworkSVG))
	}))
	defer upstream.Close()
	f := newFixture()
	f.srv.Close()
	key := core.SmartKey(12)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Custom artwork", Kind: "smart", Type: core.TypeMovie, ChildCount: 1}
	f.cols.items[key] = []core.Title{alien}
	d := f.srv.d
	d.Collections = &customURLCollections{fakeCollections: f.cols, images: artwork.NewImages(artwork.ImagesOptions{HTTPClient: upstream.Client()}), url: upstream.URL + "/poster.svg"}
	f.srv = NewServer(testDeps(d))
	defer f.srv.Close()
	for _, path := range []string{
		"/library/metadata/" + key.String() + "/thumb?X-Plex-Token=" + friendToken,
		"/photo/:/transcode?url=" + urlEsc("/library/collections/"+key.String()+"/thumb/1") + "&X-Plex-Token=" + friendToken,
	} {
		rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": ""})
		if rec.Code != http.StatusOK || rec.Body.String() != hostileArtworkSVG || rec.Header().Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("custom SVG: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		assertPassiveArtwork(t, rec)
	}
}
