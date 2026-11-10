package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// base is a field, not an embedding: embedding frontendtest.Sessions would
// hide its Sessions method behind a field of the same name.
type tileSessions struct {
	base  frontendtest.Sessions
	offer bool
}

func (s tileSessions) Markers(ctx context.Context, a frontend.SessionAccess, id string) (frontend.SessionMarkers, error) {
	return s.base.Markers(ctx, a, id)
}

func (t tileSessions) Plan(c context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	return t.base.Plan(c, r)
}
func (t tileSessions) Open(c context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	return t.base.Open(c, r)
}
func (t tileSessions) StartStream(c context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	return t.base.StartStream(c, a, id, r)
}
func (t tileSessions) SubtitleURL(c context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	return t.base.SubtitleURL(c, a, id, index, format)
}
func (t tileSessions) ServeMedia(w http.ResponseWriter, r *http.Request) { t.base.ServeMedia(w, r) }
func (t tileSessions) Report(c context.Context, a frontend.SessionAccess, id string, line frontend.Timeline) error {
	return t.base.Report(c, a, id, line)
}
func (t tileSessions) Stop(c context.Context, a frontend.SessionAccess, id string) error {
	return t.base.Stop(c, a, id)
}
func (t tileSessions) Command(c context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	return t.base.Command(c, actor, id, cmd)
}
func (t tileSessions) UpdateCapabilities(c context.Context, a frontend.SessionAccess, v frontend.SessionCapabilities) error {
	return t.base.UpdateCapabilities(c, a, v)
}
func (t tileSessions) Capabilities(c context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	return t.base.Capabilities(c, a)
}
func (t tileSessions) Sessions(c context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	return t.base.Sessions(c, a)
}
func (t tileSessions) Recover(c context.Context, id, url string, at time.Duration) error {
	return t.base.Recover(c, id, url, at)
}
func (t tileSessions) TranscodeFor(c context.Context, id string) (core.HLSSession, bool) {
	return t.base.TranscodeFor(c, id)
}
func (t tileSessions) Events(c context.Context, s frontend.EventScope) (<-chan frontend.Event, error) {
	return t.base.Events(c, s)
}
func (t tileSessions) TrickplayAvailable(id string) bool     { return t.base.TrickplayAvailable(id) }
func (t tileSessions) TrickplayBIF(id string) ([]byte, bool) { return t.base.TrickplayBIF(id) }
func (t tileSessions) TrickplayFrame(id string, at time.Duration) ([]byte, bool) {
	return t.base.TrickplayFrame(id, at)
}
func (t tileSessions) CancelTrickplay(id string) { t.base.CancelTrickplay(id) }

func (t tileSessions) TrickplayOffer(context.Context, frontend.SessionAccess, core.RatingKey) (frontend.TrickplayOffer, bool) {
	if !t.offer {
		return frontend.TrickplayOffer{}, false
	}
	return frontend.TrickplayOffer{Width: 320, Height: 180, Columns: 10, Rows: 10, Count: 4, Interval: 10e9, Bandwidth: 8000}, true
}

func (tileSessions) TrickplaySheet(context.Context, frontend.SessionAccess, core.RatingKey, int) ([]byte, error) {
	return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
}

func TestJellyfinTrickplayRouteUsesBorrowedTiles(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	key := core.RatingKey(5)
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie", Title: "Buck Bunny"}, Tracks: []frontend.StreamInfo{{Type: "video", Width: 3840, Height: 2160}}}, nil
		}},
		Sessions: tileSessions{offer: true},
	}
	inst, err := mount(context.Background(), host, "test", "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(key)
	detail := httptest.NewRequest(http.MethodGet, "/Items/"+id, nil)
	detail.Header.Set("X-Emby-Token", "grant")
	dw := httptest.NewRecorder()
	inst.Handler().ServeHTTP(dw, detail)
	if dw.Code != 200 || !strings.Contains(dw.Body.String(), `"Trickplay":{"320":`) || !strings.Contains(dw.Body.String(), `"TileWidth":10`) || !strings.Contains(dw.Body.String(), `"Interval":10000`) {
		t.Fatal(dw.Code, dw.Body.String())
	}
	// The combined browse shell must preserve the playback dimensions while
	// the final attachment replaces the detail DTO's empty trickplay shell.
	for _, field := range []string{`"Width":3840`, `"Height":2160`, `"IsHD":true`, `"ChannelId":null`} {
		if !strings.Contains(dw.Body.String(), field) {
			t.Fatalf("combined detail missing %s: %s", field, dw.Body.String())
		}
	}
	sheet := httptest.NewRequest(http.MethodGet, "/Videos/"+id+"/Trickplay/320/0.jpg", nil)
	sheet.Header.Set("X-Emby-Token", "grant")
	sw := httptest.NewRecorder()
	inst.Handler().ServeHTTP(sw, sheet)
	if sw.Code != 200 || sw.Header().Get("Content-Type") != "image/jpeg" || sw.Body.Len() == 0 {
		t.Fatal(sw.Code, sw.Header(), sw.Body.Bytes())
	}
	wrong := httptest.NewRequest(http.MethodGet, "/Videos/"+id+"/Trickplay/160/0.jpg", nil)
	wrong.Header.Set("X-Emby-Token", "grant")
	ww := httptest.NewRecorder()
	inst.Handler().ServeHTTP(ww, wrong)
	if ww.Code != 404 {
		t.Fatal(ww.Code)
	}
}
