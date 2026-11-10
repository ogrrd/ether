package frontend

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// VersionScope binds a version handle to an authenticated viewer, surface,
// device and host-built client capabilities. It grants no playback by itself.
type VersionScope struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Caps     core.ClientCaps
}

// VersionTarget is the authorized parent associated with an opaque handle.
// A caller must still open playback and revalidate the selected source.
type VersionTarget struct {
	Item core.RatingKey
	// Release is the candidate id stored with the handle. Empty when the
	// handle was resolved by an older caller that only needed the parent.
	Release string
}

type VersionHandles interface {
	Issue(context.Context, VersionScope, core.RatingKey, string) (string, error)
	Resolve(context.Context, VersionScope, string) (VersionTarget, error)
	// MatchSession verifies that a scoped handle denotes the source bound to
	// this authenticated local playback session.
	MatchSession(context.Context, VersionScope, string, string) error
}

// Editions is an optional local-media catalogue and release variant resolver.
type Editions interface {
	Editions(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]Version, error)
	Variants(ctx context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error)
	RefillSearchCache(t *core.Title) <-chan struct{}
	Available(ctx context.Context, key core.RatingKey) (bool, bool)
}

// EditionBatch resolves local copies for a page in one lookup.
type EditionBatch interface {
	EditionsMany(context.Context, *core.Viewer, core.Device, []core.RatingKey) (map[core.RatingKey][]Version, error)
}

// VersionHandleBatch is an optional bounded batch capability. It validates the
// whole requested offer set once and issues all handles atomically.
type VersionHandleBatch interface {
	IssueBatch(context.Context, VersionScope, core.RatingKey, []string) (map[string]string, error)
}
