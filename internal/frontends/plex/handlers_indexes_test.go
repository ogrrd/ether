package plex

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/trickplay"
)

type fakeTrickplay struct {
	bif       []byte
	frame     []byte
	available bool
	ensured   int
	source    core.TrickplaySource
}

func (f *fakeTrickplay) EnsureTrickplay(_ context.Context, src core.TrickplaySource) {
	f.ensured++
	f.source = src
}
func (f *fakeTrickplay) TrickplayAvailable(string) bool { return f.available }
func (f *fakeTrickplay) TrickplayBIF(string) ([]byte, bool) {
	return append([]byte(nil), f.bif...), len(f.bif) > 0
}

func TestTimelineStartsTrickplayForConsumedPlayback(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	provider := &fakeTrickplay{}
	f.srv.setTrickplay(provider)
	addIndexSession(f, fightClub.Key)
	f.playback.sessions["index-session"].BytesServed = 1000 // consumed source, not just prepared
	rec := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&time=30000&duration=8340000&state=playing")
	if rec.Code != http.StatusOK || provider.ensured != 1 {
		t.Fatalf("timeline: status=%d Ensure calls=%d", rec.Code, provider.ensured)
	}
	if provider.source.ReleaseID != core.TrickplayCacheKey(f.playback.sessions["index-session"]) || provider.source.AvailableUntil != 30*time.Second || provider.source.InputURL == "" {
		t.Fatalf("trickplay source=%+v", provider.source)
	}
}
func (f *fakeTrickplay) TrickplayFrame(string, time.Duration) ([]byte, bool) {
	return append([]byte(nil), f.frame...), len(f.frame) > 0
}
func (f *fakeTrickplay) SetTrickplayFallback(string, []byte) {}

func addIndexSession(f *fixture, key core.RatingKey) {
	f.playback.mu.Lock()
	defer f.playback.mu.Unlock()
	f.playback.sessions["index-session"] = &core.PlaySession{
		ID: "index-session", UserID: ownerUser.ID, ClientID: "client-1", Key: key,
		Resolution: &core.Resolution{Release: core.Release{ID: "release-1"}},
		StartedAt:  fixedNow,
	}
}

func TestIndexRoutesPrecedePartRouteAndServeBIFAndFrame(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	frame := []byte{0xff, 0xd8, 0xff, 0xd9}
	bif, err := transcode.MarshalBIF([]transcode.BIFFrame{
		{Time: 0, JPEG: frame}, {Time: 10 * time.Second, JPEG: frame},
		{Time: 20 * time.Second, JPEG: frame}, {Time: 30 * time.Second, JPEG: frame},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeTrickplay{bif: bif, frame: frame}
	f.srv.setTrickplay(provider)
	addIndexSession(f, fightClub.Key)

	// This path would otherwise be consumed as /library/parts/{part}/{file}.
	rec := f.get(t, "/library/parts/"+fightClub.Key.String()+"/indexes/sd")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" || !bytes.Equal(rec.Body.Bytes(), provider.bif) {
		t.Fatalf("BIF response: status=%d content-type=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	variantPart := f.srv.variantIDs.idFor(fightClub.Key, "release-1")
	rec = f.get(t, "/library/parts/"+strconv.FormatInt(variantPart, 10)+"/indexes/sd")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), provider.bif) {
		t.Fatalf("variant BIF response: status=%d body=%q", rec.Code, rec.Body.Bytes())
	}
	rec = f.request(t, http.MethodGet, "/library/parts/"+fightClub.Key.String()+"/indexes/sd", map[string]string{"Range": "bytes=0-2"})
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), provider.bif[:3]) {
		t.Fatalf("BIF range: status=%d body=%q", rec.Code, rec.Body.Bytes())
	}
	rec = f.get(t, "/library/parts/"+fightClub.Key.String()+"/indexes/sd?interval=20000")
	downsampled, err := transcode.ParseBIF(rec.Body.Bytes())
	if rec.Code != http.StatusOK || err != nil || downsampled.Len() != 2 || downsampled.Time(1) != 20*time.Second {
		t.Fatalf("interval response: status=%d BIF=%v err=%v", rec.Code, downsampled, err)
	}
	rec = f.get(t, "/library/parts/"+fightClub.Key.String()+"/indexes/sd/10000")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), frame) {
		t.Fatalf("frame response: status=%d content-type=%q body=%v", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	if rec = f.get(t, "/library/parts/9999999999999999/indexes/sd"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown part status=%d", rec.Code)
	}
}

func TestPartIndexesAttributeOnlyWhenAvailable(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	provider := &fakeTrickplay{available: false}
	f.srv.setTrickplay(provider)
	addIndexSession(f, fightClub.Key)
	path := "/library/metadata/" + fightClub.Key.String()
	if rec := f.get(t, path); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `indexes="sd"`) {
		t.Fatalf("unexpected indexes attribute: status=%d body=%s", rec.Code, rec.Body.String())
	}
	provider.available = true
	if rec := f.get(t, path); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `indexes="sd"`) {
		t.Fatalf("missing indexes attribute: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type cancelingTrickplay struct {
	fakeTrickplay
	cancels int
}

func (p *cancelingTrickplay) CancelTrickplay(string) { p.cancels++ }
func TestPlexOnlyTrickplayIdleCancelParity(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(strconv.FormatBool(shared), func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			raw := &cancelingTrickplay{}
			var provider core.TrickplayProvider = raw
			if shared {
				owner := trickplay.New(raw)
				defer owner.Close()
				provider = owner.Borrow()
			}
			f.srv.setTrickplay(provider)
			addIndexSession(f, fightClub.Key)
			playing := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&time=30000&duration=8340000&state=playing")
			if raw.ensured != 1 {
				t.Fatal("Ensure changed")
			}
			// A second active source preserves the historical idle decision, even if it
			// has not sent a timeline and therefore has no thumbnail lease yet.
			f.playback.mu.Lock()
			f.playback.sessions["other"] = &core.PlaySession{ID: "other", Key: fightClub.Key, Resolution: &core.Resolution{Release: core.Release{ID: "release-1"}}}
			f.playback.mu.Unlock()
			stopped := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&time=30000&duration=8340000&state=stopped")
			if raw.cancels != 0 {
				t.Fatal("canceled before idle")
			}
			f.playback.Stop("other")
			f.srv.cancelTrickplayIfIdle(&core.PlaySession{ID: "other", Key: fightClub.Key, Resolution: &core.Resolution{Release: core.Release{ID: "release-1"}}})
			if raw.cancels != 1 {
				t.Fatalf("idle cancels=%d", raw.cancels)
			}
			if playing.Code != 200 || stopped.Code != 200 || !bytes.Equal(playing.Body.Bytes(), stopped.Body.Bytes()) {
				t.Fatalf("timeline bytes changed: %q %q", playing.Body.Bytes(), stopped.Body.Bytes())
			}
		})
	}
}
func TestPlexStopCannotCancelJellyfinOrPrewarmLease(t *testing.T) {
	for _, name := range []string{"jellyfin", "prewarm"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			raw := &cancelingTrickplay{}
			owner := trickplay.New(raw)
			defer owner.Close()
			p := owner.Borrow()
			f.srv.setTrickplay(p)
			addIndexSession(f, fightClub.Key)
			f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&time=30000&state=playing")
			other, err := p.(core.TrickplayLeaser).AcquireTrickplay(context.Background(), core.TrickplaySource{ReleaseID: core.TrickplayCacheKey(f.playback.sessions["index-session"]), InputURL: "prewarm"})
			if err != nil {
				t.Fatal(err)
			}
			f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=stopped")
			if raw.cancels != 0 {
				t.Fatal("Plex stop canceled another holder")
			}
			other.Release(true)
			if raw.cancels != 1 {
				t.Fatal("last release failed")
			}
		})
	}
}

func TestSharedTrickplayPlexExactWireParity(t *testing.T) {
	type response struct {
		status  int
		headers http.Header
		body    []byte
	}
	var legacy []response
	for _, shared := range []bool{false, true} {
		f := newFixture()
		raw := &cancelingTrickplay{fakeTrickplay: fakeTrickplay{bif: []byte("identical-bif"), frame: []byte("identical-jpeg"), available: true}}
		if shared {
			owner := trickplay.New(raw)
			defer owner.Close()
			f.srv.setTrickplay(owner.Borrow())
		} else {
			f.srv.setTrickplay(raw)
		}
		addIndexSession(f, fightClub.Key)
		paths := []string{
			"/:/timeline?ratingKey=" + fightClub.Key.String() + "&time=30000&state=playing",
			"/library/parts/" + fightClub.Key.String() + "/indexes/sd",
			"/library/parts/" + fightClub.Key.String() + "/indexes/sd/0",
			"/:/timeline?ratingKey=" + fightClub.Key.String() + "&state=stopped",
		}
		for i, path := range paths {
			rec := f.get(t, path)
			got := response{rec.Code, rec.Header().Clone(), append([]byte(nil), rec.Body.Bytes()...)}
			if !shared {
				legacy = append(legacy, got)
			} else if !reflect.DeepEqual(got, legacy[i]) {
				t.Fatalf("wire changed for %s: legacy=%+v shared=%+v", path, legacy[i], got)
			}
		}
		if raw.cancels != 1 {
			t.Fatalf("idle cancellation changed: shared=%v calls=%d", shared, raw.cancels)
		}
		f.srv.Close()
	}
}
