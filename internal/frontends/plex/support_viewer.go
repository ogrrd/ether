package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

// SupportViewer is a bounded snapshot at archive creation, not a replay of
// policy decisions made when an earlier response was generated.
func (s *Server) SupportViewer(ctx context.Context, user *frontend.Viewer) (frontend.DiagnosticViewerFacts, error) {
	var out frontend.DiagnosticViewerFacts
	for _, sec := range s.userSections(user) {
		if len(out.Sections) >= 128 {
			out.SectionsTruncated = true
			break
		}
		out.Sections = append(out.Sections, frontend.DiagnosticViewerSection{ID: sec.ID, Type: string(sec.Type), Title: sec.Title})
	}
	if user == nil || s.state() == nil || s.catalog() == nil {
		return out, nil
	}
	pr := &Request{User: user}
	ctx = withStoredPolicy(ctx)
	states, err := s.state().InProgress(ctx, user.ID, 201)
	if err != nil {
		return out, err
	}
	if len(states) > 200 {
		out.CandidatesTruncated = true
		states = states[:200]
	}
	keys := make([]frontend.RatingKey, 0, len(states))
	for _, st := range states {
		keys = append(keys, st.Key)
	}
	titles := s.getTitles(ctx, keys)
	byKey := map[frontend.RatingKey]frontend.Title{}
	for _, ti := range titles {
		byKey[ti.Key] = ti
	}
	selected, _, _ := s.continueExperience(pr).Shelf(ctx, user, "", 200)
	visible := s.takeVisible(user, selected, 200)
	selectedSet := map[frontend.RatingKey]bool{}
	for _, ti := range visible {
		selectedSet[ti.Key] = true
	}
	seen := map[frontend.RatingKey]bool{}
	for _, st := range states {
		ti, ok := byKey[st.Key]
		c := frontend.DiagnosticViewerCandidate{Key: int64(st.Key), OffsetMS: st.ViewOffset.Milliseconds()}
		switch {
		case !ok:
			c.Reason = "not returned by the catalogue: missing or filtered by catalogue or release rules"
			if source, yes := s.catalog().(interface {
				SupportVisibility(context.Context, frontend.RatingKey) (*frontend.Title, string, error)
			}); yes {
				title, reason, err := source.SupportVisibility(ctx, st.Key)
				if err == nil {
					c.Reason = reason
					if title != nil {
						c.Title = title.Title
					}
				}
			}
		case selectedSet[st.Key]:
			c.Title = ti.Title
			c.Included = true
			c.Reason = "in progress and selected"
		case !s.visibility().Playable(ctx, user, &ti) || !s.titleAllowed(user, &ti):
			c.Title = ti.Title
			c.Reason = "excluded by library or content policy"
		case !s.visibleTitle(ctx, user, &ti):
			c.Title = ti.Title
			c.Reason = "not available to this viewer"
		case s.discovery() != nil && s.discovery().Personal != nil && !s.discovery().Personal.ContinueAllowed(ctx, user.ID, &ti):
			c.Title = ti.Title
			c.Reason = "Continue Watching dismissal applies or its lookup failed"
		case s.sameDayUnavailable(ctx, pr, &ti):
			c.Title = ti.Title
			c.Reason = "unavailable under release rules"
		default:
			c.Title = ti.Title
			c.Reason = "not selected: exclusion, duplicate show, or row limit"
		}
		out.Candidates = append(out.Candidates, c)
		seen[st.Key] = true
	}
	for _, ti := range visible {
		if !seen[ti.Key] {
			out.Candidates = append(out.Candidates, frontend.DiagnosticViewerCandidate{Key: int64(ti.Key), Title: ti.Title, Included: true, Reason: "next episode selected from completed viewing history"})
		}
	}
	// Completed history can yield a next episode even when no resume point
	// remains. Check current state because history can outlive an unscrobble.
	history, err := s.state().History(ctx, user.ID, time.Time{}, 100)
	if err != nil {
		return out, err
	}
	var completed []frontend.RatingKey
	for _, h := range history {
		if h.Completed && !seen[h.Key] {
			seen[h.Key] = true
			completed = append(completed, h.Key)
			if len(completed) == 50 {
				break
			}
		}
	}
	completedStates, err := s.state().GetMany(ctx, user.ID, completed)
	if err != nil {
		return out, err
	}
	for _, ti := range s.getTitles(ctx, completed) {
		if completedStates[ti.Key].ViewCount > 0 {
			out.Candidates = append(out.Candidates, frontend.DiagnosticViewerCandidate{Key: int64(ti.Key), Title: ti.Title, Reason: "finished; history may supply the next episode"})
		}
	}
	out.Notes = append(out.Notes, "Snapshot taken when the archive was built; selection may differ from an earlier response. Section IDs and selection follow Plex.")
	return out, ctx.Err()
}
