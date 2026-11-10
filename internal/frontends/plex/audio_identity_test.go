package plex

import (
	"context"
	"encoding/xml"
	"fmt"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func audioIDFromResponse(t *testing.T, body, language string) int64 {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(body))
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		el, ok := tok.(xml.StartElement)
		if !ok || el.Name.Local != "Stream" {
			continue
		}
		var st struct {
			ID       int64  `xml:"id,attr"`
			Type     int    `xml:"streamType,attr"`
			Language string `xml:"languageCode,attr"`
		}
		if err := d.DecodeElement(&st, &el); err != nil {
			t.Fatal(err)
		}
		if st.Type == 2 && st.Language == language {
			return st.ID
		}
	}
	t.Fatalf("no %s audio in %s", language, body)
	return 0
}

func TestPreplayAudioIdentitySurvivesProbe(t *testing.T) {
	for _, viaQuery := range []bool{false, true} {
		for _, renderBeforeSelect := range []bool{false, true} {
			t.Run(fmt.Sprintf("query=%v/probed-render-first=%v", viaQuery, renderBeforeSelect), func(t *testing.T) {
				f := newFixture()
				defer f.srv.Close()
				key := fightClub.Key
				estimate := sampleInfo
				estimate.Streams = []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Language: "eng", Codec: "ac3", Channels: 6, Default: true}}
				f.playback.info = estimate
				englishID := audioIDFromResponse(t, f.get(t, "/library/metadata/"+key.String()).Body.String(), "eng")
				actual := estimate
				actual.Streams = []core.StreamInfo{estimate.Streams[0], {Index: 1, Type: "audio", Language: "fre", Codec: "ac3", Channels: 6, Default: true}, {Index: 2, Type: "audio", Language: "eng", Codec: "ac3", Channels: 6}}
				f.playback.info = actual
				if renderBeforeSelect {
					f.srv.buildStreams(key, actual, selection{}, &ownerUser)
				}
				extra := ""
				if viaQuery {
					extra = fmt.Sprintf("&audioStreamID=%d", englishID)
				} else {
					rec := f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/%s?audioStreamID=%d", key, englishID), nil)
					if rec.Code != 200 {
						t.Fatalf("select: %d", rec.Code)
					}
				}
				for attempt := 0; attempt < 2; attempt++ {
					rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&session=audio-%d%s", key, attempt, extra))
					if rec.Code != 200 || f.trans.lastOpts.AudioStream != 2 {
						t.Fatalf("attempt %d chose stream %d: %s", attempt, f.trans.lastOpts.AudioStream, rec.Body.String())
					}
					for _, ps := range f.playback.Sessions() {
						f.playback.Stop(ps.ID)
					}
				}
				// A deliberate French selection from the actual stream list still wins.
				french := f.bindStream(key, 1, actual.Streams[1])
				f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/%s?audioStreamID=%d", key, french), nil)
				f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&session=french", key))
				if f.trans.lastOpts.AudioStream != 1 {
					t.Fatalf("explicit French lost: %+v", f.trans.lastOpts)
				}
			})
		}
	}
}

func TestMissingSelectedAudioUsesViewerPreferences(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	prefs := trackprefs.New(store.OpenTest(t))
	rules := trackprefs.Default()
	rules.AudioLanguages = []string{"de"}
	rules.PreferOriginal = false
	if err := prefs.PutRules(context.Background(), ownerUser.ID, rules); err != nil {
		t.Fatal(err)
	}
	f.srv.d.TrackPreferences = prefs
	key := fightClub.Key
	f.playback.info.Streams = []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Language: "eng", Codec: "ac3", Channels: 6}}
	english := audioIDFromResponse(t, f.get(t, "/library/metadata/"+key.String()).Body.String(), "eng")
	f.request(t, http.MethodPut, fmt.Sprintf("/library/parts/%s?audioStreamID=%d", key, english), nil)
	f.playback.info.Streams = []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Language: "fre", Codec: "ac3", Default: true}, {Index: 7, Type: "audio", Language: "deu", Codec: "aac"}}
	f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&session=missing", key))
	if f.trans.lastOpts.AudioStream != 7 {
		t.Fatalf("missing English suppressed German preference: %+v", f.trans.lastOpts)
	}
	if s := f.srv.selections.get(friendUser.ID, int64(key)); s.audioSet {
		t.Fatal("selection leaked to another viewer")
	}
	if s := f.srv.selections.get(ownerUser.ID, int64(alien.Key)); s.audioSet {
		t.Fatal("selection leaked to another title")
	}
}

func TestStreamIDsKeepIssuedAudioAndSubtitleIdentity(t *testing.T) {
	r := sessions.NewStreamTable()
	for _, kind := range []string{"audio", "subtitle"} {
		en := core.StreamInfo{Index: 1, Type: kind, Language: "eng", Codec: "ac3", Channels: 6}
		fr := en
		fr.Language = "fre"
		a := bindTable(t, r, fightClub.Key, 1, en)
		b := bindTable(t, r, fightClub.Key, 1, fr)
		if a == b {
			t.Fatalf("%s identity overwritten", kind)
		}
		old, ok := r.Lookup(a)
		if !ok || old.Info.Language != "eng" {
			t.Fatalf("old %s reference lost", kind)
		}
		if again := bindTable(t, r, fightClub.Key, 1, en); again != a {
			t.Fatal("repeated identity changed ID")
		}
	}
}

func bindTable(t *testing.T, r *sessions.StreamTable, key core.RatingKey, index int, info core.StreamInfo) int64 {
	t.Helper()
	id, err := r.Bind(sessions.StreamBinding{Key: key, Index: index, Info: info})
	if err != nil || id == 0 {
		t.Fatalf("bind %d %v", id, err)
	}
	return id
}

func TestSubtitleIdentityIgnoresPosition(t *testing.T) {
	r := sessions.NewStreamTable()
	es := core.StreamInfo{Type: "subtitle", Language: "es", Codec: "srt"}
	first := bindTable(t, r, fightClub.Key, 3, es)
	moved := es
	moved.Index = 9
	if got := bindTable(t, r, fightClub.Key, 9, moved); got != first {
		t.Fatalf("subtitle id changed with position: %d vs %d", got, first)
	}
	pgs := es
	pgs.Codec = "hdmv_pgs_subtitle"
	if got := bindTable(t, r, fightClub.Key, 3, pgs); got == first {
		t.Fatal("different codec reused the Spanish SRT id")
	}
}

func TestAudioIdentityPrefersExplicitRegionBeforeCodec(t *testing.T) {
	want := core.StreamInfo{Type: "audio", Language: "pt-BR", Codec: "ac3", Channels: 6}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "audio", Language: "por", Codec: "ac3", Channels: 6},
		{Type: "audio", Language: "pt-BR", Codec: "aac", Channels: 2},
		{Type: "audio", Language: "pt-PT", Codec: "ac3", Channels: 6},
	}}
	if got := resolveAudio(&want, 0, info); got != 1 {
		t.Fatalf("explicit Brazilian track lost to position %d", got)
	}
}

func TestStreamIdentityIDsSurviveRegistrationOrderChanges(t *testing.T) {
	for _, kind := range []string{"audio", "subtitle"} {
		en := core.StreamInfo{Type: kind, Language: "eng", Codec: "aac", Channels: 2}
		fr := en
		fr.Language = "fre"
		before := sessions.NewStreamTable()
		englishID := bindTable(t, before, fightClub.Key, 1, en)
		// A restart or table wipe must not let a cached English ID select
		// French merely because French is now the first render at that position.
		after := sessions.NewStreamTable()
		frenchID := bindTable(t, after, fightClub.Key, 1, fr)
		if frenchID == englishID {
			t.Fatalf("%s cached English ID aliases French after reset", kind)
		}
		if got := bindTable(t, after, fightClub.Key, 1, en); got != englishID {
			t.Fatalf("%s English ID changed with registration order", kind)
		}
	}
}
