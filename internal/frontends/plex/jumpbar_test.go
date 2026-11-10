package plex

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestFirstCharacterJumpbarOffsetsMatchTitleSortEveryLetter drives the section
// handler this server actually serves: /library/sections/{id}/firstCharacter
// and the title-sorted library list behind each jump.
func TestFirstCharacterJumpbarOffsetsMatchTitleSortEveryLetter(t *testing.T) {
	cfg := config.Catalog{
		Exclusions: []core.LibraryExclusion{{
			Name:      "English/French only",
			MediaType: "movie",
			Match:     "all",
			Rules: []core.CollectionRule{
				{Field: "language", Op: "notIn", Value: json.RawMessage(`["en","fr"]`)},
			},
		}},
	}
	c, err := catalog.New(store.OpenTest(t), nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	rel := fixedNow.Add(-time.Hour)
	var nextID int64 = 1
	add := func(title, lang string) {
		id := nextID
		nextID++
		ti := core.Title{
			Key:      core.MovieKey(id),
			Type:     core.TypeMovie,
			Title:    title,
			Language: lang,
			Released: rel,
		}
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
	}
	add("!Three Amigos!", "en")
	add("1917", "en")
	add("2001: A Space Odyssey", "en")
	add("The 39 Steps", "en")
	for ch := byte('A'); ch <= 'Z'; ch++ {
		letter := string(ch)
		add(letter+"lpha "+letter, "en")
		add("The "+letter+"ravo "+letter, "en")
		add("A "+letter+"harlie "+letter, "en")
		add("An "+letter+"elta "+letter, "en")
		add(letter+" Hidden Hindi", "hi")
	}
	add("À Nous la Liberté", "fr")
	add("Æon Flux", "en")
	add("Ça brûle", "fr")
	add("Český Honza", "fr")
	add("Été 85", "fr")
	add("Léolo", "fr")
	add("Léon Morin, Priest", "fr")
	add("Léon: The Professional", "en")
	add("Líneas cruzadas", "fr")
	add("Lögnens pris", "fr")
	add("Lymelife", "en")
	add("Ödipussi", "fr")
	add("Yucatán", "fr")
	add("Yummy", "en")
	add("Yuppies", "en")
	add("Yves Saint Laurent", "fr")
	if err := c.UpsertTitle(ctx, core.Title{
		Key:       core.MovieKey(nextID),
		Type:      core.TypeMovie,
		Title:     "L'Éclisse",
		SortTitle: "ÉCLISSE",
		Language:  "fr",
		Released:  rel,
	}, true); err != nil {
		t.Fatal(err)
	}
	nextID++
	add("½ Revolution", "en")
	add("七人の侍", "ja")
	add("Солярис", "ru")

	f := newFixture(func(d *testServerDeps) { d.Catalog = c })
	defer f.srv.Close()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct {
			MediaContainer model.MediaContainer
		}
		if rec.Code != 200 {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}
	page := func(t *testing.T, path string, start, size int) model.MediaContainer {
		t.Helper()
		return decode(t, f.get(t, path,
			jsonAccept[0], jsonAccept[1],
			"X-Plex-Container-Start", strconv.Itoa(start),
			"X-Plex-Container-Size", strconv.Itoa(size),
		))
	}

	dirs := decode(t, f.get(t, "/library/sections/1/firstCharacter", jsonAccept...)).Directory
	// One "#" (digits and symbols that sort before "a") plus A–Z. The
	// non-ASCII title sorts after Z and is not a second "#".
	if len(dirs) != 27 {
		t.Fatalf("expected 27 firstCharacter buckets, got %d", len(dirs))
	}
	hashTitles := 0
	for _, d := range dirs {
		if d.Title == "#" {
			hashTitles++
		}
	}
	if hashTitles != 1 {
		t.Fatalf("jump bar titled %d entries #; tvOS adds every one into the A offset", hashTitles)
	}
	if dirs[0].Key != "%23" && dirs[0].Key != "#" || dirs[0].Title != "#" || dirs[0].Size == nil || *dirs[0].Size != 4 {
		t.Fatalf("unexpected leading # bucket: %+v", dirs[0])
	}
	last := dirs[len(dirs)-1]
	if last.Key != "Z" || last.Title != "Z" {
		t.Fatalf("jump bar should end on Z, got %+v", last)
	}
	reversed := decode(t, f.get(t, "/library/sections/1/firstCharacter?sort=titleSort:desc", jsonAccept...)).Directory
	if len(reversed) != len(dirs) || reversed[0].Key != last.Key || reversed[len(reversed)-1].Key != dirs[0].Key {
		t.Fatalf("descending jumpbar did not reverse the letter order")
	}

	all := page(t, "/library/sections/1/all?type=1&sort=titleSort:asc", 0, 500)
	if all.TotalSize == nil {
		t.Fatal("title sort omitted the total")
	}
	offset := 0
	for _, d := range dirs {
		if d.Size == nil || d.FastKey == "" {
			t.Fatalf("bucket %s missing size or fastKey", d.Key)
		}
		window := page(t, "/library/sections/1/all?type=1&sort=titleSort:asc", offset, *d.Size)
		filtered := page(t, d.FastKey+"&type=1&sort=titleSort:asc", 0, 500)
		keyed := page(t, "/library/sections/1/firstCharacter/"+d.Key+"?type=1&sort=titleSort:asc", 0, 500)
		if len(window.Metadata) != *d.Size || len(filtered.Metadata) != *d.Size || len(keyed.Metadata) != *d.Size {
			t.Fatalf("bucket %s: window=%d filtered=%d keyed=%d want %d", d.Key, len(window.Metadata), len(filtered.Metadata), len(keyed.Metadata), *d.Size)
		}
		for i := range window.Metadata {
			if filtered.Metadata[i].RatingKey != window.Metadata[i].RatingKey || keyed.Metadata[i].RatingKey != window.Metadata[i].RatingKey {
				t.Fatalf("bucket %s item %d mismatch: jumpbar=%s filtered=%s keyed=%s", d.Key, i, window.Metadata[i].Title, filtered.Metadata[i].Title, keyed.Metadata[i].Title)
			}
			sortTitle := strings.ToLower(window.Metadata[i].TitleSort)
			switch d.Title {
			case "#":
				if d.Key == "~" {
					if sortTitle < "{" {
						t.Fatalf("bucket ~ contained %q (sort %q)", window.Metadata[i].Title, sortTitle)
					}
				} else if sortTitle >= "a" {
					t.Fatalf("bucket # contained %q (sort %q)", window.Metadata[i].Title, sortTitle)
				}
			default:
				if !strings.HasPrefix(sortTitle, strings.ToLower(d.Title)) {
					t.Fatalf("bucket %s landed on %q (sort %q)", d.Title, window.Metadata[i].Title, sortTitle)
				}
			}
		}
		offset += *d.Size
	}
	// Non-ASCII sort titles sit after Z. They stay in the library total and
	// off the jump bar, so they cannot inflate the offset of A.
	if offset != *all.TotalSize-1 {
		t.Fatalf("sum of jump-bar counts (%d) != title-sorted total minus the trailing title (%d)", offset, *all.TotalSize-1)
	}
	if got := all.Metadata[len(all.Metadata)-1].Title; got != "½ Revolution" {
		t.Fatalf("expected the non-ASCII title after Z, last item %q", got)
	}
	// Verify every bucket's cumulative offset equals the exact index of its first title in /all,
	// and accented L titles sort before Lymelife (so M never lands on Lögnens pris).
	indexByTitle := map[string]int{}
	for i, m := range all.Metadata {
		if _, exists := indexByTitle[m.Title]; !exists {
			indexByTitle[m.Title] = i
		}
	}
	lymelifeIdx, ok := indexByTitle["Lymelife"]
	if !ok {
		t.Fatal("Lymelife missing from /all")
	}
	for _, acc := range []string{"Léolo", "Léon Morin, Priest", "Léon: The Professional", "Líneas cruzadas", "Lögnens pris"} {
		idx, ok := indexByTitle[acc]
		if !ok || idx >= lymelifeIdx {
			t.Fatalf("expected %q (idx=%d) before Lymelife (idx=%d) in /all", acc, idx, lymelifeIdx)
		}
	}
}

type hiddenKeysCatalog struct {
	*catalog.Catalog
	hidden []core.RatingKey
}

func (c *hiddenKeysCatalog) withHidden(f core.Filter) core.Filter {
	f.ExcludeKeys = append(append([]core.RatingKey{}, f.ExcludeKeys...), c.hidden...)
	return f
}

func (c *hiddenKeysCatalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	q.Filter = c.withHidden(q.Filter)
	return c.Catalog.Browse(ctx, q)
}

func (c *hiddenKeysCatalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.withHidden(core.Filter{Type: t}), facet)
}

func (c *hiddenKeysCatalog) FacetFilter(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.withHidden(f), facet)
}

// TestFirstCharacterJumpbarRealDataOrderingAndExcludedKeys reproduces the exact
// tvOS jump-bar drift where 6 hidden release-evidence-window "#" movies
// ("100 Days", "2 Valid IDs", "28 Kegs Later", "29 Seconds to Midnight",
// "The 3 Killer Pigs", "58th") inflated the "#" bucket count by 6 (one 6-column
// grid row), causing "A" to land on "A.C.O.D." (the 7th "A" movie) instead of
// "A" (2017).
func TestFirstCharacterJumpbarRealDataOrderingAndExcludedKeys(t *testing.T) {
	c, err := catalog.New(store.OpenTest(t), nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	rel := time.Now().Add(-90 * 24 * time.Hour)
	var nextID int64 = 1
	var hidden []core.RatingKey
	add := func(title, rating string, isHidden bool) {
		id := nextID
		nextID++
		key := core.MovieKey(id)
		ti := core.Title{
			Key:           key,
			Type:          core.TypeMovie,
			Title:         title,
			Language:      "en",
			ContentRating: rating,
			Released:      rel,
		}
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
		if isHidden {
			hidden = append(hidden, key)
		}
	}

	// Visible "#" titles ending with [REC]..[REC]⁴ Apocalypse.
	add("#Alive", "TV-MA", false)
	add("(500) Days of Summer", "PG-13", false)
	add("[REC]", "R", false)
	add("[REC]²", "R", false)
	add("[REC]³ Genesis", "R", false)
	add("[REC]⁴ Apocalypse", "R", false)

	// The 6 hidden evidence-window "#" movies from the lab catalogue.
	add("100 Days", "PG", true)
	add("2 Valid IDs", "PG-13", true)
	add("28 Kegs Later", "R", true)
	add("29 Seconds to Midnight", "R", true)
	add("The 3 Killer Pigs", "NR", true)
	add("58th", "PG-13", true)

	// The first 7 visible "A" movies from the lab catalogue:
	// index 0 of A = "A" (2017), index 6 of A (one 6-item row later) = "A.C.O.D.".
	add("A", "PG-13", false)
	add("A", "PG", false)
	add("The A-Team", "PG-13", false)
	add("A-X-L", "PG", false)
	add("A. Rimbaud", "PG", false)
	add("A.C.A.B. - All Cops Are Bastards", "R", false)
	add("A.C.O.D.", "R", false)
	add("Hidden A Unobserved", "PG", true)

	// Populate B–Z with visible and hidden movies plus mixed ratings.
	for ch := byte('B'); ch <= 'Z'; ch++ {
		letter := string(ch)
		add(letter+" First PG", "PG", false)
		add("The "+letter+" Second R", "R", false)
		add(letter+" Hidden Unobserved", "PG", true)
	}
	// Trailing non-ASCII sort titles.
	add("½ Revolution", "PG", false)
	add("Été 85", "PG-13", false)

	wrapped := &hiddenKeysCatalog{Catalog: c, hidden: hidden}
	f := newFixture(func(d *testServerDeps) { d.Catalog = wrapped })
	defer f.srv.Close()

	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct {
			MediaContainer model.MediaContainer
		}
		if rec.Code != 200 {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}

	dirs := decode(t, f.get(t, "/library/sections/1/firstCharacter", jsonAccept...)).Directory
	all := decode(t, f.get(t, "/library/sections/1/all?type=1&sort=titleSort:asc",
		jsonAccept[0], jsonAccept[1],
		"X-Plex-Container-Start", "0",
		"X-Plex-Container-Size", "500",
	)).Metadata

	if len(dirs) != 27 {
		t.Fatalf("expected 27 jump-bar buckets, got %d", len(dirs))
	}
	if dirs[0].Title != "#" || dirs[0].Size == nil || *dirs[0].Size != 6 {
		t.Fatalf("expected # size=6 (excluding the 6 hidden # titles), got %+v", dirs[0])
	}

	offset := 0
	for _, d := range dirs {
		if d.Size == nil {
			t.Fatalf("bucket %s missing size", d.Title)
		}
		if offset >= len(all) {
			t.Fatalf("bucket %s offset %d out of range (len=%d)", d.Title, offset, len(all))
		}
		landed := all[offset]
		if d.Title == "A" {
			if offset != 6 {
				t.Fatalf("expected A offset=6, got %d", offset)
			}
			if landed.Title != "A" {
				t.Fatalf("selecting A landed on %q at offset %d (want \"A\")", landed.Title, offset)
			}
		}
		sortTitle := strings.ToLower(landed.TitleSort)
		if d.Title == "#" {
			if sortTitle >= "a" {
				t.Fatalf("# bucket landed on %q (sort %q)", landed.Title, sortTitle)
			}
		} else if !strings.HasPrefix(sortTitle, strings.ToLower(d.Title)) {
			t.Fatalf("bucket %s at offset %d landed on %q (sort %q)", d.Title, offset, landed.Title, sortTitle)
		}
		offset += *d.Size
	}
}
