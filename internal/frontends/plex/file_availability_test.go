package plex

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func (p *fakePlayback) CheckFileAvailability(_ context.Context, _ *core.User, key core.RatingKey, _ core.ClientCaps, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.failures[key]
	return err == nil, err
}

// fileProbeResolver ranks releases in order like the real resolver: Prefer
// first when it is a candidate and not blocked, else the best unblocked one,
// else ErrNotAvailable (the warm-up search found nothing yet).
type fileProbeResolver struct {
	releases []string
	blocked  map[string]bool
}

func (r fileProbeResolver) Resolve(_ context.Context, q core.ResolveRequest) (*core.Resolution, error) {
	pick := ""
	if slices.Contains(r.releases, q.Prefer) && !r.blocked[q.Prefer] {
		pick = q.Prefer
	}
	for _, id := range r.releases {
		if pick == "" && !r.blocked[id] {
			pick = id
		}
	}
	if pick == "" {
		return nil, core.ErrNotAvailable
	}
	return &core.Resolution{Title: q.Title.Key, Release: core.Release{ID: pick, Name: "Fjord.Harbor.1999.1080p.mkv"}, NZB: []byte(pick)}, nil
}
func (fileProbeResolver) MarkBad(context.Context, string, string) error          { return nil }
func (fileProbeResolver) Available(context.Context, core.RatingKey) (bool, bool) { return false, false }
func (r fileProbeResolver) ReleaseBlocked(_ context.Context, id string) (bool, error) {
	return r.blocked[id], nil
}

type fileProbeEngine struct {
	fail          map[string]error // release -> Open error
	empty         map[string]bool  // release -> opens with zero size
	opens, closes atomic.Int32
}

func (e *fileProbeEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	e.opens.Add(1)
	if err := e.fail[id]; err != nil {
		return nil, err
	}
	data := []byte("live-file")
	if e.empty[id] {
		data = nil
	}
	return &fileProbeSource{issue94Source: issue94Source{Reader: bytes.NewReader(data), id: id}, closed: &e.closes}, nil
}
func (*fileProbeEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

type fileProbeSource struct {
	issue94Source
	closed *atomic.Int32
}

func (s *fileProbeSource) Close() error { s.closed.Add(1); return nil }

func fileProbeManager(t *testing.T, f *fixture, res fileProbeResolver, engine *fileProbeEngine) *playback.Manager {
	t.Helper()
	cfg := config.Defaults(t.TempDir())
	cfg.Cache.DisableSpeculation = true
	m, err := playback.New(playback.Deps{Catalog: f.catalog, Resolver: res, Engine: engine, Transcoder: f.trans, SelectProfile: func(core.ClientCaps, *core.User, string) core.QualityProfile { return core.QualityProfile{} }}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMetadataFileChecksUseSourceWithoutPlaybackAdmission(t *testing.T) {
	f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
	engine := &fileProbeEngine{}
	m := fileProbeManager(t, f, fileProbeResolver{releases: []string{"checked"}}, engine)
	f.srv.d.Playback = m
	defer f.srv.Close()
	defer m.Close()
	for _, option := range []string{"", "checkFiles=1", "asyncCheckFiles=1", "checkFileAvailability=1"} {
		r := f.get(t, "/library/metadata/1000000000000550?"+option, jsonAccept...)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", option, r.Code, r.Body.String())
		}
		mc := decodeJSON(t, r)
		part := mc["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)
		for _, name := range []string{"exists", "accessible"} {
			v, present := part[name]
			if option == "" {
				if present {
					t.Fatalf("ordinary read has %s", name)
				}
			} else if !present || v != true {
				t.Fatalf("option=%s %s=%v", option, name, v)
			}
		}
		if option == "" && engine.opens.Load() != 0 {
			t.Fatal("ordinary read opened source")
		}
		if len(m.Sessions()) != 0 {
			t.Fatal("file check admitted playback")
		}
	}
	r := f.get(t, "/status/sessions", jsonAccept...)
	if decodeJSON(t, r)["size"] != float64(0) {
		t.Fatal("file check created Now Playing")
	}
	if engine.opens.Load() == 0 || engine.closes.Load() != engine.opens.Load() {
		t.Fatalf("probe opens=%d closes=%d", engine.opens.Load(), engine.closes.Load())
	}
}

// Plex apps hide Play on accessible="0", so a checkFiles answer marks a
// version missing only on definitive evidence about that exact version. The
// default (Auto) version stays playable whenever play time could still
// resolve the title.
func TestFileAvailabilityOnlyDefinitiveEvidenceHidesAVersion(t *testing.T) {
	gone := map[string]error{"gone": core.ErrArticleMissing, "flaky": core.ErrSourceUnavailable}
	for _, tc := range []struct {
		name     string
		resolver fileProbeResolver
		engine   *fileProbeEngine
		release  string // "" is the default (Auto) version
		want     bool
	}{
		{"auto plays", fileProbeResolver{releases: []string{"a"}}, &fileProbeEngine{}, "", true},
		{"auto warm-up found nothing", fileProbeResolver{}, &fileProbeEngine{}, "", true},
		{"auto top pick is gone", fileProbeResolver{releases: []string{"gone"}}, &fileProbeEngine{fail: gone}, "", true},
		{"auto top pick is empty", fileProbeResolver{releases: []string{"a"}}, &fileProbeEngine{empty: map[string]bool{"a": true}}, "", true},
		{"version plays", fileProbeResolver{releases: []string{"a", "b"}}, &fileProbeEngine{}, "b", true},
		{"resolver prefers another release", fileProbeResolver{releases: []string{"a"}}, &fileProbeEngine{}, "b", true},
		{"version warm-up found nothing", fileProbeResolver{}, &fileProbeEngine{}, "b", true},
		{"version source transiently down", fileProbeResolver{releases: []string{"flaky"}}, &fileProbeEngine{fail: gone}, "flaky", true},
		{"version release marked bad", fileProbeResolver{releases: []string{"a", "b"}, blocked: map[string]bool{"b": true}}, &fileProbeEngine{}, "b", false},
		{"version files permanently gone", fileProbeResolver{releases: []string{"gone"}}, &fileProbeEngine{fail: gone}, "gone", false},
		{"version opens empty", fileProbeResolver{releases: []string{"b"}}, &fileProbeEngine{empty: map[string]bool{"b": true}}, "b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(func(d *testServerDeps) { d.DisableEnrichment = true })
			m := fileProbeManager(t, f, tc.resolver, tc.engine)
			f.srv.d.Playback = m
			defer f.srv.Close()
			defer m.Close()
			id := int64(fightClub.Key)
			if tc.release != "" {
				id = f.srv.variantIDs.idFor(fightClub.Key, tc.release)
			}
			mc := &model.MediaContainer{Metadata: []model.Metadata{{Media: []model.Media{{Part: []model.Part{{ID: id}}}}}}}
			req := httptest.NewRequest(http.MethodGet, "/library/metadata/1000000000000550?checkFiles=1&includeChapters=1", nil)
			req = req.WithContext(withRequest(req.Context(), &Request{User: &ownerUser, ClientID: "atv", Product: "Plex for Apple TV"}))
			f.srv.projectFileAvailability(req, mc)
			p := mc.Metadata[0].Media[0].Part[0]
			if p.Exists == nil || p.Accessible == nil || *p.Exists != tc.want || *p.Accessible != tc.want {
				t.Fatalf("exists=%v accessible=%v, want %v", p.Exists, p.Accessible, tc.want)
			}
		})
	}
}
