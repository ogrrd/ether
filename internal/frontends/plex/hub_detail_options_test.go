package plex

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestHomeHubStructuralOptionsAndInlineOnDeckExclusions(t *testing.T) {
	f := newFixture(func(d *testServerDeps) {
		d.Recommender = &fakeRecommender{home: []core.Hub{{ID: "tv.test", Title: "Series", Type: core.TypeShow, Items: []core.Title{breakingBad}}}}
	})
	defer f.srv.Close()
	row := func(path string) map[string]any {
		t.Helper()
		mc := decodeJSON(t, f.get(t, path, jsonAccept...))
		hubs, _ := mc["Hub"].([]any)
		for _, raw := range hubs {
			h := raw.(map[string]any)
			if h["hubIdentifier"] == "tv.test" {
				rows := h["Metadata"].([]any)
				return rows[0].(map[string]any)
			}
		}
		t.Fatal("missing show shelf")
		return nil
	}
	plain := row("/hubs")
	if _, ok := plain["OnDeck"]; ok {
		t.Fatal("unrequested inline state block")
	}
	full := row("/hubs?includeChildren=1&includeOnDeck=1&includeExtras=1&includeRelated=1&includePreferences=1")
	for _, name := range []string{"Children", "OnDeck", "Extras", "Related", "Preferences"} {
		if _, ok := full[name]; !ok {
			t.Fatalf("requested %s absent: %+v", name, full)
		}
	}
	trimmed := row("/hubs?includeOnDeck=1&includeExtras=1&excludeElements=Media,Guid,Image")
	deck := trimmed["OnDeck"].(map[string]any)["Metadata"].(map[string]any)
	for _, name := range []string{"Media", "Guid", "Image"} {
		if _, ok := deck[name]; ok {
			t.Fatalf("inline OnDeck kept excluded %s", name)
		}
	}
	if _, ok := deck["mediaCount"]; !ok {
		t.Fatal("excluded inline media lost its count")
	}
}
