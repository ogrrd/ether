package plex

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// CollectionArtworker is optionally implemented by Deps.Collections to
// serve generated collection posters (internal/collections renders them
// with internal/artwork). ok=false falls back to the TMDB artwork path.
type CollectionArtworker interface {
	CollectionArtwork(ctx context.Context, user *frontend.Viewer, key frontend.RatingKey, element string, width int) (data []byte, contentType string, ok bool, err error)
}

// serveCollectionArtwork answers an image request for a collection with
// its generated poster, reporting whether it did.
func (s *Server) serveCollectionArtwork(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) bool {
	if key.Kind() != frontend.KindSmart {
		return false
	}
	art, ok := s.collections().(CollectionArtworker)
	if !ok {
		return false
	}
	// Same visibility as the collection itself; on failure the TMDB path
	// below answers (404 for a collection the viewer may not see).
	if _, err := s.collection(r.Context(), FromContext(r.Context()).User, key); err != nil {
		return false
	}
	data, ct, ok, err := art.CollectionArtwork(r.Context(), FromContext(r.Context()).User, key, element, width)
	if err != nil || !ok || len(data) == 0 {
		return false
	}
	data, ct, err = s.androidPhotoFormat(r, data, ct)
	if err != nil {
		writeHTTPError(w, http.StatusBadGateway)
		return true
	}
	h := w.Header()
	if plexAndroidClient(FromContext(r.Context())) {
		h.Add("Vary", "X-Plex-Product")
	}
	setArtworkHeaders(h, ct)
	// The URL carries the collection's update stamp, so it is safe to cache.
	h.Set("Cache-Control", "max-age=86400, private")
	h.Add("Vary", "X-Plex-Token")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

// collectionStamp versions collection artwork URLs so Plex clients refetch
// a poster when the collection changes.
func collectionStamp(c *frontend.Collection) string {
	if c == nil || c.UpdatedAt.IsZero() {
		return "1"
	}
	return strconv.FormatInt(c.UpdatedAt.Unix(), 10)
}
