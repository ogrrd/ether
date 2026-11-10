package plex

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Netflix-style per-viewer hooks (items #30–#32) in the front end. All
// three are optional capabilities of the recommender (frontend.PostPlayer,
// frontend.MicroReasoner, frontend.IntentTracker) gated by the owner's
// Personalize switches; without them every response is byte-identical to
// the plain PMS behaviour.

// postPlayPicks returns the viewer's ranked hook queue after key, or nil
// when the feature is off/unavailable (callers fall back to Related).
func (s *Server) postPlayPicks(ctx context.Context, pr *Request, key frontend.RatingKey, count int) []frontend.PostPlayPick {
	if s.recommender() == nil || pr.User == nil || !s.cfg().Personalize.PostPlay {
		return nil
	}
	pp, ok := s.recommender().(frontend.PostPlayer)
	if !ok {
		return nil
	}
	picks, err := pp.PostPlay(ctx, pr.User, key, s.hubContext(pr, 0), count)
	if err != nil {
		s.log.Debug("postplay picks", "key", key, "err", err)
		return nil
	}
	return picks
}

func (s *Server) postPlaySelection(ctx context.Context, pr *Request, key frontend.RatingKey, unrestricted bool) frontend.PostPlaySelection {
	opts := frontend.PostPlaySelectionOptions{Visibility: s.visibility(), Unrestricted: unrestricted, Picks: func(ctx context.Context, fetch int) []frontend.PostPlayPick {
		return s.postPlayPicks(ctx, pr, key, fetch)
	}}
	if s.recommender() != nil {
		opts.Related = func(ctx context.Context, count int) ([]frontend.Hub, error) {
			return s.recommender().Related(ctx, pr.User, key, count)
		}
	}
	return s.exp().NewPostPlaySelection(opts)
}

// postPlayHub renders ranked picks as one hub in post-play shape. The
// prefix follows the just-watched title's family, like Related's hubs.
func (s *Server) postPlayHub(ctx context.Context, pr *Request, key frontend.RatingKey, row frontend.PostPlayRow) (model.Hub, bool) {
	items := row.Items
	if len(items) == 0 {
		return model.Hub{}, false
	}
	prefix := "movie"
	if k := key.Kind(); k == frontend.KindShow || k == frontend.KindSeason || k == frontend.KindEpisode {
		prefix = "tv"
	}
	h := frontend.Hub{
		ID: prefix + ".postplay", Title: "Recommended For You",
		Context: "hub." + prefix + ".postplay", Style: "shelf", Items: items,
	}
	return s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}), true
}

// decorateDetail adds the viewer's micro-reason decision header (#31) to
// a single-item detail response. It only touches the rendered copy: the
// header prefixes Summary (every app shows it on the detail page), and
// when the title has no tagline of its own it takes the Tagline slot
// instead — the line living-room apps put directly under the title —
// rather than appearing twice. Original text is kept intact.
func (s *Server) decorateDetail(ctx context.Context, pr *Request, t *frontend.Title, md *model.Metadata) {
	if s.recommender() == nil || pr.User == nil || t == nil || md == nil {
		return
	}
	if !s.cfg().Personalize.MicroReasons {
		return
	}
	mr, ok := s.recommender().(frontend.MicroReasoner)
	if !ok {
		return
	}
	// Copy: the title may be the shared, cached catalogue record and must
	// never carry one viewer's header into another viewer's response.
	cp := *t
	head := mr.MicroReason(ctx, pr.User, &cp, s.now())
	if head.Empty() {
		return
	}
	if strings.TrimSpace(md.Tagline) == "" {
		md.Tagline = head.Header
		return
	}
	if strings.TrimSpace(md.Summary) == "" {
		md.Summary = head.Header
	} else {
		md.Summary = head.Header + "\n\n" + md.Summary
	}
}

// recordDetailIntent feeds a single-item detail open to the live
// session-intent store (#32). Hub/list renders never call this.
func (s *Server) recordDetailIntent(ctx context.Context, pr *Request, t *frontend.Title) {
	if s.recommender() == nil || pr.User == nil || t == nil {
		return
	}
	if !s.cfg().Personalize.SessionIntent {
		return
	}
	if it, ok := s.recommender().(frontend.IntentTracker); ok {
		cp := *t
		it.RecordDetailOpen(ctx, pr.User, &cp)
	}
}
