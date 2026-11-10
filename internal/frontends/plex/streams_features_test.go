package plex

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// Hide the optional scoped interface to exercise the original selection path.
type unscopedTrackPreferences struct{ TrackPreferences }

func TestNoScopedOverridesPreserveSelection(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	rules := trackprefs.Default()
	rules.AudioLanguages = []string{"en"}
	if err := prefs.PutRules(context.Background(), 7, rules); err != nil {
		t.Fatal(err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "fr"}, {Type: "audio", Language: "en"}}}
	viewer := &core.User{ID: 7}
	f.srv.UseTrackPreferences(unscopedTrackPreferences{prefs})
	old := f.srv.preferredSelection(context.Background(), viewer, fightClub.Key, info, "", selection{})
	f.srv.UseTrackPreferences(prefs)
	got := f.srv.preferredSelection(context.Background(), viewer, fightClub.Key, info, "", selection{})
	if got != old {
		t.Fatalf("no-override selection changed: old=%+v new=%+v", old, got)
	}
}

func TestScopedQualityOnlyTightensPlexRequest(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	f.srv.UseTrackPreferences(prefs)
	viewer := &core.User{ID: 7}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "en"}}}
	for _, tc := range []struct{ requested, preferred, want int }{{2000, 5000, 2000}, {5000, 2000, 2000}} {
		if err := prefs.PutScoped(context.Background(), viewer.ID, "device", "tv", trackprefs.ScopedRules{MaxBitrateKbps: &tc.preferred}); err != nil {
			t.Fatal(err)
		}
		play := &playRequest{key: fightClub.Key, audioSel: -1, opts: core.DecideOptions{MaxBitrateKbps: tc.requested}}
		f.srv.applyStreamPreferences(context.Background(), &Request{User: viewer, ClientID: "tv"}, play, &fightClub, info)
		if play.opts.MaxBitrateKbps != tc.want {
			t.Fatalf("request %d preference %d: %+v", tc.requested, tc.preferred, play.opts)
		}
	}
}

func TestPersistentShowChoiceAppliesToNextEpisodeDecision(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefs := trackprefs.New(db)
	f.srv.UseTrackPreferences(prefs)
	r := trackprefs.Default()
	r.AudioLanguages = []string{"en"}
	r.SubtitleLanguages = []string{"en"}
	r.SubtitleMode = "always"
	r.Loudness = true
	if err = prefs.PutRules(context.Background(), 0, r); err != nil {
		t.Fatal(err)
	}
	f.get(t, "/library/metadata/"+bbE1.Key.String())
	frID := f.bindStream(bbE1.Key, 2, sampleInfo.Streams[2])
	f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/%s?audioStreamID=%d&subtitleStreamID=0", bbE1.Key, frID), nil)
	f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+bbE2.Key.String()+"&session=showmemory")
	if f.trans.lastOpts.AudioStream != 2 || f.trans.lastOpts.SubtitleStream != -1 || len(f.trans.lastOpts.AudioFilters) != 1 {
		t.Fatalf("decision did not use show memory %+v", f.trans.lastOpts)
	}
	f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&session=othermovie")
	if f.trans.lastOpts.AudioStream == 2 {
		t.Fatal("show choice leaked to movie")
	}
}
func TestDecisionIncludesProbedChaptersAndMarkers(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info.Chapters = []core.Chapter{{ID: 3, Title: "Intro", Start: 5 * time.Second, End: 45 * time.Second}, {ID: 4, Title: "Credits", Start: f.playback.info.Duration - time.Minute, End: f.playback.info.Duration}}
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+bbE1.Key.String()+"&session=markers")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<Marker id="1" type="intro" startTimeOffset="5000" endTimeOffset="45000" final="0"`) || !strings.Contains(rec.Body.String(), `index="1" tag="Intro"`) || !strings.Contains(rec.Body.String(), `<Chapter id="`) {
		t.Fatalf("missing feature metadata %s", rec.Body.String())
	}
}

func TestMDECannotBypassRequestedAudioProcessing(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefs := trackprefs.New(db)
	f.srv.UseTrackPreferences(prefs)
	r := trackprefs.Default()
	r.Loudness = true
	if err = prefs.PutRules(context.Background(), 0, r); err != nil {
		t.Fatal(err)
	}
	f.trans.decision = core.Decision{Kind: core.DecisionTranscode, Container: "mpegts", VideoCopy: true, AudioCopy: false, AudioCodec: "aac"}
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&session=processing")
	if !strings.Contains(rec.Body.String(), `streamType="2" decision="transcode"`) || strings.Contains(rec.Body.String(), `mdeDecisionCode="1000"`) {
		t.Fatalf("MDE bypassed filter decision: %s", rec.Body.String())
	}
}

type fakeMarkers []markers.Marker

func (f fakeMarkers) Markers(context.Context, core.RatingKey, core.MediaInfo) ([]markers.Marker, error) {
	return append([]markers.Marker(nil), f...), nil
}

// A stinger splits credits in two; PMS renders both, in timeline order,
// with final="1" only on the last credits marker — even if a provider
// mistakenly flags an earlier one.
func TestMultipleCreditsMarkersRendering(t *testing.T) {
	srv := &Server{d: testDeps(Deps{}), log: slog.Default()}
	srv.UseMarkers(fakeMarkers{
		{Type: "credits", StartMS: 5_800_000, EndMS: 6_000_000, Final: true},
		{Type: "intro", StartMS: 30_000, EndMS: 90_000},
		{Type: "credits", StartMS: 5_400_000, EndMS: 5_760_000, Final: true},
		{Type: "recap", StartMS: 0, EndMS: 20_000},
	})
	info := core.MediaInfo{Duration: 100 * time.Minute}
	var m model.Metadata
	srv.addProbedStreamFeatures(context.Background(), core.MovieKey(603), info, &m)
	if len(m.Marker) != 3 {
		t.Fatalf("markers %+v", m.Marker)
	}
	intro, first, last := m.Marker[0], m.Marker[1], m.Marker[2]
	if intro.ID != 1 || intro.Type != "intro" || intro.StartTimeOffset != 30_000 || *intro.Final {
		t.Fatalf("intro %+v", intro)
	}
	if first.Type != "credits" || first.StartTimeOffset != 5_400_000 || first.EndTimeOffset != 5_760_000 || *first.Final {
		t.Fatalf("first credits must not be final: %+v", first)
	}
	if last.Type != "credits" || last.StartTimeOffset != 5_800_000 || last.EndTimeOffset != 6_000_000 || !*last.Final {
		t.Fatalf("last credits must be final: %+v", last)
	}
}

func TestDecisionDurationIncludesLateCredits(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info.Duration = 1497504 * time.Millisecond
	f.playback.info.Chapters = []core.Chapter{{ID: 1, Title: "Credits", Start: 1460550 * time.Millisecond, End: f.playback.info.Duration}}
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+bbE1.Key.String()+"&session=late-credits")
	var mc struct {
		Video []struct {
			Duration int64 `xml:"duration,attr"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &mc); err != nil {
		t.Fatal(err)
	}
	if len(mc.Video) != 1 || mc.Video[0].Duration != 1497504 {
		t.Fatalf("actual runtime lost: %s", rec.Body.String())
	}
}

func TestOwnedPauseTimelineReachesEncoder(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=/library/metadata/"+fightClub.Key.String()+"&session=pause-output&directPlay=0"))
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=/library/metadata/"+fightClub.Key.String()+"&session=pause-output&directPlay=0")
	if rec.Code != 200 {
		t.Fatalf("start: %s", rec.Body.String())
	}
	h := f.trans.sessions["pause-output"]
	url := "/:/timeline?ratingKey=" + fightClub.Key.String() + "&state=paused&time=60000"
	f.get(t, url, "X-Plex-Client-Identifier", "other-client")
	if h.playbackState != "" {
		t.Fatal("another client extended output lifetime")
	}
	f.get(t, url)
	if h.playbackState != "paused" {
		t.Fatal("owned pause did not reach encoder")
	}
	f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=playing&time=61000")
	if h.playbackState != "playing" {
		t.Fatal("resume did not reach encoder")
	}
}
