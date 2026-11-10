package plex

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSubtitleDownloadActivityRegistryLifecycle(t *testing.T) {
	f, subs := newSubtitleFixture(t)
	subs.offers = []SubtitleOffer{{ID: 101, Language: "en"}}
	blocking := &blockingSubtitleDownload{subs, make(chan struct{}), make(chan struct{})}
	f.srv.setSubtitles(blocking)
	released := false
	defer func() {
		if !released {
			close(blocking.complete)
		}
	}()
	root := "/library/metadata/" + fightClub.Key.String() + "/subtitles"
	mc := decodeJSON(t, f.get(t, root+"?language=en", jsonAccept...))
	key := mc["Stream"].([]any)[0].(map[string]any)["key"].(string)
	rec := f.request(t, http.MethodPut, root+"?key="+url.QueryEscape(key), nil)
	id := rec.Header().Get("X-Plex-Activity")
	if rec.Code != 200 || id == "" {
		t.Fatalf("apply %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("download never started")
	}
	active := f.get(t, "/activities")
	if active.Code != 200 || !strings.Contains(active.Body.String(), id) || !strings.Contains(active.Body.String(), `userID="1"`) {
		t.Fatalf("started activity missing %d %s", active.Code, active.Body.String())
	}
	hidden := f.request(t, http.MethodGet, "/activities", map[string]string{"X-Plex-Token": friendToken})
	if hidden.Code != 200 || strings.Contains(hidden.Body.String(), id) {
		t.Fatalf("activity leaked %d %s", hidden.Code, hidden.Body.String())
	}
	close(blocking.complete)
	released = true
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := f.get(t, "/activities")
		if !strings.Contains(rows.Body.String(), id) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ended activity retained %s", rows.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sel := f.srv.selections.get(ownerUser.ID, int64(fightClub.Key)); sel.subtitleStream == nil {
		t.Fatal("completed track not published")
	}
}
