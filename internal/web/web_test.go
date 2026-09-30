package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/useless-husband/pharos/internal/clock"
	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/demo"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/store"
)

type env struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	engine *engine.Engine
	cfg    *config.Config
	cancel context.CancelFunc
	store  *store.Store
}

const password = "correct horse battery"

// The demo history is seeded once and copied for each test: seeding
// ninety days of checks is the slow part.
var (
	seedOnce sync.Once
	seedDir  string
	seedNow  time.Time
	seedErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if seedDir != "" {
		os.RemoveAll(seedDir)
	}
	os.Exit(code)
}

func seeded(t *testing.T) (string, time.Time) {
	t.Helper()
	seedOnce.Do(func() {
		seedDir, seedErr = os.MkdirTemp("", "pharos-web-test-")
		if seedErr != nil {
			return
		}
		path := filepath.Join(seedDir, "seed.db")
		cfg, err := demo.Config(path, "en", "127.0.0.1:0")
		if err != nil {
			seedErr = err
			return
		}
		st, err := store.Open(context.Background(), path)
		if err != nil {
			seedErr = err
			return
		}
		seedNow = time.Now()
		seedErr = demo.Seed(context.Background(), st, cfg, seedNow)
		st.Close()
	})
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	return filepath.Join(seedDir, "seed.db"), seedNow
}

// newEnv runs the demo data set behind a real engine and server, after one
// round of (simulated) checks.
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	src, now := seeded(t)
	dir := t.TempDir()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "demo.db")
	if err := os.WriteFile(dbPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := demo.Config(dbPath, "en", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := clock.NewFake(now)
	eng := engine.New(engine.Options{Store: st, Clock: fc, Logger: quiet, NewProber: demo.NewProber})
	ectx, cancel := context.WithCancel(ctx)
	if err := eng.Start(ectx, cfg); err != nil {
		t.Fatal(err)
	}
	// Let every active monitor run its first check.
	fc.BlockUntil(len(cfg.Monitors)+1, 5*time.Second)
	fc.Advance(11 * time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, st := range eng.Snapshot() {
			if st.Monitor.Type != config.TypePush && st.Status.String() == "unknown" {
				ready = false
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("monitors did not complete their first check")
		}
		time.Sleep(10 * time.Millisecond)
	}
	srv, err := New(ctx, Options{Engine: eng, Store: st, Logger: quiet, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, h: srv.Handler(), engine: eng, cfg: cfg, cancel: cancel, store: st}
	t.Cleanup(func() {
		cancel()
		eng.Wait()
		st.Close()
	})
	return e
}

func withPassword(c *config.Config) {
	h, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	c.Server.Admin.PasswordHash = string(h)
}

type reqOpt func(*http.Request)

func remote(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func host(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }
func cookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}

func (e *env) do(method, target string, body io.Reader, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "203.0.113.9:4321" // remote by default
	r.Host = "localhost:8080"
	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) login() *http.Cookie {
	e.t.Helper()
	form := url.Values{"username": {"admin"}, "password": {password}}
	w := e.do("POST", "/admin/login?next=/admin/incidents", strings.NewReader(form.Encode()))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/incidents" {
		e.t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				e.t.Error("session cookie must be HttpOnly and SameSite=Lax")
			}
			return c
		}
	}
	e.t.Fatal("no session cookie")
	return nil
}

var csrfRE = regexp.MustCompile(`name="csrf-token" content="([^"]+)"`)

func (e *env) csrf(c *http.Cookie) string {
	e.t.Helper()
	w := e.do("GET", "/admin", nil, cookie(c))
	m := csrfRE.FindStringSubmatch(w.Body.String())
	if m == nil {
		e.t.Fatal("no csrf token on dashboard")
	}
	return m[1]
}

func TestStatusPage(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/", nil)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{"Acme Cloud Status", "All systems operational", "Public API", "Website and API", "Upcoming maintenance: Mail relay migration", `class="bars"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	// Private monitors (not in any group) and raw causes stay off the public page.
	for _, leak := range []string{"Primary database", "db.internal", "Nightly backup", "HTTP 503 Service Unavailable"} {
		if strings.Contains(body, leak) {
			t.Errorf("status page leaks %q", leak)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing")
	}
}

func TestShowCauses(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.StatusPage.ShowCauses = true })
	if body := e.do("GET", "/", nil).Body.String(); !strings.Contains(body, "HTTP 503 Service Unavailable") {
		t.Error("show_causes should publish failure messages")
	}
}

func TestHistoryPage(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/history/api", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Daily history") {
		t.Fatalf("history: %d", w.Code)
	}
	if w := e.do("GET", "/history/db", nil); w.Code != 404 {
		t.Errorf("private monitor history must be hidden, got %d", w.Code)
	}
	if w := e.do("GET", "/history/nope", nil); w.Code != 404 {
		t.Errorf("unknown monitor: %d", w.Code)
	}
}

func TestLocalOnlyWithoutPassword(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("GET", "/admin", nil, remote("127.0.0.1:5555")); w.Code != 200 {
		t.Errorf("loopback should reach the dashboard, got %d", w.Code)
	}
	if w := e.do("GET", "/admin", nil); w.Code != 403 {
		t.Errorf("remote client without a password configured: %d, want 403", w.Code)
	}
	// Behind a reverse proxy every request comes from loopback; a forwarded
	// request must not count as local.
	if w := e.do("GET", "/admin", nil, remote("127.0.0.1:5555"), header("X-Forwarded-For", "198.51.100.7")); w.Code != 403 {
		t.Errorf("proxied request treated as local: %d", w.Code)
	}
	if w := e.do("GET", "/api/v1/admin/monitors", nil); w.Code != 401 {
		t.Errorf("admin API without auth: %d", w.Code)
	}
}

func TestDemoDashboardIsOpen(t *testing.T) {
	e := newEnv(t, nil)
	srv, err := New(context.Background(), Options{Engine: e.engine, Store: e.store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OpenDashboard: true})
	if err != nil {
		t.Fatal(err)
	}
	e.h = srv.Handler()
	// In a container, the published port reaches the server from the
	// bridge gateway, never from loopback.
	gw := remote("172.17.0.1:40000")
	for _, p := range []string{"/admin", "/admin/monitors/api", "/api/v1/admin/monitors", "/metrics"} {
		if w := e.do("GET", p, nil, gw); w.Code != 200 {
			t.Errorf("%s: %d, want 200 in demo mode", p, w.Code)
		}
	}
	if w := e.do("GET", "/admin/login", nil, gw); w.Code != http.StatusSeeOther {
		t.Errorf("login page: %d, want a redirect to the dashboard", w.Code)
	}
	// Writes still need the CSRF token.
	if w := e.do("POST", "/admin/monitors/api/pause", nil, gw); w.Code != 403 {
		t.Errorf("POST without CSRF in demo mode: %d", w.Code)
	}
}

func TestLoginSessionsAndCSRF(t *testing.T) {
	e := newEnv(t, withPassword)
	if w := e.do("GET", "/admin/monitors/api", nil); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/admin/login?next=") {
		t.Fatalf("anonymous dashboard: %d %s", w.Code, w.Header().Get("Location"))
	}
	bad := url.Values{"username": {"admin"}, "password": {"wrong"}}
	if w := e.do("POST", "/admin/login", strings.NewReader(bad.Encode())); w.Code != 401 || !strings.Contains(w.Body.String(), "Incorrect username or password") {
		t.Fatalf("bad password: %d", w.Code)
	}
	c := e.login()
	if w := e.do("GET", "/admin/monitors/api", nil, cookie(c)); w.Code != 200 || !strings.Contains(w.Body.String(), "Response time") {
		t.Fatalf("monitor page: %d", w.Code)
	}
	for _, p := range []string{"/admin", "/admin/incidents", "/admin/incidents?state=open", "/admin/notifications", "/admin/settings", "/admin/monitors/backup", "/admin/monitors/api?range=90d&failures=1"} {
		if w := e.do("GET", p, nil, cookie(c)); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	// Tampered cookie.
	forged := *c
	forged.Value = strings.Replace(c.Value, ".", "x.", 1)
	if w := e.do("GET", "/api/v1/admin/monitors", nil, cookie(&forged)); w.Code != 401 {
		t.Errorf("forged session accepted: %d", w.Code)
	}
	// Writes need the CSRF token.
	if w := e.do("POST", "/admin/monitors/api/pause", nil, cookie(c)); w.Code != 403 {
		t.Errorf("POST without CSRF: %d", w.Code)
	}
	token := e.csrf(c)
	if w := e.do("POST", "/admin/monitors/api/pause", nil, cookie(c), header("X-CSRF-Token", token), header("Origin", "https://evil.example")); w.Code != 403 {
		t.Errorf("cross-origin POST: %d", w.Code)
	}
	w := e.do("POST", "/admin/monitors/api/pause", nil, cookie(c), header("X-CSRF-Token", token), header("Accept", "application/json"))
	if w.Code != 200 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	if st, _ := e.engine.State("api"); st.Status.String() != "paused" {
		t.Errorf("status after pause: %s", st.Status)
	}
	form := url.Values{"_csrf": {token}}
	if w := e.do("POST", "/admin/monitors/api/resume", strings.NewReader(form.Encode()), cookie(c)); w.Code != http.StatusSeeOther {
		t.Errorf("form resume: %d", w.Code)
	}
	if w := e.do("POST", "/admin/logout", strings.NewReader(form.Encode()), cookie(c)); w.Code != http.StatusSeeOther {
		t.Errorf("logout: %d", w.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, withPassword)
	bad := url.Values{"username": {"admin"}, "password": {"nope"}}.Encode()
	var last int
	for i := 0; i < 11; i++ {
		last = e.do("POST", "/admin/login", strings.NewReader(bad)).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("11th attempt: %d, want 429", last)
	}
	// Another address is not affected.
	good := url.Values{"username": {"admin"}, "password": {password}}.Encode()
	if w := e.do("POST", "/admin/login", strings.NewReader(good), remote("198.51.100.20:1")); w.Code != http.StatusSeeOther {
		t.Errorf("other client blocked: %d", w.Code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/admin/incidents": "/admin/incidents", "//evil.example/admin": "/admin", "https://evil.example": "/admin", "/admin\\@evil": "/admin", "": "/admin"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func TestPublicAPI(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/api/v1/status", nil)
	var v struct {
		Status string `json:"status"`
		Groups []struct {
			Name     string `json:"name"`
			Monitors []struct {
				ID     string              `json:"id"`
				Status string              `json:"status"`
				Uptime map[string]*float64 `json:"uptime"`
			} `json:"monitors"`
		} `json:"groups"`
		Incidents []map[string]any `json:"incidents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Groups) != 2 || v.Groups[0].Monitors[1].ID != "api" || v.Groups[0].Monitors[1].Uptime["30d"] == nil {
		t.Fatalf("status json %+v", v)
	}
	if len(v.Incidents) == 0 || v.Incidents[0]["cause"] != nil {
		t.Errorf("incidents should be listed without causes: %+v", v.Incidents)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("public API should allow cross-origin reads")
	}
}

func TestPush(t *testing.T) {
	e := newEnv(t, nil)
	m, _ := e.cfg.MonitorByID("backup")
	token := e.engine.PushToken(m)
	if w := e.do("GET", "/api/v1/push/"+token+"?ms=1500", nil); w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	if st, _ := e.engine.State("backup"); st.LastCheck == nil || st.LastCheck.Latency != 1500*time.Millisecond {
		t.Errorf("heartbeat not recorded: %+v", st.LastCheck)
	}
	if w := e.do("POST", "/api/v1/push/"+token, strings.NewReader("status=down&msg=disk+full")); w.Code != 200 {
		t.Fatalf("push down: %d", w.Code)
	}
	if st, _ := e.engine.State("backup"); st.Status.String() != "down" || st.Incident.Cause != "disk full" {
		t.Errorf("reported failure: %+v", st)
	}
	if w := e.do("GET", "/api/v1/push/wrong-token", nil); w.Code != 404 {
		t.Errorf("wrong token: %d", w.Code)
	}
	if w := e.do("GET", "/api/v1/push/"+token+"?status=maybe", nil); w.Code != 400 {
		t.Errorf("bad status: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/v1/push/"+token, nil); w.Code != 405 {
		t.Errorf("DELETE: %d", w.Code)
	}
}

func TestBadges(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/badge/api/uptime.svg?window=90d", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Body.String(), "uptime 90d") {
		t.Fatalf("uptime badge: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/badge/website/status.svg", nil); !strings.Contains(w.Body.String(), ">up<") {
		t.Errorf("status badge: %s", w.Body)
	}
	if w := e.do("GET", "/badge/db/status.svg", nil); w.Code != 404 {
		t.Errorf("badge of a private monitor: %d", w.Code)
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Server.Metrics.Token = "s3cret" })
	if w := e.do("GET", "/metrics", nil); w.Code != 401 {
		t.Fatalf("metrics without token: %d", w.Code)
	}
	w := e.do("GET", "/metrics", nil, header("Authorization", "Bearer s3cret"))
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE pharos_monitor_up gauge",
		`pharos_monitor_status{monitor="api",name="Public API",type="http",status="up"} `,
		`pharos_availability_ratio{monitor="api",name="Public API",type="http",window="30d"} 0.99`,
		`pharos_cert_expiry_timestamp_seconds{monitor="cdn"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// Every sample line must be well formed.
	sample := regexp.MustCompile(`^[a-z_]+(\{([a-z_]+="(\\.|[^"\\])*",?)+\})? [0-9eE.+-]+$`)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(line, "#") && !sample.MatchString(line) {
			t.Errorf("malformed metric line: %s", line)
		}
	}
}

func TestPromEscape(t *testing.T) {
	if got := promEscape("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("promEscape = %s", got)
	}
}

func TestAssetsAndErrors(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/", nil)
	m := regexp.MustCompile(`/assets/pharos\.css\?v=([0-9a-f]+)`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("stylesheet link missing")
	}
	a := e.do("GET", m[0], nil)
	if a.Code != 200 || !strings.Contains(a.Header().Get("Cache-Control"), "immutable") || !strings.HasPrefix(a.Header().Get("Content-Type"), "text/css") {
		t.Errorf("asset: %d %v", a.Code, a.Header())
	}
	if w := e.do("GET", "/assets/../templates/layout.html", nil); w.Code == 200 {
		t.Error("path traversal in assets")
	}
	if w := e.do("GET", "/nope", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "Page not found") {
		t.Errorf("404 page: %d", w.Code)
	}
	if w := e.do("GET", "/healthz", nil); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestZhTW(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.StatusPage.Language = "zh-TW" })
	body := e.do("GET", "/", nil).Body.String()
	for _, want := range []string{`lang="zh-TW"`, "所有服務正常運作", "即將進行維護：Mail relay migration", "可用率"} {
		if !strings.Contains(body, want) {
			t.Errorf("zh-TW page lacks %q", want)
		}
	}
}

func TestExport(t *testing.T) {
	e := newEnv(t, nil)
	dir := t.TempDir()
	files, err := e.srv.Export(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"index.html": true, "history/api.html": true, "status.json": true, "assets/pharos.css": true, ".nojekyll": true}
	for _, f := range files {
		delete(want, f)
		if strings.Contains(f, "db") || strings.Contains(f, "backup") {
			t.Errorf("private monitor exported: %s", f)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing files: %v", want)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	hist, _ := os.ReadFile(filepath.Join(dir, "history", "api.html"))
	if !strings.Contains(string(index), `href="history/api.html"`) || strings.Contains(string(index), `"/assets/`) || strings.Contains(string(index), "data-refresh") {
		t.Error("index.html should use relative links and no live refresh")
	}
	if !strings.Contains(string(hist), `href="../index.html"`) || !strings.Contains(string(hist), `href="../assets/pharos.css`) {
		t.Error("history pages should link back relatively")
	}
}

// Regression tests for the pre-release review.

func TestForwardedForCannotImpersonateLocal(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Server.TrustProxy = true })
	// nginx appends the real client: "<spoofed>, <real>".
	w := e.do("GET", "/admin", nil, remote("127.0.0.1:5555"), header("X-Forwarded-For", "127.0.0.1, 198.51.100.7"))
	if w.Code != 403 {
		t.Fatalf("spoofed X-Forwarded-For reached the dashboard: %d", w.Code)
	}
	// Even a genuinely local client behind the proxy is not "local".
	w = e.do("GET", "/admin", nil, remote("127.0.0.1:5555"), header("X-Forwarded-For", "127.0.0.1"))
	if w.Code != 403 {
		t.Fatalf("proxied request treated as local: %d", w.Code)
	}
}

func TestRateLimitUsesTheTrustedHop(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Server.TrustProxy = true; withPassword(c) })
	bad := url.Values{"username": {"admin"}, "password": {"nope"}}.Encode()
	var last int
	for i := 0; i < 11; i++ {
		spoof := fmt.Sprintf("10.0.0.%d, 198.51.100.7", i)
		last = e.do("POST", "/admin/login", strings.NewReader(bad), remote("127.0.0.1:1"), header("X-Forwarded-For", spoof)).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("rotating the client-supplied hop bypassed the limit: %d", last)
	}
}

func TestDNSRebindingIsRejected(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("GET", "/admin", nil, remote("127.0.0.1:5555"), host("evil.example:8080")); w.Code != 403 {
		t.Errorf("loopback request for a foreign host name: %d, want 403", w.Code)
	}
	for _, h := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "pharos.localhost"} {
		if w := e.do("GET", "/admin", nil, remote("127.0.0.1:5555"), host(h)); w.Code != 200 {
			t.Errorf("Host %s: %d, want 200", h, w.Code)
		}
	}
}

func TestEmptyGroupsExposeNothing(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.StatusPage.Groups = []config.Group{{Name: "Coming soon"}} })
	body := e.do("GET", "/", nil).Body.String()
	for _, leak := range []string{"Public API", "Primary database", "Nightly backup", "Static files"} {
		if strings.Contains(body, leak) {
			t.Errorf("status page with an empty group leaks %q", leak)
		}
	}
	var v struct {
		Incidents []any `json:"incidents"`
	}
	_ = json.Unmarshal(e.do("GET", "/api/v1/incidents?days=90", nil).Body.Bytes(), &v)
	if len(v.Incidents) != 0 {
		t.Errorf("incidents API leaks %d incidents", len(v.Incidents))
	}
}

func TestMaintenanceDoesNotRevealPrivateMonitors(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Maintenance[1].Monitors = []string{"mail", "db"}
		if err := config.CompileMaintenance(&c.Maintenance[1], time.UTC); err != nil {
			t.Fatal(err)
		}
	})
	body := e.do("GET", "/api/v1/status", nil).Body.String()
	if strings.Contains(body, `"db"`) || !strings.Contains(body, `"mail"`) {
		t.Errorf("maintenance monitors in the public API: %s", body)
	}
}

func TestMetricsAreLocalWithoutToken(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("GET", "/metrics", nil); w.Code != 403 {
		t.Errorf("remote /metrics without a token: %d", w.Code)
	}
	if w := e.do("GET", "/metrics", nil, remote("127.0.0.1:9")); w.Code != 200 {
		t.Errorf("local /metrics: %d", w.Code)
	}
}

func TestPasswordChangeEndsSessions(t *testing.T) {
	e := newEnv(t, withPassword)
	c := e.login()
	next := *e.cfg
	h, _ := bcrypt.GenerateFromPassword([]byte("another password!"), bcrypt.MinCost)
	next.Server.Admin.PasswordHash = string(h)
	if err := e.engine.Reload(&next); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/v1/admin/monitors", nil, cookie(c)); w.Code != 401 {
		t.Errorf("session survived a password change: %d", w.Code)
	}
}

func TestOriginBehindHostRewritingProxy(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Server.TrustProxy = true
		c.Server.BaseURL = "https://status.example.com"
		withPassword(c)
	})
	good := url.Values{"username": {"admin"}, "password": {password}}.Encode()
	w := e.do("POST", "/admin/login", strings.NewReader(good), host("127.0.0.1:8080"),
		header("Origin", "https://status.example.com"), header("X-Forwarded-For", "198.51.100.7"))
	if w.Code != http.StatusSeeOther {
		t.Errorf("login through a proxy that rewrites Host: %d", w.Code)
	}
	w = e.do("POST", "/admin/login", strings.NewReader(good), host("127.0.0.1:8080"),
		header("Origin", "https://evil.example"), header("X-Forwarded-For", "198.51.100.8"))
	if w.Code == http.StatusSeeOther {
		t.Error("cross-origin login accepted")
	}
}

func TestPushBodyIsBounded(t *testing.T) {
	e := newEnv(t, nil)
	m, _ := e.cfg.MonitorByID("backup")
	big := strings.NewReader("msg=" + strings.Repeat("a", 1<<20))
	if w := e.do("POST", "/api/v1/push/"+e.engine.PushToken(m), big); w.Code != 400 {
		t.Errorf("oversized push body: %d, want 400", w.Code)
	}
}
