package plex

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

// A stateful protocol contract across official client headers, both encodings,
// real routing and authentication, and fake media. No live PMS is mutated.
func TestPlaybackCorpusLifecycle(t *testing.T) {
	for profile := range conformplex.Clients() {
		for _, jsonMode := range []bool{false, true} {
			format := "xml"
			if jsonMode {
				format = "json"
			}
			t.Run(profile+"/"+format, func(t *testing.T) {
				f := newFixture()
				defer f.srv.Close()
				f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
				queueID := ""
				for _, probe := range conformplex.PlaybackCorpus() {
					path := strings.NewReplacer("{{machineID}}", f.srv.identity().MachineIdentifier, "{{movieKey}}", fightClub.Key.String(), "{{session}}", "contract-stream", "{{queueID}}", queueID).Replace(probe.Path)
					hdr := map[string]string{}
					key := profile
					if jsonMode {
						key += "-json"
					}
					for k, v := range probe.Headers[key] {
						hdr[k] = strings.ReplaceAll(v[0], "{{token}}", ownerToken)
					}
					if probe.Name == "transcode-start" {
						f.request(t, "GET", decisionPath(path), hdr)
					}
					rec := f.request(t, probe.Method, path, hdr)
					if rec.Code != http.StatusOK {
						t.Fatalf("%s: %d %s", probe.Name, rec.Code, rec.Body.String())
					}
					switch probe.Name {
					case "queue-create", "queue-read":
						attrs := containerAttrs(t, rec.Body.Bytes())
						if attrs["playQueueTotalCount"] != "1" || attrs["playQueueSelectedMetadataItemID"] != fightClub.Key.String() {
							t.Fatalf("queue semantic values: %v", attrs)
						}
						if probe.Name == "queue-create" {
							queueID = attrs["playQueueID"]
							if queueID == "" {
								t.Fatal("missing queue ID")
							}
						}
					case "timeline":
						if len(f.state.progress) != 1 || f.state.progress[0].offset != 12*time.Second || f.state.progress[0].duration != time.Minute {
							t.Fatalf("timeline units: %+v", f.state.progress)
						}
					case "scrobble", "unscrobble":
						st, _ := f.state.Get(context.Background(), ownerUser.ID, fightClub.Key)
						want := 1
						if probe.Name == "unscrobble" {
							want = 0
						}
						if st.ViewCount != want {
							t.Fatalf("%s count=%d want %d", probe.Name, st.ViewCount, want)
						}
					case "transcode-start":
						if !strings.HasPrefix(rec.Body.String(), "#EXTM3U") || !strings.Contains(rec.Body.String(), "session/contract-stream/base/index.m3u8") {
							t.Fatalf("master: %s", rec.Body.String())
						}
					case "transcode-playlist":
						if !strings.Contains(rec.Body.String(), "00000.ts") {
							t.Fatalf("media playlist: %s", rec.Body.String())
						}
					case "transcode-segment":
						if rec.Body.String() != "segment-0" {
							t.Fatalf("segment bytes: %q", rec.Body.String())
						}
						foreign := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": friendToken})
						if foreign.Code != http.StatusForbidden {
							t.Fatalf("foreign segment access: %d", foreign.Code)
						}
					case "transcode-stop":
						if _, ok := f.trans.Session("contract-stream"); ok {
							t.Fatal("encoder session survived stop")
						}
					}
				}
			})
		}
	}
}

type delegationAuthFake struct {
	*fakeAuth
	calls int
}

func (a *delegationAuthFake) IssueDelegation(context.Context, int64) (string, error) {
	a.calls++
	return "server-local-casting-token", nil
}

func TestSecurityTokenNeverReturnsAccountCredential(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	a := &delegationAuthFake{fakeAuth: f.srv.testAuth()}
	f.srv.setTestAuth(a)
	rec := f.get(t, "/security/token", jsonAccept...)
	attrs := containerAttrs(t, rec.Body.Bytes())
	if rec.Code != 200 || attrs["token"] != "server-local-casting-token" || a.calls != 1 || strings.Contains(rec.Body.String(), ownerToken) {
		t.Fatalf("delegation: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("casting token may be cached")
	}
	u := a.users[ownerToken]
	u.Delegated = true
	u.IsOwner = false
	a.users[ownerToken] = u
	if rec := f.get(t, "/security/token"); rec.Code != http.StatusForbidden || a.calls != 1 {
		t.Fatal("delegated credential minted another credential")
	}
	f.srv.setTestAuth(a.fakeAuth)
	if rec := f.get(t, "/security/token"); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), ownerToken) {
		t.Fatal("fallback leaked account credential")
	}
}
