package plex

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestHubSearchMatchesRecordedPMSHubOrder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "conform", "plex", "web", "web_json_hubs_search_query-burbs&limit-10&includeCollections-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recFile struct {
		Body []byte `json:"body"`
	}
	if err := json.Unmarshal(raw, &recFile); err != nil {
		t.Fatal(err)
	}
	var wantDoc struct {
		MediaContainer struct {
			Size int `json:"size"`
			Hub  []struct {
				Title         string `json:"title"`
				Type          string `json:"type"`
				HubIdentifier string `json:"hubIdentifier"`
				Context       string `json:"context"`
				Size          int    `json:"size"`
				More          bool   `json:"more"`
				Style         string `json:"style"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(recFile.Body, &wantDoc); err != nil {
		t.Fatal(err)
	}
	if len(wantDoc.MediaContainer.Hub) != 17 {
		t.Fatalf("recorded fixture hub count = %d, want 17", len(wantDoc.MediaContainer.Hub))
	}

	f := newFixture()
	defer f.srv.Close()

	for _, endpoint := range []string{"/hubs/search?query=Fjord&limit=10&includeCollections=1", "/hubs/search/voice?query=Fjord&limit=10"} {
		rec := f.get(t, endpoint, jsonAccept...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", endpoint, rec.Code, rec.Body.String())
		}
		mc := decodeJSON(t, rec)
		if int(mc["size"].(float64)) != len(wantDoc.MediaContainer.Hub) {
			t.Fatalf("%s: size = %v, want %d", endpoint, mc["size"], len(wantDoc.MediaContainer.Hub))
		}
		hubs, ok := mc["Hub"].([]any)
		if !ok || len(hubs) != len(wantDoc.MediaContainer.Hub) {
			t.Fatalf("%s: Hub len = %d, want %d", endpoint, len(hubs), len(wantDoc.MediaContainer.Hub))
		}
		for i, want := range wantDoc.MediaContainer.Hub {
			got := hubs[i].(map[string]any)
			if got["hubIdentifier"] != want.HubIdentifier || got["type"] != want.Type || got["title"] != want.Title || got["context"] != want.Context || got["style"] != want.Style {
				t.Errorf("%s hub[%d]: got (%v,%v,%v,%v,%v), want (%s,%s,%s,%s,%s)",
					endpoint, i, got["hubIdentifier"], got["type"], got["title"], got["context"], got["style"],
					want.HubIdentifier, want.Type, want.Title, want.Context, want.Style)
			}
			for _, forbidden := range []string{"key", "hubKey", "promoted"} {
				if _, exists := got[forbidden]; exists {
					t.Errorf("%s hub[%d] (%s) must not carry %q: %v", endpoint, i, want.HubIdentifier, forbidden, got[forbidden])
				}
			}
		}
	}
}

func TestHubSearchPeopleHubsAndReasonedTitles(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	hubByID := func(t *testing.T, mc map[string]any, id string) map[string]any {
		t.Helper()
		for _, raw := range mc["Hub"].([]any) {
			h := raw.(map[string]any)
			if h["hubIdentifier"] == id {
				return h
			}
		}
		t.Fatalf("missing hub %q", id)
		return nil
	}

	// Actor search: "Theo Varga" (TMDB 287, cast on Fjord Harbor).
	rec := f.get(t, "/hubs/search?query=Theo+Varga&limit=10", jsonAccept...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	actorHub := hubByID(t, mc, "actor")
	if int(actorHub["size"].(float64)) != 1 {
		t.Fatalf("actor hub size = %v, want 1", actorHub["size"])
	}
	actorDirs := actorHub["Directory"].([]any)
	actorDir := actorDirs[0].(map[string]any)
	if actorDir["tag"] != "Theo Varga" || int(actorDir["id"].(float64)) != 287 || int(actorDir["tagType"].(float64)) != 6 || actorDir["tagKey"] != "tmdb-person-287" {
		t.Fatalf("unexpected actor Directory: %#v", actorDir)
	}
	if actorDir["key"] != "/library/sections/1/all?actor=287" || actorDir["filter"] != "actor=287" || actorDir["reason"] != "section" || actorDir["reasonTitle"] != "Movies" || int(actorDir["count"].(float64)) != 1 {
		t.Fatalf("unexpected actor Directory section fields: %#v", actorDir)
	}
	thumb, _ := actorDir["thumb"].(string)
	if thumb == "" {
		t.Fatalf("actor Directory missing thumb: %#v", actorDir)
	}
	if portrait := f.get(t, thumb); portrait.Code != http.StatusOK {
		t.Fatalf("actor thumb %s status = %d", thumb, portrait.Code)
	}
	if trans := f.get(t, "/photo/:/transcode?width=100&url="+url.QueryEscape(thumb)); trans.Code != http.StatusOK {
		t.Fatalf("actor transcoded thumb status = %d", trans.Code)
	}
	if secAll := f.get(t, actorDir["key"].(string), jsonAccept...); secAll.Code != http.StatusOK {
		t.Fatalf("actor key %s status = %d", actorDir["key"], secAll.Code)
	} else {
		secMC := decodeJSON(t, secAll)
		items := secMC["Metadata"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["title"] != "Fjord Harbor" {
			t.Fatalf("actor section filter items = %#v", items)
		}
	}

	movieHub := hubByID(t, mc, "movie")
	if int(movieHub["size"].(float64)) != 1 {
		t.Fatalf("movie hub size = %v, want 1", movieHub["size"])
	}
	movieMD := movieHub["Metadata"].([]any)[0].(map[string]any)
	if movieMD["title"] != "Fjord Harbor" || movieMD["reason"] != "actor" || movieMD["reasonID"] != "287" || movieMD["reasonTitle"] != "Theo Varga" {
		t.Fatalf("unexpected reasoned movie Metadata: %#v", movieMD)
	}

	// Director search: "Mara Ellison" (TMDB 7467, director on Fjord Harbor).
	rec = f.get(t, "/hubs/search?query=Mara+Ellison&limit=10", jsonAccept...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mc = decodeJSON(t, rec)
	dirHub := hubByID(t, mc, "director")
	if int(dirHub["size"].(float64)) != 1 {
		t.Fatalf("director hub size = %v, want 1", dirHub["size"])
	}
	dirEntry := dirHub["Directory"].([]any)[0].(map[string]any)
	if dirEntry["tag"] != "Mara Ellison" || int(dirEntry["id"].(float64)) != 7467 || int(dirEntry["tagType"].(float64)) != 4 || dirEntry["key"] != "/library/sections/1/all?director=7467" {
		t.Fatalf("unexpected director Directory: %#v", dirEntry)
	}
	movieMD = hubByID(t, mc, "movie")["Metadata"].([]any)[0].(map[string]any)
	if movieMD["title"] != "Fjord Harbor" || movieMD["reason"] != "director" || movieMD["reasonID"] != "7467" || movieMD["reasonTitle"] != "Mara Ellison" {
		t.Fatalf("unexpected director-reasoned movie Metadata: %#v", movieMD)
	}

	// Universal /library/search includes people Directory rows when unfiltered or when searchTypes includes people.
	for _, target := range []string{"/library/search?query=Theo+Varga", "/library/search?query=Theo+Varga&searchTypes=people"} {
		rec = f.get(t, target, jsonAccept...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		libMC := decodeJSON(t, rec)
		results, _ := libMC["SearchResult"].([]any)
		if len(results) != 1 {
			t.Fatalf("%s: SearchResult len = %d, want 1", target, len(results))
		}
		dir := results[0].(map[string]any)["Directory"].(map[string]any)
		if dir["tag"] != "Theo Varga" || dir["key"] != "/library/sections/1/all?actor=287" {
			t.Fatalf("%s: unexpected Directory %#v", target, dir)
		}
	}
}

func TestHubSearchPeopleHonorsViewerSafetyAndVisibility(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()

	auth := f.srv.testAuth()
	auth.users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	auth.users["tvonly"] = core.User{ID: 31, Username: "tvonly", AllowedLibraries: []core.LibraryID{2}}

	// Add an unreleased movie and an adult movie with unique actors.
	unreleased := alien
	unreleased.Key = core.MovieKey(9001)
	unreleased.Released = fixedNow.Add(48 * time.Hour)
	unreleased.Cast = []core.Person{{TMDB: 9101, Name: "Future Star", ThumbPath: "/future.jpg"}}
	f.catalog.titles[unreleased.Key] = unreleased

	adult := alien
	adult.Key = core.MovieKey(9002)
	adult.Adult = true
	adult.Released = fixedNow.Add(-24 * time.Hour)
	adult.Cast = []core.Person{{TMDB: 9102, Name: "Adult Star", ThumbPath: "/adult.jpg"}}
	f.catalog.titles[adult.Key] = adult

	blockedMovie := alien
	blockedMovie.Key = core.MovieKey(9003)
	blockedMovie.Released = fixedNow.Add(-24 * time.Hour)
	blockedMovie.ContentRating = "PG"
	blockedMovie.Keywords = []string{"blocked"}
	blockedMovie.Cast = []core.Person{{TMDB: 9103, Name: "Blocked Star", ThumbPath: "/blocked.jpg"}}
	f.catalog.titles[blockedMovie.Key] = blockedMovie

	assertNoPeopleOrReasonedHits := func(token, query string) {
		t.Helper()
		rec := f.request(t, http.MethodGet, "/hubs/search?query="+url.QueryEscape(query), map[string]string{
			"X-Plex-Token": token,
			"Accept":       "application/json",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("token=%s query=%s status=%d", token, query, rec.Code)
		}
		mc := decodeJSON(t, rec)
		for _, raw := range mc["Hub"].([]any) {
			h := raw.(map[string]any)
			if int(h["size"].(float64)) != 0 {
				t.Errorf("token=%s query=%s leaked in hub %s: %#v", token, query, h["hubIdentifier"], h)
			}
		}
		libRec := f.request(t, http.MethodGet, "/library/search?query="+url.QueryEscape(query), map[string]string{
			"X-Plex-Token": token,
			"Accept":       "application/json",
		})
		libMC := decodeJSON(t, libRec)
		if int(libMC["size"].(float64)) != 0 {
			t.Errorf("token=%s query=%s leaked in /library/search: %#v", token, query, libMC)
		}
	}

	// "Theo Varga" is only on R-rated Fjord Harbor in section 1 (Movies).
	assertNoPeopleOrReasonedHits("kid", "Theo Varga")
	assertNoPeopleOrReasonedHits("tvonly", "Theo Varga")

	// Unreleased and adult titles are hidden even from owner.
	assertNoPeopleOrReasonedHits(ownerToken, "Future Star")
	assertNoPeopleOrReasonedHits(ownerToken, "Adult Star")

	// Policy-blocked title is hidden from non-owner friends.
	assertNoPeopleOrReasonedHits(friendToken, "Blocked Star")
}
