package plex

import (
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	stallTextDV    = "This version couldn't start in Dolby Vision on this device. Play it again to watch it in HDR10, or choose another version, such as 1080p."
	stallTextOther = "This version can't play on this device. Choose another version, such as 1080p."
	stallTextAudio = "This device couldn't play this version's surround audio. Play it again to hear it in stereo."
)

// guardedStart reports whether stalled-start protection covers a session:
// Dolby Vision sources on any client, Plex for iOS on any source, and Plex
// for Android when Dolby/DTS audio was copied into its stream. Other clients
// and sources keep their behaviour unchanged.
func guardedStart(pr *Request, v *frontend.StreamInfo, d frontend.Decision) bool {
	if v != nil && v.DolbyVision {
		return true
	}
	if surroundCopied(pr, d) {
		return true
	}
	return pr != nil && frontend.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, "plex-ios-hdr10-base")
}

// surroundCopied reports a Plex for Android stream carrying copied Dolby or
// DTS audio: the case an Android box without that decoder freezes on.
func surroundCopied(pr *Request, d frontend.Decision) bool {
	return plexAndroidClient(pr) && d.Kind != frontend.DecisionDirectPlay && d.AudioCopy && frontend.IsSurroundAudioCodec(d.AudioCodec)
}

// checkStartStall feeds one timeline to the guard. It returns the
// termination text when this client's start stalled, after remembering a
// Dolby Vision HDR10-base fallback for the next start.
func (s *Server) checkStartStall(pr *Request, key frontend.RatingKey, state string, offset time.Duration, tids []string) string {
	if s.transcoder() == nil || pr == nil || pr.User == nil {
		return ""
	}
	now := s.now()
	for _, id := range tids {
		e, ok := s.transcodes.get(id)
		if !ok || !e.started {
			continue
		}
		out, ok := s.plexHLSSession(id)
		if !ok {
			s.starts.Forget(id)
			continue
		}
		live, ok := s.runtime.ObserveStart(s.ctx, out, id)
		if !ok {
			continue
		}
		info := live.Info()
		v := info.Video()
		d := live.Decision()
		if !guardedStart(pr, v, d) {
			continue
		}
		base := max(e.offset, offset)
		if !s.starts.Observe(id, state, offset, live.RequestedPosition()-base, now) {
			continue
		}
		s.log.Warn("playback start stalled: segments delivered but the player never advanced",
			"key", key, "client", pr.ClientID, "product", pr.Product, "platform", pr.Platform, "session", id,
			"position", offset, "requested", live.RequestedPosition(), "videoCopy", d.VideoCopy, "stripDolbyVision", d.StripDolbyVision, "audioCopy", d.AudioCopy, "audio", d.AudioCodec)
		if v != nil && d.VideoCopy && v.DolbyVision && !d.StripDolbyVision && frontend.HasHDR10Base(v, e.caps) {
			s.starts.Remember(frontend.StartHintKey{UserID: pr.User.ID, ClientID: pr.ClientID, Key: key}, now)
			return stallTextDV
		}
		if surroundCopied(pr, d) {
			s.starts.Remember(surroundHintKey(pr), now)
			return stallTextAudio
		}
		return stallTextOther
	}
	return ""
}

// dvBaseOnly reports a remembered Dolby Vision stall for this start.
func (s *Server) dvBaseOnly(pr *Request, key frontend.RatingKey) bool {
	return pr != nil && pr.User != nil && s.starts.BaseOnly(frontend.StartHintKey{UserID: pr.User.ID, ClientID: pr.ClientID, Key: key}, s.now())
}

// surroundHintKey is the client-wide hint (zero Key) that this viewer's
// client stalled on copied surround audio.
func surroundHintKey(pr *Request) frontend.StartHintKey {
	return frontend.StartHintKey{UserID: pr.User.ID, ClientID: pr.ClientID}
}

// segmentedSurroundOff reports a remembered surround-audio stall for this
// viewer's client: its streams convert Dolby/DTS audio until the hint expires.
func (s *Server) segmentedSurroundOff(pr *Request) bool {
	return pr != nil && pr.User != nil && pr.ClientID != "" && s.starts.BaseOnly(surroundHintKey(pr), s.now())
}
