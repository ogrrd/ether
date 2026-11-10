package plex

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestBrokerPublishesSelectedTracksHLSAndDASH(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	audio := f.bindStream(fightClub.Key, 2, sampleInfo.Streams[2])
	sub := f.bindStream(fightClub.Key, 4, sampleInfo.Streams[4])
	f.srv.d.Transcoder = &dashTranscoder{fakeTranscoder: f.trans, dash: map[string]*fakeDASH{}}
	key := strconv.FormatInt(int64(fightClub.Key), 10)
	cases := []struct {
		name, session, path string
	}{
		{"decision", "dec-broker", "/video/:/transcode/universal/decision?path=/library/metadata/" + key + "&session=dec-broker&directPlay=0&directStream=1&protocol=hls&audioStreamID=" + strconv.FormatInt(audio, 10)},
		{"hls", "hls-broker", "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F" + key + "&session=hls-broker&protocol=hls&directPlay=0&directStream=1&subtitleStreamID=" + strconv.FormatInt(sub, 10)},
		{"dash", "dash-broker", "/video/:/transcode/universal/start.mpd?path=%2Flibrary%2Fmetadata%2F" + key + "&session=dash-broker&protocol=dash&directPlay=0&directStream=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, tc.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := publishedPlexSession(t, f.srv.playbackBroker, tc.session); got != tc.session {
				t.Fatalf("broker session %q", got)
			}
		})
	}
}

func TestUnmountReleasesPlaybackResources(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	dt := &dashTranscoder{fakeTranscoder: f.trans, dash: map[string]*fakeDASH{}}
	f.srv.d.Transcoder = dt
	broker := f.srv.playbackBroker
	inst, err := Frontend{Server: f.srv}.Mount(context.Background(), frontend.Host{Sessions: broker})
	if err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(int64(fightClub.Key), 10)
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+key+"&session=hls-unmount&protocol=hls&directPlay=0&directStream=1"))
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+key+"&session=hls-unmount&protocol=hls&directPlay=0&directStream=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("start %d: %s", rec.Code, rec.Body.String())
	}
	if publishedPlexSession(t, broker, "hls-unmount") != "hls-unmount" {
		t.Fatal("encoder was not published")
	}
	if err := inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.srv.ctx.Err() != nil {
		t.Fatal("unmount closed the shared server")
	}
	if _, ok := dt.Session("hls-unmount"); ok {
		t.Fatal("encoder still running")
	}
	lookup := broker.(frontend.SessionRuntime)
	if id, err := lookup.PlexSessionForPlayback(context.Background(), plexOwnerAccess(), fightClub.Key, "", "hls-unmount"); err == nil && id != "" {
		t.Fatalf("broker session survived unmount: %s", id)
	}
	again, err := Frontend{Server: f.srv}.Mount(context.Background(), frontend.Host{Sessions: broker})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close(context.Background())
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+key+"&session=hls-remount&protocol=hls&directPlay=0&directStream=1"))
	rec = f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+key+"&session=hls-remount&protocol=hls&directPlay=0&directStream=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("remount start %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBareTimelineRecordsProgressWithoutPreparing(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=120000&duration=8340000")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline %d", rec.Code)
	}
	if len(f.state.progress) != 1 || f.state.progress[0].offset != 2*time.Minute || f.state.progress[0].state != "playing" {
		t.Fatalf("progress %+v", f.state.progress)
	}
	if len(f.playback.Sessions()) != 0 {
		t.Fatal("timeline prepared a source")
	}
}

func publishedPlexSession(t *testing.T, broker frontend.Sessions, session string) string {
	t.Helper()
	lookup, ok := broker.(frontend.SessionRuntime)
	if !ok {
		t.Fatal("playback broker has no plex lookup")
	}
	id, err := lookup.PlexSessionForPlayback(context.Background(), plexOwnerAccess(), fightClub.Key, "", session)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func plexOwnerAccess() frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: &ownerUser, Device: core.Device{ID: "client-1"}, Protocol: "plex"}
}
