package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestFrontendArtworkUsesPlexSelectionAndRechecksVisibility(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	images, complete, err := source.List(context.Background(), req)
	if err != nil || !complete {
		t.Fatalf("list: complete=%v err=%v", complete, err)
	}
	var poster frontend.Image
	for _, img := range images {
		if strings.Contains(img.Path, "poster550") {
			t.Fatal("source path leaked")
		}
		if img.Kind == frontend.ImagePoster {
			poster = img
		}
	}
	if poster.Path == "" {
		t.Fatal("poster missing")
	}
	data, ct, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{Width: 300})
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("read: %s %v", ct, err)
	}
	plex := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/poster?width=300")
	if plex.Code != 200 || !bytes.Equal(data, plex.Body.Bytes()) {
		t.Fatal("selected artwork differs from Plex")
	}
	poster.Width, poster.Height, poster.BlurHash = 1200, 1800, "00TI:j"
	if _, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{Width: 300}); err != nil {
		t.Fatalf("metadata changed artwork identity: %v", err)
	}
	restricted := friendUser
	restricted.MaxContentRating = "PG"
	req.Viewer = &restricted
	if _, _, err := source.List(context.Background(), req); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden item listed: %v", err)
	}
	if _, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden item read: %v", err)
	}
	req.Viewer = &ownerUser
	changed := fightClub
	changed.PosterPath = "/different.jpg"
	f.catalog.mu.Lock()
	f.catalog.titles[changed.Key] = changed
	f.catalog.mu.Unlock()
	if _, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stale reference: %v", err)
	}
}

func TestVirtualSeasonArtworkUsesCachedViewerLayout(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	key := core.SeasonKey(breakingBad.Key.TMDB(), 9000)
	virtual := core.Title{Key: key, Type: core.TypeSeason, ShowKey: breakingBad.Key, PosterPath: breakingBad.PosterPath}
	source := FrontendArtwork{Server: f.srv, VirtualSeason: func(_ context.Context, viewer *core.User, requested core.RatingKey) (*core.Title, error) {
		if viewer.ID != ownerUser.ID || requested != key {
			return nil, core.ErrNotFound
		}
		return &virtual, nil
	}}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: key}
	images, complete, err := source.List(context.Background(), req)
	if err != nil || !complete || len(images) == 0 || images[0].Kind != frontend.ImagePoster {
		t.Fatalf("virtual artwork: %+v complete=%v err=%v", images, complete, err)
	}
	if data, _, err := source.Read(context.Background(), req, images[0], frontend.ArtworkTransform{}); err != nil || len(data) == 0 {
		t.Fatalf("read virtual poster: %v", err)
	}
	req.Viewer = &friendUser
	if _, _, err := source.Read(context.Background(), req, images[0], frontend.ArtworkTransform{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other viewer acquired virtual artwork: %v", err)
	}
}

func TestUltraBlurArtworkUsesPlexPalette(t *testing.T) {
	colors := model.UltraBlurColors{TopLeft: "ff0000", TopRight: "00ff00", BottomLeft: "0000ff", BottomRight: "ffffff"}
	data, ct, err := renderUltraBlur(colors, frontend.ArtworkTransform{Width: 2, Height: 2, Format: "png"})
	if err != nil || ct != "image/png" {
		t.Fatal(ct, err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		x, y    int
		r, g, b uint32
	}{{0, 0, 65535, 0, 0}, {1, 0, 0, 65535, 0}, {0, 1, 0, 0, 65535}, {1, 1, 65535, 65535, 65535}} {
		r, g, b, _ := img.At(tc.x, tc.y).RGBA()
		if r != tc.r || g != tc.g || b != tc.b {
			t.Fatalf("corner %d,%d: %d %d %d", tc.x, tc.y, r, g, b)
		}
	}
}

func TestPersonArtworkIsBoundToCreditAndParent(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	images, _, err := source.List(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var portrait frontend.Image
	for _, img := range images {
		if img.Kind == frontend.ImagePerson && img.Subject == "tmdb:287" {
			portrait = img
			break
		}
	}
	if portrait.Path == "" {
		t.Fatal("credit-scoped portrait missing")
	}
	if data, _, err := source.Read(context.Background(), req, portrait, frontend.ArtworkTransform{}); err != nil || len(data) == 0 {
		t.Fatalf("portrait: %v", err)
	}
	forged := portrait
	forged.Subject = "tmdb:819"
	if _, _, err := source.Read(context.Background(), req, forged, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("forged credit: %v", err)
	}
	other := alien
	other.Released = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	f.catalog.mu.Lock()
	f.catalog.titles[other.Key] = other
	f.catalog.mu.Unlock()
	req.Item = other.Key
	if _, _, err := source.Read(context.Background(), req, portrait, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("other title: %v", err)
	}
}

func TestColdArtworkSelectionIsSharedAcrossSurfaces(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var scheduled int
	source := FrontendArtwork{Server: f.srv, Variants: func(*core.Title, string) (*tmdb.Images, bool) { scheduled++; return nil, false }}
	var chosen frontend.Image
	for _, protocol := range []string{"jellyfin", "emby", "silo"} {
		req := frontend.ArtworkRequest{Viewer: &ownerUser, Device: core.Device{ID: protocol, Language: "en"}, Item: fightClub.Key}
		images, complete, err := source.List(context.Background(), req)
		if err != nil || !complete {
			t.Fatalf("%s cold list: %v", protocol, err)
		}
		var poster frontend.Image
		for _, img := range images {
			if img.Kind == frontend.ImagePoster {
				poster = img
				break
			}
		}
		if poster.Path == "" {
			t.Fatalf("%s cold poster missing", protocol)
		}
		if chosen.Path != "" && chosen != poster {
			t.Fatalf("%s selected a different poster", protocol)
		}
		chosen = poster
		data, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{Width: 300})
		plex := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/poster?width=300")
		if err != nil || !bytes.Equal(data, plex.Body.Bytes()) {
			t.Fatalf("%s differs from Plex: %v", protocol, err)
		}
	}
	if scheduled == 0 {
		t.Fatal("cold variants were not scheduled")
	}
}

func TestFrontendArtworkUsesGeneratedCollectionPoster(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	key := core.SmartKey(12)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Owner Picks", Kind: "smart", Type: core.TypeMovie, ChildCount: 1}
	f.cols.items[key] = []core.Title{alien}
	d := f.srv.currentTestDeps()
	d.Collections = &artCollections{fakeCollections: f.cols}
	f.srv = newTestServer(d)
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: key}
	images, complete, err := source.List(context.Background(), req)
	if err != nil || !complete {
		t.Fatalf("collection list: %v", err)
	}
	var thumb frontend.Image
	for _, img := range images {
		if img.Kind == frontend.ImageThumb {
			thumb = img
			break
		}
	}
	if thumb.Path == "" {
		t.Fatal("generated collection thumb missing")
	}
	data, ct, err := source.Read(context.Background(), req, thumb, frontend.ArtworkTransform{Width: 400})
	if err != nil || ct != "image/jpeg" || string(data) != "GENERATED-owner" {
		t.Fatalf("generated image: %q %q %v", data, ct, err)
	}
}

func TestFrontendArtworkVariantsAreScopedAndValidated(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv, Variants: func(*core.Title, string) (*tmdb.Images, bool) {
		return &tmdb.Images{Posters: []tmdb.ImageFile{{FilePath: "/alternative.jpg"}, {FilePath: "https://example.com/image.jpg"}}}, true
	}}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	entries, complete, err := source.List(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("fresh variants did not complete title artwork")
	}
	// Validate the legacy artwork-v1 identity independently of the implementation.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", req.Viewer.ID, req.Item, req.Device.Language, frontend.ImagePoster, "variant\x00/alternative.jpg", "artwork-v1")))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	count := 0
	for _, entry := range entries {
		if strings.Contains(entry.Path, "alternative") || strings.Contains(entry.Path, "example.com") {
			t.Fatal("source leaked")
		}
		if entry.Path != expected {
			continue
		}
		count++
		if data, ct, err := source.Read(context.Background(), req, entry, frontend.ArtworkTransform{Width: 300}); err != nil || len(data) == 0 || ct != "image/jpeg" {
			t.Fatalf("variant: %s %v", ct, err)
		}
	}
	if count != 1 {
		t.Fatalf("valid variant count %d", count)
	}
}

func TestFrontendArtworkTransform(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	images, _, err := source.List(context.Background(), req)
	if err != nil || len(images) == 0 {
		t.Fatal(err)
	}
	data, ct, err := source.Read(context.Background(), req, images[0], frontend.ArtworkTransform{Width: 30, Height: 20, Format: "png", Blur: true})
	if err != nil || ct != "image/png" {
		t.Fatalf("transform: %q %v", ct, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 30 || cfg.Height != 20 {
		t.Fatalf("dimensions: %+v %v", cfg, err)
	}
}
