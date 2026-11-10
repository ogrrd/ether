package sessions

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type plexStores struct{}

// NewSessionStores returns the sessions-backed implementation of the
// Plex session-store host contract.
func NewSessionStores() frontend.SessionStores { return plexStores{} }

func (plexStores) NewCoordination() frontend.Coordination { return NewCoordination() }

func (plexStores) NewDecisionArms(ctx context.Context, db *sql.DB, now func() time.Time, node string, log *slog.Logger, wg *sync.WaitGroup) frontend.DecisionArms {
	return NewDecisionArms(ctx, db, now, node, log, wg)
}

func (plexStores) NewStartGuard() frontend.StartGuard { return &StartGuard{} }
func (plexStores) NewPlayQueues() frontend.PlayQueues { return NewPlayQueues() }
func (plexStores) NewDownloadQueueStore() frontend.DownloadQueueStore {
	return NewDownloadQueueStore()
}

func (plexStores) NewVersionFlows(ctx context.Context, db *sql.DB) frontend.VersionFlows {
	return NewVersionFlows(ctx, db)
}

func (plexStores) NewVariantIDs(db *sql.DB, now func() time.Time) frontend.VariantIDs {
	return NewVariantIDs(db, now)
}

func (plexStores) NewStreamTable() frontend.StreamTable { return NewStreamTable() }

func (plexStores) NewDownloadJob(playback core.Playback, remuxer core.DownloadRemuxer) frontend.DownloadJob {
	return DownloadJob{Playback: playback, Remuxer: remuxer}
}

func (plexStores) NewDownloadQueuePersistence(queue frontend.DownloadQueueStore, db *sql.DB, artifacts core.DownloadRemuxer, log *slog.Logger, expiry func() time.Duration, storageID func() string, users func(context.Context) ([]core.User, error)) frontend.DownloadQueuePersistence {
	q, _ := queue.(*DownloadQueueStore)
	if q == nil {
		return DownloadQueuePersistence{}
	}
	return DownloadQueuePersistence{Queue: q, DB: db, Artifacts: artifacts, Log: log, Expiry: expiry, StorageID: storageID, Users: users}
}

func (plexStores) NewPartSources(playback core.Playback, log *slog.Logger) frontend.PartSources {
	return PartSources{Playback: playback, Log: log}
}

func (plexStores) ObserveStart(ctx context.Context, source core.HLSSession, id string) (frontend.StartObservation, bool) {
	return ObserveStart(ctx, source, id)
}
func (plexStores) EncodeQueue(q *frontend.PlayQueue) (string, error) { return EncodeQueue(q) }
func (plexStores) DecodeQueue(id, userID, rev, updated int64, data string) (*frontend.PlayQueue, error) {
	return DecodeQueue(id, userID, rev, updated, data)
}
func (plexStores) ShuffleQueue(p *frontend.PlayQueue)   { ShuffleQueue(p) }
func (plexStores) UnshuffleQueue(p *frontend.PlayQueue) { UnshuffleQueue(p) }
func (plexStores) QueueItemIndex(items []frontend.QueueItem, id int64) int {
	return QueueItemIndex(items, id)
}
func (plexStores) VariantHash(key core.RatingKey, releaseID string) int64 {
	return VariantHash(key, releaseID)
}
func (plexStores) UnderlyingActivityStore(s frontend.Sessions) frontend.PlaybackActivityStore {
	b := UnderlyingBroker(s)
	if b == nil {
		return nil
	}
	return b
}
