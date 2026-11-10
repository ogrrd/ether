package frontend

import (
	"context"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// PlaybackPreferenceSelection and PlaybackPreferenceRules are persisted value
// contracts; preference lookup and selection remain host responsibilities.
type PlaybackPreferenceSelection = trackprefs.Selection
type PlaybackPreferenceRules = trackprefs.Rules

type PlaybackTrackPreferences interface {
	Select(context.Context, int64, RatingKey, MediaInfo, string) (PlaybackPreferenceSelection, error)
	Remember(context.Context, int64, RatingKey, string, *StreamInfo) error
	GetRules(context.Context, int64) (PlaybackPreferenceRules, error)
	HasViewerRules(context.Context, int64) (bool, error)
	ForgetChoice(context.Context, int64, RatingKey, string) error
	PreloadChoices(context.Context, int64, []RatingKey) error
}
