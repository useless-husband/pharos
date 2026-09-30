package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

func parseConfig(t *testing.T, y string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(y), "test.yaml", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseInterspersed(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantPos  []string
		wantJSON bool
		wantCfg  string
	}{
		{[]string{"api"}, []string{"api"}, false, "pharos.yaml"},
		{[]string{"api", "-json"}, []string{"api"}, true, "pharos.yaml"},
		{[]string{"-c", "x.yaml", "api", "db", "-json"}, []string{"api", "db"}, true, "x.yaml"},
		{[]string{"api", "-c", "x.yaml", "db"}, []string{"api", "db"}, false, "x.yaml"},
		{[]string{"-json", "--", "-odd-id"}, []string{"-odd-id"}, true, "pharos.yaml"},
		{nil, nil, false, "pharos.yaml"},
	} {
		fs := flag.NewFlagSet("check", flag.ContinueOnError)
		cfg := fs.String("c", "pharos.yaml", "")
		asJSON := fs.Bool("json", false, "")
		pos := parseInterspersed(fs, tc.args)
		if !slices.Equal(pos, tc.wantPos) || *asJSON != tc.wantJSON || *cfg != tc.wantCfg {
			t.Errorf("%q: got %q json=%v c=%s, want %q json=%v c=%s", tc.args, pos, *asJSON, *cfg, tc.wantPos, tc.wantJSON, tc.wantCfg)
		}
	}
}

func TestSelectMonitors(t *testing.T) {
	cfg := parseConfig(t, `
monitors:
  - {id: api, type: http, url: "http://127.0.0.1:1/"}
  - {id: backup, type: push, heartbeat: 1d}
  - {id: db, type: tcp, address: "127.0.0.1:1"}
`)
	ms, skipped, err := selectMonitors(cfg, nil)
	if err != nil || len(ms) != 2 || ms[0].ID != "api" || ms[1].ID != "db" || skipped != 1 {
		t.Fatalf("all: %v skipped=%d err=%v", ms, skipped, err)
	}
	ms, _, err = selectMonitors(cfg, []string{"db", "api", "db"})
	if err != nil || len(ms) != 2 || ms[0].ID != "db" || ms[1].ID != "api" {
		t.Fatalf("named: %v %v", ms, err)
	}
	if _, _, err := selectMonitors(cfg, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "api, backup, db") {
		t.Errorf("unknown id: %v", err)
	}
	if _, _, err := selectMonitors(cfg, []string{"backup"}); err == nil || !strings.Contains(err.Error(), "push monitor") {
		t.Errorf("push id: %v", err)
	}
	onlyPush := parseConfig(t, "monitors:\n  - {id: backup, type: push, heartbeat: 1d}\n")
	if _, _, err := selectMonitors(onlyPush, nil); err == nil {
		t.Error("only push monitors: want an error")
	}
}

func TestRunChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	cfg := parseConfig(t, `
monitors:
  - {id: ok, type: http, url: "`+srv.URL+`/"}
  - {id: broken, type: http, url: "`+srv.URL+`/broken"}
`)
	results, err := runChecks(context.Background(), cfg, cfg.Monitors)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "ok" || results[1].ID != "broken" {
		t.Fatalf("results out of order: %+v", results)
	}
	// Loopback durations can be 0 on coarse clocks (Windows), so only the
	// presence of the timing is checked.
	if r := results[0]; r.Status != model.StatusUp || r.Timing == nil || r.LatencyMS < 0 {
		t.Errorf("ok: %+v", r)
	}
	if r := results[1]; r.Status != model.StatusDown || !strings.Contains(r.Message, "503") {
		t.Errorf("broken: %+v", r)
	}
}

func TestPrintChecks(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	results := []checkResult{
		{ID: "api", Target: "https://api.example.com/health", Status: model.StatusUp, warnCert: 14 * 24 * time.Hour,
			check: model.Check{Status: model.StatusUp, Latency: 142 * time.Millisecond,
				Timing:     &model.Timing{DNS: 12 * time.Millisecond, Connect: 20 * time.Millisecond, TLS: 45 * time.Millisecond, FirstByte: 60 * time.Millisecond},
				CertExpiry: now.Add(10 * 24 * time.Hour)}},
		{ID: "database", Target: "db.internal:5432", Status: model.StatusDown, Message: "connection timed out after 5s",
			check: model.Check{Status: model.StatusDown, Latency: 5 * time.Second, Message: "connection timed out after 5s"}},
	}
	var b strings.Builder
	printChecks(&b, results, 1, now, false)
	want := `MONITOR   STATUS    TIME    TARGET
api       up        142ms   https://api.example.com/health
                            dns 12ms · connect 20ms · tls 45ms · first byte 60ms
                            certificate valid until 2026-10-10 (10 days), inside the warning period
database  down      5.0s    db.internal:5432
                            connection timed out after 5s

2 monitors checked: 1 up, 1 down; 1 push monitor skipped (checked by heartbeats).
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		450 * time.Microsecond:  "0.5ms",
		142 * time.Millisecond:  "142ms",
		2500 * time.Millisecond: "2.5s",
		12 * time.Second:        "12s",
	} {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%s) = %s, want %s", d, got, want)
		}
	}
}
