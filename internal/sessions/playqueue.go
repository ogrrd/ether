package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"sync"
	"time"
)

const playQueueDBTimeout = 5 * time.Second

type PlayQueues struct {
	mu     sync.Mutex
	nextID int64
	nextIt int64
	queues map[int64]*PlayQueue  // standalone: the queues; clustered: a cache
	cl     *PlayQueuePersistence // nil on a standalone server
	now    func() time.Time
}

// queueCacheTTL bounds how long a clustered node trusts its cached copy of
// a queue without re-reading it (bus invalidations normally refresh it
// sooner).
const queueCacheTTL = 30 * time.Second

// maxQueueAttempts bounds optimistic retries of one clustered edit.
const maxQueueAttempts = 6

func NewPlayQueues() *PlayQueues {
	return &PlayQueues{nextID: 1, nextIt: 1, queues: map[int64]*PlayQueue{}, now: time.Now}
}

func (s *PlayQueues) newItemsLocked(keys []core.RatingKey) []QueueItem {
	out := make([]QueueItem, len(keys))
	for i, k := range keys {
		out[i] = QueueItem{ID: s.nextIt, Key: k}
		s.nextIt++
	}
	return out
}

// Preallocate reserves item IDs for keys before an edit on a clustered
// node (nil on a standalone server, which numbers items inside the edit).
func (s *PlayQueues) Preallocate(keys []core.RatingKey) ([]QueueItem, error) {
	if s.cl == nil || len(keys) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
	defer cancel()
	first, err := s.cl.Counter(ctx, "playqueue_item", len(keys))
	if err != nil {
		return nil, err
	}
	out := make([]QueueItem, len(keys))
	for i, k := range keys {
		out[i] = QueueItem{ID: first + int64(i), Key: k}
	}
	return out, nil
}

// CreateWith creates a queue of keys for the user's client, applies fn to it
// and returns a snapshot.
func (s *PlayQueues) CreateWith(userID int64, clientID string, keys []core.RatingKey, now time.Time, fn func(q *PlayQueue)) (PlayQueue, error) {
	if s.cl != nil {
		return s.createShared(userID, clientID, keys, now, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireClientLocked(userID, clientID, 0)
	if len(s.queues) >= MaxPlayQueues {
		s.evictOldestLocked()
	}
	q := &PlayQueue{ID: s.nextID, UserID: userID, ClientID: clientID, Items: s.newItemsLocked(keys), Version: 1, UpdatedAt: now}
	s.nextID++
	s.queues[q.ID] = q
	if fn != nil {
		fn(q)
	}
	return q.Snapshot(), nil
}

// A client owns one active queue; an absent identifier is still an identity.
func (s *PlayQueues) retireClientLocked(userID int64, clientID string, keep int64) {
	for id, q := range s.queues {
		if id != keep && q.UserID == userID && q.ClientID == clientID {
			delete(s.queues, id)
		}
	}
}

func (s *PlayQueues) evictOldestLocked() {
	var oldest *PlayQueue
	for _, q := range s.queues {
		if oldest == nil || q.UpdatedAt.Before(oldest.UpdatedAt) ||
			(q.UpdatedAt.Equal(oldest.UpdatedAt) && q.ID < oldest.ID) {
			oldest = q
		}
	}
	if oldest != nil {
		delete(s.queues, oldest.ID)
	}
}

// With runs fn on the queue and returns a snapshot (fn nil: read only).
func (s *PlayQueues) With(id, userID int64, now time.Time, fn func(q *PlayQueue)) (PlayQueue, bool) {
	if s.cl != nil {
		return s.withShared(id, userID, now, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok || q.UserID != userID {
		return PlayQueue{}, false
	}
	if fn != nil {
		fn(q)
		q.UpdatedAt = now
	}
	return q.Snapshot(), true
}

func (s *PlayQueues) Get(id, userID int64) (PlayQueue, bool) {
	return s.With(id, userID, time.Time{}, nil)
}

// SelectItem marks the item a client reports playing (timeline).
func (s *PlayQueues) SelectItem(userID, itemID int64) {
	if s.cl != nil {
		s.selectShared(userID, itemID)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queues {
		if q.UserID != userID {
			continue
		}
		for i, it := range q.Items {
			if it.ID == itemID {
				q.Selected = i
				q.UpdatedAt = s.now()
				return
			}
		}
	}
}

func (s *PlayQueues) Expire(now time.Time, ttl time.Duration) {
	s.mu.Lock()
	for id, q := range s.queues {
		if now.Sub(q.UpdatedAt) > ttl {
			delete(s.queues, id)
		}
	}
	s.mu.Unlock()
	if s.cl != nil {
		// Idempotent on every node; no leader needed.
		ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
		defer cancel()
		if _, err := s.cl.DB.ExecContext(ctx, `DELETE FROM plex_playqueues WHERE updated_at<?`, now.Add(-ttl).UnixMilli()); err != nil {
			s.cl.ExpireResult(err)
		} else {
			s.cl.ExpireResult(nil)
		}
	}
}

// AddItems appends items for keys (or inserts them after the selected
// item). pre holds IDs preallocated on a clustered node; otherwise the
// caller holds s.mu (it runs inside With) and IDs are numbered here.
func (s *PlayQueues) AddItems(q *PlayQueue, keys []core.RatingKey, pre []QueueItem, next bool) {
	items := pre
	if items == nil {
		items = s.newItemsLocked(keys)
	}
	if len(items) == 0 {
		return // a no-op must not invalidate the client's queue version
	}
	items = append([]QueueItem(nil), items...)
	for i := range items {
		items[i].Added = true
	}
	at := min(q.Selected+1, len(q.Items))
	if !next {
		for at < len(q.Items) && q.Items[at].Added {
			at++
		}
		q.LastAdded = items[len(items)-1].ID
	}
	q.Items = append(q.Items[:at], append(items, q.Items[at:]...)...)
	if q.Shuffled {
		q.Original = append(q.Original, items...)
	}
	q.Version++
}

// Forget drops a cached queue another node changed.
func (s *PlayQueues) Forget(id int64) {
	s.mu.Lock()
	delete(s.queues, id)
	s.mu.Unlock()
}

// DropCache forgets every cached queue (bus resync).
func (s *PlayQueues) DropCache() {
	if s.cl == nil {
		return
	}
	s.mu.Lock()
	s.queues = map[int64]*PlayQueue{}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Clustered storage

func EncodeQueue(q *PlayQueue) (string, error) {
	return frontend.EncodeQueue(q)
}

func DecodeQueue(id, userID, rev, updated int64, data string) (*PlayQueue, error) {
	return frontend.DecodeQueue(id, userID, rev, updated, data)
}

func (s *PlayQueues) cache(q *PlayQueue) {
	cp := q.Snapshot()
	cp.LoadedAt = s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queues[cp.ID]; !ok && len(s.queues) >= MaxPlayQueues {
		s.evictOldestLocked()
	}
	s.queues[cp.ID] = &cp
}

func (s *PlayQueues) cached(id int64) (*PlayQueue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok || s.now().Sub(q.LoadedAt) > queueCacheTTL {
		return nil, false
	}
	cp := q.Snapshot()
	return &cp, true
}

func (s *PlayQueues) load(ctx context.Context, id int64) (*PlayQueue, error) {
	var userID, rev, updated int64
	var data string
	if err := s.cl.DB.QueryRowContext(ctx, `SELECT user_id, rev, data, updated_at FROM plex_playqueues WHERE id=?`, id).
		Scan(&userID, &rev, &data, &updated); err != nil {
		return nil, queueRowError(err)
	}
	q, err := DecodeQueue(id, userID, rev, updated, data)
	if err != nil {
		return nil, err
	}
	s.cache(q)
	return q, nil
}

func (s *PlayQueues) createShared(userID int64, clientID string, keys []core.RatingKey, now time.Time, fn func(q *PlayQueue)) (PlayQueue, error) {
	items, err := s.Preallocate(keys)
	if err != nil {
		return PlayQueue{}, err
	}
	q := &PlayQueue{UserID: userID, ClientID: clientID, Items: items, Version: 1, UpdatedAt: now, Rev: 1}
	if fn != nil {
		fn(q)
	}
	data, err := EncodeQueue(q)
	if err != nil {
		return PlayQueue{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
	defer cancel()
	tx, err := s.cl.DB.BeginTx(ctx, nil)
	if err != nil {
		return PlayQueue{}, err
	}
	defer tx.Rollback()
	// Lock a common queue row before deciding which client queue survives.
	// This works With SQLite and PostgreSQL and closes concurrent-create races.
	if _, err = tx.ExecContext(ctx, `UPDATE plex_counters SET value=value WHERE name='playqueue_item'`); err != nil {
		return PlayQueue{}, err
	}
	if err = tx.QueryRowContext(ctx, `INSERT INTO plex_playqueues(user_id, rev, data, updated_at) VALUES (?, ?, ?, ?) RETURNING id`, userID, q.Rev, data, now.UnixMilli()).Scan(&q.ID); err != nil {
		return PlayQueue{}, err
	}
	retired, err := retireQueueClient(ctx, tx, userID, clientID, q.ID)
	if err != nil {
		return PlayQueue{}, err
	}
	if err = tx.Commit(); err != nil {
		return PlayQueue{}, err
	}
	for _, id := range retired {
		s.Forget(id)
		s.cl.Invalidate(id)
	}
	s.cache(q)
	return q.Snapshot(), nil
}

func (s *PlayQueues) withShared(id, userID int64, now time.Time, fn func(q *PlayQueue)) (PlayQueue, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*playQueueDBTimeout)
	defer cancel()
	for attempt := 0; attempt < maxQueueAttempts; attempt++ {
		q, ok := s.cached(id)
		if !ok || attempt > 0 {
			var err error
			if q, err = s.load(ctx, id); err != nil {
				if !errors.Is(err, core.ErrNotFound) {
					s.cl.Warn("read play queue", err)
				}
				return PlayQueue{}, false
			}
		}
		if q.UserID != userID {
			return PlayQueue{}, false
		}
		if fn == nil {
			return q.Snapshot(), true
		}
		fn(q)
		q.UpdatedAt = now
		data, err := EncodeQueue(q)
		if err != nil {
			return PlayQueue{}, false
		}
		res, err := s.cl.DB.ExecContext(ctx, `UPDATE plex_playqueues SET data=?, rev=?, updated_at=? WHERE id=? AND rev=?`,
			data, q.Rev+1, now.UnixMilli(), id, q.Rev)
		if err != nil {
			s.cl.Warn("save play queue", err)
			return PlayQueue{}, false
		}
		if n, _ := res.RowsAffected(); n == 0 {
			s.Forget(id) // another node edited it (or it expired): re-read and retry
			continue
		}
		q.Rev++
		s.cache(q)
		s.cl.Invalidate(id)
		return q.Snapshot(), true
	}
	s.cl.Warn("save play queue", errQueueConflict)
	return PlayQueue{}, false
}

// selectShared records the playing item and periodically renews its idle TTL.
// Repeated timelines for the same item must keep long sessions alive, but
// need not write the shared database on every client heartbeat.
func (s *PlayQueues) selectShared(userID, itemID int64) {
	if itemID <= 0 {
		return
	}
	find := func(q *PlayQueue) int {
		if q.UserID != userID {
			return -1
		}
		return QueueItemIndex(q.Items, itemID)
	}
	var qid int64
	s.mu.Lock()
	for _, q := range s.queues {
		if i := find(q); i >= 0 {
			if q.Selected == i && s.now().Sub(q.UpdatedAt) <= queueCacheTTL {
				s.mu.Unlock()
				return
			}
			qid = q.ID
			break
		}
	}
	s.mu.Unlock()
	if qid == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
		rows, err := s.cl.DB.QueryContext(ctx, `SELECT id, rev, data, updated_at FROM plex_playqueues WHERE user_id=? ORDER BY updated_at DESC LIMIT 50`, userID)
		if err != nil {
			cancel()
			s.cl.Warn("find play queue item", err)
			return
		}
		for rows.Next() {
			var id, rev, updated int64
			var data string
			if rows.Scan(&id, &rev, &data, &updated) != nil {
				continue
			}
			q, err := DecodeQueue(id, userID, rev, updated, data)
			if err != nil {
				continue
			}
			if i := find(q); i >= 0 {
				s.cache(q)
				if q.Selected != i || s.now().Sub(q.UpdatedAt) > queueCacheTTL {
					qid = id
				}
				break
			}
		}
		if err := rows.Err(); err != nil {
			s.cl.Warn("find play queue item", err)
		}
		rows.Close()
		cancel()
		if qid == 0 {
			return
		}
	}
	s.withShared(qid, userID, s.now(), func(q *PlayQueue) {
		if i := QueueItemIndex(q.Items, itemID); i >= 0 {
			q.Selected = i
		}
	})
}

// ---------------------------------------------------------------------------
// Handlers

func ShuffleQueue(p *PlayQueue) {
	frontend.ShuffleQueue(p)
}

func UnshuffleQueue(p *PlayQueue) {
	frontend.UnshuffleQueue(p)
}

func QueueItemIndex(items []QueueItem, id int64) int {
	return frontend.QueueItemIndex(items, id)
}

// Controls preserve the playing identity while every other slot receives a
// newly allocated identity. This also applies when the requested state repeats.
func (s *PlayQueues) Reshuffle(q *PlayQueue, shuffled bool) {
	oldVersion := q.Version
	if shuffled {
		original := append([]QueueItem(nil), q.Original...)
		if q.Shuffled {
			q.Shuffled = false
		}
		ShuffleQueue(q)
		if len(original) > 0 {
			q.Original = original
		}
	} else {
		UnshuffleQueue(q)
	}
	q.Version = oldVersion + 1
	keys := make([]core.RatingKey, len(q.Items))
	for i, it := range q.Items {
		keys[i] = it.Key
	}
	var minted []QueueItem
	if s.cl == nil {
		minted = s.newItemsLocked(keys)
	} else {
		var err error
		minted, err = s.Preallocate(keys)
		if err != nil {
			return
		}
	}
	ids := map[int64]int64{}
	for i := range q.Items {
		if i != q.Selected {
			ids[q.Items[i].ID] = minted[i].ID
			q.Items[i].ID = minted[i].ID
		}
	}
	for i := range q.Original {
		if id, ok := ids[q.Original[i].ID]; ok {
			q.Original[i].ID = id
		}
	}
}

func (s *PlayQueues) Take(id, userID int64, clientID string, now time.Time) (PlayQueue, bool) {
	if s.cl == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		q, ok := s.queues[id]
		if !ok || q.UserID != userID {
			return PlayQueue{}, false
		}
		if q.ClientID != clientID {
			s.retireClientLocked(userID, clientID, id)
			q.ClientID = clientID
			q.Version++
			q.UpdatedAt = now
		}
		return q.Snapshot(), true
	}
	current, found := s.Get(id, userID)
	if !found {
		return PlayQueue{}, false
	}
	if current.ClientID == clientID {
		return current, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
	defer cancel()
	tx, err := s.cl.DB.BeginTx(ctx, nil)
	if err != nil {
		return PlayQueue{}, false
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE plex_counters SET value=value WHERE name='playqueue_item'`); err != nil {
		return PlayQueue{}, false
	}
	var owner, rev, updated int64
	var data string
	if err = tx.QueryRowContext(ctx, `SELECT user_id,rev,data,updated_at FROM plex_playqueues WHERE id=?`, id).Scan(&owner, &rev, &data, &updated); err != nil || owner != userID {
		return PlayQueue{}, false
	}
	q, err := DecodeQueue(id, owner, rev, updated, data)
	if err != nil {
		return PlayQueue{}, false
	}
	if q.ClientID != clientID {
		q.ClientID = clientID
		q.Version++
		q.Rev++
		q.UpdatedAt = now
	}
	data, err = EncodeQueue(q)
	if err != nil {
		return PlayQueue{}, false
	}
	if _, err = tx.ExecContext(ctx, `UPDATE plex_playqueues SET data=?,rev=?,updated_at=? WHERE id=?`, data, q.Rev, now.UnixMilli(), id); err != nil {
		return PlayQueue{}, false
	}
	retired, err := retireQueueClient(ctx, tx, userID, clientID, id)
	if err != nil {
		return PlayQueue{}, false
	}
	if err = tx.Commit(); err != nil {
		return PlayQueue{}, false
	}
	for _, old := range retired {
		s.Forget(old)
		s.cl.Invalidate(old)
	}
	s.cache(q)
	s.cl.Invalidate(id)
	return q.Snapshot(), true
}

// Bound work by active queues, independently of catalogue size.
func retireQueueClient(ctx context.Context, tx *sql.Tx, userID int64, clientID string, keep int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,data FROM plex_playqueues WHERE user_id=? AND id<>? LIMIT 2000`, userID, keep)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var data string
		if err = rows.Scan(&id, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var sq struct {
			ClientID string `json:"clientID"`
		}
		if err = json.Unmarshal([]byte(data), &sq); err != nil {
			rows.Close()
			return nil, err
		}
		if sq.ClientID == clientID {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM plex_playqueues WHERE id=? AND user_id=?`, id, userID); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (s *PlayQueues) Configure(now func() time.Time, persistence *PlayQueuePersistence) {
	if now != nil {
		s.now = now
	}
	s.cl = persistence
}
func (s *PlayQueues) Persistent() bool { return s.cl != nil }
func (s *PlayQueues) Len() int         { s.mu.Lock(); defer s.mu.Unlock(); return len(s.queues) }

// FollowingPlayable returns the first playable item after the given extra in
// the viewer's cached queues. It does not perform catalogue or database reads.
func (s *PlayQueues) FollowingPlayable(userID int64, key core.RatingKey, playable func(core.RatingKey) bool) core.RatingKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queues {
		if q.UserID != userID {
			continue
		}
		for i, it := range q.Items {
			if it.Key != key {
				continue
			}
			for _, later := range q.Items[i+1:] {
				if playable(later.Key) {
					return later.Key
				}
			}
		}
	}
	return 0
}

var errQueueConflict = errors.New("plex: play queue changed concurrently")

func queueRowError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	return fmt.Errorf("plex: play queue store: %w", err)
}

// Play-queue value types live in internal/frontend
// (host_sessions_experience.go); aliases keep this package on them.
type PlayQueuePersistence = frontend.PlayQueuePersistence
type QueueItem = frontend.QueueItem
type PlayQueue = frontend.PlayQueue

const MaxPlayQueues = frontend.MaxPlayQueues
