package plex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestActivePlaybackWatchEvidence(t *testing.T) {
	for _, scenario := range []string{"no bytes", "failed start", "terminal EOF", "wrong duration", "estimated duration", "encoder input only", "pending conversion", "real converted completion", "real direct binding completion", "real completion"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			watch := state.New(store.OpenTest(t), state.Options{})
			f.srv.setState(watch)
			if err := watch.Progress(context.Background(), ownerUser.ID, fightClub.Key, 120*time.Second, 1000*time.Second, "playing"); err != nil {
				t.Fatal(err)
			}
			ps := &core.PlaySession{ID: "evidence", UserID: ownerUser.ID, ClientID: "shield-evidence", Key: fightClub.Key, Info: core.MediaInfo{Duration: 1000 * time.Second}, BytesServed: 1000}
			switch scenario {
			case "no bytes":
				ps.BytesServed = 0
			case "failed start":
				ps.Experience = &core.PlayExperience{StartFailure: "decoder failed"}
			case "estimated duration":
				ps.Info.Estimated = true
			case "encoder input only", "real converted completion":
				f.srv.d.Transcoder = evidenceTranscoder{f.trans, scenario == "real converted completion"}
				f.srv.transcodes.put(&transcodeEntry{id: "no-output", userID: ownerUser.ID, clientID: ps.ClientID, key: ps.Key, started: true})
			case "real direct binding completion":
				f.srv.transcodes.put(&transcodeEntry{id: "direct", userID: ownerUser.ID, clientID: ps.ClientID, key: ps.Key, decision: core.Decision{Kind: core.DecisionDirectPlay}})
			case "pending conversion":
				f.srv.transcodes.put(&transcodeEntry{id: "pending", userID: ownerUser.ID, clientID: ps.ClientID, key: ps.Key})
			}
			f.playback.sessions[ps.ID] = ps
			report := func(status string, pos, duration int) {
				t.Helper()
				rec := f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=%d&duration=%d", fightClub.Key, status, pos, duration), "X-Plex-Client-Identifier", "shield-evidence")
				if rec.Code != 200 {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
			}
			switch scenario {
			case "terminal EOF":
				report("playing", 120000, 1000000)
				report("stopped", 1000000, 1000000)
			case "wrong duration":
				report("playing", 120000, 130000)
				report("playing", 121000, 130000)
			case "real completion", "real converted completion", "real direct binding completion":
				report("playing", 120000, 1000000)
				report("playing", 920000, 1000000)
			default:
				report("playing", 120000, 1000000)
				report("playing", 920000, 1000000)
				report("stopped", 1000000, 1000000)
			}
			got, err := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
			if err != nil {
				t.Fatal(err)
			}
			wantOffset := 120 * time.Second
			if scenario == "wrong duration" {
				wantOffset = 121 * time.Second // valid progress uses the probed duration
			}
			if scenario == "real completion" || scenario == "real converted completion" || scenario == "real direct binding completion" {
				if got.ViewCount != 1 {
					t.Fatalf("real playback not completed: %+v", got)
				}
			} else if got.ViewCount != 0 || got.ViewOffset != wantOffset {
				t.Fatalf("failed/invalid playback changed watch state: %+v", got)
			}
		})
	}
}

type evidenceTranscoder struct {
	*fakeTranscoder
	delivered bool
}

func (e evidenceTranscoder) Session(id string) (core.HLSSession, bool) {
	if id == "no-output" {
		return evidenceHLS{&fakeHLS{id: id}, e.delivered}, true
	}
	return e.fakeTranscoder.Session(id)
}

type evidenceHLS struct {
	*fakeHLS
	delivered bool
}

func (e evidenceHLS) PlaybackDelivered() bool { return e.delivered }
