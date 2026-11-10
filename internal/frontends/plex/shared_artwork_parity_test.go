package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/overlays"
)

func TestSharedArtworkDiscoveryIdentityAndBytesMatchPlex(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	ov := overlays.New(discoveryDB(t), overlays.Options{Images: &discoveryImageSource{}})
	if err := ov.SaveSettings(ctx, overlays.Settings{Enabled: true, Localize: true}); err != nil {
		t.Fatal(err)
	}
	f.srv.setDiscovery((&discovery.Features{Overlays: ov, Localize: func(_ context.Context, title *core.Title, lang string) (*core.Title, error) {
		cp := *title
		cp.PosterPath = "/" + lang + "-poster.jpg"
		return &cp, nil
	}}).Frontend())
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key, Device: core.Device{Language: "fr"}}
	source := (FrontendArtwork{Server: f.srv}).source()
	images, _, err := source.List(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	md := f.srv.metadata(ctx, &Request{User: &ownerUser, Language: "fr"}, &fightClub, renderOpts{})
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", ownerUser.ID, fightClub.Key, "fr", frontend.ImagePoster, md.Thumb, "artwork-v1")))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	var poster frontend.Image
	for _, image := range images {
		if image.Kind == frontend.ImagePoster {
			poster = image
			break
		}
	}
	if poster.Path != expected || poster.Tag != expected {
		t.Fatalf("opaque identity changed: %+v want %s", poster, expected)
	}
	got, ct, err := source.Read(ctx, req, poster, frontend.ArtworkTransform{Width: 300})
	if err != nil {
		t.Fatal(err)
	}
	direct := f.get(t, md.Thumb+"&width=300")
	if direct.Code != 200 || ct != direct.Header().Get("Content-Type") || !bytes.Equal(got, direct.Body.Bytes()) {
		t.Fatalf("shared discovery differs: %q %s versus %q %d", got, ct, direct.Body.Bytes(), direct.Code)
	}
}
