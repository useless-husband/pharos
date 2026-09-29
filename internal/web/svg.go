package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/uptime"
)

// These SVGs contain no text, so they scale cleanly with their container.
// Colors come from CSS classes, which switch with the color scheme.

// uptimeBars draws one bar per day. Each bar carries its figures in data
// attributes for the hover tooltip and a <title> for browsers without JS.
func uptimeBars(days []uptime.Day, lang string, loc *time.Location) template.HTML {
	const w, gap, h = 4, 2, 32
	n := len(days)
	var b strings.Builder
	label := i18n.T(lang, "bars.aria", n)
	fmt.Fprintf(&b, `<svg class="bars" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="%s">`,
		n*(w+gap)-gap, h, template.HTMLEscapeString(label))
	for i, d := range days {
		ratio, ok := d.Summary.Ratio()
		pct := "–"
		if ok {
			pct = i18n.Percent(ratio)
		}
		date := d.Date.In(loc).Format("2006-01-02")
		title := dayTitle(lang, d, pct)
		fmt.Fprintf(&b, `<rect class="lv-%s" x="%d" y="0" width="%d" height="%d" rx="1" data-date="%s" data-level="%s" data-uptime="%s" data-down="%s" data-inc="%d"><title>%s</title></rect>`,
			d.Level, i*(w+gap), w, h, date, template.HTMLEscapeString(i18n.T(lang, "level."+d.Level.String())),
			pct, template.HTMLEscapeString(downText(lang, d.Summary.Down)), d.Incidents, template.HTMLEscapeString(title))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func downText(lang string, d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return i18n.Duration(lang, d.Round(time.Minute))
}

func dayTitle(lang string, d uptime.Day, pct string) string {
	s := d.Date.Format("2006-01-02") + " · " + i18n.T(lang, "level."+d.Level.String())
	if d.Level != uptime.LevelNoData {
		s += " · " + pct
	}
	if d.Summary.Down > 0 {
		s += " · " + i18n.T(lang, "bars.down_for", downText(lang, d.Summary.Down))
	}
	return s
}

// sparkline draws average latency as a thin line with a light wash below.
// Buckets without successful checks break the line.
func sparkline(points []store.LatencyPoint) template.HTML {
	const w, h, pad = 120.0, 28.0, 2.0
	if len(points) < 2 {
		return template.HTML(`<svg class="spark" viewBox="0 0 120 28" aria-hidden="true"></svg>`)
	}
	var maxV time.Duration
	for _, p := range points {
		maxV = max(maxV, p.Avg)
	}
	if maxV <= 0 {
		maxV = 1
	}
	step := w / float64(len(points)-1)
	var line, area strings.Builder
	var segStart float64 = -1
	var lastX float64
	flushArea := func() {
		if segStart >= 0 {
			fmt.Fprintf(&area, "L%.1f,%.1fL%.1f,%.1fZ", lastX, h, segStart, h)
		}
		segStart = -1
	}
	for i, p := range points {
		x := float64(i) * step
		if p.OK == 0 {
			flushArea()
			continue
		}
		y := h - pad - (h-2*pad)*float64(p.Avg)/float64(maxV)
		if segStart < 0 {
			fmt.Fprintf(&line, "M%.1f,%.1f", x, y)
			fmt.Fprintf(&area, "M%.1f,%.1fL%.1f,%.1f", x, h, x, y)
			segStart = x
		} else {
			fmt.Fprintf(&line, "L%.1f,%.1f", x, y)
			fmt.Fprintf(&area, "L%.1f,%.1f", x, y)
		}
		lastX = x
	}
	flushArea()
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 120 28" preserveAspectRatio="none" aria-hidden="true"><path class="spark-area" d="%s"/><path class="spark-line" d="%s" vector-effect="non-scaling-stroke"/></svg>`, area.String(), line.String()))
}

// timingBar draws the phases of the last check as one stacked bar with
// 2px surface gaps between segments. Labels live in the HTML legend.
func timingBar(t *model.Timing, total time.Duration) template.HTML {
	if t == nil || total <= 0 {
		return ""
	}
	phases := []struct {
		cls string
		d   time.Duration
	}{
		{"ph-dns", t.DNS}, {"ph-connect", t.Connect}, {"ph-tls", t.TLS}, {"ph-ttfb", t.FirstByte},
	}
	var sum time.Duration
	for _, p := range phases {
		sum += p.d
	}
	rest := total - sum
	if rest > 0 {
		phases = append(phases, struct {
			cls string
			d   time.Duration
		}{"ph-rest", rest})
	}
	const W, H, gap = 600.0, 12.0, 2.0
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="timing" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true">`, W, H)
	x := 0.0
	visible := 0
	for _, p := range phases {
		if p.d > 0 {
			visible++
		}
	}
	avail := W - gap*float64(max(visible-1, 0))
	for _, p := range phases {
		if p.d <= 0 {
			continue
		}
		wd := math.Max(avail*float64(p.d)/float64(total), 1)
		fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="0" width="%.1f" height="%.0f" rx="2"/>`, p.cls, x, wd, H)
		x += wd + gap
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
