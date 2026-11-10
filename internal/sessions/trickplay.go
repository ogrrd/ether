package sessions

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/jpeg"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/trickplay"
)

const (
	trickplayColumns = 10
	trickplayRows    = 10
	trickplayMaxEdge = 640
)

// trickplayGeometry is optional on the borrowed provider. Missing methods
// keep the shared thumbnail defaults (10s, 320px).
type trickplayGeometry interface {
	PreviewInterval() time.Duration
	PreviewWidth() int
}

func trickplayShape(p core.TrickplayProvider) (time.Duration, int) {
	interval, width := 10*time.Second, 320
	if g, ok := p.(trickplayGeometry); ok {
		if g.PreviewInterval() > 0 {
			interval = g.PreviewInterval()
		}
		if g.PreviewWidth() > 0 {
			width = g.PreviewWidth()
		}
	}
	return interval, width
}

func thumbnailCount(duration, interval time.Duration) int {
	if duration <= 0 || interval <= 0 {
		return 0
	}
	count := int(duration / interval)
	if duration%interval != 0 {
		count++
	}
	if count < 1 {
		count = 1
	}
	if count > 100000 {
		return 0
	}
	return count
}

// ensureTrickplay asks the borrowed provider to continue generation for the
// session's current release while holding its shared job lease.
func (b *Broker) ensureTrickplay(ctx context.Context, s *brokerSession, until time.Duration) {
	if b == nil || b.deps.Trickplay == nil || s == nil || s.live || s.playID == "" || b.deps.Playback == nil {
		return
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID == "" {
		return
	}
	if !b.trickplayBindingCurrent(s, ps) {
		return
	}
	cacheKey := core.TrickplayCacheKey(ps)
	currentSource := func() bool {
		latest, exists := b.deps.Playback.Session(s.playID)
		return exists && latest != nil && cacheKey != "" && core.TrickplayCacheKey(latest) == cacheKey
	}
	if cacheKey == "" {
		return
	}
	input := b.deps.Playback.InputURL(s.playID)
	if input == "" {
		return
	}
	if _, leased := b.deps.Trickplay.(core.TrickplayLeaser); !leased {
		b.deps.Trickplay.EnsureTrickplay(ctx, core.TrickplaySource{Current: currentSource, ReleaseID: cacheKey, InputURL: input, Info: ps.Info, AvailableUntil: until})
		return
	}
	b.mu.Lock()
	current := b.sessions[s.public.ID]
	if current == nil || b.closed || current.playID != s.playID {
		b.mu.Unlock()
		return
	}
	if current.trickplay == nil {
		current.trickplay = &trickplay.Holder{}
	}
	hold := current.trickplay
	b.mu.Unlock()
	err := hold.UpdateIf(ctx, b.deps.Trickplay, core.TrickplaySource{
		Current:        currentSource,
		ReleaseID:      cacheKey,
		InputURL:       input,
		Info:           ps.Info,
		AvailableUntil: until,
	}, func() bool {
		b.mu.Lock()
		active := !b.closed && b.sessions[s.public.ID] != nil && b.sessions[s.public.ID].trickplay == hold
		b.mu.Unlock()
		if !active {
			return false
		}
		latest, ok := b.deps.Playback.Session(s.playID)
		return ok && latest != nil && latest.Resolution != nil && core.TrickplayCacheKey(latest) == cacheKey && latest.Failovers == ps.Failovers && latest.DownFailovers == ps.DownFailovers && b.deps.Playback.InputURL(s.playID) == input
	})
	if err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}

func (b *Broker) trickplayBindingCurrent(s *brokerSession, ps *core.PlaySession) bool {
	if s.versionHandle != "" && s.exactRelease == "" {
		return false
	}
	if s.exactRelease != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != s.exactRelease) {
		return false
	}
	if s.sourcePinned && (s.sourceETag == "" || sourceRevision(ps) != s.sourceETag) {
		return false
	}
	return true
}

func (b *Broker) trickplayReady(ctx context.Context, a frontend.SessionAccess, item core.RatingKey) (string, core.MediaInfo, time.Duration, int, bool) {
	if b == nil || b.deps.Trickplay == nil || item == 0 || a.Viewer == nil || b.deps.Playback == nil {
		return "", core.MediaInfo{}, 0, 0, false
	}
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return "", core.MediaInfo{}, 0, 0, false
	}
	b.mu.Lock()
	candidates := make([]brokerSession, 0, 1)
	for _, s := range b.sessions {
		if s == nil || s.live || s.public.Item != item || s.public.ViewerID != viewer.ID {
			continue
		}
		if a.Protocol != "" && s.public.Protocol != a.Protocol {
			continue
		}
		if a.Device.ID != "" && s.public.Device.ID != a.Device.ID {
			continue
		}
		candidates = append(candidates, *s)
	}
	b.mu.Unlock()
	var best *brokerSession
	var bestInfo core.MediaInfo
	var bestRelease string
	for i := range candidates {
		s := &candidates[i]
		if !b.currentMediaPolicy(ctx, viewer, s) {
			continue
		}
		ps, ok := b.deps.Playback.Session(s.playID)
		if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID == "" || !b.trickplayBindingCurrent(s, ps) {
			continue
		}
		if best != nil && !s.public.StartedAt.After(best.public.StartedAt) {
			continue
		}
		best = s
		bestInfo = ps.Info
		bestRelease = core.TrickplayCacheKey(ps)
	}
	if best == nil {
		return "", core.MediaInfo{}, 0, 0, false
	}
	interval, width := trickplayShape(b.deps.Trickplay)
	return bestRelease, bestInfo, interval, width, true
}

// TrickplayOffer revalidates the viewer, item, version, and source generation
// before advertising tiles. A false offer leaves the Jellyfin map empty.
func (b *Broker) TrickplayOffer(ctx context.Context, a frontend.SessionAccess, item core.RatingKey) (frontend.TrickplayOffer, bool) {
	release, info, interval, width, ok := b.trickplayReady(ctx, a, item)
	if !ok {
		return frontend.TrickplayOffer{}, false
	}
	count := thumbnailCount(info.Duration, interval)
	frame, ok := b.deps.Trickplay.TrickplayFrame(release, 0)
	if !ok || len(frame) == 0 || count < 1 {
		return frontend.TrickplayOffer{}, false
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > trickplayMaxEdge || cfg.Height > trickplayMaxEdge {
		return frontend.TrickplayOffer{}, false
	}
	if width <= 0 || width > trickplayMaxEdge {
		width = cfg.Width
	}
	bandwidth := 0
	if interval > 0 {
		bandwidth = int(float64(len(frame)*8) / interval.Seconds())
	}
	return frontend.TrickplayOffer{
		Width: width, Height: cfg.Height,
		Columns: trickplayColumns, Rows: trickplayRows,
		Count: count, Interval: interval, Bandwidth: bandwidth,
	}, true
}

// TrickplaySheet composes one JPEG grid from the borrowed provider's frames.
func (b *Broker) TrickplaySheet(ctx context.Context, a frontend.SessionAccess, item core.RatingKey, index int) ([]byte, error) {
	offer, ok := b.TrickplayOffer(ctx, a, item)
	per := offer.Columns * offer.Rows
	if !ok || index < 0 || per <= 0 || offer.Width <= 0 || offer.Height <= 0 {
		return nil, core.ErrNotFound
	}
	sheets := (offer.Count + per - 1) / per
	if index >= sheets {
		return nil, core.ErrNotFound
	}
	release, _, interval, _, ok := b.trickplayReady(ctx, a, item)
	if !ok {
		return nil, core.ErrNotFound
	}
	canvas := image.NewRGBA(image.Rect(0, 0, offer.Width*offer.Columns, offer.Height*offer.Rows))
	drawn := false
	for cell := 0; cell < per; cell++ {
		at := time.Duration(index*per+cell) * interval
		if index*per+cell >= offer.Count {
			break
		}
		frame, ok := b.deps.Trickplay.TrickplayFrame(release, at)
		if !ok || len(frame) == 0 {
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(frame))
		if err != nil {
			continue
		}
		col, row := cell%offer.Columns, cell/offer.Columns
		draw.Draw(canvas, image.Rect(col*offer.Width, row*offer.Height, (col+1)*offer.Width, (row+1)*offer.Height), img, img.Bounds().Min, draw.Src)
		drawn = true
	}
	if !drawn {
		return nil, core.ErrNotFound
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TrickplayAvailable reports whether the borrowed provider has previews for id.
func (b *Broker) TrickplayAvailable(id string) bool {
	if b == nil || b.deps.Trickplay == nil || id == "" {
		return false
	}
	return b.deps.Trickplay.TrickplayAvailable(id)
}

// TrickplayBIF returns the sparse BIF snapshot for id from the borrowed provider.
func (b *Broker) TrickplayBIF(id string) ([]byte, bool) {
	if b == nil || b.deps.Trickplay == nil || id == "" {
		return nil, false
	}
	return b.deps.Trickplay.TrickplayBIF(id)
}

// TrickplayFrame returns a single generated JPEG frame for id at offset at.
func (b *Broker) TrickplayFrame(id string, at time.Duration) ([]byte, bool) {
	if b == nil || b.deps.Trickplay == nil || id == "" {
		return nil, false
	}
	return b.deps.Trickplay.TrickplayFrame(id, at)
}

// CancelTrickplay cancels idle preview generation for id when supported.
func (b *Broker) CancelTrickplay(id string) {
	if b == nil || b.deps.Trickplay == nil || id == "" {
		return
	}
	if canceler, ok := b.deps.Trickplay.(frontend.TrickplayCanceler); ok {
		canceler.CancelTrickplay(id)
	}
}
