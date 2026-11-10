package frontend

import (
	"context"
	"time"
)

// ViewerPolicy evaluates and mutates household policy with host-side authorization.
type ViewerPolicy interface {
	AccessPolicy
	SetMaxRating(context.Context, *Viewer, string) error
}

// PlaybackAccessPolicy is the optional playback admission check an access
// policy may expose. Absence means the caller applies no extra restriction.
type PlaybackAccessPolicy interface {
	CanPlay(*User, bool, time.Time) bool
}

// PlaybackConfirmationPolicy is the optional live-policy confirmation check an
// access policy may expose.
type PlaybackConfirmationPolicy interface {
	ConfirmPlayback(context.Context, *User, bool, time.Time) (*User, error)
}

// UserPolicySource is the optional per-viewer policy view an access policy
// may expose. It returns the user as policy currently sees them.
type UserPolicySource interface{ UserPolicy(*User) *User }

// ActiveUserSource checks current server membership for delegated stream
// access.
type ActiveUserSource interface {
	ActiveUser(context.Context, int64) (*User, error)
}

// ArtworkAccessPolicy is the optional restricted-viewer check an access
// policy may expose for artwork decisions.
type ArtworkAccessPolicy interface{ Restricted(*User) bool }

// UserListSource is the optional household account list an identity surface
// may expose to owner-level endpoints.
type UserListSource interface {
	Users(context.Context) ([]User, error)
}

// BrowseBlocksPolicy is the optional genre and keyword block list an access
// policy may expose for browse queries.
type BrowseBlocksPolicy interface {
	BrowseBlocks(*User) ([]string, []string)
}

// BrowseShareFilterPolicy is the optional share filter an access policy may
// expose for child visibility in browse queries.
type BrowseShareFilterPolicy interface {
	BrowseShareFilter(*User, MediaType) Filter
}
