package plex

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/store"
)

// twoServers builds two Plex front ends sharing one database (on PostgreSQL:
// one schema) and an in-memory cluster bus, as two cluster nodes would.
func twoServers(t *testing.T) (*fixture, *fixture, *clusterbus.Hub) {
	t.Helper()
	db := store.OpenTest(t)
	hub := clusterbus.NewHub()
	mk := func(name string) *fixture {
		f := newFixture(func(d *Deps) {
			d.DB, d.Cluster = db, hub.Node(name)
			d.DisableEnrichment = true
			d.SessionHeartbeat = 50 * time.Millisecond
		})
		t.Cleanup(func() {
			f.srv.Close()
			if f.broker != nil {
				f.broker.Close(context.Background())
			}
		})
		return f
	}
	return mk("a"), mk("b"), hub
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sessionsOf(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	mc := decodeJSON(t, f.get(t, "/status/sessions", jsonAccept...))
	var out []map[string]any
	if md, ok := mc["Metadata"].([]any); ok {
		for _, m := range md {
			out = append(out, m.(map[string]any))
		}
	}
	return out
}

func TestClusterHeartbeatPreservesBrokerSessions(t *testing.T) {
	a, _, _ := twoServers(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for _, row := range []struct {
		id, node string
		updated  int64
	}{{"broker-local", "a", now}, {"broker-stale", "other", now - int64((11*time.Minute)/time.Millisecond)}, {"plex-broker-local", "a", now}, {"plex-broker-stale", "other", now - int64((11*time.Minute)/time.Millisecond)}} {
		protocol := "jellyfin"
		if strings.HasPrefix(row.id, "plex-") {
			protocol = "plex"
		}
		if _, err := a.srv.cl.db.ExecContext(ctx, `INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at,protocol,surface) VALUES(?,?,?,?,?,?,?,?)`, row.id, row.node, 1, 123, `{}`, row.updated, protocol, protocol); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.srv.cl.syncSessions(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.srv.cl.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_sessions WHERE surface IS NOT NULL`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("heartbeat removed broker sessions: count=%d err=%v", count, err)
	}
}

func TestClusterSessionsListedOnEveryNode(t *testing.T) {
	a, b, hub := twoServers(t)
	a.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken, "X-Plex-Device-Name", "Den TV")
	b.get(t, "/:/timeline?ratingKey="+alien.Key.String()+"&state=playing&time=5000&duration=10000", "X-Plex-Session-Identifier", "sy", "X-Plex-Client-Identifier", "client-2")
	b.get(t, "/:/timeline?ratingKey="+alien.Key.String()+"&state=paused&time=5000&duration=10000",
		"X-Plex-Session-Identifier", "sy", "X-Plex-Client-Identifier", "client-2")

	var onA, onB []map[string]any
	waitFor(t, "both nodes list both streams", func() bool {
		onA, onB = sessionsOf(t, a), sessionsOf(t, b)
		return len(onA) == 2 && len(onB) == 2
	})
	keys := map[string]bool{}
	for _, md := range onB {
		keys[fmt.Sprint(md["sessionKey"])] = true
		if md["ratingKey"] == "1000000000000550" {
			player := md["Player"].(map[string]any)
			if player["title"] != "Den TV" || player["state"] != "playing" || md["viewOffset"].(float64) != 1000 {
				t.Fatalf("remote session on B: %v", md)
			}
		}
	}
	if len(keys) != 2 {
		t.Fatalf("session keys collide across nodes: %v", keys)
	}
	// A stop on node A disappears everywhere.
	a.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=2000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	waitFor(t, "stop reaches B", func() bool { return len(sessionsOf(t, b)) == 1 })
	// A dead node's streams vanish from the others.
	hub.Kill("b")
	time.Sleep(liveCacheTTL + 100*time.Millisecond)
	if got := sessionsOf(t, a); len(got) != 0 {
		t.Fatalf("dead node's session still listed: %v", got)
	}
}

func TestClusterTerminateAppliedByServingNode(t *testing.T) {
	a, b, _ := twoServers(t)
	ps, _ := a.playback.Prepare(context.Background(), &friendUser, fightClub.Key, core.ClientCaps{ClientID: "client-1"})
	a.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	waitFor(t, "B sees the stream", func() bool { return len(sessionsOf(t, b)) == 1 })
	if rec := b.get(t, "/status/sessions/terminate?sessionId=sx&reason=Bedtime"); rec.Code != http.StatusOK {
		t.Fatalf("terminate on B: %d", rec.Code)
	}
	waitFor(t, "A stops the source", func() bool {
		a.playback.mu.Lock()
		defer a.playback.mu.Unlock()
		return len(a.playback.stopped) == 1 && a.playback.stopped[0] == ps.ID
	})
	rec := a.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=2000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	if !strings.Contains(rec.Body.String(), `terminationCode="2006" terminationText="Bedtime"`) {
		t.Fatalf("termination not delivered by the serving node: %s", rec.Body.String())
	}
	if rec := b.get(t, "/status/sessions/terminate?sessionId=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", rec.Code)
	}
}

func TestClusterOwnerStopReachesServingNode(t *testing.T) {
	a, b, _ := twoServers(t)
	ctx := context.Background()
	ps, err := a.playback.Prepare(ctx, &friendUser, fightClub.Key, core.ClientCaps{ClientID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := sessions.PlanAdmission{Source: ps, Media: ps.Info, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}
	plexCtx := sessions.WithPlanAdmission(ctx, func(context.Context) (sessions.PlanAdmission, error) {
		return admitted, nil
	})
	viewer := &core.Viewer{ID: friendUser.ID}
	device := core.Device{ID: "c"}
	plan, err := a.broker.Plan(plexCtx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "plex", Item: fightClub.Key, Caps: core.ClientCaps{ClientID: "c"}, Intent: frontend.PlayIntent{ClientSessionID: "plex-owner", Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.broker.Open(plexCtx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "plex", PlanID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	if !b.broker.StopPlayback(ctx, ps.ID) {
		t.Fatal("remote stop was not routed through the broker")
	}
	waitFor(t, "A stops it", func() bool {
		_, live := a.playback.Session(ps.ID)
		return !live
	})
}

func TestClusterPlayQueueFollowsClientAcrossNodes(t *testing.T) {
	a, b, _ := twoServers(t)
	uri := urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + breakingBad.Key.String())
	mc := decodeJSON(t, a.request(t, http.MethodPost, "/playQueues?type=video&uri="+uri, map[string]string{"Accept": "application/json"}))
	id := fmt.Sprint(int64(mc["playQueueID"].(float64)))
	items := mc["Metadata"].([]any)
	second := int64(items[1].(map[string]any)["playQueueItemID"].(float64))

	// Node B serves the queue node A created...
	mc = decodeJSON(t, b.get(t, "/playQueues/"+id, jsonAccept...))
	if mc["playQueueTotalCount"].(float64) != 2 || fmt.Sprint(int64(mc["playQueueID"].(float64))) != id {
		t.Fatalf("queue on B: %v", mc)
	}
	// ...edits it...
	mc = decodeJSON(t, b.request(t, http.MethodPut, "/playQueues/"+id+"?uri="+urlEsc("/library/metadata/"+fightClub.Key.String()), map[string]string{"Accept": "application/json"}))
	if mc["playQueueTotalCount"].(float64) != 3 || mc["playQueueVersion"].(float64) != 2 {
		t.Fatalf("add on B: %v", mc)
	}
	// ...and records the playing item from a timeline.
	b.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=playing&time=1&duration=10&playQueueItemID=%d", bbE2.Key, second))
	// Plain additions enter Up Next before the second original item, moving
	// that item to offset 2. Node A sees both the edit and selection by ID.
	waitFor(t, "A sees B's edits", func() bool {
		mc = decodeJSON(t, a.get(t, "/playQueues/"+id, jsonAccept...))
		return mc["playQueueTotalCount"].(float64) == 3 && mc["playQueueSelectedItemOffset"].(float64) == 2 &&
			int64(mc["playQueueSelectedItemID"].(float64)) == second
	})
	// Item IDs stay unique across nodes.
	seen := map[float64]bool{}
	for _, it := range mc["Metadata"].([]any) {
		id := it.(map[string]any)["playQueueItemID"].(float64)
		if seen[id] {
			t.Fatalf("duplicate item id %v", id)
		}
		seen[id] = true
	}
	// Edits alternating between nodes never lose one.
	for i := 0; i < 4; i++ {
		f := a
		if i%2 == 1 {
			f = b
		}
		if rec := f.request(t, http.MethodPut, "/playQueues/"+id+"?uri="+urlEsc("/library/metadata/"+alien.Key.String()), nil); rec.Code != http.StatusOK {
			t.Fatalf("add %d: %d", i, rec.Code)
		}
	}
	mc = decodeJSON(t, b.get(t, "/playQueues/"+id, jsonAccept...))
	if mc["playQueueTotalCount"].(float64) != 7 || mc["playQueueVersion"].(float64) != 6 {
		t.Fatalf("after alternating edits: total=%v version=%v", mc["playQueueTotalCount"], mc["playQueueVersion"])
	}
	// Other users still cannot read it on either node.
	if rec := b.request(t, http.MethodGet, "/playQueues/"+id, map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusNotFound {
		t.Fatalf("friend read: %d", rec.Code)
	}
	// Expiry removes it cluster-wide.
	a.srv.queues.Expire(fixedNow.Add(7*time.Hour), 6*time.Hour)
	b.srv.queues.DropCache()
	if rec := b.get(t, "/playQueues/"+id); rec.Code != http.StatusNotFound {
		t.Fatalf("expired queue still served: %d", rec.Code)
	}
}

func TestClusterNotificationsReachOtherNodes(t *testing.T) {
	a, b, _ := twoServers(t)
	subA, _ := a.srv.notifier.subscribe(nil, &ownerUser)
	subB, _ := b.srv.notifier.subscribe(nil, &ownerUser)
	a.srv.WatchStateChanged(ownerUser.ID, fightClub.Key)
	select {
	case msg := <-subB.ch:
		if msg.typ != "timeline" || !strings.Contains(string(msg.data), fightClub.Key.String()) {
			t.Fatalf("B got %s %s", msg.typ, msg.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification never reached node B")
	}
	select {
	case <-subA.ch:
	case <-time.After(time.Second):
		t.Fatal("local subscriber missed the notification")
	}
	select {
	case msg := <-subA.ch:
		t.Fatalf("publisher delivered its own notification twice: %s", msg.data)
	case <-time.After(100 * time.Millisecond):
	}
	// Per-user filtering still applies to forwarded notifications.
	subFriend, _ := b.srv.notifier.subscribe(nil, &friendUser)
	a.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000", "X-Plex-Session-Identifier", "s1")
	select {
	case msg := <-subFriend.ch:
		t.Fatalf("friend received owner's playing notification: %s", msg.data)
	case <-time.After(200 * time.Millisecond):
	}
}

// A standalone server keeps sessions local while persisting its play queues.
func TestStandaloneServerKeepsSessionsLocalAndQueuesDurable(t *testing.T) {
	f := newFixture(func(d *Deps) { d.DB = store.OpenTest(t) })
	defer f.srv.Close()
	if f.srv.cl != nil || !f.srv.queues.Persistent() {
		t.Fatal("standalone session or durable queue wiring is incorrect")
	}
	uri := urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + breakingBad.Key.String())
	mc := decodeJSON(t, f.request(t, http.MethodPost, "/playQueues?type=video&uri="+uri, map[string]string{"Accept": "application/json"}))
	if mc["playQueueID"].(float64) != 1 {
		t.Fatalf("standalone queue ids changed: %v", mc["playQueueID"])
	}
	var n int
	if err := f.srv.d.DB.QueryRow(`SELECT COUNT(*) FROM plex_playqueues`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("standalone server did not persist queue: %d %v", n, err)
	}
}

// A client whose "stopped" timeline reaches another node (it switched
// connections) still ends its stream on the node serving it.
func TestClusterStoppedTimelineForwardedToServingNode(t *testing.T) {
	a, b, _ := twoServers(t)
	ps, _ := a.playback.Prepare(context.Background(), &friendUser, fightClub.Key, core.ClientCaps{ClientID: "client-1"})
	a.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	waitFor(t, "B sees the stream", func() bool { return len(sessionsOf(t, b)) == 1 })
	b.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=4000&duration=10000",
		"X-Plex-Session-Identifier", "sx", "X-Plex-Token", friendToken)
	waitFor(t, "A ends the stream", func() bool {
		_, live := a.playback.Session(ps.ID)
		return !live && len(a.srv.sessions.list()) == 0
	})
	waitFor(t, "gone everywhere", func() bool { return len(sessionsOf(t, b)) == 0 })
}
