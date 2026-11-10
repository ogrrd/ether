package sessions

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPlexStreamBindingAndSidecar(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := users.BootstrapOwner(ctx, config.Plex{OwnerPlexID: 7, OwnerUsername: "owner", Token: "server-token"}); err != nil {
		t.Fatal(err)
	}
	if err := users.SyncFriends(ctx, []plextv.Friend{{PlexID: 9, Username: "friend", Token: "friend-token", Sections: []string{"1"}}}); err != nil {
		t.Fatal(err)
	}
	friend, receipt, err := users.Accept(ctx, "friend-token")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := safety.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		u, err := users.PolicyUser(ctx, id)
		if err != nil || u == nil {
			return nil, core.ErrUnauthorized
		}
		return u, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2},
		Progress: &fakeProgress{}, ActiveViewer: active,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		FreshPlay: func(ctx context.Context, v *core.Viewer, remote bool, now time.Time) (*core.Viewer, error) {
			return policy.ConfirmPlayback(ctx, v, remote, now)
		},
		ReceiptCurrent: users.ReceiptCurrent,
		OpenSubtitle: func(id, format string) (io.ReadCloser, string, error) {
			return io.NopCloser(strings.NewReader("sidecar-" + format)), format, nil
		},
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	surface := BindSurface(b, "plex")
	api := surface.(interface {
		BindStream(context.Context, frontend.SessionAccess, StreamBinding) (int64, error)
		ResolveStream(context.Context, frontend.SessionAccess, int64) (StreamBinding, error)
		OpenSidecar(context.Context, frontend.SessionAccess, auth.Acceptance, int64) (io.ReadCloser, core.StreamInfo, error)
	})
	access := frontend.SessionAccess{Viewer: friend, Protocol: "plex"}
	info := core.StreamInfo{Type: "subtitle", Codec: "srt", Language: "en", External: true, ExternalURL: "ether-subtitle:one"}
	id, err := api.BindStream(ctx, access, StreamBinding{Key: core.MovieKey(4), Index: 3, Info: info})
	if err != nil || id == 0 || id != WireID(core.MovieKey(4), PlexStreamSlot(3, info)) {
		t.Fatalf("bind %d %v", id, err)
	}
	again, err := api.BindStream(ctx, access, StreamBinding{Key: core.MovieKey(4), Index: 3, Info: info})
	if err != nil || again != id {
		t.Fatalf("rebind %d %v", again, err)
	}
	if _, err := api.BindStream(ctx, frontend.SessionAccess{Viewer: friend, Protocol: "jellyfin"}, StreamBinding{Key: core.MovieKey(4), Info: info}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("other surface: %v", err)
	}
	got, err := api.ResolveStream(ctx, access, id)
	if err != nil || got.WireID != id || got.Info.ExternalURL != info.ExternalURL {
		t.Fatalf("resolve %+v %v", got, err)
	}
	if _, err := api.ResolveStream(ctx, access, id+9_000_000); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	reader, opened, err := api.OpenSidecar(ctx, access, receipt, id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(body) != "sidecar-srt" || opened.Codec != "srt" {
		t.Fatalf("sidecar %q %+v", body, opened)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=?`, friend.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ResolveStream(ctx, access, id); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked viewer: %v", err)
	}
	revoked, info, err := api.OpenSidecar(ctx, access, receipt, id)
	if revoked != nil || info.ExternalURL != "" || !errors.Is(err, core.ErrUnauthorized) {
		if revoked != nil {
			_ = revoked.Close()
		}
		t.Fatalf("revoked sidecar reader=%v info=%+v err=%v", revoked != nil, info, err)
	}
}

func TestPlexSidecarFileHasNoReaderOnMiss(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1},
		Progress: &fakeProgress{}, ActiveViewer: active,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	f, err := os.CreateTemp(t.TempDir(), "side-*.srt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("1\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	access := frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Protocol: "plex"}
	info := core.StreamInfo{Type: "subtitle", Codec: "srt", External: true, ExternalURL: f.Name()}
	id, err := b.BindStream(ctx, access, StreamBinding{Key: core.MovieKey(8), Index: 1, Info: info})
	if err != nil {
		t.Fatal(err)
	}
	reader, opened, err := b.OpenSidecar(ctx, access, auth.Acceptance{}, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if opened.ExternalURL != f.Name() {
		t.Fatalf("opened %+v", opened)
	}
	missing := info
	missing.ExternalURL = f.Name() + "-absent"
	miss, err := b.BindStream(ctx, access, StreamBinding{Key: core.MovieKey(8), Index: 2, Info: missing})
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err = b.OpenSidecar(ctx, access, auth.Acceptance{}, miss)
	if reader != nil || !errors.Is(err, core.ErrNotFound) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("missing file reader=%v err=%v", reader != nil, err)
	}
}
