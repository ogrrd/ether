package plex

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
)

type supportDeniedPrepare struct{ core.Playback }

func (p supportDeniedPrepare) Prepare(context.Context, *core.User, core.RatingKey, core.ClientCaps) (*core.PlaySession, error) {
	return nil, core.ErrNotFound
}

func supportZIPEvents(t *testing.T, rec *diagnostics.Recorder, id string) []map[string]any {
	t.Helper()
	if _, err := rec.Finish("owner", id, diagnostics.FinishIn{}); err != nil {
		t.Fatal(err)
	}
	blob, err := rec.Archive("owner", id)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, file := range z.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{ownerToken, friendToken, "super-secret-cookie", "private-extension-secret"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("%s leaks credential", file.Name)
			}
		}
		if file.Name == "events.jsonl" {
			for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
				var row map[string]any
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				events = append(events, row)
			}
		}
	}
	return events
}

// The wire request, authenticated viewer scope, actual admission caps and final
// ZIP must agree, including a client that skips decision/start and reads a Part.
func TestSupportPlaybackCaptureIdentityAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, platform, extra string
		want                  float64
	}{
		{"identified", "iOS", "", 2160},
		{"thin-media", "", "", 1080},
		{"explicit-limit", "iOS", "add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.height&value=720)", 720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := diagnostics.New(diagnostics.Deps{Now: func() time.Time { return fixedNow }, Secrets: func() []string { return []string{ownerToken, friendToken} }})
			f := newFixture(func(d *testServerDeps) { d.Support = rec })
			defer f.srv.Close()
			f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", "NowSeen", "X-Plex-Platform", "iOS")
			capture, err := rec.Start("owner", diagnostics.StartIn{DeviceID: rec.Devices()[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			// A failed preparation still needs its decision inputs, before any result trace.
			f.srv.d.Playback = supportDeniedPrepare{f.playback}
			headers := map[string]string{"X-Plex-Client-Identifier": "", "Range": "bytes=2-8", "Cookie": "super-secret-cookie", "X-Unknown": "private-extension-secret"}
			if tc.platform != "" {
				headers["X-Plex-Product"] = "NowSeen"
				headers["X-Plex-Platform"] = tc.platform
				headers["X-Plex-Device"] = "iPhone"
			}
			headers["X-Plex-Client-Profile-Extra"] = tc.extra
			path := "/library/parts/" + fightClub.Key.String() + "/0/file.mkv?private-extension-secret=private-extension-secret"
			f.request(t, "GET", path, headers)
			// Same route and status from another viewer/device must not enter this ZIP.
			headers["X-Plex-Token"] = friendToken
			f.request(t, "GET", path, headers)
			headers["X-Plex-Token"] = ownerToken
			headers["X-Plex-Client-Identifier"] = "different-device"
			f.request(t, "GET", path, headers)
			rows := supportZIPEvents(t, rec, capture.ID)
			started, admitted, ended := 0, 0, 0
			var attempt any
			for _, row := range rows {
				switch row["kind"] {
				case "request.started":
					started++
					attempt = row["attempt"]
					if row["status"] != nil || row["uncorrelated"] != true || row["device"] != nil || row["range"] != "bytes=2-8" {
						t.Fatalf("bad start: %+v", row)
					}
				case "client.capabilities":
					admitted++
					c := row["client"].(map[string]any)
					if c["stage"] != "admission" || c["maxHeight"] != tc.want {
						t.Fatalf("bad admission: %+v", row)
					}
					if row["attempt"] != attempt {
						t.Fatal("lost request correlation")
					}
				case "request":
					if strings.HasPrefix(row["route"].(string), "/library/parts/") {
						ended++
						if row["attempt"] != attempt {
							t.Fatal("lost completion correlation")
						}
					}
				}
			}
			if started != 1 || admitted != 1 || ended != 1 {
				t.Fatalf("start/admission/end %d/%d/%d", started, admitted, ended)
			}
		})
	}
}

func TestSupportPlaybackCaptureBeforeLongResponseCompletes(t *testing.T) {
	rec := diagnostics.New(diagnostics.Deps{Now: func() time.Time { return fixedNow }})
	f := newFixture(func(d *testServerDeps) { d.Support = rec })
	defer f.srv.Close()
	f.get(t, "/identity")
	// The selected identity needs an authenticated sighting.
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	capture, err := rec.Start("owner", diagnostics.StartIn{DeviceID: rec.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := f.srv.parse(f.srv.requestLog(f.srv.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("media"))
	}))))
	req := httptest.NewRequest("GET", "/library/parts/1/0/file.mp4", nil)
	req.Header.Set("X-Plex-Token", ownerToken)
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), req) }()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not enter")
	}
	rows := supportZIPEvents(t, rec, capture.ID)
	found := false
	for _, row := range rows {
		if row["kind"] == "request.started" {
			found = true
			if row["status"] != nil {
				t.Fatal("invented response status")
			}
		}
		if row["kind"] == "media.bytes" {
			t.Fatal("invented bytes before completion")
		}
	}
	if !found {
		t.Fatal("in-flight request absent from ZIP")
	}
}
