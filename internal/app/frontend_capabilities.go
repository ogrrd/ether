package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func init() {
	store.Register("frontend_client_families", 1, `CREATE TABLE frontend_client_families (surface TEXT NOT NULL, family TEXT NOT NULL, policy TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(surface,family));`)
}

type surfaceCapabilities struct {
	app     *App
	surface frontend.Protocol
}

func (s surfaceCapabilities) Build(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
	// Protocol is assigned by the host; a client cannot opt into another rule set.
	req.Device.Protocol = s.surface
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return frontend.CapabilityResult{}, core.ErrUnauthorized
	}
	req.Device.Remote = !identity.LAN(ip)
	declared, err := (caps.Builder{}).Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
	if err != nil {
		return frontend.CapabilityResult{}, err
	}
	cfg := s.app.cfg.Read()
	overrides := cfg.Clients.Overrides
	// Plex applies this baseline in its own cap builder. A Jellyfin or Emby
	// client that sent no codec lists gets the same floor. A sent DeviceProfile
	// already filled those lists, and that declaration wins.
	if s.surface != "plex" {
		declared = playbackFloor(declared, overrides)
	}
	policy := transcode.ResolvePolicy(declared, overrides)
	if declared.SubtitleCodecs == nil {
		declared.SubtitleCodecs = append([]string(nil), policy.SubtitleCodecs...)
	}
	if len(declared.DVProfiles) == 0 {
		declared.DVProfiles = append([]int(nil), policy.DolbyVisionProfiles...)
	}
	declared = resolver.ApplyClientRules(cfg.Quality, declared, req.Viewer)
	declared.ClientIP = ip.String()
	result := frontend.CapabilityResult{Client: declared, Quality: resolver.SelectProfile(cfg.Quality, declared, req.Viewer)}
	if req.Viewer != nil && s.surface != "plex" {
		s.app.recordFrontendFamily(ctx, s.surface, declared)
	}
	return result, nil
}

func (s surfaceCapabilities) ApplyClientRules(q frontend.HostQualityConfig, caps core.ClientCaps, viewer *core.Viewer, media string) core.ClientCaps {
	return resolver.ApplyClientRulesForMedia(q, caps, viewer, media)
}

func (a *App) recordFrontendFamily(ctx context.Context, surface frontend.Protocol, caps core.ClientCaps) {
	// Store only the static family policy, not per-device declarations or viewer IDs.
	product := strings.TrimSpace(caps.Product)
	device := caps.Device
	if device == "" {
		device = caps.DeviceName
	}
	if product == "" || len(product) > 128 || len(caps.Platform) > 128 || len(device) > 128 {
		return
	}
	policy := transcode.ClientPolicyFor(core.ClientCaps{Protocol: surface, Product: product, Platform: caps.Platform, Device: device})
	if policy.Family == "generic" || (policy.Family == "Plex Web" && surface != "plex") {
		policy.Family = product
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return
	}
	key := string(surface) + "\x00" + policy.Family
	if prev, ok := a.seenFamilies.Load(key); ok && prev.(string) == string(data) {
		return
	}
	// Bound the number of client-provided family names stored per surface.
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_client_families WHERE surface=?`, string(surface)).Scan(&count); err != nil {
		return
	}
	if count >= 256 {
		if _, err = a.db.ExecContext(ctx, `UPDATE frontend_client_families SET policy=?,last_seen=? WHERE surface=? AND family=?`, string(data), time.Now().Unix(), string(surface), policy.Family); err == nil {
			a.seenFamilies.Store(key, string(data))
		}
		return
	}
	if _, err = a.db.ExecContext(ctx, `INSERT INTO frontend_client_families(surface,family,policy,last_seen) VALUES(?,?,?,?) ON CONFLICT(surface,family) DO UPDATE SET policy=excluded.policy,last_seen=excluded.last_seen`, string(surface), policy.Family, string(data), time.Now().Unix()); err == nil {
		a.seenFamilies.Store(key, string(data))
	}
}

// playbackFloor copies a recognized client's registry codecs onto empty
// lists. The generic preset is not a declaration, so an unknown client stays
// unrestricted. HDR flags stay with the profile: the preset does not certify
// a display, except for native-player apps (see the quirk below).
func playbackFloor(declared core.ClientCaps, overrides []core.ClientCapabilityOverride) core.ClientCaps {
	device := declared.Device
	if device == "" {
		device = declared.DeviceName
	}
	defaults := transcode.PlaybackDefaultsFor(declared.Product, declared.Platform, device, "", declared.Version, overrides)
	if defaults.ID == "" || defaults.ID == "generic" {
		return declared
	}
	if len(declared.VideoCodecs) == 0 {
		declared.VideoCodecs = append([]string(nil), defaults.Video...)
	}
	if len(declared.AudioCodecs) == 0 {
		declared.AudioCodecs = append([]string(nil), defaults.Audio...)
	}
	if len(declared.Containers) == 0 {
		declared.Containers = append([]string(nil), defaults.Containers...)
	}
	if declared.MaxHeight == 0 && !declared.ExplicitMaxHeight && defaults.MaxHeight > 0 {
		declared.MaxHeight = defaults.MaxHeight
	}
	// An app that plays through its own native player (registry quirk
	// native-player-dynamic-range: Stremio desktop/Android, Nuvio) renders
	// HDR itself and tone-maps for an SDR display, so the record's range
	// flags are the player's, not a display claim. An explicit refusal wins.
	if !declared.ExplicitNoHDR && transcode.HasQuirk(declared.Product, declared.Platform, device, "", declared.Version, "native-player-dynamic-range") {
		declared.HDR10 = declared.HDR10 || defaults.HDR10
		declared.DolbyVision = declared.DolbyVision || (defaults.DolbyVision && !defaults.DolbyVisionOff)
	}
	return declared
}

var _ frontend.Capabilities = surfaceCapabilities{}
