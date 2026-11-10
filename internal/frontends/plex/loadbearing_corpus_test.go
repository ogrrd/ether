package plex

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

type loadBearingSkip struct {
	ID       string
	Endpoint string
	Path     string
	Fixture  string
	Reason   string
}

// Each skip is keyed by (endpoint, exact path, fixture). A skip that
// never matches a blocking value fails the parent test.
const allowSyncReason = "Ether answers allowSync=0; PMS 1.43.4 answers 1. Changing Plex production responses is out of this slice"
const hubDashReason = "Ether's home/dashboard hub composition is not the recorded PMS library; slice D owns the dashboard"
const hubSectionReason = "Ether's section hubs are not the recorded PMS library; slice B owns browse hubs"
const prefsReason = "Ether exposes fewer section prefs than PMS 1.43.4; production change is out of this slice"
const catalogIDReason = "section IDs and item types follow this fixture's catalog, not the recorded PMS library"

var loadBearingSkips = func() []loadBearingSkip {
	var out []loadBearingSkip
	add := func(id, endpoint, path, reason string) {
		out = append(out, loadBearingSkip{ID: id, Endpoint: endpoint, Path: path, Fixture: "*", Reason: reason})
	}

	sectionAll := "/library/sections/{{section}}/all?X-Plex-Container-Start=0&X-Plex-Container-Size=50"
	add("section-all-allowSync-xml", sectionAll, "/MediaContainer/@allowSync", allowSyncReason)
	add("section-all-allowSync-json", sectionAll, "$/MediaContainer/allowSync", allowSyncReason)

	hubPaths := [][2]string{
		{"/MediaContainer/Hub/@hubIdentifier", "$/MediaContainer/Hub[]/hubIdentifier"},
		{"/MediaContainer/Hub/@context", "$/MediaContainer/Hub[]/context"},
		{"/MediaContainer/Hub/@style", "$/MediaContainer/Hub[]/style"},
		{"/MediaContainer/Hub/@more", "$/MediaContainer/Hub[]/more"},
		{"/MediaContainer/Hub/@type", "$/MediaContainer/Hub[]/type"},
		{"/MediaContainer/Hub/@key", "$/MediaContainer/Hub[]/key"},
	}
	for _, ep := range []string{"/hubs", "/hubs/promoted"} {
		reason := hubDashReason
		for i, p := range hubPaths {
			add(ep+"-hub-xml-"+strconv.Itoa(i), ep, p[0], reason)
			add(ep+"-hub-json-"+strconv.Itoa(i), ep, p[1], reason)
		}
		add(ep+"-video-section-xml", ep, "/MediaContainer/Hub/Video/@librarySectionID", catalogIDReason)
		add(ep+"-video-type-xml", ep, "/MediaContainer/Hub/Video/@type", catalogIDReason)
		add(ep+"-meta-section-json", ep, "$/MediaContainer/Hub[]/Metadata[]/librarySectionID", catalogIDReason)
		add(ep+"-meta-type-json", ep, "$/MediaContainer/Hub[]/Metadata[]/type", catalogIDReason)
		add(ep+"-meta-key-json", ep, "$/MediaContainer/Hub[]/Metadata[]/key", catalogIDReason)
	}
	ep := "/hubs/sections/{{section}}"
	sectionHubs := [][2]string{
		{"/MediaContainer/Hub/@hubIdentifier", "$/MediaContainer/Hub[]/hubIdentifier"},
		{"/MediaContainer/Hub/@context", "$/MediaContainer/Hub[]/context"},
		{"/MediaContainer/Hub/@more", "$/MediaContainer/Hub[]/more"},
		{"/MediaContainer/Hub/@key", "$/MediaContainer/Hub[]/key"},
	}
	for i, p := range sectionHubs {
		add(ep+"-hub-xml-"+strconv.Itoa(i), ep, p[0], hubSectionReason)
		add(ep+"-hub-json-"+strconv.Itoa(i), ep, p[1], hubSectionReason)
	}
	for _, pref := range []string{"/library/sections/{{movieSection}}/prefs", "/library/sections/{{showSection}}/prefs"} {
		add(pref+"-size-xml", pref, "/MediaContainer/@size", prefsReason)
		add(pref+"-size-json", pref, "$/MediaContainer/size", prefsReason)
	}
	return out
}()

// TestRecordedHubsSearchSectionsLoadBearing compares Ether to the PMS
// recordings for hubs, search and sections on load-bearing fields.
func TestRecordedHubsSearchSectionsLoadBearing(t *testing.T) {
	used := map[string]bool{}
	for _, probe := range conformplex.Corpus() {
		if !browseLoadBearingPath(probe.Path) {
			continue
		}
		for profile, client := range conformplex.Clients() {
			for _, jsonMode := range []bool{false, true} {
				fixture := pmsFixtureName(profile, probe.Path, jsonMode)
				t.Run(profile+"/"+fixture, func(t *testing.T) {
					ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "plex", profile, fixture))
					if err != nil {
						t.Fatal(err)
					}
					f := newFixture()
					f.srv.setTestPlatformVersion("6.1.0")
					defer f.srv.Close()
					for _, title := range []core.Title{burbs, nineShow, nineS1, nineE1} {
						f.catalog.titles[title.Key] = title
					}
					f.catalog.children[nineShow.Key] = []core.RatingKey{nineS1.Key}
					f.catalog.children[nineS1.Key] = []core.RatingKey{nineE1.Key}
					f.playback.info = burbsInfo
					target := strings.NewReplacer("{{section}}", "1", "{{movieSection}}", "1", "{{showSection}}", "2", "{{movieKey}}", burbs.Key.String(), "{{showKey}}", nineShow.Key.String(), "{{seasonKey}}", nineS1.Key.String(), "{{episodeKey}}", nineE1.Key.String(), "{{collectionKey}}", "1", "{{playlistKey}}", "1").Replace(probe.Path)
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
					if rec.Code != ref.Status {
						t.Fatalf("status %d want %d: %s", rec.Code, ref.Status, rec.Body.String())
					}
					ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}
					checkCountConsistency(t, ours.Body)
					for _, d := range conformplex.Compare(ref, ours).Differences {
						if !d.Blocking || d.Kind != pmsconform.KindValue {
							continue
						}
						if catalogCountField(d.Path) && !prefsExactSize(probe.Path, d.Path) {
							continue
						}
						if skip, reason := matchLoadBearingSkip(probe.Path, fixture, d, used); skip {
							t.Logf("skip %s %s: %s", d.Kind, d.Path, reason)
							continue
						}
						t.Errorf("%s %s PMS=%v ours=%v", d.Kind, d.Path, d.Reference, d.Target)
					}
				})
			}
		}
	}
	for _, s := range loadBearingSkips {
		if !used[s.ID] {
			t.Errorf("stale skip %s (%s %s %s) never matched: %s", s.ID, s.Endpoint, s.Path, s.Fixture, s.Reason)
		}
	}
}

func browseLoadBearingPath(path string) bool {
	return strings.HasPrefix(path, "/hubs") || strings.HasPrefix(path, "/library/search") || strings.HasPrefix(path, "/library/sections")
}

func matchLoadBearingSkip(endpoint, fixture string, d pmsconform.Difference, used map[string]bool) (bool, string) {
	for _, s := range loadBearingSkips {
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

func catalogCountField(path string) bool {
	switch diffLeaf(path) {
	case "size", "totalSize", "leafCount", "viewedLeafCount", "childCount":
		return true
	}
	return false
}

func prefsExactSize(endpoint, path string) bool {
	return strings.Contains(endpoint, "/prefs") && diffLeaf(path) == "size"
}

func diffLeaf(path string) string {
	leaf := path
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}
	return strings.TrimSuffix(strings.TrimPrefix(leaf, "@"), "[]")
}

func checkCountConsistency(t *testing.T, body []byte) {
	t.Helper()
	c, ok := parseContainerCounts(body)
	if !ok {
		return
	}
	if c.size != "" {
		n, err := strconv.Atoi(c.size)
		if err == nil && n != c.children {
			t.Errorf("size=%s but children=%d", c.size, c.children)
		}
	}
	if c.size != "" && c.totalSize != "" {
		s, e1 := strconv.Atoi(c.size)
		ts, e2 := strconv.Atoi(c.totalSize)
		if e1 == nil && e2 == nil && ts < s {
			t.Errorf("totalSize=%d < size=%d", ts, s)
		}
	}
	for i, it := range c.items {
		if it.leafCount == "" || it.viewedLeafCount == "" {
			continue
		}
		leaf, e1 := strconv.Atoi(it.leafCount)
		viewed, e2 := strconv.Atoi(it.viewedLeafCount)
		if e1 == nil && e2 == nil && leaf < viewed {
			t.Errorf("item %d leafCount=%d < viewedLeafCount=%d", i, leaf, viewed)
		}
	}
}

type containerCounts struct {
	size, totalSize string
	children        int
	items           []itemCounts
}

type itemCounts struct {
	leafCount, viewedLeafCount string
}

func parseContainerCounts(body []byte) (containerCounts, bool) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return containerCounts{}, false
	}
	if b[0] == '{' {
		return jsonContainerCounts(b)
	}
	if b[0] != '<' {
		return containerCounts{}, false
	}
	return xmlContainerCounts(b)
}

func jsonContainerCounts(body []byte) (containerCounts, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return containerCounts{}, false
	}
	mc, _ := root["MediaContainer"].(map[string]any)
	if mc == nil {
		return containerCounts{}, false
	}
	out := containerCounts{size: jsonNumber(mc["size"]), totalSize: jsonNumber(mc["totalSize"])}
	for _, key := range []string{"Hub", "Metadata", "Directory", "Setting", "Video", "Photo", "Playlist", "SearchResult"} {
		arr, ok := mc[key].([]any)
		if !ok {
			continue
		}
		if out.children == 0 {
			out.children = len(arr)
		}
		for _, item := range arr {
			obj, _ := item.(map[string]any)
			if obj == nil {
				continue
			}
			out.items = append(out.items, itemCounts{leafCount: jsonNumber(obj["leafCount"]), viewedLeafCount: jsonNumber(obj["viewedLeafCount"])})
		}
	}
	return out, true
}

func jsonNumber(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	case json.Number:
		return x.String()
	}
	return ""
}

func xmlContainerCounts(body []byte) (containerCounts, bool) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out containerCounts
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, true
		}
		if err != nil {
			return containerCounts{}, false
		}
		switch se := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "size":
						out.size = a.Value
					case "totalSize":
						out.totalSize = a.Value
					}
				}
				continue
			}
			if depth == 2 {
				out.children++
				item := itemCounts{}
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "leafCount":
						item.leafCount = a.Value
					case "viewedLeafCount":
						item.viewedLeafCount = a.Value
					}
				}
				out.items = append(out.items, item)
			}
		case xml.EndElement:
			depth--
		}
	}
}
