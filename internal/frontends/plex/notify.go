package plex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// ---------------------------------------------------------------------------
// Play session tracking (timeline-driven; feeds /status/sessions and
// "playing" notifications).

type playEntry struct {
	id              string // userID/clientID/sessionIdentifier
	decisionSeen    bool
	playingSeen     bool
	sessionKey      int
	user            frontend.Viewer
	client          Request // product/platform/device snapshot (no User)
	key             frontend.RatingKey
	state           string
	offset          time.Duration
	duration        time.Duration
	playQueueItemID int64
	transcodeID     string
	startedAt       time.Time
	updatedAt       time.Time

	// Set on entries another cluster node serves (read from plex_sessions).
	node     string
	remoteTS *model.TranscodeSess
}

// sessionActivities reads and writes the mounted broker. A server that is
// not mounted keeps the rows itself so an in-flight request still sees them
// after unmount drops the broker binding.
type sessionActivities struct {
	mu     sync.Mutex
	local  map[string]playEntry
	next   int
	broker frontend.PlaybackActivityStore
}

func newSessionActivities() *sessionActivities {
	return &sessionActivities{local: map[string]playEntry{}}
}

func trackID(userID int64, clientID, sessionID string) string {
	return fmt.Sprintf("%d/%s/%s", userID, clientID, sessionID)
}
func (t *sessionActivities) bind(b frontend.PlaybackActivityStore) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.broker = b
	t.mu.Unlock()
}
func (t *sessionActivities) mounted() frontend.PlaybackActivityStore {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.broker
}
func (t *sessionActivities) has(id string) bool {
	if t == nil {
		return false
	}
	if b := t.mounted(); b != nil {
		return b.HasActivity(id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.local[id]
	return ok
}
func (t *sessionActivities) ensure(id string, key int) {
	if t == nil {
		return
	}
	if b := t.mounted(); b != nil {
		b.EnsureActivity(id, key)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.local[id]; !ok && key > 0 {
		t.local[id] = playEntry{id: id, sessionKey: key}
	}
}
func (t *sessionActivities) update(id string, fn func(*playEntry)) playEntry {
	if t == nil {
		var zero playEntry
		if fn != nil {
			fn(&zero)
		}
		return zero
	}
	if b := t.mounted(); b != nil {
		act := b.UpsertActivity(id, func(a *frontend.PlaybackActivity) {
			e := activityEntry(*a)
			fn(&e)
			*a = entryActivity(e)
		})
		return activityEntry(act)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.local[id]
	if !ok {
		t.next++
		e = playEntry{id: id, sessionKey: t.next}
	}
	fn(&e)
	t.local[id] = e
	return e
}
func (t *sessionActivities) remove(id string) (playEntry, bool) {
	if t == nil {
		return playEntry{}, false
	}
	if b := t.mounted(); b != nil {
		act, ok := b.RemoveActivity(id)
		if !ok {
			return playEntry{}, false
		}
		return activityEntry(act), true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.local[id]
	if !ok {
		return playEntry{}, false
	}
	delete(t.local, id)
	return e, true
}
func (t *sessionActivities) list() []playEntry {
	if t == nil {
		return nil
	}
	if b := t.mounted(); b != nil {
		acts := b.ListActivities()
		out := make([]playEntry, 0, len(acts))
		for _, a := range acts {
			out = append(out, activityEntry(a))
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]playEntry, 0, len(t.local))
	for _, e := range t.local {
		out = append(out, e)
	}
	return out
}
func (t *sessionActivities) expire(now time.Time, ttl time.Duration) []playEntry {
	if t == nil {
		return nil
	}
	if b := t.mounted(); b != nil {
		acts := b.ExpireActivities(now, ttl)
		out := make([]playEntry, 0, len(acts))
		for _, a := range acts {
			out = append(out, activityEntry(a))
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []playEntry
	for id, e := range t.local {
		if now.Sub(e.updatedAt) > ttl {
			out = append(out, e)
			delete(t.local, id)
		}
	}
	return out
}

func activityEntry(a frontend.PlaybackActivity) playEntry {
	e := playEntry{id: a.ID, sessionKey: a.SessionKey, decisionSeen: a.DecisionSeen, playingSeen: a.PlayingSeen, user: a.User, key: a.Key, state: a.State, offset: a.Offset, duration: a.Duration, playQueueItemID: a.PlayQueueItemID, transcodeID: a.TranscodeID, startedAt: a.StartedAt, updatedAt: a.UpdatedAt}
	e.client = Request{ClientID: a.ClientID, Product: a.Product, Version: a.Version, Platform: a.Platform, PlatformVersion: a.PlatformVersion, Device: a.Device, DeviceName: a.DeviceName, Model: a.Model, Vendor: a.Vendor, ProfileName: a.ProfileName, SessionID: a.SessionID, Remote: a.Remote}
	if a.RemoteIP != "" {
		e.client.RemoteIP = parseAddr(a.RemoteIP)
	}
	return e
}

func entryActivity(e playEntry) frontend.PlaybackActivity {
	a := frontend.PlaybackActivity{ID: e.id, SessionKey: e.sessionKey, DecisionSeen: e.decisionSeen, PlayingSeen: e.playingSeen, User: e.user, ClientID: e.client.ClientID, Product: e.client.Product, Version: e.client.Version, Platform: e.client.Platform, PlatformVersion: e.client.PlatformVersion, Device: e.client.Device, DeviceName: e.client.DeviceName, Model: e.client.Model, Vendor: e.client.Vendor, ProfileName: e.client.ProfileName, SessionID: e.client.SessionID, Remote: e.client.Remote, Key: e.key, State: e.state, Offset: e.offset, Duration: e.duration, PlayQueueItemID: e.playQueueItemID, TranscodeID: e.transcodeID, StartedAt: e.startedAt, UpdatedAt: e.updatedAt}
	if e.client.RemoteIP.IsValid() {
		a.RemoteIP = e.client.RemoteIP.String()
	}
	return a
}

// ---------------------------------------------------------------------------
// Notification fan-out

type subscriber struct {
	userID      int64
	owner       bool
	ch          chan notification
	filters     map[string]bool // empty = all
	includeLogs bool            // explicit empty filters includes logs
	language    string
}

type notification struct {
	typ  string
	data []byte   // JSON NotificationContainer (websocket)
	sse  [][]byte // one bare entry object per event (eventsource)
}

func newNotification(typ string, data []byte) notification {
	return notification{typ: typ, data: data, sse: sseEntries(data)}
}

// sseEntries splits a JSON NotificationContainer into the payloads PMS sends
// on /:/eventsource/notifications: one {"<Entry>":{...}} object per entry,
// with no container (recorded from PMS 1.43). Clients that read the
// eventsource stream ignore the websocket container, so library changes
// never reached their cached hubs.
func sseEntries(data []byte) [][]byte {
	var root struct {
		NotificationContainer map[string]json.RawMessage
	}
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	keys := make([]string, 0, len(root.NotificationContainer))
	for k, v := range root.NotificationContainer {
		if len(v) > 0 && v[0] == '[' {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out [][]byte
	for _, k := range keys {
		var items []json.RawMessage
		if json.Unmarshal(root.NotificationContainer[k], &items) != nil {
			continue
		}
		for _, item := range items {
			if b, err := json.Marshal(map[string]json.RawMessage{k: item}); err == nil {
				out = append(out, b)
			}
		}
	}
	return out
}

type notifier struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
	done   chan struct{}
}

func newNotifier() *notifier {
	return &notifier{subs: map[*subscriber]struct{}{}, done: make(chan struct{})}
}

func (n *notifier) subscribe(filters []string, user *frontend.Viewer, language ...string) (*subscriber, bool) {
	sub := &subscriber{userID: user.ID, owner: user.IsOwner, ch: make(chan notification, 64), filters: map[string]bool{}, includeLogs: filters != nil}
	if len(language) > 0 {
		sub.language = language[0]
	}
	for _, f := range filters {
		if f = strings.TrimSpace(f); f != "" {
			sub.filters[f] = true
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, false
	}
	n.subs[sub] = struct{}{}
	return sub, true
}

func (n *notifier) unsubscribe(sub *subscriber) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs, sub)
}

// encode serialises a notification for subscribers.
func encodeNotification(nc model.NotificationContainer) (notification, bool) {
	data, err := model.Marshal(nc, model.JSON)
	if err != nil {
		return notification{}, false
	}
	data = []byte(strings.TrimSuffix(string(data), "\n"))
	return newNotification(nc.Type, data), true
}

// deliver hands an encoded notification to every interested subscriber.
func (n *notifier) deliver(msg notification, userID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for sub := range n.subs {
		if msg.typ == "log" && !sub.owner {
			continue
		}
		if userID != 0 && sub.userID != userID && !sub.owner {
			continue
		}
		if (len(sub.filters) == 0 && !sub.includeLogs && msg.typ == "log") || (len(sub.filters) > 0 && !sub.filters[msg.typ]) {
			continue
		}
		select {
		case sub.ch <- localizeNotification(msg, sub.language):
		default:
		}
	}
}

func (n *notifier) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.closed {
		n.closed = true
		close(n.done)
	}
}

func notificationFilters(r *http.Request) []string {
	q := r.URL.Query()
	values, present := q["filters"]
	if !present {
		values, present = q["filter"]
	}
	if !present || len(values) == 0 {
		return nil
	}
	return strings.Split(values[len(values)-1], ",")
}

// handleWebsocket serves /:/websockets/notifications.
func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	u := websocket.Upgrader{
		CheckOrigin:      func(*http.Request) bool { return true }, // Plex tokens authorize access.
		HandshakeTimeout: 10 * time.Second,
	}
	h := w.Header().Clone()
	h.Set("Cache-Control", "no-cache")
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c, err := u.Upgrade(orderedUpgradeWriter{w}, r, h)
	if err != nil {
		s.log.Debug("websocket accept failed", "err", err)
		return
	}
	defer c.Close()
	sub, ok := s.notifier.subscribe(notificationFilters(r), FromContext(r.Context()).User, notificationLanguage(r))
	if !ok {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"), time.Now().Add(time.Second))
		return
	}
	defer s.notifier.unsubscribe(sub)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Reading processes masked control frames, echoes ping payloads and
		// acknowledges client closes. Application messages have no meaning here.
		c.SetReadLimit(64 << 10)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	defer func() { _ = c.Close(); <-readDone }()
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-readDone:
			return
		case <-s.notifier.done:
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"), time.Now().Add(time.Second))
			return
		case <-ping.C:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			// PMS's timer runs independently of pong reception and always uses
			// one byte, including connections held for many minutes.
			if err := c.WriteControl(websocket.PingMessage, []byte{0}, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case msg := <-sub.ch:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.WriteMessage(websocket.TextMessage, msg.data); err != nil {
				return
			}
		}
	}
}

// sseWriteTimeout bounds one Server-Sent Events write. Pings go out every
// 10s, so a healthy client never comes close.
const sseWriteTimeout = 2 * time.Minute

// handleEventSource serves /:/eventsource/notifications (Server-Sent Events).
func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	sub, ok := s.notifier.subscribe(notificationFilters(r), FromContext(r.Context()).User, notificationLanguage(r))
	if !ok {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	defer s.notifier.unsubscribe(sub)
	// No server WriteTimeout covers this long-lived response; arm a deadline
	// before each write so a client that stops reading errors out instead
	// of parking this goroutine and its subscription forever.
	rc := http.NewResponseController(w)
	arm := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	arm()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.notifier.done:
			return
		case <-ping.C:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			arm()
			if _, err := fmt.Fprint(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		case msg := <-sub.ch:
			if s.activeViewer(r.Context(), sub.userID) == nil {
				return
			}
			arm()
			payloads := msg.sse
			if len(payloads) == 0 {
				payloads = [][]byte{msg.data}
			}
			for _, data := range payloads {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.typ, data); err != nil {
					return
				}
			}
			fl.Flush()
		}
	}
}

// notifyPlaying publishes a PlaySessionStateNotification.
func (s *Server) notifyPlaying(e playEntry, state string) {
	if state == "" {
		state = e.state
	}
	s.publishNotification(model.NotificationContainer{
		Type: "playing",
		Size: 1,
		PlaySessionStateNotification: []model.PlaySessionStateNotification{{
			SessionKey:       fmt.Sprint(e.sessionKey),
			ClientIdentifier: e.client.ClientID,
			GUID:             "",
			RatingKey:        e.key.String(),
			URL:              "",
			Key:              "/library/metadata/" + e.key.String(),
			ViewOffset:       ms(e.offset),
			PlayQueueItemID:  e.playQueueItemID,
			State:            state,
			TranscodeSession: e.transcodeID,
		}},
	}, e.user.ID)
}

// WatchStateChanged invalidates client library metadata after a committed watch
// update. A playing notification only updates session status; clients can keep
// their cached Continue Watching shelf until a library timeline event arrives.
func (s *Server) WatchStateChanged(userID int64, key frontend.RatingKey) {
	var kind frontend.MediaType
	switch key.Kind() {
	case frontend.KindMovie:
		kind = frontend.TypeMovie
	case frontend.KindEpisode:
		kind = frontend.TypeEpisode
	default:
		return
	}
	s.notifyTimeline(key, kind, "", userID)
}

// notifyTimeline publishes a completed metadata change (watch state or versions).
// PMS terminal state 5 must omit metadataState/mediaState: carrying "created"
// is a processing hint, not the completed invalidation native detail pages need.
func (s *Server) notifyTimeline(key frontend.RatingKey, t frontend.MediaType, title string, userID int64) {
	sec, _ := s.sectionFor(t)
	typeNum := map[frontend.MediaType]int{frontend.TypeMovie: 1, frontend.TypeShow: 2, frontend.TypeSeason: 3, frontend.TypeEpisode: 4}[t]
	// PMS names an episode's season and show, and a season's show, so a
	// client can refresh the show's rows (Continue Watching, On Deck).
	var parent, root string
	switch key.Kind() {
	case frontend.KindEpisode:
		parent, root = key.Parent().String(), frontend.ShowKey(key.TMDB()).String()
	case frontend.KindSeason:
		parent = key.Parent().String()
	case frontend.KindMovie:
		// An open movie detail is the item itself. Episodes refresh because
		// the season and show ids are on the entry; a movie with only itemID
		// leaves the toolbar on "Mark as Watched" until the page is reopened.
		root = key.String()
	}
	s.publishNotification(model.NotificationContainer{
		Type: "timeline",
		Size: 1,
		TimelineEntry: []model.TimelineEntry{{
			Identifier:   model.LibraryIdentifier,
			SectionID:    fmt.Sprint(sec.ID),
			ItemID:       key.String(),
			ParentItemID: parent,
			RootItemID:   root,
			Type:         typeNum,
			Title:        title,
			State:        5,
			UpdatedAt:    s.now().Unix(),
		}},
	}, userID)
}

// publishNotification delivers a notification to this node's subscribers
// and, on a clustered node, to every other node's.
func (s *Server) publishNotification(nc model.NotificationContainer, userID int64) {
	s.recordActivities(nc)
	msg, ok := encodeNotification(nc)
	if !ok {
		return
	}
	s.notifier.deliver(msg, userID)
	if s.cl != nil {
		s.cl.shareNotification(msg, userID)
	}
}
