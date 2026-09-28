package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	c, err := Parse([]byte(src), "/etc/pharos/pharos.yaml", env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func problems(t *testing.T, src string, vars map[string]string) []Problem {
	t.Helper()
	_, err := Parse([]byte(src), "test.yaml", env(vars))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *config.Error, got %v", err)
	}
	return ce.Problems
}

func hasProblem(ps []Problem, line int, substr string) bool {
	for _, p := range ps {
		if (line == 0 || p.Line == line) && strings.Contains(p.Msg, substr) {
			return true
		}
	}
	return false
}

func TestDefaults(t *testing.T) {
	c := mustParse(t, `
monitors:
  - id: web
    type: http
    url: https://example.com
  - id: nightly-backup
    type: push
    heartbeat: 24h
`)
	if c.Server.Listen != ":8080" || c.Server.Admin.Username != "admin" {
		t.Errorf("server defaults: %+v", c.Server)
	}
	if c.Storage.Path != "/etc/pharos/pharos.db" {
		t.Errorf("storage path should resolve next to the config file, got %q", c.Storage.Path)
	}
	if c.Storage.Retention.D() != 30*24*time.Hour {
		t.Errorf("retention = %s", c.Storage.Retention)
	}
	web := c.Monitors[0]
	if web.Name != "web" || web.Method != "GET" || web.Interval.D() != time.Minute || web.Timeout.D() != 10*time.Second {
		t.Errorf("monitor defaults: %+v", web)
	}
	if got := c.ConfirmFor(web); got.Down != 3 || got.Up != 2 {
		t.Errorf("confirm = %+v", got)
	}
	if web.Line != 3 {
		t.Errorf("monitor line = %d, want 3", web.Line)
	}
	push := c.Monitors[1]
	if push.Grace.D() != time.Hour {
		t.Errorf("grace for 24h heartbeat should cap at 1h, got %s", push.Grace)
	}
	if !c.MetricsEnabled() || !c.StatusPageEnabled() {
		t.Error("metrics and status page are on by default")
	}
}

func TestUnknownFieldReportsLine(t *testing.T) {
	ps := problems(t, `
server:
  listen: ":9000"
  lisen: ":9001"
monitors:
  - id: web
    type: http
    url: https://example.com
    intervall: 30s
`, nil)
	if !hasProblem(ps, 4, `unknown setting "lisen" in server`) {
		t.Errorf("missing server typo problem: %+v", ps)
	}
	if !hasProblem(ps, 9, `unknown setting "intervall" in monitor`) {
		t.Errorf("missing monitor typo problem: %+v", ps)
	}
}

func TestEnvExpansion(t *testing.T) {
	src := `
server:
  base_url: ${BASE_URL}
notifiers:
  - name: chat
    type: discord
    url: ${DISCORD_URL:-https://discord.com/api/webhooks/1/x}
  - name: mail
    type: email
    from: pharos@example.com
    to: [ops@example.com]
    smtp:
      host: smtp.example.com
      port: ${SMTP_PORT}
      password: "p$$ss"
monitors:
  - id: web
    type: http
    url: https://example.com
`
	c, err := Parse([]byte(src), "x.yaml", env(map[string]string{"BASE_URL": "https://status.example.com/", "SMTP_PORT": "2525"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.BaseURL != "https://status.example.com" {
		t.Errorf("base_url = %q (trailing slash should be trimmed)", c.Server.BaseURL)
	}
	if c.Notifiers[0].URL != "https://discord.com/api/webhooks/1/x" {
		t.Errorf("default not applied: %q", c.Notifiers[0].URL)
	}
	if c.Notifiers[1].SMTP.Port != 2525 {
		t.Errorf("port from env = %d", c.Notifiers[1].SMTP.Port)
	}
	if c.Notifiers[1].SMTP.Password != "p$ss" {
		t.Errorf("$$ should become $, got %q", c.Notifiers[1].SMTP.Password)
	}

	ps := problems(t, src, nil)
	if !hasProblem(ps, 3, "environment variable BASE_URL is not set") {
		t.Errorf("missing env problem: %+v", ps)
	}
}

func TestCollectsAllProblems(t *testing.T) {
	ps := problems(t, `
defaults:
  notify: [pager]
notifiers:
  - name: hook
    type: webhook
    url: not-a-url
  - name: hook
    type: carrier-pigeon
monitors:
  - id: Web
    type: http
    url: https://example.com
  - id: api
    type: http
    url: ftp://example.com
    method: FETCH
    timeout: 2m
    expect:
      status: [200, "6xx"]
      body_regex: "("
  - id: api
    type: tcp
    address: db.internal
  - id: resolver
    type: dns
    record: SRV
  - id: job
    type: push
    heartbeat: 5s
    token: short
  - id: thing
    type: smoke-signal
status_page:
  language: fr
  groups:
    - name: Core
      monitors: [web, ghost]
maintenance:
  - name: weekly
    days: [funday]
    at: "02:00"
    duration: 1h
  - start: "2026-01-02 10:00"
    end: "2026-01-02 09:00"
`, nil)
	want := []struct {
		line int
		msg  string
	}{
		{3, `defaults.notify refers to unknown notifier "pager"`},
		{7, "url must be an http(s) URL"},
		{8, `notifier name "hook" is used twice`},
		{9, `unknown type "carrier-pigeon"`},
		{11, "id may only contain lowercase"},
		{16, "url must be an absolute http(s) URL"},
		{17, `unsupported method "FETCH"`},
		{18, "timeout must be positive and not longer than the interval"},
		{20, `invalid status matcher "6xx"`},
		{21, "body_regex"},
		{22, `monitor id "api" is used twice`},
		{24, "address must be host:port"},
		{25, "query (the name to resolve) is required"},
		{27, "record must be one of"},
		{30, "heartbeat of at least 10s"},
		{31, "token must be at least 16 characters"},
		{33, `unknown type "smoke-signal"`},
		{35, `language must be "en" or "zh-TW"`},
		// "Web" failed validation, so the group's reference to "web" is unknown too.
		{38, `refers to unknown monitor "web"`},
		{38, `refers to unknown monitor "ghost"`},
		{40, `unknown day "funday"`},
		{44, "end must be after start"},
	}
	for _, w := range want {
		if !hasProblem(ps, w.line, w.msg) {
			t.Errorf("missing problem at line %d: %q", w.line, w.msg)
		}
	}
	if len(ps) != len(want) {
		for _, p := range ps {
			t.Logf("line %d: %s", p.Line, p.Msg)
		}
		t.Errorf("got %d problems, want %d", len(ps), len(want))
	}
}

func TestTypeErrorsHaveLines(t *testing.T) {
	ps := problems(t, `
defaults:
  interval: soon
monitors:
  - id: web
    type: http
    url: https://example.com
    follow_redirects: sometimes
`, nil)
	if !hasProblem(ps, 3, `invalid duration "soon"`) {
		t.Errorf("duration error: %+v", ps)
	}
	if !hasProblem(ps, 8, "!!bool") && !hasProblem(ps, 8, "bool") {
		t.Errorf("bool error: %+v", ps)
	}
}

func TestEmptyFile(t *testing.T) {
	ps := problems(t, "\n# nothing here\n", nil)
	if !hasProblem(ps, 0, "empty") {
		t.Errorf("got %+v", ps)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"0":     0,
		"30s":   30 * time.Second,
		"1.5m":  90 * time.Second,
		"2h30m": 150 * time.Minute,
		"7d":    7 * 24 * time.Hour,
		"1w2d":  9 * 24 * time.Hour,
		"1d12h": 36 * time.Hour,
		"250ms": 250 * time.Millisecond,
		" 5m ":  5 * time.Minute,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "10", "5 minutes", "1y", "h", "-5m"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) should fail", bad)
		}
	}
	if s := FormatDuration(36*time.Hour + 90*time.Second); s != "1d12h1m30s" {
		t.Errorf("FormatDuration = %q", s)
	}
}

func TestMaintenanceRecurringAcrossMidnight(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Taipei")
	m := Maintenance{Days: []string{"sat"}, At: "23:30", Duration: Duration(time.Hour)}
	if err := m.compile(loc); err != nil {
		t.Fatal(err)
	}
	// Saturday 2026-10-03 23:45 and Sunday 00:15 are inside; 00:31 is not.
	in1 := time.Date(2026, 10, 3, 23, 45, 0, 0, loc)
	in2 := time.Date(2026, 10, 4, 0, 15, 0, 0, loc)
	out := time.Date(2026, 10, 4, 0, 31, 0, 0, loc)
	for _, tt := range []struct {
		at   time.Time
		want bool
	}{{in1, true}, {in2, true}, {out, false}} {
		if _, _, ok := m.ActiveAt(tt.at); ok != tt.want {
			t.Errorf("ActiveAt(%s) = %v, want %v", tt.at, ok, tt.want)
		}
	}
	s, e, ok := m.Next(time.Date(2026, 10, 1, 12, 0, 0, 0, loc), 7*24*time.Hour)
	if !ok || !s.Equal(time.Date(2026, 10, 3, 23, 30, 0, 0, loc)) || e.Sub(s) != time.Hour {
		t.Errorf("Next = %s..%s %v", s, e, ok)
	}
}

func TestMaintenanceKeepsWallClockAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	m := Maintenance{Days: []string{"daily"}, At: "02:30", Duration: Duration(30 * time.Minute), Timezone: "America/New_York"}
	if err := m.compile(time.UTC); err != nil {
		t.Fatal(err)
	}
	// 2026-11-01 is the end of DST in New York; 02:30 local still occurs once.
	s, _, ok := m.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), 48*time.Hour)
	if !ok || s.In(ny).Hour() != 2 || s.In(ny).Minute() != 30 || s.In(ny).Day() != 1 {
		t.Errorf("Next across DST = %s", s.In(ny))
	}
}

func TestMaintenanceOneOffAndCovers(t *testing.T) {
	m := Maintenance{Start: "2026-10-01T10:00:00Z", End: "2026-10-01T11:00:00Z", Monitors: []string{"db"}}
	if err := m.compile(time.UTC); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.ActiveAt(time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC)); !ok {
		t.Error("should be active")
	}
	if _, _, ok := m.ActiveAt(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)); ok {
		t.Error("end is exclusive")
	}
	if !m.Covers("db") || m.Covers("web") {
		t.Error("Covers")
	}
	all := Maintenance{}
	if !all.Covers("anything") {
		t.Error("empty monitor list covers everything")
	}
}

func TestNotifierWants(t *testing.T) {
	var n Notifier
	if !n.Wants(EventDown) || n.Wants(EventDegraded) {
		t.Error("default subscription is everything except degraded")
	}
	n.Events = []string{EventDegraded}
	if n.Wants(EventDown) || !n.Wants(EventDegraded) {
		t.Error("explicit list")
	}
}

func TestFingerprintChangesWithProbeSettings(t *testing.T) {
	a := Monitor{ID: "web", Type: TypeHTTP, URL: "https://a.example", Line: 3}
	b := a
	b.Line = 40
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("moving a monitor in the file must not change its fingerprint")
	}
	b.URL = "https://b.example"
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("changing the URL must change the fingerprint")
	}
}
