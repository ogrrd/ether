package plex

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

var (
	bandwidthSpans = []int{frontend.StatisticsYear, frontend.StatisticsMonth, frontend.StatisticsWeek, frontend.StatisticsDay, frontend.StatisticsHour, frontend.StatisticsSecond}
	mediaSpans     = []int{frontend.StatisticsYear, frontend.StatisticsMonth, frontend.StatisticsWeek, frontend.StatisticsDay, frontend.StatisticsHour}
	resourceSpans  = []int{frontend.StatisticsSecond}

	bandwidthSort = map[string]bool{"at": true, "accountID": true, "deviceID": true, "timespan": true, "lan": true, "bytes": true}
	mediaSort     = map[string]bool{"at": true, "accountID": true, "deviceID": true, "timespan": true, "count": true, "duration": true}
	resourceSort  = map[string]bool{"at": true, "timespan": true, "hostCpuUtilization": true, "processCpuUtilization": true, "hostMemoryUtilization": true, "processMemoryUtilization": true}
)

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs := s.d.Dashboard.Devices()
	mc := &model.MediaContainer{Size: len(devs)}
	for _, d := range devs {
		mc.Device = append(mc.Device, modelDevice(d))
	}
	s.write(w, r, mc)
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := parseWhole(chi.URLParam(r, "id"))
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	dev, found := s.d.Dashboard.Device(id)
	if !found {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Device: []model.Device{modelDevice(dev)}})
}

func modelDevice(d frontend.StatisticsDevice) model.Device {
	return model.Device{ID: d.ID, Name: d.Name, Platform: d.Platform, ClientIdentifier: d.ClientIdentifier, CreatedAt: d.CreatedAt}
}

// handleStatisticsBandwidth answers this node's bandwidth series. There is
// no cluster pin; the client reads the node it connected to.
func (s *Server) handleStatisticsBandwidth(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseStatQuery(w, r, bandwidthSpans, bandwidthSort)
	if !ok {
		return
	}
	page := s.d.Dashboard.Bandwidth(q)
	mc := statisticsContainer(page.TotalSize, len(page.Rows)+len(page.Devices)+len(page.Accounts), q.Offset)
	for _, row := range page.Rows {
		mc.StatisticsBandwidth = append(mc.StatisticsBandwidth, model.StatisticsBandwidth{
			AccountID: row.AccountID, DeviceID: row.DeviceID, Timespan: row.Timespan, At: row.At, Lan: row.Lan == 1, Bytes: row.Bytes,
		})
	}
	appendLookups(mc, page)
	s.write(w, r, mc)
}

func (s *Server) handleStatisticsMedia(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseStatQuery(w, r, mediaSpans, mediaSort)
	if !ok {
		return
	}
	page := s.d.Dashboard.Media(q)
	mc := statisticsContainer(page.TotalSize, len(page.Media)+len(page.Devices)+len(page.Accounts), q.Offset)
	for _, row := range page.Media {
		mc.StatisticsMedia = append(mc.StatisticsMedia, model.StatisticsMedia{
			AccountID: row.AccountID, DeviceID: row.DeviceID, Timespan: row.Timespan, At: row.At, MetadataType: row.MetadataType, Count: row.Count, Duration: row.Duration,
		})
	}
	appendLookups(mc, page)
	s.write(w, r, mc)
}

func (s *Server) handleStatisticsResources(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseStatQuery(w, r, resourceSpans, resourceSort)
	if !ok {
		return
	}
	var rows []frontend.ResourceSample
	if len(q.Timespans) == 1 && q.Timespans[0] == frontend.StatisticsSecond {
		rows = s.d.Resources.Query(frontend.StatisticsSecond, q.AtMin, q.AtMax, q.ExactAt, q.HasExact, q.HasAtMin, q.HasAtMax)
	}
	sortResources(rows, q.Sort, q.Desc)
	total := len(rows)
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.HasLimit && q.Limit <= 0 {
		rows = nil
	} else if q.Offset > len(rows) {
		rows = nil
	} else {
		rows = rows[q.Offset:]
		if q.HasLimit && len(rows) > q.Limit {
			rows = rows[:q.Limit]
		}
	}
	mc := statisticsContainer(total, len(rows), q.Offset)
	for _, row := range rows {
		mc.StatisticsResources = append(mc.StatisticsResources, model.StatisticsResources{
			Timespan: frontend.StatisticsSecond, At: row.At.Unix(), HostCPU: row.HostCPU, ProcessCPU: row.ProcessCPU, HostMemory: row.HostMem, ProcessMem: row.ProcessMem,
		})
	}
	s.write(w, r, mc)
}

func statisticsContainer(total, size, offset int) *model.MediaContainer {
	mc := &model.MediaContainer{Size: size}
	mc.SetPage(offset, total)
	return mc
}

func appendLookups(mc *model.MediaContainer, page frontend.StatisticsPage) {
	for _, d := range page.Devices {
		mc.Device = append(mc.Device, modelDevice(d))
	}
	for _, a := range page.Accounts {
		mc.Account = append(mc.Account, model.Account{ID: a.ID, Key: "/accounts/" + strconv.FormatInt(a.ID, 10), Name: a.Name})
	}
}

func (s *Server) parseStatQuery(w http.ResponseWriter, r *http.Request, kept []int, keys map[string]bool) (frontend.StatisticsQuery, bool) {
	var q frontend.StatisticsQuery
	values := r.URL.Query()
	if raw, present := firstQuery(values, "timespan"); present {
		n, ok := parseWhole32(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		if !containsInt(kept, int(n)) {
			return q, true
		}
		q.Timespans = []int{int(n)}
	} else if kept[0] == frontend.StatisticsSecond && len(kept) == 1 {
		// resources with no timespan: empty
	} else {
		for _, span := range kept {
			if span != frontend.StatisticsSecond {
				q.Timespans = append(q.Timespans, span)
			}
		}
	}
	if raw, present := firstQuery(values, "accountID"); present {
		n, ok := parseWhole(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.AccountID, q.HasAccount = n, true
	}
	if raw, present := firstQuery(values, "deviceID"); present {
		n, ok := parseWhole(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.DeviceID, q.HasDevice = n, true
	}
	if raw, present := firstQuery(values, "lan"); present {
		n, ok := parseWhole(raw)
		if !ok || (n != 0 && n != 1) {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.Lan, q.HasLan = int(n), true
	}
	if raw, present := firstQuery(values, "bytes"); present {
		if _, ok := parseWhole(raw); !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
	}
	if raw, present := firstQuery(values, "at>"); present {
		n, ok := parseWhole(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.AtMin, q.HasAtMin = n, true
	}
	if raw, present := firstQuery(values, "at<"); present {
		n, ok := parseWhole(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.AtMax, q.HasAtMax = n, true
	}
	if raw, present := firstQuery(values, "at="); present {
		n, ok := parseWhole(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.ExactAt, q.HasExact = n, true
	}
	limitRaw, hasLimit := firstQuery(values, "limit")
	if !hasLimit {
		limitRaw, hasLimit = firstQuery(values, "count")
	}
	if hasLimit {
		n, ok := parseWhole32(limitRaw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.Limit, q.HasLimit = int(n), true
	}
	if raw, present := firstQuery(values, "offset"); present {
		n, ok := parseWhole32(raw)
		if !ok {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.Offset = int(n)
	} else if pr := FromContext(r.Context()); pr.HasPage {
		q.Offset = pr.Start
		if !hasLimit && pr.Size >= 0 {
			q.Limit, q.HasLimit = pr.Size, true
		}
	}
	if raw, present := firstQuery(values, "sort"); present {
		field, desc, ok := parseSort(raw)
		if !ok || !keys[field] {
			writeHTTPError(w, http.StatusBadRequest)
			return q, false
		}
		q.Sort, q.Desc = field, desc
	}
	return q, true
}

func firstQuery(q map[string][]string, key string) (string, bool) {
	vs, ok := q[key]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

func parseWhole(s string) (int64, bool) {
	if s == "" || s[0] == '+' || s[0] == ' ' || s[len(s)-1] == ' ' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func parseWhole32(s string) (int64, bool) {
	if s == "" || s[0] == '+' || s[0] == ' ' || s[len(s)-1] == ' ' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 32)
	return n, err == nil
}

func parseSort(s string) (field string, desc bool, ok bool) {
	if s == "" {
		return "", false, false
	}
	field, rest, found := strings.Cut(s, ":")
	if field == "" {
		return "", false, false
	}
	if found {
		switch rest {
		case "desc":
			desc = true
		case "asc", "":
		default:
			return "", false, false
		}
	}
	return field, desc, true
}

func sortResources(rows []frontend.ResourceSample, key string, desc bool) {
	if key == "" || key == "at" && !desc {
		return
	}
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		var cmp int
		switch key {
		case "hostCpuUtilization":
			cmp = cmpFloat(a.HostCPU, b.HostCPU)
		case "processCpuUtilization":
			cmp = cmpFloat(a.ProcessCPU, b.ProcessCPU)
		case "hostMemoryUtilization":
			cmp = cmpFloat(a.HostMem, b.HostMem)
		case "processMemoryUtilization":
			cmp = cmpFloat(a.ProcessMem, b.ProcessMem)
		default:
			cmp = cmpFloat(float64(a.At.Unix()), float64(b.At.Unix()))
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	}
	// tiny N; insertion is enough
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

type countWriter struct {
	http.ResponseWriter
	now   func() time.Time
	flush func(int64)
	n     int64
	last  int64
	t0    time.Time
}

func (s *Server) newCountWriter(w http.ResponseWriter, r *http.Request) *countWriter {
	return &countWriter{ResponseWriter: w, now: s.now, flush: func(n int64) { s.noteStreamBytes(r, n) }}
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	if w.flush == nil {
		return n, err
	}
	t := w.now()
	if w.t0.IsZero() {
		w.t0 = t
		return n, err
	}
	if t.Sub(w.t0) >= time.Second {
		w.emit(t)
	}
	return n, err
}

func (w *countWriter) emit(t time.Time) {
	delta := w.n - w.last
	if delta > 0 {
		w.flush(delta)
		w.last = w.n
	}
	w.t0 = t
}

func (w *countWriter) finish() {
	if w.flush == nil {
		return
	}
	w.emit(w.now())
}

func (w *countWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) noteStreamBytes(r *http.Request, n int64) {
	if n <= 0 || s.d.Dashboard == nil {
		return
	}
	pr := FromContext(r.Context())
	account := int64(0)
	name := ""
	if pr.User != nil {
		account = pr.User.ID
		name = pr.User.Username
	}
	s.d.Dashboard.RecordBytes(account, name, pr.ClientID, pr.DeviceName, pr.Platform, !pr.Remote, n)
}
