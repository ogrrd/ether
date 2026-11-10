package plex

// "Play Version" support (roadmap item #8): official Plex apps render a
// native version picker whenever a <Video> item has several <Media>
// children. This file renders one Media per distinct candidate release on
// detail pages, gives non-Auto variants stable integer Media/Part ids,
// and maps a viewer's selection (mediaIndex on decision/start requests,
// or a variant Part id on direct-play requests) back to the chosen
// release for frontend.VariantPlayback.PrepareVariant.
//
// PMS shapes (python-plexapi plexapi/media.py, plexapi/video.py):
//   - Media carries `title` (the version name Plex apps show for named
//     versions; optimized versions also carry `target`/`proxyType`, which
//     Ether does not claim) — Ether sets `title` to the variant label.
//   - Movie editions use `editionTitle` on the <Video> item — Ether sets
//     it to the Auto variant's cut when the default pick is a cut.
//
// Ids: rating keys already sit near 10^15 with kind digits 1-9 at 10^15.
// Variant ids are issued from 9*10^15 and stay below 2^53 for JavaScript
// clients (the block older notes called 8*10^15). That range overlaps
// KindLocal rating keys. The registry is the authority for a variant;
// a subtitle choice whose part id is in the block but not in the registry
// follows the stream's title instead of that guessed key. The id is a
// hash of (ratingKey, release ID) with
// linear collision probing; mappings are persisted in the variants_ids
// table so old part keys keep working across restarts (without a DB they
// are recomputed deterministically, the same way a Plex stream id is).
// The Auto variant keeps Media.ID == Part.ID == ratingKey, exactly as
// before variants existed.
// Ids unseen for 90 days are pruned (variants_retention.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const variantRenderBudget = 3 * time.Second // incl. the resolver's in-flight wait

// partTitle maps a Part id back to its title: the rating key itself for
// the Auto part, or a registry id for a variant part (with its release).
// releaseID is non-empty only when the registry resolved a variant.
func (s *Server) partTitle(partID int64) (key frontend.RatingKey, releaseID string, ok bool) {
	if s.variantIDs != nil {
		if ref, found := s.variantIDs.lookup(partID); found {
			return ref.key, ref.releaseID, true
		}
	}
	// Local-file keys share this numeric range, so a miss stays a playable
	// key when it is one. Subtitle selection does not trust that guess:
	// handlePartSelect keeps the stream's own title when the part id is in
	// the variant block and the registry did not claim it.
	k := frontend.RatingKey(partID)
	if k.Valid() && isPlayableKey(k) {
		return k, "", true
	}
	return 0, "", false
}

// ---------------------------------------------------------------------------
// Rendering

// mediaForVariants renders versions on detail pages and queues only. It returns (nil, "") to fall back to the single virtual Media:
// no variant resolver wired, nothing cached yet, or only Auto exists.
// The second value is the item's editionTitle, or "".
//
// The list is pinned per viewer, client and title (play_identity.go).
// Ordinary details can gain versions as discovery completes. Explicit
// pre-play and the selected queue item freeze a short-lived flow. Clustered
// flows adopt the committed snapshot before returning metadata.
func (s *Server) mediaForVariants(ctx context.Context, pr *Request, t *frontend.Title, stamp int64, probed frontend.MediaInfo, probedRelease string, ps *frontend.PlaySession) ([]model.Media, string) {
	if s.variants == nil || pr.User == nil {
		return nil, ""
	}
	ik := s.playIdentityFor(pr, t.Key)
	live := ps != nil
	now := s.now()
	caps := s.clientCapsForKey(pr, t.Key)
	if s.versionFlows != nil && pr.versionItemKey == t.Key {
		snap, err := s.versionFlows.call(ctx, ik, nil, versionRead, now, 0)
		if err != nil {
			pr.versionErr = err
			return nil, ""
		}
		if snap.frozen {
			s.playIDs.installVersionFlow(ik, snap.variants, snap.until, now)
		} else if !live {
			// The shared flow ended or expired: unfreeze, but keep the
			// issued list (see endVersionFlow) so a restart re-read still
			// answers with the Media/Part ids the client holds (#389).
			s.playIDs.mu.Lock()
			if e := s.playIDs.m[ik]; e != nil && e.admitting == 0 {
				e.frozen = false
				e.flowUntil = time.Time{}
			}
			s.playIDs.mu.Unlock()
		}
	}
	pin := s.playIDs.pinned(ik, live, now)
	var vs []frontend.ReleaseVariant
	if (pin.frozen || pr.versionFlowKey == t.Key) && pin.resolved {
		if len(pin.variants) < 2 {
			if !live {
				s.watchVariants(pr, t)
			}
			return nil, ""
		}
		vs = cloneVariants(pin.variants)
		if pr.versionFlowKey == t.Key && !pin.frozen {
			if s.versionFlows != nil && pr.versionItemKey == t.Key {
				snap, err := s.versionFlows.call(ctx, ik, vs, versionFreeze, now, versionPreplayTTL)
				if err != nil {
					pr.versionErr = err
					return nil, ""
				}
				vs = snap.variants
				s.playIDs.installVersionFlow(ik, vs, snap.until, now)
			} else {
				s.playIDs.installVersionFlow(ik, vs, now.Add(versionPreplayTTL), now)
			}
		}
		// The resolver names Auto after the release Auto actually opened.
		// Ask once for the admitted release, not again after a failover.
		rel := pin.admittedRelease
		if rel == "" {
			rel = probedRelease
		}
		if rel != "" && pin.autoAligned != rel && vs[0].Auto && vs[0].Release.ID != rel {
			vctx, cancel := detailVariantContext(ctx, pr)
			resolved, err := s.variants.Variants(vctx, frontend.ResolveRequest{Title: t, Client: caps, User: pr.User, PlayedRelease: rel}, frontend.MaxVariants)
			cancel()
			var auto *frontend.ReleaseVariant
			if err == nil && len(resolved) > 0 && resolved[0].Auto && resolved[0].Release.ID == rel {
				auto = &resolved[0]
				vs[0] = resolved[0]
			}
			s.playIDs.alignAuto(ik, auto, rel)
		}
	} else {
		vctx, cancel := detailVariantContext(ctx, pr)
		resolved, err := s.variants.Variants(vctx, frontend.ResolveRequest{Title: t, Client: caps, User: pr.User, PlayedRelease: probedRelease}, frontend.MaxVariants)
		cancel()
		if err != nil {
			s.log.Debug("variants unavailable", "key", t.Key, "err", err)
			resolved = nil
		}
		if s.versionFlows != nil && pr.versionItemKey == t.Key {
			mode := versionOffer
			if pr.versionFlowKey == t.Key {
				mode = versionFreeze
			}
			snap, err := s.versionFlows.call(ctx, ik, resolved, mode, now, versionPreplayTTL)
			if err != nil {
				pr.versionErr = err
				return nil, ""
			}
			if snap.frozen {
				s.playIDs.installVersionFlow(ik, snap.variants, snap.until, now)
				resolved = snap.variants
			} else if len(resolved) < 2 && len(snap.variants) >= 2 {
				resolved = snap.variants
			}
		} else if pr.versionFlowKey == t.Key {
			s.playIDs.installVersionFlow(ik, resolved, now.Add(versionPreplayTTL), now)
		}
		s.playIDs.issueList(ik, resolved, live, now)
		// Read back the winner: a hub/detail render may have issued its
		// list concurrently. Never return a list we did not retain.
		pin = s.playIDs.pinned(ik, live, now)
		if pin.resolved {
			resolved = pin.variants
		}
		if len(resolved) < 2 {
			if !live {
				s.watchVariants(pr, t)
			}
			return nil, ""
		}
		vs = cloneVariants(resolved)
	}

	alignRelease := probedRelease
	if pin.frozen && pin.admittedRelease != "" {
		alignRelease = pin.admittedRelease
	}
	alignPlayedAutoVariant(vs, alignRelease)
	probedReady := !probed.Estimated && len(probed.Streams) > 0 && probed.Video() != nil
	// playedIdx is the Media the session plays: the one it was admitted on
	// when known, otherwise the version whose release is playing (Auto
	// when the playing release is not one of the explicit non-Auto picks,
	// e.g. Auto resolved past an uncached top candidate).
	playedIdx := -1
	if pin.frozen && pin.selected != 0 {
		for i, v := range vs {
			if s.variantMediaID(t, v) == pin.selected {
				playedIdx = i
				break
			}
		}
	}
	admittedIdx := playedIdx
	if playedIdx < 0 && probedRelease != "" {
		playedIdx = 0
		if probedReady && probedRelease != vs[0].Release.ID {
			for i, v := range vs {
				if !v.Auto && v.Release.ID == probedRelease {
					playedIdx = i
					break
				}
			}
		}
	}
	matchedNonAuto := playedIdx > 0
	autoDuplicatesRow := false
	for _, v := range vs[1:] {
		if sameVariantRelease(v.Release, vs[0].Release) {
			autoDuplicatesRow = true
			break
		}
	}
	stored := s.selections.get(pr.User.ID, int64(t.Key))
	out := make([]model.Media, 0, len(vs))
	for i, v := range vs {
		// The played version carries the session's probe; before playback
		// Auto carries the completed detail warm-up probe when available,
		// otherwise its estimate. A probe is drawn on the played version
		// only, never on a different explicit pick.
		played := probedReady && i == playedIdx
		warmAuto := playedIdx < 0 && probedReady && v.Auto
		variant := v
		if played || warmAuto {
			keepReleaseBitrate := !probeDisagrees(v.Info, probed) && v.Info.BitrateKbps > 0 && (probedRelease != "" || autoDuplicatesRow || probed.BitrateKbps <= 0)
			variant.Info = mergeProbedVariantInfo(v.Info, probed, keepReleaseBitrate)
		} else if !v.Auto && sameVariantRelease(v.Release, vs[0].Release) && probedReady && !matchedNonAuto && !probeDisagrees(v.Info, probed) {
			variant.Info = mergeProbedVariantInfo(v.Info, probed, v.Info.BitrateKbps > 0)
		}
		variant.Info = variantInfoForClient(variant.Info, caps)
		m := buildMediaVariant(t, variant, stamp, s.variantIDs)
		m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, now)
		if s.previewAvailable(pr, t.Key, v.Release.ID) {
			m.Part[0].Indexes = "sd"
		}
		// Every version carries a stream list, as on PMS; versions not yet
		// played show their estimate. Plex Web picks the version itself
		// (highest playable quality, ignoring selected) and reads the
		// playing Part's streams: without a video stream it adds no "None"
		// subtitle track, and its player settings panel then reads the id
		// of an undefined selected subtitle and ends playback (#267).
		{
			info := s.withCachedSidecars(ctx, pr.User, t, variant.Info)
			if stored.subtitleSet {
				info = infoWithSelectedSidecar(info, stored.subtitleStream)
			}
			sel := resolveSelection(stored, info)
			sel = s.preferredSelection(ctx, pr.User, t.Key, info, t.Language, sel)
			m.Part[0].Stream = s.issuedStreams(t.Key, ik, m.ID, info, sel, pr.User, pr)
		}
		// The admitted version stays selected while its probe is pending,
		// so the selection does not move from Auto to it once probed.
		if played || i == admittedIdx {
			m.Selected = model.Ptr(true)
			m.Part[0].Selected = model.Ptr(true)
		}
		// Auto's title is the release-name estimate. When the file that
		// actually played or warmed is a different resolution or codec,
		// keep the parsed release metadata (source, group) and replace only
		// what the probe proves wrong.
		if v.Auto && (played || warmAuto) && probeDisagrees(v.Info, probed) {
			m.Extra = []model.Attr{model.A("title", probedAutoTitle(v.Release, v.Info, probed))}
		}
		out = append(out, m)
	}
	// A recorded PMS movie has one Media and does not set Media@selected
	// (testdata/conform/plex/web movie metadata). A recorded Plex Web
	// decision (hasMDE=1) is the one Media the client already chose, marked
	// selected="1". Search and library rows show the first Media, which is
	// Auto, so a 720p profile reads as 720p there. Plex Web then walks the
	// Play Version list itself and requests that part. With several versions
	// and nothing resumed, mark Auto so a client that honors selected starts
	// there. Clients that ignore it and take the tallest Media stay inside
	// the profile because taller versions are not listed unless the owner
	// opts in.
	if !anyMediaSelected(out) && len(out) > 0 {
		out[0].Selected = model.Ptr(true)
		if len(out[0].Part) > 0 {
			out[0].Part[0].Selected = model.Ptr(true)
		}
	}
	edition := ""
	if t.Type == frontend.TypeMovie {
		edition = vs[0].Edition
	}
	return out, edition
}

// variantMediaID is the Media/Part id buildMediaVariant gives v.
func (s *Server) variantMediaID(t *frontend.Title, v frontend.ReleaseVariant) int64 {
	if v.Auto || s.variantIDs == nil {
		return int64(t.Key)
	}
	return s.variantIDs.idFor(t.Key, v.Release.ID)
}

// buildMediaVariant renders one variant's Media/Part. Auto keeps the
// historical ids (ratingKey); other variants get their registry id and
// the variant label as the Media title Plex apps show in "Play Version".
func anyMediaSelected(media []model.Media) bool {
	for i := range media {
		if media[i].Selected != nil && *media[i].Selected {
			return true
		}
	}
	return false
}

// variantInfoForClient describes the compatible HDR10 base layer on multi-
// version lists when this client lacks the stream's Dolby Vision profile (or
// has Dolby Vision turned off) and Ether will strip Dolby Vision RPUs into an
// HDR10 remux. Without this, Bedrock MDE clients with a DOVIProfile limitation
// (e.g. 2017 NVIDIA Shield darcy) reject the probed 4K Auto version and hijack
// playback onto an unprobed lower-compatibility version with no subtitle list.
func variantInfoForClient(info frontend.MediaInfo, caps frontend.ClientCaps) frontend.MediaInfo {
	v := info.Video()
	if v == nil || !v.DolbyVision {
		return info
	}
	if (frontend.SupportsDVProfile(caps, v.DolbyVisionProfile) && !caps.DolbyVisionBaseOnly) || !frontend.HasHDR10Base(v, caps) {
		return info
	}
	streams := append([]frontend.StreamInfo(nil), info.Streams...)
	for i := range streams {
		if streams[i].Type == "video" && streams[i].DolbyVision {
			streams[i].DolbyVision = false
			streams[i].DolbyVisionProfile = 0
			streams[i].DolbyVisionLevel = 0
			streams[i].DolbyVisionCompatibilityID = 0
			streams[i].DolbyVisionVersion = ""
			streams[i].DolbyVisionBLPresent = nil
			streams[i].DolbyVisionELPresent = nil
			streams[i].DolbyVisionRPUPresent = nil
		}
	}
	info.Streams = streams
	return info
}

func buildMediaVariant(t *frontend.Title, v frontend.ReleaseVariant, stamp int64, registry *variantIDRegistry) model.Media {
	m := buildMedia(t, v.Info, stamp)
	var id int64
	if v.Auto || registry == nil {
		id = int64(t.Key)
	} else {
		id = registry.idFor(t.Key, v.Release.ID)
	}
	if id != int64(t.Key) {
		ext := containerExt(plexContainer(v.Info.Container))
		m.ID = id
		m.Part[0].ID = id
		m.Part[0].Key = fmt.Sprintf("/library/parts/%d/%d/file.%s", id, stamp, ext)
	}
	if v.Label != "" {
		m.Extra = []model.Attr{model.A("title", v.Label)}
	}
	return m
}

// applyEditionTitle sets the item's editionTitle (PMS's edition label for
// movies) when the default version is a distinct cut.
func applyEditionTitle(m *model.Metadata, edition string) {
	if edition == "" {
		return
	}
	m.Extra = append(m.Extra, model.A("editionTitle", edition))
}

// ---------------------------------------------------------------------------
// Selection → playback

// partIDFromPath extracts the numeric Part id from a
// "/library/parts/{id}/..." path, as sent by clients that address the
// chosen version's part key in decision/start requests.
func partIDFromPath(p string) (int64, bool) {
	i := strings.Index(p, "/library/parts/")
	if i < 0 {
		return 0, false
	}
	p = p[i+len("/library/parts/"):]
	p, _, _ = strings.Cut(p, "/")
	p, _, _ = strings.Cut(p, "?")
	id, err := strconv.ParseInt(p, 10, 64)
	return id, err == nil
}

// resolveVariantSelection maps a positional choice only through the list
// issued to this client. Unknown/stale positions use Auto; a stable Part id
// identifies its explicit release even when the positional list is gone.
func (s *Server) resolveVariantSelection(ctx context.Context, pr *Request, p *playRequest) string {
	if p.partReleaseID != "" {
		return p.partReleaseID
	}
	if p.mediaIndex <= 0 || pr.User == nil {
		return ""
	}
	if ids, ok := s.playIDs.releases(s.playIdentityFor(pr, p.key), s.now()); ok {
		if p.mediaIndex < len(ids) {
			return ids[p.mediaIndex]
		}
		s.log.Info("variant mediaIndex out of range; playing auto version",
			"key", p.key, "mediaIndex", p.mediaIndex, "rendered", len(ids))
		return ""
	}
	// A position without its issued list is ambiguous (restart, idle expiry
	// or an old stored preference). Recomputing against today's candidates
	// can silently play another release. Explicit Part ids remain stable.
	return ""
}

// alignPlayedAutoVariant updates Auto in place when the played release is a
// same-tier alternative in vs[1:] and the VariantResolver did not already
// align vs[0] (e.g. a static test stub).
func alignPlayedAutoVariant(vs []frontend.ReleaseVariant, playedRelease string) {
	if playedRelease == "" || len(vs) < 2 || !vs[0].Auto || vs[0].Release.ID == playedRelease {
		return
	}
	autoClass := streamResolutionClass(vs[0].Info)
	for _, v := range vs[1:] {
		if v.Release.ID != playedRelease {
			continue
		}
		if v.Edition != vs[0].Edition || streamResolutionClass(v.Info) != autoClass || hasDolbyVision(v.Info) != hasDolbyVision(vs[0].Info) || hasHDR10(v.Info) != hasHDR10(vs[0].Info) {
			return
		}
		lbl := v.Label
		if v.Release.Name != "" || v.Release.Info.Height > 0 || v.Release.Info.Source != "" {
			lbl = plexAutoLabel(v.Release.Info)
		} else if lbl != "" && !strings.HasPrefix(lbl, "Auto") {
			lbl = "Auto · " + lbl
		}
		vs[0] = frontend.ReleaseVariant{
			ID:      v.Release.ID,
			Label:   lbl,
			Edition: v.Edition,
			Auto:    true,
			Release: v.Release,
			Info:    v.Info,
		}
		return
	}
}

func sameVariantRelease(a, b frontend.Release) bool {
	if a.ID != "" && a.ID == b.ID {
		return true
	}
	na := strings.TrimSpace(a.Name)
	nb := strings.TrimSpace(b.Name)
	return na != "" && strings.EqualFold(na, nb)
}

func mergeProbedVariantInfo(claimed, probed frontend.MediaInfo, keepReleaseBitrate bool) frontend.MediaInfo {
	out := probed
	if keepReleaseBitrate && claimed.BitrateKbps > 0 {
		out.BitrateKbps = claimed.BitrateKbps
	}
	if out.Size <= 0 && claimed.Size > 0 {
		out.Size = claimed.Size
	}
	if out.Duration <= 0 && claimed.Duration > 0 {
		out.Duration = claimed.Duration
	}
	return out
}

func hasDolbyVision(info frontend.MediaInfo) bool {
	v := info.Video()
	return v != nil && v.DolbyVision
}

func hasHDR10(info frontend.MediaInfo) bool {
	v := info.Video()
	return v != nil && (v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67")
}

// probeDisagrees reports that the file which played is not the picture or
// audio the release name (or its stand-in) described.
func probeDisagrees(claimed, probed frontend.MediaInfo) bool {
	if probed.Video() == nil {
		return false
	}
	if streamResolutionClass(claimed) != streamResolutionClass(probed) {
		return true
	}
	return audioCodecFamily(primaryAudioCodec(claimed)) != audioCodecFamily(primaryAudioCodec(probed))
}

func streamResolutionClass(info frontend.MediaInfo) int {
	if v := info.Video(); v != nil {
		return resolutionClass(v.Width, v.Height)
	}
	return 0
}

func resolutionClass(w, h int) int {
	tier := frontend.ResolutionTier(w, h)
	if tier > 0 && tier < 720 {
		return 480
	}
	return tier
}

func audioCodecFamily(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "e-ac3", "ec3", "eac3":
		return "eac3"
	case "ac-3", "ac3":
		return "ac3"
	case "dca", "dts", "dtshd", "dts-hd", "dca-ma":
		return "dts"
	case "truehd", "mlp":
		return "truehd"
	case "mp4a", "aac":
		return "aac"
	default:
		return strings.ToLower(strings.TrimSpace(c))
	}
}

func primaryAudioCodec(info frontend.MediaInfo) string {
	fallback := ""
	for _, st := range info.Streams {
		if st.Type != "audio" || st.Codec == "" {
			continue
		}
		if st.Default || fallback == "" {
			fallback = strings.ToLower(st.Codec)
			if st.Default {
				return fallback
			}
		}
	}
	return fallback
}

// probedAutoTitle keeps the release-name metadata (source, group, audio
// details that still match) and replaces only what the probe proves wrong.
func probedAutoTitle(rel frontend.Release, claimed, probed frontend.MediaInfo) string {
	info := rel.Info
	probedClass := streamResolutionClass(probed)
	if streamResolutionClass(claimed) != probedClass || info.Height <= 0 {
		switch probedClass {
		case 2160, 1080, 720:
			info.Height = probedClass
		default:
			info.Height = 0
		}
		if v := probed.Video(); v != nil {
			info.DolbyVision = v.DolbyVision
			info.HDR10 = v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67"
		}
	}
	probedAudio := primaryAudioCodec(probed)
	if audioCodecFamily(primaryAudioCodec(claimed)) != audioCodecFamily(probedAudio) || info.AudioCodec == "" {
		info.AudioCodec = audioCodecFamily(probedAudio)
		info.Atmos = false
		info.Channels = ""
	}
	return plexAutoLabel(info)
}

func plexAutoLabel(info frontend.ReleaseInfo) string {
	parts := []string{"Auto", plexQualityPhrase(info)}
	if a := plexAudioPhrase(info); a != "" {
		parts = append(parts, a)
	}
	if g := strings.TrimSpace(info.Group); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}

func plexQualityPhrase(info frontend.ReleaseInfo) string {
	res := "SD"
	switch {
	case info.Height >= 1800:
		res = "4K"
	case info.Height >= 1000:
		res = "1080p"
	case info.Height >= 700:
		res = "720p"
	case info.Height > 0:
		res = strconv.Itoa(info.Height) + "p"
	}
	parts := []string{res}
	switch {
	case info.DolbyVision:
		parts = append(parts, "Dolby Vision")
	case info.HDR10:
		parts = append(parts, "HDR10")
	}
	if source := frontend.SourceLabel(info.Source); source != "" {
		parts = append(parts, source)
	}
	return strings.Join(parts, " ")
}

func plexAudioPhrase(info frontend.ReleaseInfo) string {
	codec := map[string]string{
		"aac": "AAC", "ac3": "DD", "eac3": "DD+", "truehd": "TrueHD",
		"dts": "DTS", "dtshd": "DTS-HD", "flac": "FLAC", "opus": "Opus", "mp3": "MP3",
	}[info.AudioCodec]
	if codec == "" {
		codec = strings.ToUpper(info.AudioCodec)
	}
	parts := []string{}
	if codec != "" {
		parts = append(parts, codec)
	}
	if info.Atmos {
		parts = append(parts, "Atmos")
	}
	if info.Channels != "" {
		parts = append(parts, info.Channels)
	}
	return strings.Join(parts, " ")
}

// preparePlayback prepares a play session, honouring a "Play Version"
// pick when the playback manager supports it.
func (s *Server) preparePlayback(ctx context.Context, pr *Request, p *playRequest) (*frontend.PlaySession, error) {
	if s.versionFlows != nil {
		ik := s.playIdentityFor(pr, p.key)
		// Freeze the list this client was issued, not an empty one: when
		// the shared row has expired (90s pre-play TTL) or was never
		// offered, freezing empty erased the issued identities, so the
		// request's mediaIndex pointed outside the list and later renders
		// collapsed to one Media (#389). An unexpired offered row still
		// wins in the store; the local list only fills an absent one.
		issued := s.playIDs.pinned(ik, true, s.now()).variants
		snap, err := s.versionFlows.call(ctx, ik, issued, versionFreeze, s.now(), playIdentityTTL)
		if err != nil {
			return nil, err
		}
		s.playIDs.installVersionFlow(ik, snap.variants, snap.until, s.now())
	}
	if p.mediaIndex > 0 {
		if releases, issued := s.playIDs.releases(s.playIdentityFor(pr, p.key), s.now()); !issued || p.mediaIndex >= len(releases) {
			return nil, errDecisionMediaIndex
		}
	}
	releaseID := s.resolveVariantSelection(ctx, pr, p)
	p.variantRelease = releaseID
	ps, err := s.prepareForRelease(ctx, pr.User, p.key, p.caps, releaseID)
	if err != nil {
		s.endVersionFlow(ctx, s.playIdentityFor(pr, p.key))
	}
	return ps, err
}

// admittedMedia is the Media id a prepared request plays: the Part it
// addressed, the Play Version it picked by position, or Auto.
func (s *Server) admittedMedia(p *playRequest) int64 {
	if p.requestedPart != 0 {
		if k, rel, ok := s.partTitle(p.requestedPart); ok && k == p.key {
			if rel == "" {
				return int64(p.key)
			}
			return p.requestedPart
		}
	}
	if p.variantRelease != "" && p.mediaIndex > 0 && s.variantIDs != nil {
		return s.variantIDs.idFor(p.key, p.variantRelease)
	}
	return int64(p.key)
}

// prepareForRelease prepares a session on a specific release ("" = Auto).
// When the playback manager cannot target a release, it logs and plays
// the normal choice instead of failing.
func (s *Server) prepareForRelease(ctx context.Context, user *frontend.User, key frontend.RatingKey, caps frontend.ClientCaps, releaseID string) (*frontend.PlaySession, error) {
	return s.plexSources().Prepare(ctx, user, key, caps, releaseID, decisionSourceWait)
}

// variantWatchSchedule is when a detail page rendered with a single version
// checks again for more: the release search the page view started usually
// finishes within seconds, and Plex apps load a detail page only once.
var variantWatchSchedule = []time.Duration{3 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second}

// maxVariantWatches bounds the follow-ups running at once.
const maxVariantWatches = 64

// variantWatches tracks the follow-ups in flight, one per viewer, client
// and title.
type variantWatches struct {
	mu      sync.Mutex
	pending map[string]bool
}

// watchVariants follows a detail page that showed one version because the
// title's search was not cached yet (#214). When a version list appears,
// a library timeline entry tells the open page to reload, as PMS does on a
// metadata change, so Play Version is there without leaving the page.
func (s *Server) watchVariants(pr *Request, t *frontend.Title) {
	if s.variants == nil || pr == nil || pr.User == nil || t == nil {
		return
	}
	key := fmt.Sprintf("%d|%s|%d", pr.User.ID, pr.ClientID, t.Key)
	s.variantWatch.mu.Lock()
	if s.variantWatch.pending == nil {
		s.variantWatch.pending = map[string]bool{}
	}
	if s.variantWatch.pending[key] || len(s.variantWatch.pending) >= maxVariantWatches {
		s.variantWatch.mu.Unlock()
		return
	}
	s.variantWatch.pending[key] = true
	s.variantWatch.mu.Unlock()
	caps := s.clientCapsForKey(pr, t.Key)
	user, title := *pr.User, *t
	// If the two refill slots were occupied, retry admission while this
	// page is being watched. Once admitted, never retry a failed search.
	retryAdmission := pr.detailRefillRequested && pr.detailRefill == nil
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.variantWatch.mu.Lock()
			delete(s.variantWatch.pending, key)
			s.variantWatch.mu.Unlock()
		}()
		for _, wait := range variantWatchSchedule {
			timer := time.NewTimer(wait)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if retryAdmission && s.detailSearchAllowed(pr) && s.variants != nil {
				retryAdmission = s.variants.RefillSearchCache(&title) == nil
			}
			vctx, cancel := context.WithTimeout(s.ctx, variantRenderBudget)
			vs, err := s.variants.Variants(vctx, frontend.ResolveRequest{Title: &title, Client: caps, User: &user}, frontend.MaxVariants)
			cancel()
			if err == nil && len(vs) >= 2 {
				s.notifyTimeline(title.Key, title.Type, "", user.ID)
				return
			}
		}
	}()
}

var errDecisionMediaIndex = errors.New("mediaIndex outside issued media list")

// Foreground version discovery shares the TV detail probe's wait allowance.
// A slow probe must not be followed by another three-second discovery wait.
func detailVariantContext(ctx context.Context, pr *Request) (context.Context, context.CancelFunc) {
	budget := variantRenderBudget
	if pr != nil && !pr.detailWaitUntil.IsZero() {
		budget = min(budget, time.Until(pr.detailWaitUntil))
	}
	return context.WithTimeout(ctx, budget)
}
