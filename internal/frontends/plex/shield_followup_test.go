package plex

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The support capture preserves methods/routes and client identity, but
// redacts queries and carries no raw profile headers. Query/profile fields
// below are reconstructed from the existing Android TV contract fixture.
type shieldMetadataPlayback struct {
	core.Playback
	facts  core.MediaInfo
	ready  bool
	err    error
	probes int
}

func (p *shieldMetadataPlayback) ProbeMetadata(context.Context, *core.User, core.RatingKey, core.ClientCaps) error {
	p.probes++
	p.ready = p.err == nil
	return p.err
}
func (p *shieldMetadataPlayback) Prepare(ctx context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	ps, err := p.Playback.Prepare(ctx, u, k, caps)
	if ps != nil {
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "rel-auto"}}
	}
	return ps, err
}
func (p *shieldMetadataPlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return p.facts, p.ready
}
func (p *shieldMetadataPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return core.MediaInfo{Estimated: true, Container: "mkv", Streams: p.facts.Streams[:2]}
}

func TestShieldTroyCaptureFollowup(t *testing.T) {
	for _, product := range []string{"Plex for Android (TV)", "Plex for Android (Mobile)"} {
		t.Run(product, func(t *testing.T) {
			for _, warm := range []bool{false, true} {
				for _, mode := range []string{"hevc-copy-audio-transcode", "tonemap"} {
					t.Run(fmt.Sprintf("warm=%v/%s", warm, mode), func(t *testing.T) {
						f := newFixture()
						defer f.srv.Close()
						troy := fightClub
						troy.Title = "Troy"
						troy.Year = 2004
						f.catalog.titles[troy.Key] = troy
						f.srv.d.Transcoder = shieldContractTranscoder{f.trans}
						prefs := trackprefs.New(store.OpenTest(t))
						f.srv.setTrackPreferences(prefs)
						rules := trackprefs.Default()
						rules.SubtitleLanguages = []string{"en"}
						rules.SubtitleMode = "always"
						if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
							t.Fatal(err)
						}
						facts := core.MediaInfo{Container: "mkv", Duration: 30 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", BitDepth: 10, Width: 3840, Height: 2160, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", DolbyVision: true, DolbyVisionProfile: 7, DolbyVisionCompatibilityID: 6},
							{Index: 1, Type: "audio", Codec: "dca", Profile: "DTS-HD MA", Channels: 6, Default: true},
							{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Default: true},
							{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "en"},
						}}
						f.playback.info = facts
						f.srv.setVariants(&fakeVariantResolver{byKey: testVariantLists()})
						pb := &shieldMetadataPlayback{Playback: f.playback, facts: facts, ready: warm}
						f.srv.d.Playback = pb
						device := "SHIELD Android TV"
						if product == "Plex for Android (Mobile)" {
							device = "Pixel 8"
						}
						h := map[string]string{"X-Plex-Product": product, "X-Plex-Platform": "Android", "X-Plex-Device": device, "X-Plex-Model": "darcy", "X-Plex-Version": "2026.20.0"}
						path := "/library/metadata/" + fightClub.Key.String()
						read := func(target string) string {
							t.Helper()
							r := f.request(t, "GET", target, h)
							if r.Code != 200 {
								t.Fatalf("%s: %d %s", target, r.Code, r.Body.String())
							}
							return r.Body.String()
						}
						// FIRST detail GET, before queue/decision/Prepare: exact tracks, selected SRT.
						first := subtitleTags(read(path))
						if len(first) != 2 || streamAttr(selectedTag(first), "codec") != "srt" || pb.probes != map[bool]int{false: 1, true: 0}[warm] || f.playback.prepared != 0 {
							t.Fatalf("first metadata: tracks=%v probes=%d prepares=%d", first, pb.probes, f.playback.prepared)
						}
						ids := []string{streamAttr(first[0], "id"), streamAttr(first[1], "id")}
						check := func(body, selected string) {
							t.Helper()
							tags := subtitleTags(body)
							if len(tags) != 2 || streamAttr(tags[0], "id") != ids[0] || streamAttr(tags[1], "id") != ids[1] || streamAttr(selectedTag(tags), "id") != selected {
								t.Fatalf("track list changed: %v (want %v selected %s)", tags, ids, selected)
							}
						}
						r := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), h)
						if r.Code != 200 {
							t.Fatalf("queue: %d %s", r.Code, r.Body.String())
						}
						check(r.Body.String(), ids[0])
						check(read(path), ids[0])
						profile := "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac&subtitleCodec=ass,ssa,pgs,srt)"
						if mode == "tonemap" {
							profile += "+add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)"
						}
						// The fixture advertises multiple versions; select its Auto entry.
						q := url.Values{"mediaIndex": {"0"}, "path": {path}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}, "hasMDE": {"1"}, "subtitles": {"none"}, "advancedSubtitles": {"text"}, "session": {"troy-" + mode}, "X-Plex-Client-Profile-Name": {"Generic"}, "X-Plex-Client-Profile-Extra": {profile}}
						for i, id := range ids {
							if i > 0 {
								r = f.request(t, "PUT", "/library/parts/"+fightClub.Key.String()+"?subtitleStreamID="+id, h)
								if r.Code != 200 {
									t.Fatalf("PUT: %d %s", r.Code, r.Body.String())
								}
								check(read(path), id)
							}
							decision := read("/video/:/transcode/universal/decision?" + q.Encode())
							check(decision, id)
							read("/video/:/transcode/universal/start.m3u8?" + q.Encode())
							d := f.trans.sessions[q.Get("session")].req.Decision
							if d.Container != transcode.ContainerMatroska || d.SubtitleStream != 2+i {
								t.Fatalf("encoder lost subtitle: %+v", d)
							}
							if i == 0 {
								row := selectedTag(subtitleTags(decision))
								if d.AudioCopy || d.BurnSubtitles || streamAttr(row, "decision") != "copy" || streamAttr(row, "location") != "segments-av" {
									t.Fatalf("SRT conversion: %+v %s", d, row)
								}
								if mode == "tonemap" {
									if d.VideoCopy || !d.ToneMap {
										t.Fatalf("missing tone map: %+v", d)
									}
								} else if product != "Plex for Android (Mobile)" && !d.VideoCopy {
									t.Fatalf("HEVC not copied: %+v", d)
								}
							} else if !d.BurnSubtitles || d.VideoCopy {
								t.Fatalf("PGS not burned: %+v", d)
							}
							check(read(path), id)
						}
					})
				}
			}
		})
	}
}

func TestShieldMetadataProbeFailureKeepsNormalResponse(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Playback = &shieldMetadataPlayback{Playback: f.playback, facts: sampleInfo, err: core.ErrPrepareSlow}
	r := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android")
	if r.Code != 200 || r.Header().Get("Retry-After") != "" {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
}

func TestAndroidMetadataProbeLeavesOtherClientsUnchanged(t *testing.T) {
	for _, product := range []string{"Plex Web", "Plex for Apple TV", "Plezy"} {
		t.Run(product, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			p := &shieldMetadataPlayback{Playback: f.playback, facts: sampleInfo, err: core.ErrPrepareSlow}
			f.srv.d.Playback = p
			r := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", product)
			if r.Code != 200 || p.probes != 0 {
				t.Fatalf("other client changed: status=%d metadata probes=%d", r.Code, p.probes)
			}
		})
	}
}

func TestShieldDisabledDetailUsesPersistedProbe(t *testing.T) {
	x := newEtheLab(t, true, "offline-0", 3)
	x.cfg.Cache.DisableSpeculation = true
	x.mgr.SetConfig(x.cfg)
	r := x.f.get(t, "/library/metadata/"+x.title.Key.String(), "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android")
	if r.Code != 200 || len(subtitleTags(r.Body.String())) < 1 {
		t.Fatalf("persisted tracks missing: %d %s", r.Code, r.Body.String())
	}
	if x.idx.searches.Load() != 0 || x.idx.fetches.Load() != 0 {
		t.Fatal("disabled detail fetched fresh source")
	}
}
