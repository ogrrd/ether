package app

import (
	"context"
	"encoding/json"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

type surfaceTrackPreferences struct{ app *App }

func (s surfaceTrackPreferences) active(ctx context.Context, v *frontend.Viewer) error {
	if v == nil || v.ID <= 0 || s.app.trackprefs == nil {
		return core.ErrUnauthorized
	}
	u, err := s.app.users.ActiveUser(ctx, v.ID)
	if err != nil || u == nil {
		return core.ErrUnauthorized
	}
	return nil
}

func (s surfaceTrackPreferences) GetScoped(ctx context.Context, v *frontend.Viewer, scope frontend.TrackScope) (frontend.TrackPreferences, int, int, string, map[string]string, error) {
	if err := s.active(ctx, v); err != nil {
		return frontend.TrackPreferences{}, 0, 0, "", nil, err
	}
	r, sources, err := s.app.trackprefs.ScopedRulesWithSources(ctx, v.ID, trackprefs.Scope{Device: scope.Device, Series: scope.Series, Library: scope.Library})
	return frontend.TrackPreferences{AudioLanguages: r.AudioLanguages, SubtitleLanguages: r.SubtitleLanguages, SubtitleMode: r.SubtitleMode, PreferOriginal: r.PreferOriginal, SubtitleAppearance: append(json.RawMessage(nil), r.SubtitleAppearance...)}, r.MaxBitrateKbps, r.MaxHeight, r.PreferredQuality, sources, err
}
func (s surfaceTrackPreferences) SetScoped(ctx context.Context, v *frontend.Viewer, scope, id string, patch frontend.TrackPreferencePatch) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.PutScoped(ctx, v.ID, scope, id, trackprefs.ScopedRules{AudioLanguages: patch.AudioLanguages, SubtitleLanguages: patch.SubtitleLanguages, SubtitleMode: patch.SubtitleMode, SubtitleAppearance: patch.SubtitleAppearance, MaxBitrateKbps: patch.MaxBitrateKbps, MaxHeight: patch.MaxHeight, PreferredQuality: patch.PreferredQuality})
}
func (s surfaceTrackPreferences) DeleteScoped(ctx context.Context, v *frontend.Viewer, scope, id, field string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.DeleteScopedField(ctx, v.ID, scope, id, field)
}
func (s surfaceTrackPreferences) SetAudio(ctx context.Context, v *frontend.Viewer, languages []string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetAudioPreferences(ctx, v.ID, languages)
}
func (s surfaceTrackPreferences) SetSubtitleLanguage(ctx context.Context, v *frontend.Viewer, languages []string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetSubtitleLanguage(ctx, v.ID, languages)
}
func (s surfaceTrackPreferences) SetSubtitleMode(ctx context.Context, v *frontend.Viewer, mode string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetSubtitleMode(ctx, v.ID, mode)
}
func (s surfaceTrackPreferences) SetAppearance(ctx context.Context, v *frontend.Viewer, raw json.RawMessage) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetAppearance(ctx, v.ID, raw)
}
func (s surfaceTrackPreferences) SetBitrate(ctx context.Context, v *frontend.Viewer, kbps int) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetBitratePreference(ctx, v.ID, kbps)
}
func (s surfaceTrackPreferences) SetHeight(ctx context.Context, v *frontend.Viewer, height int, quality string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetHeightPreference(ctx, v.ID, height, quality)
}
func (s surfaceTrackPreferences) Get(ctx context.Context, v *frontend.Viewer) (frontend.TrackPreferences, error) {
	if err := s.active(ctx, v); err != nil {
		return frontend.TrackPreferences{}, err
	}
	rules, err := s.app.trackprefs.GetRules(ctx, v.ID)
	if err != nil {
		return frontend.TrackPreferences{}, err
	}
	return frontend.TrackPreferences{AudioLanguages: rules.AudioLanguages, SubtitleLanguages: rules.SubtitleLanguages, SubtitleMode: rules.SubtitleMode, PreferOriginal: rules.PreferOriginal, SubtitleAppearance: append(json.RawMessage(nil), rules.SubtitleAppearance...)}, nil
}
func (s surfaceTrackPreferences) SetSubtitle(ctx context.Context, v *frontend.Viewer, languages []string, mode string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetSubtitlePreferences(ctx, v.ID, languages, mode)
}

func (s surfaceTrackPreferences) Select(ctx context.Context, user int64, key frontend.RatingKey, info frontend.MediaInfo, original string) (frontend.PlaybackPreferenceSelection, error) {
	if s.app == nil || s.app.trackprefs == nil {
		return frontend.PlaybackPreferenceSelection{}, core.ErrNotAvailable
	}
	return s.app.trackprefs.Select(ctx, user, key, info, original)
}
func (s surfaceTrackPreferences) SelectScoped(ctx context.Context, user int64, key frontend.RatingKey, info frontend.MediaInfo, original string, scope trackprefs.Scope) (frontend.PlaybackPreferenceSelection, bool, error) {
	if s.app == nil || s.app.trackprefs == nil {
		return frontend.PlaybackPreferenceSelection{}, false, core.ErrNotAvailable
	}
	return s.app.trackprefs.SelectScoped(ctx, user, key, info, original, scope)
}
func (s surfaceTrackPreferences) ScopedRules(ctx context.Context, user int64, scope trackprefs.Scope) (frontend.PlaybackPreferenceRules, bool, error) {
	if s.app == nil || s.app.trackprefs == nil {
		return frontend.PlaybackPreferenceRules{}, false, core.ErrNotAvailable
	}
	return s.app.trackprefs.ScopedRules(ctx, user, scope)
}
func (s surfaceTrackPreferences) GetRules(ctx context.Context, user int64) (frontend.PlaybackPreferenceRules, error) {
	if s.app == nil || s.app.trackprefs == nil {
		return frontend.PlaybackPreferenceRules{}, core.ErrNotAvailable
	}
	return s.app.trackprefs.GetRules(ctx, user)
}
func (s surfaceTrackPreferences) HasViewerRules(ctx context.Context, user int64) (bool, error) {
	if s.app == nil || s.app.trackprefs == nil {
		return false, core.ErrNotAvailable
	}
	return s.app.trackprefs.HasViewerRules(ctx, user)
}
func (s surfaceTrackPreferences) Remember(ctx context.Context, user int64, key frontend.RatingKey, kind string, track *frontend.StreamInfo) error {
	if s.app == nil || s.app.trackprefs == nil {
		return core.ErrNotAvailable
	}
	return s.app.trackprefs.Remember(ctx, user, key, kind, track)
}
func (s surfaceTrackPreferences) ForgetChoice(ctx context.Context, user int64, key frontend.RatingKey, kind string) error {
	if s.app == nil || s.app.trackprefs == nil {
		return core.ErrNotAvailable
	}
	return s.app.trackprefs.ForgetChoice(ctx, user, key, kind)
}
func (s surfaceTrackPreferences) PreloadChoices(ctx context.Context, user int64, keys []frontend.RatingKey) error {
	if s.app == nil || s.app.trackprefs == nil {
		return nil
	}
	return s.app.trackprefs.PreloadChoices(ctx, user, keys)
}
