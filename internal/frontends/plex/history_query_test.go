package plex

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// These cases distinguish a view-type selector from a metadata-type selector,
// and ensure early empty results cannot hide malformed identifiers.
func TestHistoryQueryGrammarAndScope(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setState(historyState{fakeState: f.state, hist: []core.HistoryEntry{
		{UserID: ownerUser.ID, Key: fightClub.Key, ViewedAt: fixedNow, ViewType: 1},
		{UserID: ownerUser.ID, Key: bbE1.Key, ViewedAt: fixedNow, ViewType: 1},
	}})
	for _, tc := range []struct {
		query          string
		code           int
		movie, episode bool
	}{
		{"", 200, false, false},
		{"type=1", 200, true, true},
		{"type=4", 200, false, false},
		{"type=1,4", 200, true, true},
		{"type=01", 200, true, true},
		{"type=abc", 400, false, false},
		{"type=", 400, false, false},
		{"type=1&limit=1", 200, true, false},
		{"type=1&limit=0", 200, false, false},
		{"type=1&limit=-1", 200, false, false},
		{"type=1&limit=1.5", 400, false, false},
		{"type=1&group=", 400, false, false},
		{"type=1&librarySectionID=1", 200, true, false},
		{"type=1&librarySectionID=2", 200, false, true},
		{"type=1&librarySectionID=99", 200, false, false},
		{"type=1&accountID=99999", 200, false, false},
		{fmt.Sprintf("type=1&accountID=%d", ownerUser.PlexID), 200, true, true},
		{"type=1&metadataItemID=" + fightClub.Key.String(), 200, true, false},
		{"metadataItemID=0", 404, false, false},
		{"type=1&deviceID=abc", 200, true, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := f.get(t, "/status/sessions/history/all?"+tc.query, "Accept", "application/json")
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, fightClub.Title) != tc.movie || strings.Contains(body, bbE1.Title) != tc.episode {
				t.Fatalf("unexpected scoped history: %s", body)
			}
			if tc.movie && !strings.Contains(body, `"viewType":1`) {
				t.Fatalf("missing view type: %s", body)
			}
		})
	}
	for _, name := range []string{"accountID", "librarySectionID", "metadataItemID", "viewedAt>", "limit"} {
		for _, value := range []string{"", "abc", "1.5", " 1"} {
			q := url.Values{name: []string{value}}
			rec := f.get(t, "/status/sessions/history/all?"+q.Encode())
			if rec.Code != 400 {
				t.Errorf("%s=%q without type: status %d", name, value, rec.Code)
			}
		}
	}
}

// Existing lists resolve through their own stores, not the title catalog.
// Neither list expands its members into a per-item history request.
func TestHistoryListMetadataIdentity(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	pl, err := f.pls.Create(context.Background(), ownerUser.ID, "History list", []core.RatingKey{fightClub.Key})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []core.RatingKey{pl.Key, core.CollectionKey(8091)} {
		for _, suffix := range []string{"", "&type=1"} {
			rec := f.get(t, "/status/sessions/history/all?metadataItemID="+key.String()+suffix, "Accept", "application/json")
			if rec.Code != 200 || decodeJSON(t, rec)["size"].(float64) != 0 {
				t.Fatalf("existing list %s: %d %s", key, rec.Code, rec.Body.String())
			}
		}
	}
	for _, key := range []core.RatingKey{core.PlaylistKey(999), core.CollectionKey(999)} {
		if rec := f.get(t, "/status/sessions/history/all?type=1&metadataItemID="+key.String()); rec.Code != 404 {
			t.Fatalf("missing list %s: %d", key, rec.Code)
		}
	}
	rec := f.get(t, "/status/sessions/history/all?type=1&metadataItemID="+pl.Key.String(), "X-Plex-Token", friendToken)
	if rec.Code != 404 {
		t.Fatalf("private playlist visible: %d", rec.Code)
	}
}
