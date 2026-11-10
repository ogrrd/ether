package plex

import (
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/go-chi/chi/v5"
)

// Metadata list administration is intentionally limited to the native playlist
// API. Keep the measured refusals distinct from an absent media item.
func (s *Server) handlePlaylistMetadataRefusal(w http.ResponseWriter, r *http.Request) {
	k, err := keyParam(r)
	if err != nil || k.Kind() != frontend.KindPlaylist || s.playlists() == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if _, err = s.playlist(r.Context(), FromContext(r.Context()).User, k); err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeHTTPError(w, http.StatusBadRequest)
}
func (s *Server) handlePlaylistItemRefusal(w http.ResponseWriter, r *http.Request) {
	if _, err := strconv.ParseInt(chi.URLParam(r, "item"), 10, 64); err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	writeHTTPError(w, http.StatusBadRequest)
}
func (s *Server) handlePlaylistUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !q.Has("path") || !q.Has("sectionID") {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	writeHTTPError(w, http.StatusInternalServerError)
}

func (s *Server) handlePlaylistArtworkCandidates(w http.ResponseWriter, r *http.Request) {
	k, err := keyParam(r)
	if err != nil || k.Kind() != frontend.KindPlaylist || s.playlists() == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	pl, err := s.playlist(r.Context(), FromContext(r.Context()).User, k)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := libraryContainer()
	if strings.HasSuffix(r.URL.Path, "/posters") {
		stamp := unixOrZero(pl.UpdatedAt)
		if stamp == 0 {
			stamp = 1
		}
		composite := "/playlists/" + pl.Key.String() + "/composite/" + strconv.FormatInt(stamp, 10)
		mc.Metadata = []model.Metadata{{RatingKey: "default://", Key: composite + "?width=400&height=600", Thumb: composite + "?width=200&height=300", Extra: []model.Attr{model.A("selected", true)}}}
		mc.Size = 1
	}
	s.write(w, r, mc)
}

func (s *Server) handlePlaylistExtras(w http.ResponseWriter, r *http.Request) {
	if k, err := keyParam(r); err == nil && k.Kind() == frontend.KindPlaylist {
		s.handlePlaylistMetadataRefusal(w, r)
		return
	}
	s.handleExtras(w, r)
}
