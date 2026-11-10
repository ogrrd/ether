package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// FrontendArtwork is a compatibility adapter for callers already holding a Plex
// server. Shared runtime constructs artworkservice.Source directly.
type FrontendArtwork struct {
	Server        *Server
	Variants      func(*frontend.Title, string) (*frontend.ArtworkImages, bool)
	Metadata      func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image
	VirtualSeason func(context.Context, *frontend.User, frontend.RatingKey) (*frontend.Title, error)
	URLImages     interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	Raster frontend.SVGRasterizer
}

func (a FrontendArtwork) cfg() frontend.ArtworkSourceConfig {
	if a.Server == nil {
		return frontend.ArtworkSourceConfig{}
	}
	s := a.Server
	stored, _ := s.catalog().(frontend.StoredTitleLookup)
	return frontend.ArtworkSourceConfig{
		Catalog:       stored,
		Collections:   s.collections(),
		Playlists:     s.playlists(),
		Visibility:    s.visibility(),
		LiveTV:        s.d.LiveTV,
		Discovery:     s.discovery(),
		Images:        s.images,
		ImageURL:      s.d.ImageURL,
		Palettes:      s.ultra,
		Now:           s.now,
		Variants:      a.Variants,
		Metadata:      a.Metadata,
		VirtualSeason: a.VirtualSeason,
		URLImages:     a.URLImages,
		Raster:        a.Raster,
		Generated:     s.d.GeneratedPosters,
	}
}

func (a FrontendArtwork) source() frontend.ArtworkSource {
	if a.Server == nil || a.Server.d.ArtworkStores == nil {
		return nil
	}
	return a.Server.d.ArtworkStores.NewSource(a.cfg())
}

func (a FrontendArtwork) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	src := a.source()
	if src == nil {
		return nil, false, frontend.ErrUnauthorized
	}
	return src.List(ctx, req)
}

func (a FrontendArtwork) Read(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image, tr frontend.ArtworkTransform) ([]byte, string, error) {
	src := a.source()
	if src == nil {
		return nil, "", frontend.ErrUnauthorized
	}
	return src.Read(ctx, req, img, tr)
}

func (a FrontendArtwork) Warm(t frontend.Title, d frontend.Device) {
	if src := a.source(); src != nil {
		src.Warm(t, d)
	}
}

func (a FrontendArtwork) ChannelImage(v *frontend.Viewer, c frontend.LiveTVChannel) frontend.Image {
	src := a.source()
	if src == nil {
		return frontend.Image{}
	}
	return src.ChannelImage(v, c)
}

type storedPolicyKey struct{}

// withStoredPolicy tells a hub list to read the parent show from the mirror.
// A provider refresh of one series must not hold Recommended.
func withStoredPolicy(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, storedPolicyKey{}, true)
}

// Ordinary Plex routes retain their existing catalogue Get refresh behavior.
// Hub lists use the mirrored show for the playable check.
func (s *Server) artworkTitle(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
	if ctx != nil && ctx.Value(storedPolicyKey{}) != nil {
		if cached, ok := s.catalog().(interface {
			CachedGet(context.Context, frontend.RatingKey) (*frontend.Title, error)
		}); ok {
			return cached.CachedGet(ctx, key)
		}
	}
	return s.catalog().Get(ctx, key)
}

var _ frontend.Artwork = FrontendArtwork{}
