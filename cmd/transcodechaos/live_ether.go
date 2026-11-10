package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// liveEther is a real Live TV service, transcode manager and Plex server in
// this process, listening on an ephemeral loopback port. Nothing persists:
// the database is in memory and the work directory is removed on Close.

const liveOwnerToken = "transcodechaos-live-owner"

var liveOwner = core.User{ID: 1, Username: "owner", Title: "Owner", IsOwner: true}

type liveAuth struct{}

func (liveAuth) Authenticate(_ context.Context, token string) (*core.User, error) {
	if token != liveOwnerToken {
		return nil, core.ErrUnauthorized
	}
	u := liveOwner
	return &u, nil
}
func (liveAuth) Users(context.Context) ([]core.User, error)  { return []core.User{liveOwner}, nil }
func (liveAuth) UpdateUser(context.Context, core.User) error { return nil }
func (liveAuth) Owner(context.Context) (*core.User, error) {
	u := liveOwner
	return &u, nil
}

type liveEther struct {
	manager *transcode.Manager
	db      *sql.DB
	service *livetv.Service
	plex    *plex.Server
	server  *httptest.Server
	work    string
	logFile *os.File
	// Stability is whether the preset reached Ether, and how.
	Stability string
	handled   sync.Map
}

// channelSetup is how a scenario configures its one channel.
type channelSetup struct {
	Mode      string // passthrough | relay-copy | relay-transcode | steady
	Primary   string
	Backups   []string
	Stability string // fastest | balanced | rocksolid ("" = balanced)
}

func startLiveEther(ctx context.Context, ff, fp, work string, setup channelSetup) (*liveEther, livetv.Channel, error) {
	e, chs, err := startLiveEtherWith(ctx, ff, fp, work, []channelSetup{setup}, etherOptions{})
	if err != nil {
		return nil, livetv.Channel{}, err
	}
	return e, chs[0], nil
}

// etherOptions are the WS3 harness rows' extras: a store on disk that
// survives a service restart (Reuse opens it again without importing), and a
// Steady encode budget in cores (0 = the host's).
type etherOptions struct {
	DBPath string
	Reuse  bool
	Budget float64
}

// budgetTranscoder reports a fixed continuous-output budget so a row can
// make Steady capacity scarce on any host.
type budgetTranscoder struct {
	*transcode.Manager
	cores float64
}

func (b budgetTranscoder) ContinuousBudget() (float64, bool) { return b.cores, false }

func startLiveEtherWith(ctx context.Context, ff, fp, work string, setups []channelSetup, eo etherOptions) (*liveEther, []livetv.Channel, error) {
	e := &liveEther{work: work}
	var err error
	if e.logFile, err = os.OpenFile(filepath.Join(work, "ether.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		return nil, nil, err
	}
	log := slog.New(slog.NewTextHandler(e.logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fail := func(err error) (*liveEther, []livetv.Channel, error) { e.Close(); return nil, nil, err }
	e.manager, err = transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(work, "transcode")}, log)
	if err != nil {
		return fail(err)
	}
	if eo.DBPath != "" {
		e.db, err = store.OpenPath(ctx, eo.DBPath)
	} else {
		e.db, err = store.OpenMemory(ctx)
	}
	if err != nil {
		return fail(err)
	}
	var transcoder core.Transcoder = e.manager
	if eo.Budget > 0 {
		transcoder = budgetTranscoder{e.manager, eo.Budget}
	}
	opts := livetv.Options{Transcoder: transcoder, DB: e.db, HTTPClient: &http.Client{}, Logger: log.With("component", "livetv"), HealthSweepMinutes: func() int { return -1 }}
	e.Stability = applyStability(&opts, nil, setups[0].Stability)
	e.service, err = livetv.New(opts)
	if err != nil {
		return fail(err)
	}
	channels, err := e.service.Channels(ctx)
	if err != nil {
		return fail(err)
	}
	if !eo.Reuse || len(channels) == 0 {
		var m3u strings.Builder
		m3u.WriteString("#EXTM3U\n")
		for i, setup := range setups {
			fmt.Fprintf(&m3u, "#EXTINF:-1 tvg-id=\"chaos%d\" tvg-chno=\"%d\",Chaos %d\n%s\n", i, 7+i, i, setup.Primary)
		}
		if _, err = e.service.ImportSource(ctx, livetv.Source{Name: "Chaos provider", Playlist: m3u.String()}); err != nil {
			return fail(fmt.Errorf("import: %w", err))
		}
		if channels, err = e.service.Channels(ctx); err != nil {
			return fail(err)
		}
	}
	if len(channels) != len(setups) {
		return fail(fmt.Errorf("fixture channels: %d, want %d", len(channels), len(setups)))
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Number < channels[j].Number })
	if eo.Reuse {
		return e.servePlex(log, channels)
	}
	for i, setup := range setups {
		ch := channels[i]
		switch setup.Mode {
		case "passthrough", "relay-copy":
			// IPTV defaults to Steady; a channel without a no-signal screen plays direct.
			ch.Screens = &livetv.ChannelScreens{NoSignal: "error"}
		case "relay-transcode":
			ch.Transcode = true
			ch.Screens = &livetv.ChannelScreens{NoSignal: "error"}
		case "steady":
			ch.Transcode = true
			ch.Screens = &livetv.ChannelScreens{NoSignal: "card"}
		default:
			return fail(fmt.Errorf("unknown mode %q", setup.Mode))
		}
		if len(setup.Backups) > 0 {
			sources := append([]livetv.StreamSource(nil), ch.StreamSources...)
			if len(sources) == 0 {
				sources = []livetv.StreamSource{{ID: "primary", Name: "Primary", URL: setup.Primary, Enabled: true, Priority: 0}}
			}
			for i, u := range setup.Backups {
				sources = append(sources, livetv.StreamSource{ID: fmt.Sprintf("backup-%d", i+1), Name: fmt.Sprintf("Backup %d", i+1), URL: u, Enabled: true, Priority: i + 1})
			}
			ch.StreamSources = sources
		}
		if e.Stability == "" || strings.HasPrefix(e.Stability, "channel:") {
			e.Stability = applyStability(nil, &ch, setup.Stability)
		}
		if channels[i], err = e.service.SaveChannel(ctx, ch); err != nil {
			return fail(fmt.Errorf("save channel: %w", err))
		}
	}
	return e.servePlex(log, channels)
}

func (e *liveEther) servePlex(log *slog.Logger, channels []livetv.Channel) (*liveEther, []livetv.Channel, error) {
	cfg := config.Config{}
	cfg.Server.Port = 32400
	cfg.Server.FriendlyName = "Ether chaos"
	cfg.Server.MachineIdentifier = "c4a05c4a05c4a05c4a05c4a05c4a05c4a05c4a05"
	cfg.Plex.Claimed = true
	cfg.Plex.OwnerUsername = "owner"
	e.plex = plex.NewServer(plex.Deps{
		SessionStores: sessions.NewSessionStores(), Experience: experience.NewSelectors(),
		Auth: liveAuth{}, Transcoder: e.manager, ApplyClientRules: resolver.ApplyClientRulesForMedia,
		Config: func() config.Config { return cfg }, Logger: log.With("component", "plex"),
		Identity: plex.Identity{MachineIdentifier: cfg.Server.MachineIdentifier, FriendlyName: cfg.Server.FriendlyName, Version: "1.43.4.10903-e5521bd8c", Platform: "MacOSX", PlatformVersion: "26", MyPlexSubscription: true},
	})
	if _, err := (plex.Frontend{Server: e.plex}).Mount(context.Background(), frontend.Host{LiveTV: e.service.Frontend()}); err != nil {
		return nil, nil, err
	}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Chaos-Judge")
		began := time.Now()
		e.plex.ServeHTTP(w, r)
		if id != "" {
			e.handled.Store(id, time.Since(began).Seconds())
		}
	}))
	return e, channels, nil
}

// handledSeconds is how long Ether spent serving a judge request, so the
// judge can tell its own blocked time from a slow response.
func (e *liveEther) handledSeconds(id string) (float64, bool) {
	v, ok := e.handled.LoadAndDelete(id)
	if !ok {
		return 0, false
	}
	return v.(float64), true
}

// applyStability sets the stability preset through whichever hook the
// checked-out Ether has (worker D's config.LiveTV.Stability ->
// livetv.Options.Stability and livetv.Channel.Stability). It uses reflection
// so the harness also builds against the v0.3.5 baseline, which has none.
// The result describes what was applied ("" when nothing was).
func applyStability(opts *livetv.Options, ch *livetv.Channel, preset string) string {
	if preset == "" {
		return ""
	}
	if opts != nil {
		f := reflect.ValueOf(opts).Elem().FieldByName("Stability")
		if f.IsValid() && f.Kind() == reflect.Func && f.Type().NumIn() == 0 && f.Type().NumOut() == 1 && f.Type().Out(0).Kind() == reflect.String {
			value := preset
			f.Set(reflect.MakeFunc(f.Type(), func([]reflect.Value) []reflect.Value {
				return []reflect.Value{reflect.ValueOf(value).Convert(f.Type().Out(0))}
			}))
			return "server:" + preset
		}
	}
	if ch != nil {
		f := reflect.ValueOf(ch).Elem().FieldByName("Stability")
		if f.IsValid() && f.Kind() == reflect.String {
			f.SetString(preset)
			return "channel:" + preset
		}
	}
	return ""
}

func (e *liveEther) Close() {
	if e.server != nil {
		e.server.CloseClientConnections()
		e.server.Close()
	}
	if e.plex != nil {
		e.plex.Close()
	}
	if e.service != nil {
		_ = e.service.Close()
	}
	if e.manager != nil {
		_ = e.manager.Close()
	}
	if e.db != nil {
		_ = e.db.Close()
	}
	if e.logFile != nil {
		_ = e.logFile.Close()
	}
}

// Client identities whose headers the official apps send. Each must match
// the registry quirk plex-native-live-tune so the native tune path runs.
var liveClients = map[string]map[string]string{
	"plex-appletv": {"X-Plex-Product": "Plex for Apple TV", "X-Plex-Platform": "tvOS", "X-Plex-Platform-Version": "26.0", "X-Plex-Device": "Apple TV", "X-Plex-Model": "AppleTV14,1", "X-Plex-Version": "8.45", "X-Plex-Client-Identifier": "chaos-appletv"},
	"plex-firetv":  {"X-Plex-Product": "Plex for Android (TV)", "X-Plex-Platform": "Android", "X-Plex-Platform-Version": "11", "X-Plex-Device": "Fire TV Stick 4K Max", "X-Plex-Model": "AFTKA", "X-Plex-Version": "10.30.0", "X-Plex-Client-Identifier": "chaos-firetv"},
}

// liveStuckFactor is how many target durations without a new segment each
// client's player tolerates before it declares the live playlist stuck:
// AVFoundation 1.5 (CoreMedia -12888), Media3/ExoPlayer 3.5.
var liveStuckFactor = map[string]float64{"plex-appletv": 1.5, "plex-firetv": 3.5}

// tunePlexAs tunes as a distinct device of the same client type, so two
// concurrent viewers are two players, not one player switching channels.
func (e *liveEther) tunePlexAs(ctx context.Context, client, clientID, channelID string) (string, string, error) {
	hdr := map[string]string{}
	for k, v := range liveClients[client] {
		hdr[k] = v
	}
	if clientID != "" {
		hdr["X-Plex-Client-Identifier"] = clientID
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.server.URL+"/livetv/dvrs/1/channels/"+url.PathEscape(channelID)+"/tune", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", liveOwnerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("tune HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var doc struct {
		MediaContainer struct {
			MediaSubscription []struct {
				MediaGrabOperation []struct {
					ID       string `json:"id"`
					Metadata struct {
						Media []struct {
							Part []struct {
								Key string `json:"key"`
							} `json:"Part"`
						} `json:"Media"`
					} `json:"Metadata"`
				} `json:"MediaGrabOperation"`
			} `json:"MediaSubscription"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("tune response: %w", err)
	}
	subs := doc.MediaContainer.MediaSubscription
	if len(subs) == 0 || len(subs[0].MediaGrabOperation) == 0 || len(subs[0].MediaGrabOperation[0].Metadata.Media) == 0 || len(subs[0].MediaGrabOperation[0].Metadata.Media[0].Part) == 0 {
		return "", "", fmt.Errorf("native tune response has no media part: %.300s", body)
	}
	grab := subs[0].MediaGrabOperation[0]
	key := grab.Metadata.Media[0].Part[0].Key
	q := url.Values{"X-Plex-Token": {liveOwnerToken}}
	for k, v := range hdr {
		q.Set(k, v)
	}
	return e.server.URL + key + "?" + q.Encode(), grab.ID, nil
}

// stopSession ends the viewer session a client playlist belongs to, as a
// client does when the viewer leaves the channel.
func (e *liveEther) stopSession(ctx context.Context, playlist string) error {
	u, err := url.Parse(playlist)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/livetv/sessions/"), "/")
	if len(parts) == 0 || parts[0] == "" || !strings.HasPrefix(u.Path, "/livetv/sessions/") {
		return fmt.Errorf("no session in %s", u.Path)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, e.server.URL+"/livetv/sessions/"+parts[0], nil)
	req.Header.Set("X-Plex-Token", liveOwnerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("stop HTTP %d", resp.StatusCode)
	}
	return nil
}

// logMark is the current size of the Ether log; pathLines(mark) reads the
// path decisions logged after it.
func (e *liveEther) logMark() int64 {
	st, err := os.Stat(filepath.Join(e.work, "ether.log"))
	if err != nil {
		return 0
	}
	return st.Size()
}

// pathLine is one "live playback path selected" decision as key=value pairs.
type pathLine map[string]string

var logKV = regexp.MustCompile(`([A-Za-z_]+)=("(?:[^"\\]|\\.)*"|\S*)`)

func (e *liveEther) pathLines(mark int64) []pathLine {
	b, err := os.ReadFile(filepath.Join(e.work, "ether.log"))
	if err != nil || int64(len(b)) < mark {
		return nil
	}
	var out []pathLine
	for _, line := range strings.Split(string(b[mark:]), "\n") {
		if !strings.Contains(line, "playback path selected") {
			continue
		}
		kv := pathLine{}
		for _, m := range logKV.FindAllStringSubmatch(line, -1) {
			kv[m[1]] = strings.Trim(m[2], `"`)
		}
		out = append(out, kv)
	}
	return out
}

// activeSessionPath is the live playback path Ether chose, from its log.
func (e *liveEther) selectedPath() string {
	b, err := os.ReadFile(filepath.Join(e.work, "ether.log"))
	if err != nil {
		return ""
	}
	path := ""
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, " path="); i >= 0 && strings.Contains(line, "playback path selected") {
			path = strings.Fields(line[i+6:])[0]
		}
	}
	return path
}
