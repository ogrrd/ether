package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Planner statistics.
//
// SQLite plans from sqlite_stat1. PRAGMA optimize keeps it current, but with
// its default mask it runs ANALYZE under a temporary analysis_limit of 2000
// rows per index, cut to as few as 100 when it checks many tables. A sampled
// ANALYZE reads only the start of each index, so a low-cardinality leading
// column looks almost unique. On a 540k-title catalogue every type-leading
// index of catalog_titles came out at 312 rows per type instead of 135,000.
// The planner then walked every episode through a type-leading index for
// per-show reads (Swiftfin recently added took 1.75 s instead of 52 ms, and
// series pages 12 s). The rank tables, genre pairs, credits and person names
// sample just as badly.
//
// Ether therefore never lets SQLite sample. Without that limit (mask
// 0x10002) optimize reads every row, but analyses every stale table in one
// write transaction. RefreshStatistics asks PRAGMA optimize, in its
// debugging mode, which tables are stale and analyses them itself, from
// every row, one index per statement.

func init() {
	// Servers upgraded from earlier releases carry sampled statistics:
	// from PRAGMA optimize in daily maintenance, and from the old title-tier
	// cleanup's analysis_limit=1000 (1001 rows per type on a 190k-title lab
	// catalogue). Their row counts still match, so optimize never calls
	// them stale. Replace them once with complete statistics.
	Register("planner_statistics", 1, `PRAGMA analysis_limit=0; ANALYZE;`)
	RegisterDialect("planner_statistics", 1, Postgres, `SELECT 1;`)
}

// RefreshStatistics gives complete planner statistics to every table whose
// statistics are stale, in SQLite's own terms: an index without statistics,
// or a row count ten times larger or smaller than they record. It returns
// the tables it analysed. Each index is analysed in its own statement, so
// the write lock is held for one index scan at a time, and every pooled
// connection plans with the new statistics from its next statement.
//
// PostgreSQL's autovacuum analyses tables itself (and Maintain runs ANALYZE);
// there it does nothing.
func RefreshStatistics(ctx context.Context, db *sql.DB) ([]string, error) {
	if DialectOf(db) == Postgres {
		return nil, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stale, err := staleTables(ctx, conn)
	if err != nil || len(stale) == 0 {
		return nil, err
	}
	started := time.Now()
	var analysed []string
	var longest time.Duration
	err = withFullAnalysis(ctx, conn, func() error {
		for _, t := range stale {
			took, err := analyzeFull(ctx, conn, t)
			if err != nil {
				return err
			}
			longest = max(longest, took)
			analysed = append(analysed, t)
		}
		return nil
	})
	if len(analysed) > 0 {
		if rerr := reloadStatistics(ctx, conn); err == nil {
			err = rerr
		}
		slog.Info("store: planner statistics refreshed", "tables", strings.Join(analysed, ","),
			"took", time.Since(started).Round(time.Millisecond), "longest_statement", longest.Round(time.Millisecond))
	}
	return analysed, err
}

// AnalyzeFull gives the named tables or indexes complete planner statistics,
// a table one index per statement, and has every pooled connection plan
// with them. Use it after a bulk change whose statistics must not wait for
// maintenance: a rebuilt index has none at all.
func AnalyzeFull(ctx context.Context, db *sql.DB, names ...string) error {
	if DialectOf(db) == Postgres || len(names) == 0 {
		return nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = withFullAnalysis(ctx, conn, func() error {
		for _, n := range names {
			if _, err := analyzeFull(ctx, conn, n); err != nil {
				return err
			}
		}
		return nil
	})
	if rerr := reloadStatistics(ctx, conn); err == nil {
		err = rerr
	}
	return err
}

func connGate(conn *sql.Conn) *writeGate {
	var gate *writeGate
	_ = conn.Raw(func(dc any) error {
		if c, ok := dc.(*stmtConn); ok {
			gate = c.gate
		}
		return nil
	})
	return gate
}

// CreateIndexAnalyzed builds an index with create and analyses it from
// every row in the same transaction. Other connections see the index and
// its statistics together, on one schema change. An index committed
// without statistics looks nearly unique to every connection's planner
// (SQLite assumes about ten rows per leading value) for as long as it
// stays that way: a type-leading index then wins "the latest episode of
// this show" over the show's own index and walks every episode, a row
// lookup each. The write lock is held for the build and one scan of the
// new index. On PostgreSQL it runs create alone.
func CreateIndexAnalyzed(ctx context.Context, db *sql.DB, name, create string) error {
	if DialectOf(db) == Postgres {
		_, err := db.ExecContext(ctx, create)
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	gate := connGate(conn)
	return withFullAnalysis(ctx, conn, func() error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		gate.extendHeld(time.Now().Add(2 * time.Minute))
		if _, err := tx.ExecContext(ctx, create); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `ANALYZE `+quoteIdent(name)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: analyze %s: %w", name, err)
		}
		return tx.Commit()
	})
}

// staleTables lists the tables PRAGMA optimize would analyse, checking
// every table (0x10000) rather than only those this connection used. The
// debugging bit (0x01) makes it name them instead of analysing them under
// its sampling limit. Empty tables with no row counts to correct are left
// out: ANALYZE records nothing for an empty table without partial indexes,
// so optimize names it every time.
func staleTables(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA optimize=0x10003`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return nil, err
		}
		if name, ok := optimizeTarget(line); ok {
			names = append(names, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(names) == 0 {
		return nil, err
	}
	var stat1 int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE name='sqlite_stat1'`).Scan(&stat1); err != nil {
		return nil, err
	}
	out := names[:0]
	for _, n := range names {
		var rows, counted int
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+quoteIdent(n)+`)`).Scan(&rows); err != nil {
			return nil, err
		}
		if rows == 0 && stat1 > 0 {
			if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE tbl=? AND CAST(stat AS INTEGER) > 0)`, n).Scan(&counted); err != nil {
				return nil, err
			}
		}
		if rows != 0 || counted != 0 {
			out = append(out, n)
		}
	}
	return out, nil
}

// optimizeTarget parses one line of PRAGMA optimize's debugging output,
// `ANALYZE "main"."table"`, into the table name.
func optimizeTarget(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, `ANALYZE "main".`)
	if !ok || len(rest) < 2 || rest[0] != '"' || rest[len(rest)-1] != '"' {
		return "", false
	}
	return strings.ReplaceAll(rest[1:len(rest)-1], `""`, `"`), true
}

// withFullAnalysis runs fn with conn's analysis_limit at 0 (every row) and
// restores the previous limit.
func withFullAnalysis(ctx context.Context, conn *sql.Conn, fn func() error) error {
	var limit int
	if err := conn.QueryRowContext(ctx, `PRAGMA analysis_limit`).Scan(&limit); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA analysis_limit=0`); err != nil {
		return err
	}
	err := fn()
	if limit != 0 {
		if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA analysis_limit=%d`, limit)); err == nil {
			err = rerr
		}
	}
	return err
}

// analyzeFull analyses one table or index on conn. A table with indexes is
// analysed one index per statement, which records the same statistics as
// ANALYZE of the table but holds the write lock for one index scan at a
// time. It returns the longest statement.
func analyzeFull(ctx context.Context, conn *sql.Conn, name string) (time.Duration, error) {
	targets := []string{name}
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='index' AND tbl_name=? AND EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?) ORDER BY name`, name, name)
	if err != nil {
		return 0, err
	}
	var idx []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		idx = append(idx, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(idx) > 0 {
		targets = idx
	}
	gate := connGate(conn)
	var longest time.Duration
	for _, t := range targets {
		began := time.Now()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return longest, err
		}
		gate.extendHeld(time.Now().Add(2 * time.Minute))
		if _, err := tx.ExecContext(ctx, `ANALYZE `+quoteIdent(t)); err != nil {
			tx.Rollback()
			return longest, fmt.Errorf("store: analyze %s: %w", t, err)
		}
		if err := tx.Commit(); err != nil {
			return longest, fmt.Errorf("store: analyze %s: %w", t, err)
		}
		longest = max(longest, time.Since(began))
	}
	return longest, nil
}

// reloadStatistics makes every connection load the statistics ANALYZE just
// wrote. ANALYZE reloads them only on its own connection; the others keep
// the statistics they read with the schema, for as long as the pool keeps
// them, and plan every new statement with those. A schema change makes each
// one reload the schema, statistics included, before its next statement. An
// empty view created and dropped in one transaction changes nothing else.
func reloadStatistics(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE VIEW store_statistics_reload AS SELECT 1`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DROP VIEW store_statistics_reload`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
