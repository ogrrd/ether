package plex

import (
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLibrarySectionTitlesFollowLibraryNames(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	f.srv.setLibraryTitles(func() (string, string) { return "Ether Films", "Ether Series" })

	rec := f.get(t, "/library/sections")
	body := rec.Body.String()
	if !strings.Contains(body, `title="Ether Films"`) || !strings.Contains(body, `title="Ether Series"`) {
		t.Fatalf("custom library names missing: %s", body)
	}
	if strings.Contains(body, `title="Movies"`) || strings.Contains(body, `title="TV Shows"`) {
		t.Fatalf("default library names still present: %s", body)
	}

	movie := f.get(t, "/library/sections/1")
	if !strings.Contains(movie.Body.String(), `title1="Ether Films"`) {
		t.Fatalf("movies section title1: %s", movie.Body.String())
	}
	show := f.get(t, "/library/sections/2")
	if !strings.Contains(show.Body.String(), `title1="Ether Series"`) {
		t.Fatalf("tv section title1: %s", show.Body.String())
	}

	secs := f.srv.LibrarySections()
	if len(secs) != 2 || secs[0].Title != "Ether Films" || secs[1].Title != "Ether Series" {
		t.Fatalf("LibrarySections = %+v", secs)
	}
	// UUIDs must stay stable across renames (derived from section id, not title).
	f.srv.setLibraryTitles(func() (string, string) { return "Other Films", "Ether Series" })
	renamed := f.srv.LibrarySections()
	if renamed[0].UUID != secs[0].UUID || renamed[1].UUID != secs[1].UUID {
		t.Fatalf("rename changed UUIDs: %+v -> %+v", secs, renamed)
	}
	if renamed[0].Title != "Other Films" {
		t.Fatalf("live rename missing: %+v", renamed[0])
	}
}

func TestLibrarySectionTitlesDefaultWhenEmpty(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.request(t, http.MethodGet, "/library/sections", nil)
	body := rec.Body.String()
	if !strings.Contains(body, `title="Movies"`) || !strings.Contains(body, `title="TV Shows"`) {
		t.Fatalf("defaults missing: %s", body)
	}
}

func TestNilConfigLibraryTitlesKeepIDsAndDefaults(t *testing.T) {
	s := &Server{sections: []Section{
		{ID: 1, Type: core.TypeMovie, UUID: "11111111-1111-1111-1111-111111111111", Title: "stored"},
		{ID: 2, Type: core.TypeShow, UUID: "22222222-2222-2222-2222-222222222222", Title: "stored"},
	}}
	got := s.LibrarySections()
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 || got[0].UUID != s.sections[0].UUID || got[1].UUID != s.sections[1].UUID {
		t.Fatalf("stable ids: %+v", got)
	}
	if got[0].Title != "Movies" || got[1].Title != "TV Shows" {
		t.Fatalf("nil config titles = %+v", got)
	}
	if s.sections[0].Title != "stored" || s.sections[1].Title != "stored" {
		t.Fatal("LibrarySections mutated stored titles")
	}
	sec, ok := s.sectionFor(core.TypeMovie)
	if !ok || sec.ID != 1 || sec.UUID != s.sections[0].UUID || sec.Title != "Movies" {
		t.Fatalf("sectionFor = %+v %v", sec, ok)
	}
}
