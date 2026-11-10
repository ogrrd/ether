package plex

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
)

func TestTimelineBandwidthDecisionLifecycle(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info.BitrateKbps = 485
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv", BitrateKbps: 1234}
	headers := []string{"Accept", "application/json", "X-Plex-Product", "Plex for iOS", "X-Plex-Platform", "iOS", "X-Plex-Session-Identifier", "viewer-a"}
	timeline := func(key core.RatingKey, state string, h []string) map[string]any {
		t.Helper()
		r := f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=10000&duration=8340000", key, state), h...)
		if r.Code != 200 {
			t.Fatalf("timeline%d %s", r.Code, r.Body.String())
		}
		return decodeJSON(t, r)
	}
	requireAbsent := func(mc map[string]any) {
		t.Helper()
		if mc["Bandwidths"] != nil || mc["size"] != float64(transcodeBlockCount(mc)) {
			t.Fatalf("unexpected bandwidth %v", mc)
		}
	}
	requirePresent := func(mc map[string]any) {
		t.Helper()
		if mc["size"] != float64(1+transcodeBlockCount(mc)) {
			t.Fatalf("size%v", mc)
		}
		blocks, ok := mc["Bandwidths"].([]any)
		if !ok || len(blocks) != 1 {
			t.Fatalf("blocks%v", mc)
		}
		bw := blocks[0].(map[string]any)["Bandwidth"].([]any)[0].(map[string]any)
		if bw["time"] != float64(0) || bw["bandwidth"] != float64(509) || bw["resolution"] != "1080p" {
			t.Fatalf("bandwidth %v", bw)
		}
	}
	requireAbsent(timeline(fightClub.Key, "playing", headers))
	arm := func() {
		r := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&session=encoder-a&hasMDE=1&directPlay=0&protocol=hls", headers...)
		if r.Code != 200 {
			t.Fatalf("decision%d %s", r.Code, r.Body.String())
		}
	}
	arm()
	requirePresent(timeline(fightClub.Key, "playing", headers))
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=/library/metadata/"+fightClub.Key.String()+"&session=encoder-a&hasMDE=1&directPlay=0&protocol=hls", headers...)
	if start.Code != 200 {
		t.Fatalf("start%d %s", start.Code, start.Body.String())
	}
	converting := timeline(fightClub.Key, "playing", headers)
	requirePresent(converting)
	if transcodeBlockCount(converting) != 1 {
		t.Fatalf("missing started conversion %v", converting)
	}
	ts := converting["TranscodeSession"].([]any)[0].(map[string]any)
	if ts["key"] != "/transcode/sessions/encoder-a" || ts["protocol"] != "hls" {
		t.Fatalf("conversion identity %v", ts)
	}

	other := append([]string(nil), headers...)
	other[len(other)-1] = "viewer-b"
	requireAbsent(timeline(fightClub.Key, "playing", other))
	requireAbsent(timeline(alien.Key, "stopped", headers))
	requirePresent(timeline(fightClub.Key, "paused", headers))
	for _, product := range []string{"Plex for Windows", "Plex for Windows Phone"} {
		h := append([]string(nil), headers...)
		h[3] = product
		requireAbsent(timeline(fightClub.Key, "playing", h))
		response := f.get(t, "/status/sessions", jsonAccept...)
		listed := decodeJSON(t, response)["Metadata"].([]any)
		matched := false
		for _, row := range listed {
			md := row.(map[string]any)
			session, ok := md["Session"].(map[string]any)
			if ok && session["id"] == "viewer-a" {
				matched = true
				if session["bandwidth"] != float64(509) {
					t.Fatalf("Windows session budget%v", session)
				}
			}
		}
		if !matched {
			t.Fatal("Windows session lost its public Session block")
		}
	}
	requirePresent(timeline(fightClub.Key, "stopped", headers))
	requireAbsent(timeline(fightClub.Key, "playing", headers))
	arm()
	requirePresent(timeline(fightClub.Key, "playing", headers))
}

func TestTimelineStopDisarmsAcrossNodesAndRetainsSessionLatch(t *testing.T) {
	db := store.OpenTest(t)
	a := newFixture(func(d *testServerDeps) { d.DB = db })
	defer a.srv.Close()
	b := newFixture(func(d *testServerDeps) { d.DB = db })
	defer b.srv.Close()
	pr := &Request{User: &ownerUser, ClientID: "player", SessionID: "public-session"}
	a.srv.armDecision(pr, fightClub.Key)
	b.srv.armDecision(pr, fightClub.Key)
	a.srv.disarmDecision(pr, alien.Key)
	if !b.srv.decisionArmed(pr, fightClub.Key) {
		t.Fatal("another item stopped this arm")
	}
	a.srv.disarmDecision(pr, fightClub.Key)
	if b.srv.decisionArmed(pr, fightClub.Key) {
		t.Fatal("stale memory allowed an item stopped on another node")
	}
	if !b.srv.decisionArmed(pr, 0) {
		t.Fatal("stop erased the session's decision-seen latch")
	}
	b.srv.refreshDecisionArms(b.srv.now())
	if a.srv.decisionArmed(pr, fightClub.Key) {
		t.Fatal("janitor resurrected stopped item")
	}
	b.srv.armDecision(pr, fightClub.Key)
	if !a.srv.decisionArmed(pr, fightClub.Key) {
		t.Fatal("fresh decision did not rearm shared item")
	}
}

func transcodeBlockCount(mc map[string]any) int {
	v, _ := mc["TranscodeSession"].([]any)
	return len(v)
}
