package plex

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type completedEpisodeState struct {
	*fakeState
	entries []core.HistoryEntry
}

func (s *completedEpisodeState) History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) {
	return s.entries, nil
}

// Home must use the local recovery capability for both batch selection and
// walking past watched episodes, rather than the provider-waiting API.
type continueCatalog struct {
	*fakeCatalog
	calls int
}

func (c *continueCatalog) ContinueEpisodes(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]*core.Title, error) {
	c.calls++
	out := map[core.RatingKey]*core.Title{}
	for _, key := range keys {
		if next, err := c.fakeCatalog.NextEpisode(ctx, key); err == nil {
			out[key] = next
		}
	}
	return out, nil
}
func (c *continueCatalog) NextEpisode(context.Context, core.RatingKey) (*core.Title, error) {
	panic("Home called provider-waiting NextEpisode")
}

func TestContinueWatchingAdvancesCompletedEpisode(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	local := &continueCatalog{fakeCatalog: f.catalog}
	f.srv.setCatalog(local)
	f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
	history := &completedEpisodeState{f.state, []core.HistoryEntry{{Key: bbE1.Key, ViewedAt: fixedNow, Completed: true}}}
	f.srv.setState(history)
	for _, path := range []string{"/hubs/continueWatching/items", "/library/onDeck", "/hubs/sections/2/continueWatching/items"} {
		mc := decodeJSON(t, f.get(t, path, jsonAccept...))
		rows, _ := mc["Metadata"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["ratingKey"] != bbE2.Key.String() {
			t.Fatalf("%s: %v", path, mc)
		}
		if off, ok := rows[0].(map[string]any)["viewOffset"]; ok && off != float64(0) {
			t.Fatal("next episode inherited resume offset", off)
		}
	}
	f.state.set(ownerUser.ID, ws(bbE2.Key, 30000, 0))
	mc := decodeJSON(t, f.get(t, "/hubs/continueWatching/items", jsonAccept...))
	rows := mc["Metadata"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["viewOffset"] != float64(30000) {
		t.Fatal("resume duplicated or lost", mc)
	}
	f.state.set(ownerUser.ID, ws(bbE2.Key, 0, 1))
	history.entries = []core.HistoryEntry{{Key: bbE2.Key, ViewedAt: fixedNow, Completed: true}}
	mc = decodeJSON(t, f.get(t, "/hubs/continueWatching/items", jsonAccept...))
	if mc["size"] != float64(0) {
		t.Fatal("finished series did not leave row", mc)
	}
}
