package plex

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// sharePolicy is a viewer policy with a plex.tv share predicate.
type sharePolicy struct{ tv core.Filter }
func (sharePolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}


func (sharePolicy) Allowed(*core.User, *core.Title) bool { return true }
func (p sharePolicy) BrowseShareFilter(_ *core.User, typ core.MediaType) core.Filter {
	if typ == core.TypeMovie {
		return core.Filter{}
	}
	return p.tv
}

// A share-filtered viewer's allLeaves listing carries the plex.tv share
// predicate into the catalogue query, so the SQL pages and counts only what
// they may see; a client rating filter outside the share lists nothing.
func TestAllLeavesCarriesShareFilterIntoBrowse(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setTestPolicy(sharePolicy{tv: core.Filter{ContentRatings: []string{"TV-PG"}, ExcludeCollectionNames: []string{"Hidden"}}})
	rec := f.get(t, "/library/sections/2/allLeaves", "Accept", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	q := f.catalog.lastQ
	if q.Filter.Type != core.TypeEpisode || !slices.Equal(q.Filter.ContentRatings, []string{"TV-PG"}) || !slices.Equal(q.Filter.ExcludeCollectionNames, []string{"Hidden"}) {
		t.Fatalf("allLeaves browse filter = %+v", q.Filter)
	}
	f.catalog.lastQ = core.BrowseQuery{}
	rec = f.get(t, "/library/sections/2/allLeaves?contentRating=TV-MA", "Accept", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if f.catalog.lastQ.Filter.Type != "" {
		t.Fatalf("a rating outside the share still queried the catalogue: %+v", f.catalog.lastQ.Filter)
	}
}
