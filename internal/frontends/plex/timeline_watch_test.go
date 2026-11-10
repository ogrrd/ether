package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPlexTimelinePlayLatches(t *testing.T) {
	cases := []struct {
		name  string
		steps []struct {
			key            core.RatingKey
			position       int
			status, answer string
			offset         int
			count          int
		}
	}{}
	// Independent cases start with pristine state and a single client's track.
	type beat = struct {
		key            core.RatingKey
		position       int
		status, answer string
		offset         int
		count          int
	}
	a, b := fightClub.Key, alien.Key
	cases = append(cases,
		struct {
			name  string
			steps []beat
		}{"cold end and zero cannot complete", []beat{{a, 950000, "playing", "ignore", 0, 0}, {a, 0, "playing", "ignore", 0, 0}, {a, 950000, "stopped", "ignore", 0, 0}}},
		struct {
			name  string
			steps []beat
		}{"completion closes on stop", []beat{{a, 3000, "playing", "ignore", 0, 0}, {a, 920000, "playing", "complete", 0, 1}, {a, 930000, "stopped", "complete", 0, 1}, {a, 950000, "playing", "ignore", 0, 1}}},
		struct {
			name  string
			steps []beat
		}{"stop never clears resume", []beat{{a, 70000, "playing", "progress", 70000, 0}, {a, 3000, "stopped", "progress", 70000, 0}, {a, 3000, "paused", "ignore", 70000, 0}}},
		struct {
			name  string
			steps []beat
		}{"progress survives stop", []beat{{a, 70000, "playing", "progress", 70000, 0}, {a, 75000, "stopped", "progress", 75000, 0}, {a, 3000, "playing", "progress", 0, 0}}},
		struct {
			name  string
			steps []beat
		}{"item switch resets resume latch", []beat{{a, 70000, "playing", "progress", 70000, 0}, {b, 3000, "playing", "ignore", 0, 0}, {a, 3000, "playing", "ignore", 70000, 0}}},
		struct {
			name  string
			steps []beat
		}{"crossing reads session previous item", []beat{{a, 3000, "playing", "ignore", 0, 0}, {b, 920000, "playing", "complete", 0, 1}, {a, 930000, "stopped", "ignore", 0, 0}}},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			watch := state.New(store.OpenTest(t), state.Options{})
			f.srv.setState(watch)
			for i, step := range tc.steps {
				rec := f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=%d&duration=1000000", step.key, step.status, step.position), "Accept", "application/json")
				if rec.Code != http.StatusOK {
					t.Fatalf("step %d: %d %s", i, rec.Code, rec.Body.String())
				}
				var got struct {
					MC struct {
						PlaybackState string `json:"playbackState"`
						ViewCount     int    `json:"viewCount"`
						ViewOffset    int    `json:"viewOffset"`
					} `json:"MediaContainer"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				wantOffset := step.offset
				if wantOffset == 0 {
					wantOffset = -1
				}
				if got.MC.PlaybackState != step.answer || got.MC.ViewCount != step.count || got.MC.ViewOffset != wantOffset {
					t.Fatalf("step %d: body %s; want %s count=%d offset=%d", i, rec.Body.String(), step.answer, step.count, wantOffset)
				}
			}
		})
	}
}

func TestPlexTimelineNoopDurationAndOffline(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	at := time.Unix(1000000, 0)
	watch := state.New(store.OpenTest(t), state.Options{Now: func() time.Time { return at }})
	f.srv.setState(watch)
	key := fightClub.Key.String()
	call := func(query string) {
		t.Helper()
		rec := f.get(t, "/:/timeline?ratingKey="+key+"&"+query)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
	}
	call("state=playing&time=70000&duration=1000000")
	before, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	at = at.Add(time.Hour)
	for _, q := range []string{"state=playing&time=70000&duration=1000000", "state=playing&time=80000", "state=playing&time=80000&duration=0"} {
		call(q)
	}
	after, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if after.ViewOffset != before.ViewOffset || !after.LastViewedAt.Equal(before.LastViewedAt) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("no-op changed state: %+v -> %+v", before, after)
	}
	rec := f.get(t, "/:/timeline?ratingKey="+key+"&state=playing&time=1000001&duration=1000000")
	if rec.Code != 400 {
		t.Fatalf("impossible time: %d", rec.Code)
	}
	call("state=stopped&offline=1&viewCount=3")
	after, _ = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if after.ViewCount != 1 || after.ViewOffset != 0 {
		t.Fatalf("offline mark: %+v", after)
	}
	call("state=stopped&offline=1&viewCount=0")
	after, _ = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if after.ViewCount != 0 {
		t.Fatalf("offline clear: %+v", after)
	}
}

func TestPlexTimelineQueueResolutionAndPlaylistTrack(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)
	q, err := f.srv.queues.CreateWith(ownerUser.ID, "client-1", []core.RatingKey{fightClub.Key, alien.Key}, fixedNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	beat := func(key core.RatingKey, position int, extra string) {
		t.Helper()
		rec := f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=playing&duration=1000000&time=%d%s", key, position, extra))
		if rec.Code != 200 {
			t.Fatalf("timeline %d %s", rec.Code, rec.Body.String())
		}
	}
	beat(fightClub.Key, 3000, "")
	beat(fightClub.Key, 920000, fmt.Sprintf("&playQueueItemID=%d", q.Items[1].ID))
	a, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	b, _ := watch.Get(context.Background(), ownerUser.ID, alien.Key)
	if a.ViewCount != 0 || b.ViewCount != 1 {
		t.Fatalf("queue resolution: A=%+v B=%+v", a, b)
	}
	selected, _ := f.srv.queues.Get(q.ID, ownerUser.ID)
	if selected.Selected != 1 || selected.Version != q.Version {
		t.Fatalf("selection/version: %+v", selected)
	}
	beat(fightClub.Key, 70000, "")
	selected, _ = f.srv.queues.Get(q.ID, ownerUser.ID)
	if selected.Selected != 0 {
		t.Fatal("omitted selector did not follow owning client")
	}
	beat(alien.Key, 70000, "&playQueueItemID=")
	selected, _ = f.srv.queues.Get(q.ID, ownerUser.ID)
	if selected.Selected != 0 {
		t.Fatal("empty selector changed selection")
	}
	pl, err := f.pls.Create(context.Background(), ownerUser.ID, "Timeline list", []core.RatingKey{fightClub.Key})
	if err != nil {
		t.Fatal(err)
	}
	beat(pl.Key, 920000, "")
	list, _ := watch.Get(context.Background(), ownerUser.ID, pl.Key)
	if list.ViewCount != 0 {
		t.Fatal("media beat crossed into playlist track")
	}
	beat(pl.Key, 3000, "")
	beat(pl.Key, 920000, "")
	list, _ = watch.Get(context.Background(), ownerUser.ID, pl.Key)
	if list.ViewCount != 1 {
		t.Fatal("playlist did not complete its own crossing")
	}
	history, err := watch.History(context.Background(), ownerUser.ID, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range history {
		if h.Key == pl.Key {
			t.Fatal("playlist completion created media history")
		}
	}
	mc := decodeJSON(t, f.get(t, "/status/sessions", jsonAccept...))
	rows, _ := mc["Metadata"].([]any)
	found := false
	for _, raw := range rows {
		if raw.(map[string]any)["ratingKey"] == pl.Key.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("playlist timeline missing Now Playing identity")
	}
}

func TestPlexTimelineIntegerThresholdForShortContent(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)
	for _, position := range []int{1, 900} {
		rec := f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=playing&time=%d&duration=1001", fightClub.Key, position))
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	}
	got, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if got.ViewCount != 1 {
		t.Fatalf("integer threshold: %+v", got)
	}
}

func TestPlexTimelineCrossingIsolatedByClientAndValidation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)
	key := fightClub.Key.String()
	call := func(query, client string) int {
		t.Helper()
		return f.get(t, "/:/timeline?ratingKey="+key+"&"+query, "X-Plex-Client-Identifier", client).Code
	}
	if call("state=playing&time=3000&duration=1000000", "one") != 200 {
		t.Fatal("seed")
	}
	if call("state=playing&time=920000&duration=1000000", "two") != 200 {
		t.Fatal("cold client")
	}
	got, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if got.ViewCount != 0 {
		t.Fatal("other client's position armed a completion")
	}
	if call("state=playing&time=1000001&duration=1000000", "one") != 400 {
		t.Fatal("out-of-range report accepted")
	}
	if call("state=stopped&time=920000&duration=1000000", "one") != 200 {
		t.Fatal("crossing")
	}
	got, _ = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if got.ViewCount != 1 {
		t.Fatal("refused beat consumed the valid client's crossing")
	}
	for _, count := range []string{"-1", "oops"} {
		if call("state=stopped&offline=1&viewCount="+count, "one") != 200 {
			t.Fatal("invalid offline value must be ignored")
		}
	}
	got, _ = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if got.ViewCount != 1 {
		t.Fatal("invalid offline value changed viewed count")
	}
}

func TestLegacyProgressGrammarAndRawPersistence(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)
	k := fightClub.Key.String()
	for _, tc := range []struct {
		q      string
		status int
	}{
		{"key=" + k + "&time=120000", 400},
		{"key=" + k + "&identifier=foreign&time=oops", 404},
		{"key=/library/metadata/" + k + "&identifier=com.plexapp.plugins.library&time=120000", 500},
		{"key=" + k + "&identifier=com.plexapp.plugins.library&time=-1", 200},
		{"key=1000000000999999&identifier=com.plexapp.plugins.library&time=120000", 200},
		{"key=" + k + "&identifier=com.plexapp.plugins.library&time=%2B999999999", 200},
	} {
		rec := f.get(t, "/:/progress?"+tc.q)
		if rec.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.q, rec.Code, rec.Body.String())
		}
		if tc.status == 200 && (rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "text/html") {
			t.Fatal("legacy write response shape")
		}
	}
	got, _ := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if got.ViewOffset != 999999999*time.Millisecond || got.ViewCount != 0 {
		t.Fatalf("legacy position clamped/completed: %+v", got)
	}
	rows, err := watch.History(context.Background(), ownerUser.ID, time.Time{}, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("legacy created history: %+v %v", rows, err)
	}
	unknown, _ := watch.Get(context.Background(), ownerUser.ID, core.MovieKey(999999999))
	if unknown.ViewOffset != 0 {
		t.Fatal("unknown key wrote state")
	}
}
