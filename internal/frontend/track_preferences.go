package frontend

import (
	"context"
	"encoding/json"
)

type TrackPreferences struct {
	AudioLanguages, SubtitleLanguages []string
	SubtitleMode                      string
	PreferOriginal                    bool
	SubtitleAppearance                json.RawMessage
}
type TrackPreferenceService interface {
	PlaybackTrackPreferences
	Get(context.Context, *Viewer) (TrackPreferences, error)
	SetSubtitle(context.Context, *Viewer, []string, string) error
}

// ScopedTrackPreferenceService is an optional extension for shared playback
// preferences; identifiers use the host's library and rating-key namespaces.
type ScopedTrackPreferenceService interface {
	GetScoped(context.Context, *Viewer, TrackScope) (TrackPreferences, int, int, string, map[string]string, error)
	SetScoped(context.Context, *Viewer, string, string, TrackPreferencePatch) error
	DeleteScoped(context.Context, *Viewer, string, string, string) error
	SetAudio(context.Context, *Viewer, []string) error
	SetSubtitleLanguage(context.Context, *Viewer, []string) error
	SetSubtitleMode(context.Context, *Viewer, string) error
	SetAppearance(context.Context, *Viewer, json.RawMessage) error
	SetBitrate(context.Context, *Viewer, int) error
	SetHeight(context.Context, *Viewer, int, string) error
}

type TrackScope struct{ Device, Series, Library string }
type TrackPreferencePatch struct {
	AudioLanguages, SubtitleLanguages *[]string
	SubtitleMode                      *string
	SubtitleAppearance                *json.RawMessage
	MaxBitrateKbps                    *int
	MaxHeight                         *int
	PreferredQuality                  *string
}
