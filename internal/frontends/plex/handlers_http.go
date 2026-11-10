package plex

import (
	"context"
	"net/http"
	"slices"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// handleStartHTTP is /video/:/transcode/universal/start (no extension):
// Plex for Mac / Desktop's protocol=http Matroska stream.
func (s *Server) handleStartHTTP(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if s.transcoder() == nil || s.playback() == nil {
		writePlaybackStatus(w, http.StatusServiceUnavailable)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	if !s.decisionArmed(pr, p.key) {
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	p.protocol = frontend.ProtocolHTTP
	p.opts.Protocol = frontend.ProtocolHTTP
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writePlaybackStatus(w, http.StatusGone)
		return
	}

	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writePlaybackStatus(w, http.StatusForbidden)
		return
	}
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed {
		s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
		s.writeStartErr(w, r, pr, err)
		return
	}
	entry, ok := s.transcodes.get(p.sessionID)
	var ps *frontend.PlaySession
	if ok && entry.key == p.key && entry.userID == pr.User.ID {
		ps, ok = s.playback().Session(entry.playID)
	}
	if !ok || ps == nil || !p.sameSettings(entry.requested) {
		if _, ps, _, err = s.prepare(ctx, pr, &p); err != nil {
			s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
			s.writeStartErr(w, r, pr, err)
			return
		}
		entry, _ = s.transcodes.get(p.sessionID)
	} else if dec, err := s.publishBrokerPlayback(ctx, pr, replayPlay(p, entry), ps); err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	} else {
		entry.decision = dec
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	entry.playbackID = pr.SessionID
	s.transcodes.put(entry)

	dec := frontend.HLSDecision(entry.decision, p.opts.DirectPlay, func() frontend.Decision {
		resolvePlaySelection(&p, ps.Info)
		applySelections(&p, ps.Info, s.audioLanguages(pr.User))
		return s.transcoder().Decide(ps.Info, p.caps, p.opts)
	}, func(dec frontend.Decision) frontend.Decision {
		if len(entry.caps.TranscodeAudioCodecs) > 0 && dec.AudioCodec != "" && !slices.Contains(entry.caps.TranscodeAudioCodecs, dec.AudioCodec) {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = true
			opts.Protocol = frontend.ProtocolHTTP
			return s.transcoder().Decide(ps.Info, entry.caps, opts)
		}
		return dec
	})
	if existing, ok := s.plexHLSSession(p.sessionID); ok && existing != nil {
		if entry.started && entry.offset == p.offset {
			if hs, ok := existing.(frontend.HTTPSession); ok {
				if failed, ok := hs.(frontend.ProgressiveFailureSession); ok && failed.ProgressiveFailed() && r.Header.Get("Range") == "" {
					http.Error(w, "progressive transcode failed; start a new session", http.StatusBadGateway)
					return
				}
				s.notePlexEncoder(p.sessionID, frontend.StreamHTTP, dec, p.offset)
				unlockEncoder()
				unlockOwner()
				hs.ServeHTTP(w, r)
				return
			}
		}
		s.stopPlexHLS(p.sessionID)
	}
	hs, err := s.startPlexHLS(context.WithoutCancel(ctx), pr.User, ps.ID, p.sessionID, frontend.HLSRequest{
		SessionID:      p.sessionID,
		ReleaseID:      ps.SourceReleaseID(),
		InputURL:       s.playback().InputURL(ps.ID),
		Info:           ps.Info,
		Decision:       dec,
		StartOffset:    p.offset,
		Protocol:       frontend.ProtocolHTTP,
		CopyTimestamps: p.copyTimestamps,
	})
	if err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	}
	stream, ok := hs.(frontend.HTTPSession)
	if !ok {
		s.log.Error("transcoder returned a non-HTTP session", "session", p.sessionID)
		s.stopPlexHLS(p.sessionID)
		writePlaybackStatus(w, http.StatusInternalServerError)
		return
	}
	s.bindTranscodeStarted(entry, dec, p.offset)
	s.progressiveStops.clear(pr.User.ID, pr.ClientID, pr.SessionID, p.key)
	s.notePlexEncoder(p.sessionID, frontend.StreamHTTP, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	stream.ServeHTTP(w, r)
}
