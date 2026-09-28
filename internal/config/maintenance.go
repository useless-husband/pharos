package config

import (
	"fmt"
	"strings"
	"time"
)

type window struct {
	oneOff     bool
	start, end time.Time
	days       [7]bool
	at         time.Duration // offset from local midnight
	dur        time.Duration
	loc        *time.Location
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

var timeLayouts = []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}

func parseTimeIn(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use RFC 3339 or \"2006-01-02 15:04\")", s)
}

// compile validates the maintenance definition and prepares it for queries.
func (m *Maintenance) compile(defaultLoc *time.Location) error {
	loc := defaultLoc
	if m.Timezone != "" {
		l, err := time.LoadLocation(m.Timezone)
		if err != nil {
			return fmt.Errorf("unknown timezone %q", m.Timezone)
		}
		loc = l
	}
	w := window{loc: loc}
	oneOff := m.Start != "" || m.End != ""
	recurring := len(m.Days) > 0 || m.At != "" || m.Duration != 0
	switch {
	case oneOff && recurring:
		return fmt.Errorf("use either start/end or days/at/duration, not both")
	case oneOff:
		if m.Start == "" || m.End == "" {
			return fmt.Errorf("both start and end are required")
		}
		s, err := parseTimeIn(m.Start, loc)
		if err != nil {
			return err
		}
		e, err := parseTimeIn(m.End, loc)
		if err != nil {
			return err
		}
		if !e.After(s) {
			return fmt.Errorf("end must be after start")
		}
		w.oneOff, w.start, w.end = true, s, e
	case recurring:
		if len(m.Days) == 0 || m.At == "" || m.Duration == 0 {
			return fmt.Errorf("recurring maintenance needs days, at and duration")
		}
		for _, d := range m.Days {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "daily" || d == "every" {
				for i := range w.days {
					w.days[i] = true
				}
				continue
			}
			wd, ok := weekdays[d[:min(3, len(d))]]
			if !ok {
				return fmt.Errorf("unknown day %q (use mon..sun or daily)", d)
			}
			w.days[wd] = true
		}
		hm, err := time.Parse("15:04", m.At)
		if err != nil {
			return fmt.Errorf("at must be HH:MM, got %q", m.At)
		}
		w.at = time.Duration(hm.Hour())*time.Hour + time.Duration(hm.Minute())*time.Minute
		w.dur = m.Duration.D()
		if w.dur > 7*24*time.Hour {
			return fmt.Errorf("duration must be at most 7d")
		}
	default:
		return fmt.Errorf("set start/end for a one-off window or days/at/duration for a recurring one")
	}
	m.window = w
	return nil
}

// Covers reports whether the window applies to a monitor.
func (m Maintenance) Covers(monitorID string) bool {
	if len(m.Monitors) == 0 {
		return true
	}
	for _, id := range m.Monitors {
		if id == monitorID {
			return true
		}
	}
	return false
}

// ActiveAt returns the occurrence of the window that contains t, if any.
func (m Maintenance) ActiveAt(t time.Time) (start, end time.Time, ok bool) {
	w := m.window
	if w.oneOff {
		return w.start, w.end, !t.Before(w.start) && t.Before(w.end)
	}
	if w.loc == nil {
		return
	}
	lt := t.In(w.loc)
	// An occurrence that started up to 7 days ago may still be running.
	for back := 0; back <= 7; back++ {
		day := time.Date(lt.Year(), lt.Month(), lt.Day()-back, 0, 0, 0, 0, w.loc)
		if !w.days[day.Weekday()] {
			continue
		}
		s := addClock(day, w.at)
		e := s.Add(w.dur)
		if !t.Before(s) && t.Before(e) {
			return s, e, true
		}
	}
	return
}

// Next returns the first occurrence that starts after t, looking up to
// `within` ahead.
func (m Maintenance) Next(t time.Time, within time.Duration) (start, end time.Time, ok bool) {
	w := m.window
	limit := t.Add(within)
	if w.oneOff {
		if w.start.After(t) && !w.start.After(limit) {
			return w.start, w.end, true
		}
		return
	}
	if w.loc == nil {
		return
	}
	lt := t.In(w.loc)
	days := int(within/(24*time.Hour)) + 2
	for fwd := 0; fwd <= days; fwd++ {
		day := time.Date(lt.Year(), lt.Month(), lt.Day()+fwd, 0, 0, 0, 0, w.loc)
		if !w.days[day.Weekday()] {
			continue
		}
		s := addClock(day, w.at)
		if s.After(t) && !s.After(limit) {
			return s, s.Add(w.dur), true
		}
	}
	return
}

// addClock adds a wall-clock offset to a local midnight, so "02:00" stays
// 02:00 across daylight-saving changes.
func addClock(midnight time.Time, d time.Duration) time.Time {
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return time.Date(midnight.Year(), midnight.Month(), midnight.Day(), h, m, 0, 0, midnight.Location())
}

// CompileMaintenance validates a maintenance window that was built in code
// rather than loaded from a file, and prepares it for ActiveAt and Next.
func CompileMaintenance(m *Maintenance, loc *time.Location) error { return m.compile(loc) }
