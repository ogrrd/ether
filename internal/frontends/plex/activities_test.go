package plex

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func TestActivityRegistryLifecycleAndVisibility(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	defer f.srv.Close()
	a := model.Activity{UUID: "task", Type: "subtitle.download", UserID: ownerUser.ID, Title: "Downloading subtitles"}
	publish := func(event string) {
		f.srv.publishNotification(model.NotificationContainer{Type: "activity", Size: 1, ActivityNotification: []model.ActivityNotification{{Event: event, UUID: a.UUID, Activity: &a}}}, ownerUser.ID)
	}
	publish("started")
	xml := f.request(t, "GET", "/activities", nil)
	if !strings.Contains(xml.Body.String(), "</Activity>") {
		t.Fatalf("Activity must use paired XML tags: %s", xml.Body)
	}
	rec := f.request(t, "GET", "/activities", map[string]string{"Accept": "application/json", "X-Plex-Language": "de"})
	mc := decodeJSON(t, rec)
	rows := mc["Activity"].([]any)
	row := rows[0].(map[string]any)
	if len(rows) != 1 || row["uuid"] != "task" || row["title"] != "Untertitel werden heruntergeladen" || row["progress"] != float64(0) {
		t.Fatalf("active task: %s", rec.Body)
	}
	rec = f.request(t, "GET", "/activities", map[string]string{"Accept": "application/json", "X-Plex-Token": friendToken})
	if decodeJSON(t, rec)["size"] != float64(0) {
		t.Fatalf("leaked task: %s", rec.Body)
	}
	a.Progress = 50
	publish("updated")
	rec = f.request(t, "GET", "/activities", map[string]string{"Accept": "application/json"})
	if decodeJSON(t, rec)["Activity"].([]any)[0].(map[string]any)["progress"] != float64(50) {
		t.Fatal(rec.Body)
	}
	a.Progress = 100
	publish("ended")
	rec = f.request(t, "GET", "/activities", map[string]string{"Accept": "application/json"})
	if decodeJSON(t, rec)["size"] != float64(0) {
		t.Fatalf("stale task: %s", rec.Body)
	}
}

func TestActivityAndUpdaterContracts(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	defer f.srv.Close()
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"PUT", "/activities/missing", 400}, {"DELETE", "/activities/missing", 404}, {"GET", "/activities/missing", 404},
		{"GET", "/updater/check", 404}, {"GET", "/updater/apply", 404}, {"PUT", "/updater/apply", 400},
	} {
		rec := f.request(t, test.method, test.path, nil)
		if rec.Code != test.status {
			t.Errorf("%s %s = %d", test.method, test.path, rec.Code)
		}
	}
	sub, _ := f.srv.notifier.subscribe([]string{"update.statechange"}, &ownerUser)
	defer f.srv.notifier.unsubscribe(sub)
	rec := f.request(t, "PUT", "/updater/check", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	select {
	case event := <-sub.ch:
		var body struct {
			NotificationContainer struct {
				AutoUpdateNotification []struct {
					State      string
					CanInstall bool
				}
			}
		}
		if json.Unmarshal(event.data, &body) != nil || len(body.NotificationContainer.AutoUpdateNotification) != 1 || body.NotificationContainer.AutoUpdateNotification[0].State != "error" || body.NotificationContainer.AutoUpdateNotification[0].CanInstall {
			t.Fatal(string(event.data))
		}
	default:
		t.Fatal("missing update result")
	}
	select {
	case event := <-sub.ch:
		t.Fatalf("duplicate: %s", event.data)
	default:
	}
}

func TestNotificationLanguageSelection(t *testing.T) {
	for _, test := range []struct{ plex, browser, want string }{
		{"de", "fr", "de"}, {"", "ca-ES,ca;q=0.9,en-US;q=0.8,en;q=0.7", "en-US"},
		{"", "ca,fr;q=0.9", "fr"}, {"", "nn,ca", "nn"}, {"", "nn,nb;q=0.9", "nb"},
		{"", "fr;q=0,de;q=0.5", "de"},
		{"", "zz,en;q=0.1", "en"}, {"hu-HU", "en", "hu-HU"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", test.browser)
		r = r.WithContext(withRequest(r.Context(), &Request{Language: test.plex}))
		if got := notificationLanguage(r); got != test.want {
			t.Errorf("%q/%q = %q want %q", test.plex, test.browser, got, test.want)
		}
	}
	n, ok := encodeNotification(model.NotificationContainer{Type: "activity", Size: 1, ActivityNotification: []model.ActivityNotification{{Event: "started", UUID: "x", Activity: &model.Activity{UUID: "x", Title: "Downloading subtitles"}}}})
	if !ok {
		t.Fatal("encode")
	}
	translated := localizeNotification(n, "fr-CA")
	var body struct {
		NotificationContainer struct {
			ActivityNotification []struct{ Activity struct{ Title string } }
		}
	}
	if json.Unmarshal(translated.data, &body) != nil || body.NotificationContainer.ActivityNotification[0].Activity.Title != "Téléchargement des sous-titres" {
		t.Fatal(string(translated.data))
	}
	if len(translated.sse) != 1 {
		t.Fatal("lost SSE entry")
	}
}

func TestActivitySocketLanguageIsBoundToHandshake(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	defer f.srv.Close()
	server := httptest.NewServer(f.srv)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, test := range []struct{ header, value, want string }{
		{"X-Plex-Language", "de", "Untertitel werden heruntergeladen"},
		{"Accept-Language", "ca,fr;q=0.8", "Téléchargement des sous-titres"},
	} {
		conn, response, err := websocket.Dial(ctx, strings.Replace(server.URL, "http:", "ws:", 1)+"/:/websockets/notifications?X-Plex-Token="+ownerToken+"&filters=activity", &websocket.DialOptions{HTTPHeader: http.Header{test.header: []string{test.value}}})
		if err != nil {
			t.Fatalf("dial: %v (%v)", err, response)
		}
		deadline := time.Now().Add(time.Second)
		for {
			f.srv.notifier.mu.Lock()
			ready := len(f.srv.notifier.subs) > 0
			f.srv.notifier.mu.Unlock()
			if ready {
				break
			}
			if time.Now().After(deadline) {
				conn.CloseNow()
				t.Fatal("socket subscription not ready")
			}
			time.Sleep(time.Millisecond)
		}
		f.srv.publishNotification(model.NotificationContainer{Type: "activity", Size: 1, ActivityNotification: []model.ActivityNotification{{Event: "started", UUID: "socket-task", Activity: &model.Activity{UUID: "socket-task", Type: "subtitle.download", UserID: ownerUser.ID, Title: "Downloading subtitles"}}}}, ownerUser.ID)
		_, data, err := conn.Read(ctx)
		conn.CloseNow()
		deadline = time.Now().Add(time.Second)
		for {
			f.srv.notifier.mu.Lock()
			reaped := len(f.srv.notifier.subs) == 0
			f.srv.notifier.mu.Unlock()
			if reaped {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("socket subscription not reaped")
			}
			time.Sleep(time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), test.want) {
			t.Fatalf("%s %s: %s", test.header, test.value, data)
		}
	}
}

func TestUnsupportedManagementRefusals(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	defer f.srv.Close()
	sub, _ := f.srv.notifier.subscribe(nil, &ownerUser)
	defer f.srv.notifier.unsubscribe(sub)
	for _, req := range []struct{ method, path string }{
		{"POST", "/butler"}, {"DELETE", "/butler"}, {"PUT", "/butler/OptimizeDatabase"}, {"DELETE", "/butler/OptimizeDatabase"},
		{"PUT", "/library/sections/1/refresh"}, {"POST", "/library/sections/1/refresh"},
		{"PUT", "/library/metadata/1/refresh"}, {"PUT", "/library/metadata/1/analyze"}, {"PUT", "/library/metadata/1/unmatch"},
	} {
		rec := f.request(t, req.method, req.path, nil)
		if rec.Code != 404 {
			t.Errorf("%s %s: %d", req.method, req.path, rec.Code)
		}
		if req.path == "/library/sections/1/refresh" {
			rec = f.request(t, req.method, req.path, map[string]string{"X-Plex-Token": friendToken})
			if rec.Code != 403 {
				t.Errorf("shared %s %s: %d", req.method, req.path, rec.Code)
			}
		}
	}
	select {
	case event := <-sub.ch:
		t.Fatalf("refusal published fake activity: %s", event.data)
	default:
	}
}
