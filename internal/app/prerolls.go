package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/prerolls"
)

// buildPrerolls wires owner pre-rolls (#298). Clips live in
// <data>/prerolls and are converted by the bundled FFmpeg.
func (a *App) buildPrerolls() error {
	var conv prerolls.Converter
	if a.transcoder != nil {
		conv = prerolls.FFmpeg{Path: a.transcoder.FFmpegPath, ProbeFunc: a.transcoder.Probe}
	}
	opts := prerolls.Options{
		DB:        a.db,
		Dir:       filepath.Join(a.opts.DataDir, "prerolls"),
		Converter: conv,
		Logger:    a.log.With("component", "prerolls"),
		Viewer:    a.users.ActiveUser,
	}
	if a.state != nil {
		opts.Activity = a.state
	}
	if a.catalog != nil {
		opts.Title = a.catalog.Get
	}
	if a.collections != nil {
		opts.InCollection = a.collections.Contains
	}
	svc, err := prerolls.New(opts)
	if err != nil {
		return err
	}
	a.prerolls = svc
	return nil
}

// prerollAdmin is the pre-roll owner API dependency, nil when off.
func (a *App) prerollAdmin() admin.Prerolls {
	if a.prerolls == nil {
		return nil
	}
	return a.prerolls
}

// prerollSurface lists the pre-roll as a Jellyfin/Emby intro for an item
// the front end already authorized.
type prerollSurface struct {
	svc   *prerolls.Service
	state core.State
}

func prerollExtra(clip core.PrerollClip) frontend.Extra {
	return frontend.Extra{Key: clip.Key, Title: clip.Name, Kind: "trailer", Official: true, Duration: clip.Duration, Height: clip.Height, PublishedAt: clip.UpdatedAt}
}

func (s prerollSurface) Intro(ctx context.Context, viewer *core.Viewer, device core.Device, detail frontend.ItemDetail, away bool) (frontend.Extra, string, bool, error) {
	feature := detail.Title.Key
	if s.svc == nil || viewer == nil || (feature.Kind() != core.KindMovie && feature.Kind() != core.KindEpisode) || !s.svc.Active(ctx) {
		return frontend.Extra{}, "", false, nil
	}
	resume := detail.UserData.ViewOffset > 0
	if !resume && s.state != nil {
		if st, err := s.state.Get(ctx, viewer.ID, feature); err == nil && st.ViewOffset > 0 {
			resume = true
		}
	}
	title := detail.Title
	clip, ok := s.svc.Pick(ctx, core.PrerollRequest{
		Viewer: viewer, Device: core.ClassifyDevice(device.Product, device.Platform, device.DeviceName), Away: away,
		Feature: &title, Resume: resume, Client: device.Product, At: time.Now(),
	})
	if !ok {
		return frontend.Extra{}, "", false, nil
	}
	return prerollExtra(clip), clip.Order, true, nil
}

func (s prerollSurface) Lookup(ctx context.Context, key core.RatingKey) (frontend.Extra, bool) {
	clip, ok := s.svc.Clip(ctx, key)
	if !ok {
		return frontend.Extra{}, false
	}
	return prerollExtra(clip), true
}

func (s prerollSurface) Open(ctx context.Context, key core.RatingKey) (io.ReadSeekCloser, time.Time, error) {
	clip, ok := s.svc.Clip(ctx, key)
	if !ok {
		return nil, time.Time{}, core.ErrNotFound
	}
	f, err := os.Open(clip.Path)
	if err != nil {
		return nil, time.Time{}, core.ErrNotFound
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, time.Time{}, core.ErrNotFound
	}
	return f, st.ModTime(), nil
}

func (s prerollSurface) Started(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) {
	s.svc.Started(ctx, core.PrerollPlay{Viewer: viewer, Key: key, Client: device.Product})
}

func (s prerollSurface) Active(ctx context.Context) bool {
	return s.svc != nil && s.svc.Active(ctx)
}

func (s prerollSurface) Pick(ctx context.Context, req core.PrerollRequest) (core.PrerollClip, bool) {
	if s.svc == nil {
		return core.PrerollClip{}, false
	}
	return s.svc.Pick(ctx, req)
}

func (s prerollSurface) Clip(ctx context.Context, key core.RatingKey) (core.PrerollClip, bool) {
	if s.svc == nil {
		return core.PrerollClip{}, false
	}
	return s.svc.Clip(ctx, key)
}

func (s prerollSurface) Thumb(ctx context.Context, key core.RatingKey) (string, bool) {
	if s.svc == nil {
		return "", false
	}
	return s.svc.Thumb(ctx, key)
}

func (s prerollSurface) RecordPlay(ctx context.Context, play core.PrerollPlay) {
	if s.svc != nil {
		s.svc.Started(ctx, play)
	}
}
