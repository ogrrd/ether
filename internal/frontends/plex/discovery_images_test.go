package plex

import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/config"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/overlays"
)

type discoveryImageSource struct{ calls int }

func (s *discoveryImageSource) Get(_ context.Context, path string, _ int) ([]byte, string, error) {
	s.calls++
	return []byte(path), "image/jpeg", nil
}

func TestPhotoTranscodeDiscoveryArtwork(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	images := &discoveryImageSource{}
	ov := overlays.New(discoveryDB(t), overlays.Options{Images: images})
	if err := ov.SaveSettings(ctx, overlays.Settings{Enabled: true, Localize: true}); err != nil {
		t.Fatal(err)
	}
	f.srv.d.Discovery = (&discovery.Features{Overlays: ov, Localize: func(_ context.Context, title *core.Title, lang string) (*core.Title, error) {
		localized := *title
		localized.PosterPath = "/" + lang + "-poster.jpg"
		localized.BackdropPath = "/" + lang + "-backdrop.jpg"
		localized.LogoPath = "/" + lang + "-logo.jpg"
		return &localized, nil
	}}).Frontend()
	pr := &Request{User: &ownerUser, Language: "fr"}
	md := f.srv.metadata(ctx, pr, &fightClub, renderOpts{})
	if !strings.Contains(md.Thumb, "/discoveryPoster/") {
		t.Fatalf("custom poster not advertised: %q", md.Thumb)
	}
	for _, kind := range []string{"poster", "backdrop", "logo"} {
		t.Run(kind, func(t *testing.T) {
			path := md.Thumb + "&kind=" + kind
			direct := f.request(t, http.MethodGet, path, nil)
			want := "/fr-" + kind + ".jpg"
			if direct.Code != http.StatusOK || direct.Body.String() != want {
				t.Fatalf("direct artwork: %d %q", direct.Code, direct.Body.String())
			}
			for _, prefix := range []string{"", "http://127.0.0.1:32400"} {
				target := "/photo/:/transcode?width=300&height=450&url=" + url.QueryEscape(prefix+path)
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					r := f.request(t, method, target, nil)
					if r.Code != http.StatusOK || r.Header().Get("Content-Type") != "image/jpeg" {
						t.Fatalf("%s wrapped artwork: %d %q", method, r.Code, r.Body.String())
					}
					if method == http.MethodGet && r.Body.String() != want || method == http.MethodHead && r.Body.Len() != 0 {
						t.Fatalf("%s wrapped body: %q", method, r.Body.String())
					}
					assertPassiveArtwork(t, r)
				}
				r := f.request(t, http.MethodGet, target, map[string]string{"If-None-Match": direct.Header().Get("ETag")})
				if r.Code != http.StatusNotModified || r.Body.Len() != 0 {
					t.Fatalf("conditional artwork: %d %q", r.Code, r.Body.String())
				}
			}
		})
	}
	// The nested URL cannot supply credentials or bypass viewer restrictions.
	f.srv.testAuth().users["kid"] = core.User{ID: 30, MaxContentRating: "PG"}
	target := "/photo/:/transcode?url=" + url.QueryEscape(md.Thumb+"&X-Plex-Token="+ownerToken)
	for token, want := range map[string]int{"kid": http.StatusNotFound, "invalid": http.StatusUnauthorized} {
		before := images.calls
		r := f.request(t, http.MethodGet, target, map[string]string{"X-Plex-Token": token})
		if r.Code != want || images.calls != before {
			t.Fatalf("viewer %q: status=%d image fetches=%d", token, r.Code, images.calls-before)
		}
	}
	r := f.request(t, http.MethodGet, "/photo/:/transcode?url="+url.QueryEscape("https://untrusted.invalid"+md.Thumb), nil)
	if r.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", r.Code)
	}
}

func TestPhotoTranscodePosterProvider(t *testing.T) {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 400, 600)), nil)
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("type") != "movie" {
			t.Error("wrong type")
		}
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()
	cfg := config.Posters{Enabled: true, Source: "postersplus", BaseURL: upstream.URL, APIKey: "never-visible"}
	images := artwork.NewImages(artwork.ImagesOptions{Posters: func() config.Posters { return cfg }})
	f := newFixture()
	defer f.srv.Close()
	ov := overlays.New(discoveryDB(t), overlays.Options{Images: images})
	f.srv.d.Discovery = (&discovery.Features{Overlays: ov}).Frontend()
	pr := &Request{User: &ownerUser}
	md := f.srv.metadata(context.Background(), pr, &fightClub, renderOpts{})
	if !strings.Contains(md.Thumb, "discoveryPoster") || strings.Contains(md.Thumb, "never-visible") {
		t.Fatal(md.Thumb)
	}
	target := "/photo/:/transcode?width=200&url=" + url.QueryEscape(md.Thumb)
	r := f.request(t, http.MethodGet, target, nil)
	if r.Code != 200 || hits != 1 {
		t.Fatalf("status %d hits %d", r.Code, hits)
	}
	decoded, _, err := image.DecodeConfig(bytes.NewReader(r.Body.Bytes()))
	if err != nil || decoded.Width != 200 {
		t.Fatal(decoded, err)
	}
	for _, product := range []string{"Plex for Android (Mobile)", "Plex for Android (TV)"} {
		target := "/photo/:/transcode?width=200&format=png&url=" + url.QueryEscape("https://previous-connection.invalid"+md.Thumb)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := f.request(t, method, target, map[string]string{"X-Plex-Product": product})
			if r.Code != 200 || r.Header().Get("Content-Type") != "image/png" || r.Header().Get("Location") != "" {
				t.Fatalf("%s provider artwork: %d %s", product, r.Code, r.Header())
			}
			if method == http.MethodGet {
				if _, kind, err := image.DecodeConfig(bytes.NewReader(r.Body.Bytes())); err != nil || kind != "png" {
					t.Fatalf("provider output: %s %v", kind, err)
				}
			} else if r.Body.Len() != 0 {
				t.Fatal("HEAD body")
			}
		}
	}
	old := md.Thumb
	cfg.APIKey = "changed"
	md = f.srv.metadata(context.Background(), pr, &fightClub, renderOpts{})
	if md.Thumb == old {
		t.Fatal("client cache version unchanged")
	}
	cfg.Enabled = false
	md = f.srv.metadata(context.Background(), pr, &fightClub, renderOpts{})
	if strings.Contains(md.Thumb, "discoveryPoster") {
		t.Fatal("default not restored")
	}
}
