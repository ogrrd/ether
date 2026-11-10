package plex

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type plexTimelineWriter = frontend.PlexTimelineWriter

func (s *Server) timelineWatch(ctx context.Context, pr *Request, key frontend.RatingKey, offset, duration time.Duration, status string, r *http.Request) (context.Context, *frontend.TimelinePersistenceResult, error) {
	if suppressTimelineWatch(ctx) {
		return frontend.WithTimelineStored(ctx), nil, nil
	}
	writer, ok := s.state().(plexTimelineWriter)
	if !ok {
		return ctx, nil, nil
	}
	var offline *int
	if r.URL.Query().Get("offline") == "1" {
		n, err := strconv.Atoi(r.URL.Query().Get("viewCount"))
		if err != nil || n < 0 {
			n = -1
		}
		offline = &n
	}
	var restoreContinue func(context.Context, int64, frontend.RatingKey) error
	if d := s.discovery(); d != nil && d.Personal != nil {
		restoreContinue = d.Personal.RestoreContinue
	}
	return frontend.StoreTimeline(ctx, writer, s.now, restoreContinue, pr.User.ID, key, offset, duration, status, r.URL.Query().Get("playQueueItemID"), offline)
}
func timelineWatchAttrs(mc *model.MediaContainer, result *frontend.TimelinePersistenceResult) {
	if result == nil {
		return
	}
	offset := int64(-1)
	if result.ViewOffset > 0 {
		offset = result.ViewOffset.Milliseconds()
	}
	mc.Extra = append(mc.Extra, model.A("playbackState", result.PlaybackState), model.A("viewOffset", offset), model.A("viewCount", result.ViewCount), model.A("skipCount", 0))
}
