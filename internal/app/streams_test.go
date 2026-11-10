package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// The Plex front end's subtitle source lists, searches and picks tracks in
// the languages of the viewer's stream preferences, like playback
// enrichment does.
func TestPlexSubtitlesUsesStreamPreferenceLanguages(t *testing.T) {
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		lang := r.URL.Query().Get("languages")
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"attributes": map[string]any{
			"language": lang, "download_count": 10, "release": "Night.of.the.Living.Dead.1968.1080p",
			"files": []any{map[string]any{"file_id": 7, "file_name": "notld.srt"}},
		}}}})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"link":%q}`, srv.URL+"/file")
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nThey're coming to get you, Barbara.\n\n")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	prefs := trackprefs.New(store.OpenTest(t))
	rules := trackprefs.Default()
	rules.SubtitleLanguages = []string{"fr"}
	rules.SubtitleMode = "always"
	if err := prefs.PutRules(ctx, 0, rules); err != nil {
		t.Fatal(err)
	}
	a := &App{trackprefs: prefs, subtitles: subtitles.New(subtitles.Options{
		Config: config.OpenSubtitles{Enabled: true, APIKey: "k"}, CacheDir: t.TempDir(), BaseURL: srv.URL,
	})}
	p := subtitleSurface{app: a}
	title := &core.Title{Key: core.MovieKey(10331), Type: core.TypeMovie, IDs: core.IDs{TMDB: 10331}}
	viewer := &core.User{ID: 3, SubtitleLanguages: []string{"en"}}

	offers, err := p.SearchSubtitles(ctx, title, viewer, "fre", "Night.of.the.Living.Dead.1968.1080p")
	if err != nil || len(offers) != 1 || offers[0].ID != 7 || offers[0].Language != "fr" || offers[0].Provider != "OpenSubtitles" ||
		!offers[0].PerfectMatch || offers[0].Title != "Night.of.the.Living.Dead.1968.1080p" {
		t.Fatalf("search: %+v %v", offers, err)
	}
	st, err := p.DownloadSubtitle(ctx, title, viewer, 7)
	if err != nil || st.Language != "fr" {
		t.Fatalf("pick: %+v %v", st, err)
	}

	// A preference does not download a sidecar on its own.
	other := &core.Title{Key: core.MovieKey(10332), Type: core.TypeMovie, IDs: core.IDs{TMDB: 10332}}
	if _, err := a.subtitles.SidecarsForRelease(ctx, other, a.subtitleUser(ctx, viewer), nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := p.CachedSidecars(ctx, other, viewer, nil); len(got) != 0 {
		t.Fatalf("preferences downloaded: %+v", got)
	}
	rc, format, err := p.OpenFormat(st.ExternalURL, "vtt")
	if err != nil || format != "vtt" {
		t.Fatalf("open: %v %s", err, format)
	}
	rc.Close()
}
