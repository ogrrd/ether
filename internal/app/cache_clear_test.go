package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerCacheClearBlock(t *testing.T) {
	a := setupTestApp(t, true)
	// Complete startup reconciliation before injecting this synthetic live pin.
	// It has no real session, so the first sweep correctly treats it as stale.
	// ReconcilePins serializes that sweep; waiting on a timer would still race.
	if err := a.playback.ReconcilePins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.users.SetOwner(context.Background(), plextv.Account{ID: 42, Username: "owner"}, "test-owner"); err != nil {
		t.Fatal(err)
	}
	if err := a.cache.Put("release", 0, []byte("cached")); err != nil {
		t.Fatal(err)
	}
	post := func(path, token, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost/ether/api/cache/"+path, strings.NewReader(`{"releaseId":"release"}`))
		r.RemoteAddr = "127.0.0.1:1000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Plex-Token", token)
		r.Header.Set("X-Ether-Request", csrf)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	call := func(token, csrf string) *httptest.ResponseRecorder { return post("clear-block", token, csrf) }
	if w := call("", "1"); w.Code != 401 && w.Code != 403 {
		t.Fatalf("anonymous %d", w.Code)
	}
	if w := call("test-owner", ""); w.Code != 403 {
		t.Fatalf("csrf %d", w.Code)
	}
	if err := a.cache.Pin("release", "playback:live"); err != nil {
		t.Fatal(err)
	}
	if w := call("test-owner", "1"); w.Code != 409 {
		t.Fatalf("live %d %s", w.Code, w.Body)
	}
	if blocked, _ := a.resolver.ReleaseBlocked(context.Background(), "release"); blocked {
		t.Fatal("active release blocked")
	}
	if err := a.cache.Unpin("release", "playback:live"); err != nil {
		t.Fatal(err)
	}
	// Clear alone drops the bytes and leaves the release playable.
	if w := post("clear", "test-owner", "1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"cleared":true`) || !strings.Contains(w.Body.String(), `"blocked":false`) {
		t.Fatalf("clear only %d %s", w.Code, w.Body)
	}
	if blocked, _ := a.resolver.ReleaseBlocked(context.Background(), "release"); blocked || a.cache.Has("release", 0) {
		t.Fatalf("clear only: blocked %v, bytes remain %v", blocked, a.cache.Has("release", 0))
	}
	if err := a.cache.Put("release", 0, []byte("cached")); err != nil {
		t.Fatal(err)
	}
	if w := call("test-owner", "1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"cleared":true`) {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}
	if blocked, err := a.resolver.ReleaseBlocked(context.Background(), "release"); err != nil || !blocked {
		t.Fatalf("block %v %v", blocked, err)
	}
	if a.cache.Has("release", 0) {
		t.Fatal("bytes remain")
	}
}

func TestCacheBlockCheckDoesNotQueryDatabase(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svc := resolver.New(nil, nil, db, config.Defaults(t.TempDir()), resolver.Options{})
	defer svc.Close()
	if _, err := svc.Block(context.Background(), resolver.BlockRequest{ReleaseID: "blocked"}, "owner"); err != nil {
		t.Fatal(err)
	}
	cache, err := segcache.Open(t.TempDir(), 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	a := &App{cache: cache, resolver: svc}
	_ = a.cacheHandler()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseBlocked(context.Background(), "ordinary"); err == nil {
		t.Fatal("database close did not affect authoritative check")
	}
	for i := 0; i < 10; i++ {
		if err := cache.Put("ordinary", i, []byte("segment")); err != nil || !cache.Has("ordinary", i) {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := cache.Put("blocked", 0, []byte("segment")); err != nil || cache.Has("blocked", 0) {
		t.Fatalf("blocked write: %v", err)
	}
}
