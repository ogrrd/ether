package sessions

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// PartSources is the trusted source-lifetime boundary used after the adapter's
// authentication, title policy and issued-part checks. It does not publish a
// second broker session for a HEAD, download or anonymous continuation.
type PartSources struct {
	Playback core.Playback
	Log      *slog.Logger
}

// Prepare bounds acquisition while leaving shared source flights and normal
// admission/supersession with Playback. A successful wait is followed by normal
// admission; an error never falls through to a second acquisition attempt.
func (s PartSources) Prepare(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, wait time.Duration) (*core.PlaySession, error) {
	if s.Playback == nil {
		return nil, core.ErrNotAvailable
	}
	if p, ok := s.Playback.(core.PlaybackPrepareWithin); ok {
		if _, err := p.PrepareWithin(ctx, wait, viewer, key, caps, release, false); err != nil {
			return nil, err
		}
	}
	if release != "" {
		if p, ok := s.Playback.(core.VariantPlayback); ok {
			return p.PrepareVariant(ctx, viewer, key, caps, release)
		}
		if s.Log != nil {
			s.Log.Info("variant pick ignored: playback manager has no variant support", "key", key, "release", release)
		}
	}
	return s.Playback.Prepare(ctx, viewer, key, caps)
}

// Part retains an existing source where possible, including the unique
// anonymous part continuation. A variant mismatch records the previous source
// so admission can recognize a fallback to that same live session.
func (s PartSources) Part(viewer int64, client string, key core.RatingKey, release string) PartSource {
	ps, ok := s.Playback.SessionFor(viewer, client, key)
	anonymous := false
	if client == "" {
		if p, supported := s.Playback.(interface {
			PlexSessionForPart(int64, core.RatingKey, string) (*core.PlaySession, bool)
		}); supported {
			ps, ok = p.PlexSessionForPart(viewer, key, release)
			anonymous = ok && ps != nil
		}
	}
	result := PartSource{Session: ps, Matches: ok, AnonymousContinuation: anonymous}
	if ok && ps != nil {
		result.PreviousID = ps.ID
		if release != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != release) {
			result.Matches = false
		}
	}
	return result
}

// ServePart preserves the Plex range grammar and request cancellation. The
// admitted source ID is retained; delivery must not open or supersede a session.
func (s PartSources) ServePart(w http.ResponseWriter, r *http.Request, sourceID string) {
	s.Playback.ServeRange(w, r.WithContext(core.WithPlexPart(r.Context())), sourceID)
}

type PartSource = frontend.PartSource
