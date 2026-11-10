package plex

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestStartHLSLadderMasterAndRungs(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	f.trans.rungs = []core.HLSRung{
		{ID: "base", Bandwidth: 9120000, AverageBandwidth: 7600000, Width: 1920, Height: 1080, Codecs: "avc1.640028,mp4a.40.2", FrameRate: 23.976},
		{ID: "720p4m", Bandwidth: 4800000, AverageBandwidth: 4000000, Width: 1280, Height: 720, Codecs: "avc1.64001f,mp4a.40.2", FrameRate: 23.976},
		{ID: "480p1500k", Bandwidth: 1800000, AverageBandwidth: 1500000, Width: 854, Height: 480, Codecs: "avc1.64001e,mp4a.40.2"},
	}
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=lad-1&protocol=hls&directStream=1"))
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=lad-1&protocol=hls&directStream=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=9120000,AVERAGE-BANDWIDTH=7600000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\",FRAME-RATE=23.976\nsession/lad-1/base/index.m3u8",
		"BANDWIDTH=4800000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1280x720",
		"session/lad-1/720p4m/index.m3u8",
		"session/lad-1/480p1500k/index.m3u8",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("master missing %q\n%s", want, body)
		}
	}
	if n := strings.Count(body, "#EXT-X-STREAM-INF"); n != 3 {
		t.Errorf("variants = %d", n)
	}
	// Rung playlist and segment routes reach the rung.
	rec = f.get(t, "/video/:/transcode/universal/session/lad-1/720p4m/index.m3u8")
	if !strings.Contains(rec.Body.String(), "# rung 720p4m") {
		t.Errorf("rung playlist: %s", rec.Body.String())
	}
	rec = f.get(t, "/video/:/transcode/universal/session/lad-1/720p4m/00042.ts")
	if rec.Body.String() != "segment-720p4m-42" {
		t.Errorf("rung segment: %s", rec.Body.String())
	}
	// The base URI still works through the ladder.
	rec = f.get(t, "/video/:/transcode/universal/session/lad-1/base/00007.ts")
	if rec.Body.String() != "segment-base-7" {
		t.Errorf("base segment: %s", rec.Body.String())
	}
	// Unknown rungs 404 via the legacy path not finding a segment.
	if rec := f.get(t, "/video/:/transcode/universal/session/lad-1/999p/index.m3u8"); rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "# rung") {
		t.Errorf("unknown rung served: %s", rec.Body.String())
	}
}

// Native Android/Apple media loaders authenticate start.m3u8, then follow
// playlist URLs without retaining Plex headers. Exercise that exact traversal,
// including the independent stream grant on every rung, init and segment URL.
func TestLadderPlaylistTraversalWithoutPlexHeaders(t *testing.T) {
	for _, container := range []string{"fmp4", "mkv"} {
		t.Run(container, func(t *testing.T) { testLadderPlaylistTraversalWithoutPlexHeaders(t, container) })
	}
}

func testLadderPlaylistTraversalWithoutPlexHeaders(t *testing.T, container string) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: container}
	f.trans.rungs = []core.HLSRung{
		{ID: "base", Bandwidth: 8_000_000},
		{ID: "720p4m", Bandwidth: 4_000_000},
	}
	startPath := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=ladder-private"
	f.get(t, decisionPath(startPath))
	start := f.get(t, startPath)
	if start.Code != http.StatusOK {
		t.Fatalf("start: %d", start.Code)
	}
	f.trans.sessions["ladder-private"].rungPlaylist = "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.0,\n00000.m4s\n"
	if container == "mkv" {
		f.trans.sessions["ladder-private"].rungPlaylist = "#EXTM3U\n#EXT-X-MAP:URI=\"header\"\n#EXTINF:4.0,\n00000.mkv\n"
	}
	resolve := func(base, relative string) string {
		t.Helper()
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := url.Parse(relative)
		if err != nil {
			t.Fatal(err)
		}
		return u.ResolveReference(ref).String()
	}
	var manifests []string
	for _, line := range strings.Split(start.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			manifests = append(manifests, resolve(startPath, line))
		}
	}
	if len(manifests) != 2 {
		t.Fatalf("rung count=%d", len(manifests))
	}
	var mediaPaths []string
	for _, manifest := range manifests {
		rec := f.request(t, http.MethodGet, manifest, map[string]string{"X-Plex-Token": ""})
		if rec.Code != http.StatusOK {
			t.Fatalf("headerless rung playlist: %d", rec.Code)
		}
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "#EXT-X-MAP:URI=\"") {
				mediaPaths = append(mediaPaths, resolve(manifest, strings.SplitN(line, "\"", 3)[1]))
			} else if line != "" && !strings.HasPrefix(line, "#") {
				mediaPaths = append(mediaPaths, resolve(manifest, line))
			}
		}
	}
	if len(mediaPaths) != 4 {
		t.Fatalf("init + segment URLs=%d", len(mediaPaths))
	}
	for _, path := range append(manifests, mediaPaths...) {
		rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": ""})
		if rec.Code != http.StatusOK {
			t.Fatalf("headerless granted media: %d", rec.Code)
		}
		// The grant is still scoped to the owner and stream: do not make the
		// ladder public or let another authenticated viewer use its URLs.
		rec = f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": friendToken})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign viewer: %d", rec.Code)
		}
		u, _ := url.Parse(path)
		q := u.Query()
		q.Del(streamGrantParam)
		q.Del(readCapParam)
		u.RawQuery = q.Encode()
		rec = f.request(t, http.MethodGet, u.String(), map[string]string{"X-Plex-Token": ""})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unguarded media: %d", rec.Code)
		}
	}
	// Removing the viewer revokes every rung and segment capability.
	a := f.srv.testAuth()
	delete(a.users, ownerToken)
	for _, path := range append(manifests, mediaPaths...) {
		rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": ""})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked ladder grant: %d", rec.Code)
		}
	}
}

func TestStartHLSSingleRungUsesSessionBandwidth(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	f.trans.rungs = []core.HLSRung{{ID: "base", Bandwidth: 421000}}
	path := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=single-peak&protocol=hls&directStream=1"
	f.get(t, decisionPath(path))
	rec := f.get(t, path)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PROGRAM-ID=1,BANDWIDTH=421000,") {
		t.Fatalf("single-rung master: %d %s", rec.Code, rec.Body.String())
	}
}

// When a player steps down to an encoded rung of a Direct Stream session,
// the Plex dashboard's TranscodeSession reports the video transcode Ether's
// Activity shows, and Direct Stream again once it is back on the original.
func TestLadderServedRungReporting(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCodec: "aac", Container: "mpegts"}
	f.trans.rungs = []core.HLSRung{{ID: "base", Bandwidth: 9_000_000}, {ID: "720p4m", Bandwidth: 4_800_000}}
	start := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=lad-served&protocol=hls&directStream=1"
	f.get(t, decisionPath(start))
	if rec := f.get(t, start); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	hs := f.trans.sessions["lad-served"]
	if hs == nil {
		t.Fatal("no session")
	}
	te, ok := f.srv.transcodes.get("lad-served")
	if !ok {
		t.Fatal("no transcode entry")
	}
	if ts := f.srv.servedTranscodeSessionModel(te); ts.VideoDecision != "copy" {
		t.Fatalf("base: videoDecision %q", ts.VideoDecision)
	}
	hs.encoder = &core.EncoderStatus{Rung: "720p4m", Decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264"}}
	if ts := f.srv.servedTranscodeSessionModel(te); ts.VideoDecision != "transcode" || ts.VideoCodec != "h264" {
		t.Fatalf("rung: %+v", ts)
	}
	hs.encoder = &core.EncoderStatus{Rung: "base", Decision: f.trans.decision}
	if ts := f.srv.servedTranscodeSessionModel(te); ts.VideoDecision != "copy" {
		t.Fatalf("back on base: videoDecision %q", ts.VideoDecision)
	}
}
