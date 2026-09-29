package web

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/uptime"
)

// Overall states of the status page, worst first.
const (
	overallMajor       = "major"
	overallPartial     = "partial"
	overallDegraded    = "degraded"
	overallMaintenance = "maintenance"
	overallOK          = "operational"
	overallUnknown     = "unknown"
)

type publicGroup struct {
	Name     string
	Monitors []config.Monitor
}

// publicGroups returns what the status page shows. Monitors that are not in
// any configured group are private: dashboard only.
func publicGroups(cfg *config.Config) []publicGroup {
	if len(cfg.StatusPage.Groups) == 0 {
		return []publicGroup{{Monitors: cfg.Monitors}}
	}
	var out []publicGroup
	for _, g := range cfg.StatusPage.Groups {
		pg := publicGroup{Name: g.Name}
		for _, id := range g.Monitors {
			if m, ok := cfg.MonitorByID(id); ok {
				pg.Monitors = append(pg.Monitors, m)
			}
		}
		out = append(out, pg)
	}
	return out
}

// publicSet returns the ids shown on the status page. Build it once per
// request: checking monitors one by one against the groups is quadratic.
func publicSet(cfg *config.Config) map[string]bool {
	set := map[string]bool{}
	for _, g := range publicGroups(cfg) {
		for _, m := range g.Monitors {
			set[m.ID] = true
		}
	}
	return set
}

func isPublic(cfg *config.Config, id string) bool { return publicSet(cfg)[id] }

// publicIDs lists the public monitors. Callers must treat an empty list as
// "nothing is public": the store reads an empty filter as "everything".
func publicIDs(cfg *config.Config) []string {
	var ids []string
	for _, g := range publicGroups(cfg) {
		for _, m := range g.Monitors {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// publicMonitorIDs keeps only public ids from a list, e.g. the monitors a
// maintenance window covers.
func publicMonitorIDs(set map[string]bool, ids []string) []string {
	var out []string
	for _, id := range ids {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
}

// overallStatus summarizes public monitor states.
func overallStatus(states []model.Status) string {
	if len(states) == 0 {
		return overallUnknown
	}
	var down, degraded, maint, unknown int
	for _, s := range states {
		switch s {
		case model.StatusDown:
			down++
		case model.StatusDegraded:
			degraded++
		case model.StatusMaintenance:
			maint++
		case model.StatusUnknown:
			unknown++
		}
	}
	switch {
	case down > 0 && down == len(states):
		return overallMajor
	case down > 0:
		return overallPartial
	case degraded > 0:
		return overallDegraded
	case maint > 0:
		return overallMaintenance
	case unknown == len(states):
		return overallUnknown
	}
	return overallOK
}

type statusMonitor struct {
	ID          string
	Name        string
	Description string
	Status      model.Status
	StatusText  string
	Bars        template.HTML
	BarsShort   template.HTML
	Uptime      string
	HasUptime   bool
}

type statusGroup struct {
	Name     string
	Monitors []statusMonitor
}

type incidentView struct {
	ID        int64
	MonitorID string
	Monitor   string
	Start     string
	End       string
	Duration  string
	Cause     string
	Ongoing   bool
}

type incidentDay struct {
	Date      string
	Incidents []incidentView
}

type maintView struct {
	Name     string
	When     string
	Active   bool
	Affected string
}

type statusData struct {
	page
	Overall        string
	OverallText    string
	Description    string
	Groups         []statusGroup
	IncidentDays   []incidentDay
	Maintenance    []maintView
	HistoryDays    int
	IncidentWindow int
	Updated        string
}

// statusPageData builds everything the public page shows.
func (s *Server) statusPageData(ctx context.Context, r *http.Request) (*statusData, error) {
	cfg := s.cfg()
	sp := cfg.StatusPage
	now := s.now()
	loc := sp.Location()
	p := s.basePage(r, "") // the site title alone: no "Title · Title"
	ids := publicIDs(cfg)

	from := now.AddDate(0, 0, -sp.HistoryDays-1)
	periods := map[string][]model.Period{}
	var incidents []model.Incident
	if len(ids) > 0 {
		var err error
		if periods, err = s.store.Periods(ctx, from, now, ids...); err != nil {
			return nil, err
		}
		incSince := now.AddDate(0, 0, -max(sp.HistoryDays, sp.IncidentDays))
		if incidents, err = s.store.Incidents(ctx, store.IncidentQuery{MonitorIDs: ids, Since: incSince}); err != nil {
			return nil, err
		}
	}
	byMon := map[string][]model.Incident{}
	for _, inc := range incidents {
		byMon[inc.MonitorID] = append(byMon[inc.MonitorID], inc)
	}
	states := map[string]engine.MonitorState{}
	for _, st := range s.engine.Snapshot() {
		states[st.Monitor.ID] = st
	}

	d := &statusData{page: p, Description: sp.Description, HistoryDays: sp.HistoryDays, IncidentWindow: sp.IncidentDays,
		Updated: now.In(loc).Format("15:04")}
	var all []model.Status
	for _, g := range publicGroups(cfg) {
		sg := statusGroup{Name: g.Name}
		for _, m := range g.Monitors {
			st := states[m.ID]
			all = append(all, st.Status)
			days := uptime.Days(periods[m.ID], byMon[m.ID], sp.HistoryDays, loc, now)
			sum := uptime.Summarize(periods[m.ID], now.AddDate(0, 0, -sp.HistoryDays), now, now)
			ratio, ok := sum.Ratio()
			short := days
			if len(short) > 30 {
				short = short[len(short)-30:]
			}
			sg.Monitors = append(sg.Monitors, statusMonitor{
				ID: m.ID, Name: m.Name, Description: m.Description,
				Status: st.Status, StatusText: i18n.Status(p.Lang, st.Status),
				Bars: uptimeBars(days, p.Lang, loc), BarsShort: uptimeBars(short, p.Lang, loc),
				Uptime: i18n.Percent(ratio), HasUptime: ok,
			})
		}
		d.Groups = append(d.Groups, sg)
	}
	d.Overall = overallStatus(all)
	d.OverallText = p.T("overall." + d.Overall)

	names := map[string]string{}
	for _, m := range cfg.Monitors {
		names[m.ID] = m.Name
	}
	cut := now.AddDate(0, 0, -sp.IncidentDays)
	var recent []model.Incident
	for _, inc := range incidents {
		if inc.Ongoing() || inc.Ended.After(cut) {
			recent = append(recent, inc)
		}
	}
	d.IncidentDays = groupIncidents(recent, names, p.Lang, loc, now, sp.ShowCauses)

	public := publicSet(cfg)
	for _, w := range s.engine.Maintenance(7 * 24 * time.Hour) {
		mv := maintView{Name: w.Name, Active: !now.Before(w.Start)}
		if mv.Name == "" {
			mv.Name = p.T("maint.scheduled")
		}
		mv.When = i18n.FormatTime(w.Start, loc) + " – " + w.End.In(loc).Format("15:04")
		if w.End.Sub(w.Start) >= 24*time.Hour {
			mv.When = i18n.FormatTime(w.Start, loc) + " – " + i18n.FormatTime(w.End, loc)
		}
		var affected []string
		for _, id := range publicMonitorIDs(public, w.Monitors) {
			affected = append(affected, names[id])
		}
		if len(w.Monitors) > 0 && len(affected) == 0 {
			continue // only affects private monitors
		}
		if len(affected) > 0 {
			mv.Affected = joinNames(affected, p.Lang)
		}
		d.Maintenance = append(d.Maintenance, mv)
	}
	return d, nil
}

func joinNames(names []string, lang string) string {
	sep := ", "
	if lang == i18n.TW {
		sep = "、"
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += sep
		}
		out += n
	}
	return out
}

// groupIncidents buckets incidents by the local day they started, newest first.
func groupIncidents(incs []model.Incident, names map[string]string, lang string, loc *time.Location, now time.Time, showCause bool) []incidentDay {
	sort.Slice(incs, func(i, j int) bool { return incs[i].Started.After(incs[j].Started) })
	var out []incidentDay
	for _, inc := range incs {
		v := incidentView{
			ID: inc.ID, MonitorID: inc.MonitorID, Monitor: names[inc.MonitorID],
			Start: inc.Started.In(loc).Format("15:04"), Ongoing: inc.Ongoing(),
			Duration: i18n.Duration(lang, inc.Duration(now).Round(time.Minute)),
		}
		if v.Monitor == "" {
			v.Monitor = inc.MonitorID
		}
		if inc.Duration(now) < time.Minute {
			v.Duration = i18n.Duration(lang, inc.Duration(now).Round(time.Second))
		}
		if !inc.Ongoing() {
			v.End = inc.Ended.In(loc).Format("15:04")
			if inc.Ended.In(loc).YearDay() != inc.Started.In(loc).YearDay() {
				v.End = inc.Ended.In(loc).Format("01-02 15:04")
			}
		}
		if showCause {
			v.Cause = inc.Cause
		}
		date := inc.Started.In(loc).Format("2006-01-02")
		if n := len(out); n > 0 && out[n-1].Date == date {
			out[n-1].Incidents = append(out[n-1].Incidents, v)
		} else {
			out = append(out, incidentDay{Date: date, Incidents: []incidentView{v}})
		}
	}
	return out
}

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg().StatusPageEnabled() {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	d, err := s.statusPageData(r.Context(), r)
	if err != nil {
		s.log.Error("status page", "err", err)
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	s.render(w, r, http.StatusOK, "status", d)
}

type historyRow struct {
	Date      string
	Level     string
	LevelText string
	Uptime    string
	Downtime  string
	Incidents int
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.renderHistory(w, r, false)
}

func (s *Server) renderHistory(w http.ResponseWriter, r *http.Request, static bool) {
	cfg := s.cfg()
	id := r.PathValue("id")
	m, ok := cfg.MonitorByID(id)
	_, admin := s.currentSession(r)
	if !ok || (!isPublic(cfg, id) && !admin) || (!cfg.StatusPageEnabled() && !admin) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	now := s.now()
	sp := cfg.StatusPage
	loc := sp.Location()
	ctx := r.Context()
	periods, err := s.store.Periods(ctx, now.AddDate(0, 0, -sp.HistoryDays-1), now, id)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	incs, err := s.store.Incidents(ctx, store.IncidentQuery{MonitorIDs: []string{id}, Since: now.AddDate(0, 0, -sp.HistoryDays)})
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	p := s.basePage(r, m.Name)
	if static {
		p.Static, p.HomeHref, p.root = true, "../index.html", "../"
	}
	days := uptime.Days(periods[id], incs, sp.HistoryDays, loc, now)
	var rows []historyRow
	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		ratio, ok := d.Summary.Ratio()
		row := historyRow{Date: d.Date.Format("2006-01-02"), Level: d.Level.String(), LevelText: p.T("level." + d.Level.String()),
			Uptime: "–", Downtime: downText(p.Lang, d.Summary.Down), Incidents: d.Incidents}
		if ok {
			row.Uptime = i18n.Percent(ratio)
		}
		rows = append(rows, row)
	}
	sum := uptime.Summarize(periods[id], now.AddDate(0, 0, -sp.HistoryDays), now, now)
	ratio, hasRatio := sum.Ratio()
	st, _ := s.engine.State(id)
	names := map[string]string{id: m.Name}
	s.render(w, r, http.StatusOK, "history", struct {
		page
		Monitor      config.Monitor
		Status       model.Status
		StatusText   string
		Bars         template.HTML
		Rows         []historyRow
		Uptime       string
		HasUptime    bool
		HistoryDays  int
		IncidentDays []incidentDay
	}{p, m, st.Status, i18n.Status(p.Lang, st.Status), uptimeBars(days, p.Lang, loc), rows, i18n.Percent(ratio), hasRatio,
		sp.HistoryDays, groupIncidents(incs, names, p.Lang, loc, now, sp.ShowCauses || admin)})
}
