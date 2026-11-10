package plex

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
)

// Owner rule: no title anywhere without a poster. A movie TMDB has no
// poster for gets a thumb URL that serves Ether's generated poster.
func TestPosterlessTitleServesGeneratedPoster(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.GeneratedPosters = artwork.NewRenderer(nil, "", nil) })
	defer f.srv.Close()
	bare := core.Title{Key: core.MovieKey(424242), Type: core.TypeMovie, Title: "Quiet Harbour", Year: 1998, Released: time.Date(1998, 5, 1, 0, 0, 0, 0, time.UTC)}
	f.catalog.titles[bare.Key] = bare
	md := f.srv.metadata(context.Background(), &Request{User: &ownerUser, Product: "Plex Web"}, &bare, renderOpts{})
	if !strings.HasPrefix(md.Thumb, "/library/metadata/"+bare.Key.String()+"/thumb/g") {
		t.Fatalf("thumb = %q, want the generated poster's URL", md.Thumb)
	}
	rec := f.get(t, md.Thumb)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET %s = %d %s", md.Thumb, rec.Code, rec.Header().Get("Content-Type"))
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes())); err != nil || cfg.Height*2 != cfg.Width*3 {
		t.Fatalf("poster %v %v, want a 2:3 image", cfg, err)
	}
}
