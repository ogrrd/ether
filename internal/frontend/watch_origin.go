package frontend

import (
	"context"
	"time"
)

// WatchOrigin identifies the authenticated surface and client responsible for
// a watch update. HistoryTitle retains display identity after catalogue removal.
type WatchOrigin struct {
	HistoryTitle        *Title
	Surface             Protocol
	SessionID, ClientID string
	DeviceID            int64
}
type watchOriginKey struct{}

func WithWatchOrigin(ctx context.Context, origin WatchOrigin) context.Context {
	return context.WithValue(ctx, watchOriginKey{}, origin)
}
func WatchOriginFrom(ctx context.Context) WatchOrigin {
	origin, _ := ctx.Value(watchOriginKey{}).(WatchOrigin)
	return origin
}
func WithHistoryTitle(ctx context.Context, title *Title) context.Context {
	if title == nil {
		return ctx
	}
	snapshot := *title
	origin := WatchOriginFrom(ctx)
	origin.HistoryTitle = &snapshot
	return WithWatchOrigin(ctx, origin)
}

// TimelinePersistenceResult describes committed state, rather than echoing a
// client's playhead.
type TimelinePersistenceResult struct {
	PlaybackState      string
	ViewOffset         time.Duration
	ViewCount          int
	Changed, Completed bool
}
type TimelineWriter interface {
	StoreTimeline(context.Context, int64, RatingKey, time.Duration, time.Duration, string, string, *int) (TimelinePersistenceResult, error)
}
type timelineStoredKey struct{}

func WithTimelineStored(ctx context.Context) context.Context {
	return context.WithValue(ctx, timelineStoredKey{}, true)
}
func TimelineStored(ctx context.Context) bool {
	stored, _ := ctx.Value(timelineStoredKey{}).(bool)
	return stored
}
