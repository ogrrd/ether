package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/personalize"
)

func TestHomeShelfSurvivesHTTPCountSequence(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for i := int64(1); i <= 80; i++ {
		title := fightClub
		title.Key = core.MovieKey(10000 + i)
		title.Title = fmt.Sprintf("Comedy %d", i)
		title.Genres = []string{"Comedy"}
		title.Rating = 8.5
		title.VoteCount = 10000
		title.Popularity = 100
		f.catalog.titles[title.Key] = title
	}
	e, err := personalize.New(personalize.Options{DB: discoveryDB(t), Catalog: f.catalog, State: f.srv.state(), RefreshEvery: -1, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.srv.setRecommender(e)
	ctx := context.Background()
	if err := e.Rebuild(ctx, &ownerUser); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"home.toprated", "home.showcase.comedy"} {
		t.Run(id, func(t *testing.T) {
			_, err := e.SetHomeLayout(ctx, core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "*", Status: "off"}, {Row: id, Status: "always", Slot: 1}}}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/hubs?count=12", "/hubs?count=4", "/hubs/promoted?count=4", "/hubs?count=1", "/hubs/promoted?count=5", "/hubs?count=12"} {
				response := f.get(t, path, "Accept", "application/json", "X-Plex-Product", "Plex for iOS", "X-Plex-Platform", "iOS", "X-Plex-Version", "2026.20.0", "X-Plex-Session-Identifier", "shelf-session")
				var got struct {
					MediaContainer struct {
						Hub []struct {
							ID   string `json:"hubIdentifier"`
							Size int    `json:"size"`
						}
					}
				}
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if response.Code != 200 || len(got.MediaContainer.Hub) != 1 || got.MediaContainer.Hub[0].ID != id {
					t.Fatalf("%s: status=%d hubs=%+v", path, response.Code, got.MediaContainer.Hub)
				}
				parsed, _ := url.Parse(path)
				want, _ := strconv.Atoi(parsed.Query().Get("count"))
				if got.MediaContainer.Hub[0].Size != want {
					t.Fatalf("%s: size=%d want=%d", path, got.MediaContainer.Hub[0].Size, want)
				}
				t.Logf("%s: %s size=%d", path, id, got.MediaContainer.Hub[0].Size)
			}
		})
	}
}
