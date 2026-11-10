package plex

import (
	"context"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type durablePlexBinding interface {
	PlexHLSDistributed(string) bool
	SavePlexBinding(context.Context, string, []byte) error
	LoadPlexBinding(context.Context, string) ([]byte, error)
}
type plexBindingSnapshot struct {
	Binding   frontend.Binding[struct{}]
	ReadToken string
	Source    frontend.PlaySession
}

func (s *Server) saveDistributedBinding(ctx context.Context, id string) error {
	storage, ok := s.currentPlexEncoder().(durablePlexBinding)
	if !ok || !storage.PlexHLSDistributed(id) {
		return nil
	}
	entry, ok := s.transcodes.get(id)
	if !ok {
		return frontend.ErrNotAvailable
	}
	ps, ok := s.d.Playback.Session(entry.playID)
	if !ok || ps == nil {
		return frontend.ErrNotAvailable
	}
	b := entry.binding()
	snapshot := plexBindingSnapshot{Binding: frontend.Binding[struct{}]{ID: b.ID, StreamToken: b.StreamToken, PlayID: b.PlayID, PlaybackID: b.PlaybackID, UserID: b.UserID, ClientID: b.ClientID, Key: b.Key, Decision: b.Decision, Info: b.Info, Subtitle: b.Subtitle, Caps: b.Caps, Opts: b.Opts, Offset: b.Offset, Started: b.Started, UpdatedAt: b.UpdatedAt}, ReadToken: s.readCaps.token(id), Source: *ps}
	snapshot.Source.Source = nil
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return storage.SavePlexBinding(ctx, id, raw)
}

// Restore only protocol authentication facts. The original middleware still
// checks live viewer, device, title and policy. Execution attachment is separate.
func (s *Server) loadDistributedBinding(id string) (*transcodeEntry, bool) {
	storage, ok := s.currentPlexEncoder().(durablePlexBinding)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	raw, err := storage.LoadPlexBinding(ctx, id)
	if err != nil {
		return nil, false
	}
	var snapshot plexBindingSnapshot
	if len(raw) > 256<<10 || json.Unmarshal(raw, &snapshot) != nil {
		return nil, false
	}
	b := snapshot.Binding
	if b.ID != id || !b.Started || b.UserID <= 0 || b.StreamToken == "" || b.PlayID == "" || snapshot.Source.ID != b.PlayID || snapshot.Source.UserID != b.UserID || snapshot.Source.Key != b.Key {
		return nil, false
	}
	if snapshot.ReadToken != "" {
		s.readCaps.mu.Lock()
		if old := s.readCaps.bySession[id]; old != "" {
			delete(s.readCaps.byToken, old)
		}
		s.readCaps.byToken[snapshot.ReadToken] = readCapability{sessionID: id, clientID: b.ClientID, userID: b.UserID}
		s.readCaps.bySession[id] = snapshot.ReadToken
		s.readCaps.mu.Unlock()
	}
	if restore, ok := s.d.Playback.(interface {
		RestoreSource(context.Context, string, frontend.PlaySession) error
	}); ok {
		if restore.RestoreSource(ctx, id, snapshot.Source) != nil {
			return nil, false
		}
	}
	return &transcodeEntry{id: id, streamToken: b.StreamToken, playID: b.PlayID, playbackID: b.PlaybackID, userID: b.UserID, clientID: b.ClientID, key: b.Key, decision: b.Decision, info: b.Info, subtitle: b.Subtitle, caps: b.Caps, opts: b.Opts, offset: b.Offset, started: b.Started, updatedAt: b.UpdatedAt}, true
}
