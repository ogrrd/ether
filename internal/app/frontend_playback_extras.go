package app

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type extraSurface struct{ extras *extras.Service }

func (s extraSurface) Lookup(key core.RatingKey) (frontend.Extra, bool) {
	if s.extras == nil {
		return frontend.Extra{}, false
	}
	clip, ok := s.extras.Lookup(key)
	if !ok {
		return frontend.Extra{}, false
	}
	return frontendExtra(clip), true
}

func (s extraSurface) Open(ctx context.Context, key core.RatingKey) (string, string, map[string]string, error) {
	if s.extras == nil {
		return "", "", nil, core.ErrNotFound
	}
	clip, ok := s.extras.Lookup(key)
	if !ok {
		return "", "", nil, core.ErrNotFound
	}
	resolved, err := s.extras.Resolve(ctx, clip)
	if err != nil {
		return "", "", nil, err
	}
	return resolved.Container, resolved.URL, resolved.Headers, nil
}

func frontendExtra(clip extras.Extra) frontend.Extra {
	return frontend.Extra{Key: clip.Key, ParentKey: clip.ParentKey, Title: clip.Title, Kind: clip.Subtype, Source: clip.Site, SourceID: clip.SiteKey, Language: clip.Language, Official: clip.Official, PublishedAt: clip.PublishedAt, Duration: clip.Duration, Height: clip.Size}
}

func toExtraClip(e extras.Extra) frontend.ExtraClip {
	return frontend.ExtraClip{
		Key:         e.Key,
		ParentKey:   e.ParentKey,
		ParentTitle: e.ParentTitle,
		Title:       e.Title,
		Subtype:     e.Subtype,
		ExtraType:   e.ExtraType,
		Site:        e.Site,
		SiteKey:     e.SiteKey,
		Language:    e.Language,
		Official:    e.Official,
		PublishedAt: e.PublishedAt,
		Size:        e.Size,
		Duration:    e.Duration,
		Source:      e.Source,
	}
}

func (s extraSurface) Enabled() bool {
	return s.extras != nil && s.extras.Enabled()
}

func (s extraSurface) List(ctx context.Context, parent *core.Title, lang string) ([]frontend.ExtraClip, error) {
	if s.extras == nil {
		return nil, nil
	}
	list, err := s.extras.List(ctx, parent, lang)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	out := make([]frontend.ExtraClip, len(list))
	for i, clip := range list {
		out[i] = toExtraClip(clip)
	}
	return out, nil
}

func (s extraSurface) Clip(key core.RatingKey) (frontend.ExtraClip, bool) {
	if s.extras == nil {
		return frontend.ExtraClip{}, false
	}
	clip, ok := s.extras.Lookup(key)
	if !ok {
		return frontend.ExtraClip{}, false
	}
	return toExtraClip(clip), true
}

func (s extraSurface) Resolve(ctx context.Context, e frontend.ExtraClip) (frontend.ResolvedExtra, error) {
	if s.extras == nil {
		return frontend.ResolvedExtra{}, core.ErrNotFound
	}
	clip, ok := s.extras.Lookup(e.Key)
	if !ok {
		clip = extras.Extra{
			Key:         e.Key,
			ParentKey:   e.ParentKey,
			ParentTitle: e.ParentTitle,
			Title:       e.Title,
			Subtype:     e.Subtype,
			ExtraType:   e.ExtraType,
			Site:        e.Site,
			SiteKey:     e.SiteKey,
			Language:    e.Language,
			Official:    e.Official,
			PublishedAt: e.PublishedAt,
			Size:        e.Size,
			Duration:    e.Duration,
			Source:      e.Source,
		}
	}
	resolved, err := s.extras.Resolve(ctx, clip)
	if err != nil {
		return frontend.ResolvedExtra{}, err
	}
	return frontend.ResolvedExtra{
		URL:       resolved.URL,
		Headers:   resolved.Headers,
		Expires:   resolved.Expires,
		Container: resolved.Container,
	}, nil
}

func (s extraSurface) PrewarmBytes() int64 {
	if s.extras == nil {
		return 0
	}
	return s.extras.PrewarmBytes()
}

// cinemaSurface serves the same preroll selection Plex prepends to a movie,
// using viewer-authorized similar and library rows. A title the viewer cannot
// open is omitted, so a restricted viewer never receives its trailer.
type cinemaSurface struct {
	exp    frontend.Experience
	extras *extras.Service
}

func (s cinemaSurface) Trailers(ctx context.Context, viewer *core.Viewer, device core.Device, feature core.RatingKey, limit int) ([]frontend.Extra, error) {
	if s.exp == nil || s.extras == nil || !s.extras.Enabled() || limit <= 0 {
		return nil, nil
	}
	detail, err := s.exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: feature})
	if err != nil {
		return nil, err
	}
	if detail.Title.Type != core.TypeMovie {
		return nil, nil
	}
	titles, err := s.candidates(ctx, viewer, device, feature)
	if err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(titles))
	for _, title := range titles {
		keys = append(keys, title.Key)
	}
	watched := map[core.RatingKey]bool{}
	if len(keys) > 0 {
		status, err := s.exp.UserData(ctx, viewer, keys)
		if err != nil {
			return nil, err
		}
		for key, row := range status {
			watched[key] = row.ViewCount > 0
		}
	}
	candidates := make([]experience.CinemaCandidate, 0, len(titles))
	lookups := 0
	for _, title := range titles {
		if lookups >= 12 {
			break
		}
		if title.Key == feature || title.Type != core.TypeMovie || watched[title.Key] {
			candidates = append(candidates, experience.CinemaCandidate{Title: title, Watched: watched[title.Key] || title.Key == feature})
			continue
		}
		lookups++
		item, err := s.exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: title.Key})
		if err != nil {
			continue
		}
		candidates = append(candidates, experience.CinemaCandidate{Title: item.Title, Extras: item.Extras})
	}
	return experience.SelectCinemaTrailers(feature, candidates, limit), nil
}

func (s cinemaSurface) candidates(ctx context.Context, viewer *core.Viewer, device core.Device, feature core.RatingKey) ([]core.Title, error) {
	page, err := s.exp.Similar(ctx, viewer, device, feature, frontend.ItemQuery{Limit: 24, Types: []core.MediaType{core.TypeMovie}})
	if err != nil {
		return nil, err
	}
	titles := append([]core.Title(nil), page.Page.Items...)
	if len(titles) > 0 {
		return titles, nil
	}
	library, err := s.exp.LibraryItems(ctx, viewer, device, "1", frontend.ItemQuery{Limit: 24, Types: []core.MediaType{core.TypeMovie}, Sort: core.SortPopularity, Desc: true})
	if err != nil {
		return nil, err
	}
	return library.Items, nil
}

type subtitleSurface struct {
	app     *App
	exp     frontend.Experience
	surface string
}

func (s subtitleSurface) Enabled() bool {
	return s.app != nil && s.app.subtitles != nil && s.app.subtitles.Enabled()
}

func (s subtitleSurface) Search(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, language string) ([]frontend.SubtitleOffer, error) {
	if !s.Enabled() || s.exp == nil {
		return nil, nil
	}
	detail, err := s.exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil {
		return nil, err
	}
	release := ""
	if s.app.playback != nil && viewer != nil && device.ID != "" {
		if session := s.app.playSessionOn(s.surface, viewer.ID, device.ID, key); session != nil && session.Resolution != nil {
			release = session.Resolution.Release.Name
		}
	}
	found, err := s.app.subtitles.SearchOnDemand(ctx, &detail.Title, language, release)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.SubtitleOffer, 0, len(found))
	for _, candidate := range found {
		name := candidate.Release
		if name == "" {
			name = candidate.FileName
		}
		lang := candidate.Language
		if lang == "" {
			lang = language
		}
		provider := candidate.Provider
		if provider == "" {
			provider = "OpenSubtitles"
		}
		out = append(out, frontend.SubtitleOffer{
			ID: strconv.FormatInt(candidate.FileID, 10), Language: lang, Name: name,
			Provider: provider, Format: "srt", Author: candidate.Uploader, Downloads: candidate.DownloadCount,
			Rating: candidate.Ratings, HearingImpaired: candidate.HearingImp, Forced: candidate.Forced,
			HashMatch: candidate.HashMatch, AITranslated: candidate.AITranslated,
		})
	}
	return out, nil
}

func (s subtitleSurface) Download(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, offerID string) (int, error) {
	if !s.Enabled() || s.exp == nil {
		return 0, core.ErrNotFound
	}
	fileID, err := strconv.ParseInt(offerID, 10, 64)
	if err != nil || fileID <= 0 {
		return 0, core.ErrNotFound
	}
	detail, err := s.exp.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil {
		return 0, err
	}
	if viewer == nil {
		return 0, core.ErrUnauthorized
	}
	if s.app.db != nil {
		if err := sessions.NewSubtitleIntents(s.app.db).Record(ctx, viewer.ID, key, true); err != nil {
			return 0, err
		}
	}
	info, err := s.app.subtitles.DownloadOnDemand(ctx, &detail.Title, fileID)
	if err != nil {
		return 0, err
	}
	var sess *core.PlaySession
	if viewer != nil {
		sess = s.app.playSessionOn(s.surface, viewer.ID, device.ID, key)
	}
	s.app.startSubtitleCorrection(ctx, &detail.Title, info.ExternalURL, sess)
	embedded := []core.StreamInfo{}
	if s.app.playback != nil {
		embedded = s.app.playback.Estimate(ctx, &detail.Title, core.ClientCaps{ClientID: device.ID, Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, Version: device.Version}).Streams
	}
	for _, stream := range s.app.subtitles.CachedSidecars(&detail.Title, s.app.subtitleUser(ctx, viewer), embedded) {
		if stream.ExternalURL != "" && stream.ExternalURL == info.ExternalURL {
			return stream.Index, nil
		}
	}
	return info.Index, nil
}
