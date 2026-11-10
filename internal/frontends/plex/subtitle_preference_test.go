package plex

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

func TestAccountSubtitleModeStaysManualUntilChanged(t *testing.T) {
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	user := core.User{ID: 7, Username: "ada", SubtitleLanguages: []string{"de"}}
	plain := testServer(Deps{})
	plain.setTestAuth(&fakeAuth{users: map[string]core.User{"t": user}})
	withPrefs := testServer(Deps{TrackPreferences: prefs})
	withPrefs.setTestAuth(plain.testAuth())
	body := func(s *Server) string {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleAccounts(w, httptest.NewRequest("GET", "/accounts", nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	untouched := body(plain)
	inherited := body(withPrefs)
	if untouched != inherited || !strings.Contains(inherited, `subtitleMode="0"`) || !strings.Contains(inherited, `defaultSubtitleLanguage="de"`) {
		t.Fatalf("unchanged account changed\nplain:\n%s\nprefs:\n%s", untouched, inherited)
	}
	shared := trackprefs.Default()
	shared.SubtitleMode = "always"
	shared.SubtitleLanguages = []string{"ja"}
	if err := prefs.PutRules(context.Background(), 0, shared); err != nil {
		t.Fatal(err)
	}
	if again := body(withPrefs); again != untouched {
		t.Fatalf("shared default leaked into an untouched account\n%s", again)
	}
	rules := trackprefs.Default()
	rules.SubtitleMode = "always"
	rules.SubtitleLanguages = []string{"en", "fr"}
	if err := prefs.PutRules(context.Background(), user.ID, rules); err != nil {
		t.Fatal(err)
	}
	changed := body(withPrefs)
	if !strings.Contains(changed, `subtitleMode="2"`) || !strings.Contains(changed, `defaultSubtitleLanguage="en"`) {
		t.Fatal(changed)
	}
	rules.SubtitleMode = "foreign-only"
	if err := prefs.PutRules(context.Background(), user.ID, rules); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(withPrefs), `subtitleMode="1"`) {
		t.Fatal(body(withPrefs))
	}
}

func TestPlexSelectionHonoursSubtitleMode(t *testing.T) {
	db := store.OpenTest(t)
	prefs := trackprefs.New(db)
	rules := trackprefs.Default()
	rules.AudioLanguages = []string{"ja"}
	rules.SubtitleLanguages = []string{"en"}
	rules.SubtitleMode = "foreign-only"
	if err := prefs.PutRules(context.Background(), 7, rules); err != nil {
		t.Fatal(err)
	}
	srv := &Server{d: testDeps(Deps{TrackPreferences: prefs}), log: slog.Default()}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "ja"}, {Type: "audio", Language: "en"}, {Type: "subtitle", Language: "en", Forced: true}, {Type: "subtitle", Language: "en"}}}
	got := srv.preferredSelection(context.Background(), &core.User{ID: 7}, core.MovieKey(1), info, "ja", selection{})
	if !got.subtitleSet || got.subtitle != 3 || got.audio != 0 {
		t.Fatalf("%+v", got)
	}
	rules.SubtitleMode = "off"
	if err := prefs.PutRules(context.Background(), 7, rules); err != nil {
		t.Fatal(err)
	}
	got = srv.preferredSelection(context.Background(), &core.User{ID: 7}, core.MovieKey(1), info, "ja", selection{})
	if !got.subtitleSet || got.subtitle != -1 {
		t.Fatalf("off %+v", got)
	}
}
