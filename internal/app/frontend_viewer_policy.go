package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type surfaceViewerPolicy struct{ app *App }

func (s surfaceViewerPolicy) SetMaxRating(ctx context.Context, actor *frontend.Viewer, rating string) error {
	if actor == nil || s.app == nil || s.app.users == nil {
		return core.ErrUnauthorized
	}
	v, err := s.app.users.ActiveUser(ctx, actor.ID)
	if err != nil || !experience.MaySetViewerRating(actor, v) {
		return core.ErrUnauthorized
	}
	return s.app.users.SetViewerMaxRating(ctx, v.ID, rating)
}

func (s surfaceViewerPolicy) Allowed(u *core.User, t *core.Title) bool {
	if s.app == nil || s.app.safety == nil {
		return t != nil && !t.Adult
	}
	return s.app.safety.Allowed(u, t)
}

func (s surfaceViewerPolicy) DiscoveryAllowed(u *core.User, t *core.Title) bool {
	if s.app == nil || s.app.safety == nil {
		return t != nil && !t.Adult
	}
	return s.app.safety.DiscoveryAllowed(u, t)
}

func (s surfaceViewerPolicy) TitleFacts(u *core.User) core.TitleFacts {
	if s.app == nil || s.app.safety == nil {
		return 0
	}
	return s.app.safety.TitleFacts(u)
}

func (s surfaceViewerPolicy) CanPlay(u *core.User, remote bool, now time.Time) bool {
	if s.app == nil || s.app.safety == nil {
		return u != nil
	}
	return s.app.safety.CanPlay(u, remote, now)
}

func (s surfaceViewerPolicy) ConfirmPlayback(ctx context.Context, u *core.User, remote bool, now time.Time) (*core.User, error) {
	if s.app == nil || s.app.safety == nil {
		return nil, core.ErrPolicyUnconfirmed
	}
	return s.app.safety.ConfirmPlayback(ctx, u, remote, now)
}

func (s surfaceViewerPolicy) BitrateLimit(u *core.User) int {
	if s.app == nil || s.app.safety == nil {
		return 0
	}
	return s.app.safety.BitrateLimit(u)
}

func (s surfaceViewerPolicy) Restricted(u *core.User) bool {
	if s.app == nil || s.app.safety == nil {
		return u == nil || u.MaxContentRating != "" || len(u.AllowedLibraries) > 0
	}
	return s.app.safety.Restricted(u)
}

func (s surfaceViewerPolicy) UserPolicy(u *core.User) *core.User {
	if s.app == nil || s.app.safety == nil {
		return u
	}
	return s.app.safety.UserPolicy(u)
}

func (s surfaceViewerPolicy) AllowDownloads(u *core.User) bool {
	if s.app == nil || s.app.safety == nil {
		return u != nil && u.IsOwner
	}
	return s.app.safety.AllowDownloads(u)
}

func (s surfaceViewerPolicy) BrowseBlocks(u *core.User) ([]string, []string) {
	if s.app == nil || s.app.safety == nil {
		return nil, nil
	}
	return s.app.safety.BrowseBlocks(u)
}

func (s surfaceViewerPolicy) BrowseShareFilter(u *core.User, typ core.MediaType) core.Filter {
	if s.app == nil || s.app.safety == nil {
		return core.Filter{}
	}
	return s.app.safety.BrowseShareFilter(u, typ)
}

var (
	_ frontend.ViewerPolicy         = surfaceViewerPolicy{}
	_ frontend.PlaybackAccessPolicy = surfaceViewerPolicy{}
	_ frontend.BitrateAccessPolicy  = surfaceViewerPolicy{}
	_ frontend.ArtworkAccessPolicy  = surfaceViewerPolicy{}
	_ frontend.UserPolicySource     = surfaceViewerPolicy{}
	_ frontend.DownloadAccessPolicy = surfaceViewerPolicy{}
)
