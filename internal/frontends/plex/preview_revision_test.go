package plex

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type previewFileSource struct {
	*os.File
	release string
}

func (s *previewFileSource) Size() int64 {
	st, err := s.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}
func (s *previewFileSource) ReleaseID() string   { return s.release }
func (s *previewFileSource) LocalFile() *os.File { return s.File }

type revisionPreviews struct {
	*fakeTrickplay
	frames map[string][]byte
}

func (s *revisionPreviews) TrickplayAvailable(id string) bool { return len(s.frames[id]) > 0 }
func (s *revisionPreviews) TrickplayFrame(id string, _ time.Duration) ([]byte, bool) {
	v, ok := s.frames[id]
	return v, ok
}

func TestPreviewHTTPFollowsSourceMemberAndRevision(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	provider := &revisionPreviews{fakeTrickplay: &fakeTrickplay{}, frames: map[string][]byte{}}
	f.srv.setTrickplay(provider)
	addIndexSession(f, fightClub.Key)
	dir := t.TempDir()
	open := func(name string) *previewFileSource {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("first bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		// Open through os.Root like the production local-source path: on
		// Windows that shares delete access, so the removal below models a
		// real library deletion instead of failing on the test's own handle.
		file, err := os.OpenInRoot(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		return &previewFileSource{file, "release-1"}
	}
	ps := f.playback.sessions["index-session"]
	ps.Source = open("member-a.mkv")
	oldKey := core.TrickplayCacheKey(ps)
	provider.frames[oldKey] = []byte("old-preview")
	path := "/library/parts/" + fightClub.Key.String() + "/indexes/sd/0"
	if r := f.get(t, path); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), provider.frames[oldKey]) {
		t.Fatalf("first preview: %d %s", r.Code, r.Body.String())
	}
	ps.Source = open("member-b.mkv")
	nextKey := core.TrickplayCacheKey(ps)
	if nextKey == oldKey {
		t.Fatal("different member shared preview identity")
	}
	if r := f.get(t, path); r.Code != http.StatusNotFound {
		t.Fatalf("stale member served: %d %s", r.Code, r.Body.String())
	}
	provider.frames[nextKey] = []byte("new-preview")
	if r := f.get(t, path); r.Code != 200 || r.Body.String() != "new-preview" {
		t.Fatalf("new member: %d %s", r.Code, r.Body.String())
	}
	if err := os.WriteFile(ps.Source.Name(), []byte("changed source with a different size"), 0600); err != nil {
		t.Fatal(err)
	}
	if core.TrickplayCacheKey(ps) == nextKey {
		t.Fatal("file mutation kept old identity")
	}
	if r := f.get(t, path); r.Code != 404 {
		t.Fatalf("stale revision served: %d", r.Code)
	}
	if err := os.Remove(ps.Source.Name()); err != nil {
		t.Fatal(err)
	}
	if r := f.get(t, path); r.Code != 404 {
		t.Fatalf("deleted version served: %d", r.Code)
	}
}
