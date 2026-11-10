package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

// TimelineStore persists the client's report and restores continuation only
// when persistence changed. The adapter has already parsed the wire grammar.
type TimelineStore struct {
	Writer          frontend.TimelineWriter
	Now             func() time.Time
	RestoreContinue func(context.Context, int64, core.RatingKey) error
}

func (p TimelineStore) Store(ctx context.Context, user int64, key core.RatingKey, offset, duration time.Duration, status, queue string, offline *int) (context.Context, *frontend.TimelinePersistenceResult, error) {
	result, err := p.Writer.StoreTimeline(ctx, user, key, offset, duration, status, queue, offline)
	if err != nil {
		return ctx, nil, err
	}
	if result.Changed && (result.PlaybackState == "complete" || result.ViewOffset > time.Minute) {
		if parents, ok := p.Writer.(interface {
			PlexTouchParents(context.Context, int64, core.RatingKey, time.Time, bool) error
		}); ok {
			_ = parents.PlexTouchParents(ctx, user, key, p.Now(), result.Completed)
		}
	}
	if result.Changed {
		if restore, ok := p.Writer.(interface {
			RestoreContinue(context.Context, int64, core.RatingKey) error
		}); ok {
			_ = restore.RestoreContinue(ctx, user, key)
		}
		if p.RestoreContinue != nil {
			_ = p.RestoreContinue(ctx, user, key)
		}
	}
	return frontend.WithTimelineStored(ctx), &result, nil
}
