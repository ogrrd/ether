package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/store"
)

type policyTestCatalog struct {
	core.Catalog
	titles []core.Title
}

func (c policyTestCatalog) Search(_ context.Context, _ string, _ []core.MediaType, limit int) ([]core.Title, error) {
	return c.titles[:min(limit, len(c.titles))], nil
}
func (c policyTestCatalog) Get(_ context.Context, key core.RatingKey) (*core.Title, error) {
	for _, t := range c.titles {
		if t.Key == key {
			return &t, nil
		}
	}
	return nil, core.ErrNotFound
}

type policyTestAuth struct{ core.Auth }

func (policyTestAuth) Authenticate(_ context.Context, token string) (*core.User, error) {
	if token == "owner" {
		return &core.User{ID: 1, IsOwner: true}, nil
	}
	if token == "unrestricted" {
		return &core.User{ID: 3}, nil
	}
	return &core.User{ID: 2, MaxContentRating: "PG"}, nil
}
func TestPlexPolicyProductionWiringKeepsOwnerSearch(t *testing.T) {
	policy, err := safety.New(context.Background(), store.OpenTest(t))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{safety: policy}
	hidden := core.Title{Key: core.MovieKey(123), Type: core.TypeMovie, Title: "First Movie", ContentRating: "R"}
	title := core.Title{Key: core.MovieKey(124), Type: core.TypeMovie, Title: "Second Movie", ContentRating: "G"}
	deps := plex.Deps{
		SessionStores:     sessions.NewSessionStores(),
		Experience:        experience.NewSelectors(),
		Catalog:           policyTestCatalog{titles: []core.Title{hidden, title}},
		Auth:              policyTestAuth{},
		DisableEnrichment: true,
	}
	a.wirePlexPolicy(&deps)
	if deps.AccessPolicy != policy {
		t.Fatal("Plex did not receive the production safety service")
	}
	restricted, ok := deps.AccessPolicy.(core.ArtworkAccessPolicy)
	if !ok || restricted.Restricted(&core.User{ID: 1, IsOwner: true}) || restricted.Restricted(&core.User{ID: 3}) || !restricted.Restricted(&core.User{ID: 2, MaxContentRating: "PG"}) {
		t.Fatal("production restriction classification is incorrect")
	}
	srv := plex.NewServer(deps)
	defer srv.Close()
	for _, tc := range []struct {
		token         string
		first, second bool
	}{{"owner", true, false}, {"unrestricted", true, false}, {"viewer", false, true}} {
		r := httptest.NewRequest(http.MethodGet, "/search?query=Movie&limit=1", nil)
		r.Header.Set("X-Plex-Token", tc.token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, hidden.Title) != tc.first || strings.Contains(body, title.Title) != tc.second {
			t.Errorf("%s: %d %s", tc.token, w.Code, body)
		}
	}
}
