package plex

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// baseCaps is what a product/platform can play when the client sends no
// profile. The values come from the capability registry. Profile-extra
// directives refine them afterwards.
type baseCaps struct {
	video        []string
	audio        []string
	containers   []string
	maxHeight    int
	hdr10        bool
	dv           bool
	dvOff        bool // owner override turned Dolby Vision off
	heightLocked bool
}

// ClientCaps derives frontend.ClientCaps for the request. remoteMaxKbps caps
// the bitrate of remote clients (0 = unlimited).
func (pr *Request) ClientCaps(remoteMaxKbps int) frontend.ClientCaps {
	return pr.ClientCapsWith(remoteMaxKbps, nil)
}

// ClientCapsWith is ClientCaps with owner registry overrides applied before
// the request's own profile and capability headers.
func (pr *Request) ClientCapsWith(remoteMaxKbps int, overrides []frontend.ClientCapabilityOverride) frontend.ClientCaps {
	if pr == nil {
		return frontend.ClientCaps{}
	}
	// Plex Web's browser split, desktop mpv clients, Plezy's platform split
	// and the Fire TV 4K Max model patch all live in the registry. An owner
	// override sits under the headers this request actually sent.
	b := playbackCaps(pr, overrides)
	c := frontend.ClientCaps{
		Protocol:        "plex",
		Product:         pr.Product,
		Platform:        pr.Platform,
		PlatformVersion: pr.PlatformVersion,
		Device:          pr.Device,
		DeviceName:      pr.DeviceName,
		ClientID:        pr.ClientID,
		Version:         pr.Version,
		MaxHeight:       b.maxHeight,
		HDR10:           b.hdr10,
		DolbyVision:     b.dv,
		VideoCodecs:     slices.Clone(b.video),
		AudioCodecs:     slices.Clone(b.audio),
		Containers:      slices.Clone(b.containers),
		Remote:          pr.Remote,
	}
	if pr.RemoteIP.IsValid() {
		c.ClientIP = pr.RemoteIP.String()
	}
	if b.heightLocked {
		c.ExplicitMaxHeight = true
	}
	if b.dvOff {
		c.DolbyVisionBaseOnly = true
	}
	dv, subs, dvChanged, subsChanged := frontend.PolicyStamp(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, overrides)
	if dvChanged {
		c.DVProfiles = dv
	}
	if subsChanged {
		c.SubtitleCodecs = subs
	}
	if pr.ClientCapabilities != "" {
		applyLegacyCapabilities(&c, pr.ClientCapabilities)
	}
	if pr.ProfileExtra != "" {
		ev := applyProfileExtra(&c, pr.ProfileExtra)
		promoteWebHDRFromProfile(&c, pr.Product, ev)
	}
	if c.Remote && remoteMaxKbps > 0 && (c.MaxBitrateKbps == 0 || c.MaxBitrateKbps > remoteMaxKbps) {
		if c.MaxBitrateKbps == 0 {
			c.DefaultRemoteBitrate = true
		}
		c.MaxBitrateKbps = remoteMaxKbps
	}
	return c
}

func playbackCaps(pr *Request, overrides []frontend.ClientCapabilityOverride) baseCaps {
	got := frontend.PlaybackDefaultsFor(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, overrides)
	return baseCaps{video: got.Video, audio: got.Audio, containers: got.Containers, maxHeight: got.MaxHeight, hdr10: got.HDR10, dv: got.DolbyVision, dvOff: got.DolbyVisionOff, heightLocked: got.HeightLocked}
}

// declaredCaps is the registry plus this request's headers, before a quality
// rule changes the ceiling.
func (s *Server) declaredCaps(pr *Request) frontend.ClientCaps {
	if pr == nil {
		return frontend.ClientCaps{}
	}
	return pr.ClientCapsWith(s.cfg().Transcode.RemoteMaxBitrateKbps, s.cfg().Clients.Overrides)
}

// profileExtraEvidence records affirmative codec/HDR claims and HDR denials
// from X-Plex-Client-Profile-Extra. Omission is not support: listing hevc
// alone never implies HDR10 (Braedon review of #136).
type profileExtraEvidence struct {
	affirmativeHEVC  bool
	affirmativeHDR10 bool // bitDepth≥10 and/or colorTrc allow-list includes smpte2084
	hdrDenied        bool
}

// promoteWebHDRFromProfile enables HDR10 remux for Plex Web when profile-extra
// affirmatively listed hevc and also claimed 10-bit/PQ, with no HDR denial.
// Matches the Profile-Extra Plex Web sends once it knows the title is HDR10
// HEVC (bitDepth upperBound=10 and/or colorTrc allow-list including smpte2084).
// HEVC alone never implies HDR10 (Braedon #136); browser name alone never
// promotes. An early thin Profile-Extra may still tone-map; a later start with
// the full report remuxes.
func promoteWebHDRFromProfile(c *frontend.ClientCaps, product string, ev profileExtraEvidence) {
	if !strings.Contains(strings.ToLower(product), "plex web") {
		return
	}
	if ev.affirmativeHEVC {
		c.VideoCodecs = addUnique(c.VideoCodecs, "hevc")
	}
	if !ev.affirmativeHEVC || !ev.affirmativeHDR10 || ev.hdrDenied {
		return
	}
	c.HDR10 = true
}

// limitationDeniesHDR reports profile-extra limits that mean this client
// cannot take HEVC HDR10 (8-bit cap or explicit PQ rejection).
func limitationDeniesHDR(a map[string]string) bool {
	sn := normalizeCodec(a["scopeName"])
	if sn != "hevc" && sn != "av1" && sn != "*" && sn != "" {
		return false
	}
	name, typ := a["name"], a["type"]
	val := limitationValue(a)
	switch a["scope"] {
	case "videoCodec":
		if name == "video.bitDepth" && typ == "upperBound" {
			n, err := strconv.Atoi(val)
			return err == nil && n < 10
		}
		if name == "video.colorTrc" && typ == "notMatch" {
			return strings.Contains(val, "smpte2084")
		}
	}
	return false
}

// limitationAffirmsHDR10 reports profile-extra limits that affirmatively claim
// 10-bit and/or PQ (HDR10) support — the signals PMS Firefox sent when it
// Direct Streamed an HDR10 HEVC remux.
func limitationAffirmsHDR10(a map[string]string) bool {
	sn := normalizeCodec(a["scopeName"])
	if sn != "hevc" && sn != "av1" && sn != "*" && sn != "" {
		return false
	}
	name, typ, scope := a["name"], a["type"], a["scope"]
	val := limitationValue(a)
	if name == "video.bitDepth" && typ == "upperBound" && scope == "videoCodec" {
		n, err := strconv.Atoi(val)
		return err == nil && n >= 10
	}
	// PMS: scope=videoTranscodeTarget&type=match&name=video.colorTrc&list=...|smpte2084
	if name == "video.colorTrc" && typ == "match" && (scope == "videoCodec" || scope == "videoTranscodeTarget") {
		return strings.Contains(val, "smpte2084")
	}
	return false
}

// limitationValue prefers list= (PMS colorTrc allow-lists) then value=.
func limitationValue(a map[string]string) string {
	if v := a["list"]; v != "" {
		return v
	}
	return a["value"]
}

// clientCaps derives frontend.ClientCaps for the request and applies any
// matching client rule override from the server's quality configuration.
// The override stays behind Host.Capabilities so this package does not import the resolver.
func (s *Server) clientCaps(pr *Request) frontend.ClientCaps {
	return s.clientCapsForKey(pr, 0)
}

func (s *Server) clientCapsForKey(pr *Request, key frontend.RatingKey) frontend.ClientCaps {
	cfg := s.cfg()
	caps := s.declaredCaps(pr)
	if s.host.Capabilities == nil {
		return caps
	}
	media := frontend.MediaAny
	switch key.Kind() {
	case frontend.KindMovie:
		media = frontend.MediaMovie
	case frontend.KindEpisode:
		media = frontend.MediaTV
	}
	return s.host.Capabilities.ApplyClientRules(cfg.Quality, caps, pr.User, media)
}

// Directive is one parsed X-Plex-Client-Profile-Extra directive, e.g.
// add-limitation(scope=videoCodec&scopeName=h264&...).
type Directive struct {
	Verb string
	Args map[string]string
}

// ParseProfileExtra splits a profile-extra string into directives. The value
// may arrive URL-encoded; directives are separated by "+".
func ParseProfileExtra(s string) []Directive {
	// Encoded form: "add-limitation%28scope%3D...%29%2B...". PathUnescape
	// keeps a literal "+" (the directive separator); when the value came
	// through a query string "+" has already become a space, which the verb
	// trimming below tolerates.
	if !strings.Contains(s, "(") {
		if dec, err := url.PathUnescape(s); err == nil {
			s = dec
		}
	}
	var out []Directive
	for len(s) > 0 {
		open := strings.IndexByte(s, '(')
		if open < 0 {
			break
		}
		close := strings.IndexByte(s[open:], ')')
		if close < 0 {
			break
		}
		close += open
		verb := strings.Trim(strings.TrimSpace(s[:open]), "+ ")
		args := map[string]string{}
		for _, kv := range strings.Split(s[open+1:close], "&") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if dec, err := url.QueryUnescape(v); err == nil {
				v = dec
			}
			args[strings.TrimSpace(k)] = v
		}
		if verb != "" {
			out = append(out, Directive{Verb: verb, Args: args})
		}
		s = s[close+1:]
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = normalizeCodec(strings.TrimSpace(p))
		if p != "" && p != "*" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeCodec maps Plex/legacy codec names to Ether's core names.
func normalizeCodec(c string) string {
	c = strings.ToLower(c)
	switch c {
	case "h265", "hev1", "hvc1", "x265":
		return "hevc"
	case "avc", "avc1", "x264":
		return "h264"
	case "dca":
		return "dts"
	case "dca-ma", "dts-hd", "dtshd_ma":
		return "dtshd"
	case "e-ac3", "ec3":
		return "eac3"
	case "ts", "mpegts", "m2ts":
		return "mpegts"
	case "matroska":
		return "mkv"
	case "m4v":
		return "mp4"
	}
	return c
}

func addUnique(xs []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(xs, a) {
			xs = append(xs, a)
		}
	}
	return xs
}

func applyProfileExtra(c *frontend.ClientCaps, extra string) profileExtraEvidence {
	var ev profileExtraEvidence
	for _, d := range ParseProfileExtra(extra) {
		a := d.Args
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
			if t := a["type"]; t != "" && t != "videoProfile" {
				continue
			}
			codecs := splitList(a["videoCodec"])
			if slices.Contains(codecs, "hevc") {
				ev.affirmativeHEVC = true
			}
			c.VideoCodecs = addUnique(c.VideoCodecs, codecs...)
			c.AudioCodecs = addUnique(c.AudioCodecs, splitList(a["audioCodec"])...)
			if ct := a["container"]; ct != "" {
				c.Containers = addUnique(c.Containers, splitList(ct)...)
				if a["protocol"] == frontend.ProtocolHLS {
					c.HLSContainers = addUnique(c.HLSContainers, splitList(ct)...)
				}
			}
		case "add-direct-play-profile":
			if t := a["type"]; t != "" && t != "videoProfile" {
				continue
			}
			codecs := splitList(a["videoCodec"])
			if slices.Contains(codecs, "hevc") {
				ev.affirmativeHEVC = true
			}
			c.VideoCodecs = addUnique(c.VideoCodecs, codecs...)
			c.AudioCodecs = addUnique(c.AudioCodecs, splitList(a["audioCodec"])...)
			c.Containers = addUnique(c.Containers, splitList(a["container"])...)
		case "add-limitation":
			if (strings.EqualFold(c.Product, "PM4K") || strings.EqualFold(c.Product, "Plex for Kodi")) && a["scope"] == "videoAudioCodec" {
				n, err := strconv.Atoi(limitationValue(a))
				if err == nil && n > 0 && ((a["name"] == "audio.channels" && n <= 32) || (a["name"] == "audio.bitrate" && n <= 100000) || (a["name"] == "audio.samplingRate" && n <= 192000)) && (a["type"] == "upperBound" || a["type"] == "lowerBound") && (a["name"] == "audio.channels" || a["name"] == "audio.bitrate" || a["name"] == "audio.samplingRate") {
					c.AudioLimits = append(c.AudioLimits, frontend.AudioLimit{Codec: normalizeCodec(a["scopeName"]), Name: a["name"], Type: a["type"], Value: n})
				}
			}
			if limitationDeniesHDR(a) {
				ev.hdrDenied = true
			}
			if limitationAffirmsHDR10(a) {
				ev.affirmativeHDR10 = true
			}
			applyLimitation(c, a)
		}
	}
	return ev
}

// streamingVideoTarget keeps segmented video output separate from direct-play
// decoder support. A smart TV (Plex for LG, VIDAA) Direct Plays 4K HEVC files
// over HTTP while its HLS transcode target lists only H.264 (#408).
func streamingVideoTarget(extra, protocol string) []string {
	var codecs []string
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if typ := a["type"]; typ != "" && typ != "videoProfile" {
			continue
		}
		if context := a["context"]; context != "" && context != "all" && context != "streaming" {
			continue
		}
		if target := a["protocol"]; target != "" && target != protocol {
			continue
		}
		codecs = addUnique(codecs, splitList(a["videoCodec"])...)
	}
	return codecs
}

// streamingAudioTarget keeps output capabilities separate from direct-play
// support. A TV may decode EAC3 files while its DASH player requests AAC.
// Targets for downloads or another streaming protocol cannot widen this one.
func streamingAudioTarget(extra, protocol string) []string {
	var codecs []string
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if typ := a["type"]; typ != "" && typ != "videoProfile" {
			continue
		}
		if context := a["context"]; context != "" && context != "all" && context != "streaming" {
			continue
		}
		if target := a["protocol"]; target != "" && target != protocol {
			continue
		}
		codecs = addUnique(codecs, splitList(a["audioCodec"])...)
	}
	return codecs
}

// h264LevelMaxHeight maps an H.264 level*10 upper bound (e.g. 40, 41, 42) to
// the tallest standard frame height that level's macroblock ceiling supports.
// Levels >= 5.0 support 4K and return 0 (unbounded).
func h264LevelMaxHeight(level int) int {
	switch {
	case level <= 0:
		return 0
	case level <= 30:
		return 480
	case level <= 32:
		return 720
	case level <= 42:
		return 1080
	default:
		return 0
	}
}

func applyLimitation(c *frontend.ClientCaps, a map[string]string) {
	scope := a["scope"]
	if scope != "videoCodec" && scope != "videoAudioCodec" && scope != "transcodeTarget" && scope != "videoContainer" {
		return
	}
	name := a["name"]
	typ := a["type"]
	// PMS sends colorTrc denials as list=, the same shape as its allow-lists.
	val := limitationValue(a)
	n, numErr := strconv.Atoi(val)
	switch {
	case scope == "videoCodec" && name == "video.height" && typ == "upperBound" && numErr == nil && n > 0:
		if c.MaxHeight == 0 || n < c.MaxHeight {
			c.MaxHeight = n
			c.ExplicitMaxHeight = true
		}
	case scope == "videoCodec" && name == "video.width" && typ == "upperBound" && numErr == nil && n > 0:
		h := n * 9 / 16
		switch {
		case n <= 1280:
			h = 720
		case n <= 1920:
			h = 1080
		case n <= 3840:
			h = 2160
		}
		if c.MaxHeight == 0 || h < c.MaxHeight {
			c.MaxHeight = h
			c.ExplicitMaxHeight = true
		}
	case scope == "videoCodec" && name == "video.level" && typ == "upperBound" && numErr == nil && n > 0:
		sn := normalizeCodec(a["scopeName"])
		if sn != "" && sn != "*" {
			if c.VideoLevels == nil {
				c.VideoLevels = map[string]int{}
			}
			if old := c.VideoLevels[sn]; old == 0 || n < old {
				c.VideoLevels[sn] = n
			}
			if sn == "h264" {
				if h := h264LevelMaxHeight(n); h > 0 {
					if c.VideoMaxHeights == nil {
						c.VideoMaxHeights = map[string]int{}
					}
					if old := c.VideoMaxHeights["h264"]; old == 0 || h < old {
						c.VideoMaxHeights["h264"] = h
					}
					if ceiling := decoderCeiling(*c); ceiling > 0 && (c.MaxHeight == 0 || ceiling < c.MaxHeight) {
						c.MaxHeight = ceiling
						c.ExplicitMaxHeight = true
					}
				}
			}
		}
	case scope == "videoCodec" && name == "video.bitrate" && typ == "upperBound" && numErr == nil && n > 0:
		c.DefaultRemoteBitrate = false
		if c.MaxBitrateKbps == 0 || n < c.MaxBitrateKbps {
			c.MaxBitrateKbps = n
		}
	case scope == "videoCodec" && name == "video.bitDepth" && typ == "upperBound" && numErr == nil:
		// 8-bit only for the scoped codec: no HDR on that codec. HDR is
		// effectively HEVC/AV1 only, so an 8-bit cap on either kills HDR.
		sn := normalizeCodec(a["scopeName"])
		if n < 10 && (sn == "hevc" || sn == "av1" || sn == "*") {
			c.HDR10 = false
			c.ExplicitNoHDR = true
			c.DolbyVision = false
		}
	case scope == "videoCodec" && strings.EqualFold(name, "video.DOVIProfile"):
		if strings.EqualFold(typ, "notMatch") {
			if val == "*" || val == "" {
				c.DolbyVision = false
			} else if numErr == nil && n > 0 {
				// A profile-specific refusal must not disable other supported
				// Dolby Vision profiles (recorded PMS measurement, 2026-10-17).
				profiles := frontend.ClientPolicyFor(*c).DolbyVisionProfiles
				c.DVProfiles = slices.DeleteFunc(profiles, func(profile int) bool { return profile == n })
				if len(c.DVProfiles) == 0 {
					c.DolbyVision = false
				}
			}
		}
	case scope == "videoCodec" && name == "video.colorTrc" && typ == "notMatch":
		if strings.Contains(val, "smpte2084") {
			c.HDR10 = false
			c.ExplicitNoHDR = true
		}
	}
}

// applyLegacyCapabilities parses the old X-Plex-Client-Capabilities header,
// e.g. "protocols=http-live-streaming,http-mp4-streaming;videoDecoders=h264{profile:high&resolution:1080&level:51},h265;audioDecoders=mp3,aac{bitrate:160000},ac3".
func applyLegacyCapabilities(c *frontend.ClientCaps, s string) {
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "videoDecoders":
			// A decoder's resolution bounds that codec only (#394). Plex for
			// Roku lists just h264{resolution:1080} while it plays 4K HEVC,
			// which PMS serves it. The client-wide ceiling drops only when
			// every codec the client decodes is bounded.
			for _, dec := range splitDecoders(v) {
				codec := normalizeCodec(dec.name)
				c.VideoCodecs = addUnique(c.VideoCodecs, codec)
				if r, err := strconv.Atoi(dec.params["resolution"]); err == nil && r > 0 {
					if c.VideoMaxHeights == nil {
						c.VideoMaxHeights = map[string]int{}
					}
					if old := c.VideoMaxHeights[codec]; old == 0 || r < old {
						c.VideoMaxHeights[codec] = r
					}
				}
			}
			if ceiling := decoderCeiling(*c); ceiling > 0 && ceiling < c.MaxHeight {
				c.MaxHeight = ceiling
				c.ExplicitMaxHeight = true
			}
		case "audioDecoders":
			for _, dec := range splitDecoders(v) {
				c.AudioCodecs = addUnique(c.AudioCodecs, normalizeCodec(dec.name))
			}
		}
	}
}

// decoderCeiling is the tallest per-decoder bound across the client's video
// codecs, or 0 when any of them is unbounded.
func decoderCeiling(c frontend.ClientCaps) int {
	ceiling := 0
	for _, codec := range c.VideoCodecs {
		h := c.VideoMaxHeights[codec]
		if h <= 0 {
			return 0
		}
		ceiling = max(ceiling, h)
	}
	return ceiling
}

type decoder struct {
	name   string
	params map[string]string
}

func splitDecoders(s string) []decoder {
	var out []decoder
	depth := 0
	start := 0
	flush := func(end int) {
		item := strings.TrimSpace(s[start:end])
		if item == "" {
			return
		}
		d := decoder{params: map[string]string{}}
		if i := strings.IndexByte(item, '{'); i >= 0 {
			d.name = item[:i]
			inner := strings.TrimSuffix(item[i+1:], "}")
			for _, kv := range strings.Split(inner, "&") {
				if k, v, ok := strings.Cut(kv, ":"); ok {
					d.params[k] = v
				}
			}
		} else {
			d.name = item
		}
		out = append(out, d)
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return out
}

// applyDecisionQuery refines caps with per-request transcode parameters
// (maxVideoBitrate, videoResolution, location).
func applyDecisionQuery(c *frontend.ClientCaps, q url.Values) {
	for _, name := range []string{"maxVideoBitrate", "videoBitrate"} {
		if n, err := strconv.Atoi(q.Get(name)); err == nil && n > 0 {
			c.DefaultRemoteBitrate = false
			if c.MaxBitrateKbps == 0 || n < c.MaxBitrateKbps {
				c.MaxBitrateKbps = n
			}
		}
	}
	if h := parseResolutionHeight(q.Get("videoResolution")); h > 0 {
		c.ExplicitMaxHeight = true
		if c.MaxHeight == 0 || h < c.MaxHeight {
			c.MaxHeight = h
		}
	}
	switch q.Get("location") {
	case "wan", "cellular":
		c.Remote = true
	case "lan":
		// Trust the network check over the client's claim for LAN; a client
		// saying "lan" while coming from a public address is still remote.
	}
}

// parseResolutionHeight parses "1920x1080" / "1280:720" / "1080" into a
// height.
func parseResolutionHeight(s string) int {
	if s == "" {
		return 0
	}
	if i := strings.IndexAny(s, "x:X"); i >= 0 {
		w, err1 := strconv.Atoi(s[:i])
		h, err2 := strconv.Atoi(s[i+1:])
		if err1 != nil || err2 != nil {
			return 0
		}
		// Portrait requests (e.g. 1080x1920) still mean 1080p.
		return min(w, h)
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(strings.ToLower(s), "p"))
	return n
}
