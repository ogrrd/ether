package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

// SQLite has one writer. Left to SQLite, connections that want to write at
// the same moment poll the lock with growing sleeps (up to 100 ms each), so
// a waiter can sleep well past the moment the lock came free, and a burst of
// writers starves whoever arrived first. writeGate makes this process's
// connections to one database file take turns in Go instead: a writer waits
// on a channel and runs as soon as the previous one finishes. Reads never
// wait on it. Another process writing the same file is still handled by
// SQLite's busy_timeout.
type writeGate struct {
	slot chan struct{}
	// pausedUntil, while a maintenance rewrite holds the gate (pause),
	// is how long a writer waits for it instead of writeGateWait.
	pausedUntil time.Time
	mu          sync.Mutex
	holder      string
	started     time.Time
}

// writeGateWait matches busy_timeout: a writer that waits longer fails the
// way SQLite would have.
const writeGateWait = 10 * time.Second

var errWriteGateTimeout = errors.New("database is locked (waited 10s for this process's other writer)")

// IsWriteContention reports whether err means another writer held the
// database (this process's write gate, SQLite's lock, or the caller's
// deadline while waiting), so the same write may succeed when retried.
func IsWriteContention(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errWriteGateTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY")
}

var (
	gatesMu sync.Mutex
	gates   = map[string]*writeGate{}
)

// gateFor returns the gate shared by every connection to the database a
// DSN names. In-memory databases are private to their connection and
// ungated.
func gateFor(dsn string) *writeGate {
	if strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory") {
		return nil
	}
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g := gates[path]
	if g == nil {
		g = &writeGate{slot: make(chan struct{}, 1)}
		gates[path] = g
	}
	return g
}

func (g *writeGate) acquire(ctx context.Context) error {
	start := time.Now()
	select {
	case g.slot <- struct{}{}:
		g.held()
		return nil
	default:
	}
	g.mu.Lock()
	prior, priorStart := g.holder, g.started
	wait := max(writeGateWait, time.Until(g.pausedUntil))
	g.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case g.slot <- struct{}{}:
			g.held()
			if time.Since(start) >= time.Second {
				slog.Warn("sqlite writer waited", "wait", time.Since(start), "holder_at_start", prior, "holder_age_at_start", start.Sub(priorStart))
			}
			return nil
		case <-ctx.Done():
			g.logHolder(time.Since(start))
			return ctx.Err()
		case <-t.C:
			g.mu.Lock()
			rem := time.Until(g.pausedUntil)
			g.mu.Unlock()
			if rem > 0 {
				t.Reset(rem)
				continue
			}
			g.logHolder(time.Since(start))
			return errWriteGateTimeout
		}
	}
}

// pause takes the gate for a whole-database rewrite (Compact) that SQLite
// runs under its own write lock: this process's writers then queue on the
// gate, in order, for up to until, instead of failing on busy_timeout
// after 10 s. release ends the pause.
func (g *writeGate) pause(ctx context.Context, until time.Time) (release func(), err error) {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.held()
	g.mu.Lock()
	g.pausedUntil = until
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.pausedUntil = time.Time{}
		g.mu.Unlock()
		g.release()
	}, nil
}

func (g *writeGate) held() {
	var pcs [32]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	label := "unknown"
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, "/ether/internal/") && !strings.Contains(frame.Function, "/ether/internal/store.") {
			label = frame.Function
			break
		}
		if !more {
			break
		}
	}
	g.mu.Lock()
	g.holder, g.started = label, time.Now()
	g.mu.Unlock()
}

func (g *writeGate) logHolder(wait time.Duration) {
	if wait < time.Second {
		return
	}
	g.mu.Lock()
	label, started := g.holder, g.started
	g.mu.Unlock()
	if !started.IsZero() {
		slog.Warn("sqlite writer blocked", "wait", wait, "holder", label, "held", time.Since(started))
	}
}

// extendHeld lets a known long maintenance transaction (CreateIndexAnalyzed,
// analyzeFull) that already holds the gate keep in-process writers queued up
// to until, subject to each caller's own context deadline, instead of failing
// at writeGateWait (10 s). release clears pausedUntil when the holder finishes.
func (g *writeGate) extendHeld(until time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
	g.mu.Unlock()
}

func (g *writeGate) release() {
	g.mu.Lock()
	label, started := g.holder, g.started
	g.holder, g.started = "", time.Time{}
	g.pausedUntil = time.Time{}
	g.mu.Unlock()
	<-g.slot
	elapsed := time.Since(started)
	if !started.IsZero() {
		writeGateHolds.add(elapsed)
	}
	if !started.IsZero() && elapsed >= time.Second {
		slog.Warn("sqlite writer held", "holder", label, "duration", elapsed)
	}
}

// WriteGateHolds is how often and how long this process's SQLite writers
// held the write gate, for benchmarks (etherperf syncbench). Buckets count
// holds under 1, 5, 20, 100 and 1000 ms and over.
type WriteGateHolds struct {
	Holds   int64
	Held    time.Duration
	Max     time.Duration
	Buckets [6]int64
}

var writeGateHolds gateHoldStats

type gateHoldStats struct {
	mu sync.Mutex
	s  WriteGateHolds
}

var gateHoldBounds = [5]time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond, time.Second}

func (g *gateHoldStats) add(d time.Duration) {
	b := len(gateHoldBounds)
	for i, bound := range gateHoldBounds {
		if d < bound {
			b = i
			break
		}
	}
	g.mu.Lock()
	g.s.Holds++
	g.s.Held += d
	g.s.Max = max(g.s.Max, d)
	g.s.Buckets[b]++
	g.mu.Unlock()
}

// ResetWriteGateHolds starts the counts again.
func ResetWriteGateHolds() {
	writeGateHolds.mu.Lock()
	writeGateHolds.s = WriteGateHolds{}
	writeGateHolds.mu.Unlock()
}

// ReadWriteGateHolds returns the write-gate holds since the last reset.
func ReadWriteGateHolds() WriteGateHolds {
	writeGateHolds.mu.Lock()
	defer writeGateHolds.mu.Unlock()
	return writeGateHolds.s
}

// writes reports whether a statement modifies the database (and so takes
// SQLite's write lock outside a transaction). PASSIVE checkpoints run beside
// writers. Our canonical TRUNCATE statement takes SQLite's writer lock, so it
// must use the same admission as writers; otherwise a writer can hold the Go
// gate while its underlying BEGIN fails behind an untracked checkpoint.
func writes(query string) bool {
	q := strings.TrimLeft(query, " \t\r\n(")
	if len(q) < 6 {
		return false
	}
	if strings.HasPrefix(strings.ToUpper(q[:5]), "DROP ") {
		return true
	}
	switch strings.ToUpper(q[:6]) {
	case "INSERT", "UPDATE", "DELETE", "REPLAC", "CREATE", "ANALYZ", "ALTER ":
		return true
	case "PRAGMA":
		return strings.EqualFold(strings.TrimSuffix(strings.Join(strings.Fields(q), " "), ";"), "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	// WITH ... INSERT/UPDATE/DELETE and the like.
	if strings.EqualFold(q[:4], "WITH") {
		u := strings.ToUpper(q)
		return strings.Contains(u, "INSERT ") || strings.Contains(u, "UPDATE ") || strings.Contains(u, "DELETE ")
	}
	return false
}

// gatedTx releases the gate when its transaction ends.
type gatedTx struct {
	driver.Tx
	conn *stmtConn
}

func (t gatedTx) Commit() error {
	defer t.conn.endTx()
	return t.Tx.Commit()
}

func (t gatedTx) Rollback() error {
	defer t.conn.endTx()
	return t.Tx.Rollback()
}

// gatedRows releases the gate when a writing query's rows are closed
// (INSERT ... RETURNING).
type gatedRows struct {
	rowsExtras
	release func()
	once    sync.Once
}

func (r *gatedRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(r.release)
	return err
}
