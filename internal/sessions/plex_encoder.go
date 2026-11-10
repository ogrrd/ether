package sessions

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
)

// StartSegmentedHLS is the shared universal encoder seam after the Plex adapter has
// performed its existing acceptance, policy, selection and grant checks. Errors
// never authorize caller fallback or a second encoder.
func (b *Broker) StartSegmentedHLS(ctx context.Context, viewer *core.User, playID, publicID string, q core.HLSRequest) (core.HLSSession, error) {
	if q.SessionID != publicID || playID == "" || !validPlexTranscodeID(publicID) {
		return nil, core.ErrUnauthorized
	}
	v, err := b.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	entry := b.sessions[publicID]
	if entry == nil || entry.public.Protocol != "plex" || entry.public.ViewerID != v.ID || entry.playID != playID || b.closed {
		b.mu.Unlock()
		return nil, core.ErrUnauthorized
	}
	copy := *entry
	b.mu.Unlock()
	if !b.currentMediaPolicy(ctx, v, &copy) {
		return nil, core.ErrUnauthorized
	}
	ps, ok := b.deps.Playback.Session(playID)
	if !ok || ps == nil || ps.UserID != v.ID || ps.Key != copy.public.Item || q.ReleaseID != ps.SourceReleaseID() {
		return nil, core.ErrNotAvailable
	}
	if copy.allowTranscoding != nil && !*copy.allowTranscoding && decisionEncodes(q.Decision, ps.Info) {
		return nil, core.ErrUnauthorized
	}
	if remote, ok := b.deps.Playback.(interface {
		RevalidateSession(context.Context, string) error
	}); ok {
		if err = remote.RevalidateSession(ctx, playID); err != nil {
			return nil, err
		}
	}
	remote := b.deps.DistributedHLS
	if remote != nil {
		if remote.Supports(q) {
			revision := sourceRevision(ps)
			if revision == "" {
				return nil, core.ErrSourceChanged
			}
			return remote.StartDistributedHLS(ctx, v.ID, playID, revision, q)
		}
		if remote.Allocated(publicID) {
			return nil, core.ErrNotAvailable
		}
	}
	if b.deps.Transcoder == nil {
		return nil, core.ErrNotAvailable
	}
	// A local Plex encoder reads the same playback source as a broker
	// start, so it needs the same input lease: without one its retirement
	// can never verify input-producer drain and every Plex session close
	// ended "encoder input producer retirement is unverified" (#410).
	var lease core.EncoderInputLease
	if q.InputLease == nil {
		var err error
		lease, err = b.mintEncoderInput(ctx, playID)
		if err != nil {
			return nil, err
		}
		q.InputLease = lease
	}
	hs, err := b.deps.Transcoder.StartHLS(ctx, q)
	if (err != nil || hs == nil) && lease != nil {
		lease.SealInput()
	}
	return hs, err
}

// SegmentedHLSSession reports ownership independently of current availability. A nil
// session with true means retained remote responsibility, never local fallback.
func (b *Broker) SegmentedHLSSession(id string) (core.HLSSession, bool) {
	if remote := b.deps.DistributedHLS; remote != nil && remote.Allocated(id) {
		session, _ := remote.Session(id)
		return session, true
	}
	if b.deps.Transcoder == nil {
		return nil, false
	}
	return b.deps.Transcoder.Session(id)
}

// StopSegmentedHLS handles local and remote encoder retirement through the broker.
// Remote ownership includes uncertain start/stop phases; true reports ownership,
// not proof that an asynchronous remote retirement has completed.
func (b *Broker) StopSegmentedHLS(id string) bool {
	if remote := b.deps.DistributedHLS; remote != nil && remote.Allocated(id) {
		remote.Stop(id)
		return true
	}
	if b.deps.Transcoder != nil {
		b.deps.Transcoder.Stop(id)
		return true
	}
	return false
}

// BindPlexDelivery preserves the original authenticated Plex handler at owner.
func (b *Broker) BindPlexDelivery(handler http.Handler) error {
	if remote, ok := b.deps.DistributedHLS.(interface{ BindPlexDelivery(http.Handler) error }); ok {
		return remote.BindPlexDelivery(handler)
	}
	return core.ErrNotAvailable
}
func (b *Broker) ForwardEncodedMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	if remote, ok := b.deps.DistributedHLS.(interface {
		ForwardEncodedMedia(http.ResponseWriter, *http.Request, string) bool
	}); ok {
		return remote.ForwardEncodedMedia(w, r, id)
	}
	return false
}

func (b *Broker) ForwardGrantMedia(w http.ResponseWriter, r *http.Request, id string) bool {
	if remote, ok := b.deps.DistributedHLS.(interface {
		ForwardGrantMedia(http.ResponseWriter, *http.Request, string) bool
	}); ok {
		return remote.ForwardGrantMedia(w, r, id)
	}
	return false
}

// PlexHLSDistributed reports allocation before protocol code reads any binding
// or source snapshot. Local sessions retain their original publication path.
func (b *Broker) PlexHLSDistributed(id string) bool {
	return b.deps.DistributedHLS != nil && b.deps.DistributedHLS.Allocated(id)
}

func (b *Broker) SavePlexBinding(ctx context.Context, id string, raw []byte) error {
	if b.deps.DistributedHLS == nil || !b.deps.DistributedHLS.Allocated(id) {
		return nil
	}
	if remote, ok := b.deps.DistributedHLS.(interface {
		SavePlexBinding(context.Context, string, []byte) error
	}); ok {
		return remote.SavePlexBinding(ctx, id, raw)
	}
	return core.ErrNotAvailable
}
func (b *Broker) LoadPlexBinding(ctx context.Context, id string) ([]byte, error) {
	if remote, ok := b.deps.DistributedHLS.(interface {
		LoadPlexBinding(context.Context, string) ([]byte, error)
	}); ok {
		return remote.LoadPlexBinding(ctx, id)
	}
	return nil, core.ErrNotAvailable
}

func (b *Broker) DeleteBinding(ctx context.Context, id string) error {
	if remote, ok := b.deps.DistributedHLS.(interface {
		DeleteBinding(context.Context, string) error
	}); ok {
		return remote.DeleteBinding(ctx, id)
	}
	return core.ErrNotAvailable
}
