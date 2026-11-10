package plex

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/go-chi/chi/v5"
)

// DiscoveryFeatures is the shared discovery contract adapted by Plex.
type DiscoveryFeatures = frontend.DiscoveryFeatures

func (s *Server) discoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	d := s.discovery()
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(purpose frontend.RowPurpose, load func(context.Context, *frontend.User, int) ([]frontend.Hub, error)) {
		var hubs []frontend.Hub
		selector := s.exp().NewDiscoverySelection(frontend.DiscoverySelectionOptions{Visibility: s.visibility(), Unrestricted: func(u *frontend.User) bool { return !s.viewerRestricted(u) }})
		rows := selector.Load(ctx, pr.User, limit, func(ctx context.Context, fetch int) []frontend.DiscoveryCandidate {
			hubs, _ = load(ctx, pr.User, fetch)
			candidates := make([]frontend.DiscoveryCandidate, len(hubs))
			for i, hub := range hubs {
				candidates[i] = frontend.DiscoveryCandidate{Index: i, Purpose: purpose, Items: hub.Items}
			}
			return candidates
		})
		for _, row := range rows {
			h := hubs[row.Index]
			h.Items = row.Items
			out = append(out, hubSpec{Hub: h})
		}
	}
	if d.Watchlist != nil {
		add(frontend.RowPurpose("watchlist"), d.Watchlist.Hubs)
	}
	if d.Personal != nil {
		add(frontend.RowPurpose("recommendation"), d.Personal.Hubs)
	}
	return out
}
func (s *Server) discoveryRemoveContinue(ctx context.Context, pr *Request, key frontend.RatingKey) error {
	if s.discovery() == nil || s.discovery().Personal == nil || pr.User == nil {
		return nil
	}
	t, e := s.catalog().Get(ctx, key)
	if e != nil {
		return e
	}
	if !s.visibleTitle(ctx, pr.User, t) {
		return frontend.ErrNotFound
	}
	return s.discovery().Personal.RemoveContinue(ctx, pr.User.ID, key)
}

func (s *Server) decorateDiscoveryMetadata(ctx context.Context, pr *Request, t *frontend.Title, m *model.Metadata, detail bool) {
	d := s.discovery()
	if d == nil || t == nil || pr.User == nil || !s.titleAllowed(pr.User, t) {
		return
	}
	selected := d.SelectArtwork(ctx, pr.User, t, pr.Language)
	localized := selected.Title
	if localized != nil && localized != t {
		m.Title = localized.Title
		m.Summary = localized.Summary
		m.Tagline = localized.Tagline
	}
	if d.OverviewTranslate != nil && pr.Language != "" {
		if summary, tagline, ok := d.OverviewTranslate(ctx, t, pr.Language, detail); ok {
			if summary != "" {
				m.Summary = summary
			}
			if tagline != "" {
				m.Tagline = tagline
			}
		}
	}
	if selected.PosterVersion != "" {
		m.Thumb = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?lang=%s", t.Key.String(), selected.PosterVersion, url.QueryEscape(pr.Language))
	}
	if selected.BackdropVersion != "" {
		m.Art = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=backdrop&lang=%s", t.Key.String(), selected.BackdropVersion, url.QueryEscape(pr.Language))
	}
	if selected.Enabled {
		hasLogo := false
		for i := range m.Image {
			m.Image[i].Alt = m.Title
			switch m.Image[i].Type {
			case "coverPoster":
				if t.Type != frontend.TypeEpisode && m.Thumb != "" {
					m.Image[i].URL = m.Thumb
				}
			case "background":
				if m.Art != "" {
					m.Image[i].URL = m.Art
				}
			case "clearLogo":
				hasLogo = true
				if selected.LogoVersion != "" {
					m.Image[i].URL = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), selected.LogoVersion, url.QueryEscape(pr.Language))
				}
			}
		}
		if !hasLogo && selected.LogoVersion != "" {
			m.Image = append(m.Image, model.Image{Alt: m.Title, Type: "clearLogo", URL: fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), selected.LogoVersion, url.QueryEscape(pr.Language))})
		}
	}
	if d.Ratings != nil && (t.Type == frontend.TypeMovie || t.Type == frontend.TypeShow) {
		bs, e := d.Ratings.Cached(ctx, t)
		cfg, ce := d.Ratings.Settings(ctx)
		if e == nil && ce == nil && cfg.Enabled {
			d.Ratings.Queue(*t)
			if cfg.Audience != "" && cfg.Audience != "none" && cfg.Missing == "tmdb" {
				m.AudienceRating, m.AudienceRatingImage = 0, ""
				if t.VoteCount > 0 && t.Rating > 0 && t.Rating <= 10 {
					m.AudienceRating, m.AudienceRatingImage = math.Round(t.Rating*10)/10, "themoviedb://image.rating"
				}
			}
			if cfg.Audience == "none" || (cfg.Audience != "" && cfg.Missing == "none") {
				m.AudienceRating, m.AudienceRatingImage = 0, ""
			}
			if cfg.Critic != "" && cfg.Critic != "none" && cfg.Missing == "tmdb" {
				m.Rating, m.RatingImage = 0, ""
				if t.VoteCount > 0 && t.Rating > 0 && t.Rating <= 10 {
					m.Rating, m.RatingImage = math.Round(t.Rating*10)/10, "themoviedb://image.rating"
				}
			}
			if cfg.Critic == "none" || (cfg.Critic != "" && cfg.Missing == "none") {
				m.Rating, m.RatingImage = 0, ""
			}
			for _, b := range bs {
				if b.Source == cfg.Critic && b.Image != "" {
					m.Rating = b.Value
					m.RatingImage = b.Image
				}
				if b.Source == cfg.Audience && b.Image != "" {
					m.AudienceRating = b.Value
					m.AudienceRatingImage = b.Image
				}
			}
		}
		if e == nil && ce == nil && cfg.Enabled && (len(bs) > 0 || cfg.Audience == "none" || cfg.Missing != "") && len(m.Rating_) > 0 {
			m.Rating_ = nil
			if m.RatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "critic", Image: m.RatingImage, Value: m.Rating})
			}
			if m.AudienceRatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "audience", Image: m.AudienceRatingImage, Value: m.AudienceRating})
			}
		}

	}
	if d.LLM != nil && d.Watchlist != nil && (t.Type == frontend.TypeSeason || t.Type == frontend.TypeEpisode) {
		if d.Watchlist.RecapsEnabled(ctx) {
			entry := d.CachedRecap(pr.User, t, s.state(), s.now())
			if entry != "" {
				m.Summary = "Previously watched: " + entry + "\n\n" + m.Summary
			}

		}
	}
}
func (s *Server) handleDiscoveryPoster(w http.ResponseWriter, r *http.Request) {
	k, e := frontend.ParseRatingKey(chi.URLParam(r, "id"))
	if e != nil {
		writeHTTPError(w, 400)
		return
	}
	q := r.URL.Query()
	s.serveDiscoveryPoster(w, r, k, q.Get("kind"), q.Get("lang"))
}

// serveDiscoveryPoster also serves Plex's photo transcode wrapper. Keep the
// authenticated outer request: credentials inside an artwork URL grant nothing.
func (s *Server) serveDiscoveryPoster(w http.ResponseWriter, r *http.Request, k frontend.RatingKey, kind, lang string) {
	d := s.discovery()
	if d == nil || d.Overlays == nil {
		writeHTTPError(w, 404)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	t, e := s.artworkTitle(ctx, k)
	if e != nil || pr.User == nil || !s.visibleTitle(ctx, pr.User, t) {
		writeHTTPError(w, 404)
		return
	}
	if lang == "" {
		lang = pr.Language
	}
	t, version, e := d.PrepareArtwork(ctx, pr.User, t, lang)
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	backdropWidth, _ := strconv.Atoi(r.URL.Query().Get("width"))
	highBackdrop := kind == "backdrop" && backdropWidth > 1280 && backdropWidth <= 4000
	etag := `"` + version + `"`
	if highBackdrop {
		etag = fmt.Sprintf("\"%s-w%d\"", version, backdropWidth)
	}
	if format := androidPhotoOutputFormat(r); format != "" {
		etag = strings.TrimSuffix(etag, `"`) + "-" + format + `"`
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Vary", "X-Plex-Token, X-Plex-Language")
	w.Header().Set("ETag", etag)
	if !d.Overlays.CustomPostersEnabled() && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var data []byte
	var ct string
	if highBackdrop {
		_, path := d.Overlays.Paths(ctx, pr.User, t, lang)
		image, err := s.images.Get(ctx, s.d.ImageURL(path, tmdbSize(backdropWidth)), s.now())
		e = err
		if err == nil {
			data, ct = image.Data, image.ContentType
		}
	} else {
		data, ct, e = d.RenderArtwork(ctx, pr.User, t, kind, lang)
	}
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	if d.Overlays.CustomPostersEnabled() {
		if width, _ := strconv.Atoi(r.URL.Query().Get("width")); width > 0 && (kind == "" || kind == "poster") {
			if resized, err := s.d.ArtworkStores.ResizePoster(data, width); err == nil {
				data = resized
				ct = "image/jpeg"
			}
		}
	}
	data, ct, e = s.androidPhotoFormat(r, data, ct)
	if e != nil {
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	if plexAndroidClient(pr) {
		w.Header().Add("Vary", "X-Plex-Product")
	}
	if d.Overlays.CustomPostersEnabled() {
		etag = fmt.Sprintf("\"%x\"", sha256.Sum256(data))
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	setArtworkHeaders(w.Header(), ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
