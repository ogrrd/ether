package plex

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// slotHash is the visible id for a subtitle search offer. Offers are not
// stream bindings; playback ids come from the broker.
func slotHash(key frontend.RatingKey, slot string) int64 { return frontend.WireID(key, slot) }

// ---------------------------------------------------------------------------
// Stream selections (PUT /library/parts/{id}?audioStreamID=&subtitleStreamID=)

// Selection identity excludes mutable probe/default hints. It includes the
// distinguishing audio/subtitle metadata so an old ID cannot change language
// when a later release reuses the same stream position.
type selection struct {
	audio       int // position in MediaInfo.Streams
	audioSet    bool
	audioStream *frontend.StreamInfo
	subtitle    int // position, -1 = none
	subtitleSet bool
	// subtitleStream is the chosen subtitle's identity (language, codec,
	// and source), not its position. A later variant order can put a
	// different track in the old slot; resolveSubtitle finds this one.
	subtitleStream *frontend.StreamInfo
}

type selectionStore struct {
	store *frontend.Selections[selection]
}

func newSelectionStore() *selectionStore {
	return &selectionStore{store: frontend.NewSelections[selection]()}
}
func (s *selectionStore) get(userID, partID int64) selection { return s.store.Get(userID, partID) }
func (s *selectionStore) update(userID, partID int64, fn func(*selection)) {
	s.store.Update(userID, partID, fn)
}

// ---------------------------------------------------------------------------
// Transcode bindings: client transcode session id -> play session/decision

type transcodeEntry struct {
	id          string // client "session" param
	streamToken string // independent random capability; never a client-chosen ID
	playID      string // frontend.PlaySession.ID
	playbackID  string // explicit X-Plex-Session-Identifier owning this encoder
	userID      int64
	clientID    string
	key         frontend.RatingKey
	requested   playRequest // original decision/start inputs, before policy refinement
	decision    frontend.Decision
	info        frontend.MediaInfo
	subtitle    int // selected subtitle's position in info, -1 = none
	caps        frontend.ClientCaps
	opts        frontend.DecideOptions
	offset      time.Duration
	started     bool // StartHLS ran
	updatedAt   time.Time
	// A failed decision for this binding (failKey, failErr at failedAt):
	// the start request that follows it answers with the same error at
	// once instead of resolving again, including media loader retries.
	failKey  frontend.RatingKey
	failErr  error
	failedAt time.Time
}

// decisionFailureTTL bounds how long a failed decision answers the start
// request that follows it; after that a start resolves afresh.
const decisionFailureTTL = 15 * time.Second

// stoppedTranscodeTTL bounds how long a session retired by timeline?state=stopped
// or universal/stop answers a duplicate universal/stop from the same viewer with
// 200 OK instead of 404 (#408).
const stoppedTranscodeTTL = 2 * time.Minute

type stoppedTranscode struct {
	userID int64
	at     time.Time
}

type transcodeTable struct {
	load      func(string) (*transcodeEntry, bool)
	table     *frontend.BindingTable[playRequest]
	stoppedMu sync.Mutex
	stopped   map[string]stoppedTranscode
}

func newTranscodeTable() *transcodeTable {
	return &transcodeTable{table: frontend.NewBindingTable[playRequest](), stopped: map[string]stoppedTranscode{}}
}
func (e *transcodeEntry) binding() *frontend.Binding[playRequest] {
	return &frontend.Binding[playRequest]{ID: e.id, StreamToken: e.streamToken, PlayID: e.playID, PlaybackID: e.playbackID, UserID: e.userID, ClientID: e.clientID, Key: e.key, Requested: e.requested, Decision: e.decision, Info: e.info, Subtitle: e.subtitle, Caps: e.caps, Opts: e.opts, Offset: e.offset, Started: e.started, UpdatedAt: e.updatedAt, FailKey: e.failKey, FailErr: e.failErr, FailedAt: e.failedAt}
}
func plexBinding(b *frontend.Binding[playRequest]) *transcodeEntry {
	return &transcodeEntry{id: b.ID, streamToken: b.StreamToken, playID: b.PlayID, playbackID: b.PlaybackID, userID: b.UserID, clientID: b.ClientID, key: b.Key, requested: b.Requested, decision: b.Decision, info: b.Info, subtitle: b.Subtitle, caps: b.Caps, opts: b.Opts, offset: b.Offset, started: b.Started, updatedAt: b.UpdatedAt, failKey: b.FailKey, failErr: b.FailErr, failedAt: b.FailedAt}
}
func (t *transcodeTable) put(e *transcodeEntry) {
	b := e.binding()
	t.table.Put(b)
	e.streamToken = b.StreamToken
	t.stoppedMu.Lock()
	delete(t.stopped, e.id)
	t.stoppedMu.Unlock()
}
func (t *transcodeTable) claim(id string, userID int64, clientID string, now time.Time) bool {
	return t.table.Claim(id, userID, clientID, now)
}
func (t *transcodeTable) get(id string) (*transcodeEntry, bool) {
	b, ok := t.table.Get(id)
	// Decision and start may land on different server replicas. A local
	// decision is not the started stream binding: its grant and source facts
	// predate the durable snapshot published by the starting server. Restore
	// that snapshot without replacing the local decision/settings or token.
	if t.load != nil && (!ok || !b.Started) {
		if restored, found := t.load(id); found {
			if !ok || restored.userID == b.UserID && restored.clientID == b.ClientID {
				return restored, true
			}
		}
	}
	if !ok {
		return nil, false
	}
	return plexBinding(b), true
}
func (t *transcodeTable) fail(id string, userID int64, clientID string, key frontend.RatingKey, err error, now time.Time) {
	// awaitSource fails before gated admission has claimed a binding.
	if t.table.Claim(id, userID, clientID, now) {
		t.table.Fail(id, userID, clientID, key, err, now)
	}
}
func (t *transcodeTable) takeFailure(id string, userID int64, key frontend.RatingKey, now time.Time) (error, bool) {
	e, ok := t.table.Get(id)
	if !ok || e.UserID != userID || e.FailKey != key || e.FailErr == nil || now.Sub(e.FailedAt) >= decisionFailureTTL {
		return nil, false
	}
	return e.FailErr, true
}
func (t *transcodeTable) touch(id string, now time.Time) { t.table.Touch(id, now) }
func (t *transcodeTable) remove(id string)               { t.table.Remove(id) }
func (t *transcodeTable) retire(id string, now time.Time) {
	if b, ok := t.table.Get(id); ok && b.UserID != 0 && id != "" {
		t.stoppedMu.Lock()
		if t.stopped == nil {
			t.stopped = map[string]stoppedTranscode{}
		}
		for k, v := range t.stopped {
			if now.Sub(v.at) >= stoppedTranscodeTTL {
				delete(t.stopped, k)
			}
		}
		if len(t.stopped) >= 256 {
			var oldestKey string
			var oldestAt time.Time
			for k, v := range t.stopped {
				if oldestKey == "" || v.at.Before(oldestAt) {
					oldestKey, oldestAt = k, v.at
				}
			}
			if oldestKey != "" {
				delete(t.stopped, oldestKey)
			}
		}
		t.stopped[id] = stoppedTranscode{userID: b.UserID, at: now}
		t.stoppedMu.Unlock()
	}
	t.table.Remove(id)
}
func (t *transcodeTable) wasStoppedBy(id string, userID int64, now time.Time) bool {
	if id == "" || userID == 0 {
		return false
	}
	t.stoppedMu.Lock()
	defer t.stoppedMu.Unlock()
	st, ok := t.stopped[id]
	if !ok {
		return false
	}
	if now.Sub(st.at) >= stoppedTranscodeTTL {
		delete(t.stopped, id)
		return false
	}
	return st.userID == userID
}
func (t *transcodeTable) forClient(userID int64, clientID string, key frontend.RatingKey) []string {
	return t.table.ForClient(userID, clientID, key)
}
func (t *transcodeTable) expire(now time.Time, ttl time.Duration) []string {
	return t.table.Expire(now, ttl)
}

// ---------------------------------------------------------------------------
// Facet cache: genre/country keys -> titles for filter resolution.

type facetCache struct {
	mu sync.Mutex
	m  map[string]facetEntry
}

type facetEntry struct {
	vals []frontend.FacetValue
	at   time.Time
}

func newFacetCache() *facetCache { return &facetCache{m: map[string]facetEntry{}} }

const facetTTL = 10 * time.Minute

type facetFilterCatalog interface {
	FacetFilter(ctx context.Context, f frontend.Filter, facet string) ([]frontend.FacetValue, error)
}

func (s *Server) firstCharacter(ctx context.Context, f frontend.Filter) ([]frontend.FacetValue, error) {
	f.FirstCharacter = ""
	if fc, ok := s.d.Catalog.(facetFilterCatalog); ok {
		return fc.FacetFilter(ctx, f, "firstCharacter")
	}
	return s.d.Catalog.Facet(ctx, f.Type, "firstCharacter")
}

func (s *Server) facet(ctx context.Context, t frontend.MediaType, name string) ([]frontend.FacetValue, error) {
	if name == "firstCharacter" {
		return s.firstCharacter(ctx, frontend.Filter{Type: t})
	}
	k := string(t) + "/" + name
	s.facets.mu.Lock()
	e, ok := s.facets.m[k]
	s.facets.mu.Unlock()
	if ok && s.now().Sub(e.at) < facetTTL {
		return e.vals, nil
	}
	vals, err := s.d.Catalog.Facet(ctx, t, name)
	if err != nil {
		return nil, err
	}
	s.facets.mu.Lock()
	s.facets.m[k] = facetEntry{vals: vals, at: s.now()}
	s.facets.mu.Unlock()
	return vals, nil
}

// facetTitle maps a filter value (a facet key or already a title) to the
// facet title the catalogue filters on.
func (s *Server) facetTitle(ctx context.Context, t frontend.MediaType, name, value string) string {
	if _, err := strconv.Atoi(value); err != nil {
		// A facet's values are its titles; only a numeric key can name a
		// different title. Looking a name up counted the facet over the
		// whole library on every filtered page the facet cache had not seen.
		return value
	}
	vals, err := s.facet(ctx, t, name)
	if err != nil {
		return value
	}
	for _, v := range vals {
		if v.Key == value {
			return v.Title
		}
	}
	return value
}

// byPlayID returns the transcode session ids fed by a playback session.
func (t *transcodeTable) byPlayID(playID string) []string { return t.table.ByPlayID(playID) }
func (t *transcodeTable) ids() []string                   { return t.table.IDs() }
