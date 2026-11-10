package plex

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/streamgrant"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func liveFixture(t *testing.T, transcoders ...core.Transcoder) (*fixture, *livetv.Service, livetv.Channel) {
	t.Helper()
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	db, e := store.OpenMemory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nsegment.ts\n")
	}))
	t.Cleanup(up.Close)
	var tc core.Transcoder
	if len(transcoders) > 0 {
		tc = transcoders[0]
	}
	service, e := livetv.New(livetv.Options{Transcoder: tc, DB: db, Now: func() time.Time { return fixedNow }, HTTPClient: up.Client()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { service.Close() })
	src := livetv.Source{Name: "Owner IPTV", Playlist: fmt.Sprintf("#EXTM3U\n#EXTINF:-1 tvg-id=\"owner\" tvg-chno=\"7\",Owner channel\n%s/master.m3u8\n", up.URL), Guide: `<tv><channel id="owner"><display-name>Owner channel</display-name></channel><programme channel="owner" start="20261018110000 +0000" stop="20261018130000 +0000"><title>Owner programme</title><desc>Owner-supplied guide</desc><rating><value>PG</value></rating></programme></tv>`}
	if _, e = service.ImportSource(context.Background(), src); e != nil {
		t.Fatal(e)
	}
	cs, e := service.Channels(context.Background())
	if e != nil || len(cs) != 1 {
		t.Fatalf("channels: %v %+v", e, cs)
	}
	f.srv.d.LiveTV = service.Frontend()
	f.srv.setTestSubscription(true)
	return f, service, cs[0]
}

type timedOutLiveProber struct{ core.Transcoder }

func (timedOutLiveProber) Probe(context.Context, string) (*core.MediaInfo, error) {
	return nil, context.DeadlineExceeded
}

func TestLiveTuneTimeoutIsRetryableUnavailable(t *testing.T) {
	f, service, channel := liveFixture(t, timedOutLiveProber{})
	r := f.request(t, "POST", "/livetv/dvrs/1/channels/"+channel.ID+"/tune", map[string]string{"X-Plex-Product": "Plex for Apple TV"})
	if r.Code != 503 || r.Header().Get("Retry-After") != "1" || len(service.ViewingSessions()) != 0 {
		t.Fatalf("timeout returned internal error or leaked session: status=%d retry=%q", r.Code, r.Header().Get("Retry-After"))
	}
}

func TestLiveChannelIdentifiersForTuning(t *testing.T) {
	f, _, channel := liveFixture(t)
	paths := []string{
		liveProviderPath + "/lineups/dvr/channels",
		"/livetv/epg/channels", "/livetv/epg/lineupchannels", "/livetv/epg/channelmap",
		"/media/grabbers/devices/1/channels", "/media/grabbers/devices/1/channelmap",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			jsonResponse := f.request(t, "GET", path, map[string]string{"Accept": "application/json"})
			if jsonResponse.Code != http.StatusOK {
				t.Fatalf("JSON status %d: %s", jsonResponse.Code, jsonResponse.Body.String())
			}
			channels := decodeJSON(t, jsonResponse)["Channel"].([]any)
			if len(channels) != 1 {
				t.Fatalf("channels: %s", jsonResponse.Body.String())
			}
			ch := channels[0].(map[string]any)
			if _, exists := ch["key"]; exists || ch["id"] != channel.ID || ch["gridKey"] != channel.ID {
				t.Fatalf("channel identifiers: %s", jsonResponse.Body.String())
			}
			xmlResponse := f.request(t, "GET", path, map[string]string{"Accept": "application/xml"})
			var container struct {
				Channels []struct {
					Attrs []xml.Attr `xml:",any,attr"`
				} `xml:"Channel"`
			}
			if xmlResponse.Code != http.StatusOK || xml.Unmarshal(xmlResponse.Body.Bytes(), &container) != nil || len(container.Channels) != 1 {
				t.Fatalf("XML channels: %d %s", xmlResponse.Code, xmlResponse.Body.String())
			}
			attrs := map[string]string{}
			for _, attr := range container.Channels[0].Attrs {
				attrs[attr.Name.Local] = attr.Value
			}
			if _, exists := attrs["key"]; exists || attrs["id"] != channel.ID || attrs["gridKey"] != channel.ID {
				t.Fatalf("XML channel identifiers: %s", xmlResponse.Body.String())
			}
		})
	}
	// Plezy falls back to id when key is absent and uses it in the tune path.
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/"+url.PathEscape(channel.ID)+"/tune", map[string]string{"Accept": "application/json"})
	if tune.Code != http.StatusOK {
		t.Fatalf("tune by channel id: %d %s", tune.Code, tune.Body.String())
	}
}

func TestLiveTVDVRGuideProviderAndTuneShapes(t *testing.T) {
	f, _, channel := liveFixture(t)
	// Official PMS contract uses a Dvr list, Device children, Channel grid keys,
	// broadcast bounds on Media, and Media.uuid on the tune metadata.
	dvr := f.request(t, "GET", "/livetv/dvrs", map[string]string{"Accept": "application/json"})
	if dvr.Code != 200 {
		t.Fatalf("DVR: %d %s", dvr.Code, dvr.Body.String())
	}
	mc := decodeJSON(t, dvr)
	dvrs, ok := mc["Dvr"].([]any)
	if !ok || len(dvrs) != 1 {
		t.Fatalf("Dvr shape: %s", dvr.Body.String())
	}
	dv := dvrs[0].(map[string]any)
	if dv["key"] != "1" || dv["epgIdentifier"] != livetv.Provider || dv["lineup"] == nil || dv["Device"] == nil {
		t.Fatalf("DVR fields %s", dvr.Body.String())
	}
	channels := f.request(t, "GET", liveProviderPath+"/lineups/dvr/channels", map[string]string{"Accept": "application/json"})
	cm := decodeJSON(t, channels)
	chs := cm["Channel"].([]any)
	ch := chs[0].(map[string]any)
	if ch["id"] != channel.ID || ch["vcn"] != "7" || ch["gridKey"] != channel.ID {
		t.Fatalf("channel shape: %s", channels.Body.String())
	}
	if strings.Contains(channels.Body.String(), "master.m3u8") {
		t.Fatal("owner upstream URL exposed")
	}
	grid := f.request(t, "GET", liveProviderPath+"/grid?date=2026-10-18&channelGridKey="+channel.ID, map[string]string{"Accept": "application/json"})
	gm := decodeJSON(t, grid)
	programmes := gm["Metadata"].([]any)
	p := programmes[0].(map[string]any)
	media := p["Media"].([]any)[0].(map[string]any)
	if p["type"] != "movie" || p["live"] == true {
		t.Fatalf("programme advertised as a tune session: %s", grid.Body.String())
	}
	detail := f.request(t, "GET", p["key"].(string), map[string]string{"Accept": "application/json"})
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "Owner-supplied guide") {
		t.Fatalf("programme detail: %d %s", detail.Code, detail.Body.String())
	}
	if p["title"] != "Owner programme" || media["beginsAt"] != float64(time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC).Unix()) || media["channelIdentifier"] != channel.ID || media["protocol"] != "livetv" {
		t.Fatalf("EPG shape: %s", grid.Body.String())
	}
	providers := f.request(t, "GET", "/media/providers", map[string]string{"Accept": "application/json"})
	if !strings.Contains(providers.Body.String(), livetv.Provider) {
		t.Fatal("Live TV provider undiscoverable")
	}
	home := f.request(t, "GET", "/hubs", map[string]string{"Accept": "application/json"})
	if !strings.Contains(home.Body.String(), "livetv.onnow") {
		t.Fatalf("On Now row absent: %s", home.Body.String())
	}
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/7/tune", map[string]string{"Accept": "application/json"})
	if tune.Code != 200 {
		t.Fatalf("tune %d %s", tune.Code, tune.Body.String())
	}
	tm := decodeJSON(t, tune)
	subscriptions, ok := tm["MediaSubscription"].([]any)
	if !ok || len(subscriptions) != 1 || tune.Header().Get("X-Plex-Activity") == "" {
		t.Fatalf("Plex Web tune needs subscription + activity: %s", tune.Body.String())
	}
	subscription := subscriptions[0].(map[string]any)
	operation, ok := subscription["MediaGrabOperation"].(map[string]any)
	if !ok || operation["mediaIndex"] != float64(0) || operation["Metadata"] == nil || subscription["Timeline"] == nil {
		t.Fatalf("Plex Web grab/timeline shape: %s", tune.Body.String())
	}
	metadata := tm["Metadata"].([]any)[0].(map[string]any)
	if metadata["live"] != true || metadata["genuineMediaAnalysis"] != false {
		t.Fatalf("tune flags %s", tune.Body.String())
	}
	sessionPath := metadata["key"].(string)
	sessionID := strings.TrimPrefix(sessionPath, "/livetv/sessions/")
	subget := f.request(t, "GET", "/media/subscriptions/"+sessionID, map[string]string{"Accept": "application/json"})
	if subget.Code != 200 || !strings.Contains(subget.Body.String(), "MediaGrabOperation") {
		t.Fatalf("subscription polling %d %s", subget.Code, subget.Body.String())
	}
	tuneMedia := metadata["Media"].([]any)[0].(map[string]any)
	if tuneMedia["uuid"] != sessionID {
		t.Fatal("session UUID absent")
	}
	part := tuneMedia["Part"].([]any)[0].(map[string]any)
	if part["key"] != sessionPath+"/0/index.m3u8" {
		t.Fatal("incorrect consumer playlist key")
	}
	decision := f.request(t, "GET", "/video/:/transcode/universal/decision?path="+sessionPath, map[string]string{"Accept": "application/json"})
	if decision.Code != 200 || !strings.Contains(decision.Body.String(), "generalDecisionCode") {
		t.Fatalf("live decision %d %s", decision.Code, decision.Body.String())
	}
	start := f.request(t, "GET", "/video/:/transcode/universal/start.m3u8?path="+sessionPath, nil)
	if start.Code != 200 || !strings.Contains(start.Body.String(), "#EXTM3U") || start.Header().Get("Location") != "" {
		t.Fatalf("live start %d %s", start.Code, start.Body.String())
	}
	playlist := f.request(t, "GET", sessionPath+"/0/index.m3u8", nil)
	if playlist.Code != 200 || !strings.Contains(playlist.Body.String(), "/relay/0.ts") {
		t.Fatalf("playlist %d %s", playlist.Code, playlist.Body.String())
	}
	other := f.request(t, "GET", sessionPath+"/0/index.m3u8", map[string]string{"X-Plex-Token": friendToken})
	if other.Code != 403 {
		t.Fatalf("foreign consumer access: %d", other.Code)
	}
	unknown := f.request(t, "GET", sessionPath+"/9/index.m3u8", nil)
	if unknown.Code != 404 {
		t.Fatal("unknown consumer accepted")
	}
}

// TestLiveGridAndTuneLoadOneChannel pins the Plex grid and tune to the
// requested channel: the old paths converted every channel's programmes in the
// window and ran a current-programme query per channel.
func TestLiveGridAndTuneLoadOneChannel(t *testing.T) {
	f, service, channel := liveFixture(t)
	var playlist, guide strings.Builder
	playlist.WriteString("#EXTM3U\n")
	guide.WriteString("<tv>")
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("bulk%d", i)
		fmt.Fprintf(&playlist, "#EXTINF:-1 tvg-id=\"%s\",Bulk %d\nhttps://owner.invalid/%s.m3u8\n", id, i, id)
		fmt.Fprintf(&guide, `<channel id="%s"><display-name>Bulk %d</display-name></channel><programme channel="%s" start="20261018110000 +0000" stop="20261018130000 +0000"><title>%s show</title></programme>`, id, i, id, id)
	}
	guide.WriteString("</tv>")
	if _, err := service.ImportSource(context.Background(), livetv.Source{ID: "bulk", Name: "Bulk", Playlist: playlist.String(), Guide: guide.String()}); err != nil {
		t.Fatal(err)
	}
	ctx, stats := store.WithQueryStats(context.Background())
	req := httptest.NewRequest("GET", liveProviderPath+"/grid?date=2026-10-18&channelGridKey="+url.QueryEscape(channel.ID), nil)
	req = req.WithContext(ctx)
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grid: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Owner programme") || strings.Contains(body, "bulk1 show") {
		t.Fatalf("grid not channel-scoped: %s", body)
	}
	if rows := stats.Rows(); rows > 6 {
		t.Fatalf("scoped grid read %d rows; the whole-lineup path read 21 channels plus programmes", rows)
	}

	detailCtx, detailStats := store.WithQueryStats(context.Background())
	programmeID := fmt.Sprintf("%s-%d", channel.ID, time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC).UnixMilli())
	detail := httptest.NewRequest("GET", liveProviderPath+"/metadata/"+programmeID, nil).WithContext(detailCtx)
	detail.Header.Set("X-Plex-Token", ownerToken)
	detailRec := httptest.NewRecorder()
	f.srv.ServeHTTP(detailRec, detail)
	if detailRec.Code != 200 || !strings.Contains(detailRec.Body.String(), "Owner-supplied guide") || detailStats.Rows() > 6 {
		t.Fatalf("detail not channel-scoped: status=%d rows=%d", detailRec.Code, detailStats.Rows())
	}

	tuneCtx, tuneStats := store.WithQueryStats(context.Background())
	tune := httptest.NewRequest("POST", "/livetv/dvrs/1/channels/"+url.PathEscape(channel.ID)+"/tune", nil)
	tune = tune.WithContext(tuneCtx)
	tune.Header.Set("X-Plex-Token", ownerToken)
	tune.Header.Set("X-Plex-Client-Identifier", "client-1")
	trec := httptest.NewRecorder()
	f.srv.ServeHTTP(trec, tune)
	if trec.Code != http.StatusOK {
		t.Fatalf("tune: %d %s", trec.Code, trec.Body.String())
	}
	if q := tuneStats.Queries(); q > 10 {
		t.Fatalf("tune used %d queries; the old path ran one per channel", q)
	}
}

func TestLiveTVHonestPassAndViewerRestrictions(t *testing.T) {
	f, service, c := liveFixture(t)
	f.srv.setTestSubscription(false)
	for _, route := range []string{"/livetv/dvrs", liveProviderPath + "/grid", "/media/grabbers"} {
		rec := f.get(t, route)
		if rec.Code != 404 {
			t.Fatalf("Pass-disabled route advertised %s: %d", route, rec.Code)
		}
	}
	providers := f.request(t, "GET", "/media/providers", map[string]string{"Accept": "application/json"})
	if strings.Contains(providers.Body.String(), livetv.Provider) {
		t.Fatal("Pass spoofed")
	}
	f.srv.setTestSubscription(true)
	if e := service.SetChannelViewers(context.Background(), c.ID, livetv.ChannelViewers{Deny: []int64{friendUser.ID}}); e != nil {
		t.Fatal(e)
	}
	channels := f.request(t, "GET", liveProviderPath+"/lineups/dvr/channels", map[string]string{"Accept": "application/json", "X-Plex-Token": friendToken})
	if strings.Contains(channels.Body.String(), "Owner channel") {
		t.Fatal("channel access policy leaked")
	}
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/7/tune", map[string]string{"X-Plex-Token": friendToken})
	if tune.Code != 404 {
		t.Fatal("restricted channel tuned")
	}
	f.srv.testAuth().users["kid-live"] = core.User{ID: 99, MaxContentRating: "G"}
	if e := service.SetChannelViewers(context.Background(), c.ID, livetv.ChannelViewers{Allow: []int64{friendUser.ID}}); e != nil {
		t.Fatal(e)
	}
	guide := f.request(t, "GET", liveProviderPath+"/grid?date=2026-10-18", map[string]string{"X-Plex-Token": "kid-live"})
	if strings.Contains(guide.Body.String(), "Owner programme") {
		t.Fatal("programme rating restriction leaked")
	}
}

func TestLiveTVProviderRequiresAccessibleEnabledChannel(t *testing.T) {
	ctx := context.Background()
	f, service, channel := liveFixture(t)
	visible := func(token string) bool {
		t.Helper()
		response := f.request(t, "GET", "/media/providers", map[string]string{"Accept": "application/json", "X-Plex-Token": token})
		if response.Code != http.StatusOK {
			t.Fatalf("providers: %d %s", response.Code, response.Body.String())
		}
		return strings.Contains(response.Body.String(), livetv.Provider)
	}
	if !visible(ownerToken) {
		t.Fatal("enabled channel did not advertise Live TV")
	}
	channel.HiddenApps = []string{"plex"}
	if _, err := service.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	if visible(ownerToken) || visible(friendToken) {
		t.Fatal("Plex-hidden channel advertised Live TV")
	}
	if channels, err := service.AllowedChannels(ctx, &ownerUser); err != nil || len(channels) != 1 {
		t.Fatalf("Plex visibility changed the external lineup: %v %v", channels, err)
	}
	response := f.request(t, "GET", liveProviderPath+"/lineups/dvr/channels", map[string]string{"Accept": "application/json"})
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), channel.Name) {
		t.Fatalf("Plex-hidden channel in guide: %d %s", response.Code, response.Body.String())
	}
	channel.HiddenApps = nil
	channel.Enabled = false
	if _, err := service.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	if visible(ownerToken) || visible(friendToken) {
		t.Fatal("disabled channel advertised Live TV")
	}
	channel.Enabled, channel.Private = true, true
	if _, err := service.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	if !visible(ownerToken) || visible(friendToken) {
		t.Fatal("private channel advertised to an ungranted viewer")
	}
	if err := service.SetChannelViewers(ctx, channel.ID, livetv.ChannelViewers{Allow: []int64{friendUser.ID}}); err != nil {
		t.Fatal(err)
	}
	if !visible(friendToken) {
		t.Fatal("granted private channel was not advertised")
	}
	if err := service.SavePolicy(ctx, livetv.Policy{UserID: friendUser.ID, Restricted: true}); err != nil {
		t.Fatal(err)
	}
	if visible(friendToken) {
		t.Fatal("empty viewer allow-list advertised Live TV")
	}
}

func TestLiveTVProviderAfterXtreamLiveImportDisabled(t *testing.T) {
	ctx := context.Background()
	f, service, channel := liveFixture(t)
	if err := service.DeleteSource(ctx, channel.SourceID); err != nil {
		t.Fatal(err)
	}
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player_api.php" {
			fmt.Fprint(w, `<tv></tv>`)
			return
		}
		switch r.URL.Query().Get("action") {
		case "get_live_categories":
			fmt.Fprint(w, `[{"category_id":"1","category_name":"News"}]`)
		case "get_live_streams":
			fmt.Fprint(w, `[{"num":1,"stream_id":1,"name":"News","category_id":"1"}]`)
		default:
			fmt.Fprint(w, `{"user_info":{"auth":1,"status":"Active"}}`)
		}
	}))
	t.Cleanup(panel.Close)
	source := livetv.Source{ID: "xtream", Name: "Test panel", VOD: true, Auth: &livetv.SourceAuth{Type: "xtream", Server: panel.URL, Username: "test", Password: "test"}}
	if _, err := service.ImportSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	visible := func() bool {
		t.Helper()
		response := f.request(t, "GET", "/media/providers", map[string]string{"Accept": "application/json"})
		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), livetv.Provider)
	}
	if !visible() {
		t.Fatal("Xtreme live import did not advertise Live TV")
	}
	off := false
	source.LiveTV = &off
	result, err := service.ImportSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChannelCount != 0 || !result.VOD || visible() {
		t.Fatalf("Xtreme live import disabled: channels=%d, VOD=%v, provider=%v", result.ChannelCount, result.VOD, visible())
	}
}

// Native clients require usable source analysis at tune, before requesting media.
type liveProbeTranscoder struct {
	*fakeTranscoder
	info   core.MediaInfo
	probes int
}

func (t *liveProbeTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	t.probes++
	v := t.info
	return &v, nil
}
func (t *liveProbeTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

// Current publisher native-client tune parsing reads nonempty subscription/grab
// arrays, grab.Metadata, and a Media array. The older PMS serializer differs.
func TestNativeTuneReturnsRollingSubscription(t *testing.T) {
	tc := &liveProbeTranscoder{fakeTranscoder: newFakeTranscoder(core.Decision{}), info: sampleInfo}
	f, service, c := liveFixture(t, tc)
	for _, client := range []struct{ product, platform, version string }{
		{"Plex for Apple TV", "tvOS", "2026.20.0"},
		{"Plex for iOS", "iOS", "2026.20.0"},
		{"Plex for Android", "Android", "2026.20.0"},
		{"Plex for Apple TV", "tvOS", ""},
	} {
		for _, format := range []string{"application/json", "application/xml"} {
			tune := f.request(t, "POST", "/livetv/dvrs/1/channels/"+c.ID+"/tune", map[string]string{"Accept": format, "X-Plex-Product": client.product, "X-Plex-Platform": client.platform, "X-Plex-Version": client.version, "X-Plex-Session-Identifier": "client-playback-session"})
			if tune.Code != 200 || tune.Header().Get("X-Plex-Activity") == "" {
				t.Fatalf("tune: %d %s", tune.Code, tune.Body.String())
			}
			body := tune.Body.String()
			if strings.Contains(body, "playQueueID") {
				t.Fatalf("tune returned an ordinary play queue: %s", body)
			}
			if format == "application/json" {
				mc := decodeJSON(t, tune)
				sub := mc["MediaSubscription"].([]any)[0].(map[string]any)
				op := sub["MediaGrabOperation"].([]any)[0].(map[string]any)
				video := op["Metadata"].(map[string]any)
				media := video["Media"].([]any)[0].(map[string]any)
				if sub["type"] != float64(4) || sub["channelIdentifier"] != c.ID || op["status"] != "inprogress" || video["genuineMediaAnalysis"] != true {
					t.Fatal(body)
				}
				subID, ok := op["mediaSubscriptionID"].(float64)
				if !ok || subID <= 0 || sub["key"] != fmt.Sprintf("%.0f", subID) || op["Video"] != nil || mc["Metadata"] != nil || op["provider"] != f.srv.identity().MachineIdentifier {
					t.Fatal(body)
				}
				uuid := media["uuid"].(string)
				if len(uuid) != 36 || uuid[8] != '-' || uuid[14] != '4' || video["guid"] != "plex://livetv/session/"+uuid {
					t.Fatalf("invalid live identity: %s", body)
				}
				if len(media["Part"].([]any)[0].(map[string]any)["Stream"].([]any)) < 2 {
					t.Fatalf("source analysis missing: %s", body)
				}
			} else if !strings.Contains(body, `<MediaSubscription`) || !strings.Contains(body, `type="4"`) || !strings.Contains(body, `<Video`) || !strings.Contains(body, `genuineMediaAnalysis="1"`) {
				t.Fatal(body)
			}
			infos := service.Sessions(context.Background(), &ownerUser)
			if len(infos) != 1 || infos[0].ClientSessionID != "client-playback-session" || infos[0].ActivityID != tune.Header().Get("X-Plex-Activity") || infos[0].ActivityID == infos[0].ID {
				t.Fatalf("client/server identities conflated or abandoned tune retained: %+v", infos)
			}
		}
	}
}

func TestNativeLiveSubscriptionOwnershipAndPlayback(t *testing.T) {
	tc := &liveProbeTranscoder{fakeTranscoder: newFakeTranscoder(core.Decision{}), info: sampleInfo}
	f, service, channel := liveFixture(t, tc)
	headers := map[string]string{"Accept": "application/json", "X-Plex-Product": "Plex for Apple TV", "X-Plex-Platform": "tvOS", "X-Plex-Version": "2026.20.0"}
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/"+channel.ID+"/tune", headers)
	if tune.Code != 200 {
		t.Fatal(tune.Body.String())
	}
	sub := decodeJSON(t, tune)["MediaSubscription"].([]any)[0].(map[string]any)
	op := sub["MediaGrabOperation"].([]any)[0].(map[string]any)
	video := op["Metadata"].(map[string]any)
	path := video["key"].(string)
	id := video["Media"].([]any)[0].(map[string]any)["uuid"].(string)
	subPath := "/media/subscriptions/" + sub["key"].(string)
	opPath := op["key"].(string)
	for _, target := range []string{subPath, opPath} {
		if got := f.request(t, "GET", target, headers); got.Code != 200 || !strings.Contains(got.Body.String(), id) {
			t.Fatalf("cannot refetch native bootstrap %s: %d %s", target, got.Code, got.Body.String())
		}
		for _, method := range []string{"GET", "DELETE"} {
			foreign := f.request(t, method, target, map[string]string{"Accept": "application/json", "X-Plex-Token": friendToken, "X-Plex-Platform": "Android"})
			if foreign.Code == 200 {
				t.Fatalf("another viewer can %s %s", method, target)
			}
		}
	}
	infos := service.Sessions(context.Background(), &ownerUser)
	if len(infos) != 1 {
		t.Fatal("foreign stop affected owner's session")
	}
	if infos[0].ClientSessionID != "client-1" {
		t.Fatal("missing client-session header did not fall back to client identifier")
	}
	listed := f.request(t, "GET", "/media/subscriptions", headers)
	if listed.Code != 200 || decodeJSON(t, listed)["MediaSubscription"].([]any)[0].(map[string]any)["key"] != sub["key"] {
		t.Fatalf("subscription list lost tune identity: %d %s", listed.Code, listed.Body.String())
	}
	decision := f.request(t, "GET", "/video/:/transcode/universal/decision?path="+url.QueryEscape(path)+"&hasMDE=1", headers)
	if decision.Code != 200 || !strings.Contains(decision.Body.String(), path) {
		t.Fatalf("selected live item cannot reach MDE: %d %s", decision.Code, decision.Body.String())
	}
	start := f.request(t, "GET", "/video/:/transcode/universal/start.m3u8?path="+url.QueryEscape(path), headers)
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), "#EXTM3U") || start.Header().Get("Location") != "" {
		t.Fatalf("live playback bootstrap did not reach media: %d %s", start.Code, start.Body.String())
	}
	for _, state := range []string{"playing", "stopped"} {
		timeline := f.request(t, "GET", "/:/timeline?key="+url.QueryEscape(path)+"&ratingKey="+id+"&state="+state, headers)
		if timeline.Code != 200 {
			t.Fatalf("live timeline %s: %d %s", state, timeline.Code, timeline.Body.String())
		}
	}
	if len(f.state.progress) != 0 {
		t.Fatal("live channel entered catalogue watch history")
	}
	if ended := f.request(t, "GET", subPath, headers); ended.Code == 200 {
		t.Fatal("ended live session remained playable through its subscription")
	}
	// A second subscription has a new numeric identity, and its DELETE resolves
	// to the correct owned stream instead of treating the number as a UUID.
	tune = f.request(t, "POST", "/livetv/dvrs/1/channels/"+channel.ID+"/tune", headers)
	next := decodeJSON(t, tune)["MediaSubscription"].([]any)[0].(map[string]any)
	if next["key"] == sub["key"] {
		t.Fatal("subscription identity reused")
	}
	stop := f.request(t, "DELETE", "/media/subscriptions/"+next["key"].(string), headers)
	if stop.Code != 200 || len(service.Sessions(context.Background(), &ownerUser)) != 0 {
		t.Fatalf("owned numeric subscription stop: %d %s", stop.Code, stop.Body.String())
	}
}

// tokenDASH writes a manifest whose media URLs use the Live TV session prefix
// ServeDASH rewrites onto stream-token routes.
type tokenDASH struct{ fakeDASH }

func (d *tokenDASH) ServeManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/dash+xml")
	fmt.Fprintf(w, `<MPD><SegmentTemplate initialization="session/%s/0/header" media="session/%s/0/$Number$.m4s"/></MPD>`, d.id, d.id)
}

type streamTokenTranscoder struct{ *fakeTranscoder }

func (t *streamTokenTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	info := sampleInfo
	return &info, nil
}

func (t *streamTokenTranscoder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	if req.Protocol != core.ProtocolDASH {
		return t.fakeTranscoder.StartHLS(ctx, req)
	}
	return &tokenDASH{fakeDASH{fakeHLS{id: req.SessionID, req: req}}}, nil
}

// A client that follows the playlist and DASH URLs the server just emitted
// gets the media with the stream token alone. Those URLs carry no account token.
func TestStreamTokenAloneFetchesPlaylistAndDASH(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			w.Header().Set("Content-Type", "video/mp2t")
			fmt.Fprint(w, "segment-bytes")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nsegment.ts\n")
	}))
	t.Cleanup(up.Close)
	tc := &streamTokenTranscoder{newFakeTranscoder(core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"})}
	service, err := livetv.New(livetv.Options{Transcoder: tc, DB: db, Now: func() time.Time { return fixedNow }, HTTPClient: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	src := livetv.Source{Name: "Owner IPTV", Playlist: fmt.Sprintf("#EXTM3U\n#EXTINF:-1 tvg-id=\"owner\" tvg-chno=\"7\",Owner channel\n%s/master.m3u8\n", up.URL)}
	if _, err = service.ImportSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	f.srv.d.LiveTV = service.Frontend()
	f.srv.setTestSubscription(true)

	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/7/tune", map[string]string{"Accept": "application/json"})
	if tune.Code != 200 {
		t.Fatalf("tune %d %s", tune.Code, tune.Body.String())
	}
	metadata := decodeJSON(t, tune)["Metadata"].([]any)[0].(map[string]any)
	sessionPath := metadata["key"].(string)
	playlist := f.request(t, "GET", sessionPath+"/0/index.m3u8", nil)
	if playlist.Code != 200 {
		t.Fatalf("playlist %d %s", playlist.Code, playlist.Body.String())
	}
	var mediaURL string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.Contains(line, streamgrant.StreamGrantParam) && strings.Contains(line, "/relay/") {
			mediaURL = strings.TrimSpace(line)
			break
		}
	}
	if mediaURL == "" || strings.Contains(playlist.Body.String(), "X-Plex-Token") {
		t.Fatalf("playlist did not publish a stream-token media URL:\n%s", playlist.Body.String())
	}
	followed := f.request(t, "GET", mediaURL, map[string]string{"X-Plex-Token": ""})
	if followed.Code != 200 || followed.Body.String() != "segment-bytes" {
		t.Fatalf("playlist media with only the stream token: %d %q", followed.Code, followed.Body.String())
	}

	mpd := f.request(t, "GET", "/video/:/transcode/universal/start.mpd?path="+sessionPath, nil)
	if mpd.Code != 200 {
		t.Fatalf("dash %d %s", mpd.Code, mpd.Body.String())
	}
	manifest := html.UnescapeString(mpd.Body.String())
	if strings.Contains(manifest, "X-Plex-Token") {
		t.Fatalf("account token in DASH manifest:\n%s", manifest)
	}
	attr := regexp.MustCompile(`(?:initialization|media)="([^"]+)"`)
	matches := attr.FindAllStringSubmatch(manifest, -1)
	if len(matches) < 2 {
		t.Fatalf("DASH manifest has no media URLs:\n%s", manifest)
	}
	initURL := matches[0][1]
	segmentURL := strings.ReplaceAll(matches[1][1], "$Number$", "0")
	for _, target := range []struct{ url, body string }{{initURL, "init-0"}, {segmentURL, "media-0-0"}} {
		if !strings.Contains(target.url, streamgrant.StreamGrantParam) {
			t.Fatalf("DASH URL has no stream token: %s", target.url)
		}
		got := f.request(t, "GET", target.url, map[string]string{"X-Plex-Token": ""})
		if got.Code != 200 || got.Body.String() != target.body {
			t.Fatalf("DASH %s with only the stream token: %d %q", target.url, got.Code, got.Body.String())
		}
	}
}

// Plex tvOS plays a virtual channel through decision and start.m3u8, then
// follows the segment with the stream token and no account token. A relative
// segment URL would be resolved under /video/:/transcode/universal/, miss the
// stream-token route, and the player would close the session.
func TestVirtualChannelHLSFromStartPlaylistKeepsTheSession(t *testing.T) {
	f := newFixture()
	t.Cleanup(func() { f.srv.Close() })
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Unix(0, 0).UTC().Add(time.Hour + time.Minute + 44*time.Second)
	pb := newFakePlayback(core.MediaInfo{Duration: 2 * time.Hour, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720},
		{Index: 1, Type: "audio", Codec: "aac"},
	}})
	tc := newFakeTranscoder(core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"})
	service, err := livetv.New(livetv.Options{Transcoder: tc, Playback: pb, DB: db, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	if _, err = service.SaveChannel(context.Background(), livetv.Channel{
		Name: "Family Animation", Number: "4", Kind: "virtual", Enabled: true, Rating: "PG",
		Epoch: time.Unix(0, 0).UTC(),
		Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Cartoon", DurationMS: int64((2 * time.Hour).Milliseconds()), Rating: "PG"}},
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.d.LiveTV = service.Frontend()
	f.srv.setTestSubscription(true)
	headers := map[string]string{"Accept": "application/json", "X-Plex-Product": "Plex for Apple TV", "X-Plex-Platform": "tvOS", "X-Plex-Version": "2026.20.0"}
	tune := f.request(t, "POST", "/livetv/dvrs/1/channels/4/tune", headers)
	if tune.Code != 200 {
		t.Fatalf("tune %d %s", tune.Code, tune.Body.String())
	}
	tuned := decodeJSON(t, tune)
	var sessionPath string
	if md, ok := tuned["Metadata"].([]any); ok && len(md) > 0 {
		sessionPath = md[0].(map[string]any)["key"].(string)
	} else {
		sub := tuned["MediaSubscription"].([]any)[0].(map[string]any)
		op := sub["MediaGrabOperation"].([]any)[0].(map[string]any)
		sessionPath = op["Metadata"].(map[string]any)["key"].(string)
	}
	decision := f.request(t, "GET", "/video/:/transcode/universal/decision?path="+url.QueryEscape(sessionPath), headers)
	if decision.Code != 200 {
		t.Fatalf("decision %d %s", decision.Code, decision.Body.String())
	}
	start := f.request(t, "GET", "/video/:/transcode/universal/start.m3u8?path="+url.QueryEscape(sessionPath), headers)
	if start.Code != 200 || !strings.Contains(start.Body.String(), "#EXTM3U") {
		t.Fatalf("start.m3u8 %d %s", start.Code, start.Body.String())
	}
	var segment string
	for _, line := range strings.Split(start.Body.String(), "\n") {
		if strings.Contains(line, "/relay/") {
			segment = strings.TrimSpace(line)
			break
		}
	}
	if segment == "" || !strings.HasPrefix(segment, sessionPath+"/0/relay/") || !strings.Contains(segment, streamgrant.StreamGrantParam+"=") {
		t.Fatalf("start.m3u8 segment is not a rooted stream-token URL:\n%s", start.Body.String())
	}

	if strings.Contains(start.Body.String(), "\nprogram/") {
		t.Fatal("start.m3u8 still advertises a relative segment")
	}
	media := f.request(t, "GET", segment, map[string]string{"X-Plex-Token": ""})
	if media.Code != 200 || !strings.HasPrefix(media.Body.String(), "shared-ts-") {
		t.Fatalf("segment with only the stream token: %d %q", media.Code, media.Body.String())
	}
	if len(service.ViewingSessions()) != 1 {
		t.Fatal("segment request closed the live session")
	}
	index := f.request(t, "GET", sessionPath+"/0/index.m3u8", headers)
	if index.Code != 200 || !strings.Contains(index.Body.String(), "\nrelay/") {
		t.Fatalf("session playlist lost relative segments: %d %s", index.Code, index.Body.String())
	}
}

// Malformed guide queries are the client's error, not the server's.
func TestLiveGuideBadQueriesAreBadRequests(t *testing.T) {
	f, _, _ := liveFixture(t)
	for _, q := range []string{"date=tomorrow", "beginsAt=x", "endsAt=y", "beginsAt=2000&endsAt=1000", "beginsAt=0&endsAt=99999999999"} {
		if r := f.request(t, "GET", liveProviderPath+"/grid?"+q, nil); r.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, r.Code)
		}
	}
	if r := f.request(t, "GET", liveProviderPath+"/grid", nil); r.Code != http.StatusOK {
		t.Errorf("default grid: %d %s", r.Code, r.Body.String())
	}
}

func TestChannelCreateAndDeleteRefreshThePlexLineup(t *testing.T) {
	f, service, channel := liveFixture(t)
	before := liveRefreshedAt(t, f)
	sub, ok := f.srv.notifier.subscribe(nil, &core.User{ID: 1, IsOwner: true})
	if !ok {
		t.Fatal("subscribe")
	}
	defer f.srv.notifier.unsubscribe(sub)
	service.SetOnChange(f.srv.NotifyLiveLineup)
	added, err := service.SaveChannel(context.Background(), livetv.Channel{
		ID: "added", Number: "8", Name: "Added channel", Kind: "virtual", Enabled: true,
		Epoch: time.Unix(0, 0).UTC(),
		Items: []livetv.Item{{Key: core.MovieKey(3), Title: "Added feature", DurationMS: 3600000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	afterSave := liveRefreshedAt(t, f)
	if afterSave <= before {
		t.Fatalf("create left refreshedAt at %v", afterSave)
	}
	lineup := f.request(t, "GET", liveProviderPath+"/lineups/dvr/channels", map[string]string{"Accept": "application/json"})
	if !strings.Contains(lineup.Body.String(), "Added channel") {
		t.Fatalf("new channel missing from lineup: %s", lineup.Body.String())
	}
	if err = service.DeleteChannel(context.Background(), channel.ID); err != nil {
		t.Fatal(err)
	}
	afterDelete := liveRefreshedAt(t, f)
	if afterDelete <= afterSave {
		t.Fatalf("delete left refreshedAt at %v", afterDelete)
	}
	lineup = f.request(t, "GET", liveProviderPath+"/lineups/dvr/channels", map[string]string{"Accept": "application/json"})
	if strings.Contains(lineup.Body.String(), channel.Name) || !strings.Contains(lineup.Body.String(), added.Name) {
		t.Fatalf("lineup after delete: %s", lineup.Body.String())
	}
	deadline := time.After(2 * time.Second)
	sawGuide := false
	for !sawGuide {
		select {
		case msg := <-sub.ch:
			if strings.Contains(string(msg.data), "livetv.guide") {
				sawGuide = true
			}
		case <-deadline:
			t.Fatal("clients were not told to reload the lineup")
		}
	}
}

func liveRefreshedAt(t *testing.T, f *fixture) float64 {
	t.Helper()
	rec := f.request(t, "GET", "/livetv/dvrs", map[string]string{"Accept": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("dvr %d %s", rec.Code, rec.Body.String())
	}
	dvrs := decodeJSON(t, rec)["Dvr"].([]any)
	return dvrs[0].(map[string]any)["refreshedAt"].(float64)
}
