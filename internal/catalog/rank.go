package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// The popularity rank. TMDB moves almost every title's popularity every
// day, and an index ordered by popularity moves each changed title's entry
// to a random leaf: a daily refresh of a 127k-title library rewrote 5 GB of
// WAL through the nine catalog_titles indexes that ordered by popularity,
// and 61 MB without them.
//
// Popularity order therefore lives in a narrow rank table, one row per
// movie and show: the columns browse filters on and rk, the title's
// popularity negated as of the last rebuild, so that rk ascending, then
// key, is most popular first with a deterministic tie order. There are two
// copies, catalog_rank_a and catalog_rank_b, and the view catalog_rank
// names the live one. A daily rebuild writes the idle copy in key order,
// builds its indexes in bulk and swaps the view in the same transaction,
// so a reader sees one copy or the other, never a mix.
//
// Triggers keep the live copy current for every write that is not a
// popularity change: a new title gets its rank row (rk from its
// popularity) as it is stored, so it appears in popularity order at once;
// a title whose type, browsability, adult flag, release, year, language,
// runtime or status changes has its row updated; a deleted title loses
// it. A popularity change alone writes nothing here: the next rebuild
// picks it up. The idle copy is rebuilt from scratch, so it is never
// maintained.

const rankColumns = `key, type, browsable, adult, release_known, released_ts, year, language, runtime_sec, status, rk`

func rankTableSQL(side string, bigint string) string {
	t := "catalog_rank_" + side
	return `CREATE TABLE ` + t + ` (key ` + bigint + ` PRIMARY KEY, type TEXT NOT NULL, browsable INTEGER NOT NULL, adult INTEGER NOT NULL,
 release_known INTEGER NOT NULL, released_ts ` + bigint + ` NOT NULL, year INTEGER NOT NULL, language TEXT NOT NULL,
 runtime_sec INTEGER NOT NULL, status TEXT NOT NULL, rk REAL NOT NULL);`
}

// rankIndexSQL is a copy's indexes: popularity order alone and within a
// year, language, runtime or status, each covering the released check, and
// across browsable and non-browsable titles (upcoming-movie listings and
// search).
func rankIndexSQL(side string) string {
	t := "catalog_rank_" + side
	return `CREATE INDEX ` + t + `_pop ON ` + t + `(type, browsable, adult, rk, key, release_known, released_ts);
CREATE INDEX ` + t + `_year ON ` + t + `(type, browsable, adult, year, rk);
CREATE INDEX ` + t + `_lang ON ` + t + `(type, browsable, adult, language, rk);
CREATE INDEX ` + t + `_runtime ON ` + t + `(type, browsable, adult, runtime_sec, rk);
CREATE INDEX ` + t + `_status ON ` + t + `(type, browsable, adult, status, rk);
CREATE INDEX ` + t + `_all ON ` + t + `(type, adult, rk);`
}

const rankFill = `SELECT key, type, browsable, adult, release_known, released_ts, year, language, runtime_sec, status, -popularity FROM catalog_titles WHERE type IN ('movie','show')`

// rankUpsert writes src's rank row into one copy, when that copy is the
// live one and src is a movie or show, keeping its rk (a popularity change
// waits for the rebuild). The idle copy is rebuilt from scratch, so it is
// never maintained; while it is being rebuilt, changed keys go to
// catalog_rank_pending (rankPending) and the rebuild applies them before
// the swap. A writer never waits on the copy being rebuilt.
func rankUpsert(side, src string) string {
	t := "catalog_rank_" + side
	return `INSERT INTO ` + t + `(` + rankColumns + `) SELECT ` + src + `.key, ` + src + `.type, ` + src + `.browsable, ` + src + `.adult,
 CASE WHEN ` + src + `.released_ts<>0 THEN 1 ELSE 0 END, ` + src + `.released_ts, ` + src + `.year, ` + src + `.language, ` + src + `.runtime_sec, ` + src + `.status, -` + src + `.popularity
 WHERE ` + src + `.type IN ('movie','show') AND EXISTS (SELECT 1 FROM catalog_meta WHERE k='rank_side' AND v='` + side + `')
 ON CONFLICT(key) DO UPDATE SET type=excluded.type, browsable=excluded.browsable, adult=excluded.adult, release_known=excluded.release_known,
 released_ts=excluded.released_ts, year=excluded.year, language=excluded.language, runtime_sec=excluded.runtime_sec, status=excluded.status;`
}

func rankDelete(side, src string) string {
	return `DELETE FROM catalog_rank_` + side + ` WHERE key=` + src + `.key AND EXISTS (SELECT 1 FROM catalog_meta WHERE k='rank_side' AND v='` + side + `');`
}

// rankPending records src's key while a copy is being rebuilt.
func rankPending(src string) string {
	return `INSERT INTO catalog_rank_pending(key) SELECT ` + src + `.key WHERE EXISTS (SELECT 1 FROM catalog_meta WHERE k='rank_building' AND v<>'') ON CONFLICT(key) DO NOTHING;`
}

// rankWatched is every catalog_titles column a rank row copies, popularity
// excepted.
const rankWatched = `type, browsable, adult, released_ts, year, language, runtime_sec, status`

func init() {
	sqlite := rankTableSQL("a", "INTEGER") + rankIndexSQL("a") + rankTableSQL("b", "INTEGER") + rankIndexSQL("b") +
		`CREATE TABLE catalog_rank_pending (key INTEGER PRIMARY KEY);
CREATE VIEW catalog_rank AS SELECT ` + rankColumns + ` FROM catalog_rank_a;
CREATE TRIGGER catalog_rank_insert AFTER INSERT ON catalog_titles WHEN new.type IN ('movie','show') BEGIN
 ` + rankUpsert("a", "new") + `
 ` + rankUpsert("b", "new") + `
 ` + rankPending("new") + `
END;
CREATE TRIGGER catalog_rank_update AFTER UPDATE OF ` + rankWatched + ` ON catalog_titles WHEN new.type IN ('movie','show') OR old.type IN ('movie','show') BEGIN
 DELETE FROM catalog_rank_a WHERE key=old.key AND new.type NOT IN ('movie','show') AND EXISTS (SELECT 1 FROM catalog_meta WHERE k='rank_side' AND v='a');
 DELETE FROM catalog_rank_b WHERE key=old.key AND new.type NOT IN ('movie','show') AND EXISTS (SELECT 1 FROM catalog_meta WHERE k='rank_side' AND v='b');
 ` + rankUpsert("a", "new") + `
 ` + rankUpsert("b", "new") + `
 ` + rankPending("new") + `
END;
CREATE TRIGGER catalog_rank_delete AFTER DELETE ON catalog_titles WHEN old.type IN ('movie','show') BEGIN
 ` + rankDelete("a", "old") + `
 ` + rankDelete("b", "old") + `
 ` + rankPending("old") + `
END;
INSERT INTO catalog_meta(k,v) VALUES('rank_side','a'),('rank_building','') ON CONFLICT(k) DO UPDATE SET v=excluded.v;`
	store.Register("catalog", 53, sqlite)
	// A deep popularity page walks _pop in (rk, key) order, the order pages
	// are cut in. Without the key the planner walked _all instead and read
	// each rank row it passed: 5,000 pages for a page 10,000 deep against
	// 115. Rebuilds recreate the idle copy from rankIndexSQL; this moves
	// both copies now.
	var popKey []string
	for _, side := range []string{"a", "b"} {
		t := "catalog_rank_" + side
		popKey = append(popKey, `DROP INDEX IF EXISTS `+t+`_pop`, `CREATE INDEX `+t+`_pop ON `+t+`(type, browsable, adult, rk, key, release_known, released_ts)`)
	}
	store.Register("catalog", 57, strings.Join(popKey, ";\n")+";")
	// PostgreSQL: one row trigger function writes the live copy, and records
	// the key while the other is being rebuilt.
	pgUpsert := func(side string) string {
		t := "catalog_rank_" + side
		return `IF live='` + side + `' THEN
   INSERT INTO ` + t + `(` + rankColumns + `) VALUES (NEW.key, NEW.type, NEW.browsable, NEW.adult,
   CASE WHEN NEW.released_ts<>0 THEN 1 ELSE 0 END, NEW.released_ts, NEW.year, NEW.language, NEW.runtime_sec, NEW.status, -NEW.popularity)
   ON CONFLICT(key) DO UPDATE SET type=excluded.type, browsable=excluded.browsable, adult=excluded.adult, release_known=excluded.release_known,
   released_ts=excluded.released_ts, year=excluded.year, language=excluded.language, runtime_sec=excluded.runtime_sec, status=excluded.status;
  END IF;`
	}
	store.RegisterDialect("catalog", 53, store.Postgres, rankTableSQL("a", "BIGINT")+rankIndexSQL("a")+rankTableSQL("b", "BIGINT")+rankIndexSQL("b")+
		`CREATE TABLE catalog_rank_pending (key BIGINT PRIMARY KEY);
CREATE VIEW catalog_rank AS SELECT `+rankColumns+` FROM catalog_rank_a;
CREATE FUNCTION catalog_rank_fn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE building TEXT; live TEXT;
BEGIN
 SELECT v INTO building FROM catalog_meta WHERE k='rank_building';
 building := COALESCE(building, '');
 SELECT v INTO live FROM catalog_meta WHERE k='rank_side';
 live := COALESCE(live, 'a');
 IF building<>'' THEN
  INSERT INTO catalog_rank_pending(key) VALUES (CASE WHEN TG_OP='DELETE' THEN OLD.key ELSE NEW.key END) ON CONFLICT(key) DO NOTHING;
 END IF;
 IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND NEW.type NOT IN ('movie','show')) THEN
  IF live='a' THEN DELETE FROM catalog_rank_a WHERE key=OLD.key; ELSE DELETE FROM catalog_rank_b WHERE key=OLD.key; END IF;
 END IF;
 IF TG_OP<>'DELETE' AND NEW.type IN ('movie','show') THEN
  `+pgUpsert("a")+`
  `+pgUpsert("b")+`
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_rank_insert AFTER INSERT ON catalog_titles FOR EACH ROW WHEN (NEW.type IN ('movie','show')) EXECUTE FUNCTION catalog_rank_fn();
CREATE TRIGGER catalog_rank_update AFTER UPDATE OF `+rankWatched+` ON catalog_titles FOR EACH ROW WHEN (NEW.type IN ('movie','show') OR OLD.type IN ('movie','show')) EXECUTE FUNCTION catalog_rank_fn();
CREATE TRIGGER catalog_rank_delete AFTER DELETE ON catalog_titles FOR EACH ROW WHEN (OLD.type IN ('movie','show')) EXECUTE FUNCTION catalog_rank_fn();
INSERT INTO catalog_meta(k,v) VALUES('rank_side','a'),('rank_building','') ON CONFLICT(k) DO UPDATE SET v=excluded.v;`)
}

// rankRebuildEvery spaces the rank rebuilds: TMDB publishes popularity
// once a day.
const rankRebuildEvery = 24 * time.Hour

// RankRebuild is what one rebuild did.
type RankRebuild struct {
	Side  string
	Rows  int64
	Took  time.Duration
	Built time.Time
}

// rankBuildHook, when set (tests), runs while the idle copy is being
// built, as a concurrent writer would.
var rankBuildHook func()

// rankChunk is how many titles one fill transaction copies: a writer
// waits for at most one chunk.
const rankChunk = 5000

// RebuildRank refreshes every title's rank from its current popularity:
// it rebuilds the idle copy and makes it the live one. Writers never wait
// on it for longer than one short transaction: while the copy is being
// built the triggers leave it alone and record changed keys in
// catalog_rank_pending; the copy is filled in key-ordered chunks of their
// own transactions, its indexes built one at a time, and the pending keys
// applied in the short transaction that swaps the view.
func (c *Catalog) RebuildRank(ctx context.Context) (RankRebuild, error) {
	c.schemaWorkMu.Lock()
	defer c.schemaWorkMu.Unlock()
	if c.tierSchemaPending(ctx) {
		return RankRebuild{}, fmt.Errorf("catalog: rank rebuild deferred until tier copy/swap completes")
	}
	return c.rebuildRank(ctx)
}

// rebuildRank requires schemaWorkMu. Tier migration also calls it after
// cleanup, before reporting completion, to replace its invalidated rank.
func (c *Catalog) rebuildRank(ctx context.Context) (RankRebuild, error) {
	start := c.now()
	began := time.Now()
	var out RankRebuild
	pg := store.DialectOf(c.db) == store.Postgres
	exec := func(stmts ...string) error {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("catalog: rank rebuild: %s: %w", strings.Fields(s)[0], err)
			}
		}
		return tx.Commit()
	}
	live := "a"
	var v string
	if err := c.db.QueryRowContext(ctx, `SELECT v FROM catalog_meta WHERE k='rank_side'`).Scan(&v); err == nil && v == "b" {
		live = "b"
	}
	idle := map[string]string{"a": "b", "b": "a"}[live]
	t := "catalog_rank_" + idle
	bigint := "INTEGER"
	if pg {
		bigint = "BIGINT"
	}
	if pg {
		// One rebuild at a time across a cluster's nodes.
		conn, err := c.db.Conn(ctx)
		if err != nil {
			return out, err
		}
		defer conn.Close()
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext('ether:catalog-rank-rebuild'))`).Scan(&got); err != nil {
			return out, err
		}
		if !got {
			return out, fmt.Errorf("catalog: rank rebuild already running")
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('ether:catalog-rank-rebuild'))`)
	}
	// From here the triggers leave the idle copy alone.
	if err := exec(`DELETE FROM catalog_rank_pending`, `UPDATE catalog_meta SET v='`+idle+`' WHERE k='rank_building'`); err != nil {
		return out, err
	}
	abandon := func(err error) (RankRebuild, error) {
		// The idle copy is incomplete; the next rebuild starts it again.
		_ = exec(`UPDATE catalog_meta SET v='' WHERE k='rank_building'`)
		return out, err
	}
	if err := exec(`DROP TABLE IF EXISTS `+t, strings.TrimSuffix(rankTableSQL(idle, bigint), ";")); err != nil {
		return abandon(err)
	}
	last := int64(-1 << 62)
	for {
		var next sql.NullInt64
		if err := c.db.QueryRowContext(ctx, `SELECT MAX(key) FROM (SELECT key FROM catalog_titles WHERE type IN ('movie','show') AND key>? ORDER BY key LIMIT ?) s`, last, rankChunk).Scan(&next); err != nil {
			return abandon(err)
		}
		if !next.Valid {
			break
		}
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return abandon(err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO `+t+`(`+rankColumns+`) `+rankFill+` AND key>? AND key<=? ORDER BY key`, last, next.Int64)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			return abandon(fmt.Errorf("catalog: rank fill: %w", err))
		}
		n, _ := res.RowsAffected()
		out.Rows += n
		last = next.Int64
	}
	if rankBuildHook != nil {
		rankBuildHook()
	}
	for _, ix := range strings.Split(strings.TrimSuffix(rankIndexSQL(idle), ";"), ";\n") {
		if pg {
			if err := exec(ix); err != nil {
				return abandon(err)
			}
			continue
		}
		if err := store.CreateIndexAnalyzed(ctx, c.db, strings.Fields(ix)[2], ix); err != nil {
			return abandon(fmt.Errorf("catalog: rank rebuild: %w", err))
		}
	}
	if pg {
		if err := exec(`ANALYZE ` + t); err != nil {
			return abandon(err)
		}
	}
	// Apply what changed during the build and swap, in one short
	// transaction. On PostgreSQL a writer whose statement began before
	// this commits may still record a key; the passes after it apply
	// those to the now-live copy (which the triggers already write).
	catchUp := []string{
		`DELETE FROM ` + t + ` WHERE key IN (SELECT key FROM catalog_rank_pending)`,
		`INSERT INTO ` + t + `(` + rankColumns + `) ` + rankFill + ` AND key IN (SELECT key FROM catalog_rank_pending)`,
		`DELETE FROM catalog_rank_pending`,
	}
	finish := []string{`UPDATE catalog_meta SET v='' WHERE k='rank_building'`, `UPDATE catalog_meta SET v='` + idle + `' WHERE k='rank_side'`,
		`INSERT INTO catalog_meta(k,v) VALUES('rank_built','` + fmt.Sprint(start.Unix()) + `') ON CONFLICT(k) DO UPDATE SET v=excluded.v`}
	if !pg {
		// One short transaction: pending keys, the live copy and the view
		// change together; writers take turns with it.
		swap := append(append(append([]string{}, catchUp...), `DROP VIEW IF EXISTS catalog_rank`, `CREATE VIEW catalog_rank AS SELECT `+rankColumns+` FROM `+t), finish...)
		if err := exec(swap...); err != nil {
			return abandon(err)
		}
	} else {
		// Apply most pending keys first, then swap the view, the live copy
		// and the rest of the pending keys in one short transaction.
		if err := exec(catchUp...); err != nil {
			return abandon(err)
		}
		if err := c.swapRankViewPG(ctx, t, append(append([]string{}, catchUp...), finish...)); err != nil {
			return abandon(err)
		}
		// A writer whose statement began before that commit recorded its
		// key and wrote the old copy: apply those to the new one.
		for range 3 {
			if err := exec(catchUp...); err != nil {
				return out, err
			}
		}
	}
	c.rankReady.Store(true)
	c.writeGen.Add(1)
	out.Side, out.Took, out.Built = idle, time.Since(began), start
	return out, nil
}

// rankDue reports whether the rank has not been rebuilt within
// rankRebuildEvery (or ever).
func (c *Catalog) rankDue(ctx context.Context) bool {
	var v string
	var at int64
	if err := c.db.QueryRowContext(ctx, `SELECT v FROM catalog_meta WHERE k='rank_built'`).Scan(&v); err != nil {
		return true
	}
	if _, err := fmt.Sscan(v, &at); err != nil {
		return true
	}
	return c.now().Sub(time.Unix(at, 0)) >= rankRebuildEvery
}

// rankLoop rebuilds the rank when it is due: at start on a database that
// never had one built (an upgrade fills it here, outside the migration
// transaction), then daily.
func (c *Catalog) rankLoop(ctx context.Context) {
	for {
		if c.rankDue(ctx) {
			if r, err := c.RebuildRank(ctx); err != nil {
				if ctx.Err() == nil {
					c.log.Warn("catalog: rebuild popularity rank", "err", err)
				}
			} else {
				c.log.Info("catalog: popularity rank rebuilt", "titles", r.Rows, "took", r.Took.Round(time.Millisecond))
			}
		}
		if err := c.swapTitleIndexes(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("catalog: replace popularity indexes", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// loadRankReady reads whether a rank has ever been built.
func (c *Catalog) loadRankReady(ctx context.Context) {
	var v string
	if err := c.db.QueryRowContext(ctx, `SELECT v FROM catalog_meta WHERE k='rank_built'`).Scan(&v); err == nil {
		c.rankReady.Store(true)
	} else if err != sql.ErrNoRows {
		c.log.Debug("catalog: read rank state", "err", err)
	}
}

func (c *Catalog) swapRankViewPG(ctx context.Context, table string, with []string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = func() error {
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, s := range append([]string{`SET LOCAL lock_timeout = '2s'`, `CREATE OR REPLACE VIEW catalog_rank AS SELECT ` + rankColumns + ` FROM ` + table}, with...) {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					return err
				}
			}
			return tx.Commit()
		}()
		if err == nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return fmt.Errorf("catalog: swap rank view: %w", err)
}
