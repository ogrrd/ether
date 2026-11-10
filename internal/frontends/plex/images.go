package plex

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

// setArtworkHeaders keeps remote SVG (or an upstream HTML error page) from
// executing on Ether's origin when opened directly. Plex and the owner UI share
// an origin, and the latter stores its owner credential in localStorage. Image
// requests can carry a viewer's query token, so viewer authentication alone does
// not make an active artwork document safe for the owner's browser.
func setArtworkHeaders(h http.Header, contentType string) {
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
}

// ---------------------------------------------------------------------------
// Artwork cache: an in-memory LRU of fetched TMDB images, with request
// coalescing so a grid of posters is fetched once.

func tmdbSize(w int) string { return kitimages.TMDBSize(w) }

var tmdbSizeRe = regexp.MustCompile(`/t/p/(w\d+|h\d+|original)/`)

// resizeTMDBURL rewrites the size segment of a TMDB image URL.
func resizeTMDBURL(u string, width int) string {
	if width <= 0 || !tmdbSizeRe.MatchString(u) {
		return u
	}
	return tmdbSizeRe.ReplaceAllString(u, "/t/p/"+tmdbSize(width)+"/")
}

// ---------------------------------------------------------------------------
// Handlers

// artPath picks the TMDB path for an item's element.
type refreshingArtworkCatalog struct{ frontend.Catalog }

// memoryTitles is the catalogue's read of titles held without a row
// (catalog.MemoryTitle).
type memoryTitles interface {
	MemoryTitle(context.Context, frontend.RatingKey) (*frontend.Title, bool, error)
}

// StoredTitle reads a stored title through Get, which refreshes a stale
// row. A search or list hit without a row is read from memory: fetching its
// thumbnail must not promote it into the library (Get would).
func (c refreshingArtworkCatalog) StoredTitle(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
	if m, ok := c.Catalog.(memoryTitles); ok {
		t, stored, err := m.MemoryTitle(ctx, key)
		switch {
		case err != nil:
			return nil, err
		case t != nil:
			return t, nil
		case !stored:
			return nil, frontend.ErrNotFound
		}
	}
	return c.Get(ctx, key)
}

// UndatedSearchHit forwards catalog.UndatedSearchHit to the artwork source.
func (c refreshingArtworkCatalog) UndatedSearchHit(ctx context.Context, key frontend.RatingKey) bool {
	u, ok := c.Catalog.(interface {
		UndatedSearchHit(context.Context, frontend.RatingKey) bool
	})
	return ok && u.UndatedSearchHit(ctx, key)
}

// serveGeneratedPoster serves Ether's poster for a movie or show the viewer
// may see that has no artwork. Seasons and episodes point at their show's
// thumb, so they reach this through the show.
func (s *Server) serveGeneratedPoster(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) bool {
	if s.d.GeneratedPosters == nil || (element != "thumb" && element != "poster") || (key.Kind() != frontend.KindMovie && key.Kind() != frontend.KindShow) {
		return false
	}
	ctx := r.Context()
	t, err := (refreshingArtworkCatalog{s.catalog()}).StoredTitle(ctx, key)
	if err != nil || t == nil || t.Key != key || t.Adult || t.IsFutureRelease(s.now()) || !s.visibleTitle(ctx, FromContext(ctx).User, t) {
		return false
	}
	data, ct, ok, err := s.d.ArtworkStores.RenderTitlePoster(ctx, s.d.GeneratedPosters, t, width)
	if !ok || err != nil {
		return false
	}
	if data, ct, err = s.androidPhotoFormat(r, data, ct); err != nil {
		return false
	}
	h := w.Header()
	setArtworkHeaders(h, ct)
	if plexAndroidClient(FromContext(ctx)) {
		h.Add("Vary", "X-Plex-Product")
	}
	h.Set("Cache-Control", "max-age=604800, private")
	h.Add("Vary", "X-Plex-Token")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

func (s *Server) artPath(ctx context.Context, key frontend.RatingKey, element string) (string, int, error) {
	cfg := (FrontendArtwork{Server: s}).cfg()
	cfg.Catalog = refreshingArtworkCatalog{s.catalog()}
	pr := FromContext(ctx)
	cfg.CollectionDevice = pr.Device
	return s.d.ArtworkStores.NewSource(cfg).ArtPath(ctx, frontend.ArtworkRequest{Viewer: pr.User, Item: key, Device: frontend.Device{Product: pr.Product, Platform: pr.Platform}}, element)
}

// handleImage serves /library/metadata/{key}/{element}[/{ts}] and the
// collection/playlist equivalents.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	key, err := frontend.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	element := chi.URLParam(r, "element")
	switch element {
	case "thumb", "art", "poster", "banner", "clearLogo", "logo", "background", "composite", "squareArt":
	default:
		s.handleUnknown(w, r)
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	s.serveItemImage(w, r, key, element, width)
}

func (s *Server) serveItemImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) {
	if key.IsPreroll() && s.prerollThumb(w, r, key) {
		return
	}
	if element == "composite" || element == "squareArt" {
		element = "thumb"
	}
	raw := r.URL.Query().Get("collection")
	if raw == "" && s.serveCollectionArtwork(w, r, key, element, width) {
		return
	}
	path, defWidth, err := s.artPath(r.Context(), key, element)
	if raw != "" || err != nil || path == "" {
		t, _, e := s.lookupPlaceholder(r.Context(), FromContext(r.Context()), key, raw)
		if e != nil && raw == "" {
			t, e = s.scheduledEpisode(r.Context(), FromContext(r.Context()), key)
		}
		if e != nil {
			if s.servePlexOverlayImage(w, r, key, element) || (raw == "" && s.serveGeneratedPoster(w, r, key, element, width)) {
				return
			}
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		path := t.PosterPath
		if t.Type == frontend.TypeEpisode {
			path = t.StillPath
		}
		if element == "art" || element == "background" {
			path = t.BackdropPath
		}
		if element != "thumb" && element != "poster" && element != "art" && element != "background" {
			path = ""
		}
		if path == "" {
			if s.servePlexOverlayImage(w, r, key, element) || (raw == "" && s.serveGeneratedPoster(w, r, key, element, width)) {
				return
			}
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if width <= 0 {
			if element == "art" || element == "background" {
				width = 1280
			} else {
				width = 500
			}
		}
		s.serveRemoteImage(w, r, s.d.ImageURL(path, tmdbSize(width)))
		return
	}
	if width <= 0 {
		width = defWidth
	}
	s.serveRemoteImage(w, r, s.d.ImageURL(path, tmdbSize(width)))
}

// servePlexOverlayImage is the last-resort artwork path: only when the owner
// toggle is on and this key already has a cached overlay URL for the element.
func (s *Server) servePlexOverlayImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string) bool {
	remote := s.plexMetaRemoteImage(r.Context(), key, element)
	if remote == "" {
		return false
	}
	// This runs when the catalogue art path refused or had nothing, which
	// includes titles this viewer may not see: check before serving.
	t, err := s.catalog().Get(r.Context(), key)
	if err != nil || t == nil || t.Adult || !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		return false
	}
	if err := s.writeRemoteImage(w, r, remote); err != nil {
		writeHTTPError(w, http.StatusNotFound)
	}
	return true
}

func (s *Server) serveRemoteImage(w http.ResponseWriter, r *http.Request, u string) {
	if err := s.writeRemoteImage(w, r, u); err != nil {
		if s.images != nil && s.images.IsNotFound(err) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if r.Context().Err() == nil {
			s.log.Debug("image fetch failed", "url", u, "err", err)
		}
		writeHTTPError(w, http.StatusBadGateway)
	}
}

func (s *Server) writeRemoteImage(w http.ResponseWriter, r *http.Request, u string) error {
	img, err := s.images.Get(r.Context(), u, s.now())
	if err != nil {
		return err
	}
	data, ct, err := s.androidPhotoFormat(r, img.Data, img.ContentType)
	if err != nil {
		return err
	}
	h := w.Header()
	setArtworkHeaders(h, ct)
	if plexAndroidClient(FromContext(r.Context())) {
		h.Add("Vary", "X-Plex-Product")
	}
	h.Set("Cache-Control", "max-age=604800, private")
	h.Add("Vary", "X-Plex-Token")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return nil
}

// Person artwork is scoped to one title so authorization requires one catalog lookup.
func (s *Server) servePersonImage(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, kind, identity string) {
	if r.Context().Err() != nil {
		return
	}
	t, err := s.artworkTitle(r.Context(), key)
	if r.Context().Err() != nil || err != nil || t == nil || !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var people []frontend.Person
	switch kind {
	case "actor":
		people = t.Cast
	case "director":
		people = t.Directors
	case "writer":
		people = t.Writers
	default:
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(identity)
	if err != nil || len(raw) == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var imagePath string
	for _, person := range people {
		if person.ThumbPath == string(raw) {
			imagePath = person.ThumbPath
			break
		}
	}
	if imagePath == "" {
		if ov, ok := s.plexMetaCached(r.Context(), key); ok {
			for _, person := range overlayPeople(ov, kind) {
				if person.Thumb == string(raw) {
					imagePath = person.Thumb
					break
				}
			}
		}
	}
	if imagePath == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if plexImageURL(imagePath) {
		s.serveRemoteImage(w, r, imagePath)
		return
	}
	// Keep the same w185 profile URL emitted by metadata before proxying; poster widths are not profile configuration. Width and height are intentionally ignored.
	s.serveRemoteImage(w, r, s.d.ImageURL(imagePath, "w185"))
}
func (s *Server) handlePersonImage(w http.ResponseWriter, r *http.Request) {
	key, err := frontend.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.servePersonImage(w, r, key, chi.URLParam(r, "kind"), chi.URLParam(r, "identity"))
}

var personImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/person/(actor|director|writer)/([A-Za-z0-9_-]+)$`)

var metadataImageRe = regexp.MustCompile(`^/library/(?:metadata|collections)/(\d+)/(thumb|art|poster|banner|clearLogo|composite|squareArt)(?:/\d+)?$`)
var playlistImageRe = regexp.MustCompile(`^/playlists/(\d+)/(composite|thumb|art)(?:/\d+)?$`)
var discoveryImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/discoveryPoster/[^/]+$`)

// handlePhotoTranscode serves /photo/:/transcode. The url parameter may be
// one of our own artwork paths (relative, or absolute on this server) or an
// absolute TMDB image URL. Ether does no pixel work: it serves the TMDB
// variant closest to the requested width.
func (s *Server) handlePhotoTranscode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// PMS validates dimensions before resolving the image.
	for _, name := range []string{"width", "height"} {
		if q.Has(name) {
			if _, err := strconv.Atoi(photoQueryValue(q, name)); err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
	}
	// Validate even options handled by the upstream image service.
	for _, name := range []string{"minSize", "upscale", "opacity", "quality", "blur"} {
		if raw, present := firstQuery(q, name); present {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || ((name == "minSize" || name == "upscale") && n != 0 && n != 1) {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			if name == "blur" && n < 0 {
				writeHTTPError(w, http.StatusInternalServerError)
				return
			}
		}
	}
	raw := photoQueryValue(q, "url")
	if !q.Has("url") {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if raw == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	width, _ := strconv.Atoi(photoQueryValue(q, "width"))
	height, _ := strconv.Atoi(photoQueryValue(q, "height"))
	if width <= 0 && height > 0 {
		width = height * 2 / 3 // poster aspect
	} else if strings.HasSuffix(photoQueryValue(q, "minSize"), "1") && height > 0 && height*2/3 > width {
		width = height * 2 / 3
	}
	u, err := url.Parse(raw)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if u.IsAbs() && s.allowedImageHost(u.Host) {
		if !s.remoteArtworkAllowed(r, u) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.serveRemoteImage(w, r, resizeTMDBURL(u.String(), width))
		return
	}
	// Android may wrap an artwork URL from a previous local/remote/relay
	// connection. Resolve only known server artwork routes under the OUTER
	// viewer; never dial that hostname or use credentials from the nested URL.
	androidLocal := plexAndroidClient(FromContext(r.Context())) && (u.Scheme == "http" || u.Scheme == "https") &&
		(metadataImageRe.MatchString(u.Path) || discoveryImageRe.MatchString(u.Path) || personImageRe.MatchString(u.Path) || playlistImageRe.MatchString(u.Path) || liveChannelLogoRe.MatchString(u.Path))
	if !u.IsAbs() || s.isSelfHost(r, u.Host) || androidLocal {
		path := u.Path
		if m := liveChannelLogoRe.FindStringSubmatch(path); m != nil {
			s.serveLiveLogo(w, r, m[1], true)
			return
		}
		if m := discoveryImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			params := u.Query()
			r = r.Clone(r.Context())
			effective := r.URL.Query()
			effective.Set("width", strconv.Itoa(width))
			r.URL.RawQuery = effective.Encode()
			s.serveDiscoveryPoster(w, r, key, params.Get("kind"), params.Get("lang"))
			return
		}
		if m := personImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.servePersonImage(w, r, key, m[2], m[3])
			return
		}
		if m := metadataImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			if collection := u.Query().Get("collection"); collection != "" {
				r = r.Clone(r.Context())
				q := r.URL.Query()
				q.Set("collection", collection)
				r.URL.RawQuery = q.Encode()
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		if m := playlistImageRe.FindStringSubmatch(path); m != nil {
			key, err := frontend.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if !s.allowedImageHost(u.Host) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	s.serveRemoteImage(w, r, resizeTMDBURL(u.String(), width))
}

var liveChannelLogoRe = regexp.MustCompile(`^/livetv/channels/([^/]+)/logo\.(?:svg|png)$`)

// isSelfHost reports whether host refers to this server.
func (s *Server) isSelfHost(r *http.Request, host string) bool {
	h := strings.ToLower(host)
	if h == "" || strings.EqualFold(h, r.Host) {
		return true
	}
	name := h
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.HasSuffix(h, "]") {
		name = h[:i]
	}
	name = strings.Trim(name, "[]")
	return name == "localhost" || name == "127.0.0.1" || name == "::1" || strings.HasSuffix(name, ".plex.direct")
}

// allowedImageHost limits the proxy to the artwork CDN, so it is not an
// open proxy.
func (s *Server) allowedImageHost(host string) bool {
	if frontend.MetadataImageHost(host) {
		return true
	}
	ref, err := url.Parse(s.d.ImageURL("/x.jpg", "original"))
	if err != nil {
		return false
	}
	return strings.EqualFold(ref.Host, host)
}

// photoQueryValue follows PMS duplicate-parameter semantics, including an empty
// final value.
func photoQueryValue(q url.Values, name string) string {
	vs := q[name]
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// Honor explicit Android formats through the shared, byte-bounded variant
// cache. Authorization remains in the source handler, before cache lookup.
func (s *Server) androidPhotoFormat(r *http.Request, data []byte, ct string) ([]byte, string, error) {
	return s.images.RasterFormat(r.Context(), data, ct, androidPhotoOutputFormat(r), s.now())
}

func androidPhotoOutputFormat(r *http.Request) string {
	if !plexAndroidClient(FromContext(r.Context())) || r.URL.Path != "/photo/:/transcode" {
		return ""
	}
	format := strings.ToLower(photoQueryValue(r.URL.Query(), "format"))
	if format == "jpg" {
		format = "jpeg"
	}
	if format == "jpeg" || format == "png" {
		return format
	}
	return ""
}
