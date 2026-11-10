package plex

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestDecisionSessionResumePointInvariant drives the real client sequence
// (decision, start, segments, timelines) through a decision-armed converted
// session. Delivered progress moves the resume point; a failed or aborted
// attempt leaves the viewer's previous place and view count untouched (#331).
func TestDecisionSessionResumePointInvariant(t *testing.T) {
	const (
		previous = 30 * time.Minute
		length   = 100 * time.Minute
	)
	for _, tc := range []struct {
		name      string
		deliver   bool // a segment reaches the player
		failed    bool // the progressive/hardware encoder broke mid-response
		startFail bool // the source recorded a failed start
		stopFirst bool // the client stops the encoder before its terminal timeline
		reports   []resumeReport
		want      time.Duration
		watched   bool
	}{
		{name: "delivered progress sets resume", deliver: true, reports: []resumeReport{{"playing", previous + 5*time.Minute}, {"stopped", previous + 5*time.Minute}}, want: previous + 5*time.Minute},
		{name: "delivered rewind sets resume", deliver: true, reports: []resumeReport{{"playing", 10 * time.Minute}, {"stopped", 10 * time.Minute}}, want: 10 * time.Minute},
		{name: "delivered completion is watched", deliver: true, reports: []resumeReport{{"playing", previous + time.Minute}, {"playing", 95 * time.Minute}, {"stopped", 95 * time.Minute}}, watched: true},
		{name: "abort before delivery keeps resume", reports: []resumeReport{{"playing", 0}, {"buffering", previous + 2*time.Minute}, {"stopped", length}}, want: previous},
		{name: "abort after encoder stop keeps resume", stopFirst: true, reports: []resumeReport{{"buffering", previous + 2*time.Minute}, {"stopped", length}}, want: previous},
		{name: "abort at zero keeps resume", reports: []resumeReport{{"buffering", 0}, {"stopped", 0}}, want: previous},
		{name: "delivered zero then stop keeps resume", deliver: true, reports: []resumeReport{{"buffering", 0}, {"stopped", 0}}, want: previous},
		{name: "encoder failure keeps resume", deliver: true, failed: true, reports: []resumeReport{{"playing", previous + 3*time.Minute}, {"stopped", length}}, want: previous},
		{name: "failed start keeps resume", deliver: true, startFail: true, reports: []resumeReport{{"playing", previous + 3*time.Minute}, {"playing", 95 * time.Minute}, {"stopped", length}}, want: previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.playback.info.Duration = length
			watch := state.New(store.OpenTest(t), state.Options{})
			f.srv.setState(watch)
			tr := &deliveryTranscoder{fakeTranscoder: f.trans, failed: tc.failed}
			f.srv.d.Transcoder = tr
			ctx := context.Background()
			if err := watch.Progress(ctx, ownerUser.ID, fightClub.Key, previous, length, "playing"); err != nil {
				t.Fatal(err)
			}
			hdr := []string{"X-Plex-Client-Identifier", "resume-client", "X-Plex-Session-Identifier", "play-id", "Accept", "application/json"}
			for _, path := range []string{"decision?", "start.m3u8?"} {
				if r := f.get(t, "/video/:/transcode/universal/"+path+issue94Query("resume-trans", 3000), hdr...); r.Code != http.StatusOK {
					t.Fatalf("%s HTTP %d %s", path, r.Code, r.Body.String())
				}
			}
			if !f.srv.decisionArmed(&Request{User: &ownerUser, ClientID: "resume-client", SessionID: "play-id"}, fightClub.Key) {
				t.Fatal("decision did not arm the session")
			}
			ps, ok := f.playback.SessionFor(ownerUser.ID, "resume-client", fightClub.Key)
			if !ok {
				t.Fatal("no source session")
			}
			ps.BytesServed = 4 << 20 // ffmpeg read-ahead is never proof the player saw a frame
			if tc.startFail {
				ps.Experience = &core.PlayExperience{StartFailure: "decoder failed"}
			}
			if tc.deliver {
				if r := f.get(t, "/video/:/transcode/universal/session/resume-trans/base/00000.ts", hdr...); r.Code != http.StatusOK {
					t.Fatalf("segment HTTP %d", r.Code)
				}
				if !tr.delivered.Load() {
					t.Fatal("segment did not record delivery")
				}
			}
			for i, report := range tc.reports {
				if tc.stopFirst && i == len(tc.reports)-1 {
					f.get(t, "/video/:/transcode/universal/stop?session=resume-trans", hdr...)
				}
				path := fmt.Sprintf("/:/timeline?ratingKey=%s&state=%s&time=%d&duration=%d", fightClub.Key, report.state, report.at.Milliseconds(), length.Milliseconds())
				if r := f.get(t, path, hdr...); r.Code != http.StatusOK {
					t.Fatalf("timeline %v HTTP %d %s", report, r.Code, r.Body.String())
				}
			}
			got, err := watch.Get(ctx, ownerUser.ID, fightClub.Key)
			if err != nil {
				t.Fatal(err)
			}
			if tc.watched {
				if got.ViewCount != 1 || got.ViewOffset != 0 {
					t.Fatalf("real completion: %+v", got)
				}
				return
			}
			if got.ViewCount != 0 || got.ViewOffset != tc.want {
				t.Fatalf("ViewOffset %v ViewCount %d, want %v and unwatched", got.ViewOffset, got.ViewCount, tc.want)
			}
		})
	}
}

// deliveryTranscoder reports output delivery once a segment is served, as
// the real HLS and progressive sessions do.
type deliveryTranscoder struct {
	*fakeTranscoder
	failed    bool
	delivered atomic.Bool
	mu        sync.Mutex
	wrapped   map[string]*deliveryHLS
}

func (d *deliveryTranscoder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	hs, err := d.fakeTranscoder.StartHLS(ctx, req)
	if err != nil {
		return nil, err
	}
	return d.wrap(hs.(*fakeHLS)), nil
}

func (d *deliveryTranscoder) Session(id string) (core.HLSSession, bool) {
	hs, ok := d.fakeTranscoder.Session(id)
	if !ok {
		return nil, false
	}
	return d.wrap(hs.(*fakeHLS)), true
}

func (d *deliveryTranscoder) wrap(h *fakeHLS) *deliveryHLS {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wrapped == nil {
		d.wrapped = map[string]*deliveryHLS{}
	}
	if w, ok := d.wrapped[h.id]; ok && w.fakeHLS == h {
		return w
	}
	w := &deliveryHLS{fakeHLS: h, owner: d}
	d.wrapped[h.id] = w
	return w
}

type deliveryHLS struct {
	*fakeHLS
	owner *deliveryTranscoder
}

func (h *deliveryHLS) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	h.fakeHLS.ServeSegment(w, r, n)
	h.owner.delivered.Store(true)
}

func (h *deliveryHLS) ServeRungSegment(w http.ResponseWriter, r *http.Request, rung string, n int) {
	h.fakeHLS.ServeRungSegment(w, r, rung, n)
	h.owner.delivered.Store(true)
}

func (h *deliveryHLS) PlaybackDelivered() bool { return h.owner.delivered.Load() }
func (h *deliveryHLS) ProgressiveFailed() bool { return h.owner.failed }

type resumeReport struct {
	state string
	at    time.Duration
}
