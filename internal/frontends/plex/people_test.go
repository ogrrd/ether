package plex

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Recorded from a real PMS (GET only, 2026-11): for a movie's
// Role tag {id:117489, filter:"actor=117489", tag:"Tom Hanks",
// tagKey:"5d77682654f42c001f8c2650"},
//
//	GET /library/people/117489 and /library/people/5d77682654f42c001f8c2650
//	→ {"MediaContainer":{"size":1,"identifier":"com.plexapp.plugins.library",
//	   "mediaTagPrefix":"/system/bundle/media/flags/","mediaTagVersion":…,
//	   "Directory":[{"id":117489,"filter":"actor=117489","tag":"Tom Hanks",
//	   "tagType":6,"tagKey":"5d77682654f42c001f8c2650","thumb":"…"}]}}
//
// A director answers tagType 4 / filter "director=…", a writer 5 /
// "writer=…". The numeric form's /media lists only that tag kind's titles
// (actor: 59) in a container with viewGroup "movie" and viewMode 65592
// (director/writer: neither); the tagKey form's /media lists the union
// across kinds (63) with neither attribute. Rows name their library
// (librarySectionTitle/ID/Key), order is titleSort ascending (a sort=
// parameter does not change it), library filters such as year= and genre=
// narrow the list, excludeElements=Media drops Media and adds mediaCount,
// and an unknown id is a 404 on both routes. Ether's person ids are TMDB
// ids, shared across kinds, so the numeric form resolves to the person's
// primary kind (actor, then director, then writer, then crew) and the
// tmdb-person-<id> tagKey form keeps PMS's union behaviour.

func peopleFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture()
	// A show Theo Varga (287) is cast in, and a movie he only directed.
	show := breakingBad
	show.ContentRating = "TV-PG"
	show.Released = time.Date(2008, 1, 20, 0, 0, 0, 0, time.UTC)
	show.Cast = []core.Person{{TMDB: 287, Name: "Theo Varga", Role: "Walter", ThumbPath: "/brad.jpg"}}
	f.catalog.titles[show.Key] = show
	directed := alien
	directed.Key = core.MovieKey(999)
	directed.Title = "Zulu Directed"
	directed.Year = 2001
	directed.Released = time.Date(2001, 5, 1, 0, 0, 0, 0, time.UTC)
	directed.Directors = []core.Person{{TMDB: 287, Name: "Theo Varga"}}
	f.catalog.titles[directed.Key] = directed
	return f
}

func TestLibraryPersonDirectory(t *testing.T) {
	f := peopleFixture(t)
	defer f.srv.Close()

	for _, id := range []string{"287", "tmdb-person-287"} {
		rec := f.get(t, "/library/people/"+id, jsonAccept...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", id, rec.Code, rec.Body.String())
		}
		mc := decodeJSON(t, rec)
		if int(mc["size"].(float64)) != 1 || mc["identifier"] != "com.plexapp.plugins.library" {
			t.Fatalf("%s: container %#v", id, mc)
		}
		if _, ok := mc["allowSync"]; ok {
			t.Errorf("%s: PMS sends no allowSync here: %#v", id, mc)
		}
		dirs := mc["Directory"].([]any)
		if len(dirs) != 1 {
			t.Fatalf("%s: Directory len = %d", id, len(dirs))
		}
		dir := dirs[0].(map[string]any)
		want := map[string]any{"filter": "actor=287", "tag": "Theo Varga", "tagKey": "tmdb-person-287"}
		for k, v := range want {
			if dir[k] != v {
				t.Errorf("%s: Directory[%s] = %v, want %v (%#v)", id, k, dir[k], v, dir)
			}
		}
		if int(dir["id"].(float64)) != 287 || int(dir["tagType"].(float64)) != 6 {
			t.Errorf("%s: Directory id/tagType = %v/%v", id, dir["id"], dir["tagType"])
		}
		if len(dir) != 6 {
			t.Errorf("%s: Directory keys = %#v, want exactly id,filter,tag,tagType,tagKey,thumb", id, dir)
		}
		if thumb, _ := dir["thumb"].(string); thumb == "" || f.get(t, thumb).Code != http.StatusOK {
			t.Errorf("%s: thumb %q does not resolve", id, thumb)
		}
	}

	// XML attribute order matches PMS.
	rec := f.get(t, "/library/people/287")
	body := rec.Body.String()
	if !strings.Contains(body, `<Directory id="287" filter="actor=287" tag="Theo Varga" tagType="6" tagKey="tmdb-person-287" thumb="`) {
		t.Errorf("XML Directory order/shape:\n%s", body)
	}

	// Director-only and writer-only people resolve to their own kind.
	for id, kind := range map[string]string{"7467": "director", "7468": "writer"} {
		rec := f.get(t, "/library/people/"+id, jsonAccept...)
		dir := decodeJSON(t, rec)["Directory"].([]any)[0].(map[string]any)
		if dir["filter"] != kind+"="+id {
			t.Errorf("%s: filter = %v, want %s=%s", id, dir["filter"], kind, id)
		}
	}
	rec = f.get(t, "/library/people/7467", jsonAccept...)
	dir := decodeJSON(t, rec)["Directory"].([]any)[0].(map[string]any)
	if int(dir["tagType"].(float64)) != 4 {
		t.Errorf("director tagType = %v, want 4", dir["tagType"])
	}
}

func personMediaTitles(t *testing.T, f *fixture, path string, hdr map[string]string) (map[string]any, []map[string]any) {
	t.Helper()
	if hdr == nil {
		hdr = map[string]string{}
	}
	hdr["Accept"] = "application/json"
	rec := f.request(t, http.MethodGet, path, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	var rows []map[string]any
	if raw, ok := mc["Metadata"].([]any); ok {
		for _, r := range raw {
			rows = append(rows, r.(map[string]any))
		}
	}
	return mc, rows
}

func rowTitles(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r["title"].(string)
	}
	return out
}

func TestLibraryPersonMedia(t *testing.T) {
	f := peopleFixture(t)
	defer f.srv.Close()

	// Numeric (actor) form: cast titles only, actor-typed container.
	mc, rows := personMediaTitles(t, f, "/library/people/287/media", nil)
	if mc["viewGroup"] != "movie" || int(mc["viewMode"].(float64)) != 65592 {
		t.Errorf("actor media container viewGroup/viewMode = %v/%v", mc["viewGroup"], mc["viewMode"])
	}
	if got := strings.Join(rowTitles(rows), ","); got != "Boundary Bay,Fjord Harbor" {
		t.Fatalf("actor media titles = %s, want Boundary Bay,Fjord Harbor (titleSort, cast only)", got)
	}
	for _, r := range rows {
		if r["librarySectionTitle"] == "" || r["librarySectionKey"] == "" || r["librarySectionID"] == nil {
			t.Errorf("row does not name its library: %#v", r)
		}
	}

	// tagKey form: union across kinds, untyped container (PMS parity).
	mc, rows = personMediaTitles(t, f, "/library/people/tmdb-person-287/media", nil)
	if _, ok := mc["viewGroup"]; ok {
		t.Errorf("tagKey media must not carry viewGroup: %#v", mc)
	}
	if _, ok := mc["viewMode"]; ok {
		t.Errorf("tagKey media must not carry viewMode: %#v", mc)
	}
	if got := strings.Join(rowTitles(rows), ","); got != "Boundary Bay,Fjord Harbor,Zulu Directed" {
		t.Fatalf("tagKey media titles = %s, want union incl. directed title", got)
	}

	// Director form: director titles only, no viewGroup/viewMode.
	mc, rows = personMediaTitles(t, f, "/library/people/7467/media", nil)
	if _, ok := mc["viewGroup"]; ok {
		t.Errorf("director media must not carry viewGroup")
	}
	if got := strings.Join(rowTitles(rows), ","); got != "Fjord Harbor" {
		t.Fatalf("director media titles = %s", got)
	}

	// Library filters narrow the list; paging windows it.
	_, rows = personMediaTitles(t, f, "/library/people/tmdb-person-287/media?year=1999", nil)
	if got := strings.Join(rowTitles(rows), ","); got != "Fjord Harbor" {
		t.Errorf("year=1999 titles = %s", got)
	}
	mc, rows = personMediaTitles(t, f, "/library/people/tmdb-person-287/media", map[string]string{
		"X-Plex-Container-Start": "1", "X-Plex-Container-Size": "1",
	})
	if got := strings.Join(rowTitles(rows), ","); got != "Fjord Harbor" || int(mc["totalSize"].(float64)) != 3 || int(mc["size"].(float64)) != 1 {
		t.Errorf("paged media = %s totalSize=%v size=%v", got, mc["totalSize"], mc["size"])
	}

	// excludeElements=Media drops the Media children and counts them.
	_, rows = personMediaTitles(t, f, "/library/people/287/media?excludeElements=Media", nil)
	for _, r := range rows {
		if _, ok := r["Media"]; ok {
			t.Errorf("excludeElements=Media kept Media: %#v", r)
		}
		if r["type"] == "movie" {
			if n, ok := r["mediaCount"].(float64); !ok || int(n) != 1 {
				t.Errorf("movie mediaCount = %v, want 1", r["mediaCount"])
			}
		}
	}
}

func TestLibraryPersonUnknownAndRestricted(t *testing.T) {
	f := peopleFixture(t)
	defer f.srv.Close()

	for _, path := range []string{
		"/library/people/999999999", "/library/people/999999999/media",
		"/library/people/tmdb-person-999999999", "/library/people/tmdb-person-999999999/media",
		"/library/people/abc", "/library/people/abc/media",
		"/library/people/5d77682654f42c001f8c2650", "/library/people/5d77682654f42c001f8c2650/media",
	} {
		if rec := f.get(t, path, jsonAccept...); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}

	// A viewer limited to the TV section sees only the show credit, and a
	// rating-capped viewer never sees the R-rated movie.
	auth := f.srv.testAuth()
	auth.users["tvonly"] = core.User{ID: 31, Username: "tvonly", AllowedLibraries: []core.LibraryID{2}}
	auth.users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "TV-PG"}
	for token, want := range map[string]string{"tvonly": "Boundary Bay", "kid": "Boundary Bay"} {
		_, rows := personMediaTitles(t, f, "/library/people/287/media", map[string]string{"X-Plex-Token": token})
		if got := strings.Join(rowTitles(rows), ","); got != want {
			t.Errorf("%s media = %s, want %s", token, got, want)
		}
	}
}
