package plex

import (
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A resumed VOD's video timestamps remain on the title clock. Both sidecar
// routes and extracted embedded subtitles must use that clock even when a
// client supplies a different seek offset, including after track reselection.
func TestSubtitleTimelineAcrossResume(t *testing.T) {
	for _, container := range []string{"mpegts", "fmp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			subs := newFakeSubtitles()
			f := newFixture()
			f.srv.setSubtitles(subs)
			defer f.srv.Close()
			f.playback.info = probedInfo
			const srt = "1\n00:00:13,000 --> 00:00:14,000\nFirst\n\n2\n00:00:18,000 --> 00:00:19,000\nSecond\n\n"
			subs.files["ether-subtitle:release-en"] = srt
			id := externalID(fightClub.Key, "ether-subtitle:release-en")
			key := fightClub.Key.String()
			f.get(t, "/library/metadata/"+key)
			embedded := core.StreamInfo{Index: 3, Type: "subtitle", Codec: "subrip", Language: "fr"}
			embeddedID := slotHash(fightClub.Key, streamSlot(3, embedded))
			f.srv.d.Transcoder = &extractTranscoder{fakeTranscoder: f.trans, out: srtToVTT(srt)}
			f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: container}
			session := "timeline-" + container
			f.get(t, decisionPath(fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=/library/metadata/%s&session=%s&protocol=hls&offset=12&directPlay=0&directStream=1&subtitleStreamID=%d", key, session, id)))
			rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/start.m3u8?path=/library/metadata/%s&session=%s&protocol=hls&offset=12&directPlay=0&directStream=1&subtitleStreamID=%d", key, session, id))
			if rec.Code != 200 {
				t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
			}
			for _, target := range []string{fmt.Sprintf("/video/:/transcode/universal/subtitles?session=%s&format=vtt&offset=15", session), fmt.Sprintf("/library/streams/%d?format=vtt", id), fmt.Sprintf("/video/:/transcode/universal/subtitles?path=/library/metadata/%s&session=%s&format=vtt&offset=15&subtitleStreamID=%d", key, session, embeddedID), fmt.Sprintf("/library/streams/%d?format=vtt&offset=15", embeddedID)} {
				rec = f.get(t, target)
				body := rec.Body.String()
				if rec.Code != 200 || !strings.Contains(body, "00:00:13.000 --> 00:00:14.000") || !strings.Contains(body, "00:00:18.000 --> 00:00:19.000") {
					t.Errorf("%s: %d %s", target, rec.Code, body)
				}
				if container == "mkv" && !strings.Contains(target, fmt.Sprintf("/library/streams/%d?", id)) && !strings.Contains(body, "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:900000") {
					t.Errorf("%s: missing Matroska origin: %s", target, body)
				}
			}
		})
	}
}
