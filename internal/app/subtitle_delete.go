package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

func (s subtitleSurface) DeleteSubtitle(_ context.Context, key core.RatingKey, _ *core.User, stream core.StreamInfo) error {
	return s.app.subtitles.DeleteUpload(key, stream)
}
