package catalog

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("catalog_plex_shelf_order", 1, `CREATE INDEX catalog_plex_shelf_order ON catalog_titles(type,released_ts DESC,key ASC); CREATE INDEX catalog_plex_show_order ON catalog_titles(show_key,key);`)
}

// RecentPage merges independently indexed movie and episode streams in SQL.
// Only the requested page is hydrated; totals use the catalogue's count cache.
func (c *Catalog) RecentPage(ctx context.Context, q core.BrowseQuery, kinds []core.MediaType) (core.Page, error) {
	var branches []string
	var args []any
	total := 0
	for _, kind := range kinds {
		f := q.Filter
		f.Type = kind
		f = c.viewerPaging(ctx, f)
		counted, err := c.Browse(ctx, core.BrowseQuery{Filter: f, Sort: core.SortAdded, Desc: true, Limit: 1})
		if err != nil {
			return core.Page{}, err
		}
		total += counted.Total
		var where string
		var a []any
		if kind == core.TypeEpisode {
			where, a, err = c.storedEpisodesWhere(f)
			branches = append(branches, `SELECT e.key AS key,e.released_ts AS stamp FROM catalog_titles e CROSS JOIN catalog_titles t WHERE t.key=e.show_key AND e.type='episode' AND `+where)
		} else {
			where, a, err = c.browseWhere(f)
			branches = append(branches, `SELECT t.key AS key,t.released_ts AS stamp FROM catalog_titles t WHERE `+where)
		}
		if err != nil {
			return core.Page{}, err
		}
		args = append(args, a...)
	}
	if len(branches) == 0 {
		return core.Page{}, nil
	}
	limit := min(max(q.Limit, 1), 10000)
	if q.Limit == 0 {
		limit = 0
	}
	args = append(args, limit, max(q.Offset, 0))
	keys := `SELECT key,stamp FROM (` + strings.Join(branches, ` UNION ALL `) + `) recent ORDER BY stamp DESC,key ASC LIMIT ? OFFSET ?`
	rows, err := c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM (`+keys+`) page JOIN catalog_titles t ON t.key=page.key ORDER BY page.stamp DESC,t.key ASC`, args...)
	return core.Page{Items: rows, Total: total}, err
}

// PlexOnDeckPage starts with the viewer's indexed watch rows, chooses one
// candidate per show, applies catalogue policy, and pages before hydration.
func (c *Catalog) PlexOnDeckPage(ctx context.Context, uid int64, offset, limit int, f core.Filter, episodesOnly bool) (core.WatchPage, error) {
	return c.OnDeckPageSince(ctx, uid, offset, limit, f, episodesOnly, time.Time{})
}

// OnDeckPageSince is PlexOnDeckPage within a window (PMS's
// OnDeckWindow): a show or movie last watched before since is left out,
// except a season premiere (episode 1 of a season after the first) that
// aired since then. A zero since keeps every row.
func (c *Catalog) OnDeckPageSince(ctx context.Context, uid int64, offset, limit int, f core.Filter, episodesOnly bool, since time.Time) (core.WatchPage, error) {
	if episodesOnly && f.Type == core.TypeMovie {
		return core.WatchPage{}, nil
	}
	movie := f
	movie.Type = core.TypeMovie
	movie = c.viewerPaging(ctx, movie)
	mw, ma, err := c.browseWhere(movie)
	if err != nil {
		return core.WatchPage{}, err
	}
	episode := f
	episode.Type = core.TypeEpisode
	episode = c.viewerPaging(ctx, episode)
	ew, ea, err := c.storedEpisodesWhere(episode)
	if err != nil {
		return core.WatchPage{}, err
	}
	// The viewer's watch rows are partitioned by entity: a movie is its own
	// key, an episode belongs to its show.
	const entityOf = `CASE WHEN w.rating_key>=4000000000000000 AND w.rating_key<5000000000000000 THEN 2000000000000000+(w.rating_key-4000000000000000)/10000000 ELSE w.rating_key END`
	const watchable = `((w.rating_key>=1000000000000000 AND w.rating_key<2000000000000000) OR (w.rating_key>=4000000000000000 AND w.rating_key<5000000000000000)) AND (w.view_offset_ns>0 OR w.view_count>0)`
	// An OFFSET keeps each EXISTS a per-row key probe on PostgreSQL, which
	// otherwise may hash it: read every browsable movie and every episode
	// of the catalogue once per request. SQLite probes by key either way and
	// needs the LIMIT to accept the OFFSET.
	visible := `(EXISTS(SELECT 1 FROM catalog_titles t WHERE t.key=w.rating_key AND ` + mw + ` LIMIT 1 OFFSET 0) OR EXISTS(SELECT 1 FROM catalog_titles e CROSS JOIN catalog_titles t WHERE e.key=w.rating_key AND t.key=e.show_key AND ` + ew + ` LIMIT 1 OFFSET 0))`
	visibleArgs := append(append([]any{}, ma...), ea...)
	var active string
	var args []any
	if since.IsZero() {
		active = `WITH active AS (
 SELECT w.*,` + entityOf + ` AS entity FROM state_watch w WHERE w.user_id=? AND ` + watchable + ` AND ` + visible + `
)`
		args = append([]any{uid}, visibleArgs...)
	} else {
		// Within the window only three kinds of entity can reach the deck:
		// one watched since the window opened (state_watch_last_viewed;
		// SQLite needs the literal last_viewed_at>0 to use that partial
		// index with a bound parameter),
		// one with an undated view (state_watch_undated), and a show with a
		// season premiere aired inside the window (the recent-episode
		// index). The ranking below then sees every watch row of those
		// entities, as the whole history would give it, while the
		// visibility probes (the expensive part) run only for them: the
		// materialized step keeps PostgreSQL from probing every row first.
		active = `WITH candidates AS MATERIALIZED (
 SELECT ` + entityOf + ` AS entity FROM state_watch w WHERE w.user_id=? AND w.last_viewed_at>0 AND w.last_viewed_at>=? AND ` + watchable + `
 UNION SELECT ` + entityOf + ` FROM state_watch w WHERE w.user_id=? AND w.last_viewed_at<=0 AND ` + watchable + `
 UNION SELECT 2000000000000000+(p.key-4000000000000000)/10000000 FROM catalog_titles p WHERE p.type='episode' AND p.release_known=1 AND p.released_ts>=? AND p.key%10000=1 AND (p.key/10000)%1000>1
), mine AS MATERIALIZED (
 SELECT w.*,` + entityOf + ` AS entity FROM state_watch w WHERE w.user_id=? AND ` + watchable + ` AND ` + entityOf + ` IN (SELECT entity FROM candidates)
), active AS (
 SELECT w.* FROM mine w WHERE ` + visible + `
)`
		args = append([]any{uid, since.UnixNano(), uid, since.Unix(), uid}, visibleArgs...)
	}
	const rest = `, ranked AS (
 SELECT active.*,ROW_NUMBER() OVER(PARTITION BY entity ORDER BY CASE WHEN view_offset_ns>0 AND view_count=0 THEN 0 ELSE 1 END,last_viewed_at DESC,rating_key DESC) AS n,
 MAX(last_viewed_at) OVER(PARTITION BY entity) AS latest
 FROM active
), chosen AS MATERIALIZED (
 SELECT CASE WHEN entity<2000000000000000 OR (view_offset_ns>0 AND view_count=0) THEN rating_key ELSE (
 SELECT e.key FROM catalog_titles e CROSS JOIN catalog_titles t WHERE t.key=e.show_key AND e.show_key=ranked.entity AND e.type='episode' AND e.season>0 AND e.episode>0 AND e.key>ranked.rating_key
 AND (e.released_ts=0 OR e.released_ts<=?)
 AND NOT EXISTS(SELECT 1 FROM state_watch watched WHERE watched.user_id=? AND watched.rating_key=e.key AND watched.view_count>0)
 AND /*nextVisible*/ ORDER BY e.key LIMIT 1) END AS key,last_viewed_at AS stamp,updated_at AS updated,entity,latest
 FROM ranked WHERE n=1 AND (entity>=2000000000000000 OR view_offset_ns>0)
 AND NOT EXISTS(SELECT 1 FROM state_continue_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)
 AND NOT EXISTS(SELECT 1 FROM personalize_discovery_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)
)`
	branches := []string{}
	args = append(args, c.now().Unix(), uid)
	args = append(args, ea...)
	args = append(args, uid)
	personalArgIndex := len(args)
	args = append(args, uid)
	head := active + rest
	deck := "chosen"
	if !since.IsZero() {
		// The show's latest view of any episode (a movie's own) dates it;
		// an undated view (an import) is not known to be old. A premiere is
		// probed by key only for rows outside the window.
		head += `, windowed AS MATERIALIZED (
 SELECT chosen.* FROM chosen WHERE chosen.latest>=? OR chosen.latest<=0
 OR EXISTS(SELECT 1 FROM catalog_titles p WHERE p.key=chosen.key AND p.type='episode' AND p.episode=1 AND p.season>1 AND p.released_ts>=? LIMIT 1 OFFSET 0)
)`
		args = append(args, since.UnixNano(), since.Unix())
		deck = "windowed"
	}
	if !episodesOnly && f.Type != core.TypeEpisode {
		branches = append(branches, `SELECT `+deck+`.key,`+deck+`.stamp,`+deck+`.updated FROM `+deck+` CROSS JOIN catalog_titles t WHERE t.key=`+deck+`.key AND `+mw)
		args = append(args, ma...)
	}
	if f.Type != core.TypeMovie {
		branches = append(branches, `SELECT `+deck+`.key,`+deck+`.stamp,`+deck+`.updated FROM `+deck+` CROSS JOIN catalog_titles e CROSS JOIN catalog_titles t WHERE e.key=`+deck+`.key AND t.key=e.show_key AND e.type='episode' AND `+ew)
		args = append(args, ea...)
	}
	prefix := head + `, qualified AS (` + strings.Join(branches, ` UNION ALL `) + `) `
	prefix = strings.Replace(prefix, "/*nextVisible*/", ew, 1)
	var personal bool
	schemaQuery := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='personalize_discovery_hidden')`
	if store.DialectOf(c.db) == store.Postgres {
		schemaQuery = `SELECT to_regclass('personalize_discovery_hidden') IS NOT NULL`
	}
	if err := c.db.QueryRowContext(ctx, schemaQuery).Scan(&personal); err != nil {
		return core.WatchPage{}, err
	}
	if !personal {
		prefix = strings.Replace(prefix, ` AND NOT EXISTS(SELECT 1 FROM personalize_discovery_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)`, "", 1)
		args = append(args[:personalArgIndex], args[personalArgIndex+1:]...)
	}
	// The CTE walks the viewer's watch rows and dominates the request, so
	// it runs once: the page's clocks carry the total (COUNT(*) OVER() is
	// taken before LIMIT), and the page is hydrated by key.
	countArgs := args
	args = append(append([]any{}, args...), min(max(limit, 0), 10000), max(offset, 0))
	clocks, err := c.db.QueryContext(ctx, prefix+`SELECT key,stamp,updated,COUNT(*) OVER() FROM qualified ORDER BY stamp DESC,key ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return core.WatchPage{}, err
	}
	total := -1
	var keys []any
	at := map[core.RatingKey]int{}
	anchors := map[core.RatingKey]core.WatchStatus{}
	for clocks.Next() {
		var key core.RatingKey
		var stamp, updated int64
		if err := clocks.Scan(&key, &stamp, &updated, &total); err != nil {
			clocks.Close()
			return core.WatchPage{}, err
		}
		anchor := core.WatchStatus{Key: key}
		if stamp > 0 {
			anchor.LastViewedAt = time.Unix(0, stamp)
		}
		if updated > 0 {
			anchor.UpdatedAt = time.Unix(0, updated)
		}
		anchors[key] = anchor
		at[key] = len(keys)
		keys = append(keys, key)
	}
	if err := clocks.Err(); err != nil {
		clocks.Close()
		return core.WatchPage{}, err
	}
	clocks.Close()
	if total < 0 {
		// An empty page (offset past the end, or no limit) has no row to
		// carry the total.
		if err := c.db.QueryRowContext(ctx, prefix+`SELECT COUNT(*) FROM qualified`, countArgs...).Scan(&total); err != nil {
			return core.WatchPage{}, err
		}
	}
	var rows []core.Title
	if len(keys) > 0 {
		rows, err = c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE t.key IN (`+placeholders(len(keys))+`)`, keys...)
		slices.SortFunc(rows, func(a, b core.Title) int { return at[a.Key] - at[b.Key] })
	}
	return core.WatchPage{Page: core.Page{Items: rows, Total: total}, Anchors: anchors}, err
}
