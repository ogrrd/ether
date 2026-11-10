package state

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The writer can be busy across completion, restart and a second completion.
// Persisting only the last position would lose viewed counts and history.
func TestPendingPlexPlaysKeepCompletionsAndLaterResume(t *testing.T) {
	s, now := newTestState(t)
	p := &pendingWrites{s: s, entries: map[progressKey]pendingWrite{}, wake: make(chan struct{}, 1)}
	s.pending = p
	defer func() { s.pending = nil }()
	ctx := WithOrigin(context.Background(), Origin{Surface: "plex", ClientID: "one"})
	key := core.MovieKey(88)
	for _, offset := range []time.Duration{3 * time.Second, 920 * time.Second, 70 * time.Second, 950 * time.Second, 80 * time.Second} {
		*now = now.Add(time.Second)
		if _, err := s.StoreTimeline(ctx, 1, key, offset, 1000*time.Second, "playing", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.Get(ctx, 1, key)
	if err != nil || pending.ViewCount != 2 || pending.ViewOffset != 80*time.Second {
		t.Fatalf("pending view: %+v %v", pending, err)
	}
	k, w, ok := p.next()
	if !ok {
		t.Fatal("no pending work")
	}
	if err := p.apply(ctx, k, w); err != nil {
		t.Fatal(err)
	}
	p.remove(k, w.seq)
	final, err := s.Get(ctx, 1, key)
	if err != nil || final.ViewCount != 2 || final.ViewOffset != 80*time.Second {
		t.Fatalf("stored view: %+v %v", final, err)
	}
	rows, err := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 0, Limit: 20})
	if err != nil || len(rows) != 2 {
		t.Fatalf("completion history: %+v %v", rows, err)
	}
}
