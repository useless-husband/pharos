package notify

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
)

var at = time.Date(2026, 9, 29, 6, 30, 0, 0, time.UTC)

func downEvent() model.Event {
	return model.Event{
		Kind: model.EventDown, At: at, Status: model.StatusDown, Previous: model.StatusUp,
		Monitor:  model.MonitorRef{ID: "api", Name: "Public API", Type: "http", Target: "https://api.example.com/health"},
		Message:  "HTTP 503 Service Unavailable",
		Incident: &model.Incident{ID: 7, MonitorID: "api", Started: at, Cause: "HTTP 503 Service Unavailable"},
	}
}

func TestRender(t *testing.T) {
	taipei, _ := time.LoadLocation("Asia/Taipei")
	m := Render(downEvent(), "en", taipei, "https://status.example.com")
	if m.Title != "DOWN: Public API" || !strings.Contains(m.Body, "Cause: HTTP 503") || !strings.Contains(m.Body, "2026-09-29 14:30 CST") {
		t.Errorf("en down: %q / %q", m.Title, m.Body)
	}
	if m.Link != "https://status.example.com/admin/monitors/api" || m.Severity != SeverityCritical {
		t.Errorf("link/severity: %q %v", m.Link, m.Severity)
	}
	up := downEvent()
	up.Kind, up.Status, up.Duration = model.EventUp, model.StatusUp, 2*time.Hour+5*time.Minute+20*time.Second
	m = Render(up, "zh-TW", taipei, "")
	if m.Title != "已恢復：Public API" || !strings.Contains(m.Body, "中斷時間共 2 小時 5 分鐘") || m.Link != "" {
		t.Errorf("zh-TW up: %q / %q / %q", m.Title, m.Body, m.Link)
	}
	perf := downEvent()
	perf.Kind, perf.Status = model.EventDegraded, model.StatusUp
	if m := Render(perf, "en", time.UTC, ""); !strings.HasPrefix(m.Title, "PERFORMANCE RECOVERED") || m.Severity != SeverityGood {
		t.Errorf("perf recovered: %+v", m)
	}
	cert := downEvent()
	cert.Kind, cert.CertExpiry, cert.Duration = model.EventCert, at.Add(9*24*time.Hour), 9*24*time.Hour
	if m := Render(cert, "en", time.UTC, ""); !strings.Contains(m.Body, "expires in 9 days (2026-10-08)") {
		t.Errorf("cert: %q", m.Body)
	}
}

type captured struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	status  []int // response codes to return, in order; last repeats
}

func (c *captured) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.headers = append(c.headers, r.Header.Clone())
		c.paths = append(c.paths, r.URL.Path)
		code := 200
		if len(c.status) > 0 {
			code = c.status[min(len(c.bodies)-1, len(c.status)-1)]
		}
		c.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *captured) json(t *testing.T, i int) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(c.bodies[i], &v); err != nil {
		t.Fatalf("body %d is not JSON: %s", i, c.bodies[i])
	}
	return v
}

func send(t *testing.T, n config.Notifier, ev model.Event) error {
	t.Helper()
	s, err := NewSender(n)
	if err != nil {
		t.Fatal(err)
	}
	return s.Send(context.Background(), Render(ev, "en", time.UTC, "https://status.example.com"))
}

func TestWebhookPayloadAndSignature(t *testing.T) {
	var c captured
	srv := c.server(t)
	n := config.Notifier{Name: "hook", Type: "webhook", URL: srv.URL + "/hooks/pharos", Secret: "s3cret", Headers: map[string]string{"X-Team": "ops"}}
	if err := send(t, n, downEvent()); err != nil {
		t.Fatal(err)
	}
	body := c.json(t, 0)
	if body["event"] != "down" || body["status"] != "down" || body["previous_status"] != "up" || body["title"] != "DOWN: Public API" {
		t.Errorf("payload %v", body)
	}
	if mon := body["monitor"].(map[string]any); mon["id"] != "api" || mon["target"] != "https://api.example.com/health" {
		t.Errorf("monitor %v", mon)
	}
	if inc := body["incident"].(map[string]any); inc["id"].(float64) != 7 {
		t.Errorf("incident %v", inc)
	}
	h := c.headers[0]
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(c.bodies[0])
	if h.Get("X-Pharos-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Error("signature does not verify")
	}
	if h.Get("X-Team") != "ops" || h.Get("X-Pharos-Event") != "down" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
}

func TestChatPayloads(t *testing.T) {
	var c captured
	srv := c.server(t)
	for _, n := range []config.Notifier{
		{Name: "s", Type: "slack", URL: srv.URL + "/slack"},
		{Name: "d", Type: "discord", URL: srv.URL + "/discord"},
		{Name: "n", Type: "ntfy", URL: srv.URL + "/pharos-alerts", Token: "tk"},
	} {
		if err := send(t, n, downEvent()); err != nil {
			t.Fatalf("%s: %v", n.Type, err)
		}
	}
	slack := c.json(t, 0)
	if slack["text"] != "DOWN: Public API" || !strings.Contains(string(c.bodies[0]), "status.example.com/admin/monitors/api|Open in Pharos") {
		t.Errorf("slack %s", c.bodies[0])
	}
	embed := c.json(t, 1)["embeds"].([]any)[0].(map[string]any)
	if embed["color"].(float64) != 0xd03b3b || embed["url"] != "https://status.example.com/admin/monitors/api" {
		t.Errorf("discord embed %v", embed)
	}
	ntfy := c.json(t, 2)
	if c.paths[2] != "/" || ntfy["topic"] != "pharos-alerts" || ntfy["priority"].(float64) != 5 || c.headers[2].Get("Authorization") != "Bearer tk" {
		t.Errorf("ntfy %v path %s", ntfy, c.paths[2])
	}
	if _, err := NewSender(config.Notifier{Type: "ntfy", URL: "https://ntfy.sh/"}); err == nil {
		t.Error("ntfy without topic must be rejected")
	}
}

func TestTelegramRedactsToken(t *testing.T) {
	var c captured
	c.status = []int{401}
	srv := c.server(t)
	tg := &telegram{token: "123:SECRET", chatID: "42", api: srv.URL}
	err := tg.Send(context.Background(), Render(downEvent(), "en", time.UTC, ""))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must not contain the token: %v", err)
	}
	var perm *PermanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "401") {
		t.Errorf("a rejected bot token is permanent and must not be retried: %v", err)
	}
	if c.paths[0] != "/bot123:SECRET/sendMessage" {
		t.Errorf("path %s", c.paths[0])
	}
	body := c.json(t, 0)
	if body["parse_mode"] != "HTML" || !strings.HasPrefix(body["text"].(string), "<b>DOWN: Public API</b>") {
		t.Errorf("telegram body %v", body)
	}
}

func TestPermanentVersusRetryable(t *testing.T) {
	var c captured
	c.status = []int{404, 500, 429}
	srv := c.server(t)
	n := config.Notifier{Type: "webhook", URL: srv.URL}
	var perm *PermanentError
	if err := send(t, n, downEvent()); !errors.As(err, &perm) {
		t.Errorf("404 should be permanent: %v", err)
	}
	if err := send(t, n, downEvent()); err == nil || errors.As(err, &perm) {
		t.Errorf("500 should be retryable: %v", err)
	}
	if err := send(t, n, downEvent()); err == nil || errors.As(err, &perm) {
		t.Errorf("429 should be retryable: %v", err)
	}
}

// smtpServer is a minimal SMTP server that records one message.
func smtpServer(t *testing.T, starttls bool) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 localhost ESMTP test")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				if starttls {
					say("250-localhost")
					say("250 STARTTLS")
				} else {
					say("250 localhost")
				}
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 OK")
			case cmd == "DATA":
				say("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				got <- b.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("502 unsupported")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestEmail(t *testing.T) {
	addr, got := smtpServer(t, false)
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	n := config.Notifier{Type: "email", From: "Pharos <pharos@example.com>", To: []string{"ops@example.com", "林小明 <ming@example.com>"},
		SMTP: config.SMTP{Host: host, Port: p, Security: "none"}}
	ev := downEvent()
	ev.Monitor.Name = "官網"
	if err := send(t, n, ev); err != nil {
		t.Fatal(err)
	}
	raw := <-got
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	dec := new(mime.WordDecoder)
	var subject string
	for _, l := range strings.Split(head, "\r\n") {
		if s, ok := strings.CutPrefix(l, "Subject: "); ok {
			subject, _ = dec.DecodeHeader(s)
		}
	}
	if subject != "DOWN: 官網" {
		t.Errorf("subject %q", subject)
	}
	if !strings.Contains(head, "X-Pharos-Monitor: api") || !strings.Contains(head, "Content-Transfer-Encoding: quoted-printable") {
		t.Errorf("headers:\n%s", head)
	}
	plain, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !strings.Contains(string(plain), "官網 is down.") || !strings.Contains(string(plain), "Open in Pharos: https://status.example.com/admin/monitors/api") {
		t.Errorf("body:\n%s", plain)
	}
}

func TestEmailRequiresStartTLSWhenConfigured(t *testing.T) {
	addr, _ := smtpServer(t, false)
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	n := config.Notifier{Type: "email", From: "p@example.com", To: []string{"o@example.com"}, SMTP: config.SMTP{Host: host, Port: p, Security: "starttls"}}
	err := send(t, n, downEvent())
	var perm *PermanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("expected refusal to downgrade, got %v", err)
	}
}

type fakeSender struct {
	mu    sync.Mutex
	errs  []error
	calls int32
	msgs  []Message
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := int(atomic.AddInt32(&f.calls, 1)) - 1
	f.msgs = append(f.msgs, m)
	if i < len(f.errs) {
		return f.errs[i]
	}
	return nil
}

func dispatcherConfig() *config.Config {
	ops := []string{"ops"}
	none := []string{}
	return &config.Config{
		StatusPage: config.StatusPage{Language: "en"},
		Notifiers: []config.Notifier{
			{Name: "ops", Type: "webhook", URL: "http://x"},
			{Name: "perf", Type: "webhook", URL: "http://y", Events: []string{"degraded"}},
		},
		Defaults: config.Defaults{Notify: []string{"ops", "perf"}},
		Monitors: []config.Monitor{{ID: "api"}, {ID: "quiet", Notify: &none}, {ID: "only-ops", Notify: &ops}},
	}
}

func TestDispatcherRoutingRetriesAndLog(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	senders := map[string]*fakeSender{
		"ops":  {errs: []error{errors.New("connection reset"), errors.New("HTTP 502")}},
		"perf": {},
	}
	d, err := NewDispatcher(dispatcherConfig(), Options{
		Log: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		NewSender: func(n config.Notifier) (Sender, error) { return senders[n.Name], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Notify(downEvent()) // api: ops only (perf wants degraded only)
	quiet := downEvent()
	quiet.Monitor.ID = "quiet"
	d.Notify(quiet) // explicitly no notifiers
	deg := downEvent()
	deg.Kind = model.EventDegraded
	deg.Monitor.ID = "only-ops"
	d.Notify(deg) // ops does not want degraded by default; perf is not routed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)

	if got := atomic.LoadInt32(&senders["ops"].calls); got != 3 {
		t.Errorf("ops attempts = %d, want 3 (two failures then success)", got)
	}
	if got := atomic.LoadInt32(&senders["perf"].calls); got != 0 {
		t.Errorf("perf should not receive a down event, got %d", got)
	}
	log, _ := st.Notifications(context.Background(), 10)
	if len(log) != 1 || log[0].State != store.NotifyDelivered || log[0].Attempts != 3 || log[0].IncidentID != 7 {
		t.Fatalf("log %+v", log)
	}
	d.Notify(downEvent()) // after Close: dropped
	if got := atomic.LoadInt32(&senders["ops"].calls); got != 3 {
		t.Error("events after Close must be dropped")
	}
}

func TestDispatcherStopsOnPermanentError(t *testing.T) {
	st, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	s := &fakeSender{errs: []error{&PermanentError{errors.New("HTTP 404")}}}
	d, _ := NewDispatcher(dispatcherConfig(), Options{Log: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: []time.Duration{time.Millisecond}, NewSender: func(config.Notifier) (Sender, error) { return s, nil }})
	d.Notify(downEvent())
	d.Close(context.Background())
	if s.calls != 1 {
		t.Errorf("permanent errors must not be retried: %d calls", s.calls)
	}
	log, _ := st.Notifications(context.Background(), 10)
	if log[0].State != store.NotifyFailed || log[0].LastError != "HTTP 404" {
		t.Errorf("log %+v", log[0])
	}
}

func TestDispatcherTest(t *testing.T) {
	s := &fakeSender{}
	d, _ := NewDispatcher(dispatcherConfig(), Options{NewSender: func(config.Notifier) (Sender, error) { return s, nil }})
	if err := d.Test(context.Background(), "ops"); err != nil {
		t.Fatal(err)
	}
	if s.msgs[0].Title != "Test notification from Pharos" || !strings.Contains(s.msgs[0].Body, `"ops"`) {
		t.Errorf("test message %+v", s.msgs[0])
	}
	if err := d.Test(context.Background(), "nope"); err == nil {
		t.Error("unknown notifier")
	}
}
