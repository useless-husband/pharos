// Package i18n holds user-facing text in English and Traditional Chinese.
package i18n

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

// Supported languages.
const (
	EN = "en"
	TW = "zh-TW"
)

// T returns the translation of key, formatted with args. Unknown keys fall
// back to English, then to the key itself, so a missing string is visible
// but never breaks a page.
func T(lang, key string, args ...any) string {
	s, ok := catalog[lang][key]
	if !ok {
		s, ok = catalog[EN][key]
	}
	if !ok {
		s = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// Has reports whether key exists in English (every key must).
func Has(key string) bool { _, ok := catalog[EN][key]; return ok }

// Status returns a status label.
func Status(lang string, s model.Status) string { return T(lang, "status."+s.String()) }

// Duration renders a duration for people: "45 seconds", "2 hours 5 minutes",
// "3 days 4 hours". At most two units are shown.
func Duration(lang string, d time.Duration) string {
	if d < 0 {
		d = -d
	}
	type unit struct {
		d        time.Duration
		one, few string
	}
	units := []unit{
		{24 * time.Hour, "day", "days"},
		{time.Hour, "hour", "hours"},
		{time.Minute, "minute", "minutes"},
		{time.Second, "second", "seconds"},
	}
	if d < time.Second {
		return T(lang, "dur.second.few", 0)
	}
	var parts []string
	for _, u := range units {
		if d >= u.d && len(parts) < 2 {
			n := int(d / u.d)
			d -= time.Duration(n) * u.d
			key := "dur." + u.one + ".few"
			if n == 1 {
				key = "dur." + u.one + ".one"
			}
			parts = append(parts, T(lang, key, n))
		} else if len(parts) > 0 {
			break // "2 hours 5 minutes", never "2 hours 0 minutes 5 seconds"
		}
	}
	return strings.Join(parts, " ")
}

// Short renders a compact duration for tables: "450ms", "1.2s", "3m", "5h", "2d".
func Short(d time.Duration) string {
	switch {
	case d <= 0:
		return "0ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Percent formats an availability ratio with enough precision to be
// meaningful: 100%, 99.98%, 97.3%.
func Percent(r float64) string {
	if r >= 1 {
		return "100%"
	}
	// Truncate rather than round: 99.996% must never display as 100%,
	// which would hide an outage.
	p := r * 100
	if p >= 99 {
		return fmt.Sprintf("%.2f%%", math.Floor(p*100)/100)
	}
	return fmt.Sprintf("%.1f%%", math.Floor(p*10)/10)
}

// Ago renders "5 minutes ago" style relative times.
func Ago(lang string, d time.Duration) string {
	if d < 30*time.Second {
		return T(lang, "time.just_now")
	}
	if d < time.Minute {
		d = time.Minute
	}
	// Show a single unit for relative times.
	var s string
	switch {
	case d < time.Hour:
		s = plural(lang, "minute", int(d/time.Minute))
	case d < 48*time.Hour:
		s = plural(lang, "hour", int(d/time.Hour))
	default:
		s = plural(lang, "day", int(d/(24*time.Hour)))
	}
	return T(lang, "time.ago", s)
}

// In renders "in 5 minutes".
func In(lang string, d time.Duration) string {
	if d < time.Minute {
		return T(lang, "time.in", plural(lang, "second", max(0, int(d/time.Second))))
	}
	if d < time.Hour {
		return T(lang, "time.in", plural(lang, "minute", int(d/time.Minute)))
	}
	if d < 48*time.Hour {
		return T(lang, "time.in", plural(lang, "hour", int(d/time.Hour)))
	}
	return T(lang, "time.in", plural(lang, "day", int(d/(24*time.Hour))))
}

func plural(lang, unit string, n int) string {
	if n == 1 {
		return T(lang, "dur."+unit+".one", n)
	}
	return T(lang, "dur."+unit+".few", n)
}

// FormatTime renders an absolute time in loc: "2026-09-29 14:05 UTC+8".
func FormatTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(loc)
	return t.Format("2006-01-02 15:04 ") + zoneLabel(t)
}

// zoneLabel names t's UTC offset, "UTC+8" or "UTC-3:30". Zone
// abbreviations are ambiguous (CST is China, Cuba and US Central time) and
// many zones have none.
func zoneLabel(t time.Time) string {
	_, off := t.Zone()
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	if m := off % 3600 / 60; m != 0 {
		return fmt.Sprintf("UTC%s%d:%02d", sign, off/3600, m)
	}
	return fmt.Sprintf("UTC%s%d", sign, off/3600)
}
