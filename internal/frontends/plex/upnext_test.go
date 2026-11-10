package plex

import (
	"encoding/json"
	"encoding/xml"
	"net/url"
	"strings"
	"testing"

	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

type upNextWireItem struct {
	Key       string     `json:"key" xml:"key,attr"`
	RatingKey string     `json:"ratingKey" xml:"ratingKey,attr"`
	Media     []struct{} `json:"Media" xml:"Media"`
}
type upNextWireContainer struct {
	Hub []struct {
		Key           string `json:"key" xml:"key,attr"`
		HubKey        string `json:"hubKey" xml:"hubKey,attr"`
		HubIdentifier string `json:"hubIdentifier" xml:"hubIdentifier,attr"`
	} `json:"Hub" xml:"Hub"`
	Metadata []upNextWireItem `json:"Metadata" xml:"Video"`
}

func TestPostplayUpNextReloadAndPlayQueue(t *testing.T) {
	for name, client := range conformplex.Clients() {
		for _, format := range []string{"xml", "json"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				f := newFixture()
				defer f.srv.Close()
				// The next episode must work without the optional recommender.
				f.srv.d.Recommender = nil
				f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
				headers := map[string]string{"Accept": "application/" + format}
				for k, v := range client.Headers {
					headers[k] = v[0]
				}
				headers["X-Plex-Token"] = ownerToken
				read := func(path string) upNextWireContainer {
					t.Helper()
					r := f.request(t, "GET", path, headers)
					if r.Code != 200 {
						t.Fatalf("%s: %d %s", path, r.Code, r.Body)
					}
					var mc upNextWireContainer
					var err error
					if format == "json" {
						var body struct{ MediaContainer upNextWireContainer }
						err = json.Unmarshal(r.Body.Bytes(), &body)
						mc = body.MediaContainer
					} else {
						err = xml.Unmarshal(r.Body.Bytes(), &mc)
					}
					if err != nil {
						t.Fatal(err)
					}
					return mc
				}
				post := read("/hubs/metadata/" + bbE1.Key.String() + "/postplay")
				if len(post.Hub) != 1 || post.Hub[0].HubIdentifier != "tv.upnext" {
					t.Fatalf("postplay: %+v", post.Hub)
				}
				hub := post.Hub[0]
				fetched := read(hub.Key)
				if len(fetched.Metadata) != 1 || fetched.Metadata[0].RatingKey != bbE2.Key.String() || len(fetched.Metadata[0].Media) == 0 {
					t.Fatalf("unplayable next episode: %+v", fetched.Metadata)
				}
				for _, uri := range []string{hub.Key, "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library" + hub.Key, "library://section/directory/" + url.PathEscape(hub.Key), hub.HubKey, "/hubs/items?identifier=tv.upnext&metadataID=" + bbE1.Key.String()} {
					q := url.Values{"type": {"video"}, "continuous": {"1"}, "uri": {uri}, "key": {fetched.Metadata[0].Key}}
					r := f.request(t, "POST", "/playQueues?"+q.Encode(), headers)
					attrs := containerAttrs(t, r.Body.Bytes())
					if r.Code != 200 || attrs["playQueueSelectedMetadataItemID"] != bbE2.Key.String() || attrs["playQueueTotalCount"] != "1" {
						t.Fatalf("queue from %s: %d %s", uri, r.Code, r.Body)
					}
				}
				f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
				for _, probe := range conformplex.PlaybackCorpus() {
					if !strings.HasPrefix(probe.Name, "transcode-") {
						continue
					}
					path := strings.NewReplacer("{{movieKey}}", bbE2.Key.String(), "{{session}}", "up-next").Replace(probe.Path)
					if probe.Name == "transcode-start" {
						if decision := f.request(t, "GET", decisionPath(path), headers); decision.Code != 200 {
							t.Fatalf("next episode decision: %d %s", decision.Code, decision.Body)
						}
					}
					r := f.request(t, probe.Method, path, headers)
					if r.Code != 200 {
						t.Fatalf("next episode %s: %d %s", probe.Name, r.Code, r.Body)
					}
					if probe.Name == "transcode-segment" && r.Body.String() != "segment-0" {
						t.Fatal("next episode media not delivered")
					}
				}
			})
		}
	}
}

func TestUpNextCannotExposeHiddenSourceOrNextEpisode(t *testing.T) {
	for _, hidden := range []core.Title{bbE1, bbE2, breakingBad} {
		t.Run(hidden.Key.String(), func(t *testing.T) {
			f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
			defer f.srv.Close()
			hidden.Keywords = []string{"blocked"}
			f.catalog.titles[hidden.Key] = hidden
			path := "/hubs/items/tv.upnext?metadataID=" + bbE1.Key.String()
			headers := map[string]string{"X-Plex-Token": friendToken, "Accept": "application/json"}
			r := f.request(t, "GET", path, headers)
			mc := decodeJSON(t, r)
			if mc["size"] != float64(0) {
				t.Fatalf("hidden episode in row: %s", r.Body)
			}
			r = f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(path), headers)
			if r.Code != 404 {
				t.Fatalf("hidden queue: %d %s", r.Code, r.Body)
			}
		})
	}
}
