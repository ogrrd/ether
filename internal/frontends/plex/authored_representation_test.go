package plex

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// This runner never executes a program. Completed empty runs let the real
// Manager render its estimated VOD manifest; this proves URL authorship, not
// encoder bytes, decoding, transport TLS, or loss recovery.
type authoredRunner struct{}

func (authoredRunner) Run(_ context.Context, c transcode.Command) error {
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, "ffmpeg version fixture\n ------\n V..... libx264 fixture\n A..... aac fixture\n")
	}
	return nil
}
func (authoredRunner) Start(transcode.Command) (transcode.Process, error) {
	done := make(chan struct{})
	close(done)
	return authoredProcess{done}, nil
}

type authoredProcess struct{ done <-chan struct{} }

func (p authoredProcess) Done() <-chan struct{} { return p.done }
func (authoredProcess) Err() error              { return nil }
func (authoredProcess) Stop()                   {}
func (authoredProcess) Pause() error            { return nil }
func (authoredProcess) Resume() error           { return nil }

func authoredManager(t *testing.T) *transcode.Manager {
	t.Helper()
	dir := t.TempDir()
	tool := filepath.Join(dir, "fixture-tool")
	if err := os.WriteFile(tool, []byte("fixture, never executed"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := transcode.New(config.Transcode{FFmpegPath: tool, FFprobePath: tool, TempDir: filepath.Join(dir, "sessions")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithRunner(authoredRunner{}), transcode.WithoutHWDetection())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestAuthoredPlexRepresentationOwnerContract(t *testing.T) {
	m := authoredManager(t)
	const publicID = "authored-public"
	remote := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	owner := newFixture(func(d *Deps) { d.Encoder = remote })
	defer owner.srv.Close()
	owner.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	startPath := "/video/:/transcode/universal/start.m3u8?path=/library/metadata/1000000000000550&session=" + publicID
	owner.get(t, decisionPath(startPath))
	master := owner.get(t, startPath)
	if master.Code != 200 {
		t.Fatal(master.Code)
	}
	delegate := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}, forwardHandler: owner.srv, forwardID: publicID}
	sender := newFixture(func(d *Deps) { d.Encoder = delegate })
	defer sender.srv.Close()
	entry, _ := owner.srv.transcodes.get(publicID)
	resolve := func(base, child string) string {
		b, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		c, err := url.Parse(child)
		if err != nil {
			t.Fatal(err)
		}
		return b.ResolveReference(c).String()
	}
	var media []string
	var index string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			index = resolve(startPath, line)
			media = append(media, index)
		}
	}
	if index == "" {
		t.Fatal("real Plex master authored no child")
	}
	for _, container := range []string{transcode.ContainerMPEGTS, transcode.ContainerFMP4, transcode.ContainerMatroska} {
		hs, err := m.StartHLS(context.Background(), core.HLSRequest{SessionID: "private-" + container, ManifestSessionID: publicID, InputURL: "fixture-input", Info: core.MediaInfo{Duration: 12 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, AudioStream: 0, Container: container}})
		if err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		owner.srv.serveStreamManifest(out, httptest.NewRequest("GET", index, nil), publicID, hs.ServePlaylist)
		if out.Code != 200 {
			t.Fatal(out.Code)
		}
		for _, match := range manifestURI.FindAllStringSubmatch(out.Body.String(), -1) {
			media = append(media, resolve(index, match[2]))
		}
		for _, line := range strings.Split(out.Body.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				media = append(media, resolve(index, line))
			}
		}
	}
	// The encoder registry key differs deliberately from the public broker ID.
	hs, err := m.StartHLS(context.Background(), core.HLSRequest{SessionID: "private-dash-execution", ManifestSessionID: publicID, InputURL: "fixture-input", Protocol: core.ProtocolDASH, Info: core.MediaInfo{Duration: 12 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}, Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, AudioStream: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Session(publicID); ok {
		t.Fatal("public identity became encoder registry key")
	}
	if _, ok := m.Session("private-dash-execution"); !ok {
		t.Fatal("private encoder registration missing")
	}
	mpd := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	owner.srv.serveStreamManifest(mpd, httptest.NewRequest("GET", "/video/:/transcode/universal/start.mpd", nil).WithContext(ctx), publicID, hs.(core.DASHSession).ServeManifest)
	if mpd.Code != 200 || strings.Contains(mpd.Body.String(), "private-dash-execution") {
		t.Fatal("MPD leaked private identity", mpd.Code)
	}
	var manifest struct {
		Period struct {
			Sets []struct {
				Template struct {
					Init  string `xml:"initialization,attr"`
					Media string `xml:"media,attr"`
				} `xml:"SegmentTemplate"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal(mpd.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Period.Sets) == 0 {
		t.Fatal("real MPD authored no representations")
	}
	for _, set := range manifest.Period.Sets {
		for _, target := range []string{set.Template.Init, set.Template.Media} {
			target = strings.NewReplacer("$RepresentationID$", "0", "$Number$", "0").Replace(target)
			if !strings.Contains(target, "session/"+publicID+"/0/") {
				t.Fatalf("wrong public MPD identity: %s", target)
			}
			media = append(media, resolve("/video/:/transcode/universal/start.mpd", target))
		}
	}
	seenFiles := map[string]bool{}
	for _, target := range media {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		seenFiles[filepath.Base(u.Path)] = true
		t.Run(strings.Split(target, "?")[0], func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				req := httptest.NewRequest(method, target, nil)
				if id, ok := strictPlexMediaID(req); !ok || id != publicID {
					t.Fatal("writer URL rejected", target)
				}
				out := httptest.NewRecorder()
				sender.srv.ServeHTTP(out, req)
				if out.Code != 200 {
					t.Fatal("authored grant URL rejected", out.Code, target)
				}
			}
			// Full account credentials retain priority over a valid stream grant.
			req := httptest.NewRequest("GET", target, nil)
			req.Header.Set("X-Plex-Token", friendToken)
			out := httptest.NewRecorder()
			sender.srv.ServeHTTP(out, req)
			if out.Code != 403 {
				t.Fatal("grant bypassed different account owner", out.Code)
			}
			owner.srv.d.AccessPolicy = &forwardedPolicy{denyTitle: true}
			out = httptest.NewRecorder()
			sender.srv.ServeHTTP(out, httptest.NewRequest("GET", target, nil))
			owner.srv.d.AccessPolicy = nil
			if out.Code != 403 {
				t.Fatal("current title denial bypassed", out.Code)
			}
		})
	}
	for _, name := range []string{"index.m3u8", "init.mp4", "header", "00000.ts", "00000.m4s", "00000.mkv", "0.m4s"} {
		if !seenFiles[name] {
			t.Fatalf("writer coverage missing %s", name)
		}
	}
	for _, path := range []string{"/video/:/transcode/universal/start.mpd", "/video/:/transcode/universal/subtitles", "/subtitles/:/transcode/universal/start", "/video/:/transcode/universal/session/" + publicID + "/base/manifest.mpd", "/video/:/transcode/universal/session/" + publicID + "/base/subtitle-0.ass"} {
		target := streamGrantURL(path+"?session="+publicID, entry.streamToken)
		req := httptest.NewRequest("GET", target, nil)
		if _, ok := strictPlexMediaID(req); ok {
			t.Fatal("control/subtitle or disk filename became representation", path)
		}
		before := delegate.grantForwards
		out := httptest.NewRecorder()
		sender.srv.ServeHTTP(out, req)
		if out.Code != 401 || delegate.grantForwards != before {
			t.Fatal("nonrepresentation delegated", path, out.Code)
		}
	}
	if _, ok := sender.srv.transcodes.get(publicID); ok {
		t.Fatal("sender fabricated session")
	}
}
