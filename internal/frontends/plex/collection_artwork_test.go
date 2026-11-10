package plex

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// artCollections adds generated posters to the fake collections.
type artCollections struct {
	*fakeCollections
	mu       sync.Mutex
	calls    []string
	allowArt bool
}

func (a *artCollections) CollectionArtwork(_ context.Context, u *core.User, key core.RatingKey, element string, width int) ([]byte, string, bool, error) {
	a.mu.Lock()
	allowArt := a.allowArt
	if element == "thumb" || (element == "art" && allowArt) {
		a.calls = append(a.calls, element+":"+strconv.Itoa(width))
	}
	a.mu.Unlock()
	if key.Kind() != core.KindSmart {
		return nil, "", false, nil
	}
	if element != "thumb" && !(element == "art" && allowArt) {
		return nil, "", false, nil
	}
	return []byte("GENERATED-" + u.Username), "image/jpeg", true, nil
}

func (a *artCollections) snapshotCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func TestGeneratedCollectionArtwork(t *testing.T) {
	updated := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	f := newFixture()
	f.srv.Close()
	owned := core.Collection{Key: core.SmartKey(12), Title: "Owner Picks", Kind: "smart", Type: core.TypeMovie,
		ChildCount: 1, BackdropPath: "/ownerbd.jpg", UpdatedAt: updated}
	f.cols.cols[owned.Key] = owned
	f.cols.items[owned.Key] = []core.Title{alien}
	art := &artCollections{fakeCollections: f.cols}
	d := f.srv.currentTestDeps()
	d.Collections = art
	f.srv = newTestServer(d)
	defer f.srv.Close()

	// The collection's thumb URL carries its update stamp.
	rec := f.get(t, "/library/sections/1/collections", jsonAccept...)
	stamp := strconv.FormatInt(updated.Unix(), 10)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/library/metadata/"+owned.Key.String()+"/thumb/"+stamp) {
		t.Fatalf("collections listing: %d %s", rec.Code, rec.Body)
	}
	// Plex fetches the generated poster.
	rec = f.get(t, "/library/metadata/"+owned.Key.String()+"/thumb/"+stamp)
	if rec.Code != http.StatusOK || rec.Body.String() != "GENERATED-owner" || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %q", rec.Code, rec.Body)
	}
	// Through the photo transcoder too (how Plex Web asks), with the width.
	rec = f.get(t, "/photo/:/transcode?width=400&height=600&url="+urlEsc("/library/collections/"+owned.Key.String()+"/thumb/"+stamp))
	if rec.Code != http.StatusOK || rec.Body.String() != "GENERATED-owner" {
		t.Fatalf("photo transcode: %d %q", rec.Code, rec.Body)
	}
	callsSnap := art.snapshotCalls()
	if len(callsSnap) == 0 || callsSnap[len(callsSnap)-1] != "thumb:400" {
		t.Fatalf("width not passed: %v", callsSnap)
	}
	// Franchise collections keep their TMDB poster (the hook declines).
	calls := len(art.snapshotCalls())
	f.get(t, "/library/collections/5000000000008091/thumb/1")
	if len(art.snapshotCalls()) != calls {
		t.Fatal("franchise art should not ask the collection engine")
	}
}

func TestCustomCollectionArtEndpoint(t *testing.T) {
	updated := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	f := newFixture()
	f.srv.Close()
	owned := core.Collection{Key: core.SmartKey(12), Title: "Owner Picks", Kind: "smart", Type: core.TypeMovie,
		ChildCount: 1, PosterPath: "https://example.com/custom.jpg", BackdropPath: "https://example.com/custom.jpg", UpdatedAt: updated}
	f.cols.cols[owned.Key] = owned
	f.cols.items[owned.Key] = []core.Title{alien}
	art := &artCollections{fakeCollections: f.cols, allowArt: true}
	d := f.srv.currentTestDeps()
	d.Collections = art
	f.srv = newTestServer(d)
	defer f.srv.Close()

	stamp := strconv.FormatInt(updated.Unix(), 10)
	rec := f.get(t, "/library/metadata/"+owned.Key.String()+"/art/"+stamp)
	if rec.Code != http.StatusOK || rec.Body.String() != "GENERATED-owner" {
		t.Fatalf("art: %d %q", rec.Code, rec.Body)
	}
	rec = f.get(t, "/photo/:/transcode?width=600&height=338&url="+urlEsc("/library/metadata/"+owned.Key.String()+"/art/"+stamp))
	if rec.Code != http.StatusOK || rec.Body.String() != "GENERATED-owner" {
		t.Fatalf("art transcode: %d %q", rec.Code, rec.Body)
	}
}

func TestListResponsesIncludeViewedLeafCount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	// Scrobble the entire show (severance has 2 episodes in fixture).
	rec := f.get(t, "/:/scrobble?identifier=com.plexapp.plugins.library&key="+breakingBad.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("scrobble show: %d %s", rec.Code, rec.Body)
	}

	for _, path := range []string{
		"/library/sections/2/all",
		"/hubs/search?query=Boundary",
		"/library/sections/2/all?type=2",
	} {
		rec = f.get(t, path, jsonAccept...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"viewedLeafCount":2`) {
			t.Fatalf("%s omitted viewedLeafCount:2 in %s", path, body)
		}
		if strings.Contains(body, `"viewCount":1`) {
			t.Fatalf("%s fabricated independent parent viewCount from leaves: %s", path, body)
		}
	}
}
