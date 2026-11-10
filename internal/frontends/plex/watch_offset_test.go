package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Watch progress is the client's /:/timeline time. A transcode session
// status, including one whose encoder is minutes ahead of playback, does
// not write state_watch.
func TestWatchProgressFollowsClientTimelineNotEncoder(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)

	const client = 1426 * time.Second // 23:46
	rec := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=playing&time=1426000&duration=5424620")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", rec.Code, rec.Body.String())
	}
	st, err := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ViewOffset != client {
		t.Fatalf("view offset = %v, want client timeline %v", st.ViewOffset, client)
	}

	// The encoder head from the stalled Abzurdah session (47:54). Nothing
	// in session status may persist it.
	if rec = f.get(t, "/transcode/sessions"); rec.Code != http.StatusOK {
		t.Fatalf("transcode sessions: %d", rec.Code)
	}
	st, err = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ViewOffset != client {
		t.Fatalf("view offset = %v after encoder status, want %v", st.ViewOffset, client)
	}
}

// Reproduces the Apple TV lab A.C.O.D. (2013, 87m43s) sequence (#42):
// manual scrobble + unscrobble, a ~21-minute playback soak across release
// failovers ending in state=stopped, and a brief resume ending in state=stopped.
// At every step after unscrobble, the item must remain in-progress (viewCount
// omitted, viewOffset preserved) and never flip to watched below 90%.
func TestPartialSoakAfterUnscrobbleStaysInProgress(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.setState(watch)

	key := fightClub.Key.String()

	// 1. Earlier manual Mark as Watched followed by Mark as Unwatched.
	if rec := f.get(t, "/:/scrobble?identifier=com.plexapp.plugins.library&key="+key); rec.Code != http.StatusOK {
		t.Fatalf("scrobble: %d", rec.Code)
	}
	if rec := f.get(t, "/:/unscrobble?identifier=com.plexapp.plugins.library&key="+key); rec.Code != http.StatusOK {
		t.Fatalf("unscrobble: %d", rec.Code)
	}

	// 2. 21m33s soak across slightly differing release durations (5263380ms -> 5263174ms)
	// ending with state=stopped at ~24.6% of the 87m43s runtime.
	steps := []struct {
		state    string
		timeMs   string
		duration string
	}{
		{"playing", "0", "5263380"},
		{"playing", "290647", "5263380"},
		{"buffering", "647315", "5263380"},
		{"playing", "647462", "5263174"},
		{"playing", "1289457", "5263174"},
		{"stopped", "1293516", "5263174"},
		// 3. Later brief reopen from Continue Watching / Watch Version check.
		{"playing", "1293516", "5263307"},
		{"playing", "1303152", "5263307"},
		{"stopped", "1310656", "5263307"},
	}
	for _, s := range steps {
		u := "/:/timeline?ratingKey=" + key + "&key=/library/metadata/" + key +
			"&state=" + s.state + "&time=" + s.timeMs + "&duration=" + s.duration
		if rec := f.get(t, u); rec.Code != http.StatusOK {
			t.Fatalf("timeline %+v: %d %s", s, rec.Code, rec.Body.String())
		}
		st, err := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
		if err != nil {
			t.Fatal(err)
		}
		if st.ViewCount != 0 {
			t.Fatalf("after %+v: ViewCount = %d, want 0 (unwatched/in-progress)", s, st.ViewCount)
		}
	}

	st, err := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ViewOffset != 1310656*time.Millisecond {
		t.Fatalf("final ViewOffset = %v, want 1310656ms", st.ViewOffset)
	}

	rec := f.get(t, "/library/metadata/"+key, "Accept", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata: %d", rec.Code)
	}
	var body struct {
		MediaContainer struct {
			Metadata []map[string]any `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.MediaContainer.Metadata) != 1 {
		t.Fatalf("metadata len = %d, want 1", len(body.MediaContainer.Metadata))
	}
	md := body.MediaContainer.Metadata[0]
	if _, watched := md["viewCount"]; watched {
		t.Fatalf("metadata has viewCount=%v after 21m/87m soak, want omitted", md["viewCount"])
	}
	if got := md["viewOffset"]; got != float64(1310656) {
		t.Fatalf("metadata viewOffset = %v, want 1310656", got)
	}
}
