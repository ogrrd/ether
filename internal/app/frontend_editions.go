package app

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// surfaceEditions exposes probed local copies and release variants.
type surfaceEditions struct{ app *App }

func (s surfaceEditions) Editions(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) ([]frontend.Version, error) {
	if s.app == nil {
		return nil, nil
	}
	title, err := s.app.catalog.CachedGet(ctx, key)
	if err != nil || title == nil {
		return nil, nil
	}
	variants, err := localEditionVariants(s.app.opts.DataDir, title)
	if err != nil || len(variants) == 0 {
		return nil, err
	}
	out := make([]frontend.Version, 0, len(variants))
	for _, variant := range variants {
		version := frontendVersion(title, variant)
		if name := probedFileName(variant.Label); name != "" {
			version.FileName = name
		}
		out = append(out, version)
	}
	return out, nil
}

// probedFileName accepts a local edition label only when it is the probed
// file's basename. Release titles and indexer names are not file names.
func probedFileName(label string) string {
	if label == "" || strings.ContainsAny(label, `/\`) {
		return ""
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(label), ".")) {
	case "mkv", "mp4", "m4v", "avi", "mov", "wmv", "webm", "mpg", "mpeg", "ts", "m2ts", "mpegts":
		return label
	default:
		return ""
	}
}

func (s surfaceEditions) EditionsMany(ctx context.Context, _ *core.Viewer, _ core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Version, error) {
	out := map[core.RatingKey][]frontend.Version{}
	if s.app == nil || s.app.catalog == nil || len(keys) == 0 {
		return out, nil
	}
	titles, err := s.app.catalog.CachedGetMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	list := make([]core.Title, 0, len(titles))
	for _, key := range keys {
		title, ok := titles[key]
		if ok && title.Key == key {
			list = append(list, title)
		}
	}
	variants, err := localEditionVariantsMany(s.app.opts.DataDir, list)
	if err != nil {
		return nil, err
	}
	for key, copies := range variants {
		title := titles[key]
		versions := make([]frontend.Version, 0, len(copies))
		for _, variant := range copies {
			version := frontendVersion(&title, variant)
			if name := probedFileName(variant.Label); name != "" {
				version.FileName = name
			}
			versions = append(versions, version)
		}
		out[key] = versions
	}
	return out, nil
}

func (s surfaceEditions) Variants(ctx context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	if s.app == nil || s.app.resolver == nil {
		return nil, nil
	}
	return s.app.resolver.Variants(ctx, req, max)
}

func (s surfaceEditions) RefillSearchCache(t *core.Title) <-chan struct{} {
	if s.app == nil || s.app.resolver == nil {
		return nil
	}
	return s.app.resolver.RefillSearchCache(t)
}

func (s surfaceEditions) Available(ctx context.Context, key core.RatingKey) (bool, bool) {
	if s.app == nil || s.app.resolver == nil {
		return false, false
	}
	return s.app.resolver.Available(ctx, key)
}
