package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestFrontendPartialRuntimeCloseIsSingleOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var watch, artwork, server atomic.Int32
	a := &App{plexClose: func() { server.Add(1) }}
	r := &frontendRuntime{app: a, ctx: ctx, cancel: cancel, stateUnsub: func() { watch.Add(1) }, artworkWait: func() { artwork.Add(1) }}
	r.close()
	r.close()
	if ctx.Err() == nil || watch.Load() != 1 || artwork.Load() != 1 || server.Load() != 1 || a.plexClose != nil {
		t.Fatalf("close counts %d %d %d ctx=%v", watch.Load(), artwork.Load(), server.Load(), ctx.Err())
	}
}

func TestPlexFactoryCancellationUnderConstructionLock(t *testing.T) {
	a := &App{}
	ctx, cancel := context.WithCancel(context.Background())
	a.plexBuildMu.Lock()
	done := make(chan error, 1)
	go func() { _, err := a.buildPlex(ctx, config.Config{}); done <- err }()
	cancel()
	a.plexBuildMu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled construction blocked")
	}
	if a.currentPlexServer() != nil || a.plexClose != nil {
		t.Fatal("cancelled construction published a server")
	}
}

func TestNeutralServerLibrariesMatchPlexIncludingSpike(t *testing.T) {
	for _, spike := range []bool{false, true} {
		cfg := config.Config{}
		cfg.Spike.Enabled = spike
		deps := plex.Deps{
			SessionStores:     sessions.NewSessionStores(),
			Experience:        experience.NewSelectors(),
			DisableEnrichment: true,
		}
		if spike {
			deps.Sections = []plex.Section{{ID: 1, Type: core.TypeMovie, Title: "Movies"}}
		}
		server := plex.NewServer(deps)
		got, want := serverLibraries(cfg), server.LibrarySections()
		_ = server.Close()
		if len(got) != len(want) {
			t.Fatalf("spike=%v library count %d want %d", spike, len(got), len(want))
		}
		for i := range got {
			if got[i].ID != want[i].ID || got[i].Type != want[i].Type || got[i].Name != want[i].Title {
				t.Fatalf("library ordering/identity mismatch: %+v %+v", got, want)
			}
		}
		visibility := experienceVisibility(nil, nil, nil, nil, got)
		for _, kind := range []core.MediaType{core.TypeMovie, core.TypeShow, core.TypeSeason, core.TypeEpisode, core.MediaType("unknown")} {
			wantType := core.TypeMovie
			if kind == core.TypeShow || kind == core.TypeSeason || kind == core.TypeEpisode {
				wantType = core.TypeShow
			}
			wantID, wantOK := 0, false
			for _, section := range want {
				if section.Type == wantType {
					wantID, wantOK = section.ID, true
					break
				}
			}
			if id, ok := visibility.SectionFor(kind); id != wantID || ok != wantOK {
				t.Fatalf("spike=%v kind=%s id=%d/%v want=%d/%v", spike, kind, id, ok, wantID, wantOK)
			}
		}
	}
}
