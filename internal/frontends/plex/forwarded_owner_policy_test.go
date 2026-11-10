package plex

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type forwardedPolicy struct{ denyWAN, denyTitle bool }

func (p *forwardedPolicy) Allowed(*core.User, *core.Title) bool { return !p.denyTitle }
func (p *forwardedPolicy) CanPlay(_ *core.User, remote bool, _ time.Time) bool {
	return !p.denyWAN || !remote
}

func TestForwardedOwnerRechecksCurrentPolicyAndOriginalNetwork(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	owner := newFixture(func(d *Deps) { d.Encoder = remote })
	defer owner.srv.Close()
	owner.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	owner.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=policy-owner"))
	start := owner.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=policy-owner")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	delegate := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}, forwardHandler: owner.srv, forwardID: "policy-owner"}
	sender := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer sender.srv.Close()
	path := "/video/:/transcode/universal/session/policy-owner/base/00042.ts"
	policy := &forwardedPolicy{}
	owner.srv.d.AccessPolicy = policy
	request := func(peer, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Plex-Token", token)
		r.Header.Set("X-Plex-Client-Identifier", "client-1")
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, r)
		return out
	}
	if got := request("198.51.100.8:5000", ownerToken); got.Code != 200 {
		t.Fatal("initial forwarded owner access", got.Code)
	}
	policy.denyWAN = true
	if got := request("198.51.100.8:5000", ownerToken); got.Code != 403 {
		t.Fatal("owner ignored current WAN restriction", got.Code)
	}
	if got := request("10.0.0.5:5000", ownerToken); got.Code != 200 {
		t.Fatal("restored LAN provenance rejected", got.Code)
	}
	policy.denyWAN = false
	policy.denyTitle = true
	if got := request("10.0.0.5:5000", ownerToken); got.Code != 403 {
		t.Fatal("title hidden after admission still streamed", got.Code)
	}
	policy.denyTitle = false
	entry, _ := owner.srv.transcodes.get("policy-owner")
	ps, _ := owner.playback.Session(entry.playID)
	ps.UserID++
	if got := request("10.0.0.5:5000", ownerToken); got.Code != 403 {
		t.Fatal("changed source user binding streamed", got.Code)
	}
	ps.UserID--
	delete(owner.srv.d.Auth.(*fakeAuth).users, ownerToken)
	if got := request("10.0.0.5:5000", ownerToken); got.Code != 401 {
		t.Fatal("owner revocation ignored", got.Code)
	}
}

func TestForwardedMarkerOnlyAddsRestrictions(t *testing.T) {
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	f := newFixture(func(d *Deps) { d.Encoder = remote })
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	f.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=spoof-policy"))
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=spoof-policy")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	path := "/video/:/transcode/universal/session/spoof-policy/base/00042.ts"
	f.srv.d.AccessPolicy = &forwardedPolicy{denyTitle: true}
	// Legacy non-forwarded local behavior is unchanged by this opt-in boundary.
	if got := f.get(t, path); got.Code != 200 {
		t.Fatal("nonforwarded path changed", got.Code)
	}
	if got := f.request(t, "GET", path, map[string]string{"X-Plex-Token": ownerToken, "X-Ether-Delivery-Hop": "1"}); got.Code != 403 {
		t.Fatal("spoof marker did not add restrictions", got.Code)
	}
	if got := f.request(t, "GET", path, map[string]string{"X-Plex-Token": "invalid", "X-Ether-Delivery-Hop": "1"}); got.Code != 401 {
		t.Fatal("spoof marker bypassed credentials", got.Code)
	}
	local := newFixture()
	defer local.srv.Close()
	local.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	local.get(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=spoof-policy"))
	if got := local.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=spoof-policy"); got.Code != 200 {
		t.Fatal("local start", got.Code)
	}
	local.srv.d.AccessPolicy = &forwardedPolicy{denyTitle: true}
	if got := local.request(t, "GET", path, map[string]string{"X-Plex-Token": ownerToken, "X-Ether-Delivery-Hop": "1"}); got.Code != 200 {
		t.Fatal("nil seam changed legacy local behavior", got.Code)
	}
}
