package plex

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPlexGrantMissingEntryUsesOriginalOwnerPrecedenceAndPolicy(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	owner := newFixture(func(d *Deps) { d.Encoder = remote })
	defer owner.srv.Close()
	owner.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	owner.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=grant-owner"))
	start := owner.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=grant-owner")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	delegate := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}, forwardHandler: owner.srv, forwardID: "grant-owner"}
	sender := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer sender.srv.Close()
	path := "/video/:/transcode/universal/session/grant-owner/base/00042.ts"
	entry, _ := owner.srv.transcodes.get("grant-owner")
	dual := owner.srv.mediaURL(path, "grant-owner", entry.streamToken)
	if !strings.Contains(dual, streamGrantParam+"=") || !strings.Contains(dual, readCapParam+"=") {
		t.Fatal("fixture did not use genuine dual-credential mediaURL")
	}
	read := owner.srv.readCaps.token("grant-owner")
	grantOnly := streamGrantURL(path, entry.streamToken)
	readOnly := readCapURL(path, read)
	wrongGrantRead := readCapURL(streamGrantURL(path, "wrong"), read)
	request := func(target, device, peer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = peer
		if device != "" {
			req.Header.Set("X-Plex-Client-Identifier", device)
		}
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, req)
		return out
	}
	for _, tc := range []struct {
		uri, device string
		want        int
	}{{dual, "", 200}, {grantOnly, "", 200}, {wrongGrantRead, "client-1", 200}, {wrongGrantRead, "other-device", 401}, {wrongGrantRead, "", 401}, {readOnly, "client-1", 200}, {readOnly, "other-device", 401}, {readOnly, "", 401}, {streamGrantURL(path, "wrong"), "", 401}} {
		out := request(tc.uri, tc.device, "10.0.0.5:5000")
		if out.Code != tc.want {
			t.Fatalf("owner grant precedence/device got %d want %d", out.Code, tc.want)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, dual, nil)
		req.RemoteAddr = "10.0.0.5:5000"
		req.Header.Set("Range", "bytes=0-2")
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, req)
		if out.Code != 206 || out.Header().Get("Content-Range") != "bytes 0-2/3" || (method == http.MethodHead && out.Body.Len() != 0) {
			t.Fatal("grant transfer changed HEAD/range representation", method, out.Code)
		}
	}
	if _, ok := sender.srv.transcodes.get("grant-owner"); ok || len(sender.trans.sessions) != 0 {
		t.Fatal("grant transfer fabricated local entry/encoder")
	}
	policy := &forwardedPolicy{denyWAN: true}
	owner.srv.d.AccessPolicy = policy
	if out := request(dual, "", "198.51.100.8:5000"); out.Code != 401 && out.Code != 403 {
		t.Fatal("forwarded grant WAN restriction ignored", out.Code)
	}
	policy.denyWAN = false
	policy.denyTitle = true
	if out := request(dual, "", "10.0.0.5:5000"); out.Code != 403 {
		t.Fatal("forwarded grant hidden title streamed", out.Code)
	}
	policy.denyTitle = false
	delete(owner.srv.d.Auth.(*fakeAuth).users, ownerToken)
	if out := request(dual, "", "10.0.0.5:5000"); out.Code != 401 {
		t.Fatal("revoked owner grant accepted", out.Code)
	}
}

func TestPlexGrantTransferRejectsAmbiguityAndPreservesLocalNil(t *testing.T) {
	delegate := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	sender := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer sender.srv.Close()
	path := "/video/:/transcode/universal/session/missing/base/00042.ts"
	for _, suffix := range []string{"", "?" + streamGrantParam + "=one&" + streamGrantParam + "=two", "?" + strings.ToLower(streamGrantParam) + "=one", "?" + streamGrantParam + "=one&X-Plex-Token=invalid", "?" + streamGrantParam + "=one&" + readCapParam + "=a&" + readCapParam + "=b", "?" + streamGrantParam + "=", "?" + streamGrantParam + "=" + strings.Repeat("x", 513), "?" + streamGrantParam + "=%00"} {
		before := delegate.grantForwards
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, httptest.NewRequest("GET", path+suffix, nil))
		if out.Code != 401 || delegate.grantForwards != before {
			t.Fatal("ambiguous/missing/account credential reached grant transport", out.Code)
		}
	}
	for _, header := range []string{"Authorization", streamGrantParam, readCapParam, "X-Ether-Delivery-Hop"} {
		before := delegate.grantForwards
		req := httptest.NewRequest("GET", streamGrantURL(path, "one"), nil)
		req.Header.Set(header, "one")
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, req)
		if out.Code != 401 || delegate.grantForwards != before {
			t.Fatal("ambiguous carrier/loop reached grant transport", header, out.Code)
		}
	}
	req := httptest.NewRequest("GET", readCapURL(path, "read"), nil)
	if _, ok := grantPlexMediaID(req, &Request{}); ok {
		t.Fatal("readcap-only without device eligible")
	}
	// Recognized unknown owner still fails closed; no local fallback or entry.
	out := httptest.NewRecorder()
	sender.srv.ServeHTTP(out, httptest.NewRequest("GET", streamGrantURL(path, "one"), nil))
	if out.Code != 401 || delegate.grantForwards == 0 {
		t.Fatal("unknown owner changed closed behavior")
	}
	sender.srv.UsePlexEncoder(nil)
	before := delegate.grantForwards
	out = httptest.NewRecorder()
	sender.srv.ServeHTTP(out, httptest.NewRequest("GET", streamGrantURL(path, "one"), nil))
	if out.Code != 401 || delegate.grantForwards != before {
		t.Fatal("nil grant seam changed authentication")
	}
	// Failed authentication for an existing local entry never delegates elsewhere.
	local := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer local.srv.Close()
	local.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	local.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=local-grant"))
	start := local.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=local-grant")
	if start.Code != 200 {
		t.Fatal(start.Code)
	}
	before = delegate.grantForwards
	localPath := "/video/:/transcode/universal/session/local-grant/base/00042.ts?" + url.QueryEscape(streamGrantParam) + "=wrong"
	out = httptest.NewRecorder()
	local.srv.ServeHTTP(out, httptest.NewRequest("GET", localPath, nil))
	if out.Code != 401 || delegate.grantForwards != before {
		t.Fatal("local entry failure delegated")
	}
}
