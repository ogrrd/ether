package plex

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// The entire option set is checked before any lookup/warm-up, so a malformed
// asynchronous flag cannot trigger work or change the failure to a lookup error.
func TestMetadataOptionsValidateBeforeLookup(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setCatalog(nil)
	for _, name := range []string{"checkFiles", "skipRefresh", "asyncCheckFiles", "asyncAugmentMetadata", "asyncRefreshLocalMediaAgent", "asyncRefreshAnalysis"} {
		for _, value := range []string{"", "abc", "2", "true", "-1", "1.5", " 1", "1 "} {
			path := "/library/metadata/1000000000000550?" + url.Values{name: {value}}.Encode()
			if rec := f.get(t, path); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "400") {
				t.Fatalf("%s=%q: %d %s", name, value, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestMetadataOptionsPaddedIncludesAndRelatedCount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, value := range []string{"0", "1", "00", "01"} {
		for _, name := range []string{"checkFiles", "skipRefresh", "asyncCheckFiles", "asyncAugmentMetadata", "asyncRefreshLocalMediaAgent", "asyncRefreshAnalysis"} {
			if rec := f.get(t, "/library/metadata/"+fightClub.Key.String()+"?"+name+"="+value); rec.Code != 200 {
				t.Fatalf("%s=%s: %d %s", name, value, rec.Code, rec.Body.String())
			}
		}
	}
	show := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"?includeChildren=001").Body.String()
	if !strings.Contains(show, "<Children") {
		t.Fatalf("padded show children missing: %s", show)
	}
	if childStart := strings.Index(show, "<Children"); childStart >= 0 && strings.Contains(show[childStart:], "librarySection") {
		t.Fatalf("inline child repeats container section fields: %s", show)
	}
	season := f.get(t, "/library/metadata/"+bbS1.Key.String()+"?includeChildren=1").Body.String()
	if strings.Contains(season, "<Children") {
		t.Fatalf("season inlined children: %s", season)
	}
	f.srv.recommender().(*fakeRecommender).related = []core.Hub{{ID: "similar", Title: "More Like This", Type: core.TypeMovie, Items: []core.Title{alien}}}
	base := "/library/metadata/" + fightClub.Key.String() + "?includeRelated=01"
	body := f.get(t, base).Body.String()
	if !strings.Contains(body, "<Related>") || !strings.Contains(body, alien.Title) {
		t.Fatalf("padded related missing: %s", body)
	}
	body = f.get(t, base+"&includeRelatedCount=0").Body.String()
	i := strings.Index(body, "<Related>")
	if i < 0 || strings.Contains(body[i:], alien.Title) || !strings.Contains(body[i:], `more="1"`) || !strings.Contains(body[i:], `size="0"`) {
		t.Fatalf("zero related count: %s", body)
	}
}
