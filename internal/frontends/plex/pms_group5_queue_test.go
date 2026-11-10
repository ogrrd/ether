package plex

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"net/url"
	"testing"
)

// Exercise the request chain, since queue versions, identity and selection
// must remain coherent across mutation responses and subsequent reads.
func TestPMSGroup5QueueTransitions(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	headers := map[string]string{"Accept": "application/json", "X-Plex-Client-Identifier": "A"}
	uri := "/library/metadata/" + fightClub.Key.String() + "," + bbE1.Key.String() + "," + bbE2.Key.String()
	create := func(client string) map[string]any {
		h := map[string]string{"Accept": "application/json", "X-Plex-Client-Identifier": client}
		r := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape(uri), h)
		if r.Code != 200 {
			t.Fatalf("create %d %s", r.Code, r.Body.String())
		}
		return decodeJSON(t, r)
	}
	q := create("A")
	path := fmt.Sprintf("/playQueues/%.0f", q["playQueueID"])
	if _, ok := q["allowSync"]; ok {
		t.Fatal("queue contains allowSync")
	}
	q2 := create("B")
	path2 := fmt.Sprintf("/playQueues/%.0f", q2["playQueueID"])
	r := f.request(t, "GET", path+"?own=1", map[string]string{"Accept": "application/json", "X-Plex-Client-Identifier": "B"})
	if r.Code != 200 || decodeJSON(t, r)["playQueueVersion"] != float64(2) {
		t.Fatal("ownership transfer", r.Body.String())
	}
	if f.request(t, "GET", path2, headers).Code != 404 {
		t.Fatal("taker's old queue survived")
	}
	create("A")
	if f.request(t, "GET", path, headers).Code != 200 {
		t.Fatal("creator retired transferred queue")
	}
	for query, want := range map[string]int{"window=1": 2, "window=0": 0, "center=999999&window=1": 0, "includeBefore=0": 2, "includeAfter=0": 0} {
		r = f.request(t, "GET", path+"?"+query, headers)
		if r.Code != 200 || decodeJSON(t, r)["size"] != float64(want) {
			t.Fatalf("%s: %s", query, r.Body.String())
		}
	}
	for _, query := range []string{"window=-1", "includeBefore=true", "includeAfter="} {
		if f.request(t, "GET", path+"?"+query, headers).Code != 400 {
			t.Fatal(query)
		}
	}
	rows := q["Metadata"].([]any)
	last := rows[2].(map[string]any)["playQueueItemID"].(float64)
	f.srv.queues.SelectItem(ownerUser.ID, int64(last))
	r = f.request(t, "DELETE", fmt.Sprintf("%s/items/%.0f", path, last), headers)
	deleted := decodeJSON(t, r)
	if r.Code != 200 || deleted["size"] != float64(0) || deleted["playQueueTotalCount"] != float64(3) || deleted["playQueueVersion"] != float64(2) || deleted["playQueueSelectedItemOffset"] != float64(0) {
		t.Fatalf("delete: %s", r.Body.String())
	}
	r = f.request(t, "GET", path+"?window=50", headers)
	q = decodeJSON(t, r)
	if _, ok := q["playQueueSelectedItemID"]; ok || q["size"] != float64(0) {
		t.Fatal("lost selection", r.Body.String())
	}
	center := rows[0].(map[string]any)["playQueueItemID"].(float64)
	r = f.request(t, "GET", fmt.Sprintf("%s?center=%.0f&window=1", path, center), headers)
	if decodeJSON(t, r)["size"] != float64(2) {
		t.Fatal("explicit center", r.Body.String())
	}
}

func TestPMSGroup5QueueFlagsAndShuffle(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	h := map[string]string{"Accept": "application/json"}
	uri := url.QueryEscape("/library/metadata/" + fightClub.Key.String() + "," + bbE1.Key.String() + "," + bbE2.Key.String())
	for _, flag := range []string{"", "True", "true", "2", "-1", " 1"} {
		r := f.request(t, "POST", "/playQueues?type=video&uri="+uri+"&shuffle="+url.QueryEscape(flag), h)
		if r.Code != 400 {
			t.Fatal(flag, r.Code)
		}
	}
	r := f.request(t, "POST", "/playQueues?type=video&uri="+uri+"&shuffle=%2B01&repeat=001", h)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	q := decodeJSON(t, r)
	path := fmt.Sprintf("/playQueues/%.0f", q["playQueueID"])
	selected := q["playQueueSelectedItemID"]
	version := q["playQueueVersion"].(float64)
	for _, op := range []string{"shuffle", "shuffle", "unshuffle", "unshuffle"} {
		r = f.request(t, "PUT", path+"/"+op, h)
		next := decodeJSON(t, r)
		version++
		if r.Code != 200 || next["playQueueSelectedItemID"] != selected || next["playQueueVersion"] != version {
			t.Fatalf("%s: %s", op, r.Body.String())
		}
		prior := map[any]bool{}
		for _, row := range q["Metadata"].([]any) {
			prior[row.(map[string]any)["playQueueItemID"]] = true
		}
		for _, row := range next["Metadata"].([]any) {
			id := row.(map[string]any)["playQueueItemID"]
			if id != selected && prior[id] {
				t.Fatal("non-selected ID survived", op)
			}
		}
		q = next
	}
}

func TestPMSGroup5QueueDurableRestart(t *testing.T) {
	db := store.OpenTest(t)
	a := newFixture(func(d *testServerDeps) { d.DB = db; d.DisableEnrichment = true })
	q, err := a.srv.queues.CreateWith(ownerUser.ID, "persistent", []core.RatingKey{fightClub.Key, bbE1.Key}, fixedNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.srv.queues.SelectItem(ownerUser.ID, q.Items[1].ID)
	a.srv.Close()
	b := newFixture(func(d *testServerDeps) { d.DB = db; d.DisableEnrichment = true })
	defer b.srv.Close()
	restored, ok := b.srv.queues.Get(q.ID, ownerUser.ID)
	if !ok || restored.Selected != 1 || restored.Items[1].ID != q.Items[1].ID {
		t.Fatalf("restart lost queue: %+v", restored)
	}
	fresh, err := b.srv.queues.CreateWith(ownerUser.ID, "other", []core.RatingKey{alien.Key}, fixedNow, nil)
	if err != nil || fresh.ID <= q.ID || fresh.Items[0].ID <= q.Items[1].ID {
		t.Fatal("counters reset", fresh, err)
	}
}
