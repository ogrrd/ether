package plex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func TestRecentlyAddedUsesAirDerivedDatesAcrossSections(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	movie := core.Title{Key: core.MovieKey(9001), Type: core.TypeMovie, Title: "Movie", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	show := core.Title{Key: core.ShowKey(9002), Type: core.TypeShow, Title: "Show", Released: time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC), AddedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)}
	f.catalog.mu.Lock()
	f.catalog.titles = map[core.RatingKey]core.Title{movie.Key: movie, show.Key: show}
	f.catalog.mu.Unlock()
	rec := f.get(t, "/hubs/home/recentlyAdded", "Accept", "application/json")
	var body struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	items := body.MediaContainer.Metadata
	if rec.Code != 200 || len(items) != 2 || items[0].RatingKey != show.Key.String() || items[0].AddedAt != show.AddedAt.Unix() || items[0].UpdatedAt != show.UpdatedAt.Unix() || items[1].RatingKey != movie.Key.String() || items[1].AddedAt != movie.Released.Unix() || items[1].UpdatedAt != movie.UpdatedAt.Unix() {
		t.Fatalf("recent order or dates: %s", rec.Body.String())
	}
	if sort, _ := sortFor("updatedAt", "desc"); sort != core.SortUpdated {
		t.Fatalf("updatedAt sort changed to %q", sort)
	}
}

// Owner-approved delta: movie/episode catalogue date 2024 is replaced by the
// actual 2004 release/air date; original availability and refresh bytes stay.
func TestPre1970ReleaseIsTheDateAdded(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	s := testServer(Deps{})
	s.setTestNow(func() time.Time { return now })
	movie := &core.Title{Key: core.MovieKey(289), Type: core.TypeMovie, Title: "Casablanca", Released: time.Date(1942, 11, 26, 0, 0, 0, 0, time.UTC)}
	md := s.metadata(context.Background(), &Request{}, movie, renderOpts{})
	if md.AddedAt != movie.Released.Unix() || md.AddedAt >= 0 || md.OriginallyAvailableAt != "1942-11-26" {
		t.Fatalf("movie added %d originally %q", md.AddedAt, md.OriginallyAvailableAt)
	}
	episode := &core.Title{Key: core.EpisodeKey(91759, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1, Released: time.Date(1960, 12, 9, 0, 0, 0, 0, time.UTC)}
	em := s.metadata(context.Background(), &Request{}, episode, renderOpts{})
	if em.AddedAt != episode.Released.Unix() || em.OriginallyAvailableAt != "1960-12-09" {
		t.Fatalf("episode added %d originally %q", em.AddedAt, em.OriginallyAvailableAt)
	}
	split := &core.Title{Key: core.MovieKey(289), Type: core.TypeMovie, Title: "Casablanca", Year: 1942, Released: time.Date(1943, 1, 15, 0, 0, 0, 0, time.UTC)}
	sm := s.metadata(context.Background(), &Request{}, split, renderOpts{})
	if sm.Year != 1942 || sm.OriginallyAvailableAt != "1943-01-15" || sm.AddedAt != split.Released.Unix() {
		t.Fatalf("split year %d originally %q added %d", sm.Year, sm.OriginallyAvailableAt, sm.AddedAt)
	}
	unknown := s.metadata(context.Background(), &Request{}, &core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Undated"}, renderOpts{})
	if unknown.AddedAt != 0 || unknown.OriginallyAvailableAt != "" {
		t.Fatalf("unknown added %d originally %q", unknown.AddedAt, unknown.OriginallyAvailableAt)
	}
}

func TestApprovedReleaseDateAddedAtDelta(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeEpisode} {
		title := core.Title{Key: core.MovieKey(9003), Type: typ, Title: "Date delta", Released: time.Unix(1072915200, 0).UTC(), AddedAt: time.Unix(1704067200, 0).UTC(), UpdatedAt: time.Unix(1767225600, 0).UTC()}
		md := f.srv.metadata(context.Background(), &Request{User: &ownerUser}, &title, renderOpts{})
		if md.AddedAt != 1072915200 || md.OriginallyAvailableAt != "2004-01-01" || md.UpdatedAt != 1767225600 {
			t.Fatalf("%s: %+v", typ, md)
		}
		title.Released = time.Time{}
		if got := f.srv.metadata(context.Background(), &Request{User: &ownerUser}, &title, renderOpts{}); got.AddedAt != 0 {
			t.Fatalf("unknown release: %+v", got)
		}
	}
}

// Root-approved delta: a DVD season now advertises its derived aired date,
// while originallyAvailableAt remains absent because it has no own premiere.
func TestApprovedVirtualSeasonDateAddedDelta(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	title := core.Title{Key: core.SeasonKey(10, 9000), Type: core.TypeSeason, Title: "Disc 1", AddedAt: time.Unix(1072915200, 0).UTC()}
	md := f.srv.metadata(context.Background(), &Request{User: &ownerUser}, &title, renderOpts{})
	if md.AddedAt != 1072915200 || md.OriginallyAvailableAt != "" {
		t.Fatalf("virtual season delta: %+v", md)
	}
}
