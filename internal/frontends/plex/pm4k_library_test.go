package plex

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPM4KLibrarySlash(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/?X-Plex-Token="+ownerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /library/ with token: got %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var root struct{ XMLName xml.Name }
	if err := xml.Unmarshal(rec.Body.Bytes(), &root); err != nil || root.XMLName.Local != "MediaContainer" {
		t.Fatalf("expected Plex MediaContainer XML: %v, %s", err, rec.Body.String())
	}
}

func TestPM4KLibrarySlashCompatibility(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, accept := range []string{"application/xml", "application/json"} {
		t.Run(accept, func(t *testing.T) {
			headers := map[string]string{"Accept": accept}
			canonical := f.request(t, http.MethodGet, "/library", headers)
			alias := f.request(t, http.MethodGet, "/library/", headers)
			if canonical.Code != http.StatusOK || alias.Code != canonical.Code || alias.Body.String() != canonical.Body.String() || alias.Header().Get("Content-Type") != canonical.Header().Get("Content-Type") {
				t.Fatalf("canonical: %d %s; alias: %d %s", canonical.Code, canonical.Body.String(), alias.Code, alias.Body.String())
			}
			head := f.request(t, http.MethodHead, "/library/", headers)
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != canonical.Header().Get("Content-Type") {
				t.Fatalf("HEAD: %d body %q", head.Code, head.Body.String())
			}
		})
	}
	if rec := f.request(t, http.MethodGet, "/library/", map[string]string{"X-Plex-Token": ""}); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: got %d, want 401", rec.Code)
	}
	if rec := f.get(t, "/library/not-a-real-endpoint"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: got %d, want 404", rec.Code)
	}
	f.srv.testAuth().users[friendToken] = core.User{ID: 2, Username: "friend", AllowedLibraries: []core.LibraryID{1}}
	if rec := f.get(t, "/library/", "X-Plex-Token", friendToken); rec.Code != http.StatusOK {
		t.Fatalf("friend library root: %d", rec.Code)
	}
	sections := f.get(t, "/library/sections", "X-Plex-Token", friendToken)
	if sections.Code != http.StatusOK || !strings.Contains(sections.Body.String(), `title="Movies"`) || strings.Contains(sections.Body.String(), `title="TV Shows"`) {
		t.Errorf("friend sections: %d %s", sections.Code, sections.Body.String())
	}
	if rec := f.get(t, "/library/sections/2/all", "X-Plex-Token", friendToken); rec.Code != http.StatusNotFound {
		t.Errorf("restricted section: got %d, want 404", rec.Code)
	}
}
