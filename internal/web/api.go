package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/uptime"
)

// API types. These are part of Pharos's public interface; see docs/api.md.

type apiUptime struct {
	Day   *float64 `json:"24h"`
	Week  *float64 `json:"7d"`
	Month *float64 `json:"30d"`
	Qtr   *float64 `json:"90d"`
}

type apiMonitor struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Status      model.Status `json:"status"`
	Since       *time.Time   `json:"since,omitempty"`
	Uptime      apiUptime    `json:"uptime"`
}

type apiGroup struct {
	Name     string       `json:"name,omitempty"`
	Monitors []apiMonitor `json:"monitors"`
}

type apiIncident struct {
	ID              int64      `json:"id"`
	MonitorID       string     `json:"monitor_id"`
	Monitor         string     `json:"monitor"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds int64      `json:"duration_seconds"`
	Cause           string     `json:"cause,omitempty"`
}

func ratioPtr(ps []model.Period, from, to, now time.Time) *float64 {
	r, ok := uptime.Summarize(ps, from, to, now).Ratio()
	if !ok {
		return nil
	}
	return &r
}

func apiUptimeFor(ps []model.Period, now time.Time) apiUptime {
	return apiUptime{
		Day:   ratioPtr(ps, now.Add(-24*time.Hour), now, now),
		Week:  ratioPtr(ps, now.AddDate(0, 0, -7), now, now),
		Month: ratioPtr(ps, now.AddDate(0, 0, -30), now, now),
		Qtr:   ratioPtr(ps, now.AddDate(0, 0, -90), now, now),
	}
}

func toAPIIncident(inc model.Incident, names map[string]string, now time.Time, showCause bool) apiIncident {
	a := apiIncident{ID: inc.ID, MonitorID: inc.MonitorID, Monitor: names[inc.MonitorID], StartedAt: inc.Started.UTC(),
		DurationSeconds: int64(inc.Duration(now).Seconds())}
	if !inc.Ongoing() {
		t := inc.Ended.UTC()
		a.EndedAt = &t
	}
	if showCause {
		a.Cause = inc.Cause
	}
	return a
}

// handleAPIStatus is the public, machine-readable status page.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	if !cfg.StatusPageEnabled() {
		s.jsonError(w, http.StatusNotFound, "status page disabled")
		return
	}
	ctx := r.Context()
	now := s.now()
	ids := publicIDs(cfg)
	periods, err := s.store.Periods(ctx, now.AddDate(0, 0, -90), now, ids...)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	states := map[string]engine.MonitorState{}
	for _, st := range s.engine.Snapshot() {
		states[st.Monitor.ID] = st
	}
	var groups []apiGroup
	var all []model.Status
	for _, g := range publicGroups(cfg) {
		ag := apiGroup{Name: g.Name, Monitors: []apiMonitor{}}
		for _, m := range g.Monitors {
			st := states[m.ID]
			all = append(all, st.Status)
			am := apiMonitor{ID: m.ID, Name: m.Name, Description: m.Description, Status: st.Status, Uptime: apiUptimeFor(periods[m.ID], now)}
			if !st.Since.IsZero() {
				t := st.Since.UTC()
				am.Since = &t
			}
			ag.Monitors = append(ag.Monitors, am)
		}
		groups = append(groups, ag)
	}
	names := map[string]string{}
	for _, m := range cfg.Monitors {
		names[m.ID] = m.Name
	}
	incs, err := s.store.Incidents(ctx, store.IncidentQuery{MonitorIDs: ids, Since: now.AddDate(0, 0, -cfg.StatusPage.IncidentDays)})
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	apiIncs := []apiIncident{}
	for _, inc := range incs {
		apiIncs = append(apiIncs, toAPIIncident(inc, names, now, cfg.StatusPage.ShowCauses))
	}
	maint := []engine.Window{}
	for _, mw := range s.engine.Maintenance(7 * 24 * time.Hour) {
		public := len(mw.Monitors) == 0
		for _, id := range mw.Monitors {
			public = public || isPublic(cfg, id)
		}
		if public {
			maint = append(maint, mw)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"title":       cfg.StatusPage.Title,
		"status":      overallStatus(all),
		"updated_at":  now.UTC(),
		"groups":      groups,
		"incidents":   apiIncs,
		"maintenance": maint,
	})
}

func (s *Server) handleAPIIncidents(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	if !cfg.StatusPageEnabled() {
		s.jsonError(w, http.StatusNotFound, "status page disabled")
		return
	}
	now := s.now()
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 400 {
		days = cfg.StatusPage.IncidentDays
	}
	ids := publicIDs(cfg)
	incs, err := s.store.Incidents(r.Context(), store.IncidentQuery{MonitorIDs: ids, Since: now.AddDate(0, 0, -days), Limit: 500})
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	names := map[string]string{}
	for _, m := range cfg.Monitors {
		names[m.ID] = m.Name
	}
	out := []apiIncident{}
	for _, inc := range incs {
		out = append(out, toAPIIncident(inc, names, now, cfg.StatusPage.ShowCauses))
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.writeJSON(w, http.StatusOK, map[string]any{"incidents": out})
}

// handlePush records a heartbeat. GET, HEAD and POST are all accepted so any
// tool can call it: `curl -fsS https://status.example.com/api/v1/push/TOKEN`.
// Optional parameters: status=down, msg=..., ms=<duration in ms>.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.jsonError(w, http.StatusMethodNotAllowed, "use GET or POST")
		return
	}
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		q = r.Form
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
	}
	ok := true
	switch strings.ToLower(q.Get("status")) {
	case "", "up", "ok", "success":
	case "down", "fail", "failed", "error":
		ok = false
	default:
		s.jsonError(w, http.StatusBadRequest, "status must be up or down")
		return
	}
	var latency time.Duration
	if v := q.Get("ms"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			s.jsonError(w, http.StatusBadRequest, "ms must be a non-negative number")
			return
		}
		latency = time.Duration(n * float64(time.Millisecond))
	}
	msg := q.Get("msg")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	id, err := s.engine.Push(r.PathValue("token"), ok, msg, latency)
	if err == engine.ErrNotFound {
		s.jsonError(w, http.StatusNotFound, "unknown push token")
		return
	}
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "monitor": id})
}

// Admin API

type apiAdminMonitor struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Target      string          `json:"target"`
	Public      bool            `json:"public"`
	Status      model.Status    `json:"status"`
	Since       *time.Time      `json:"since,omitempty"`
	Incident    *model.Incident `json:"incident,omitempty"`
	LastCheck   *apiCheck       `json:"last_check,omitempty"`
	CertExpiry  *time.Time      `json:"cert_expiry,omitempty"`
	NextCheck   *time.Time      `json:"next_check,omitempty"`
	Maintenance *engine.Window  `json:"maintenance,omitempty"`
	Uptime      apiUptime       `json:"uptime"`
}

type apiCheck struct {
	At          time.Time     `json:"at"`
	Status      model.Status  `json:"status"`
	LatencyMS   float64       `json:"latency_ms"`
	Message     string        `json:"message,omitempty"`
	Timing      *model.Timing `json:"timing_ns,omitempty"`
	Maintenance bool          `json:"maintenance,omitempty"`
}

func toAPICheck(c *model.Check) *apiCheck {
	if c == nil {
		return nil
	}
	return &apiCheck{At: c.At.UTC(), Status: c.Status, LatencyMS: float64(c.Latency.Microseconds()) / 1000, Message: c.Message, Timing: c.Timing, Maintenance: c.Maintenance}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func (s *Server) adminMonitor(st engine.MonitorState, cfg *config.Config, ps []model.Period, now time.Time) apiAdminMonitor {
	m := st.Monitor
	return apiAdminMonitor{ID: m.ID, Name: m.Name, Type: m.Type, Target: m.Target(), Public: isPublic(cfg, m.ID),
		Status: st.Status, Since: timePtr(st.Since), Incident: st.Incident, LastCheck: toAPICheck(st.LastCheck),
		CertExpiry: timePtr(st.CertExpiry), NextCheck: timePtr(st.NextCheck), Maintenance: st.Maintenance, Uptime: apiUptimeFor(ps, now)}
}

func (s *Server) handleAPIMonitors(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	now := s.now()
	periods, err := s.store.Periods(r.Context(), now.AddDate(0, 0, -90), now)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []apiAdminMonitor{}
	for _, st := range s.engine.Snapshot() {
		out = append(out, s.adminMonitor(st, cfg, periods[st.Monitor.ID], now))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"monitors": out})
}

func (s *Server) handleAPIMonitor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, ok := s.engine.State(id)
	if !ok {
		s.jsonError(w, http.StatusNotFound, "monitor not found")
		return
	}
	now := s.now()
	periods, err := s.store.Periods(r.Context(), now.AddDate(0, 0, -90), now, id)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, s.adminMonitor(st, s.cfg(), periods[id], now))
}

func (s *Server) handleAPIChecks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.engine.State(id); !ok {
		s.jsonError(w, http.StatusNotFound, "monitor not found")
		return
	}
	q := store.CheckQuery{MonitorID: id, FailuresOnly: r.URL.Query().Get("failures") == "1"}
	q.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "before must be RFC 3339")
			return
		}
		q.Before = t
	}
	checks, err := s.store.Checks(r.Context(), q)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []*apiCheck{}
	for i := range checks {
		out = append(out, toAPICheck(&checks[i]))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"checks": out})
}

func (s *Server) handleAPILatency(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.engine.State(id); !ok {
		s.jsonError(w, http.StatusNotFound, "monitor not found")
		return
	}
	cfg := s.cfg()
	d, _, err := s.latencyChart(r.Context(), id, r.URL.Query().Get("range"), cfg.StatusPage.Language, cfg.StatusPage.Location())
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// handleEvents streams live updates to the dashboard (Server-Sent Events).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer
	events, cancel := s.engine.Hub().Subscribe()
	defer cancel()
	fmt.Fprint(w, "retry: 5000\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload := map[string]any{"monitor": ev.Monitor.ID, "status": ev.Status, "at": ev.At.UTC()}
			if ev.Check != nil {
				payload["check"] = toAPICheck(ev.Check)
			}
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, b)
			flusher.Flush()
		}
	}
}
