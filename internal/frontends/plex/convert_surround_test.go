package plex

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestConvertSurroundAudioPlexAndroidMobile(t *testing.T) {
	f := newFixture(func(d *Deps) {
		baseConfig := depsConfig(d)
		setDepsConfig(d, func() config.Config {
			c := baseConfig()
			c.Quality = config.DefaultQuality()
			c.Quality.Rules = []config.QualityRule{{
				Protocol:             "plex",
				Product:              "Plex for Android",
				Profile:              "4K",
				ConvertSurroundAudio: true,
			}}
			return c
		})
	})
	f.srv.d.Transcoder = &realDecider{f.trans}
	defer f.srv.Close()

	f.playback.info = core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6, Default: true},
		},
	}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&protocol=hls&session=convert-surround",
		append(jsonAccept,
			"X-Plex-Product", "Plex for Android (Mobile)",
			"X-Plex-Platform", "Android",
			"X-Plex-Client-Identifier", "android-pixel9",
			"X-Plex-Client-Profile-Extra", extra,
		)...,
	)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mc := decodeJSON(t, rec)
	if mc["transcodeDecisionCode"] != float64(1001) {
		t.Fatalf("expected transcodeDecisionCode 1001, got %v (%s)", mc["transcodeDecisionCode"], rec.Body.String())
	}
	if mc["mdeDecisionCode"] == float64(1000) {
		t.Fatalf("expected mdeDecisionCode not to be 1000 (direct play), got %v", mc["mdeDecisionCode"])
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"codec":"aac"`) && !strings.Contains(body, `codec="aac"`) {
		t.Fatalf("expected audio converted to aac in decision body:\n%s", body)
	}
}

func TestConvertSurroundAudioDisabledPreservesSurround(t *testing.T) {
	f := newFixture(func(d *Deps) {
		baseConfig := depsConfig(d)
		setDepsConfig(d, func() config.Config {
			c := baseConfig()
			c.Quality.Rules = []config.QualityRule{{
				Protocol:             "plex",
				Product:              "Plex for Android",
				Profile:              "4K",
				ConvertSurroundAudio: false,
			}}
			return c
		})
	})
	f.srv.d.Transcoder = &realDecider{f.trans}
	defer f.srv.Close()

	f.playback.info = core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6, Default: true},
		},
	}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&protocol=hls&session=preserve-surround",
		append(jsonAccept,
			"X-Plex-Product", "Plex for Android (Mobile)",
			"X-Plex-Platform", "Android",
			"X-Plex-Client-Identifier", "android-pixel9",
			"X-Plex-Client-Profile-Extra", extra,
		)...,
	)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mc := decodeJSON(t, rec)
	if mc["mdeDecisionCode"] != float64(1000) {
		t.Fatalf("expected mdeDecisionCode 1000 (direct play), got %v (%s)", mc["mdeDecisionCode"], rec.Body.String())
	}
}

// The same hasMDE=1 request without an owner rule, on the path Kakasanti's
// freeze took: an on-demand source that Ether remuxes into segmented
// Matroska. Plex for Android phones never get Dolby/DTS copied into that
// stream, Shield keeps its passthrough copy, Google TV sticks lose only DTS,
// and a client whose stream stalled on copied surround converts next time.
func TestAndroidMDEDecisionSurroundAudio(t *testing.T) {
	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"
	phone := []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android", "X-Plex-Device", "Pixel 9 Pro", "X-Plex-Model", "komodo"}
	shield := []string{"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "SHIELD Android TV", "X-Plex-Model", "mdarcy"}
	chromecast := []string{"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "Chromecast", "X-Plex-Model", "sabrina"}
	usenet := &core.Resolution{Release: core.Release{ID: "nzb-123", Protocol: "usenet"}}
	for _, tc := range []struct {
		name       string
		client     []string
		audio      string
		source     *core.Resolution
		convert    bool // owner rule "Convert Dolby and DTS to AAC"
		stalled    bool // this client's last stream stalled on copied surround
		wantDirect bool
		wantAudio  string // output codec; "copy" keeps the source track
	}{
		{name: "phone stream converts E-AC-3", client: phone, audio: "eac3", source: usenet, wantAudio: "aac"},
		{name: "phone file direct play untouched", client: phone, audio: "eac3", wantDirect: true},
		{name: "Shield stream keeps E-AC-3 for passthrough", client: shield, audio: "eac3", source: usenet, wantAudio: "copy"},
		{name: "Shield with owner rule converts", client: shield, audio: "eac3", source: usenet, convert: true, wantAudio: "aac"},
		{name: "Shield after a surround stall converts", client: shield, audio: "eac3", source: usenet, stalled: true, wantAudio: "aac"},
		{name: "Chromecast stream keeps E-AC-3", client: chromecast, audio: "eac3", source: usenet, wantAudio: "copy"},
		{name: "Chromecast stream converts DTS to E-AC-3", client: chromecast, audio: "dts", source: usenet, wantAudio: "eac3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := config.DefaultQuality()
			if tc.convert {
				q.Rules = []config.QualityRule{{Protocol: "plex", Product: "Plex for Android", Profile: "4K", ConvertSurroundAudio: true}}
			}
			f := newFixture(func(d *Deps) {
				base := depsConfig(d)
				setDepsConfig(d, func() config.Config {
					c := base()
					c.Quality = q
					return c
				})
			})
			defer f.srv.Close()
			f.srv.d.Transcoder = &realDecider{f.trans}
			f.playback.resolution = tc.source
			if tc.stalled {
				f.srv.starts.Remember(surroundHintKey(&Request{User: &ownerUser, ClientID: "android-client"}), f.srv.now())
			}
			f.playback.info = core.MediaInfo{Container: "mkv", BitrateKbps: 5000, Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Index: 1, Type: "audio", Codec: tc.audio, Channels: 6, Default: true},
			}}
			hdr := append(append([]string{}, jsonAccept...), tc.client...)
			hdr = append(hdr, "X-Plex-Client-Identifier", "android-client", "X-Plex-Client-Profile-Name", "Generic", "X-Plex-Client-Profile-Extra", extra)
			rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&directStream=1&directStreamAudio=1&protocol=hls&mediaIndex=0&partIndex=0", hdr...)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			mc := decodeJSON(t, rec)
			part := mc["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)
			if tc.wantDirect {
				if mc["mdeDecisionCode"] != float64(1000) || part["decision"] != nil && part["decision"] != "directplay" {
					t.Fatalf("want MDE direct play, got %v / part %v", mc, part)
				}
				return
			}
			var video, audio map[string]any
			for _, raw := range part["Stream"].([]any) {
				st := raw.(map[string]any)
				switch st["streamType"] {
				case float64(1):
					video = st
				case float64(2):
					audio = st
				}
			}
			if video == nil || audio == nil || video["decision"] != "copy" {
				t.Fatalf("want copied video: %v", part)
			}
			if tc.wantAudio == "copy" {
				if audio["decision"] != "copy" || audio["codec"] != tc.audio {
					t.Fatalf("want %s copied, got %v", tc.audio, audio)
				}
				return
			}
			if audio["decision"] != "transcode" || audio["codec"] != tc.wantAudio {
				t.Fatalf("want %s -> %s, got %v", tc.audio, tc.wantAudio, audio)
			}
		})
	}
}
