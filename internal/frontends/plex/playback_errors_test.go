package plex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// hideFromDiscovery makes the availability-discovery filter drop key, as it
// does for a recent title recorded unavailable.
func hideFromDiscovery(f *fixture, key core.RatingKey) {
	f.srv.d.FilterAvailable = func(_ context.Context, ts []core.Title) []core.Title {
		var out []core.Title
		for _, v := range ts {
			if v.Key != key {
				out = append(out, v)
			}
		}
		return out
	}
}

// Discovery evidence hides a recent title from browse and search, but a
// viewer who presses Play still gets an attempt: the friendly decision, a
// play queue, and a part request that resolves (or fails with 404).
func TestDiscoveryHiddenTitleStaysPlayable(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	hideFromDiscovery(f, fightClub.Key)
	f.playback.failures[fightClub.Key] = core.ErrNotAvailable

	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&session=d1", jsonAccept...)
	if rec.Code != http.StatusOK {
		t.Fatalf("decision on a discovery-hidden title: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	if mc["generalDecisionCode"].(float64) != codeGeneralError || mc["generalDecisionText"] != msgNotAvailable {
		t.Fatalf("decision: %v", mc)
	}

	uri := urlEsc("server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + fightClub.Key.String())
	if rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+uri, map[string]string{"Accept": "application/json"}); rec.Code != http.StatusOK {
		t.Fatalf("play queue for a discovery-hidden title: %d", rec.Code)
	}

	if rec := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv"); rec.Code != http.StatusNotFound {
		t.Fatalf("unavailable part of a discovery-hidden title: %d", rec.Code)
	}
	// Lists still hide it exactly as before (until the viewer has it open).
	for _, path := range []string{"/hubs/search?query=Fjord", "/library/sections/1/all"} {
		r := f.get(t, path)
		if r.Code != http.StatusOK || strings.Contains(r.Body.String(), `title="Fjord Harbor"`) {
			t.Fatalf("%s leaked the hidden title: %d", path, r.Code)
		}
	}
	delete(f.playback.failures, fightClub.Key)
	f.srv.transcodes.remove("d1")
	if rec := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", "Range", "bytes=0-9"); rec.Code != http.StatusPartialContent {
		t.Fatalf("part of a discovery-hidden title that resolves: %d", rec.Code)
	}

}

// Content policy still blocks playback outright.
func TestPlayAttemptStillEnforcesPolicy(t *testing.T) {
	f := newFixture(func(d *Deps) { setDepsPolicy(d, keywordPolicy{blocked: "blocked"}) })
	defer f.srv.Close()
	blocked := fightClub
	blocked.Keywords = []string{"blocked"}
	f.catalog.titles[blocked.Key] = blocked
	if rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550", "X-Plex-Token", friendToken); rec.Code != http.StatusNotFound {
		t.Fatalf("blocked decision: %d", rec.Code)
	}
	if rec := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", "X-Plex-Token", friendToken); rec.Code != http.StatusNotFound {
		t.Fatalf("blocked part: %d", rec.Code)
	}
}

// The start request that follows a failed decision for the same session
// gets that failure at once; it covers retries and expires.
func TestFailedDecisionAnswersFollowingStart(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	// Background work (artwork) reads the clock too: advance it atomically.
	var clock atomic.Int64
	clock.Store(fixedNow.UnixNano())
	f.srv.setTestNow(func() time.Time { return time.Unix(0, clock.Load()) })
	const decision = "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=s1"
	const start = "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=s1&protocol=hls"

	f.playback.failures[fightClub.Key] = core.ErrNotAvailable
	f.get(t, decision)
	// Were start to resolve again it would now succeed: the 404 proves it
	// answered from the remembered decision.
	delete(f.playback.failures, fightClub.Key)
	rec := f.get(t, start)
	if rec.Code != http.StatusNotFound || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("start after a failed decision: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if f.playback.prepared != 0 {
		t.Fatalf("start resolved again: prepared=%d", f.playback.prepared)
	}
	// A media loader retry still gets the terminal failure.
	if rec := f.get(t, start); rec.Code != http.StatusNotFound {
		t.Fatalf("second start: %d %s", rec.Code, rec.Body.String())
	}

	// Expired: a start long after the failed decision resolves afresh.
	f.playback.failures[fightClub.Key] = core.ErrNotAvailable
	f.get(t, strings.Replace(decision, "s1", "s2", 1))
	delete(f.playback.failures, fightClub.Key)
	clock.Add(int64(decisionFailureTTL + time.Second))
	if rec := f.get(t, strings.Replace(start, "s1", "s2", 1)); rec.Code != http.StatusOK {
		t.Fatalf("start after the remembered failure expired: %d", rec.Code)
	}
}

func TestRememberedFailureIsPerViewerAndTitle(t *testing.T) {
	tt := newTranscodeTable()
	now := fixedNow
	if !tt.claim("s", 1, "c", now) {
		t.Fatal("claim")
	}
	tt.fail("s", 2, "c", fightClub.Key, core.ErrNotAvailable, now) // another viewer: ignored
	if _, ok := tt.takeFailure("s", 1, fightClub.Key, now); ok {
		t.Fatal("another viewer's failure recorded on this binding")
	}
	tt.fail("s", 1, "c", fightClub.Key, core.ErrNotAvailable, now)
	if _, ok := tt.takeFailure("s", 1, alien.Key, now); ok {
		t.Fatal("failure answered a different title")
	}
	tt.fail("s", 1, "c", fightClub.Key, core.ErrNotAvailable, now)
	if err, ok := tt.takeFailure("s", 1, fightClub.Key, now); !ok || !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("take: %v %v", err, ok)
	}
	// A successful decision replaces the binding and clears the failure.
	tt.fail("s", 1, "c", fightClub.Key, core.ErrNotAvailable, now)
	tt.put(&transcodeEntry{id: "s", userID: 1, clientID: "c", key: fightClub.Key, updatedAt: now})
	if _, ok := tt.takeFailure("s", 1, fightClub.Key, now); ok {
		t.Fatal("successful decision kept the failure")
	}
}

func TestPlaybackMessageNamesTheCause(t *testing.T) {
	notCached := func(name string) error { return fmt.Errorf("%q: %w", name, core.ErrNotCached) }
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no indexers", fmt.Errorf("resolve: %w", core.ErrNoSources), msgNoSources},
		{"debrid without torrent indexer", core.ErrNoTorrentSources, msgNoTorrentSources},
		{"debrid key rejected through failover",
			fmt.Errorf("%w: 3 releases failed (last: real-debrid: bad_token): %w", core.ErrNotAvailable, core.ErrDebridAuth), msgDebridAuth},
		{"debrid key rejected in validation", errors.Join(fmt.Errorf("Real-Debrid: %w", core.ErrDebridAuth)), msgDebridAuth},
		{"every candidate uncached", errors.Join(errors.Join(notCached("a")), notCached("b")), msgNotCached},
		{"uncached after failover", fmt.Errorf("%w: 2 releases failed (last: x)", core.ErrNotCached), msgNotCached},
		{"uncached and a timeout", errors.Join(notCached("a"), errors.New("timeout")), msgNotAvailable},
		{"uncached among not-available", errors.Join(notCached("a"), fmt.Errorf("x: %w", core.ErrNotAvailable)), msgNotAvailable},
		{"plain not available", fmt.Errorf("%q: %w", "x", core.ErrNotAvailable), msgNotAvailable},
		{"sent to download", errors.Join(notCached("a"), fmt.Errorf("%q: %w", "b", core.ErrDownloadQueued)), msgDownloadQueued},
		{"stream limit", core.ErrStreamLimit, msgStreamLimit},
		{"cached-only", &core.UncachedError{Err: notCached("a"), CachedOnly: true}, msgNotCachedOnly},
		{"cached-only, Usenet found nothing", &core.UncachedError{Err: notCached("a"), CachedOnly: true, Usenet: core.UsenetNone}, msgNotCachedOnlyNoNZB},
		{"cached-only, Usenet releases broken", fmt.Errorf("%w (last failure: x)", &core.UncachedError{Err: notCached("a"), CachedOnly: true, Usenet: core.UsenetFailed}), msgNotCachedOnlyBadNZB},
		{"warm-up with cached-only off", &core.UncachedError{Err: notCached("a"), Usenet: core.UsenetNone}, msgNotCachedNoNZB},
		{"download limit", &core.UncachedError{Err: fmt.Errorf("%q: %w", "a", core.ErrDownloadLimit)}, msgDownloadLimit},
		{"download refused after a transient add failure", &core.UncachedError{Err: fmt.Errorf("%w: %w", core.ErrDownloadRefused, errors.New("HTTP 503"))}, msgDownloadRefused},
	}
	for _, c := range cases {
		if got := playbackMessage(c.err); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecisionExplainsMissingSources(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.failures[fightClub.Key] = fmt.Errorf("resolve: %w", core.ErrNoTorrentSources)
	mc := decodeJSON(t, f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550", jsonAccept...))
	if mc["generalDecisionText"] != msgNoTorrentSources || mc["transcodeDecisionText"] != msgNoTorrentSources {
		t.Fatalf("decision: %v", mc)
	}
}

// API consumers may retry a transient lookup; a player start must end this
// attempt, with the specific text available through the timeline.
func TestTransientPlaybackErrorMapping(t *testing.T) {
	s := &Server{log: slog.Default()}
	for _, err := range []error{core.ErrSourceUnavailable, context.DeadlineExceeded, core.ErrPrepareSlow} {
		w := httptest.NewRecorder()
		s.writeErr(w, httptest.NewRequest("GET", "/", nil), err)
		if w.Code != 503 || w.Header().Get("Retry-After") != notAvailableRetryAfter {
			t.Fatalf("lookup %v: %d %v", err, w.Code, w.Header())
		}
		w = httptest.NewRecorder()
		s.writeStartErr(w, httptest.NewRequest("GET", "/", nil), nil, err)
		if w.Code != 400 || w.Header().Get("Retry-After") != "" {
			t.Fatalf("start %v: %d %v", err, w.Code, w.Header())
		}
	}
}

// failureBudgetPlayback exercises the production bounded-prepare interface.
// A failed bounded prepare must never fall through to unbounded Prepare.
type failureBudgetPlayback struct {
	*fakePlayback
	err   error
	calls int
}

func (p *failureBudgetPlayback) PrepareWithin(_ context.Context, budget time.Duration, _ *core.User, _ core.RatingKey, _ core.ClientCaps, _ string, _ bool) (*core.PlaySession, error) {
	if budget <= 0 || budget > decisionSourceWait {
		panic("unbounded client acquisition")
	}
	p.calls++
	return nil, p.err
}

// TestPlaybackFailureClientContract protects the cross-client invariant:
// failures have terminal decisions, media requests end without retry advice,
// and buffering timelines carry the same specific reason. These are HTTP
// contracts; they do not assert how proprietary apps render the dialog.
func TestPlaybackFailureClientContract(t *testing.T) {
	clients := []struct{ name, product, platform, format string }{
		{"firetv", "Plex for Android (TV)", "Android", "xml"},
		{"androidtv", "Plex for Android (TV)", "Android", "xml"},
		{"shield", "Plex for Android (TV)", "Android", "xml"},
		{"appletv", "Plex for Apple TV", "tvOS", "xml"},
		{"ios", "Plex for iOS", "iOS", "json"},
		{"web", "Plex Web", "Chrome", "json"},
		{"lg", "Plex for LG", "webOS", "json"},
		{"samsung", "Plex for Samsung", "Tizen", "json"},
		{"roku", "Plex for Roku", "Roku", "xml"},
	}
	failures := []struct {
		cause       error
		partGateway bool
	}{
		{core.ErrNotAvailable, false}, {core.ErrNotCached, false},
		{&core.UncachedError{Err: core.ErrNotCached, CachedOnly: true}, false},
		{core.ErrDownloadRefused, false}, {core.ErrDownloadQueued, false}, {core.ErrDownloadLimit, false},
		{core.ErrDebridAuth, true}, {core.ErrSourceUnavailable, true}, {core.ErrReleaseRejected, true},
		{context.DeadlineExceeded, true}, {core.ErrPrepareSlow, false}, {core.ErrStreamLimit, false},
		{core.ErrNoSources, false}, {errors.New("encoder failed"), true},
	}

	for _, client := range clients {
		t.Run(client.name, func(t *testing.T) {
			for i, tc := range failures {
				failure := tc.cause
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					f := newFixture()
					defer f.srv.Close()
					p := &failureBudgetPlayback{fakePlayback: f.playback, err: failure}
					f.srv.d.Playback = p
					h := map[string]string{"X-Plex-Product": client.product, "X-Plex-Platform": client.platform,
						"X-Plex-Client-Identifier": client.name, "X-Plex-Session-Identifier": "attempt",
						"Accept": "application/" + client.format}
					base := "?path=/library/metadata/1000000000000550&session=attempt&hasMDE=1&directPlay=1&mediaIndex=0&partIndex=0"
					started := time.Now()
					decision := f.request(t, "GET", "/video/:/transcode/universal/decision"+base, h)
					attrs := containerAttrs(t, decision.Body.Bytes())
					want := playbackMessage(failure)
					if decision.Code != 200 || attrs["generalDecisionCode"] != "2000" || attrs["directPlayDecisionCode"] != "3000" || attrs["transcodeDecisionCode"] != "4000" || attrs["mdeDecisionCode"] != "2000" {
						t.Fatalf("nonterminal decision: %d %v", decision.Code, attrs)
					}
					for _, field := range []string{"generalDecisionText", "directPlayDecisionText", "transcodeDecisionText", "mdeDecisionText"} {
						if attrs[field] != want {
							t.Fatalf("%s = %q want %q", field, attrs[field], want)
						}
					}
					checkTimeline := func() {
						t.Helper()
						r := f.request(t, "GET", "/:/timeline?ratingKey=1000000000000550&state=buffering&time=0", h)
						a := containerAttrs(t, r.Body.Bytes())
						if a["terminationCode"] != "2006" || a["terminationText"] != want {
							t.Fatalf("timeline: %v", a)
						}
					}
					checkTimeline() // even if the client never follows decision with start
					for _, path := range []string{"/video/:/transcode/universal/start.m3u8" + base, "/video/:/transcode/universal/start.mpd" + base, "/library/parts/1000000000000550/1/file.mkv"} {
						for range 2 { // media loader retry must not consume the remembered error
							r := f.request(t, "GET", path, h)
							badStatus := r.Code < 400 || r.Code >= 500
							if tc.partGateway && strings.HasPrefix(path, "/library/parts/") {
								badStatus = r.Code != http.StatusBadGateway
							}
							if badStatus || r.Header().Get("Retry-After") != "" || r.Body.Len() != 0 {
								t.Fatalf("%s: %d %v %s", path, r.Code, r.Header(), r.Body.String())
							}
							checkTimeline()
						}
					}
					if !errors.Is(failure, core.ErrPrepareSlow) && p.calls != 1 {
						t.Fatalf("failed source was retried %d times", p.calls)
					}
					// Also exercise clients that skip decision: every media route must
					// use bounded preparation rather than plain Prepare.
					f.srv.transcodes.remove("attempt")
					for _, path := range []string{"/video/:/transcode/universal/start.m3u8" + base, "/video/:/transcode/universal/start.mpd" + base, "/library/parts/1000000000000550/1/file.mkv"} {
						before := p.calls
						r := f.request(t, "GET", path, h)
						badStatus := r.Code < 400 || r.Code >= 500
						if tc.partGateway && strings.HasPrefix(path, "/library/parts/") {
							badStatus = r.Code != http.StatusBadGateway
						}
						if badStatus || p.calls != before+1 {
							t.Fatalf("unbounded media route %s: %d, calls %d -> %d", path, r.Code, before, p.calls)
						}
						checkTimeline()
					}
					if time.Since(started) >= decisionSourceWait {
						t.Fatal("client failure exceeded decision budget")
					}
				})
			}
		})
	}
}
