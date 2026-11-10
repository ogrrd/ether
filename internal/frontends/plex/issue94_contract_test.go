package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// This independent wire fixture follows the public Plezy contract inspected at
// 1dd75b3e5e95913cdaa5b8c22911490e72dfb7c5; it is not a client implementation.
type issue94Indexer struct{ releases []core.Release }

func (i *issue94Indexer) Name() string { return "offline" }
func (i *issue94Indexer) Search(context.Context, core.SearchRequest) ([]core.Release, error) {
	return i.releases, nil
}
func (i *issue94Indexer) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	return []byte(r.ID), nil
}

type issue94Source struct {
	*bytes.Reader
	id string
}

func (s *issue94Source) Size() int64       { return int64(s.Len()) }
func (s *issue94Source) Name() string      { return s.id + ".mkv" }
func (s *issue94Source) ReleaseID() string { return s.id }
func (s *issue94Source) Close() error      { return nil }

type issue94Engine struct{}

func (issue94Engine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	return &issue94Source{bytes.NewReader(bytes.Repeat([]byte("x"), 65536)), id}, nil
}
func (issue94Engine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

type issue94Transcoder struct{ *fakeTranscoder }

func (tr *issue94Transcoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

func issue94Fixture(t *testing.T, with720 bool, profile string) (*fixture, *playback.Manager) {
	t.Helper()
	cfg := config.Defaults(t.TempDir())
	if profile != "default" {
		cfg.Quality.Default = "test"
		min := 0
		if profile == "min1080" {
			min = 1080
		}
		cfg.Quality.Profiles = []core.QualityProfile{{Name: "test", MinHeight: min, MaxHeight: 1080}}
		cfg.Quality.Rules = nil
		cfg.Quality.Remote = ""
	}
	releases := []core.Release{{ID: "hd", GUID: "hd", Indexer: "offline", Name: "Fjord.Harbor.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, PubDate: time.Now().Add(-24 * time.Hour), NZBURL: "offline"}}
	if with720 {
		releases = append(releases, core.Release{ID: "sd", GUID: "sd", Indexer: "offline", Name: "Fjord.Harbor.1999.720p.WEB-DL.H264-GRP", Size: 2000 << 20, PubDate: time.Now().Add(-24 * time.Hour), NZBURL: "offline"})
	}
	rs := resolver.New(&issue94Indexer{releases}, nil, nil, cfg, resolver.Options{})
	t.Cleanup(func() { rs.Close() })
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true; d.Hooks = Hooks{} })
	tr := &issue94Transcoder{newFakeTranscoder(core.Decision{})}
	mgr, err := playback.New(playback.Deps{Catalog: f.catalog, Resolver: rs, Engine: issue94Engine{}, Transcoder: tr, SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return rs.ProfileFor(c, u) }, NativeProbe: func(_ context.Context, s core.MediaSource) (*core.MediaInfo, error) {
		info := sampleInfo
		info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
		if s.ReleaseID() == "sd" {
			info.Streams[0].Height = 720
			info.Streams[0].Width = 1280
			info.BitrateKbps = 1800
		}
		return &info, nil
	}}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.d.Playback = mgr
	f.srv.d.Transcoder = tr
	t.Cleanup(func() { f.srv.Close(); mgr.Close() })
	return f, mgr
}
func issue94Query(id string, kbps int) string {
	q := url.Values{"path": {"/library/metadata/1000000000000550"}, "mediaIndex": {"0"}, "partIndex": {"0"}, "protocol": {"hls"}, "hasMDE": {"1"}, "fastSeek": {"1"}, "directPlay": {"0"}, "directStream": {"0"}, "location": {"lan"}, "session": {id}, "videoResolution": {"1280x720"}, "videoQuality": {"60"}, "subtitles": {"none"}, "X-Plex-Session-Identifier": {"play-id"}, "X-Plex-Client-Profile-Extra": {fmt.Sprintf("add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.bitrate&value=%d&replace=true)+add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mp4&videoCodec=h264&audioCodec=aac)", kbps)}}
	return q.Encode()
}
func TestIssue94PlezySequence(t *testing.T) {
	for _, profile := range []string{"default", "open", "min1080"} {
		for _, alt := range []bool{false, true} {
			for _, kbps := range []int{2000, 3000, 4000} {
				for _, switching := range []bool{false, true} {
					name := fmt.Sprintf("%s/alt=%t/kbps=%d/switch=%t", profile, alt, kbps, switching)
					t.Run(name, func(t *testing.T) {
						f, m := issue94Fixture(t, alt, profile)
						hdr := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "play-id", "Accept", "application/json"}
						if switching {
							origQ := url.Values{"path": {"/library/metadata/1000000000000550"}, "session": {"trans-id"}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}}
							original := f.get(t, "/video/:/transcode/universal/decision?"+origQ.Encode(), hdr...)
							if original.Code != 200 {
								t.Fatalf("original decision %d", original.Code)
							}
							r := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", append(hdr, "Range", "bytes=0-9")...)
							if r.Code != 206 {
								t.Fatalf("original part %d %s", r.Code, r.Body.String())
							}
							if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
								t.Fatal("no original session")
							}
							r = f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", hdr...)
							if r.Code != 200 {
								t.Fatalf("timeline %d", r.Code)
							}
						}
						q := issue94Query("trans-id", kbps)
						d := f.get(t, "/video/:/transcode/universal/decision?"+q, hdr...)
						start := f.get(t, "/video/:/transcode/universal/start.m3u8?"+q, hdr...)
						ps, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
						release := "none"
						if ok && ps.Resolution != nil {
							release = ps.Resolution.Release.ID
						}
						var response struct {
							MediaContainer struct {
								GeneralCode int    `json:"generalDecisionCode"`
								GeneralText string `json:"generalDecisionText"`
							} `json:"MediaContainer"`
						}
						if err := json.Unmarshal(d.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						if d.Code != 200 {
							t.Fatalf("decision HTTP %d", d.Code)
						}
						if alt {
							if start.Code != 200 || release != "sd" {
								t.Fatalf("expected 720 source and HLS: start=%d release=%s decision=%d %s", start.Code, release, response.MediaContainer.GeneralCode, response.MediaContainer.GeneralText)
							}
						} else {
							if start.Code != 404 || release != "none" || response.MediaContainer.GeneralCode != 2000 || !strings.Contains(response.MediaContainer.GeneralText, "no playable release") {
								t.Fatalf("expected unavailable: start=%d release=%s decision=%d %s", start.Code, release, response.MediaContainer.GeneralCode, response.MediaContainer.GeneralText)
							}
						}
						t.Logf("decision HTTP=%d general=%d text=%q diagnostic start=%d release=%s", d.Code, response.MediaContainer.GeneralCode, response.MediaContainer.GeneralText, start.Code, release)
					})
				}
			}
		}
	}
}

func TestIssue94SessionTiming(t *testing.T) {
	for _, transID := range []string{"old-trans", "new-trans"} {
		for _, timing := range []string{"stopped-before", "no-stop", "stale-stop-after"} {
			t.Run(transID+"/"+timing, func(t *testing.T) {
				f, m := issue94Fixture(t, true, "open")
				hdr := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "old-play", "Accept", "application/json"}
				original := url.Values{"path": {"/library/metadata/1000000000000550"}, "session": {"old-trans"}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}}
				if r := f.get(t, "/video/:/transcode/universal/decision?"+original.Encode(), hdr...); r.Code != 200 {
					t.Fatalf("original decision %d", r.Code)
				}
				if r := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", append(hdr, "Range", "bytes=0-9")...); r.Code != 206 {
					t.Fatalf("part %d", r.Code)
				}
				old, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
				if !ok {
					t.Fatal("missing initial session")
				}
				stop := func() {
					r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", hdr...)
					if r.Code != 200 {
						t.Fatalf("stop %d", r.Code)
					}
				}
				if timing == "stopped-before" {
					stop()
				}
				q := issue94Query(transID, 3000)
				q = strings.Replace(q, "X-Plex-Session-Identifier=play-id", "X-Plex-Session-Identifier=new-play", 1)
				newHdr := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "new-play", "Accept", "application/json"}
				if r := f.get(t, "/video/:/transcode/universal/decision?"+q, newHdr...); r.Code != 200 {
					t.Fatalf("decision %d", r.Code)
				}
				r := f.get(t, "/video/:/transcode/universal/start.m3u8?"+q, newHdr...)
				if r.Code != 200 {
					t.Fatalf("start %d %s", r.Code, r.Body.String())
				}
				current, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
				if !ok {
					t.Fatal("missing new session")
				}
				if timing == "stopped-before" && current.ID == old.ID {
					t.Fatal("stopped session reused")
				}
				if timing != "stopped-before" && current.ID != old.ID {
					t.Fatal("active source not reused")
				}
				if timing == "stale-stop-after" {
					stop()
					if _, ok = m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
						t.Fatal("delayed stop removed current session")
					}
					r = f.get(t, "/video/:/transcode/universal/session/"+transID+"/base/index.m3u8", newHdr...)
					if r.Code != 200 {
						t.Fatalf("stale stop killed HLS: %d", r.Code)
					}
					t.Logf("stale old-play preserved current playback; playlist HTTP=%d", r.Code)
				} else {
					t.Logf("start=%d reused=%t", r.Code, current.ID == old.ID)
				}
			})
		}
	}
}

func TestIssue94StopIdentityCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, sid, client, token string
		stopped                  bool
	}{
		{"superseded", "old", "phone", ownerToken, false},
		{"current", "new", "phone", ownerToken, true},
		{"omitted", "", "phone", ownerToken, true},
		{"unknown", "other", "phone", ownerToken, true},
		{"other client", "old", "other-phone", ownerToken, false},
		{"other viewer", "old", "phone", friendToken, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			request := func(sid string) {
				q := issue94Query("trans", 3000)
				hdr := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", sid, "Accept", "application/json"}
				r := f.get(t, "/video/:/transcode/universal/decision?"+q, hdr...)
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
				r = f.get(t, "/video/:/transcode/universal/start.m3u8?"+q, hdr...)
				if r.Code != 200 {
					t.Fatalf("start %d", r.Code)
				}
			}
			request("old")
			request("new")
			hdr := []string{"X-Plex-Client-Identifier", tc.client, "X-Plex-Token", tc.token}
			if tc.sid != "" {
				hdr = append(hdr, "X-Plex-Session-Identifier", tc.sid)
			}
			r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", hdr...)
			if r.Code != 200 {
				t.Fatalf("stop %d", r.Code)
			}
			_, live := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if live == tc.stopped {
				t.Fatalf("live=%t want stopped=%t", live, tc.stopped)
			}
		})
	}
}

func TestIssue94LatePartAndDownloadDoNotReplaceOwner(t *testing.T) {
	for _, tc := range []struct{ name, method, sid, query string }{
		{"old range", "GET", "old", ""},
		{"old head", "HEAD", "old", ""},
		{"download", "GET", "download", "?download=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			decision := func(id string) {
				hdr := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Accept", "application/json"}
				q := issue94Query("trans", 3000)
				r := f.get(t, "/video/:/transcode/universal/decision?"+q, hdr...)
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
				r = f.get(t, "/video/:/transcode/universal/start.m3u8?"+q, hdr...)
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
			}
			decision("old")
			decision("new")
			r := f.request(t, tc.method, "/library/parts/1000000000000550/1759276800/file.mkv"+tc.query, map[string]string{"X-Plex-Client-Identifier": "phone", "X-Plex-Session-Identifier": tc.sid, "Range": "bytes=0-9"})
			if r.Code != 206 {
				t.Fatalf("part=%d", r.Code)
			}
			r = f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "old")
			if r.Code != 200 {
				t.Fatalf("stop=%d", r.Code)
			}
			if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
				t.Fatal("late request made old stop remove replacement")
			}
			r = f.get(t, "/video/:/transcode/universal/session/trans/base/index.m3u8", "X-Plex-Client-Identifier", "phone")
			if r.Code != 200 {
				t.Fatalf("playlist=%d", r.Code)
			}
			_ = f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "new")
			if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); ok {
				t.Fatal("current stop did not stop playback")
			}
		})
	}
}

func TestIssue94DirectPartIdentifierReuse(t *testing.T) {
	f, m := issue94Fixture(t, true, "open")
	part := func(id string) {
		r := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Range", "bytes=0-9")
		if r.Code != 206 {
			t.Fatalf("part %s: %d", id, r.Code)
		}
	}
	stop := func(id string) {
		r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id)
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	live := func() bool { _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); return ok }
	part("old")
	part("new")
	part("old")
	stop("old")
	if !live() {
		t.Fatal("late old part stole direct-play ownership")
	}
	stop("new")
	if live() {
		t.Fatal("current direct-play stop failed")
	}
	part("old")
	if !live() {
		t.Fatal("old identifier could not start new source")
	}
	stop("old")
	if live() {
		t.Fatal("reused identifier could not stop new source")
	}
}

func TestIssue94DecisionCanReusePriorIdentifier(t *testing.T) {
	f, m := issue94Fixture(t, true, "open")
	for _, id := range []string{"old", "new", "old"} {
		r := f.get(t, "/video/:/transcode/universal/decision?"+issue94Query("trans", 3000), "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Accept", "application/json")
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=8340000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "old")
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); ok {
		t.Fatal("authoritatively reused identifier did not stop")
	}
}
