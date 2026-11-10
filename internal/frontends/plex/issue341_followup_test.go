package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func TestIssue341ArtworkRecording(t *testing.T) {
	rec := diagnostics.New(diagnostics.Deps{Now: func() time.Time { return fixedNow }})
	f := newFixture(func(d *testServerDeps) { d.Support = rec })
	defer f.srv.Close()
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	capture, err := rec.Start("owner", diagnostics.StartIn{DeviceID: rec.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/photo/:/transcode", "/library/metadata/1000000000718789/thumb/1"} {
		for _, dev := range []string{"client-1", "", "other-device"} {
			// Bad nested URL/missing title produce image failures without network I/O.
			r := f.get(t, path, "X-Plex-Client-Identifier", dev)
			if r.Code < 400 {
				t.Fatal(r.Code)
			}
		}
	}
	// Authentication failures remain visible when the loader identifies the device.
	unauthorized := f.get(t, "/photo/:/transcode", "X-Plex-Token", "invalid")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal(unauthorized.Code)
	}
	got, err := rec.Get("owner", capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range got.Observations {
		if e.Kind == diagnostics.KindRequest && diagnostics.ArtworkRequestPath(e.Route) {
			count += e.Count
		}
	}
	// Since #365 a capture retains every rejection authenticated as its
	// viewer, including from a companion client under another device id
	// (Plex's built-in download server calls as the viewer that way), kept
	// unattributed. So both image routes count three times each —
	// identified, viewer-only and other-device — plus the identified 401.
	// Playback-route rejections are the exception (attempt correlation);
	// artwork has no such correlation to break.
	if count != 7 {
		t.Fatalf("image requests retained = %d, want 7 (identified, viewer-only, other-device per route, and identified 401); %+v", count, got.Observations)
	}
}

// Reconstructs the reporter's PUT -> conversion decision -> start sequence.
// These are authored requests; the ZIP redacts its stream IDs and profile.
func TestIssue341PhonePUTDecisionStart(t *testing.T) {
	for _, codec := range []string{"subrip", "ass", "hdmv_pgs_subtitle"} {
		t.Run(codec, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.srv.d.Transcoder = shieldContractTranscoder{f.trans}
			f.playback.info = core.MediaInfo{Container: "mkv", Duration: 30 * time.Second, BitrateKbps: 1000, Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
				{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true},
				{Index: 2, Type: "subtitle", Codec: codec, Language: "en"},
			}}
			hdr := []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android", "X-Plex-Version", "2026.22.0", "X-Plex-Device", "SM-F976B", "X-Plex-Model", "canoe"}
			key := fightClub.Key.String()
			tags := subtitleTags(f.get(t, "/library/metadata/"+key, hdr...).Body.String())
			if len(tags) == 0 {
				t.Fatal("no subtitle tracks")
			}
			id := streamAttr(tags[0], "id")
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				t.Fatal(id)
			}
			put := f.request(t, http.MethodPut, "/library/parts/"+key+"?allParts=1&subtitleStreamID="+id, headerMap(hdr...))
			if put.Code != 200 {
				t.Fatalf("PUT: %d %s", put.Code, put.Body.String())
			}
			q := url.Values{"path": {"/library/metadata/" + key}, "protocol": {"hls"}, "directPlay": {"0"}, "directStream": {"1"}, "hasMDE": {"1"}, "subtitles": {"none"}, "advancedSubtitles": {"text"}, "session": {"i341-" + codec}, "X-Plex-Client-Profile-Extra": {"add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264&audioCodec=aac&subtitleCodec=ass,ssa,pgs,srt)"}}
			decision := f.get(t, "/video/:/transcode/universal/decision?"+q.Encode(), hdr...)
			if decision.Code != 200 || !strings.Contains(decision.Body.String(), `generalDecisionCode="1001"`) {
				t.Fatalf("decision: %d %s", decision.Code, decision.Body.String())
			}
			row := selectedTag(subtitleTags(decision.Body.String()))
			if streamAttr(row, "id") != id || streamAttr(row, "location") != "segments-av" {
				t.Fatal(row)
			}
			start := f.get(t, "/video/:/transcode/universal/start.m3u8?"+q.Encode(), hdr...)
			if start.Code != 200 {
				t.Fatalf("start: %d %s", start.Code, start.Body.String())
			}
			hs := f.trans.sessions[q.Get("session")]
			if hs == nil {
				t.Fatal("no encoder handoff")
			}
			d := hs.req.Decision
			if d.SubtitleStream != 2 || d.Container != transcode.ContainerMatroska || d.BurnSubtitles != (codec == "hdmv_pgs_subtitle") {
				t.Fatalf("handoff: %+v", d)
			}
			if r := f.get(t, "/library/metadata/"+key, hdr...); r.Code != 200 {
				t.Fatalf("played title: %d", r.Code)
			}
		})
	}
}

type issue341CancelledCatalog struct {
	core.Catalog
	cancel context.CancelFunc
}

func (c issue341CancelledCatalog) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	c.cancel()
	return nil, core.ErrNotFound
}

func TestIssue341CancelledMetadataIsNotNotFound(t *testing.T) {
	recorder := diagnostics.New(diagnostics.Deps{Now: func() time.Time { return fixedNow }})
	f := newFixture(func(d *testServerDeps) { d.Support = recorder })
	defer f.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := f.srv.catalog()
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	capture, err := recorder.Start("owner", diagnostics.StartIn{DeviceID: recorder.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.setCatalog(issue341CancelledCatalog{original, cancel})
	r := httptest.NewRequest(http.MethodGet, "/library/metadata/"+fightClub.Key.String(), nil).WithContext(ctx)
	r.Header.Set("X-Plex-Token", ownerToken)
	r.Header.Set("X-Plex-Client-Identifier", "client-1")
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	if w.Code == 404 || w.Body.Len() != 0 {
		t.Fatalf("cancelled request became a missing title: %d %s", w.Code, w.Body.String())
	}
	view, err := recorder.Get("owner", capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Observations) != 1 || view.Observations[0].Status != 499 {
		t.Fatalf("cancelled observation: %+v", view.Observations)
	}
	f.srv.setCatalog(original)
	if got := f.get(t, r.URL.Path); got.Code != 200 {
		t.Fatal(got.Code)
	}
}
