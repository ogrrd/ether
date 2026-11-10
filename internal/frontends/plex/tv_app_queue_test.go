package plex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// The Plex smart-TV app (Plex for LG and Plex for VIDAA 5.94) caches an
// item's playback settings from the first Media list it sees and fails with
// "Unable to find setting item: MediaVersion" when a later response lists
// more versions, before it ever asks for a decision (#89, #339, #375). Its
// episode play flow crosses a season list, the detail page, a checkFiles
// pre-play, a Part-less play queue over the following episodes and the
// decision. Every one of them must show the title as the same single Media,
// whatever versions discovery has found, and the queue must read like PMS's:
// its selection attributes name the selected row and an excluded Part
// leaves Media@partCount behind.
func TestTVAppPlayQueueContract(t *testing.T) {
	for _, product := range []string{"Plex for LG", "Plex for VIDAA"} {
		t.Run(product, func(t *testing.T) {
			f, pb := newVariantPlaybackFixture(t)
			cat := f.srv.catalog().(*fakeCatalog)
			for _, x := range []core.Title{breakingBad, bbS1, bbE1, bbE2} {
				cat.titles[x.Key] = x
			}
			cat.children[breakingBad.Key] = []core.RatingKey{bbS1.Key}
			cat.children[bbS1.Key] = []core.RatingKey{bbE1.Key, bbE2.Key}
			full := testVariantLists()[fightClub.Key]
			f.resolver.byKey[bbE2.Key] = full[:2]
			// Discovery finishes mid-flow: the season list and first detail
			// see no versions, everything after sees three.
			ident := url.Values{
				"X-Plex-Client-Identifier": {"tv-375"}, "X-Plex-Product": {product}, "X-Plex-Platform": {"webOS"},
				"X-Plex-Version": {"5.94.4"}, "X-Plex-Token": {ownerToken},
			}.Encode()
			do := func(method, path, query string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path+"?"+query+"&"+ident, nil)
				req.Header.Set("Accept", "application/json")
				rec := httptest.NewRecorder()
				f.srv.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
				}
				return rec
			}
			type row struct {
				RatingKey       string `json:"ratingKey"`
				PlayQueueItemID int64  `json:"playQueueItemID"`
				Media           []struct {
					ID        flexID     `json:"id"`
					PartCount *int       `json:"partCount"`
					Part      []wirePart `json:"Part"`
				} `json:"Media"`
			}
			type container struct {
				Size                            int    `json:"size"`
				PlayQueueSelectedItemID         int64  `json:"playQueueSelectedItemID"`
				PlayQueueSelectedItemOffset     *int   `json:"playQueueSelectedItemOffset"`
				PlayQueueSelectedMetadataItemID string `json:"playQueueSelectedMetadataItemID"`
				PlayQueueTotalCount             int    `json:"playQueueTotalCount"`
				Metadata                        []row  `json:"Metadata"`
			}
			decode := func(rec *httptest.ResponseRecorder) container {
				t.Helper()
				var body struct{ MediaContainer container }
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body.MediaContainer
			}
			single := func(stage string, rows []row, key core.RatingKey, parts bool) {
				t.Helper()
				for _, r := range rows {
					if r.RatingKey != key.String() {
						continue
					}
					if len(r.Media) != 1 || int64(r.Media[0].ID) != int64(key) {
						t.Fatalf("%s: %s media %+v, want one Auto Media", stage, key, r.Media)
					}
					if parts && (len(r.Media[0].Part) != 1 || int64(r.Media[0].Part[0].ID) != int64(key)) {
						t.Fatalf("%s: part %+v", stage, r.Media[0].Part)
					}
					return
				}
				t.Fatalf("%s: no row for %s", stage, key)
			}
			ep := "/library/metadata/" + bbE1.Key.String()
			list := decode(do("GET", "/library/metadata/"+bbS1.Key.String()+"/children", "X-Plex-Container-Start=0&X-Plex-Container-Size=10&includeDetails=1&excludeFields=file,tagline"))
			single("season list", list.Metadata, bbE1.Key, true)
			single("detail", decode(do("GET", ep, "")).Metadata, bbE1.Key, true)
			f.resolver.mu.Lock()
			f.resolver.byKey[bbE1.Key] = full
			f.resolver.mu.Unlock()
			single("pre-play", decode(do("GET", ep, "checkFiles=1&includeChapters=1")).Metadata, bbE1.Key, true)

			uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library" + ep
			q := decode(do("POST", "/playQueues", url.Values{
				"type": {"video"}, "uri": {uri}, "continuous": {"1"}, "repeat": {"0"}, "own": {"1"},
				"excludeElements": {"Actor,Collection,Country,Director,Genre,Label,Mood,Part,Producer,Role,Similar,Writer,Photo"},
				"excludeFields":   {"file,tagline"},
			}.Encode()))
			if len(q.Metadata) != 2 || q.Size != 2 || q.PlayQueueTotalCount != 2 ||
				q.Metadata[0].RatingKey != bbE1.Key.String() || q.Metadata[1].RatingKey != bbE2.Key.String() {
				t.Fatalf("queue rows: %+v", q)
			}
			if q.PlayQueueSelectedItemOffset == nil || *q.PlayQueueSelectedItemOffset != 0 ||
				q.PlayQueueSelectedItemID != q.Metadata[0].PlayQueueItemID || q.PlayQueueSelectedMetadataItemID != bbE1.Key.String() {
				t.Fatalf("queue selection: %+v", q)
			}
			for _, key := range []core.RatingKey{bbE1.Key, bbE2.Key} {
				single("queue", q.Metadata, key, false)
			}
			for _, r := range q.Metadata {
				if m := r.Media[0]; len(m.Part) != 0 || m.PartCount == nil || *m.PartCount != 1 {
					t.Fatalf("queue %s: excluded Part must leave partCount=1: %+v", r.RatingKey, m)
				}
			}

			single("after queue", decode(do("GET", ep, "")).Metadata, bbE1.Key, true)
			single("next episode", decode(do("GET", "/library/metadata/"+bbE2.Key.String(), "")).Metadata, bbE2.Key, true)
			dq := "path=" + url.QueryEscape(ep) + "&mediaIndex=0&partIndex=0&session=tv375&directPlay=1&directStream=1&protocol=hls"
			single("decision", decode(do("GET", "/video/:/transcode/universal/decision", dq)).Metadata, bbE1.Key, true)
			if len(pb.variantCalls) != 0 {
				t.Fatalf("Auto must open the release, got explicit versions %v", pb.variantCalls)
			}
		})
	}
}
