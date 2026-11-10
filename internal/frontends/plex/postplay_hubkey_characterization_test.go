package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Shared admission keeps related cards, their hubKey and detail reads consistent.
// Recorded PMS hubKeys name exactly the rendered cards (see #285 evidence).
func TestPostplayRelatedHubKeyAuthorizationCharacterization(t *testing.T) {
	for _, scenario := range []string{"denied title", "denied parent", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f, rec := hookFixture(t, false)
			candidate := alien
			switch scenario {
			case "denied title":
				candidate.Adult = true
			case "denied parent":
				candidate = bbE1
				parent := breakingBad
				parent.Adult = true
				f.catalog.titles[parent.Key] = parent
			case "unavailable":
				f.srv.d.FilterAvailable = func(_ context.Context, items []core.Title) []core.Title {
					out := make([]core.Title, 0, len(items))
					for _, item := range items {
						if item.Key != candidate.Key {
							out = append(out, item)
						}
					}
					return out
				}
			}
			f.catalog.titles[candidate.Key] = candidate
			rec.fakeRecommender.related = []core.Hub{{ID: "movie.similar", Title: "Related", Type: core.TypeMovie, Items: []core.Title{candidate, fightClub}}}
			f.srv.testAuth().users["restricted-postplay"] = core.User{ID: 902, AllowedLibraries: []core.LibraryID{1, 2}}
			// The source movie is visible to both viewers and is distinct from candidates.
			source := core.Title{Key: core.MovieKey(905), Type: core.TypeMovie, Title: "Source", Released: fixedNow.AddDate(-1, 0, 0)}
			f.catalog.titles[source.Key] = source
			for _, tc := range []struct{ name, token string }{{"owner", ownerToken}, {"restricted", "restricted-postplay"}} {
				t.Run(tc.name, func(t *testing.T) {
					hdr := map[string]string{"Accept": "application/json", "X-Plex-Token": tc.token}
					response := f.request(t, http.MethodGet, "/hubs/metadata/"+source.Key.String()+"/postplay", hdr)
					if response.Code != http.StatusOK {
						t.Fatalf("postplay status=%d body=%s", response.Code, response.Body.String())
					}
					var body struct{ MediaContainer model.MediaContainer }
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					var row *model.Hub
					for i := range body.MediaContainer.Hub {
						if body.MediaContainer.Hub[i].HubIdentifier == "movie.similar" {
							row = &body.MediaContainer.Hub[i]
							break
						}
					}
					if row == nil {
						t.Fatalf("missing related row: %s", response.Body.String())
					}
					parsed, err := url.Parse(row.HubKey)
					if err != nil {
						t.Fatal(err)
					}
					const prefix = "/library/metadata/"
					if !strings.HasPrefix(parsed.Path, prefix) {
						t.Fatalf("invalid HubKey %q", row.HubKey)
					}
					ids := strings.Split(strings.TrimPrefix(parsed.Path, prefix), ",")
					var metadata []string
					for _, item := range row.Metadata {
						metadata = append(metadata, item.RatingKey)
					}
					single := f.request(t, http.MethodGet, prefix+candidate.Key.String(), hdr)
					opened := f.request(t, http.MethodGet, row.HubKey, hdr)
					var openedBody struct {
						MediaContainer struct {
							Metadata []struct {
								RatingKey string `json:"ratingKey"`
							} `json:"Metadata"`
						} `json:"MediaContainer"`
					}
					if opened.Code == http.StatusOK {
						if err := json.Unmarshal(opened.Body.Bytes(), &openedBody); err != nil {
							t.Fatal(err)
						}
					}
					var openedIDs []string
					for _, item := range openedBody.MediaContainer.Metadata {
						openedIDs = append(openedIDs, item.RatingKey)
					}
					t.Logf("scenario=%s viewer=%s HubKey=%q IDs=%v Metadata=%v single=%d HubKeyStatus=%d HubKeyMetadata=%v", scenario, tc.name, row.HubKey, ids, metadata, single.Code, opened.Code, openedIDs)
					if opened.Code != http.StatusOK {
						t.Fatalf("HubKey status=%d body=%s", opened.Code, opened.Body.String())
					}
					wantIDs := []string{fightClub.Key.String()}
					if !reflect.DeepEqual(ids, wantIDs) || !reflect.DeepEqual(metadata, wantIDs) || single.Code != http.StatusNotFound || !reflect.DeepEqual(openedIDs, wantIDs) {
						t.Fatalf("inconsistent admission: IDs=%v metadata=%v single=%d dereference=%v", ids, metadata, single.Code, openedIDs)
					}
				})
			}
		})
	}
}
