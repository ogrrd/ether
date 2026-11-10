package clusterplayback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// SavePlexBinding retains opaque protocol-owned credentials only in the shared
// database. It never exports them to membership, events, logs or historical
// admission records. Only the live coordinator may publish this generation.
func (r *Runtime) SavePlexBinding(ctx context.Context, id string, raw []byte) error {
	if len(raw) == 0 || len(raw) > maxAdmissionBytes || !r.ownsCoordination(ctx, id) {
		return ErrDenied
	}
	route, err := r.route(ctx, id)
	if err != nil {
		return err
	}
	record, err := r.o.Placement.Lookup(ctx, id)
	if err != nil || record.Fence != route.Fence || record.Retired || !record.Authority {
		return ErrDenied
	}
	c, err := r.CurrentCoordinator(ctx, id)
	if err != nil {
		return err
	}
	result, err := r.o.DB.ExecContext(ctx, `INSERT INTO clusterplayback_plex_bindings(session,generation,binding) SELECT session,generation,? FROM clusterplayback_coordinators WHERE session=? AND generation=? AND node=? AND boot=? AND epoch=? AND deadline>? ON CONFLICT(session) DO UPDATE SET generation=excluded.generation,binding=excluded.binding WHERE clusterplayback_plex_bindings.generation<=excluded.generation`, string(raw), id, route.Fence.Generation, r.o.Self.NodeID, r.o.Self.Boot, c.Epoch, time.Now().Add(r.o.ClockSkew).UnixMilli())
	if err == nil {
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrDenied
		}
	}

	return err
}
func (r *Runtime) LoadPlexBinding(ctx context.Context, id string) ([]byte, error) {
	if r.o.MemberRole(ctx, r.o.Self, config.RoleServer) != nil {
		return nil, ErrDenied
	}
	record, err := r.o.Placement.Lookup(ctx, id)
	if err != nil || record.Retired || !record.Authority {
		return nil, ErrDenied
	}
	var raw string
	err = r.o.DB.QueryRowContext(ctx, `SELECT binding FROM clusterplayback_plex_bindings WHERE session=? AND generation=?`, id, record.Generation).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAdmissionBytes {
		return nil, ErrDenied
	}
	return []byte(raw), nil
}

// RestoreSource restores a read-only source snapshot for original protocol
// authorization. It creates no preparation, producer, source pin or ownership.
func (p *RemotePlayback) RestoreSource(ctx context.Context, id string, snapshot core.PlaySession) error {
	return p.restoreProtocolSource(ctx, id, snapshot)
}
func (p *RemotePlayback) RestoreNeutralSource(ctx context.Context, id string, snapshot core.PlaySession) error {
	return p.restoreProtocolSource(ctx, id, snapshot)
}
func (p *RemotePlayback) restoreProtocolSource(ctx context.Context, id string, snapshot core.PlaySession) error {
	route, err := p.r.route(ctx, id)
	if err != nil {
		return err
	}
	record, err := p.r.o.Placement.Lookup(ctx, id)
	if err != nil || record.Retired || !record.Authority || record.Fence != route.Fence || route.Viewer != snapshot.UserID {
		return ErrDenied
	}
	admission, err := p.r.readExecutionAdmission(ctx, route)
	if err != nil {
		return err
	}
	if snapshot.Info.Size != admission.Request.Info.Size {
		return ErrDenied
	}
	snapshot.Source = remoteSource{revision: route.Revision, release: admission.Request.ReleaseID, size: snapshot.Info.Size}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observed[snapshot.ID] == nil && len(p.observed) >= p.r.o.HistoryCapacity {
		return ErrPending
	}
	p.observed[snapshot.ID] = &observedSession{session: id, generation: record.Generation, snapshot: copyPlaySession(&snapshot)}
	return nil
}

func (r *Runtime) DeleteBinding(ctx context.Context, id string) error {
	c, err := r.CurrentCoordinator(ctx, id)
	if err != nil {
		return err
	}
	if c.Identity != r.o.Self {
		return ErrDenied
	}
	_, err = r.o.DB.ExecContext(ctx, `DELETE FROM clusterplayback_plex_bindings WHERE session=? AND generation=(SELECT generation FROM clusterplayback_coordinators WHERE session=? AND node=? AND boot=? AND epoch=?)`, id, id, r.o.Self.NodeID, r.o.Self.Boot, c.Epoch)
	return err
}
