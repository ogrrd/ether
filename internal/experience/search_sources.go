package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// SearchSources lives in internal/frontend (host_sessions_experience.go).
// Its former For method is Selectors.SearchFor.
type SearchSources = frontend.SearchSources

// SearchFor is the shared search seen through one frontend's visibility.
// unrestricted is the trusted host policy capability (nil fails closed).
func SearchFor(src SearchSources, vis Visibility, unrestricted func(*core.User) bool) Search {
	return Search{
		Catalog: src.Catalog, Natural: src.Natural, Familiar: src.Familiar, Details: src.Details,
		Visibility: vis, Unrestricted: unrestricted,
		Vibe: VibeSearch{Source: src.Vibe, Visibility: vis, Unrestricted: unrestricted},
	}
}
