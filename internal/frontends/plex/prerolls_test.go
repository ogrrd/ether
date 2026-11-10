package plex

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakePrerolls struct {
	mu      sync.Mutex
	clip    *core.PrerollClip
	thumb   string
	reqs    []core.PrerollRequest
	started []core.PrerollPlay
	actives int
}

func (f *fakePrerolls) Active(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actives++
	return f.clip != nil
}

func (f *fakePrerolls) Pick(_ context.Context, req core.PrerollRequest) (core.PrerollClip, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.clip == nil || req.Resume {
		return core.PrerollClip{}, false
	}
	return *f.clip, true
}

func (f *fakePrerolls) Clip(_ context.Context, key core.RatingKey) (core.PrerollClip, bool) {
	if f.clip == nil || key != f.clip.Key {
		return core.PrerollClip{}, false
	}
	return *f.clip, true
}

func (f *fakePrerolls) Thumb(context.Context, core.RatingKey) (string, bool) {
	return f.thumb, f.thumb != ""
}

func (f *fakePrerolls) Started(_ context.Context, play core.PrerollPlay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, play)
}

// With no pre-roll configured, play queues must be byte-identical to a
// server without the feature (owner bar).
func TestPrerollAbsentLeavesPlayQueuesByteIdentical(t *testing.T) {
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String()
	targets := []string{
		"/playQueues?type=video&extrasPrefixCount=1&uri=" + urlEsc(uri) + "&key=" + urlEsc("/library/metadata/"+fightClub.Key.String()),
		"/playQueues?type=video&uri=" + urlEsc(uri),
		"/playQueues?type=video&continuous=1&uri=" + urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/"+bbE1.Key.String()),
	}
	run := func(p core.Prerolls) [][]byte {
		f, _, _ := extrasFixture(t)
		f.srv.setPrerolls(p)
		// Artwork colours fill in the background on first sight; they are
		// unrelated to pre-rolls, so keep them out of the comparison.
		f.srv.ultra = nil
		var out [][]byte
		for _, target := range targets {
			for _, accept := range []string{"application/json", "application/xml"} {
				rec := f.request(t, http.MethodPost, target, map[string]string{"Accept": accept})
				out = append(out, rec.Body.Bytes())
			}
		}
		return out
	}
	without := run(nil)
	none := &fakePrerolls{}
	with := run(none)
	for i := range without {
		if !bytes.Equal(without[i], with[i]) {
			j := 0
			for j < len(without[i]) && j < len(with[i]) && without[i][j] == with[i][j] {
				j++
			}
			t.Fatalf("response %d differs at %d: %q vs %q", i, j, without[i][max(0, j-80):min(len(without[i]), j+80)], with[i][max(0, j-80):min(len(with[i]), j+80)])
		}
	}
	if none.actives == 0 || len(none.reqs) != 0 {
		t.Fatalf("with no pre-roll: %d activity checks, %d selections", none.actives, len(none.reqs))
	}
}

func TestPrerollPlayQueueAndDirectPlay(t *testing.T) {
	dir := t.TempDir()
	clipPath := filepath.Join(dir, "1.mp4")
	payload := bytes.Repeat([]byte("preroll!"), 512)
	if err := os.WriteFile(clipPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	clip := core.PrerollClip{Key: core.PrerollKey(1), Name: "Halloween sting", Path: clipPath, Duration: 9500 * time.Millisecond,
		Width: 1920, Height: 1080, BitrateKbps: 6000, Size: int64(len(payload)), Order: core.PrerollBeforeTrailers, UpdatedAt: time.Unix(1700000000, 0)}
	f, _, wp := extrasFixture(t)
	pre := &fakePrerolls{clip: &clip}
	f.srv.setPrerolls(pre)
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String()
	queue := func(target string) []string {
		t.Helper()
		mc := decodeJSON(t, f.request(t, http.MethodPost, target, map[string]string{"Accept": "application/json"}))
		var keys []string
		for _, it := range mc["Metadata"].([]any) {
			keys = append(keys, it.(map[string]any)["ratingKey"].(string))
		}
		if mc["playQueueSelectedMetadataItemID"] != keys[0] {
			t.Fatalf("playback must start at the first prefix item: %v", mc["playQueueSelectedMetadataItemID"])
		}
		return keys
	}
	// Before the Cinema Trailer, then the feature.
	keys := queue("/playQueues?type=video&extrasPrefixCount=1&uri=" + urlEsc(uri) + "&key=" + urlEsc("/library/metadata/"+fightClub.Key.String()))
	if want := []string{clip.Key.String(), alienTrailer.Key.String(), fightClub.Key.String()}; !equalStrings(keys, want) {
		t.Fatalf("queue %v, want %v", keys, want)
	}
	// After the trailer when the rule says so.
	clip.Order = core.PrerollAfterTrailers
	keys = queue("/playQueues?type=video&extrasPrefixCount=1&uri=" + urlEsc(uri) + "&key=" + urlEsc("/library/metadata/"+fightClub.Key.String()))
	if want := []string{alienTrailer.Key.String(), clip.Key.String(), fightClub.Key.String()}; !equalStrings(keys, want) {
		t.Fatalf("queue %v, want %v", keys, want)
	}
	// No extrasPrefixCount: the pre-roll still plays on a fresh play.
	keys = queue("/playQueues?type=video&uri=" + urlEsc(uri))
	if want := []string{clip.Key.String(), fightClub.Key.String()}; !equalStrings(keys, want) {
		t.Fatalf("queue %v, want %v", keys, want)
	}
	if req := pre.reqs[len(pre.reqs)-1]; req.Resume || req.Feature == nil || req.Feature.Key != fightClub.Key || req.Viewer == nil || req.Device == "" {
		t.Fatalf("selection request %+v", req)
	}
	// A saved resume point without extrasPrefixCount is a resume: no pre-roll.
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: time.Minute})
	mc := decodeJSON(t, f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"}))
	if mc["playQueueTotalCount"].(float64) != 1 {
		t.Fatalf("resume got a pre-roll: %v", mc)
	}
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key})

	// The queued pre-roll is a direct-play MP4 clip with its own thumb.
	clip.Order = core.PrerollBeforeTrailers
	mc = decodeJSON(t, f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"}))
	item := mc["Metadata"].([]any)[0].(map[string]any)
	if item["type"] != "clip" || item["title"] != "Halloween sting" || item["thumb"] == "" {
		t.Fatalf("pre-roll item %v", item)
	}
	media := item["Media"].([]any)[0].(map[string]any)
	if media["container"] != "mp4" || media["videoCodec"] != "h264" || media["audioCodec"] != "aac" {
		t.Fatalf("pre-roll media %v", media)
	}
	part := media["Part"].([]any)[0].(map[string]any)["key"].(string)
	before := wp.count()
	rec := f.request(t, http.MethodGet, part, map[string]string{"Range": "bytes=0-99"})
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 100 {
		t.Fatalf("pre-roll part: %d %d", rec.Code, rec.Body.Len())
	}
	_ = f.request(t, http.MethodGet, part, map[string]string{"Range": "bytes=100-"})
	pre.mu.Lock()
	started := append([]core.PrerollPlay(nil), pre.started...)
	pre.mu.Unlock()
	if len(started) == 0 || started[0].Key != clip.Key || started[0].Feature != fightClub.Key || started[0].FeatureTitle != fightClub.Title {
		t.Fatalf("pre-roll start %+v", started)
	}
	if wp.count() < before {
		t.Fatalf("feature prewarm regressed")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
