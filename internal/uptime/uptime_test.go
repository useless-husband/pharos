package uptime

import (
	"math"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/model"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }

func TestSummarizeExcludesMaintenanceAndGaps(t *testing.T) {
	periods := []model.Period{
		{Status: model.StatusUp, Start: at(0), End: at(10)},
		{Status: model.StatusDown, Start: at(10), End: at(11)},
		{Status: model.StatusMaintenance, Start: at(11), End: at(13)},
		{Status: model.StatusUnknown, Start: at(13), End: at(14)}, // Pharos was offline
		{Status: model.StatusDegraded, Start: at(14), End: at(15)},
		{Status: model.StatusUp, Start: at(15)}, // still open
	}
	s := Summarize(periods, at(0), at(24), at(20))
	if s.Up != 15*time.Hour || s.Down != time.Hour || s.Degraded != time.Hour || s.Maintenance != 2*time.Hour {
		t.Fatalf("summary %+v", s)
	}
	r, ok := s.Ratio()
	if !ok || math.Abs(r-16.0/17.0) > 1e-9 {
		t.Errorf("ratio = %v, want 16/17", r)
	}
	// Window clipping.
	s = Summarize(periods, at(9), at(10.5), at(20))
	if s.Up != time.Hour || s.Down != 30*time.Minute {
		t.Errorf("clipped summary %+v", s)
	}
	if _, ok := Summarize(nil, at(0), at(1), at(1)).Ratio(); ok {
		t.Error("empty window has no ratio")
	}
}

func TestDaysClassificationAndIncidents(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Taipei")
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, loc)
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, loc) }
	periods := []model.Period{
		{Status: model.StatusUp, Start: day(7, 0), End: day(8, 3)},
		{Status: model.StatusDown, Start: day(8, 3), End: day(8, 4)},
		{Status: model.StatusUp, Start: day(8, 4), End: day(9, 0)},
		{Status: model.StatusMaintenance, Start: day(9, 0), End: day(10, 0)},
		{Status: model.StatusDegraded, Start: day(10, 0), End: day(10, 1)},
		{Status: model.StatusUp, Start: day(10, 1)},
	}
	incidents := []model.Incident{{Started: day(8, 3), Ended: day(8, 4)}}
	days := Days(periods, incidents, 5, loc, now)
	want := []Level{LevelNoData, LevelOperational, LevelOutage, LevelMaintenance, LevelDegraded}
	for i, d := range days {
		if d.Level != want[i] {
			t.Errorf("day %s level %s, want %s", d.Date.Format("01-02"), d.Level, want[i])
		}
	}
	if days[2].Incidents != 1 || days[1].Incidents != 0 {
		t.Error("incident attribution")
	}
	if !days[4].Date.Equal(day(10, 0)) {
		t.Errorf("last day should be today, got %s", days[4].Date)
	}
	// Today is only counted up to now.
	if got := days[4].Summary.Counted(); got != 12*time.Hour {
		t.Errorf("today counted %s, want 12h", got)
	}
}
