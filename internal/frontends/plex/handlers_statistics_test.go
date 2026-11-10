package plex

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/braedonsaunders/ether/internal/stats"
)

func TestStatisticsOwnerOnly(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, path := range []string{"/statistics/bandwidth?timespan=6", "/statistics/resources?timespan=6", "/statistics/media?timespan=4", "/devices", "/devices/1"} {
		rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": friendToken})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s friend: %d", path, rec.Code)
		}
	}
}

func TestStatisticsTimespanParse(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, raw := range []string{"abc", "1.5", "", " 6"} {
		rec := f.get(t, "/statistics/bandwidth?timespan="+url.QueryEscape(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("timespan=%q: %d %s", raw, rec.Code, rec.Body.String())
		}
	}
	for _, raw := range []string{"0", "-1", "06", "007"} {
		rec := f.get(t, "/statistics/bandwidth?timespan="+raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("timespan=%q: %d", raw, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `size="0"`) && raw != "0" && raw != "06" {
			// -1 and 007 are unstored: empty 200
			if !strings.Contains(rec.Body.String(), `size="0"`) {
				t.Fatalf("empty unstored %q: %s", raw, rec.Body.String())
			}
		}
	}
	rec := f.get(t, "/statistics/bandwidth/zzz")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("prefix: %d", rec.Code)
	}
}

func TestStatisticsBandwidthShapeAndFilters(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	clock := fixedNow
	f.srv.setDashboard(stats.NewDashboard(stats.DashboardOptions{Now: func() time.Time { return clock }, Loc: time.UTC}))
	f.srv.dashboard().RecordBytes(1, "owner", "web", "Chrome", "Chrome", true, 4096)
	clock = fixedNow.Add(2 * time.Second)
	f.srv.dashboard().RecordBytes(2, "friend", "tv", "Apple TV", "tvOS", false, 100)

	rec := f.get(t, "/statistics/bandwidth?timespan=6")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `StatisticsBandwidth`) || !strings.Contains(body, `bytes="4096"`) || !strings.Contains(body, `lan="1"`) {
		t.Fatalf("shape: %d %s", rec.Code, body)
	}
	js, err := model.Marshal(&model.MediaContainer{StatisticsBandwidth: []model.StatisticsBandwidth{{Lan: true, Bytes: 1}}}, model.JSON)
	if err != nil || !strings.Contains(string(js), `"lan":true`) {
		t.Fatalf("json lan bool: %s %v", js, err)
	}
	if !strings.Contains(body, `name="Chrome"`) || !strings.Contains(body, `name="owner"`) {
		t.Fatalf("lookup: %s", body)
	}
	if !strings.Contains(body, `totalSize="2"`) {
		t.Fatalf("totalSize: %s", body)
	}

	rec = f.get(t, "/statistics/bandwidth?timespan=6&accountID=1")
	if strings.Contains(rec.Body.String(), `accountID="2"`) || !strings.Contains(rec.Body.String(), `accountID="1"`) {
		t.Fatalf("account filter: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&accountID=99")
	if !strings.Contains(rec.Body.String(), `size="0"`) || strings.Contains(rec.Body.String(), `Device`) {
		t.Fatalf("unknown account: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&accountID=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad account: %d", rec.Code)
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&lan=2")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad lan: %d", rec.Code)
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&limit=1")
	if !strings.Contains(rec.Body.String(), `totalSize="2"`) || strings.Count(rec.Body.String(), "<StatisticsBandwidth") != 1 {
		t.Fatalf("limit pages series: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth")
	if strings.Count(rec.Body.String(), `timespan="6"`) != 0 {
		t.Fatalf("no timespan must omit per-second: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `timespan="4"`) {
		t.Fatalf("no timespan includes hour: %s", rec.Body.String())
	}

	rec = f.get(t, "/statistics/bandwidth?timespan=6&limit=0")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `totalSize="2"`) || strings.Contains(rec.Body.String(), "<StatisticsBandwidth") {
		t.Fatalf("limit=0: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&limit=-1")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<StatisticsBandwidth") {
		t.Fatalf("limit=-1: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&sort=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown sort: %d", rec.Code)
	}

	firstAt := f.srv.dashboard().Bandwidth(stats.Query{Timespans: []int{hoststat.SpanSec}}).Rows[0].At
	rec = f.get(t, "/statistics/bandwidth?timespan=6&"+url.QueryEscape("at>=")+"="+strconv.FormatInt(firstAt, 10))
	if strings.Count(rec.Body.String(), "<StatisticsBandwidth") != 2 {
		t.Fatalf("ignored at>=: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/bandwidth?timespan=6&"+url.QueryEscape("at>")+"="+strconv.FormatInt(firstAt+1, 10))
	if strings.Count(rec.Body.String(), "<StatisticsBandwidth") != 1 {
		t.Fatalf("at> inclusive: %s", rec.Body.String())
	}
}

func TestStatisticsResourcesBuckets(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	res := hoststat.NewResources(hoststat.Options{Now: func() time.Time { return fixedNow }, Loc: time.UTC})
	res.Add(hoststat.Sample{At: fixedNow, HostCPU: 12.5, ProcessCPU: 1.5, HostMem: 40, ProcessMem: 2})
	f.srv.setResources(res)
	rec := f.get(t, "/statistics/resources?timespan=6")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `hostCpuUtilization="12.5"`) || !strings.Contains(body, `timespan="6"`) {
		t.Fatalf("resources: %d %s", rec.Code, body)
	}
	rec = f.get(t, "/statistics/resources?timespan=4")
	if !strings.Contains(rec.Body.String(), `size="0"`) {
		t.Fatalf("coarser resources: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/resources")
	if !strings.Contains(rec.Body.String(), `size="0"`) {
		t.Fatalf("no timespan resources: %s", rec.Body.String())
	}
}

func TestStatisticsMediaAndDevices(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setDashboard(stats.NewDashboard(stats.DashboardOptions{Now: func() time.Time { return fixedNow }, Loc: time.UTC}))
	f.srv.dashboard().RecordPlay(1, "owner", "web", 1, 3600)
	f.srv.dashboard().RecordPlay(1, "owner", "web", 1, 1800)
	rec := f.get(t, "/statistics/media?timespan=4")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `metadataType="1"`) || !strings.Contains(rec.Body.String(), `count="2"`) || !strings.Contains(rec.Body.String(), `duration="5400"`) {
		t.Fatalf("media: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/devices")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `clientIdentifier="web"`) {
		t.Fatalf("devices: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/devices/abc")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("devices/abc: %d", rec.Code)
	}
	rec = f.get(t, "/statistics/media?timespan=4&sort=metadataType")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("metadataType sort: %d", rec.Code)
	}
	rec = f.get(t, "/statistics/media?timespan=4&sort=duration")
	if rec.Code != http.StatusOK {
		t.Fatalf("duration sort: %d", rec.Code)
	}
	rec = f.get(t, "/statistics/media?timespan=4&accountID=99")
	if !strings.Contains(rec.Body.String(), `size="0"`) || strings.Contains(rec.Body.String(), `Device`) {
		t.Fatalf("media unknown account: %s", rec.Body.String())
	}
	rec = f.get(t, "/statistics/media?timespan=4&deviceID=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("media bad device: %d", rec.Code)
	}
}

func TestCountWriterFlushesEachSecond(t *testing.T) {
	clock := time.Unix(1000, 0)
	var got []int64
	w := &countWriter{
		ResponseWriter: httptest.NewRecorder(),
		now:            func() time.Time { return clock },
		flush:          func(n int64) { got = append(got, n) },
	}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("no flush on first write: %v", got)
	}
	clock = clock.Add(time.Second)
	if _, err := w.Write([]byte("ef")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 6 {
		t.Fatalf("second flush: %v", got)
	}
	w.finish()
	if len(got) != 1 {
		t.Fatalf("finish after flush: %v", got)
	}
}

func TestStatisticsCountersOnPartAndSegment(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/parts/1000000000000550/1759276800/file.mkv", "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("part: %d %s", rec.Code, rec.Body.String())
	}
	got := f.srv.dashboard().Bandwidth(stats.Query{Timespans: []int{hoststat.SpanSec}})
	if got.TotalSize != 1 || got.Rows[0].Bytes != 10 {
		t.Fatalf("part bytes: %+v", got)
	}

	f.trans.sessions["dash-sess"] = &fakeHLS{id: "dash-sess"}
	f.srv.transcodes.put(&transcodeEntry{id: "dash-sess", userID: ownerUser.ID, clientID: "client-1", key: fightClub.Key, started: true})
	rec = f.get(t, "/video/:/transcode/universal/session/dash-sess/00000.ts")
	if rec.Code != http.StatusOK || rec.Body.String() != "segment-0" {
		t.Fatalf("segment: %d %s", rec.Code, rec.Body.String())
	}
	got = f.srv.dashboard().Bandwidth(stats.Query{Timespans: []int{hoststat.SpanSec}})
	var sum int64
	for _, row := range got.Rows {
		sum += row.Bytes
	}
	if sum != 10+int64(len("segment-0")) {
		t.Fatalf("part+segment bytes %d rows %+v", sum, got.Rows)
	}
}
