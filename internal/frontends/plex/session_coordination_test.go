package plex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestPlexUsesSharedPlaybackCoordination(t *testing.T) {
	coord := sessions.NewCoordination()
	f := newFixture(func(d *Deps) { d.Sessions = sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{Coordination: coord}) })
	defer f.srv.Close()
	unlock, err := coord.Gates.Acquire(context.Background(), "owner/1/device")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = f.srv.lockPlaybackOwner(ctx, 1, "device"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Plex bypassed shared gate: %v", err)
	}
}
