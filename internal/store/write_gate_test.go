package store

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// A writer waiting behind another connection's transaction runs as soon as
// that transaction ends, instead of polling SQLite's lock with sleeps.
func TestWriteGateHandsOverPromptly(t *testing.T) {
	ctx := context.Background()
	db, err := OpenPath(ctx, filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE wg (id INTEGER PRIMARY KEY, who TEXT)`); err != nil {
		t.Fatal(err)
	}
	const hold = 30 * time.Millisecond
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO wg(who) VALUES('long')`); err != nil {
				t.Error(err)
			}
			time.Sleep(hold)
			if err := tx.Commit(); err != nil {
				t.Error(err)
			}
		}
	}()
	var waits []time.Duration
	for range 20 {
		start := time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO wg(who) VALUES('short')`); err != nil {
			t.Fatal(err)
		}
		waits = append(waits, time.Since(start))
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	if worst := waits[len(waits)-1]; worst > 3*hold {
		t.Fatalf("a short write waited %s behind %s transactions (waits %v)", worst, hold, waits)
	}
}

func TestWritesClassifiesStatements(t *testing.T) {
	for q, want := range map[string]bool{
		"INSERT INTO a VALUES(1)": true, "  update a set b=1": true, "DELETE FROM a": true, "REPLACE INTO a VALUES(1)": true,
		"WITH x AS (SELECT 1) INSERT INTO a SELECT * FROM x": true, "SELECT 1": false, "PRAGMA wal_checkpoint(PASSIVE)": false,
		"WITH x AS (SELECT 1) SELECT * FROM x": false, "CREATE INDEX i ON a(b)": true, "DROP INDEX IF EXISTS i": true, "ALTER TABLE a ADD COLUMN c INT": true,
	} {
		if got := writes(q); got != want {
			t.Errorf("writes(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestWriteGateExtendHeldQueuesWaiterAndHonorsContext(t *testing.T) {
	g := &writeGate{slot: make(chan struct{}, 1)}
	if err := g.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.extendHeld(time.Now().Add(300 * time.Millisecond))

	// A caller with its own short deadline still cancels promptly.
	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.acquire(shortCtx); err == nil {
		t.Fatal("expected context deadline error while gate held")
	}

	// Release clears pausedUntil and wakes queued waiters in order.
	done := make(chan error, 1)
	go func() {
		if err := g.acquire(context.Background()); err != nil {
			done <- err
			return
		}
		g.release()
		done <- nil
	}()
	time.Sleep(20 * time.Millisecond)
	g.release()
	if err := <-done; err != nil {
		t.Fatalf("queued waiter failed: %v", err)
	}
	if !g.pausedUntil.IsZero() {
		t.Fatalf("pausedUntil = %v after release, want zero", g.pausedUntil)
	}
}
