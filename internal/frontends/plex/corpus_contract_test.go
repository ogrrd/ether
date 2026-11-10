package plex

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

// Every recorded browsing probe is exercised in both encodings and all three
// client profiles. Dataset-dependent listings differ; protocol identity and
// status are still checked against the recording rather than merely parsed.
var contractSkips = func() []loadBearingSkip {
	var out []loadBearingSkip
	add := func(id, endpoint, path, reason string) {
		out = append(out, loadBearingSkip{ID: id, Endpoint: endpoint, Path: path, Fixture: "*", Reason: reason})
	}
	reason := "Ether answers allowSync=0; PMS 1.43.4 answers 1. Changing Plex production responses is out of this slice"
	for _, ep := range []string{
		"/library/metadata/{{movieKey}}?includeExtras=1&includeRelated=1&includeOnDeck=1&includeChapters=1&includeGuids=1",
		"/library/metadata/{{showKey}}/children",
		"/library/metadata/{{seasonKey}}/children",
		"/library/metadata/{{episodeKey}}",
	} {
		add(ep+"-allowSync-xml", ep, "/MediaContainer/@allowSync", reason)
		add(ep+"-allowSync-json", ep, "$/MediaContainer/allowSync", reason)
	}
	return out
}()

func valueContractPath(path string) bool {
	return strings.HasPrefix(path, "/library/metadata/") ||
		strings.HasPrefix(path, "/library/onDeck") ||
		strings.HasPrefix(path, "/library/collections/") ||
		strings.HasPrefix(path, "/playlists/")
}

func matchContractSkip(endpoint, fixture string, d pmsconform.Difference, used map[string]bool) (bool, string) {
	for _, s := range contractSkips {
		if s.Endpoint != endpoint || s.Path != d.Path {
			continue
		}
		if s.Fixture != "*" && s.Fixture != fixture {
			continue
		}
		used[s.ID] = true
		return true, s.Reason
	}
	return false, ""
}

func TestRecordedCorpusProtocolContracts(t *testing.T) {
	used := map[string]bool{}
	for _, probe := range conformplex.Corpus() {
		for profile, client := range conformplex.Clients() {
			for _, jsonMode := range []bool{false, true} {
				t.Run(profile+"/"+pmsFixtureName(profile, probe.Path, jsonMode), func(t *testing.T) {
					ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "plex", profile, pmsFixtureName(profile, probe.Path, jsonMode)))
					if err != nil {
						t.Fatal(err)
					}
					f := newFixture()
					// XML's scalar heuristic treats a two-part OS version as a number;
					// use a normal dotted version string for this cross-platform fixture.
					f.srv.setTestPlatformVersion("6.1.0")
					defer f.srv.Close()
					for _, title := range []core.Title{burbs, nineShow, nineS1, nineE1} {
						f.catalog.titles[title.Key] = title
					}
					f.catalog.children[nineShow.Key] = []core.RatingKey{nineS1.Key}
					f.catalog.children[nineS1.Key] = []core.RatingKey{nineE1.Key}
					f.playback.info = burbsInfo
					target := strings.NewReplacer("{{section}}", "1", "{{movieSection}}", "1", "{{showSection}}", "2", "{{movieKey}}", burbs.Key.String(), "{{showKey}}", nineShow.Key.String(), "{{seasonKey}}", nineS1.Key.String(), "{{episodeKey}}", nineE1.Key.String(), "{{collectionKey}}", "1", "{{playlistKey}}", "1", "{{personKey}}", "31", "{{personTagKey}}", "tmdb-person-31").Replace(probe.Path)
					hdr := map[string]string{}
					for k, v := range client.Headers {
						if !strings.EqualFold(k, "X-Plex-Token") {
							hdr[k] = v[0]
						}
					}
					hdr["Accept"] = "application/xml"
					if jsonMode {
						hdr["Accept"] = "application/json"
					}
					rec := f.request(t, probe.Method, target, hdr)
					// Fixtures for personal collection/playlist IDs refer to the recorded
					// owner's data, absent from this fixture. Their absence must remain 404.
					expected := ref.Status
					if strings.HasPrefix(probe.Path, "/library/collections/") || strings.HasPrefix(probe.Path, "/playlists/") {
						expected = http.StatusNotFound
					}
					if rec.Code != expected {
						t.Fatalf("status %d want %d: %s", rec.Code, expected, rec.Body.String())
					}
					if expected != http.StatusOK {
						return
					}
					ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}
					for _, diff := range conformplex.Compare(ref, ours).Differences {
						// Three legacy root JSON recordings lost the original multiuser
						// boolean during redaction. Their explicitly redacted value is opaque.
						if strings.HasSuffix(diff.Path, "/multiuser") && diff.Reference == "[REDACTED]" {
							continue
						}
						switch diff.Kind {
						case pmsconform.KindParse, pmsconform.KindType, pmsconform.KindStatus, pmsconform.KindHeader:
							t.Errorf("%s %s PMS=%v ours=%v", diff.Kind, diff.Path, diff.Reference, diff.Target)
						case pmsconform.KindValue:
							if !diff.Blocking || !valueContractPath(probe.Path) {
								continue
							}
							if catalogCountField(diff.Path) {
								continue
							}
							if skip, reason := matchContractSkip(probe.Path, pmsFixtureName(profile, probe.Path, jsonMode), diff, used); skip {
								t.Logf("skip %s %s: %s", diff.Kind, diff.Path, reason)
								continue
							}
							t.Errorf("%s %s PMS=%v ours=%v", diff.Kind, diff.Path, diff.Reference, diff.Target)
						}
					}
					got, want := containerAttrs(t, rec.Body.Bytes()), containerAttrs(t, ref.Body)
					for _, key := range []string{"identifier", "mediaTagPrefix"} {
						if value, ok := want[key]; ok && got[key] != value {
							t.Errorf("%s=%q PMS=%q", key, got[key], value)
						}
					}
				})
			}
		}
	}
	for _, s := range contractSkips {
		if !used[s.ID] {
			t.Errorf("stale skip %s (%s %s %s) never matched: %s", s.ID, s.Endpoint, s.Path, s.Fixture, s.Reason)
		}
	}
}
