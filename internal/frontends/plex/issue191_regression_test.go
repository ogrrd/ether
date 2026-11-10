package plex

import (
	"context"
	"encoding/xml"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func mkDVHybridVariant(id, label string, auto bool, kbps int) core.ReleaseVariant {
	v := mkVariant(id, label, "", auto, 2160, "hevc", "eac3", true, kbps)
	v.Info.Streams[0].Profile = "main 10"
	v.Info.Streams[0].ColorPrimaries = "bt2020"
	v.Info.Streams[0].DolbyVisionProfile = 8
	v.Info.Streams[0].DolbyVisionCompatibilityID = 1
	return v
}

type wireXMLDoc struct {
	DirectPlayDecisionCode int `xml:"directPlayDecisionCode,attr"`
	Videos                 []struct {
		Media []struct {
			ID       string `xml:"id,attr"`
			Selected string `xml:"selected,attr"`
			Parts    []struct {
				ID       string `xml:"id,attr"`
				Decision string `xml:"decision,attr"`
				Selected string `xml:"selected,attr"`
				Streams  []struct {
					StreamType  int    `xml:"streamType,attr"`
					Codec       string `xml:"codec,attr"`
					ColorTrc    string `xml:"colorTrc,attr"`
					DOVIPresent string `xml:"DOVIPresent,attr"`
					DOVIProfile int    `xml:"DOVIProfile,attr"`
				} `xml:"Stream"`
			} `xml:"Part"`
		} `xml:"Media"`
		TranscodeSession *struct {
			Protocol         string `xml:"protocol,attr"`
			Container        string `xml:"container,attr"`
			VideoDecision    string `xml:"videoDecision,attr"`
			AudioDecision    string `xml:"audioDecision,attr"`
			SubtitleDecision string `xml:"subtitleDecision,attr"`
		} `xml:"TranscodeSession"`
	} `xml:"Video"`
}

// Regression for #191 Comment #27 (daimikegor support bundle):
//  1. Multi-version 4K DV.HDR + 1080p SDR item on 2017 NVIDIA Shield (darcy,
//     HDR10-capable, no Dolby Vision) must advertise the compatible 4K HDR10
//     base layer across Media[0..3] rather than exposing DOVIProfile="8" on
//     Media[0] and causing Bedrock MDE to hijack playback onto an unprobed
//     sibling or 1080p SDR bestCompat variant with 0 subtitle streams.
//  2. DV-capable clients (e.g. Apple TV, Profile 8 capable) keep DOVIProfile="8"
//     across all 4K DV variants so Media[0..3] stay consistent.
//  3. Both Play #1 and back-out + re-entry Play #2 preserve English (SRT)
//     selected="1" and mux subrip into HLS MKV with StripDolbyVision on darcy.
//  4. Active HLS MKV Direct Stream reports Part@decision="transcode",
//     Media@selected="1", TranscodeSession@container="mkv", videoDecision="copy",
//     and subtitleDecision="copy" on /status/sessions so Tautulli reports Direct
//     Stream instead of Direct Play.
func TestIssue191ShieldDVHybridMultiVersionSubtitlesAndTautulli(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Transcoder = shieldContractTranscoder{f.trans}

	prefs := trackprefs.New(store.OpenTest(t))
	f.srv.setTrackPreferences(prefs)
	rules := trackprefs.Default()
	rules.SubtitleLanguages = []string{"en"}
	rules.SubtitleMode = "always"
	if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}

	probed := core.MediaInfo{
		Container:   "mkv",
		Duration:    2799 * time.Second,
		BitrateKbps: 15800,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", BitDepth: 10, Width: 3840, Height: 2160, FrameRate: 23.976, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", DolbyVision: true, DolbyVisionProfile: 8, DolbyVisionCompatibilityID: 1, Default: true},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, SampleRate: 48000, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Title: "English", Default: true},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "es", Title: "Spanish"},
		},
	}
	f.playback.info = probed
	f.srv.setVariants(&fakeVariantResolver{byKey: map[core.RatingKey][]core.ReleaseVariant{
		fightClub.Key: {
			mkDVHybridVariant("rel-auto", "Auto · 2160p WEB-DL · DD+ 5.1 · GRP", true, 15800),
			mkDVHybridVariant("rel-alt1", "2160p WEB-DL · DD+ 5.1 · 5.2 GB · ALT1", false, 15200),
			mkDVHybridVariant("rel-alt2", "2160p WEB-DL · DD+ 5.1 · 4.9 GB · ALT2", false, 14500),
			mkDVHybridVariant("rel-4k", "2160p WEB-DL · GRP", false, 15800),
			mkVariant("rel-1080", "1080p WEB-DL · GRP (High Compatibility)", "", false, 1080, "h264", "eac3", false, 6200),
		},
	}})
	pb := &shieldMetadataPlayback{Playback: f.playback, facts: probed, ready: false}
	f.srv.d.Playback = pb

	path := "/library/metadata/" + fightClub.Key.String()
	darcyHeaders := map[string]string{
		"X-Plex-Product":           "Plex for Android (TV)",
		"X-Plex-Platform":          "Android",
		"X-Plex-Device":            "SHIELD Android TV",
		"X-Plex-Model":             "darcy",
		"X-Plex-Version":           "2026.20.0",
		"X-Plex-Client-Identifier": "shield-darcy-191",
	}

	parseWire := func(body string) wireXMLDoc {
		t.Helper()
		var doc wireXMLDoc
		if err := xml.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("xml unmarshal: %v\n%s", err, body)
		}
		return doc
	}

	// Verify DV-capable Apple TV keeps DOVIProfile="8" across all 4K DV variants.
	appleTVHeaders := map[string]string{
		"X-Plex-Product":           "Plex for Apple TV",
		"X-Plex-Platform":          "tvOS",
		"X-Plex-Device":            "Apple TV",
		"X-Plex-Client-Identifier": "appletv-191",
	}
	atvDetail := f.request(t, "GET", path, appleTVHeaders)
	if atvDetail.Code != 200 {
		t.Fatalf("appleTV detail: %d %s", atvDetail.Code, atvDetail.Body.String())
	}
	atvDoc := parseWire(atvDetail.Body.String())
	if len(atvDoc.Videos) != 1 || len(atvDoc.Videos[0].Media) != 5 {
		t.Fatalf("unexpected appleTV media count: %+v", atvDoc.Videos)
	}
	for idx := 0; idx < 4; idx++ {
		vs := atvDoc.Videos[0].Media[idx].Parts[0].Streams[0]
		if vs.DOVIProfile != 8 || vs.DOVIPresent != "1" {
			t.Fatalf("appleTV Media[%d] lost Dolby Vision Profile 8: %+v", idx, vs)
		}
	}

	// Verify 2017 Shield (darcy) across Play #1 and back-out + Play #2.
	profile := "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3&subtitleCodec=ass,ssa,pgs,srt)"
	for attempt := 1; attempt <= 2; attempt++ {
		detailResp := f.request(t, "GET", path, darcyHeaders)
		if detailResp.Code != 200 {
			t.Fatalf("attempt %d detail: %d", attempt, detailResp.Code)
		}
		doc := parseWire(detailResp.Body.String())
		if len(doc.Videos) != 1 || len(doc.Videos[0].Media) != 5 {
			t.Fatalf("attempt %d media len=%d", attempt, len(doc.Videos[0].Media))
		}
		for idx := 0; idx < 4; idx++ {
			vs := doc.Videos[0].Media[idx].Parts[0].Streams[0]
			if vs.DOVIPresent != "" || vs.DOVIProfile != 0 || vs.ColorTrc != "smpte2084" {
				t.Fatalf("attempt %d darcy Media[%d] should advertise HDR10 base without DOVI: %+v", attempt, idx, vs)
			}
		}
		subs := subtitleTags(detailResp.Body.String())
		if len(subs) != 2 || streamAttr(selectedTag(subs), "codec") != "srt" || streamAttr(selectedTag(subs), "languageTag") != "en" {
			t.Fatalf("attempt %d detail subtitles=%v", attempt, subs)
		}

		pqResp := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), darcyHeaders)
		if pqResp.Code != 200 {
			t.Fatalf("attempt %d playQueue: %d", attempt, pqResp.Code)
		}
		pqSubs := subtitleTags(pqResp.Body.String())
		if len(pqSubs) != 2 || streamAttr(selectedTag(pqSubs), "codec") != "srt" {
			t.Fatalf("attempt %d playQueue subtitles=%v", attempt, pqSubs)
		}

		sessID := "darcy-191-play-" + string(rune('0'+attempt))
		q := url.Values{
			"mediaIndex":                  {"0"},
			"path":                        {path},
			"protocol":                    {"hls"},
			"directPlay":                  {"1"},
			"directStream":                {"1"},
			"directStreamAudio":           {"1"},
			"hasMDE":                      {"1"},
			"session":                     {sessID},
			"X-Plex-Session-Identifier":   {sessID},
			"X-Plex-Client-Profile-Name":  {"Generic"},
			"X-Plex-Client-Profile-Extra": {profile},
		}
		decResp := f.request(t, "GET", "/video/:/transcode/universal/decision?"+q.Encode(), darcyHeaders)
		if decResp.Code != 200 {
			t.Fatalf("attempt %d decision: %d %s", attempt, decResp.Code, decResp.Body.String())
		}
		decDoc := parseWire(decResp.Body.String())
		if decDoc.DirectPlayDecisionCode == 1000 {
			t.Fatalf("attempt %d darcy DV-strip session must not report MDE DirectPlay 1000", attempt)
		}
		decSubs := subtitleTags(decResp.Body.String())
		selSub := selectedTag(decSubs)
		if streamAttr(selSub, "codec") != "srt" || streamAttr(selSub, "decision") != "copy" || streamAttr(selSub, "location") != "segments-av" {
			t.Fatalf("attempt %d decision selected subtitle=%q (all=%v)", attempt, selSub, decSubs)
		}

		startResp := f.request(t, "GET", "/video/:/transcode/universal/start.m3u8?"+q.Encode(), darcyHeaders)
		if startResp.Code != 200 {
			t.Fatalf("attempt %d start.m3u8: %d %s", attempt, startResp.Code, startResp.Body.String())
		}
		d := f.trans.sessions[sessID].req.Decision
		if d.Container != transcode.ContainerMatroska || !d.VideoCopy || !d.StripDolbyVision || d.SubtitleStream != 2 || d.BurnSubtitles {
			t.Fatalf("attempt %d unexpected transcode decision: %+v", attempt, d)
		}

		// Report playing timeline and inspect /status/sessions (Tautulli contract).
		tl := url.Values{
			"ratingKey":                 {fightClub.Key.String()},
			"key":                       {path},
			"state":                     {"playing"},
			"time":                      {"15000"},
			"duration":                  {"2799000"},
			"X-Plex-Session-Identifier": {sessID},
		}
		tlResp := f.request(t, "GET", "/:/timeline?"+tl.Encode(), darcyHeaders)
		if tlResp.Code != 200 {
			t.Fatalf("attempt %d timeline playing: %d %s", attempt, tlResp.Code, tlResp.Body.String())
		}

		sessResp := f.get(t, "/status/sessions")
		if sessResp.Code != 200 {
			t.Fatalf("attempt %d /status/sessions: %d %s", attempt, sessResp.Code, sessResp.Body.String())
		}
		sessDoc := parseWire(sessResp.Body.String())
		if len(sessDoc.Videos) != 1 {
			t.Fatalf("attempt %d expected 1 active session, got %d: %s", attempt, len(sessDoc.Videos), sessResp.Body.String())
		}
		item := sessDoc.Videos[0]
		if len(item.Media) != 1 || item.Media[0].Selected != "1" || len(item.Media[0].Parts) != 1 {
			t.Fatalf("attempt %d session media malformed: %+v", attempt, item.Media)
		}
		part := item.Media[0].Parts[0]
		if part.Decision != "transcode" || part.Selected != "1" {
			t.Fatalf("attempt %d Tautulli requires Part@decision=transcode and selected=1, got decision=%q selected=%q", attempt, part.Decision, part.Selected)
		}
		if item.TranscodeSession == nil {
			t.Fatalf("attempt %d missing TranscodeSession on /status/sessions", attempt)
		}
		ts := item.TranscodeSession
		if ts.VideoDecision != "copy" || ts.AudioDecision != "copy" || ts.SubtitleDecision != "copy" || ts.Container != "mkv" || ts.Protocol != "hls" {
			t.Fatalf("attempt %d unexpected TranscodeSession: %+v", attempt, ts)
		}

		// Stop playback so Play #2 tests back-out + re-entry with persisted resumed facts.
		tl.Set("state", "stopped")
		stopResp := f.request(t, "GET", "/:/timeline?"+tl.Encode(), darcyHeaders)
		if stopResp.Code != 200 {
			t.Fatalf("attempt %d timeline stopped: %d", attempt, stopResp.Code)
		}
	}
}

// Regression for #191 Comment #28 (tjukkie, iPhone / Plex for iOS):
// First detail open and direct PlayQueue creation on Plex for iOS must run
// bounded synchronous MetadataProber.ProbeMetadata so embedded subtitles are
// present and selected on first play without backing out and re-entering.
func TestIssue191PlexIOSFirstOpenProbesEmbeddedSubtitles(t *testing.T) {
	newIOSFixture := func(t *testing.T) (*fixture, *shieldMetadataPlayback, map[string]string) {
		t.Helper()
		f := newFixture()
		t.Cleanup(func() { _ = f.srv.Close() })
		prefs := trackprefs.New(store.OpenTest(t))
		f.srv.setTrackPreferences(prefs)
		rules := trackprefs.Default()
		rules.SubtitleLanguages = []string{"en"}
		rules.SubtitleMode = "always"
		if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
			t.Fatal(err)
		}
		facts := core.MediaInfo{
			Container: "mkv",
			Duration:  1200 * time.Second,
			Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080, BitDepth: 10, Default: true},
				{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, Default: true},
				{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Default: true},
			},
		}
		f.playback.info = facts
		pb := &shieldMetadataPlayback{Playback: f.playback, facts: facts, ready: false}
		f.srv.d.Playback = pb
		headers := map[string]string{
			"X-Plex-Product":           "Plex for iOS",
			"X-Plex-Platform":          "iOS",
			"X-Plex-Device":            "iPhone",
			"X-Plex-Client-Identifier": "iphone-191",
		}
		return f, pb, headers
	}

	t.Run("detail-open", func(t *testing.T) {
		f, pb, h := newIOSFixture(t)
		path := "/library/metadata/" + fightClub.Key.String()
		r := f.request(t, "GET", path, h)
		if r.Code != 200 {
			t.Fatalf("detail: %d %s", r.Code, r.Body.String())
		}
		subs := subtitleTags(r.Body.String())
		if pb.probes != 1 || len(subs) != 1 || streamAttr(selectedTag(subs), "codec") != "srt" {
			t.Fatalf("iOS first detail open missed embedded subtitle: probes=%d subs=%v", pb.probes, subs)
		}
	})

	t.Run("direct-playqueue", func(t *testing.T) {
		f, pb, h := newIOSFixture(t)
		path := "/library/metadata/" + fightClub.Key.String()
		r := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), h)
		if r.Code != 200 {
			t.Fatalf("playQueue: %d %s", r.Code, r.Body.String())
		}
		subs := subtitleTags(r.Body.String())
		if pb.probes != 1 || len(subs) != 1 || streamAttr(selectedTag(subs), "codec") != "srt" {
			t.Fatalf("iOS direct playQueue missed embedded subtitle: probes=%d subs=%v", pb.probes, subs)
		}
	})

	t.Run("disable-speculation-respected", func(t *testing.T) {
		x := newEtheLab(t, true, "offline-0", 3)
		x.cfg.Cache.DisableSpeculation = true
		x.mgr.SetConfig(x.cfg)
		r := x.f.get(t, "/library/metadata/"+x.title.Key.String(), "X-Plex-Product", "Plex for iOS", "X-Plex-Platform", "iOS")
		if r.Code != 200 {
			t.Fatalf("detail: %d %s", r.Code, r.Body.String())
		}
		if x.idx.searches.Load() != 0 || x.idx.fetches.Load() != 0 {
			t.Fatal("DisableSpeculation iOS detail fetched fresh source")
		}
	})
}
