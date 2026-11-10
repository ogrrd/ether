// Package spike serves a handful of local video files as a Plex library so
// the Plex front end (internal/frontends/plex) can be tested against official Plex
// apps before the Usenet engine exists. It implements core.Catalog and
// core.Playback over the files, plus in-memory State, Playlists,
// Collections, Recommender, Auth and a direct-play-only Transcoder.
package spike

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// Options configure the spike library.
type Options struct {
	// Files overrides config.Spike.LocalFiles when non-empty.
	Files []string
	// Tokens accepted as the owner, in addition to config.Plex.Token.
	Tokens []string
	// AcceptAnyToken maps every non-empty token to the owner. For LAN
	// testing with real Plex apps (whose per-device plex.tv tokens the spike
	// cannot verify). Never enable on an exposed server.
	AcceptAnyToken bool
	// FFprobePath overrides config.Transcode.FFprobePath / PATH lookup.
	FFprobePath string
	// DisableProbe skips ffprobe and estimates media info (tests).
	DisableProbe bool
	// SessionIdle closes play sessions without activity (default 10 min).
	SessionIdle time.Duration
	Logger      *slog.Logger
}

// Library is the spike bundle.
type Library struct {
	cfg  func() config.Config
	opts Options
	log  *slog.Logger

	mu     sync.RWMutex
	byKey  map[core.RatingKey]*localFile
	sorted []*localFile

	playback  *playback
	state     *memState
	playlists *memPlaylists
	auth      *auth
	trans     *transcoder

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// New builds the library: every configured file is stat'ed and probed
// (ffprobe when on PATH, otherwise estimated). Missing files are skipped
// with a warning; it is an error if none remain.
func New(ctx context.Context, cfg func() config.Config, opts Options) (*Library, error) {
	if cfg == nil {
		return nil, errors.New("spike: config getter required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SessionIdle <= 0 {
		opts.SessionIdle = 10 * time.Minute
	}
	c := cfg()
	files := opts.Files
	if len(files) == 0 {
		files = c.Spike.LocalFiles
	}
	if len(files) == 0 {
		return nil, errors.New("spike: no local files configured")
	}
	l := &Library{
		cfg:       cfg,
		opts:      opts,
		log:       opts.Logger.With("component", "spike"),
		byKey:     map[core.RatingKey]*localFile{},
		state:     newMemState(),
		playlists: newMemPlaylists(),
		stop:      make(chan struct{}),
	}
	ffprobe := opts.FFprobePath
	if ffprobe == "" {
		ffprobe = c.Transcode.FFprobePath
	}
	pr := newProber(ffprobe)
	if opts.DisableProbe {
		pr = prober{}
	}
	for _, p := range files {
		f, err := l.loadFile(ctx, pr, p)
		if err != nil {
			l.log.Warn("skipping local file", "path", p, "err", err)
			continue
		}
		if _, dup := l.byKey[f.title.Key]; dup {
			continue
		}
		l.byKey[f.title.Key] = f
		l.sorted = append(l.sorted, f)
	}
	if len(l.sorted) == 0 {
		return nil, errors.New("spike: none of the configured files could be read")
	}
	sort.Slice(l.sorted, func(i, j int) bool { return l.sorted[i].title.Title < l.sorted[j].title.Title })
	l.playback = newPlayback(l)
	l.auth = newAuth(l)
	l.trans = &transcoder{prober: pr}
	l.wg.Add(1)
	go l.janitor()
	return l, nil
}

func (l *Library) loadFile(ctx context.Context, pr prober, path string) (*localFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", abs)
	}
	info, err := pr.probe(ctx, abs)
	if err != nil {
		l.log.Info("ffprobe unavailable; estimating media info", "path", abs, "err", err)
		info = estimateInfo(abs, st.Size())
	}
	if info.Size == 0 {
		info.Size = st.Size()
	}
	title, year := ParseFileName(abs)
	t := core.Title{
		Key:       fileKey(abs),
		Type:      core.TypeMovie,
		Title:     title,
		Year:      year,
		Runtime:   info.Duration,
		Status:    "Released",
		UpdatedAt: st.ModTime().UTC().Truncate(time.Second),
	}
	if s := sortTitle(title); s != title {
		t.SortTitle = s
	}
	if year > 0 {
		t.Released = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &localFile{path: abs, title: t, info: info, mtime: st.ModTime(), size: st.Size()}, nil
}

func (l *Library) file(k core.RatingKey) (*localFile, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	f, ok := l.byKey[k]
	return f, ok
}

func (l *Library) files() []*localFile {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]*localFile(nil), l.sorted...)
}

// Catalog etc. expose the individual adapters.
func (l *Library) Catalog() core.Catalog         { return catalog{l} }
func (l *Library) Playback() core.Playback       { return l.playback }
func (l *Library) State() core.State             { return l.state }
func (l *Library) Playlists() core.Playlists     { return l.playlists }
func (l *Library) Collections() core.Collections { return noCollections{} }
func (l *Library) Recommender() core.Recommender { return recommender{l} }
func (l *Library) Auth() core.Auth               { return l.auth }
func (l *Library) Transcoder() core.Transcoder   { return l.trans }

// Deps returns a plex.Deps wired to the spike adapters, a movies-only
// section and the identity from the config.
func (l *Library) Deps() plex.Deps {
	c := l.cfg()
	return plex.Deps{
		SessionStores:    sessions.NewSessionStores(),
		Experience:       experience.NewSelectors(),
		Catalog:          l.Catalog(),
		Collections:      l.Collections(),
		Recommender:      l.Recommender(),
		State:            l.State(),
		Playlists:        l.Playlists(),
		Auth:             l.Auth(),
		Playback:         l.Playback(),
		Transcoder:       l.Transcoder(),
		Config:           l.cfg,
		ApplyClientRules: resolver.ApplyClientRulesForMedia,
		Logger:           l.opts.Logger,
		Identity: plex.Identity{
			MachineIdentifier: c.Server.MachineIdentifier,
			FriendlyName:      c.Server.FriendlyName,
		},
		Sections: []plex.Section{{ID: 1, Type: core.TypeMovie, Title: "Movies"}},
	}
}

// OpenPlex serves this library through the Plex front end and the shared
// session broker. Timeline progress and stops are recorded only by the broker.
func (l *Library) OpenPlex() (*plex.Server, func(), error) {
	srv := plex.NewServer(l.Deps())
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id <= 0 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	events := sessions.NewEventBus(nil, active)
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Lookup: func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return l.Catalog().Get(ctx, key)
		},
		Playback:     l.Playback(),
		Transcoder:   l.Transcoder(),
		Progress:     l.State(),
		ActiveViewer: active,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Events:       events,
	})
	if err != nil {
		events.Close()
		srv.Close()
		return nil, nil, err
	}
	srv.UsePlaybackBroker(broker)
	return srv, func() {
		broker.Close(context.Background())
		events.Close()
	}, nil
}

// Close stops the session janitor and closes open files.
func (l *Library) Close() error {
	l.once.Do(func() {
		close(l.stop)
		l.wg.Wait()
		for _, ps := range l.playback.Sessions() {
			l.playback.Stop(ps.ID)
		}
	})
	return nil
}

func (l *Library) janitor() {
	defer l.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			l.playback.expire(now, l.opts.SessionIdle)
		}
	}
}
