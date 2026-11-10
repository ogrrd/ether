package plex

import (
	"context"
	"net/url"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type plexMetaCtxKey struct{}

type plexMetaBatch struct {
	rows  map[frontend.RatingKey]frontend.CachedMetadataOverlay
	guids map[frontend.RatingKey]string
}

func (s *Server) plexMetaOn() bool {
	if s == nil || s.d.LibraryMetadata == nil || s.d.Config == nil {
		return false
	}
	cfg := s.d.Config()
	return cfg.Plex.Claimed && cfg.Plex.UsePlexMetadata
}

func (s *Server) withPlexMeta(ctx context.Context, keys []frontend.RatingKey, guids map[frontend.RatingKey]string) context.Context {
	if !s.plexMetaOn() {
		return ctx
	}
	cached := s.d.LibraryMetadata.CachedMany(ctx, keys)
	now := s.now()
	refresh := make([]frontend.RatingKey, 0, len(keys))
	seen := map[frontend.RatingKey]bool{}
	for _, k := range keys {
		if k == 0 || seen[k] {
			continue
		}
		seen[k] = true
		guid := guids[k]
		if guid == "" {
			continue
		}
		row, ok := cached[k]
		if !ok || row.Stale(now) || row.GUID != guid {
			refresh = append(refresh, k)
		}
	}
	s.d.LibraryMetadata.Schedule(refresh...)
	return context.WithValue(ctx, plexMetaCtxKey{}, plexMetaBatch{rows: cached, guids: guids})
}

func (s *Server) plexMetaCached(ctx context.Context, k frontend.RatingKey) (frontend.CachedMetadataOverlay, bool) {
	if !s.plexMetaOn() || k == 0 {
		return frontend.CachedMetadataOverlay{}, false
	}
	var (
		row  frontend.CachedMetadataOverlay
		ok   bool
		guid string
	)
	if batch, has := ctx.Value(plexMetaCtxKey{}).(plexMetaBatch); has {
		row, ok = batch.rows[k]
		guid = batch.guids[k]
	} else {
		row, ok = s.d.LibraryMetadata.Cached(ctx, k)
		if s.d.LibraryGUIDs != nil {
			guid = s.d.LibraryGUIDs.Cached(ctx, k)
		}
	}
	if !ok || row.Empty() || guid == "" || row.GUID != guid {
		return frontend.CachedMetadataOverlay{}, false
	}
	return row, true
}

func (s *Server) plexMetaOverlay(ctx context.Context, k frontend.RatingKey) (frontend.CachedMetadataOverlay, bool) {
	return s.plexMetaCached(ctx, k)
}

func (s *Server) plexMetaRatingAllowed(u *frontend.Viewer, t *frontend.Title) bool {
	if !s.needPlexRatingFilter(u) || t == nil {
		return true
	}
	row, ok := s.plexMetaCached(context.Background(), t.Key)
	return s.plexMetaRatingOK(u, t, row, ok)
}

func (s *Server) needPlexRatingFilter(u *frontend.Viewer) bool {
	return u != nil && u.MaxContentRating != "" && s.plexMetaOn()
}

func (s *Server) plexMetaRatingOK(u *frontend.Viewer, t *frontend.Title, row frontend.CachedMetadataOverlay, ok bool) bool {
	if !ok {
		return true
	}
	return frontend.MetadataOverlayRatingAllowed(u, t.ContentRating, row.ContentRating)
}

func (s *Server) plexMetaRatingAllowedCached(u *frontend.Viewer, t *frontend.Title, rows map[frontend.RatingKey]frontend.CachedMetadataOverlay, guids map[frontend.RatingKey]string) bool {
	if !s.needPlexRatingFilter(u) || t == nil {
		return true
	}
	guid := guids[t.Key]
	row, ok := rows[t.Key]
	if !ok || row.Empty() || guid == "" || row.GUID != guid {
		return true
	}
	return s.plexMetaRatingOK(u, t, row, true)
}

func (s *Server) plexMetaRemoteImage(ctx context.Context, key frontend.RatingKey, element string) string {
	switch key.Kind() {
	case frontend.KindMovie, frontend.KindShow, frontend.KindSeason, frontend.KindEpisode:
	default:
		return ""
	}
	o, ok := s.plexMetaCached(ctx, key)
	if !ok {
		return ""
	}
	switch element {
	case "thumb", "poster":
		return o.ThumbURL
	case "art", "background":
		return o.ArtURL
	}
	return ""
}

func plexImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	return frontend.MetadataImageHost(u.Host)
}

func overlayPeople(o frontend.CachedMetadataOverlay, kind string) []frontend.MetadataPerson {
	switch kind {
	case "actor":
		return o.Cast
	case "director":
		return o.Directors
	case "writer":
		return o.Writers
	}
	return nil
}

func applyPlexMetaTitle(t *frontend.Title, o frontend.MetadataOverlay) *frontend.Title {
	cp := *t
	frontend.ApplyMetadataDisplay(&cp, o)
	return &cp
}

func applyPlexMetaDates(t *frontend.Title, m *model.Metadata, o frontend.MetadataOverlay) {
	if o.AvailableAt.IsZero() && o.Year == 0 {
		return
	}
	if t.Type == frontend.TypeSeason {
		return
	}
	if !o.AvailableAt.IsZero() {
		m.OriginallyAvailableAt = o.AvailableAt.Format("2006-01-02")
	}
	if t.Type == frontend.TypeEpisode {
		return
	}
	if o.Year > 0 {
		m.Year = o.Year
	} else if !o.AvailableAt.IsZero() {
		m.Year = o.AvailableAt.Year()
	}
}
