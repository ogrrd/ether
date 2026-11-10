package app

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type playabilityCapabilities struct {
	calls int
	err   error
}

func TestItemDetailRemainsVisibleWhenPlaybackDenied(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 99, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(44), Type: core.TypeMovie, IDs: core.IDs{TMDB: 44}, Title: "Visible", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	service, err := a.newSurfaceExperience("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	surface := service.(*surfaceExperience)
	calls := 0
	surface.playability = func(context.Context, frontend.ItemRequest) *bool { calls++; denied := false; return &denied }
	detail, err := service.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: core.Device{ID: "tv"}, Key: title.Key})
	if err != nil || detail.Title.Key != title.Key || detail.CanPlay == nil || *detail.CanPlay || calls != 1 {
		t.Fatalf("detail=%+v error=%v calls=%d", detail, err, calls)
	}
	_, err = service.Item(ctx, frontend.ItemRequest{Viewer: &core.User{ID: viewer.ID + 10000}, Key: title.Key})
	if err == nil || calls != 1 {
		t.Fatal("unauthorized viewer reached playback policy")
	}
}

func (c *playabilityCapabilities) Build(_ context.Context, r frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
	c.calls++
	return frontend.CapabilityResult{Client: core.ClientCaps{ClientID: r.Device.ClientID, Remote: true}}, c.err
}

func (c *playabilityCapabilities) ApplyClientRules(_ frontend.HostQualityConfig, caps core.ClientCaps, _ *core.Viewer, _ string) core.ClientCaps {
	return caps
}

type playabilityBroker struct {
	calls   int
	request frontend.PlanRequest
	err     error
}

func (p *playabilityBroker) Plan(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	p.calls++
	p.request = r
	return frontend.PlayPlan{}, p.err
}

func TestDetailCanPlayUsesTrustedPeerAndEstimateOnly(t *testing.T) {
	req := frontend.ItemRequest{Viewer: &core.User{ID: 7}, Device: core.Device{ID: "tv"}, Key: core.MovieKey(5)}
	caps := &playabilityCapabilities{}
	broker := &playabilityBroker{}
	if got := detailCanPlay(context.Background(), req, "jellyfin", caps, broker); got != nil || caps.calls != 0 || broker.calls != 0 {
		t.Fatal("accepted missing trusted peer")
	}
	ctx := frontend.WithClientIP(context.Background(), netip.MustParseAddr("203.0.113.9"))
	got := detailCanPlay(ctx, req, "jellyfin", caps, broker)
	if got == nil || !*got || broker.calls != 1 || !broker.request.Intent.Estimate || broker.request.Version != "" || broker.request.Item != req.Key || broker.request.Viewer.ID != req.Viewer.ID || broker.request.Device.ID != req.Device.ID || !broker.request.Caps.Remote {
		t.Fatalf("result %v, request %+v", got, broker.request)
	}
	broker.err = core.ErrUnauthorized
	if got = detailCanPlay(ctx, req, "jellyfin", caps, broker); got == nil || *got {
		t.Fatal("denied playback not reflected")
	}
	broker.err = core.ErrUnreleased
	if got = detailCanPlay(ctx, req, "jellyfin", caps, broker); got == nil || *got {
		t.Fatal("unreleased playback not reflected")
	}
	broker.err = errors.New("temporary failure")
	if got = detailCanPlay(ctx, req, "jellyfin", caps, broker); got != nil {
		t.Fatal("transient failure claimed a policy result")
	}
	caps.err = errors.New("untrusted")
	before := broker.calls
	if got = detailCanPlay(ctx, req, "jellyfin", caps, broker); got != nil || broker.calls != before {
		t.Fatal("failed capability check reached broker")
	}
}
