package plex

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestInfuseDirectPartUsesMinuteResumeThreshold(t *testing.T) {
	for _, tc := range []struct {
		offset int
		resume bool
	}{{30000, false}, {60000, false}, {61000, true}, {120000, true}, {201274, true}} {
		t.Run(fmt.Sprint(tc.offset), func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.srv.setState(state.New(store.OpenTest(t), state.Options{}))
			headers := map[string]string{"Accept": "application/json", "X-Plex-Product": "Infuse", "X-Plex-Platform": "tvOS", "X-Plex-Client-Identifier": "infuse-direct"}
			partHeaders := make(map[string]string, len(headers)+1)
			for key, value := range headers {
				partHeaders[key] = value
			}
			partHeaders["Range"] = "bytes=0-9"
			part := "/library/parts/" + fightClub.Key.String() + "/1759276800/file.mkv"
			if rec := f.request(t, http.MethodGet, part, partHeaders); rec.Code != http.StatusPartialContent {
				t.Fatalf("direct part: %d %s", rec.Code, rec.Body.String())
			}
			ps, ok := f.playback.SessionFor(ownerUser.ID, "infuse-direct", fightClub.Key)
			if !ok {
				t.Fatal("missing direct playback")
			}
			ps.BytesServed = 10 // the fake range server delivered the requested bytes
			for _, playbackState := range []string{"playing", "stopped"} {
				path := fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=%d&duration=5820000", fightClub.Key, playbackState, tc.offset)
				if rec := f.request(t, http.MethodPost, path, headers); rec.Code != http.StatusOK {
					t.Fatalf("timeline %s: %d %s", playbackState, rec.Code, rec.Body.String())
				}
			}
			metadata := decodeJSON(t, f.request(t, http.MethodGet, "/library/metadata/"+fightClub.Key.String(), headers))
			items, ok := metadata["Metadata"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("metadata: %+v", metadata)
			}
			item := items[0].(map[string]any)
			offset, _ := item["viewOffset"].(float64)
			if (offset > 0) != tc.resume || (tc.resume && offset != float64(tc.offset)) {
				t.Fatalf("metadata viewOffset=%v, want resume=%v offset=%d", offset, tc.resume, tc.offset)
			}
			continueBody := decodeJSON(t, f.request(t, http.MethodGet, "/hubs/continueWatching", headers))
			hubs, _ := continueBody["Hub"].([]any)
			found := false
			for _, rawItem := range anySlice(continueBody["Metadata"]) {
				if item, ok := rawItem.(map[string]any); ok && item["ratingKey"] == fightClub.Key.String() {
					found = true
				}
			}
			for _, raw := range hubs {
				hub := raw.(map[string]any)
				for _, rawItem := range anySlice(hub["Metadata"]) {
					if item, ok := rawItem.(map[string]any); ok && item["ratingKey"] == fightClub.Key.String() {
						found = true
					}
				}
			}
			if found != tc.resume {
				t.Fatalf("Continue Watching contains movie=%v, want %v: %+v", found, tc.resume, continueBody)
			}
		})
	}
}

func anySlice(v any) []any {
	items, _ := v.([]any)
	return items
}
