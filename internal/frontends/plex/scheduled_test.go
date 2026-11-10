package plex

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/store"
)

type scheduledCatalog struct {
	*fakeCatalog
	future core.Title
}

func (c *scheduledCatalog) ScheduledEpisodes(_ context.Context, k core.RatingKey) ([]core.Title, error) {
	if k != c.future.Key.Parent() {
		return nil, core.ErrNotFound
	}
	return []core.Title{c.future}, nil
}
func (c *scheduledCatalog) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	if k == c.future.Key {
		return nil, core.ErrNotFound
	}
	return c.fakeCatalog.Get(ctx, k)
}

func TestScheduledEpisodeHTTPIsMetadataOnlyAndRevocable(t *testing.T) {
	enabled := false
	f := newFixture(func(d *Deps) {
		old := depsConfig(d)
		setDepsConfig(d, func() config.Config { c := old(); c.Catalog.ShowUpcomingEpisodes = enabled; return c })
	})
	defer f.srv.Close()
	future := bbE2
	future.Key = core.EpisodeKey(bbE2.Key.TMDB(), 1, 3)
	future.Episode = 3
	future.Title = "Future episode"
	future.Released = fixedNow.Add(48 * time.Hour)
	future.StillPath = "/future-still.jpg"
	f.srv.d.Catalog = &scheduledCatalog{f.catalog, future}
	base := "/library/metadata/" + future.Key.String()
	children := "/library/metadata/" + bbS1.Key.String() + "/children"
	if r := f.get(t, base); r.Code != 404 {
		t.Fatalf("default detail: %d", r.Code)
	}
	enabled = true
	for _, path := range []string{base, children + "?X-Plex-Container-Start=2&X-Plex-Container-Size=1"} {
		r := f.get(t, path)
		var mc struct {
			Metadata []struct {
				Title   string     `xml:"title,attr"`
				Summary string     `xml:"summary,attr"`
				Media   []struct{} `xml:"Media"`
			} `xml:"Video"`
		}
		if err := xml.Unmarshal(r.Body.Bytes(), &mc); err != nil {
			t.Fatal(err)
		}
		if r.Code != 200 || len(mc.Metadata) != 1 || len(mc.Metadata[0].Media) != 0 || !strings.Contains(mc.Metadata[0].Title, "Unaired") || !strings.Contains(mc.Metadata[0].Summary, future.Released.Format("2006-01-02")) {
			t.Fatalf("placeholder %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	jsonResponse := f.get(t, base, "Accept", "application/json")
	var body struct {
		MediaContainer struct {
			Metadata []struct {
				Title string
				Media []json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.MediaContainer.Metadata) != 1 || len(body.MediaContainer.Metadata[0].Media) != 0 || !strings.Contains(body.MediaContainer.Metadata[0].Title, "Unaired") {
		t.Fatalf("JSON placeholder: %s", jsonResponse.Body.String())
	}

	for _, path := range []string{base + "/thumb", "/photo/:/transcode?url=" + url.QueryEscape(base+"/thumb")} {
		if r := f.get(t, path); r.Code != http.StatusOK {
			t.Fatalf("image: %d %s", r.Code, r.Body.String())
		}
	}
	decision := "/video/:/transcode/universal/decision?path=" + url.QueryEscape(base)
	if r := f.get(t, decision); r.Code != 404 && !strings.Contains(r.Body.String(), "has not been released") && !strings.Contains(r.Body.String(), "not found") {
		t.Fatalf("future playback: %d %s", r.Code, r.Body.String())
	}
	if r := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"/allLeaves"); strings.Contains(r.Body.String(), "Future episode") {
		t.Fatal("future leaked into allLeaves/autoplay")
	}
	// Listing requests queue catalogue enrichment. Finish those reads before
	// changing the fixture configuration or its deliberately unsynchronized map.
	f.srv.waitBackground()
	// Removing the opt-in immediately revokes metadata and image access.
	enabled = false
	for _, path := range []string{base, base + "/thumb"} {
		if r := f.get(t, path); r.Code != 404 {
			t.Fatalf("disabled %s: %d", path, r.Code)
		}
	}
	f.srv.waitBackground()
	enabled = true
	f.srv.setTestPolicy(denyTitlePolicy{})
	parent := f.catalog.titles[bbS1.Key]
	parent.Title = "Blocked"
	f.catalog.titles[bbS1.Key] = parent
	for _, path := range []string{base, base + "/thumb"} {
		if r := f.get(t, path); r.Code != 404 {
			t.Fatalf("denied parent %s: %d", path, r.Code)
		}
	}
}

func TestScheduledEpisodeViewerGuardsAndAlternateOrder(t *testing.T) {
	for _, restriction := range []string{"section", "rating", "adult", "show policy", "alternate order", "unknown date", "wrong season"} {
		t.Run(restriction, func(t *testing.T) {
			f := newFixture(func(d *Deps) {
				old := depsConfig(d)
				setDepsConfig(d, func() config.Config { c := old(); c.Catalog.ShowUpcomingEpisodes = true; return c })
			})
			defer f.srv.Close()
			future := bbE2
			future.Key = core.EpisodeKey(bbE2.Key.TMDB(), 1, 3)
			future.Episode = 3
			future.Title = "Future episode"
			future.Released = fixedNow.Add(48 * time.Hour)
			future.StillPath = "/future.jpg"
			token := ownerToken
			switch restriction {
			case "section":
				token = "movie-only"
				f.srv.testAuth().users[token] = core.User{ID: 88, AllowedLibraries: []core.LibraryID{1}}
			case "rating":
				token = "young"
				f.srv.testAuth().users[token] = core.User{ID: 89, MaxContentRating: "PG"}
				future.ContentRating = "R"
			case "adult":
				future.Adult = true
			case "show policy":
				f.srv.setTestPolicy(denyTitlePolicy{})
				show := f.catalog.titles[breakingBad.Key]
				show.Title = "Blocked"
				f.catalog.titles[show.Key] = show
			case "alternate order":
				db, err := store.OpenMemory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				svc := episodeorder.New(db, orderedSource{}, f.catalog)
				f.srv.d.EpisodeOrder = svc
				if err := svc.Save(context.Background(), 0, breakingBad.Key, episodeorder.Preference{GroupID: "dvd", Specials: "last"}); err != nil {
					t.Fatal(err)
				}
			case "unknown date":
				future.Released = time.Time{}
			case "wrong season":
				future.Key = core.EpisodeKey(bbE2.Key.TMDB(), 2, 3)
			}
			f.srv.d.Catalog = &scheduledCatalog{f.catalog, future}
			base := "/library/metadata/" + future.Key.String()
			for _, path := range []string{base, base + "/thumb", "/photo/:/transcode?url=" + url.QueryEscape(base+"/thumb")} {
				if r := f.get(t, path, "X-Plex-Token", token); r.Code != 404 {
					t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
				}
			}
			if r := f.get(t, "/library/metadata/"+bbS1.Key.String()+"/children", "X-Plex-Token", token); strings.Contains(r.Body.String(), "Future episode") {
				t.Fatal("future leaked in listing")
			}
		})
	}
}
