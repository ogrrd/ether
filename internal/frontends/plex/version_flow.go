package plex

import (
	"context"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

const versionPreplayTTL = frontend.VersionPreplayTTL
const (
	versionRead   = frontend.VersionRead
	versionOffer  = frontend.VersionOffer
	versionFreeze = frontend.VersionFreeze
	versionEnd    = frontend.VersionEnd
	versionTouch  = frontend.VersionTouch
)

type versionSnapshot struct {
	variants []frontend.ReleaseVariant
	frozen   bool
	until    time.Time
}
type versionFlowStore struct{ flows frontend.VersionFlows }

func newVersionFlowStore(s *Server, db *sql.DB) *versionFlowStore {
	flows := s.runtime.NewVersionFlows(s.ctx, db)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); flows.Run() }()
	return &versionFlowStore{flows: flows}
}
func (v *versionFlowStore) call(ctx context.Context, k playIdentityKey, vs []frontend.ReleaseVariant, mode int, now time.Time, ttl time.Duration) (versionSnapshot, error) {
	snap, err := v.flows.Call(ctx, frontend.VersionFlowKey{UserID: k.userID, ClientID: k.clientID, Key: k.key}, vs, mode, now, ttl)
	return versionSnapshot{snap.Variants, snap.Frozen, snap.Until}, err
}

func (s *Server) endVersionFlow(ctx context.Context, k playIdentityKey) {
	s.playIDs.mu.Lock()
	if e := s.playIDs.m[k]; e != nil {
		// End the freeze, keep the issued list: a stopped timeline is also
		// how a client restarts playback to apply a subtitle it just
		// downloaded (#389). It re-reads the item and addresses the Part
		// from the list it still holds, then re-decides with its held
		// mediaIndex; clearing the list here collapsed that re-read to
		// one Media and refused the index. The list expires with the
		// identity instead (playIdentityTTL from last use).
		e.frozen = false
		e.flowUntil = time.Time{}
		e.selected = 0
		e.admittedRelease = ""
		e.autoAligned = ""
		e.at = s.now()
	}
	s.playIDs.mu.Unlock()
	if s.versionFlows != nil {
		if _, err := s.versionFlows.call(ctx, k, nil, versionEnd, s.now(), 0); err != nil {
			s.log.Warn("end version flow", "err", err)
		}
	}
}
