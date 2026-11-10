package plex

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"net/url"
	"path"
)

// Absolute CDN URLs contain no rating key. A restricted viewer can only proxy
// artwork already associated with a visible mirrored title.
func (s *Server) remoteArtworkAllowed(r *http.Request, u *url.URL) bool {
	viewer := FromContext(r.Context()).User
	restricted := viewer == nil || viewer.MaxContentRating != "" || len(viewer.AllowedLibraries) > 0
	if policy, ok := s.policy().(frontend.ArtworkAccessPolicy); ok {
		restricted = restricted || policy.Restricted(viewer)
	}
	if !restricted {
		return true
	}
	if s.d.DB == nil {
		return false
	}
	imagePath := "/" + path.Base(u.Path)
	rows, err := s.d.DB.QueryContext(r.Context(), `SELECT key FROM catalog_titles WHERE adult=0 AND (poster=? OR backdrop=? OR still=? OR logo=?) LIMIT 20`, imagePath, imagePath, imagePath, imagePath)
	if err != nil {
		return false
	}
	var keys []frontend.RatingKey
	for rows.Next() {
		var key frontend.RatingKey
		if rows.Scan(&key) != nil {
			rows.Close()
			return false
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false
	}
	for _, key := range keys {
		title, err := s.d.Catalog.Get(r.Context(), key)
		if err == nil && s.visibleTitle(r.Context(), viewer, title) {
			return true
		}
	}
	return false
}
