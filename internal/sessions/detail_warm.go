package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// playCapsTTL bounds how long a device's declared playback caps stand in for
// it on detail pages. Apps re-declare on every PlaybackInfo, so a device that
// still plays keeps its entry fresh; a long-idle one stops warming.
const playCapsTTL = 7 * 24 * time.Hour

// maxRememberedPlayCaps bounds the in-memory table; the stalest entry goes.
const maxRememberedPlayCaps = 4096

type rememberedPlayCaps struct {
	caps core.ClientCaps
	at   time.Time
}

var _ frontend.DetailWarmer = (*Broker)(nil)

// playCaps applies the broker's surface identity and owner limits to a
// client's declared caps. Plan and every warm-up use it, so a warm entry is
// keyed and profiled exactly like the Prepare that later takes it.
func (b *Broker) playCaps(v *core.Viewer, protocol frontend.Protocol, device core.Device, caps core.ClientCaps) core.ClientCaps {
	caps = copyRequest(frontend.PlanRequest{Caps: caps}).Caps
	caps.Protocol = protocol
	caps.ClientID = clientKey(protocol, device)
	applySubtitleProfiles(&caps)
	if caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}
	if v != nil && b.deps.BitrateLimit != nil {
		if limit := b.deps.BitrateLimit(v); limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}
	return caps
}

// warmDiscarder is implemented by playback managers that can drop a warm
// entry before Prepare would take it.
type warmDiscarder interface {
	DiscardWarm(user *core.User, caps core.ClientCaps, key core.RatingKey)
}

// capsWiden reports whether a new declaration allows something the earlier
// one did not: a warm pick made under the earlier caps may then be lower than
// what Play would select now (a raised bitrate or resolution limit, HDR or
// Dolby Vision turned on, another codec). Narrower caps are safe: Prepare's
// own acceptance check rejects a warm release the device cannot play.
func capsWiden(next, prev core.ClientCaps) bool {
	if prev.MaxBitrateKbps > 0 && (next.MaxBitrateKbps == 0 || next.MaxBitrateKbps > prev.MaxBitrateKbps) {
		return true
	}
	if prev.MaxHeight > 0 && (next.MaxHeight == 0 || next.MaxHeight > prev.MaxHeight) {
		return true
	}
	if (next.HDR10 && !prev.HDR10) || (next.DolbyVision && !prev.DolbyVision) {
		return true
	}
	if len(prev.VideoCodecs) > 0 {
		if len(next.VideoCodecs) == 0 {
			return true
		}
		for _, c := range next.VideoCodecs {
			if !prev.SupportsVideo(c) {
				return true
			}
		}
	}
	return false
}

// rememberPlayCaps keeps the latest declared caps (before owner limits) of
// one viewer's device on one surface. When the declaration widened since the
// detail page warmed key, that warm entry is dropped before Prepare.
func (b *Broker) rememberPlayCaps(viewer *core.Viewer, protocol frontend.Protocol, device core.Device, key core.RatingKey, caps core.ClientCaps) {
	if viewer == nil || device.ID == "" || len(device.ID) > 1024 {
		return
	}
	if prev, ok := b.rememberedPlayCaps(viewer.ID, protocol, device.ID); ok && capsWiden(caps, prev) {
		if d, ok := b.deps.Playback.(warmDiscarder); ok {
			d.DiscardWarm(viewer, b.playCaps(viewer, protocol, device, prev), key)
		}
	}
	b.storePlayCaps(viewer.ID, protocol, device.ID, caps)
}

func (b *Broker) storePlayCaps(viewer int64, protocol frontend.Protocol, device string, caps core.ClientCaps) {
	caps = copyRequest(frontend.PlanRequest{Caps: caps}).Caps
	now := b.deps.Now()
	key := deviceKey{viewer: viewer, protocol: protocol, device: device}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.playCapsByDevice == nil {
		b.playCapsByDevice = make(map[deviceKey]rememberedPlayCaps)
	}
	if _, ok := b.playCapsByDevice[key]; !ok && len(b.playCapsByDevice) >= maxRememberedPlayCaps {
		var oldest deviceKey
		var at time.Time
		for k, e := range b.playCapsByDevice {
			if at.IsZero() || e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		delete(b.playCapsByDevice, oldest)
	}
	b.playCapsByDevice[key] = rememberedPlayCaps{caps: caps, at: now}
}

func (b *Broker) rememberedPlayCaps(viewer int64, protocol frontend.Protocol, device string) (core.ClientCaps, bool) {
	key := deviceKey{viewer: viewer, protocol: protocol, device: device}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.playCapsByDevice[key]
	if !ok || b.deps.Now().Sub(e.at) > playCapsTTL {
		return core.ClientCaps{}, false
	}
	return copyRequest(frontend.PlanRequest{Caps: e.caps}).Caps, true
}

// WarmDetail implements frontend.DetailWarmer. A device that has not yet
// declared its playback caps on this surface is not warmed, and neither is
// one whose network locality changed since: remote and LAN plays select
// differently, so a warm pick could be lower than Play's.
func (b *Broker) WarmDetail(ctx context.Context, req frontend.PlanRequest) bool {
	if req.Protocol == "" || req.Protocol == "plex" || req.Device.ID == "" || req.Viewer == nil || req.Item == 0 || req.Channel != "" || req.Version != "" || req.Intent.Hints != nil {
		return false
	}
	warm, ok := b.deps.Playback.(core.Prewarmer)
	if !ok {
		return false
	}
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return false
	}
	declared, ok := b.rememberedPlayCaps(v.ID, req.Protocol, req.Device.ID)
	if !ok || declared.Remote != req.Caps.Remote {
		return false
	}
	if req.Caps.ClientIP != "" {
		declared.ClientIP = req.Caps.ClientIP
	}
	if v, err = b.confirm(ctx, v, declared); err != nil {
		return false
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	title, err := lookup(ctx, v, req.Device, req.Item)
	if err != nil || title == nil || title.Key != req.Item || (title.Type != core.TypeMovie && title.Type != core.TypeEpisode) || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return false
	}
	return warm.Prewarm(context.WithoutCancel(ctx), v, req.Item, b.playCaps(v, req.Protocol, req.Device, declared), core.PrewarmOptions{Reason: core.PrewarmDetail, PlayIntent: true})
}
