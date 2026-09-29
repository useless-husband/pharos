package web

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/uptime"
)

// handleBadge serves README badges for public monitors:
//
//	/badge/{id}/status.svg
//	/badge/{id}/uptime.svg?window=30d   (24h, 7d, 30d, 90d)
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	id := r.PathValue("id")
	m, ok := cfg.MonitorByID(id)
	if !ok || !isPublic(cfg, id) || !cfg.StatusPageEnabled() {
		http.NotFound(w, r)
		return
	}
	var label, value, color string
	switch r.PathValue("kind") {
	case "status.svg":
		st, _ := s.engine.State(id)
		label, value = strings.ToLower(m.Name), i18n.T("en", "badge."+st.Status.String())
		color = badgeColor(st.Status)
	case "uptime.svg":
		win := r.URL.Query().Get("window")
		days := map[string]int{"24h": 1, "7d": 7, "30d": 30, "90d": 90}[win]
		if days == 0 {
			win, days = "30d", 30
		}
		now := s.now()
		from := now.AddDate(0, 0, -days)
		periods, err := s.store.Periods(r.Context(), from, now, id)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		label = "uptime " + win
		ratio, has := uptime.Summarize(periods[id], from, now, now).Ratio()
		switch {
		case !has:
			value, color = "no data", "#6e6e6e"
		case ratio >= 0.999:
			value, color = i18n.Percent(ratio), "#2b7a3d"
		case ratio >= 0.99:
			value, color = i18n.Percent(ratio), "#8a6400"
		default:
			value, color = i18n.Percent(ratio), "#b42318"
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	// Badges are embedded by GitHub's image proxy; keep its cache short.
	w.Header().Set("Cache-Control", "max-age=60, s-maxage=60")
	_, _ = w.Write(badgeSVG(label, value, color))
}

func badgeColor(s model.Status) string {
	switch s {
	case model.StatusUp:
		return "#2b7a3d"
	case model.StatusDegraded:
		return "#8a6400"
	case model.StatusDown:
		return "#b42318"
	case model.StatusMaintenance:
		return "#1f63b8"
	}
	return "#6e6e6e"
}

// textWidth approximates Verdana 11px, the de facto badge font.
func textWidth(s string) float64 {
	w := 0.0
	for _, r := range s {
		switch {
		case r >= 0x2E80: // CJK: full width
			w += 11
		case strings.ContainsRune("il.:|!'", r):
			w += 3.5
		case strings.ContainsRune("mwMW%", r):
			w += 10
		case r >= 'A' && r <= 'Z':
			w += 7.5
		default:
			w += 6.6
		}
	}
	return w
}

func badgeSVG(label, value, color string) []byte {
	lw := textWidth(label) + 12
	vw := textWidth(value) + 12
	total := lw + vw
	esc := template.HTMLEscapeString
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="20" role="img" aria-label="%s: %s">
<title>%s: %s</title>
<clipPath id="r"><rect width="%.0f" height="20" rx="3"/></clipPath>
<g clip-path="url(#r)"><rect width="%.0f" height="20" fill="#454545"/><rect x="%.0f" width="%.0f" height="20" fill="%s"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11">
<text x="%.1f" y="14">%s</text><text x="%.1f" y="14">%s</text></g>
</svg>`, total, esc(label), esc(value), esc(label), esc(value), total, lw, lw, vw, color, lw/2, esc(label), lw+vw/2, esc(value)))
}

// handleMetrics exposes Prometheus metrics in the text exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg()
	if !cfg.MetricsEnabled() {
		http.NotFound(w, r)
		return
	}
	if tok := cfg.Server.Metrics.Token; tok != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pharos"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	now := s.now()
	periods, err := s.store.Periods(r.Context(), now.AddDate(0, 0, -30), now)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	help := func(name, typ, text string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
	}
	states := s.engine.Snapshot()
	sort.Slice(states, func(i, j int) bool { return states[i].Monitor.ID < states[j].Monitor.ID })
	// Label values are quoted by hand: Go's %q escapes differently from
	// the Prometheus text format.
	q := func(v string) string { return `"` + promEscape(v) + `"` }
	lbl := func(m config.Monitor) string {
		return "monitor=" + q(m.ID) + ",name=" + q(m.Name) + ",type=" + q(m.Type)
	}

	help("pharos_build_info", "gauge", "Build information.")
	fmt.Fprintf(&b, "pharos_build_info{version=%s} 1\n", q(s.version))

	help("pharos_monitor_up", "gauge", "1 if the monitor is up or degraded, 0 if down; absent while unknown, paused or in maintenance.")
	for _, st := range states {
		switch st.Status {
		case model.StatusUp, model.StatusDegraded:
			fmt.Fprintf(&b, "pharos_monitor_up{%s} 1\n", lbl(st.Monitor))
		case model.StatusDown:
			fmt.Fprintf(&b, "pharos_monitor_up{%s} 0\n", lbl(st.Monitor))
		}
	}
	help("pharos_monitor_status", "gauge", "Current status of each monitor, one series per possible status.")
	for _, st := range states {
		for _, s2 := range []model.Status{model.StatusUnknown, model.StatusUp, model.StatusDegraded, model.StatusDown, model.StatusMaintenance, model.StatusPaused} {
			v := 0
			if st.Status == s2 {
				v = 1
			}
			fmt.Fprintf(&b, "pharos_monitor_status{%s,status=%s} %d\n", lbl(st.Monitor), q(s2.String()), v)
		}
	}
	help("pharos_check_duration_seconds", "gauge", "Duration of the most recent check.")
	for _, st := range states {
		if st.LastCheck != nil && st.Monitor.Type != config.TypePush {
			fmt.Fprintf(&b, "pharos_check_duration_seconds{%s} %g\n", lbl(st.Monitor), st.LastCheck.Latency.Seconds())
		}
	}
	help("pharos_last_check_timestamp_seconds", "gauge", "Unix time of the most recent check or heartbeat.")
	for _, st := range states {
		if st.LastCheck != nil {
			fmt.Fprintf(&b, "pharos_last_check_timestamp_seconds{%s} %d\n", lbl(st.Monitor), st.LastCheck.At.Unix())
		}
	}
	help("pharos_cert_expiry_timestamp_seconds", "gauge", "Unix time when the monitored TLS certificate expires.")
	for _, st := range states {
		if !st.CertExpiry.IsZero() {
			fmt.Fprintf(&b, "pharos_cert_expiry_timestamp_seconds{%s} %d\n", lbl(st.Monitor), st.CertExpiry.Unix())
		}
	}
	help("pharos_availability_ratio", "gauge", "Time-weighted availability over a window (maintenance excluded).")
	for _, st := range states {
		for _, w := range []struct {
			k string
			d time.Duration
		}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}} {
			if r, ok := uptime.Summarize(periods[st.Monitor.ID], now.Add(-w.d), now, now).Ratio(); ok {
				fmt.Fprintf(&b, "pharos_availability_ratio{%s,window=%s} %g\n", lbl(st.Monitor), q(w.k), r)
			}
		}
	}
	help("pharos_incident_open", "gauge", "1 while the monitor has an open incident.")
	for _, st := range states {
		v := 0
		if st.Incident != nil {
			v = 1
		}
		fmt.Fprintf(&b, "pharos_incident_open{%s} %d\n", lbl(st.Monitor), v)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}
