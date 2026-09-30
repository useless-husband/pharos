package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/uptime"
)

type statTile struct {
	Label string
	Value string
	Note  string
	Class string
}

type overviewRow struct {
	ID         string
	Name       string
	Type       string
	Target     string
	Status     model.Status
	StatusText string
	Message    string
	Up24       string
	Up7        string
	Up30       string
	Avg        string
	P95        string
	Spark      template.HTML
	LastAgo    string
	LastAt     string
	Public     bool
}

// availability formats a window's availability or a dash.
func availability(periods []model.Period, from, to, now time.Time) string {
	r, ok := uptime.Summarize(periods, from, to, now).Ratio()
	if !ok {
		return "–"
	}
	return i18n.Percent(r)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.cfg()
	now := s.now()
	p := s.basePage(r, "")
	p.Title, p.Nav = p.T("nav.monitors"), "monitors"
	periods, err := s.store.Periods(ctx, now.AddDate(0, 0, -30), now)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	open, _ := s.store.Incidents(ctx, store.IncidentQuery{OpenOnly: true})
	spark, err := s.store.LatencySeriesAll(ctx, now.Add(-24*time.Hour).Truncate(time.Hour), now, time.Hour)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}

	var rows []overviewRow
	counts := map[model.Status]int{}
	var all []model.Period
	public := publicSet(cfg)
	for _, st := range s.engine.Snapshot() {
		m := st.Monitor
		counts[st.Status]++
		ps := periods[m.ID]
		all = append(all, ps...)
		row := overviewRow{
			ID: m.ID, Name: m.Name, Type: m.Type, Target: m.Target(),
			Status: st.Status, StatusText: i18n.Status(p.Lang, st.Status),
			Up24:   availability(ps, now.Add(-24*time.Hour), now, now),
			Up7:    availability(ps, now.AddDate(0, 0, -7), now, now),
			Up30:   availability(ps, now.AddDate(0, 0, -30), now, now),
			Public: public[m.ID], Avg: "–", P95: "–",
		}
		if st.LastCheck != nil {
			row.LastAgo = i18n.Ago(p.Lang, now.Sub(st.LastCheck.At))
			row.LastAt = i18n.FormatTime(st.LastCheck.At, p.Loc)
			if st.Status != model.StatusUp && st.LastCheck.Message != "" {
				row.Message = st.LastCheck.Message
			}
		}
		if st.Status == model.StatusDown && st.Incident != nil {
			row.Message = st.Incident.Cause
		}
		if m.Type != config.TypePush {
			pts := spark[m.ID]
			row.Spark = sparkline(pts)
			avg, p95 := summarizeLatency(pts)
			if avg > 0 {
				row.Avg, row.P95 = i18n.Short(avg), i18n.Short(p95)
			}
		}
		rows = append(rows, row)
	}
	fleet := "–"
	if len(rows) > 0 {
		// Fleet availability: every monitor's counted time pooled together.
		if r, ok := uptime.Summarize(all, now.AddDate(0, 0, -30), now, now).Ratio(); ok {
			fleet = i18n.Percent(r)
		}
	}
	operational := counts[model.StatusUp]
	tiles := []statTile{
		{Label: p.T("tile.monitors"), Value: strconv.Itoa(len(rows)), Note: p.T("tile.operational", operational)},
		{Label: p.T("tile.down"), Value: strconv.Itoa(counts[model.StatusDown]), Class: classIf(counts[model.StatusDown] > 0, "bad")},
		{Label: p.T("tile.degraded"), Value: strconv.Itoa(counts[model.StatusDegraded]), Class: classIf(counts[model.StatusDegraded] > 0, "warn")},
		{Label: p.T("tile.open_incidents"), Value: strconv.Itoa(len(open))},
		{Label: p.T("tile.availability_30d"), Value: fleet},
	}
	s.render(w, r, http.StatusOK, "admin_overview", struct {
		page
		Tiles []statTile
		Rows  []overviewRow
	}{p, tiles, rows})
}

func classIf(cond bool, class string) string {
	if cond {
		return class
	}
	return ""
}

// summarizeLatency returns the check-weighted average and the largest P95.
func summarizeLatency(pts []store.LatencyPoint) (avg, p95 time.Duration) {
	var sum time.Duration
	var n int
	for _, pt := range pts {
		if pt.OK == 0 {
			continue
		}
		sum += pt.Avg * time.Duration(pt.OK)
		n += pt.OK
		p95 = max(p95, pt.P95)
	}
	if n > 0 {
		avg = sum / time.Duration(n)
	}
	return
}

type chartPoint struct {
	T   int64   `json:"t"`
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
	N   int     `json:"n"`
	OK  int     `json:"ok"`
}

type chartSpan struct {
	S int64 `json:"s"`
	E int64 `json:"e"`
}

type chartData struct {
	From     int64             `json:"from"`
	To       int64             `json:"to"`
	Bucket   int64             `json:"bucket"`
	Timezone string            `json:"tz"`
	Lang     string            `json:"lang"`
	Points   []chartPoint      `json:"points"`
	Outages  []chartSpan       `json:"outages"`
	Labels   map[string]string `json:"labels"`
}

var ranges = []struct {
	key    string
	d      time.Duration
	bucket time.Duration
}{
	{"24h", 24 * time.Hour, 10 * time.Minute},
	{"7d", 7 * 24 * time.Hour, time.Hour},
	{"30d", 30 * 24 * time.Hour, 6 * time.Hour},
	{"90d", 90 * 24 * time.Hour, 24 * time.Hour},
}

func pickRange(key string) (string, time.Duration, time.Duration) {
	for _, r := range ranges {
		if r.key == key {
			return r.key, r.d, r.bucket
		}
	}
	return ranges[0].key, ranges[0].d, ranges[0].bucket
}

// alignLocal rounds t down to a multiple of bucket counted from local
// midnight in loc.
func alignLocal(t time.Time, bucket time.Duration, loc *time.Location) time.Time {
	lt := t.In(loc)
	midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	if bucket >= 24*time.Hour {
		return midnight
	}
	return midnight.Add(lt.Sub(midnight) / bucket * bucket)
}

func (s *Server) latencyChart(ctx context.Context, id string, rangeKey string, lang string, loc *time.Location) (chartData, []store.LatencyPoint, error) {
	now := s.now()
	_, span, bucket := pickRange(rangeKey)
	// Align buckets to local time (midnight for daily buckets) so day labels
	// are right and the chart does not shift on every reload.
	from := alignLocal(now.Add(-span), bucket, loc)
	pts, err := s.store.LatencySeries(ctx, id, from, now, bucket)
	if err != nil {
		return chartData{}, nil, err
	}
	incs, err := s.store.Incidents(ctx, store.IncidentQuery{MonitorIDs: []string{id}, Since: from})
	if err != nil {
		return chartData{}, nil, err
	}
	d := chartData{From: from.UnixMilli(), To: now.UnixMilli(), Bucket: bucket.Milliseconds(), Timezone: loc.String(), Lang: lang,
		Labels: map[string]string{
			"avg": i18n.T(lang, "chart.avg"), "p95": i18n.T(lang, "chart.p95"), "outage": i18n.T(lang, "chart.outage"),
			"checks": i18n.T(lang, "chart.checks"), "nodata": i18n.T(lang, "chart.nodata"), "failed": i18n.T(lang, "chart.failed"),
			"empty": i18n.T(lang, "chart.empty"),
		}}
	if d.Timezone == "Local" {
		d.Timezone = ""
	}
	ms := func(t time.Duration) float64 { return float64(t.Microseconds()) / 1000 }
	for _, p := range pts {
		d.Points = append(d.Points, chartPoint{T: p.Start.UnixMilli(), Avg: ms(p.Avg), P95: ms(p.P95), Max: ms(p.Max), N: p.Checks, OK: p.OK})
	}
	for _, inc := range incs {
		end := inc.Ended
		if end.IsZero() {
			end = now
		}
		d.Outages = append(d.Outages, chartSpan{S: inc.Started.UnixMilli(), E: end.UnixMilli()})
	}
	return d, pts, nil
}

type checkRow struct {
	At         string
	Ago        string
	Status     model.Status
	StatusText string
	Latency    string
	Message    string
	Maint      bool
}

type phase struct {
	Class string
	Label string
	Value string
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.cfg()
	id := r.PathValue("id")
	st, ok := s.engine.State(id)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	m := st.Monitor
	now := s.now()
	p := s.basePage(r, m.Name)
	p.Nav = "monitors"
	rangeKey, _, _ := pickRange(r.URL.Query().Get("range"))
	failuresOnly := r.URL.Query().Get("failures") == "1"

	periods, err := s.store.Periods(ctx, now.AddDate(0, 0, -90), now, id)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	ps := periods[id]
	tiles := []statTile{
		{Label: p.T("tile.availability", "24h"), Value: availability(ps, now.Add(-24*time.Hour), now, now)},
		{Label: p.T("tile.availability", "7d"), Value: availability(ps, now.AddDate(0, 0, -7), now, now)},
		{Label: p.T("tile.availability", "30d"), Value: availability(ps, now.AddDate(0, 0, -30), now, now)},
		{Label: p.T("tile.availability", "90d"), Value: availability(ps, now.AddDate(0, 0, -90), now, now)},
	}

	var chart chartData
	var pts []store.LatencyPoint
	if m.Type != config.TypePush {
		chart, pts, err = s.latencyChart(ctx, id, rangeKey, p.Lang, p.Loc)
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		avg, p95 := summarizeLatency(pts)
		if avg > 0 {
			tiles = append(tiles, statTile{Label: p.T("tile.avg_latency", rangeKey), Value: i18n.Short(avg), Note: p.T("tile.p95", i18n.Short(p95))})
		}
	}
	if !st.CertExpiry.IsZero() {
		left := st.CertExpiry.Sub(now)
		t := statTile{Label: p.T("tile.cert"), Value: st.CertExpiry.In(p.Loc).Format("2006-01-02"), Note: i18n.In(p.Lang, left)}
		if left < cfg.CertExpiryWarnFor(m) {
			t.Class = "warn"
		}
		tiles = append(tiles, t)
	}

	checks, _ := s.store.Checks(ctx, store.CheckQuery{MonitorID: id, Limit: 20, FailuresOnly: failuresOnly})
	var rows []checkRow
	for _, c := range checks {
		rows = append(rows, checkRow{At: i18n.FormatTime(c.At, p.Loc), Ago: i18n.Ago(p.Lang, now.Sub(c.At)), Status: c.Status,
			StatusText: i18n.Status(p.Lang, c.Status), Latency: i18n.Short(c.Latency), Message: c.Message, Maint: c.Maintenance})
	}
	incs, _ := s.store.Incidents(ctx, store.IncidentQuery{MonitorIDs: []string{id}, Limit: 20})
	names := map[string]string{id: m.Name}
	incidentDays := groupIncidents(incs, names, p.Lang, p.Loc, now, true)

	var phases []phase
	var timing template.HTML
	if st.LastCheck != nil && st.LastCheck.Timing != nil {
		t := st.LastCheck.Timing
		timing = timingBar(t, st.LastCheck.Latency)
		for _, ph := range []struct {
			cls, key string
			d        time.Duration
		}{{"ph-dns", "phase.dns", t.DNS}, {"ph-connect", "phase.connect", t.Connect}, {"ph-tls", "phase.tls", t.TLS}, {"ph-ttfb", "phase.ttfb", t.FirstByte}} {
			if ph.d > 0 {
				phases = append(phases, phase{ph.cls, p.T(ph.key), i18n.Short(ph.d)})
			}
		}
		rest := st.LastCheck.Latency - t.DNS - t.Connect - t.TLS - t.FirstByte
		if rest > 0 {
			phases = append(phases, phase{"ph-rest", p.T("phase.rest"), i18n.Short(rest)})
		}
	}

	var pushURL string
	if m.Type == config.TypePush {
		base := cfg.Server.BaseURL
		if base == "" {
			scheme := "http"
			if s.secure(r) {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		pushURL = base + "/api/v1/push/" + st.PushToken
	}
	var notifiers []string
	notifiers = append(notifiers, cfg.NotifiersFor(m)...)
	confirm := cfg.ConfirmFor(m)
	var since, lastAt, lastAgo, next string
	if !st.Since.IsZero() {
		since = i18n.FormatTime(st.Since, p.Loc)
	}
	if st.LastCheck != nil {
		lastAt, lastAgo = i18n.FormatTime(st.LastCheck.At, p.Loc), i18n.Ago(p.Lang, now.Sub(st.LastCheck.At))
	}
	if !st.NextCheck.IsZero() && st.NextCheck.Sub(now) >= time.Second && st.Status != model.StatusPaused {
		next = i18n.In(p.Lang, st.NextCheck.Sub(now))
	}
	var rangeKeys []string
	for _, rg := range ranges {
		rangeKeys = append(rangeKeys, rg.key)
	}
	var tableRows []chartPoint
	for i := len(chart.Points) - 1; i >= 0; i-- {
		tableRows = append(tableRows, chart.Points[i])
	}
	s.render(w, r, http.StatusOK, "admin_monitor", struct {
		page
		M            config.Monitor
		S            engine.MonitorState
		StatusText   string
		Since        string
		LastAt       string
		LastAgo      string
		Next         string
		Tiles        []statTile
		Chart        chartData
		ChartRows    []chartPoint
		Range        string
		Ranges       []string
		Checks       []checkRow
		FailuresOnly bool
		IncidentDays []incidentDay
		Timing       template.HTML
		Phases       []phase
		PushURL      string
		Notifiers    []string
		Confirm      config.Confirm
		Public       bool
	}{p, m, st, i18n.Status(p.Lang, st.Status), since, lastAt, lastAgo, next, tiles, chart, tableRows, rangeKey, rangeKeys,
		rows, failuresOnly, incidentDays, timing, phases, pushURL, notifiers, confirm, isPublic(cfg, id)})
}

func (s *Server) handleMonitorAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "check":
		err = s.engine.CheckNow(id)
	case "pause":
		err = s.engine.SetPaused(r.Context(), id, true)
	case "resume":
		err = s.engine.SetPaused(r.Context(), id, false)
	default:
		s.jsonError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err == engine.ErrNotFound {
		s.jsonError(w, http.StatusNotFound, "monitor not found")
		return
	}
	if err != nil {
		s.log.Error("monitor action", "monitor", id, "action", r.PathValue("action"), "err", err)
		s.jsonError(w, http.StatusInternalServerError, "action failed")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	http.Redirect(w, r, "/admin/monitors/"+url.PathEscape(id), http.StatusSeeOther)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	now := s.now()
	p := s.basePage(r, "")
	p.Title, p.Nav = p.T("nav.incidents"), "incidents"
	openOnly := r.URL.Query().Get("state") == "open"
	incs, err := s.store.Incidents(r.Context(), store.IncidentQuery{OpenOnly: openOnly, Limit: 300})
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	names := map[string]string{}
	for _, m := range cfg.Monitors {
		names[m.ID] = m.Name
	}
	type row struct {
		incidentView
		Date       string
		Resolution string
		Known      bool
	}
	var rows []row
	var total time.Duration
	for _, inc := range incs {
		v := groupIncidents([]model.Incident{inc}, names, p.Lang, p.Loc, now, true)[0].Incidents[0]
		_, known := names[inc.MonitorID]
		res := inc.Resolution
		if key := "resolution." + strings.ReplaceAll(res, " ", "_"); res != "" && i18n.Has(key) {
			res = p.T(key)
		}
		rows = append(rows, row{incidentView: v, Date: inc.Started.In(p.Loc).Format("2006-01-02"), Resolution: res, Known: known})
		total += inc.Duration(now)
	}
	s.render(w, r, http.StatusOK, "admin_incidents", struct {
		page
		Rows     []row
		OpenOnly bool
		Total    string
	}{p, rows, openOnly, i18n.Duration(p.Lang, total.Round(time.Minute))})
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	p := s.basePage(r, "")
	p.Title, p.Nav = p.T("nav.notifications"), "notifications"
	logs, err := s.store.Notifications(r.Context(), 100)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	type logRow struct {
		store.NotificationLog
		At      string
		Monitor string
	}
	names := map[string]string{}
	for _, m := range cfg.Monitors {
		names[m.ID] = m.Name
	}
	var rows []logRow
	for _, l := range logs {
		name := names[l.MonitorID]
		if name == "" {
			name = l.MonitorID
		}
		rows = append(rows, logRow{l, i18n.FormatTime(l.Created, p.Loc), name})
	}
	type notifierRow struct {
		Name, Type, Events string
		Monitors           int
	}
	var ns []notifierRow
	for _, n := range cfg.Notifiers {
		events := strings.Join(n.Events, ", ")
		if events == "" {
			events = p.T("notifiers.default_events")
		}
		count := 0
		for _, m := range cfg.Monitors {
			for _, name := range cfg.NotifiersFor(m) {
				if name == n.Name {
					count++
				}
			}
		}
		ns = append(ns, notifierRow{n.Name, n.Type, events, count})
	}
	s.render(w, r, http.StatusOK, "admin_notifications", struct {
		page
		Notifiers []notifierRow
		Log       []logRow
		Result    string
		ResultOK  bool
	}{p, ns, rows, r.URL.Query().Get("result"), r.URL.Query().Get("ok") == "1"})
}

func (s *Server) handleNotifierTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p := s.basePage(r, "")
	q := url.Values{}
	if s.notify == nil {
		q.Set("result", "notifications are not available")
	} else if err := s.notify.Test(r.Context(), name); err != nil {
		q.Set("result", p.T("notifiers.test_failed", name, truncateText(err.Error(), 300)))
	} else {
		q.Set("result", p.T("notifiers.test_sent", name))
		q.Set("ok", "1")
	}
	http.Redirect(w, r, "/admin/notifications?"+q.Encode(), http.StatusSeeOther)
}

func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	p := s.basePage(r, "")
	p.Title, p.Nav = p.T("nav.settings"), "settings"
	type kv struct{ K, V string }
	var dbSize string
	if fi, err := os.Stat(cfg.Storage.Path); err == nil {
		dbSize = fmt.Sprintf("%.1f MB", float64(fi.Size())/1e6)
	}
	auth := p.T("settings.auth_password", cfg.Server.Admin.Username)
	if !s.passwordSet() {
		auth = p.T("settings.auth_local")
	}
	metrics := p.T("settings.off")
	if cfg.MetricsEnabled() {
		metrics = "/metrics"
		if cfg.Server.Metrics.Token != "" {
			metrics += " " + p.T("settings.metrics_token")
		}
	}
	tz := p.Loc.String()
	rows := []kv{
		{p.T("settings.version"), s.version},
		{p.T("settings.config"), orDash(cfg.Path)},
		{p.T("settings.database"), strings.TrimSpace(cfg.Storage.Path + " " + dbSize)},
		{p.T("settings.retention"), i18n.Duration(p.Lang, cfg.Storage.Retention.D())},
		{p.T("settings.timezone"), tz},
		{p.T("settings.auth"), auth},
		{p.T("settings.metrics"), metrics},
		{p.T("settings.base_url"), orDash(cfg.Server.BaseURL)},
		{p.T("settings.started"), i18n.FormatTime(s.started, p.Loc) + " (" + i18n.Ago(p.Lang, s.now().Sub(s.started)) + ")"},
	}
	type mw struct{ Name, Scope, When, State string }
	var windows []mw
	now := s.now()
	for _, m := range cfg.Maintenance {
		v := mw{Name: orDash(m.Name), Scope: p.T("settings.all_monitors")}
		if len(m.Monitors) > 0 {
			v.Scope = strings.Join(m.Monitors, ", ")
		}
		if st, e, ok := m.ActiveAt(now); ok {
			v.State, v.When = p.T("settings.active"), i18n.FormatTime(st, p.Loc)+" – "+i18n.FormatTime(e, p.Loc)
		} else if st, e, ok := m.Next(now, 400*24*time.Hour); ok {
			v.State, v.When = p.T("settings.upcoming"), i18n.FormatTime(st, p.Loc)+" – "+i18n.FormatTime(e, p.Loc)
		} else {
			v.State = p.T("settings.past")
		}
		windows = append(windows, v)
	}
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].State < windows[j].State })
	s.render(w, r, http.StatusOK, "admin_settings", struct {
		page
		Rows      []kv
		Windows   []mw
		CanReload bool
		Result    string
		ResultOK  bool
	}{p, rows, windows, s.reload != nil, r.URL.Query().Get("result"), r.URL.Query().Get("ok") == "1"})
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	p := s.basePage(r, "")
	if s.reload == nil {
		q.Set("result", "reload is not available")
	} else if err := s.reload(); err != nil {
		q.Set("result", p.T("settings.reload_failed", truncateText(err.Error(), 1500)))
	} else {
		q.Set("result", p.T("settings.reloaded"))
		q.Set("ok", "1")
	}
	http.Redirect(w, r, "/admin/settings?"+q.Encode(), http.StatusSeeOther)
}
