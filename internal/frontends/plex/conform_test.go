package plex

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

// The reference fixtures in testdata/conform/plex were recorded from a real PMS
// 1.43.4 playing "The 'Burbs" (HEVC Main 10 + DTS stereo + PGS in MKV).
// These tests give Ether the same title and media and diff its decision
// responses against PMS's with pmsconform: field names, JSON types,
// child order and XML attribute order must match; only data Ether does not
// have (Common Sense Media, critic ratings, extras, deep analysis) may be
// missing.

var burbs = core.Title{
	Key: core.MovieKey(11974), Type: core.TypeMovie, IDs: core.IDs{TMDB: 11974, IMDB: "tt0096734"},
	Title: "The 'Burbs", Summary: "He's a man of peace in a savage land: Suburbia.", Tagline: "He's a man of peace in a savage land... Suburbia.",
	Year: 1989, Released: time.Date(1989, 2, 17, 0, 0, 0, 0, time.UTC), Runtime: 101 * time.Minute,
	Genres: []string{"Comedy", "Thriller", "Mystery", "Horror"}, Studios: []string{"Universal Pictures"}, Countries: []string{"United States of America"},
	ContentRating: "PG", Rating: 6.8, PosterPath: "/burbs.jpg", BackdropPath: "/burbsart.jpg", LogoPath: "/burbslogo.png",
	Cast: []core.Person{
		{TMDB: 31, Name: "Tom Hanks", Role: "Ray Peterson", ThumbPath: "/hanks.jpg"},
		{TMDB: 1150, Name: "Bruce Dern", Role: "Lt. Mark Rumsfield", ThumbPath: "/dern.jpg"},
	},
	Directors:    []core.Person{{TMDB: 11218, Name: "Joe Dante", ThumbPath: "/dante.jpg"}},
	Writers:      []core.Person{{TMDB: 70939, Name: "Dana Olsen"}},
	CollectionID: 9999, CollectionName: "Suburbia",
	UpdatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
}

var burbsInfo = core.MediaInfo{
	Container: "mkv", Duration: 6086390 * time.Millisecond, Size: 4289930164, BitrateKbps: 5639,
	Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "main 10", Language: "en", Width: 1920, Height: 1036, FrameRate: 23.976, BitDepth: 10, BitrateKbps: 4103, ColorTransfer: "bt709"},
		{Index: 1, Type: "audio", Codec: "dts", Profile: "dts", Language: "en", Title: "Stereo", Channels: 2, ChannelLayout: "stereo", SampleRate: 48000, Default: true, BitrateKbps: 1536},
		{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "en"},
	},
}

// pmsOnlyRE are paths of data PMS has and Ether doesn't: Common Sense
// Media, critic ratings, extras, locked fields, labels, producers and
// similar-title tags, deep analysis, preview thumbnails and probe details
// that the virtual media can't know before a release is opened.
var pmsOnlyRE = regexp.MustCompile(`(@|/)(chapterSource|contentRatingAge|primaryExtraKey|rating|ratingImage|CommonSenseMedia|Field|Label|Producer|Similar|deepAnalysisVersion|indexes|requiredBandwidths|bitrateMode|chromaLocation|codedHeight|codedWidth|colorPrimaries|colorRange|colorSpace|level|refFrames|titleSort)$`)

// etherOnlyRE are paths Ether legitimately adds: TMDB has writer photos,
// and Ether lists the TMDB audience rating on episodes too (PMS does when
// it has one, e.g. IMDb's).
var etherOnlyRE = regexp.MustCompile(`(Writer(\[\])?/@?thumb|Video/Rating|Metadata\[\]/Rating)$`)

func pmsFixtureName(profile, path string, jsonMode bool) string {
	return pmsconform.FixtureName(profile, path, jsonMode) + ".json"
}

func TestDecisionConformsToPMS(t *testing.T) {
	type mode struct {
		name     string
		template string // corpus path
		decision core.Decision
		codes    map[string]string // container attributes that must equal PMS's
	}
	transcode := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "mp3", Container: "mpegts", BitrateKbps: 9079}
	modes := []mode{
		{
			name:     "legacy",
			template: "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=1&directStream=1&videoQuality=8&X-Plex-Client-Profile-Extra=",
			decision: transcode,
			codes:    map[string]string{"generalDecisionCode": "", "generalDecisionText": "", "directPlayDecisionCode": "", "transcodeDecisionCode": "", "transcodeDecisionText": ""},
		},
		{
			name:     "mde-direct",
			template: "/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=1&directStream=1&videoQuality=8",
			decision: transcode, // ignored: the client decides
			codes:    map[string]string{"mdeDecisionCode": "", "mdeDecisionText": "", "generalDecisionCode": "", "transcodeDecisionCode": ""},
		},
		{
			name:     "mde-stream",
			template: "/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=0&directStream=1&subtitleSize=100&audioBoost=100&location=lan",
			decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "mp3", Container: "mpegts", BitrateKbps: 20256},
			codes:    map[string]string{"generalDecisionCode": "", "directPlayDecisionCode": "", "directPlayDecisionText": "", "transcodeDecisionCode": "", "mdeDecisionCode": ""},
		},
	}
	clients := conformplex.Clients()
	for _, md := range modes {
		for _, profile := range []string{"web", "ios", "appletv"} {
			for _, jsonMode := range []bool{false, true} {
				name := md.name + "/" + profile + "/" + map[bool]string{false: "xml", true: "json"}[jsonMode]
				t.Run(name, func(t *testing.T) {
					ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "plex", profile, pmsFixtureName(profile, md.template, jsonMode)))
					if err != nil {
						t.Fatalf("fixture: %v", err)
					}
					f := newFixture()
					defer f.srv.Close()
					f.catalog.titles[burbs.Key] = burbs
					f.playback.info = burbsInfo
					f.trans.decision = md.decision

					hdr := map[string]string{}
					for k, v := range clients[profile].Headers {
						if k != "X-Plex-Token" {
							hdr[k] = v[0]
						}
					}
					hdr["Accept"] = "application/xml"
					if jsonMode {
						hdr["Accept"] = "application/json"
					}
					target := strings.ReplaceAll(md.template, "{{movieKey}}", burbs.Key.String())
					f.request(t, http.MethodGet, target, hdr)
					f.srv.waitBackground() // UltraBlurColors
					rec := f.request(t, http.MethodGet, target, hdr)
					ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}

					for _, d := range conformplex.Compare(ref, ours).Differences {
						switch {
						case d.Kind == pmsconform.KindMissing && pmsOnlyRE.MatchString(d.Path):
						case d.Kind == pmsconform.KindExtra && etherOnlyRE.MatchString(d.Path):
						case d.Kind == pmsconform.KindValue && artworkTypeGapRE.MatchString(d.Path):
						case d.Kind == pmsconform.KindValue && !strings.HasSuffix(d.Path, "#order") && !stableMediaValue.MatchString(d.Path):
						default:
							t.Errorf("%s %s ref=%v ours=%v", d.Kind, d.Path, d.Reference, d.Target)
						}
					}
					refAttrs, ourAttrs := containerAttrs(t, ref.Body), containerAttrs(t, ours.Body)
					for k := range md.codes {
						if refAttrs[k] != ourAttrs[k] {
							t.Errorf("container %s = %q, PMS %q", k, ourAttrs[k], refAttrs[k])
						}
					}
					if !jsonMode {
						checkAttrOrder(t, ref.Body, ours.Body)
					}
				})
			}
		}
	}
}

// Stable media semantics must agree as well as the response shape. IDs,
// artwork URLs and text summaries legitimately differ between catalogues.
// artworkTypeGapRE is a data gap the value-set comparison exposes: PMS
// lists a critic rating and backgroundSquare art Ether's metadata does not
// carry, so the set of Rating and Image types differs while each type Ether
// does send matches PMS.
var artworkTypeGapRE = regexp.MustCompile(`/(Rating|Image)(\[\])?/@?type$`)

var stableMediaValue = regexp.MustCompile(`(@|/)(type|year|videoCodec|audioCodec|container|width|height|bitDepth|streamType|channels|sampleRate|protocol)$`)

// detailPMSOnlyRE adds what a PMS detail page has beyond the decision's
// data gaps: chapters, extras (trailers), the scanner's stream details and
// PMS's stream titles, audio profiles and BIF indexes.
var detailPMSOnlyRE = regexp.MustCompile(`(@|/)(Chapter|Extras/Video|Extras/Metadata|canAutoSync|hasScalingMatrix|scanType|audioProfile|loudnessAnalysisVersion|skipCount|viewCount|lastViewedAt|viewOffset|hasThumbnail|has64bitOffsets|optimizedForStreaming|librarySectionKey|librarySectionID|librarySectionTitle)$`)

// TestDetailConformsToPMS renders the detail page PMS recorded for the
// same title (with a related hub) and requires PMS's structure and types.
func TestDetailConformsToPMS(t *testing.T) {
	template := "/library/metadata/{{movieKey}}?includeExtras=1&includeRelated=1&includeOnDeck=1&includeChapters=1&includeGuids=1"
	for _, profile := range []string{"web", "ios", "appletv"} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(profile+"/"+map[bool]string{false: "xml", true: "json"}[jsonMode], func(t *testing.T) {
				ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "plex", profile, pmsFixtureName(profile, template, jsonMode)))
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				f := newFixture()
				defer f.srv.Close()
				f.catalog.titles[burbs.Key] = burbs
				f.playback.info = burbsInfo
				related := fightClub
				related.CollectionID, related.CollectionName = 1, "Fincher"
				f.srv.recommender().(*fakeRecommender).related = []core.Hub{{ID: "movie.similar", Title: "More Like This", Type: core.TypeMovie, Context: "hub.movie.similar", Items: []core.Title{related}}}
				hdr := []string{"Accept", "application/xml"}
				if jsonMode {
					hdr = jsonAccept
				}
				rec := f.getWarm(t, strings.ReplaceAll(template, "{{movieKey}}", burbs.Key.String()), hdr...)
				ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}
				for _, d := range conformplex.Compare(ref, ours).Differences {
					switch {
					case d.Kind == pmsconform.KindMissing && (pmsOnlyRE.MatchString(d.Path) || detailPMSOnlyRE.MatchString(d.Path)):
					case d.Kind == pmsconform.KindExtra && etherOnlyRE.MatchString(d.Path):
					case d.Kind == pmsconform.KindValue && artworkTypeGapRE.MatchString(d.Path):
					case d.Kind == pmsconform.KindValue && !strings.HasSuffix(d.Path, "#order") && !stableMediaValue.MatchString(d.Path):
					default:
						t.Errorf("%s %s ref=%v ours=%v", d.Kind, d.Path, d.Reference, d.Target)
					}
				}
				if !jsonMode {
					checkAttrOrder(t, ref.Body, ours.Body)
				}
			})
		}
	}
}

// The TV fixtures are "9-1-1" season 1, episode 1.
var (
	nineShow = core.Title{
		Key: core.ShowKey(75219), Type: core.TypeShow, IDs: core.IDs{TMDB: 75219, IMDB: "tt7235466", TVDB: 337349},
		Title: "9-1-1", Summary: "Explores the high-pressure experiences of first responders.", Year: 2018,
		Released: time.Date(2018, 1, 3, 0, 0, 0, 0, time.UTC), Runtime: 43 * time.Minute, ChildCount: 1, LeafCount: 1,
		Genres: []string{"Drama", "Action & Adventure"}, Studios: []string{"Ryan Murphy Television"}, ContentRating: "TV-14", Rating: 7.6,
		PosterPath: "/911.jpg", BackdropPath: "/911art.jpg", LogoPath: "/911logo.png",
		Cast:      []core.Person{{TMDB: 1, Name: "Angela Bassett", Role: "Athena Grant", ThumbPath: "/ab.jpg"}},
		UpdatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	nineS1 = core.Title{
		Key: core.SeasonKey(75219, 1), Type: core.TypeSeason, ShowKey: nineShow.Key, ShowTitle: "9-1-1", Season: 1, Title: "Season 1",
		AddedAt:    time.Date(2018, 1, 3, 0, 0, 0, 0, time.UTC),
		ChildCount: 1, PosterPath: "/911s1.jpg", UpdatedAt: nineShow.UpdatedAt,
	}
	nineE1 = core.Title{
		Key: core.EpisodeKey(75219, 1, 1), Type: core.TypeEpisode, ShowKey: nineShow.Key, ShowTitle: "9-1-1", Season: 1, Episode: 1,
		Title: "Pilot", Summary: "It takes a team of people to help answer the famous question.", Runtime: 46 * time.Minute, Rating: 7.5,
		Released: time.Date(2018, 1, 3, 0, 0, 0, 0, time.UTC), StillPath: "/pilot911.jpg",
		IDs:       core.IDs{TMDB: 1428394, IMDB: "tt7235642", TVDB: 6407446},
		Directors: []core.Person{{TMDB: 2, Name: "Bradley Buecker", ThumbPath: "/bb.jpg"}},
		Writers:   []core.Person{{TMDB: 3, Name: "Brad Falchuk", ThumbPath: "/bf.jpg"}},
		Cast:      []core.Person{{TMDB: 4, Name: "Alyvia Alyn Lind", Role: "Young Christina", ThumbPath: "/aal.jpg"}},
		UpdatedAt: nineShow.UpdatedAt,
	}
	nineInfo = core.MediaInfo{
		Container: "mkv", Duration: 2782989 * time.Millisecond, Size: 4052046285, BitrateKbps: 11648,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "high", Language: "en", Width: 1920, Height: 1080, FrameRate: 23.976, BitDepth: 8, BitrateKbps: 11008, ColorTransfer: "bt709"},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "en", Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, Default: true, BitrateKbps: 640},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "en", Title: "SDH"},
		},
	}
)

// tvPMSOnlyRE: theme music, Common Sense Media ages, TVDB ratings images
// and per-season UltraBlur PMS computes from season art.
var tvPMSOnlyRE = regexp.MustCompile(`(@|/)(theme|parentTheme|grandparentTheme|indexes|audioProfile|chromaLocation|codedHeight|codedWidth|colorPrimaries|colorRange|colorSpace|hasScalingMatrix|level|refFrames|scanType|canAutoSync|UltraBlurColors|titleSort|bitrate)$`)

// TestTVConformsToPMS checks show children, season children and episode
// detail against the recorded PMS responses for the same episode.
func TestTVConformsToPMS(t *testing.T) {
	for _, template := range []string{"/library/metadata/{{showKey}}/children", "/library/metadata/{{seasonKey}}/children", "/library/metadata/{{episodeKey}}"} {
		for _, profile := range []string{"web", "ios", "appletv"} {
			for _, jsonMode := range []bool{false, true} {
				t.Run(template+"/"+profile+"/"+map[bool]string{false: "xml", true: "json"}[jsonMode], func(t *testing.T) {
					ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "plex", profile, pmsFixtureName(profile, template, jsonMode)))
					if err != nil {
						t.Fatalf("fixture: %v", err)
					}
					f := newFixture()
					defer f.srv.Close()
					for _, ti := range []core.Title{nineShow, nineS1, nineE1} {
						f.catalog.titles[ti.Key] = ti
					}
					f.catalog.children[nineShow.Key] = []core.RatingKey{nineS1.Key}
					f.catalog.children[nineS1.Key] = []core.RatingKey{nineE1.Key}
					f.playback.info = nineInfo
					target := strings.NewReplacer("{{showKey}}", nineShow.Key.String(), "{{seasonKey}}", nineS1.Key.String(), "{{episodeKey}}", nineE1.Key.String()).Replace(template)
					hdr := []string{"Accept", "application/xml"}
					if jsonMode {
						hdr = jsonAccept
					}
					rec := f.getWarm(t, target, hdr...)
					ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}
					for _, d := range conformplex.Compare(ref, ours).Differences {
						switch {
						case d.Kind == pmsconform.KindMissing && (pmsOnlyRE.MatchString(d.Path) || tvPMSOnlyRE.MatchString(d.Path)):
						case d.Kind == pmsconform.KindExtra && etherOnlyRE.MatchString(d.Path):
						case d.Kind == pmsconform.KindValue && artworkTypeGapRE.MatchString(d.Path):
						case d.Kind == pmsconform.KindValue && !strings.HasSuffix(d.Path, "#order") && !stableMediaValue.MatchString(d.Path):
						default:
							t.Errorf("%s %s ref=%v ours=%v", d.Kind, d.Path, d.Reference, d.Target)
						}
					}
					if !jsonMode {
						checkAttrOrder(t, ref.Body, ours.Body)
					}
				})
			}
		}
	}
}

// containerAttrs returns the root container's attributes as strings.
func containerAttrs(t *testing.T, body []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	b := bytes.TrimSpace(body)
	if len(b) > 0 && b[0] == '{' {
		var v struct {
			MediaContainer map[string]any `json:"MediaContainer"`
		}
		if err := jsonUnmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		for k, x := range v.MediaContainer {
			switch x.(type) {
			case map[string]any, []any:
			default:
				out[k] = jsonScalar(x)
			}
		}
		return out
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			for _, a := range se.Attr {
				out[a.Name.Local] = a.Value
			}
			return out
		}
	}
}

// checkAttrOrder requires the attributes Ether and PMS both send on each
// element to appear in the same order (first element of each name;
// streams by type).
func checkAttrOrder(t *testing.T, ref, ours []byte) {
	t.Helper()
	ro, oo := attrOrders(t, ref), attrOrders(t, ours)
	for el, want := range ro {
		got, ok := oo[el]
		if !ok {
			continue
		}
		common := map[string]bool{}
		for _, a := range want {
			common[a] = true
		}
		var g []string
		inGot := map[string]bool{}
		for _, a := range got {
			inGot[a] = true
			if common[a] {
				g = append(g, a)
			}
		}
		var w []string
		for _, a := range want {
			if inGot[a] {
				w = append(w, a)
			}
		}
		if strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("%s attribute order:\n ours %v\n PMS  %v", el, g, w)
		}
	}
}

func attrOrders(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	d := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		key := se.Name.Local
		var names []string
		for _, a := range se.Attr {
			names = append(names, a.Name.Local)
			if key == "Stream" && a.Name.Local == "streamType" {
				key += ":" + a.Value
			}
		}
		if key == "Stream" {
			key += ":?"
		}
		if _, seen := out[key]; !seen {
			out[key] = names
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonScalar(x any) string {
	switch v := x.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return ""
	}
	return fmt.Sprint(x)
}

func TestPMSFixturesPresent(t *testing.T) {
	// Guard against the fixture set moving: the conformance tests above
	// silently depend on it.
	if _, err := os.Stat(filepath.Join("..", "..", "..", "testdata", "conform", "plex", "manifest.json")); err != nil {
		t.Fatalf("reference fixtures missing: %v", err)
	}
}
