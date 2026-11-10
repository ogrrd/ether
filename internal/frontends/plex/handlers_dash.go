package plex

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// MPEG-DASH playback for Plex Web, which asks for DASH (protocol=dash,
// start.mpd) in Chromium and Firefox when it cannot direct play. The URL
// layout is the one real PMS uses:
//
//	/video/:/transcode/universal/start.mpd?session={id}&...    static MPD
//	/video/:/transcode/universal/session/{id}/{rep}/header     init segment
//	/video/:/transcode/universal/session/{id}/{rep}/{n}.m4s    media segment n
//
// Representation 0 is video and 1 audio; segments are numbered from 0
// over the whole title, so seeking is just asking for another number.

// handleStartDASH is /video/:/transcode/universal/start.mpd.
func (s *Server) handleStartDASH(w http.ResponseWriter, r *http.Request) {
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
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()
	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writePlaybackStatus(w, http.StatusGone)
		return
	}
	p.protocol = frontend.ProtocolDASH
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writePlaybackStatus(w, http.StatusForbidden)
		return
	}
	// A slow decision leaves acquisition running. Join that shared flight
	// below; only a completed source failure should short-circuit start.
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed && !errors.Is(err, frontend.ErrPrepareSlow) {
		s.log.Info("start DASH playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
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
			if ctx.Err() != nil {
				return // the player dropped this request; its preparation carries on
			}
			s.log.Info("start DASH playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
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

	dec := entry.decision
	if dec.Kind == frontend.DecisionDirectPlay {
		// The client asked for DASH anyway: remux.
		dec.Kind, dec.VideoCopy, dec.AudioCopy = frontend.DecisionRemux, true, true
		if len(entry.caps.TranscodeAudioCodecs) > 0 && dec.AudioCodec != "" && !slices.Contains(entry.caps.TranscodeAudioCodecs, dec.AudioCodec) {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = true
			dec = s.transcoder().Decide(ps.Info, entry.caps, opts)
		}
	}
	if existing, ok := s.plexHLSSession(p.sessionID); ok && existing != nil {
		// The MPD spans the whole title whatever the offset, so a repeated
		// start for the same decision keeps the session (and its finished
		// segments); a new decision request starts afresh.
		if ds, isDASH := existing.(frontend.DASHSession); isDASH && entry.started {
			s.notePlexEncoder(p.sessionID, frontend.StreamDASH, dec, p.offset)
			unlockEncoder()
			unlockOwner()
			s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
			return
		}
		s.stopPlexHLS(p.sessionID)
	}
	hs, err := s.startPlexHLS(context.WithoutCancel(ctx), pr.User, ps.ID, p.sessionID, frontend.HLSRequest{
		SoftSubtitleStream: sessionSoftSubtitle(entry, pr, dec),
		SessionID:          p.sessionID,
		ReleaseID:          ps.SourceReleaseID(),
		InputURL:           s.playback().InputURL(ps.ID),
		Info:               ps.Info,
		Decision:           dec,
		StartOffset:        p.offset,
		Protocol:           frontend.ProtocolDASH,
	})
	if err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	}
	ds, ok := hs.(frontend.DASHSession)
	if !ok {
		s.log.Error("transcoder returned a non-DASH session", "session", p.sessionID)
		s.stopPlexHLS(p.sessionID)
		writePlaybackStatus(w, http.StatusInternalServerError)
		return
	}
	s.bindTranscodeStarted(entry, dec, p.offset)
	s.notePlexEncoder(p.sessionID, frontend.StreamDASH, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
}

// dashSession resolves the session and representation of a segment URL.
func (s *Server) dashSession(w http.ResponseWriter, r *http.Request) (frontend.DASHSession, int, bool) {
	if s.transcoder() == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	id, idOK := routeSessionID(r)
	if idOK && s.forwardPlexMedia(w, r, id) {
		return nil, 0, false
	}
	rep, err := strconv.Atoi(chi.URLParam(r, "stream"))
	hs, ok := s.plexHLSSession(id)
	if !idOK || err != nil || rep < 0 || !ok || hs == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	ds, ok := hs.(frontend.DASHSession)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	s.transcodes.touch(id, s.now())
	return ds, rep, true
}

// handleDASHInit is session/{session}/{stream}/header.
func (s *Server) handleDASHInit(w http.ResponseWriter, r *http.Request) {
	if ds, rep, ok := s.dashSession(w, r); ok {
		ds.ServeInit(w, r, rep)
	}
}

// handleDASHSegment is session/{session}/{stream}/{segment}.m4s.
func (s *Server) handleDASHSegment(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(chi.URLParam(r, "segment"), ".m4s")
	n, err := strconv.Atoi(name)
	if !ok || err != nil || n < 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if ds, rep, ok := s.dashSession(w, r); ok {
		cw := s.newCountWriter(w, r)
		ds.ServeMedia(cw, r, rep, n)
		cw.finish()
	}
}

// dashDecisionMedia adjusts a transcode decision's Media for DASH the way
// PMS reports it: an mp4 container over protocol dash, with the video and
// the selected audio in separate segment streams. Unselected audio keeps
// the shape applyStreamDecision already returned: the source index, and no
// decision or location. Marking every audio stream segments-audio makes
// Plex Web request a representation that dashReps never builds.
func dashDecisionMedia(m *model.Media) {
	m.Container = "mp4"
	m.Protocol = frontend.ProtocolDASH
	for i := range m.Part {
		part := &m.Part[i]
		part.Container = "mp4"
		part.Protocol = frontend.ProtocolDASH
		for j := range part.Stream {
			st := &part.Stream[j]
			switch st.StreamType {
			case 1:
				st.Location = "segments-video"
			case 2:
				if st.Selected != nil && *st.Selected {
					st.Location = "segments-audio"
				}
			}
		}
	}
}
