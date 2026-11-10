package plex

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

func TestPartSelectionResetClearsOnlyRequestedPersistentChoice(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	prefs := trackprefs.New(store.OpenTest(t))
	f.srv.setTrackPreferences(prefs)
	ctx := context.Background()
	rules := trackprefs.Default()
	rules.PreferOriginal = false
	rules.AudioLanguages = []string{"en"}
	rules.SubtitleLanguages = []string{"en"}
	rules.SubtitleMode = "always"
	if err := prefs.PutRules(ctx, ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}
	key := fightClub.Key
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "audio", Index: 0, Language: "eng", Codec: "aac"},
		{Type: "audio", Index: 1, Language: "fra", Codec: "aac"},
		{Type: "subtitle", Index: 2, Language: "eng", Codec: "srt"},
		{Type: "subtitle", Index: 3, Language: "fra", Codec: "srt"},
	}}
	audio := f.bindStream(key, 1, info.Streams[1])
	sub := f.bindStream(key, 3, info.Streams[3])
	request := func(q string) {
		t.Helper()
		rec := f.request(t, http.MethodPut, "/library/parts/"+key.String()+"?"+q, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	check := func(audio, sub int) {
		t.Helper()
		got, err := prefs.Select(ctx, ownerUser.ID, key, info, "en")
		if err != nil || got.Audio != audio || got.Subtitle != sub {
			t.Fatalf("got %+v %v, want %d/%d", got, err, audio, sub)
		}
	}
	request(fmt.Sprintf("audioStreamID=%d&subtitleStreamID=%d", audio, sub))
	check(1, 3)
	request("audioStreamID=-1")
	check(0, 3)
	request("subtitleStreamID=-1")
	check(0, 2)
	request("subtitleStreamID=not-a-number")
	check(0, -1)
	request("subtitleStreamID=-1")
	check(0, 2)
}
