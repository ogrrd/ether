package plex

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPlaylistSearchAndRecentProjection(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	a, _ := f.pls.Create(context.Background(), ownerUser.ID, "Night", []core.RatingKey{fightClub.Key})
	b, _ := f.pls.Create(context.Background(), ownerUser.ID, "Night Watch", []core.RatingKey{bbE1.Key})
	_, _ = f.pls.Create(context.Background(), friendUser.ID, "Night Secret", nil)
	f.pls.pls[a.Key].AddedAt = fixedNow.Add(-48 * time.Hour)
	f.pls.pls[b.Key].AddedAt = fixedNow.Add(-24 * time.Hour)
	// A recently played older list must precede a newer unplayed one.
	if err := f.srv.state().Scrobble(context.Background(), ownerUser.ID, a.Key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/hubs/search?query=Night", "/library/search?query=Night"} {
		body := f.get(t, path).Body.String()
		if !strings.Contains(body, a.Key.String()) || !strings.Contains(body, b.Key.String()) || strings.Contains(body, "Night Secret") {
			t.Fatalf("%s: %s", path, body)
		}
	}
	body := f.get(t, "/hubs/search?query=Night&contentDirectoryID=1").Body.String()
	if !strings.Contains(body, a.Key.String()) || strings.Contains(body, b.Key.String()) {
		t.Fatal(body)
	}
	body = f.get(t, "/hubs/search?query=Night&contentDirectoryID=01").Body.String()
	if strings.Contains(body, a.Key.String()) || strings.Contains(body, b.Key.String()) {
		t.Fatal(body)
	}
	body = f.get(t, "/hubs?contentDirectoryID=1,2").Body.String()
	if strings.Contains(body, "home.playlists") {
		t.Fatal(body)
	}
	for _, probe := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/playQueues", 200},
		{"GET", "/library/metadata/" + a.Key.String() + "/related", 200},
		{"GET", "/library/metadata/" + a.Key.String() + "/allLeaves", 404},
		{"GET", "/library/metadata/" + a.Key.String() + "/extras", 400},
		{"GET", "/library/metadata/" + a.Key.String() + "/match", 400}, {"GET", "/playQueues?type=video", 400},
		{"POST", "/playlists/upload", 400}, {"POST", "/playlists/upload?path=x&sectionID=1", 500},
		{"DELETE", "/library/metadata/" + a.Key.String(), 200},
		{"PUT", "/library/metadata/" + a.Key.String() + "/prefs", 400},
		{"PUT", "/library/metadata/" + a.Key.String() + "/analyze", 400},
		{"GET", "/library/metadata/" + a.Key.String() + "/posters", 200},
		{"GET", "/library/metadata/" + a.Key.String() + "/themes", 200},
		{"PUT", "/playlists/" + a.Key.String() + "/items/1", 400},
		{"PUT", "/playlists/" + a.Key.String() + "/items/not-an-id", 404},
	} {
		if rec := f.request(t, probe.method, probe.path, nil); rec.Code != probe.status {
			t.Fatalf("%s %s: %d %s", probe.method, probe.path, rec.Code, rec.Body)
		}
	}
	if _, err := f.pls.Get(context.Background(), ownerUser.ID, a.Key); err != nil {
		t.Fatal("metadata DELETE removed playlist")
	}
	body = f.get(t, "/hubs/sections/1?includeLibraryPlaylists=1").Body.String()
	if !strings.Contains(body, "recent.library.playlists.1") || !strings.Contains(body, "Night") || strings.Contains(body, "Night Watch") {
		t.Fatal(body)
	}
	body = f.get(t, "/playlists/"+a.Key.String()+"?excludeFields=title").Body.String()
	if strings.Contains(body, `title="Night"`) {
		t.Fatal(body)
	}
	rec := f.request(t, "POST", "/playlists?type=video&title=Window&uri=unresolved&X-Plex-Container-Start=0&X-Plex-Container-Size=0", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "<Playlist ") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	request := httptest.NewRequest("POST", "/playlists?type=video&title=QueryOnly&uri=unresolved", strings.NewReader("not-a-form"))
	request.ContentLength = 2048
	request.Header.Set("X-Plex-Token", ownerToken)
	queryResponse := httptest.NewRecorder()
	f.srv.ServeHTTP(queryResponse, request)
	if queryResponse.Code != 200 {
		t.Fatal(queryResponse.Code, queryResponse.Body.String())
	}
	for _, test := range []struct {
		title, query string
		score        float64
	}{{"Night", "Night", .92}, {"Night Watch", "Night", .72}, {"Night Watch", "Ni Wa", .52}, {"A Night Watch", "Night", .32}} {
		if got := playlistSearchScore(test.title, test.query); got != test.score {
			t.Fatalf("score %s / %s = %v", test.title, test.query, got)
		}
	}
	pr := &Request{User: &ownerUser}
	h, _, ok := f.srv.playlistsHubProjection(withRequest(context.Background(), pr), pr, 1)
	if !ok || len(h.Metadata) != 1 || h.Metadata[0].RatingKey != a.Key.String() {
		t.Fatalf("recent: %+v", h)
	}
}
