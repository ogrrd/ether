package plex

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// stateKey reads the item key from ratingKey= or key=.
func stateKey(r *http.Request) frontend.RatingKey {
	q := r.URL.Query()
	if k, err := frontend.ParseRatingKey(q.Get("ratingKey")); err == nil {
		return k
	}
	return keyFromPath(q.Get("key"))
}

func msParam(v string) time.Duration {
	return durationParam(v, time.Millisecond)
}

// durationParam parses a non-negative count of unit. NaN, infinities and
// values past the time.Duration range read as 0: converting them would
// yield an implementation-defined (typically negative) duration.
func durationParam(v string, unit time.Duration) time.Duration {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || !(n >= 0) || n*float64(unit) >= float64(math.MaxInt64) {
		return 0
	}
	return time.Duration(n * float64(unit))
}

// handleTimeline is /:/timeline: playback progress from the client.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	if s.handleLiveTimeline(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	origin := frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID}
	if s.d.Dashboard != nil {
		origin.DeviceID = s.d.Dashboard.SeeDevice(pr.ClientID, pr.DeviceName, pr.Platform).ID
	}
	ctx = frontend.WithWatchOrigin(ctx, origin)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	q := r.URL.Query()
	key := stateKey(r)
	if pr.User == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if key == 0 {
		s.write(w, r, &model.MediaContainer{Extra: []model.Attr{model.A("playbackState", "ignore"), model.A("viewOffset", -1)}})
		return
	}
	state := q.Get("state")
	switch state {
	case "playing", "paused", "stopped", "buffering":
	default:
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	// Reject malformed positions before they can affect playback or watch state.
	// Keep the existing handling of late positions from source switches below.
	for _, name := range []string{"time", "duration"} {
		if q.Has(name) {
			n, err := strconv.ParseInt(q.Get(name), 10, 64)
			if err != nil || n < 0 || n > math.MaxInt64/int64(time.Millisecond) {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
	}
	key = s.queues.TimelineQueue(pr.User.ID, pr.ClientID, key, q.Get("playQueueItemID"), q.Has("playQueueItemID"))
	offsetCheck, durationCheck := msParam(q.Get("time")), msParam(q.Get("duration"))
	if durationCheck > 0 && offsetCheck > durationCheck {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if key.Kind() == frontend.KindPlaylist {
		if _, err := s.playlist(ctx, pr.User, key); err != nil {
			s.writeErr(w, r, err)
			return
		}
	} else {
		title, err := s.d.Catalog.Get(ctx, key)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		ctx = frontend.WithHistoryTitle(ctx, title)
		if title == nil || !s.titleVisible(pr.User, title) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
	}
	s.rememberStreamRequest(r, pr.User, key)
	// A late stop from an explicitly superseded playback must not stop the
	// replacement source or overwrite its progress. Unknown/omitted IDs keep
	// the historical behavior for clients with different request identities.
	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, key, pr.SessionID) {
		s.cleanSupersededEncoders(ctx, pr.User.ID, pr.ClientID, key, pr.SessionID)
		s.sessions.remove(trackID(pr.User.ID, pr.ClientID, pr.SessionID))
		s.writeEmpty(w, r)
		return
	}
	offset := msParam(q.Get("time"))
	duration := msParam(q.Get("duration"))
	user := pr.User
	s.log.Debug("playback timeline", "user", user.ID, "client", pr.ClientID, "key", key, "state", state, "position", offset, "duration", duration)
	var ps *frontend.PlaySession
	if s.d.Playback != nil {
		if p, ok := s.d.Playback.SessionFor(user.ID, pr.ClientID, key); ok {
			ps = p
		}
	}
	tids := s.transcodes.forClient(user.ID, pr.ClientID, key)
	tid := trackID(user.ID, pr.ClientID, pr.SessionID)
	// A broken progressive response must not turn a client's terminal EOF
	// estimate into a completion. Keep the last client-reported playhead;
	// encoder output can be buffered ahead of what the viewer actually saw.
	failedProgressive := false
	for _, id := range tids {
		if s.d.Transcoder != nil {
			if session, ok := s.plexHLSSession(id); ok {
				if failed, ok := session.(interface{ ProgressiveFailed() bool }); ok && failed.ProgressiveFailed() {
					failedProgressive = true
				}
			}
		}
	}
	if failedProgressive {
		found := false
		for _, activity := range s.sessions.list() {
			if activity.id == tid && activity.key == key {
				offset, duration, found = activity.offset, activity.duration, true
				break
			}
		}
		if !found && s.d.State != nil {
			if previous, err := s.d.State.Get(ctx, user.ID, key); err == nil {
				offset, duration = previous.ViewOffset, previous.Duration
			}
		}
	}
	if saved, ok := s.progressiveStops.get(user.ID, pr.ClientID, pr.SessionID, key, s.now()); ok {
		offset, duration = saved.offset, saved.duration
	}
	// Some clients carry the previous queue item's playhead into the first
	// timeline for the next item. A position past the item's end is not
	// evidence of watching it: keep the last real position for this item and
	// handle the report (session, stop, encoders) as usual.
	if impossiblePlayhead(offset, duration) {
		s.log.Debug("ignoring impossible playhead", "key", key, "position", offset, "duration", duration)
		offset = s.lastKnownOffset(ctx, user.ID, tid, key)
	}
	// A failed start cannot write a resume point or consume the completion latch.
	// Source bytes include ffmpeg read-ahead; conversions require output evidence.
	stalled := s.checkStartStall(pr, key, state, offset, tids)
	if failedProgressive || stalled != "" || s.playbackNeverDelivered(ps, tids) {
		ctx = context.WithValue(ctx, noPlaybackDelivery{}, true)
		offset = s.lastKnownOffset(ctx, user.ID, tid, key)
	}
	if ps != nil {
		// Client duration can be a stale queue estimate. Only a probed duration is
		// authoritative for an active source; estimates cannot establish completion.
		if !ps.Info.Estimated && ps.Info.Duration > 0 {
			duration = ps.Info.Duration
		} else {
			duration = 0
		}
		if state == "stopped" {
			// EOF after a decoder crash is not a played position. The last nonterminal
			// report is the evidence of where this viewer actually got to.
			offset = min(offset, s.lastKnownOffset(ctx, user.ID, tid, key))
		}
	}
	ctx, watchResult, watchErr := s.timelineWatch(ctx, pr, key, offset, duration, state, r)
	if watchErr != nil {
		s.writeErr(w, r, watchErr)
		return
	}
	if !s.reportBrokerTimeline(ctx, pr, key, offset, duration, state, sanitizeSessionID(q.Get("session"))) {
		s.log.Warn("timeline report", "key", key, "state", state)
	}
	if reason := stalled; reason != "" {
		// The player took the media and never started. End this attempt
		// with the reason; the stop below closes its encoder and source.
		s.setTermination(tid, reason)
		if failed, ok := s.d.Playback.(interface{ NoteFailedStart(string, string) }); ok && ps != nil {
			failed.NoteFailedStart(ps.ID, reason)
		}
		s.reportBrokerTimeline(ctx, pr, key, offset, duration, "stopped", sanitizeSessionID(q.Get("session")))
	}
	for _, id := range tids {
		if e, ok := s.transcodes.get(id); ok && s.playbackOwners.Stale(user.ID, pr.ClientID, key, e.playbackID) {
			s.cleanSupersededEncoders(ctx, user.ID, pr.ClientID, key, e.playbackID)
			continue
		}
		s.transcodes.touch(id, s.now())
		frontend.NotifyTranscodeActivity(s.d.Transcoder, id, state)
	}
	pqItem, _ := strconv.ParseInt(q.Get("playQueueItemID"), 10, 64)

	mc := &model.MediaContainer{}
	mc.Bandwidths = s.timelineBandwidths(pr, key, tids, ps)
	mc.TranscodeSess = s.timelineTranscodeSessions(pr, key, tids)
	mc.Size = len(mc.Bandwidths) + len(mc.TranscodeSess)
	if reason, killed := s.takeTermination(tid); killed {
		mc.Extra = []model.Attr{model.A("terminationCode", 2006), model.A("terminationText", reason)}
		state = "stopped"
	}
	if state == "stopped" {
		if q.Get("state") == "stopped" {
			s.disarmDecision(pr, key)
		}
		s.endVersionFlow(ctx, s.playIdentityFor(pr, key))
		if e, ok := s.sessions.remove(tid); ok {
			e.offset = offset
			s.notifyPlaying(e, "stopped")
			s.cl.poke()
		} else if s.cl != nil && ps == nil && len(tids) == 0 {
			// The client switched nodes mid-play: the node serving it ends it.
			s.cl.forwardStop(r.Context(), tid, offset)
		}
		if ps != nil {
			s.cancelTrickplayIfIdle(ps)
		}
		for _, id := range tids {
			unlock, err := s.lockEncoder(ctx, id)
			if err != nil {
				return
			}
			if e, ok := s.transcodes.get(id); ok && e.userID == user.ID && e.clientID == pr.ClientID && e.key == key {
				if s.d.Transcoder != nil {
					s.stopPlexHLS(id)
				}
				s.dropTranscode(id)
			}
			unlock()
		}
	} else {
		if s.versionFlows != nil {
			if _, err := s.versionFlows.call(ctx, s.playIdentityFor(pr, key), nil, versionTouch, s.now(), 0); err != nil {
				s.log.Warn("refresh version flow", "err", err)
			}
		}
		client := *pr
		client.User = nil
		fresh := !s.sessions.has(tid)
		if fresh && s.cl != nil {
			s.sessions.ensure(tid, s.cl.allocSessionKey())
		}
		armed := s.decisionArmed(pr, 0)
		var changed bool
		e := s.sessions.update(tid, func(e *playEntry) {
			changed = fresh || e.key != key || e.state != state
			if e.key != key {
				e.startedAt = s.now()
			}
			e.user = *user
			e.client = client
			e.decisionSeen = e.decisionSeen || armed
			e.playingSeen = e.playingSeen || state == "playing"
			e.key = key
			e.state = state
			e.offset = offset
			e.duration = duration
			e.playQueueItemID = pqItem
			e.transcodeID = ""
			if len(tids) > 0 {
				e.transcodeID = tids[0]
			}
			e.updatedAt = s.now()
		})
		s.notifyPlaying(e, state)
		if changed {
			s.cl.poke() // share starts and state changes now; positions ride the heartbeat
		}
	}
	timelineWatchAttrs(mc, watchResult)
	s.observePlaybackReported(pr)
	s.write(w, r, mc)
}

// impossiblePlayhead reports a position more than two seconds past the
// reported duration, which no real playback of the item can produce.
func impossiblePlayhead(offset, duration time.Duration) bool {
	return duration > 0 && offset-duration > 2*time.Second
}

// lastKnownOffset is the item's last real position: this session's report,
// else the viewer's saved state, else the start.
func (s *Server) lastKnownOffset(ctx context.Context, userID int64, tid string, key frontend.RatingKey) time.Duration {
	for _, activity := range s.sessions.list() {
		if activity.id == tid && activity.key == key {
			return activity.offset
		}
	}
	if s.d.State != nil {
		if previous, err := s.d.State.Get(ctx, userID, key); err == nil && previous.ViewOffset <= previous.Duration {
			return previous.ViewOffset
		}
	}
	return 0
}

// handleProgress is the legacy /:/progress?key=&time=&state=.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	ctx = frontend.WithWatchOrigin(ctx, frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID})
	q := r.URL.Query()
	if q.Get("key") == "" || q.Get("identifier") == "" || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, 400)
		return
	}
	raw, err := strconv.ParseInt(q.Get("key"), 10, 64)
	if err != nil {
		writeHTTPError(w, 500)
		return
	}
	if q.Get("identifier") != "com.plexapp.plugins.library" {
		writeHTTPError(w, 404)
		return
	}
	key := frontend.RatingKey(raw)
	n, err := strconv.ParseInt(q.Get("time"), 10, 64)
	if err != nil || n > math.MaxInt64/int64(time.Millisecond) {
		writeHTTPError(w, 500)
		return
	}
	if n < 0 {
		writeStateMutationOK(w)
		return
	}
	// Unknown integer keys are accepted by this legacy writer but create no row.
	known := false
	switch key.Kind() {
	case frontend.KindPlaylist:
		if s.d.Playlists != nil {
			_, err = s.d.Playlists.Get(ctx, pr.User.ID, key)
			known = err == nil
		}
	case frontend.KindCollection, frontend.KindSmart:
		_, err = s.collectionRaw(ctx, pr.User, key)
		known = err == nil
	default:
		if s.d.Catalog != nil {
			var title *frontend.Title
			var e error
			if stored, ok := s.d.Catalog.(frontend.StoredTitleLookup); ok {
				title, e = stored.StoredTitle(ctx, key)
			} else {
				title, e = s.d.Catalog.Get(ctx, key)
			}
			known = e == nil && title != nil && s.titleVisible(pr.User, title)
		}
	}
	if !known {
		writeStateMutationOK(w)
		return
	}
	offset := time.Duration(n) * time.Millisecond
	if legacy, ok := s.d.State.(interface {
		LegacyProgress(context.Context, int64, frontend.RatingKey, time.Duration) error
	}); ok {
		err = legacy.LegacyProgress(ctx, pr.User.ID, key, offset)
	} else {
		err = s.d.State.Progress(ctx, pr.User.ID, key, offset, 0, q.Get("state"))
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.observePlaybackReported(pr)
	writeStateMutationOK(w)
}

func (s *Server) handleScrobble(w http.ResponseWriter, r *http.Request)   { s.scrobble(w, r, true) }
func (s *Server) handleUnscrobble(w http.ResponseWriter, r *http.Request) { s.scrobble(w, r, false) }

func (s *Server) scrobble(w http.ResponseWriter, r *http.Request, watched bool) {
	ctx := r.Context()
	pr := FromContext(ctx)
	ctx = frontend.WithWatchOrigin(ctx, frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID})
	key, ok := s.mutationKey(w, r, false)
	if !ok {
		return
	}
	if title, err := s.d.Catalog.Get(ctx, key); err == nil && title != nil {
		ctx = frontend.WithHistoryTitle(ctx, title)
	}
	// Containers mark every episode.
	keys := []frontend.RatingKey{key}
	if key.Kind() == frontend.KindShow || key.Kind() == frontend.KindSeason {
		if ks, err := s.expandKey(ctx, pr, key); err == nil && len(ks) > 0 {
			keys = ks
		}
	}
	if native, ok := s.d.State.(interface {
		ScrobbleTree(context.Context, int64, frontend.RatingKey, []frontend.RatingKey, bool) error
	}); ok {
		if err := native.ScrobbleTree(ctx, pr.User.ID, key, keys, watched); err != nil {
			s.writeErr(w, r, err)
			return
		}
	} else {
		for _, k := range keys {
			var err error
			if watched {
				err = s.d.State.Scrobble(ctx, pr.User.ID, k)
			} else {
				err = s.d.State.Unscrobble(ctx, pr.User.ID, k)
			}
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
		}
	}
	s.log.Info("watch state changed", "user", pr.User.ID, "client", pr.ClientID,
		"key", key, "watched", watched, "items", len(keys))
	// An empty body has no Content-Type. tvOS treats that PUT as a failure
	// and leaves the button on the label it had when the detail opened.
	writeStateMutationOK(w)
}

// handleRate is /:/rate?key=&rating= (0-10, -1 clears).
func (s *Server) handleRate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	ctx = frontend.WithWatchOrigin(ctx, frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID})
	rating, err := strconv.ParseFloat(r.URL.Query().Get("rating"), 64)
	if err != nil || math.IsNaN(rating) || math.IsInf(rating, 0) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	key, ok := s.mutationKey(w, r, true)
	if !ok {
		return
	}
	if rating > 0 {
		promoter := s.d.VibeSearch
		if promoter == nil {
			promoter, _ = s.d.Collections.(frontend.VibeSearcher)
		}
		if promoter != nil {
			if _, promoted, err := promoter.PromoteEphemeral(ctx, pr.User, key); err != nil {
				s.writeErr(w, r, err)
				return
			} else if promoted {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	if native, ok := s.d.State.(interface {
		RateExact(context.Context, int64, frontend.RatingKey, float64) error
	}); ok {
		err = native.RateExact(ctx, pr.User.ID, key, rating)
	} else {
		err = s.d.State.Rate(ctx, pr.User.ID, key, math.Max(0, math.Min(10, rating)))
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeStateMutationOK(w)
}

// handleRemoveFromContinueWatching hides the home row while preserving resume state.
func (s *Server) handleRemoveFromContinueWatching(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	ctx = frontend.WithWatchOrigin(ctx, frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID})
	key, _ := frontend.ParseRatingKey(r.URL.Query().Get("ratingKey"))
	if key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if title, err := s.metadataTitle(ctx, pr, key); err != nil || !s.visibleTitle(ctx, pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if hidden, ok := s.d.State.(interface {
		HideContinue(context.Context, int64, frontend.RatingKey) error
	}); ok {
		if err := hidden.HideContinue(ctx, pr.User.ID, key); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	if err := s.discoveryRemoveContinue(ctx, pr, key); err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Sessions

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	ctx = frontend.WithWatchOrigin(ctx, frontend.WatchOrigin{Surface: "plex", SessionID: pr.SessionID, ClientID: pr.ClientID})
	entries := s.sessionEntries(ctx)
	sort.Slice(entries, func(i, j int) bool { return entries[i].sessionKey < entries[j].sessionKey })
	mc := &model.MediaContainer{}
	for _, e := range entries {
		if !e.playingSeen {
			continue
		}
		if !pr.User.IsOwner && e.user.ID != pr.User.ID {
			continue
		}
		viewer := &Request{
			User:     &e.user,
			ClientID: e.client.ClientID,
			Product:  e.client.Product,
			Platform: e.client.Platform,
			Device:   e.client.Device,
			Model:    e.client.Model,
			Version:  e.client.Version,
		}
		var md model.Metadata
		var sessionTitle *frontend.Title
		if e.key.Kind() == frontend.KindPlaylist {
			pl, err := s.playlist(ctx, &e.user, e.key)
			if err != nil {
				continue
			}
			md = s.playlistMetadata(withRequest(ctx, viewer), pl, false)
		} else {
			t, err := s.d.Catalog.Get(ctx, e.key)
			if err != nil {
				continue
			}
			sessionTitle = t
			md = s.metadata(ctx, viewer, t, renderOpts{media: true, session: true})
		}
		if e.node == "" && e.transcodeID != "" && sessionTitle != nil {
			if te, ok := s.transcodes.get(e.transcodeID); ok && te.started {
				if m, ok := s.sessionTranscodeMedia(sessionTitle, te, viewer); ok {
					md.Media = []model.Media{m}
				}
			}
		}
		sessionItem(&md)

		md.SessionKey = strconv.Itoa(e.sessionKey)
		md.ViewOffset = ms(e.offset)
		md.User = &model.SessionUser{ID: strconv.FormatInt(e.user.PlexID, 10), Thumb: e.user.Thumb, Title: e.user.Title}
		if md.User.Title == "" {
			md.User.Title = e.user.Username
		}
		state := e.state
		if state == "" {
			state = "playing"
		}
		title := e.client.DeviceName
		if title == "" {
			title = e.client.Device
		}
		md.Player = &model.Player{
			Address:           e.client.RemoteIP.String(),
			Device:            e.client.Device,
			MachineIdentifier: e.client.ClientID,
			Model:             e.client.Model,
			Platform:          e.client.Platform,
			PlatformVersion:   e.client.PlatformVersion,
			Product:           e.client.Product,
			Profile:           resolvedPlayerProfile(&e.client),
			State:             state,
			Title:             title,
			Version:           e.client.Version,
			Local:             !e.client.Remote,
			UserID:            e.user.PlexID,
		}
		loc := "lan"
		if e.client.Remote {
			loc = "wan"
		}
		md.Session = &model.Session{ID: e.client.SessionID, Location: loc}
		if md.Session.ID == "" {
			md.Session.ID = e.client.ClientID
		}
		if e.node != "" {
			md.TranscodeSession = e.remoteTS
		} else if e.transcodeID != "" {
			if te, ok := s.transcodes.get(e.transcodeID); ok {
				ts := s.servedTranscodeSessionModel(te)
				md.TranscodeSession = &ts
			}
		}
		md.Session.Bandwidth = s.sessionBandwidth(e, md)
		sessionClient := e.client
		sessionClient.User = &e.user
		if !e.decisionSeen && !s.decisionArmed(&sessionClient, 0) {
			md.Session = nil
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleTerminate is the owner's "stop this stream" (sessionId, reason):
// delivered as terminationCode/Text on the client's next timeline.
func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("sessionId")
	if sid == "" {
		// An empty id would match every session that has no session
		// identifier and stop an arbitrary one.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	reason := q.Get("reason")
	if reason == "" {
		reason = "The server owner has ended this stream."
	}
	if s.terminateLocal(sid, reason) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Another node may serve it: that node applies the kill.
	if s.cl != nil {
		for _, e := range s.cl.remoteEntries(r.Context()) {
			if matchesSession(e, sid) {
				s.cl.publish(topicTerminate, terminateEvent{Scope: "plex", ID: sid, Reason: reason, Node: e.node})
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	writeHTTPError(w, http.StatusNotFound)
}

func matchesSession(e playEntry, sid string) bool {
	id := e.client.SessionID
	if id == "" {
		id = e.client.ClientID
	}
	return sid != "" && id == sid
}

// terminateLocal ends a session this node tracks: the stop is delivered as
// terminationCode/Text on the client's next timeline and the source closes.
func (s *Server) terminateLocal(sid, reason string) bool {
	for _, e := range s.sessions.list() {
		if matchesSession(e, sid) {
			unlock, err := s.lockPlaybackOwner(s.ctx, e.user.ID, e.client.ClientID)
			if err != nil {
				return false
			}
			defer unlock()
			if s.playbackOwners.Stale(e.user.ID, e.client.ClientID, e.key, e.client.SessionID) {
				s.cleanSupersededEncoders(s.ctx, e.user.ID, e.client.ClientID, e.key, e.client.SessionID)
				return true
			}

			s.setOwnerTermination(e.id, reason)
			if s.d.Playback != nil {
				if ps, ok := s.d.Playback.SessionFor(e.user.ID, e.client.ClientID, e.key); ok && ps != nil {
					if stop, ok := s.playbackBroker.(interface {
						StopPlayback(context.Context, string) bool
					}); ok {
						if !stop.StopPlayback(s.ctx, ps.ID) {
							s.adoptBrokerTimeline(s.ctx, &Request{User: &e.user, ClientID: e.client.ClientID, SessionID: e.client.SessionID}, e.key)
							stop.StopPlayback(s.ctx, ps.ID)
						}
					}
				}
			}
			return true
		}
	}
	return false
}

// termination is an owner stream kill awaiting the client's next timeline.
type termination struct {
	reason    string
	at        time.Time
	delivered bool
	ownerKill bool
}

// terminationTTL bounds how long a kill waits for its client: one that
// never reports again (closed app, lost network) would otherwise keep its
// entry forever.
const terminationTTL = 10 * time.Minute

func (s *Server) setTermination(tid, reason string) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.terms == nil {
		s.terms = map[string]termination{}
	}
	if current, ok := s.terms[tid]; ok && current.ownerKill && s.now().Sub(current.at) <= terminationTTL {
		return
	}
	s.terms[tid] = termination{reason: reason, at: s.now()}
}

func (s *Server) takeTermination(tid string) (string, bool) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	t, ok := s.terms[tid]
	if !ok || t.delivered {
		return "", false
	}
	if t.ownerKill {
		t.delivered = true
		s.terms[tid] = t
	} else {
		delete(s.terms, tid)
	}
	return t.reason, true
}

func (s *Server) expireTerminations(now time.Time, ttl time.Duration) {
	s.decisionArms.Expire(now)
	s.termMu.Lock()
	defer s.termMu.Unlock()
	for tid, t := range s.terms {
		if now.Sub(t.at) > ttl {
			delete(s.terms, tid)
		}
	}
}

// playbackTerminated remains true after the one-shot timeline notice, preventing
// a stopped player's byte requests from reopening the same playback session.
func (s *Server) playbackTerminated(pr *Request) bool {
	if pr == nil || pr.User == nil {
		return false
	}
	s.termMu.Lock()
	defer s.termMu.Unlock()
	t, ok := s.terms[trackID(pr.User.ID, pr.ClientID, pr.SessionID)]
	return ok && t.ownerKill && s.now().Sub(t.at) <= terminationTTL
}

func (s *Server) setOwnerTermination(tid, reason string) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.terms == nil {
		s.terms = map[string]termination{}
	}
	s.terms[tid] = termination{reason: reason, at: s.now(), ownerKill: true}
}
