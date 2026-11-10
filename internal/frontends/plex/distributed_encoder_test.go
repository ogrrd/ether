package plex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type recordedPlexEncoder struct {
	expectedRelease                  string
	grantForwards                    int
	forwardHandler                   http.Handler
	forwardID                        string
	sessions                         map[string]core.HLSSession
	starts, stops, lookups, forwards int
	viewer                           int64
	playID, publicID                 string
	request                          core.HLSRequest
	startErr                         error
}

func (e *recordedPlexEncoder) StartSegmentedHLS(_ context.Context, u *core.User, playID, id string, q core.HLSRequest) (core.HLSSession, error) {
	e.starts++
	if u == nil || playID == "" || id == "" || q.SessionID != id {
		return nil, core.ErrUnauthorized
	}
	if e.expectedRelease != "" && q.ReleaseID != e.expectedRelease {
		return nil, core.ErrSourceChanged
	}
	e.viewer, e.playID, e.publicID, e.request = u.ID, playID, id, q
	if e.startErr != nil {
		return nil, e.startErr
	}
	hs := &remotePlexFixtureSession{fakeDASH: fakeDASH{fakeHLS{id: id, req: q}}}
	e.sessions[id] = hs
	return hs, nil
}
func (e *recordedPlexEncoder) SegmentedHLSSession(id string) (core.HLSSession, bool) {
	e.lookups++
	hs, ok := e.sessions[id]
	return hs, ok
}
func (e *recordedPlexEncoder) StopSegmentedHLS(id string) bool {
	if _, ok := e.sessions[id]; !ok {
		return false
	}
	e.stops++
	delete(e.sessions, id)
	return true
}
func (e *recordedPlexEncoder) ForwardPlexMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	e.forwards++
	if e.forwardHandler != nil && id == e.forwardID {
		// Simulate a fresh private receiver invoking the complete public handler:
		// no sender user/context/acceptance assertion is transferred.
		request := httptest.NewRequest(r.Method, r.URL.RequestURI(), nil)
		request.Header = r.Header.Clone()
		request.RemoteAddr, request.Host, request.TLS = r.RemoteAddr, r.Host, r.TLS
		request.Header.Set("X-Ether-Delivery-Hop", "1")
		e.forwardHandler.ServeHTTP(w, request)
		return true
	}
	return false
}

type remotePlexFixtureSession struct{ fakeDASH }

func (s *remotePlexFixtureSession) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(206)
		if r.Method != "HEAD" {
			_, _ = w.Write([]byte("abc"))
		}
		return
	}
	s.fakeHLS.ServeSegment(w, r, n)
}

func TestPlexEncoderUniversalRoutesPreserveAuthorization(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	f := newFixture(func(d *Deps) { d.Encoder = remote })
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	startPath := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=remote-plex"
	f.get(t, decisionPath(startPath))
	master := f.get(t, startPath)
	if master.Code != 200 || remote.starts != 1 || remote.viewer != ownerUser.ID || remote.playID == "" || remote.publicID != "remote-plex" || len(f.trans.sessions) != 0 {
		t.Fatalf("universal start bypassed broker: %d %s", master.Code, master.Body.String())
	}
	if master.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" || !strings.Contains(master.Body.String(), "session/remote-plex/base/index.m3u8?") || strings.Contains(master.Body.String(), "/cluster/") {
		t.Fatal("public master contract changed")
	}
	entry, _ := f.srv.transcodes.get("remote-plex")
	path := "/video/:/transcode/universal/session/remote-plex/base/00042.ts"
	for _, tc := range []struct {
		url, token string
		status     int
		served     bool
	}{
		{path, "", 401, false}, {streamGrantURL(path, "wrong"), "", 401, false}, {path, friendToken, 403, false}, {streamGrantURL(path, entry.streamToken), "", 200, true}, {path, ownerToken, 200, true},
	} {
		before := remote.forwards
		out := f.request(t, "GET", tc.url, map[string]string{"X-Plex-Token": tc.token})
		if out.Code != tc.status || (remote.forwards > before) != tc.served {
			t.Fatalf("public credential reached private seam incorrectly: %d want %d", out.Code, tc.status)
		}
	}
	ranged := f.request(t, "GET", path, map[string]string{"X-Plex-Token": ownerToken, "Range": "bytes=0-2"})
	if ranged.Code != 206 || ranged.Body.String() != "abc" || ranged.Header().Get("Content-Range") != "bytes 0-2/3" {
		t.Fatal("range representation changed")
	}
	head := f.request(t, "HEAD", path, map[string]string{"X-Plex-Token": ownerToken, "Range": "bytes=0-2"})
	if head.Code != 206 || head.Body.Len() != 0 || head.Header().Get("Content-Range") != "bytes 0-2/3" {
		t.Fatal("HEAD representation contract changed")
	}
	before := remote.starts
	_ = f.get(t, startPath, "X-Plex-Token", friendToken)
	if remote.starts != before {
		t.Fatal("foreign viewer reached start seam")
	}
	_ = f.get(t, "/video/:/transcode/universal/stop?session=remote-plex", "X-Plex-Token", friendToken)
	if remote.stops != 0 {
		t.Fatal("foreign viewer reached stop seam")
	}
	_ = f.get(t, "/video/:/transcode/universal/stop?session=remote-plex")
	if remote.stops != 1 {
		t.Fatal("owner stop bypassed remote ownership")
	}
}

func TestPlexEncoderFailureAndMountCleanupRetainOwnership(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}, startErr: errors.New("uncertain owned remote start")}
	f := newFixture(func(d *Deps) { d.Encoder = remote })
	defer f.srv.Close()
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=pending-remote"))
	_ = f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=pending-remote")
	if remote.starts != 1 || len(f.trans.sessions) != 0 {
		t.Fatal("remote error fell back to local encoder")
	}
	remote.sessions["pending-remote"] = nil
	// Ownership, even a nil pending session, prevents local lookup/stop fallback.
	if hs, owned := f.srv.plexHLSSession("pending-remote"); !owned || hs != nil {
		t.Fatal("pending execution lost ownership")
	}
	f.srv.releaseMountedPlayback()
	if remote.stops != 1 || f.srv.currentPlexEncoder() != remote {
		t.Fatal("clearing mounted broker lost remote cleanup dependency")
	}
}

func TestPlexEncoderDASHAndHTTPStartsUseSameSeam(t *testing.T) {
	for _, tc := range []struct{ format, protocol, content string }{{"start.mpd", "dash", "application/dash+xml"}, {"start", "http", "video/x-matroska"}} {
		t.Run(tc.protocol, func(t *testing.T) {
			remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
			f := newFixture(func(d *Deps) { d.Encoder = remote })
			defer f.srv.Close()
			ps, err := f.playback.Prepare(context.Background(), &ownerUser, fightClub.Key, core.ClientCaps{ClientID: "client-1"})
			if err != nil {
				t.Fatal(err)
			}
			ps.Resolution = &core.Resolution{Release: core.Release{ID: "sourced-release-" + tc.protocol}}
			remote.expectedRelease = ps.SourceReleaseID()
			f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4"}
			f.get(t, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=remote-"+tc.protocol+"&protocol="+tc.protocol)
			out := f.get(t, "/video/:/transcode/universal/"+tc.format+"?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=remote-"+tc.protocol+"&protocol="+tc.protocol)
			if out.Code != 200 || remote.starts != 1 || remote.request.Protocol != tc.protocol || remote.request.ReleaseID == "" || remote.request.ReleaseID != ps.SourceReleaseID() || out.Header().Get("Content-Type") != tc.content || len(f.trans.sessions) != 0 {
				t.Fatalf("%s start contract changed: %d %s", tc.protocol, out.Code, out.Body.String())
			}
		})
	}
}

func TestPlexTokenMissingEntryTransfersOnlyToOwnerAuthorization(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	owner := newFixture(func(d *Deps) { d.Encoder = remote })
	defer owner.srv.Close()
	owner.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	owner.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=owner-transfer"))
	start := owner.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=owner-transfer")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	delegate := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}, forwardHandler: owner.srv, forwardID: "owner-transfer"}
	sender := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer sender.srv.Close()
	path := "/video/:/transcode/universal/session/owner-transfer/base/00042.ts"
	for _, tc := range []struct {
		token   string
		want    int
		forward bool
	}{{ownerToken, 200, true}, {friendToken, 403, true}, {"invalid", 401, false}, {"", 401, false}} {
		before := delegate.forwards
		out := sender.request(t, "GET", path, map[string]string{"X-Plex-Token": tc.token})
		if out.Code != tc.want || (delegate.forwards > before) != tc.forward {
			t.Fatalf("owner authorization transfer got %d want %d", out.Code, tc.want)
		}
	}
	if _, ok := sender.srv.transcodes.get("owner-transfer"); ok || len(sender.trans.sessions) != 0 {
		t.Fatal("terminal transfer fabricated local playback state")
	}
	// Sender acceptance cannot override owner revocation or missing ownership.
	delete(owner.srv.d.Auth.(*fakeAuth).users, ownerToken)
	out := sender.request(t, "GET", path, map[string]string{"X-Plex-Token": ownerToken})
	if out.Code != 401 {
		t.Fatal("sender token decision overrode owner revocation")
	}
	for _, tc := range []struct{ method, path string }{{"POST", path}, {"GET", "/video/:/transcode/universal/session/unknown/base/00042.ts"}, {"GET", "/video/:/transcode/universal/session/owner-transfer/x/decision"}, {"GET", "/video/:/transcode/universal/session/owner-transfer/x/../00042.ts"}} {
		before := delegate.forwards
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if _, ok := missingPlexMediaID(req); ok && strings.Contains(tc.path, "unknown") { // A recognized unknown owner must keep legacy absence behavior.
			out := sender.request(t, tc.method, tc.path, map[string]string{"X-Plex-Token": ownerToken})
			if out.Code != 404 {
				t.Fatal("unknown owner bypassed absence")
			}
			continue
		}
		if _, ok := missingPlexMediaID(req); ok {
			t.Fatal("control/ambiguous route eligible for owner transfer")
		}
		if delegate.forwards != before {
			t.Fatal("invalid route forwarded")
		}
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Ether-Delivery-Hop", "1")
	if _, ok := missingPlexMediaID(req); ok {
		t.Fatal("forwarding loop eligible")
	}
	// A nil seam retains the old authenticated missing-entry status.
	sender.srv.UsePlexEncoder(nil)
	out = sender.request(t, "GET", path, map[string]string{"X-Plex-Token": ownerToken})
	if out.Code != 404 {
		t.Fatal("nil seam changed missing-entry behavior")
	}
}

func (e *recordedPlexEncoder) ForwardPlexGrantMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	e.grantForwards++
	return e.ForwardPlexMedia(w, r, id)
}
