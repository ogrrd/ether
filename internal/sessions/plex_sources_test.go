package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type sourceAdmissionProbe struct {
	core.Playback
	waited, admitted int
	waitErr          error
	release          string
}

func (p *sourceAdmissionProbe) PrepareWithin(ctx context.Context, wait time.Duration, u *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	p.waited++
	p.release = release
	return nil, p.waitErr
}
func (p *sourceAdmissionProbe) Prepare(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	p.admitted++
	return &core.PlaySession{ID: "auto", UserID: u.ID, Key: key, ClientID: caps.ClientID}, nil
}
func (p *sourceAdmissionProbe) PrepareVariant(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, release string) (*core.PlaySession, error) {
	p.admitted++
	p.release = release
	return &core.PlaySession{ID: release, UserID: u.ID, Key: key, ClientID: caps.ClientID}, nil
}
func TestPlexSourcesPreserveBoundedAdmission(t *testing.T) {
	p := &sourceAdmissionProbe{waitErr: context.Canceled}
	source := PartSources{Playback: p}
	viewer := &core.User{ID: 7}
	caps := core.ClientCaps{ClientID: "device"}
	if _, err := source.Prepare(context.Background(), viewer, core.MovieKey(1), caps, "picked", time.Second); !errors.Is(err, context.Canceled) || p.admitted != 0 {
		t.Fatalf("wait failure fell through: %v %+v", err, p)
	}
	p.waitErr = nil
	ps, err := source.Prepare(context.Background(), viewer, core.MovieKey(1), caps, "picked", time.Second)
	if err != nil || ps.ID != "picked" || p.admitted != 1 || p.waited != 2 {
		t.Fatalf("variant admission: %+v %v %+v", ps, err, p)
	}
	// A base-only manager intentionally retains automatic fallback.
	source.Playback = struct{ core.Playback }{p}
	ps, err = source.Prepare(context.Background(), viewer, core.MovieKey(1), caps, "picked", time.Second)
	if err != nil || ps.ID != "auto" || p.admitted != 2 {
		t.Fatalf("automatic fallback: %+v %v", ps, err)
	}
}
