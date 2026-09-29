package engine

import (
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const (
	U = model.StatusUp
	G = model.StatusDegraded
	D = model.StatusDown
)

func feed(tr *tracker, th config.Confirm, seq ...model.Status) []change {
	var out []change
	for i, s := range seq {
		c := model.Check{Status: s, At: t0.Add(time.Duration(i) * time.Minute), Message: "check " + s.String()}
		if ch, ok := tr.observe(c, th); ok {
			out = append(out, ch)
		}
	}
	return out
}

func TestTrackerConfirmsAndDatesFromStreakStart(t *testing.T) {
	th := config.Confirm{Down: 3, Up: 2}
	var tr tracker
	//                    0  1  2  3  4  5  6  7  8  9  10 11
	got := feed(&tr, th, U, U, D, D, U, D, D, D, D, U, G, U)
	// minute 0: the first healthy result is accepted immediately.
	// minutes 2-3: two failures, then a success: a blip, not an outage.
	// minutes 5-7: three failures confirm an outage dated from minute 5.
	// minutes 9-10: up then degraded both count toward recovery; the
	//   monitor recovers as degraded (the latest result), dated minute 9.
	// minute 11: a single up after degraded is not yet confirmed (Up=2).
	want := []change{
		{from: model.StatusUnknown, to: U, at: t0},
		{from: U, to: D, at: t0.Add(5 * time.Minute), cause: "check down"},
		{from: D, to: G, at: t0.Add(9 * time.Minute)},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.from != w.from || g.to != w.to || !g.at.Equal(w.at) || (w.cause != "" && g.cause != w.cause) {
			t.Errorf("change %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestTrackerTable(t *testing.T) {
	cases := []struct {
		name string
		th   config.Confirm
		seq  []model.Status
		want []model.Status // sequence of confirmed targets
	}{
		{"startup healthy", config.Confirm{Down: 3, Up: 2}, []model.Status{U, U}, []model.Status{U}},
		{"startup degraded", config.Confirm{Down: 3, Up: 2}, []model.Status{G}, []model.Status{G}},
		{"startup down needs confirmation", config.Confirm{Down: 3, Up: 2}, []model.Status{D, D}, nil},
		{"startup down confirmed", config.Confirm{Down: 2, Up: 2}, []model.Status{D, D}, []model.Status{D}},
		{"blips are ignored", config.Confirm{Down: 2, Up: 1}, []model.Status{U, D, U, D, U, D, U}, []model.Status{U}},
		{"recovery with mixed up and degraded", config.Confirm{Down: 1, Up: 3}, []model.Status{U, D, G, U, G}, []model.Status{U, D, G}},
		{"recovery interrupted", config.Confirm{Down: 1, Up: 2}, []model.Status{U, D, U, D, U, U}, []model.Status{U, D, U}},
		{"degradation needs Down confirmations", config.Confirm{Down: 2, Up: 2}, []model.Status{U, G, U, G, G}, []model.Status{U, G}},
		{"degraded back to up needs Up confirmations", config.Confirm{Down: 2, Up: 3}, []model.Status{G, U, U, G, U, U, U}, []model.Status{G, U}},
		{"degraded then down", config.Confirm{Down: 2, Up: 2}, []model.Status{G, D, D}, []model.Status{G, D}},
		{"thresholds of one", config.Confirm{Down: 1, Up: 1}, []model.Status{U, D, U, G, U}, []model.Status{U, D, U, G, U}},
		// Review finding: failing and slow checks alternating must not
		// hide an outage. Slow checks neither count nor reset failures.
		{"alternating failed and slow from up", config.Confirm{Down: 3, Up: 2}, []model.Status{U, D, G, D, G, D}, []model.Status{U, G, D}},
		{"alternating failed and slow from degraded", config.Confirm{Down: 3, Up: 2}, []model.Status{G, D, G, D, G, D}, []model.Status{G, D}},
		{"a healthy check resets failures", config.Confirm{Down: 3, Up: 2}, []model.Status{U, D, G, U, D, G, D}, []model.Status{U, G}},
		{"slow then failing escalates", config.Confirm{Down: 3, Up: 2}, []model.Status{U, G, G, G, D, D, D}, []model.Status{U, G, D}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tr tracker
			var got []model.Status
			for _, ch := range feed(&tr, c.th, c.seq...) {
				got = append(got, ch.to)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestTrackerPendingAndReset(t *testing.T) {
	th := config.Confirm{Down: 3, Up: 2}
	var tr tracker
	feed(&tr, th, U)
	if tr.pending() {
		t.Error("no streak yet")
	}
	tr.observe(model.Check{Status: D, At: t0.Add(time.Hour)}, th)
	if !tr.pending() {
		t.Error("a failure starts a pending streak")
	}
	tr.reset(model.StatusUnknown, t0)
	if tr.pending() || tr.status != model.StatusUnknown {
		t.Error("reset clears the streak")
	}
}

func TestTrackerDatesOutageFromFirstFailureAcrossSlowChecks(t *testing.T) {
	th := config.Confirm{Down: 3, Up: 2}
	var tr tracker
	got := feed(&tr, th, U, D, G, D, G, D)
	down := got[len(got)-1]
	if down.to != D || !down.at.Equal(t0.Add(time.Minute)) || down.cause != "check down" {
		t.Fatalf("down change %+v, want dated at the first failure (minute 1)", down)
	}
}

func TestTrackerPendingByStatus(t *testing.T) {
	th := config.Confirm{Down: 3, Up: 2}
	var tr tracker
	feed(&tr, th, G, G, G, G)
	if tr.status != G || tr.pending() {
		t.Error("a steady degraded state is not pending: it should use the normal interval")
	}
	tr.observe(model.Check{Status: D, At: t0.Add(time.Hour)}, th)
	if !tr.pending() {
		t.Error("a failure while degraded is pending")
	}
}

func TestTrackerNeverDatesBeforeCurrentStatus(t *testing.T) {
	th := config.Confirm{Down: 2, Up: 1}
	var tr tracker
	tr.reset(U, t0.Add(10*time.Minute)) // e.g. restored after a restart
	// A down streak whose first check predates `since` must not produce a
	// period that starts before the current one.
	tr.observe(model.Check{Status: D, At: t0.Add(5 * time.Minute)}, th)
	ch, ok := tr.observe(model.Check{Status: D, At: t0.Add(11 * time.Minute)}, th)
	if !ok || !ch.at.Equal(t0.Add(10*time.Minute)) { // clamped to when "up" began
		t.Errorf("change %+v %v", ch, ok)
	}
}
