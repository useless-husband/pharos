// Package uptime turns status periods into availability figures.
//
// Availability is time-weighted over confirmed status: the share of counted
// time (up, degraded or down) during which a monitor was up or degraded.
// Maintenance, paused and unknown time is excluded from both sides, so a
// planned outage or a gap in monitoring never lowers the number.
package uptime

import (
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

// Summary is the time a monitor spent in each status within a window.
type Summary struct {
	Up          time.Duration
	Degraded    time.Duration
	Down        time.Duration
	Maintenance time.Duration
	Paused      time.Duration
}

// Counted is the availability denominator.
func (s Summary) Counted() time.Duration { return s.Up + s.Degraded + s.Down }

// Ratio returns availability in [0, 1]; ok is false when nothing was counted.
func (s Summary) Ratio() (ratio float64, ok bool) {
	c := s.Counted()
	if c <= 0 {
		return 0, false
	}
	return float64(s.Up+s.Degraded) / float64(c), true
}

// Summarize measures periods within [from, to). Open periods end at now.
func Summarize(periods []model.Period, from, to, now time.Time) Summary {
	var s Summary
	if to.After(now) {
		to = now
	}
	for _, p := range periods {
		d := p.Overlap(from, to, now)
		switch p.Status {
		case model.StatusUp:
			s.Up += d
		case model.StatusDegraded:
			s.Degraded += d
		case model.StatusDown:
			s.Down += d
		case model.StatusMaintenance:
			s.Maintenance += d
		case model.StatusPaused:
			s.Paused += d
		}
	}
	return s
}

// Level classifies a day for the history bar.
type Level int

const (
	LevelNoData Level = iota
	LevelOperational
	LevelDegraded
	LevelOutage
	LevelMaintenance
)

func (l Level) String() string {
	return [...]string{"nodata", "operational", "degraded", "outage", "maintenance"}[l]
}

// Day is one calendar day of history.
type Day struct {
	Date      time.Time // local midnight
	Summary   Summary
	Incidents int
	Level     Level
}

// Days splits history into n calendar days ending today in loc, oldest first.
// incidents are counted on the day they started.
func Days(periods []model.Period, incidents []model.Incident, n int, loc *time.Location, now time.Time) []Day {
	ln := now.In(loc)
	today := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
	days := make([]Day, n)
	for i := range days {
		start := time.Date(today.Year(), today.Month(), today.Day()-(n-1-i), 0, 0, 0, 0, loc)
		end := time.Date(start.Year(), start.Month(), start.Day()+1, 0, 0, 0, 0, loc)
		d := Day{Date: start, Summary: Summarize(periods, start, end, now)}
		for _, inc := range incidents {
			if !inc.Started.Before(start) && inc.Started.Before(end) {
				d.Incidents++
			}
		}
		d.Level = classify(d.Summary)
		days[i] = d
	}
	return days
}

func classify(s Summary) Level {
	switch {
	case s.Down > 0:
		return LevelOutage
	case s.Degraded > 0:
		return LevelDegraded
	case s.Up > 0:
		return LevelOperational
	case s.Maintenance > 0:
		return LevelMaintenance
	}
	return LevelNoData
}
