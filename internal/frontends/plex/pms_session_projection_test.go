package plex

import (
	"encoding/xml"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPMSSessionProjectionAndPublicIdentity(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.get(t, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&hasMDE=1&directPlay=1&session=projection", "X-Plex-Platform", "Chrome")
	if rec := f.get(t, "/library/parts/1000000000000550/1/file.mkv", "Range", "bytes=0-9"); rec.Code != 206 {
		t.Fatalf("media delivery: %d", rec.Code)
	}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1234&duration=10000", "X-Plex-Platform", "Chrome")
	rec := f.get(t, "/status/sessions", jsonAccept...)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rows := decodeJSON(t, rec)["Metadata"].([]any)
	md := rows[0].(map[string]any)
	if _, ok := md["librarySectionID"].(string); !ok {
		t.Fatalf("section id: %#v", md)
	}
	if _, ok := md["Image"]; ok {
		t.Fatal("session contains Image")
	}
	if _, ok := md["Guid"]; ok {
		t.Fatal("session contains Guid")
	}
	if md["sessionKey"] == nil || md["viewOffset"] != float64(1234) {
		t.Fatalf("timeline identity/offset: %#v", md)
	}
	sess := md["Session"].(map[string]any)
	player := md["Player"].(map[string]any)
	if sess["id"] != player["machineIdentifier"] {
		t.Fatalf("public fallback: %#v %#v", sess, player)
	}
	if player["profile"] != "Web" {
		t.Fatalf("resolved profile: %#v", player)
	}
	media := md["Media"].([]any)[0].(map[string]any)
	if _, ok := media["id"].(string); !ok {
		t.Fatalf("media id: %#v", media)
	}
	for _, p := range media["Part"].([]any) {
		part := p.(map[string]any)
		if _, ok := part["id"].(string); !ok {
			t.Fatalf("part id: %#v", part)
		}
	}
	rec = f.get(t, "/status/sessions/terminate?sessionId="+md["sessionKey"].(string))
	if rec.Code != 404 {
		t.Fatalf("sessionKey accepted as public id: %d", rec.Code)
	}
	rec = f.get(t, "/status/sessions/terminate?sessionId="+sess["id"].(string))
	if rec.Code != 200 {
		t.Fatalf("public id refused: %d", rec.Code)
	}
}

func TestPMSSessionProjectionNestedTypes(t *testing.T) {
	md := model.Metadata{LibrarySectionID: 1, Image: []model.Image{{}}, Media: []model.Media{{ID: 9, AspectRatio: 1.5, Part: []model.Part{{ID: 7, Stream: []model.Stream{{ID: 8}}}}}}}
	sessionItem(&md)
	mc := model.MediaContainer{Size: 1, Metadata: []model.Metadata{md}}
	b, err := model.Marshal(mc, model.JSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"9"`, `"aspectRatio":"1.5"`, `"id":"7"`, `"id":"8"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s: %s", want, b)
		}
	}
	x, err := model.Marshal(mc, model.XML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(x), `<Media aspectRatio="1.5" id="9"`) {
		t.Fatalf("unsorted media: %s", x)
	}
}

func TestPMSDecisionArmingIsSessionAndItemScoped(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	start := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=arm"
	if got := f.get(t, start); got.Code != 400 {
		t.Fatalf("unarmed start: %d", got.Code)
	}
	decision := strings.Replace(start, "start.m3u8", "decision", 1)
	if got := f.get(t, decision, "X-Plex-Session-Identifier", "first"); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := f.get(t, start, "X-Plex-Session-Identifier", "second"); got.Code != 400 {
		t.Fatalf("another player borrowed decision: %d", got.Code)
	}
	other := strings.Replace(start, "1000000000000550", alien.Key.String(), 1)
	if got := f.get(t, other, "X-Plex-Session-Identifier", "first"); got.Code != 400 {
		t.Fatalf("another item borrowed decision: %d", got.Code)
	}
	if got := f.get(t, start, "X-Plex-Session-Identifier", "first"); got.Code != 200 {
		t.Fatalf("armed start: %d %s", got.Code, got.Body.String())
	}
}

func TestPMSSessionExpiryBoundary(t *testing.T) {
	tracker := newSessionActivities()
	now := time.Now()
	tracker.update("live", func(e *playEntry) { e.updatedAt = now; e.key = core.MovieKey(550) })
	if got := tracker.expire(now.Add(180*time.Second), 180*time.Second); len(got) != 0 {
		t.Fatal("expired at cutoff")
	}
	if got := tracker.expire(now.Add(180*time.Second+time.Nanosecond), 180*time.Second); len(got) != 1 {
		t.Fatal("did not expire after cutoff")
	}
}

func TestPMSSessionControlUnknownGrammar(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, action := range []string{"ping", "stop"} {
		for _, tc := range []struct {
			q      string
			status int
		}{{"", 400}, {"?session=", 404}, {"?session=unknown", 404}} {
			rec := f.get(t, "/video/:/transcode/universal/"+action+tc.q)
			if rec.Code != tc.status {
				t.Errorf("%s%s: %d want %d", action, tc.q, rec.Code, tc.status)
			}
		}
	}
	for _, path := range []string{"/status/sessions/garbage", "/transcode/sessions/unknown"} {
		if rec := f.get(t, path); rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := f.request(t, "PATCH", "/status/sessions", nil); rec.Code != 404 {
		t.Fatalf("PATCH sessions: %d", rec.Code)
	}
	if rec := f.get(t, "/status/sessions/terminate"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "400") {
		t.Fatalf("missing terminate id: %d", rec.Code)
	}
}

func TestPMSSessionAppearanceLifecycle(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	count := func() int { return int(decodeJSON(t, f.get(t, "/status/sessions", jsonAccept...))["size"].(float64)) }
	f.get(t, "/library/parts/1000000000000550/file.mkv")
	if count() != 0 {
		t.Fatal("part admission created Now Playing")
	}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=paused&time=1000&duration=10000")
	if count() != 0 {
		t.Fatal("initial pause created visible session")
	}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000")
	rec := decodeJSON(t, f.get(t, "/status/sessions", jsonAccept...))
	if rec["size"] != float64(1) {
		t.Fatalf("playing not listed: %#v", rec)
	}
	md := rec["Metadata"].([]any)[0].(map[string]any)
	if _, ok := md["Session"]; ok {
		t.Fatal("unarmed player has Session")
	}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000&duration=10000")
	if count() != 0 {
		t.Fatal("stop retained session")
	}
}

func TestPMSTerminationBlocksOwnerKillsOnly(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	pr := &Request{User: &core.User{ID: 1}, ClientID: "client", SessionID: "session"}
	id := trackID(1, "client", "session")
	f.srv.setTermination(id, "failed source")
	if f.srv.playbackTerminated(pr) {
		t.Fatal("failed source blocked retry")
	}
	f.srv.takeTermination(id)
	f.srv.setOwnerTermination(id, "owner stop")
	if !f.srv.playbackTerminated(pr) {
		t.Fatal("owner stop did not block")
	}
	if _, ok := f.srv.takeTermination(id); !ok {
		t.Fatal("missing owner notice")
	}
	if _, ok := f.srv.takeTermination(id); ok {
		t.Fatal("repeated owner notice")
	}
	if !f.srv.playbackTerminated(pr) {
		t.Fatal("notice consumption allowed reopen")
	}
	f.srv.setTermination(id, "late source failure")
	if !f.srv.playbackTerminated(pr) {
		t.Fatal("late failure erased owner stop")
	}
}

// decisionPath preserves the player's session and request options while selecting
// the decision endpoint for tests that exercise a subsequent media start.
func decisionPath(startPath string) string {
	path, query, _ := strings.Cut(startPath, "?")
	i := strings.LastIndex(path, "/")
	return path[:i+1] + "decision?" + query
}

func TestPMSDecisionArmsAreProcessScoped(t *testing.T) {
	db := store.OpenTest(t)
	f := newFixture(func(d *testServerDeps) { d.DB = db })
	defer f.srv.Close()
	pr := &Request{User: &ownerUser, ClientID: "client", SessionID: "play"}
	f.srv.armDecision(pr, fightClub.Key)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM plex_decision_arms WHERE node_id='standalone'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("arm not durable: %d %v", n, err)
	}
	if _, err := db.Exec(`INSERT INTO plex_decision_arms(user_id,client_id,session_id,title_key,expires,node_id) VALUES(1,'remote','remote',?,?,'other-node')`, fightClub.Key, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	if err := db.QueryRow(`SELECT count(*) FROM plex_decision_arms`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("process cleanup removed another node or left own arms: %d %v", n, err)
	}
}

func TestPMSPlayerIdentityAttributeOrder(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	headers := []string{"X-Plex-Client-Identifier", "identity-client", "X-Plex-Session-Identifier", "identity-session", "X-Plex-Product", "Player Product", "X-Plex-Version", "2.0", "X-Plex-Platform", "Chrome", "X-Plex-Platform-Version", "10", "X-Plex-Device", "Device", "X-Plex-Device-Name", "Living Room", "X-Plex-Model", "Model", "X-Plex-Device-Vendor", "Ignored Vendor", "X-Plex-Client-Profile-Name", "Chrome"}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000", headers...)
	rec := f.get(t, "/status/sessions")
	decoder := xml.NewDecoder(rec.Body)
	for {
		tok, err := decoder.Token()
		if err != nil {
			t.Fatalf("missing Player: %v", err)
		}
		element, ok := tok.(xml.StartElement)
		if !ok || element.Name.Local != "Player" {
			continue
		}
		var names []string
		for _, attr := range element.Attr {
			names = append(names, attr.Name.Local)
		}
		want := []string{"address", "device", "machineIdentifier", "model", "platform", "platformVersion", "product", "profile", "state", "title", "version", "local", "relayed", "secure", "userID"}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("Player attributes %v want %v", names, want)
		}
		break
	}
}
