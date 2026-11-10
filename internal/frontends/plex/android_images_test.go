package plex

import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// Reconstructed connection-switch and format requests, not an Android capture.
func TestAndroidPhotoConnectionAndFormat(t *testing.T) {
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewRGBA(image.Rect(0, 0, 40, 60))); err != nil {
		t.Fatal(err)
	}
	// The handler runs for test requests and for background palette fetches
	// concurrently, so the count must be atomic.
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(source.Bytes())
	}))
	defer upstream.Close()
	for _, product := range []string{"Plex for Android (Mobile)", "Plex for Android (TV)", "Plex Web", "Plezy"} {
		t.Run(product, func(t *testing.T) {
			f := newFixture(func(d *Deps) {
				d.ImageURL = func(path, size string) string { return upstream.URL + "/t/p/" + size + path }
				setDepsHTTPClient(d, upstream.Client())
			})
			defer f.srv.Close()

			android := strings.HasPrefix(product, "Plex for Android")
			for _, detail := range []bool{false, true} {
				md := f.srv.metadata(context.Background(), &Request{User: &ownerUser, Product: product, Platform: "Android"}, &fightClub, renderOpts{detail: detail})
				for _, art := range []string{md.Thumb, md.Art} {
					if art == "" || !strings.HasPrefix(art, "/") || strings.Contains(art, ownerToken) {
						t.Fatalf("art URL: %q", art)
					}
					for _, base := range []string{"", "http://127.0.0.1:32400", "https://10-0-0-43.server.plex.direct:32450", "https://previous-connection.invalid:443"} {
						q := url.Values{"url": {base + art}, "width": {"300"}, "height": {"450"}, "minSize": {"1"}, "upscale": {"1"}, "format": {"jpeg"}}
						target := "/photo/:/transcode?" + q.Encode()
						rec := f.get(t, target, "X-Plex-Product", product, "X-Plex-Platform", "Android")
						if base == "https://previous-connection.invalid:443" && !android {
							if rec.Code != 403 {
								t.Fatalf("other client changed: %d", rec.Code)
							}
							continue
						}
						if rec.Code != 200 {
							t.Errorf("%s: %d", base, rec.Code)
							continue
						}
						want := "image/png"
						if android {
							want = "image/jpeg"
						}
						if rec.Header().Get("Content-Type") != want {
							t.Errorf("format request returned %s, want %s", rec.Header().Get("Content-Type"), want)
						}
						_, kind, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
						if err != nil || "image/"+kind != want {
							t.Errorf("bytes: %s %v", kind, err)
						}
						if rec.Header().Get("Content-Length") == "" || rec.Header().Get("Location") != "" {
							t.Fatal("missing length or redirected")
						}
					}
				}
			}
			for _, option := range []string{"width=bad", "height=1.5", "minSize=true", "upscale=2"} {
				r := f.get(t, "/photo/:/transcode?url="+url.QueryEscape("/library/metadata/"+fightClub.Key.String()+"/thumb/1")+"&"+option, "X-Plex-Product", product)
				if r.Code != 400 {
					t.Fatalf("invalid %s: %d", option, r.Code)
				}
			}
			f.srv.testAuth().users["kid"] = core.User{ID: 30, MaxContentRating: "PG"}
			// Let queued palette work finish so only the requests below can count.
			f.srv.waitBackground()
			before := hits.Load()
			// Never fetch arbitrary URLs, or use a nested owner token to elevate a viewer.
			target := "/photo/:/transcode?url=" + url.QueryEscape("https://previous-connection.invalid/library/metadata/"+fightClub.Key.String()+"/thumb/1?X-Plex-Token="+ownerToken)
			if r := f.get(t, target, "X-Plex-Token", "invalid", "X-Plex-Product", product); r.Code != http.StatusUnauthorized {
				t.Fatal(r.Code)
			}
			if android {
				if r := f.get(t, target, "X-Plex-Token", "kid", "X-Plex-Product", product); r.Code != 404 {
					t.Fatalf("nested token elevated viewer: %d", r.Code)
				}
			}
			if r := f.get(t, "/photo/:/transcode?url="+url.QueryEscape("http://169.254.169.254/latest/meta-data"), "X-Plex-Product", product); r.Code != http.StatusForbidden {
				t.Fatal(r.Code)
			}
			if hits.Load() != before {
				t.Fatal("unauthorized image fetch")
			}
		})
	}
}
