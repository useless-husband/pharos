package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

func dur(d time.Duration) config.Duration { return config.Duration(d) }

func httpMonitor(url string) config.Monitor {
	return config.Monitor{ID: "t", Type: config.TypeHTTP, URL: url, Method: "GET",
		Interval: dur(time.Minute), Timeout: dur(2 * time.Second)}
}

func run(t *testing.T, m config.Monitor) model.Check {
	t.Helper()
	p, err := New(m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.Probe(context.Background())
}

func expect(t *testing.T, c model.Check, status model.Status, msg string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Message, msg) {
		t.Fatalf("got %s %q, want %s containing %q", c.Status, c.Message, status, msg)
	}
}

func TestHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "Pharos") {
			t.Errorf("user agent %q", r.UserAgent())
		}
		w.Header().Set("X-Version", "build 42")
		fmt.Fprint(w, `{"status":"ok","checks":[{"db":"up"}],"count":3}`)
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(w, "done")
	})
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %s", r.Method, r.Header.Get("X-Token"), r.Host)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("up with timings", func(t *testing.T) {
		c := run(t, httpMonitor(srv.URL+"/ok"))
		expect(t, c, model.StatusUp, "")
		if c.Latency <= 0 || c.Timing == nil || c.Timing.FirstByte <= 0 {
			t.Errorf("missing latency/timing: %+v %+v", c.Latency, c.Timing)
		}
	})
	t.Run("bad status", func(t *testing.T) {
		expect(t, run(t, httpMonitor(srv.URL+"/fail")), model.StatusDown, "HTTP 503 Service Unavailable")
	})
	t.Run("accepted status list", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/fail")
		m.Expect.Status = []string{"200", "5xx"}
		expect(t, run(t, m), model.StatusUp, "")
	})
	t.Run("body and headers", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/ok")
		m.Expect.BodyContains = `"status":"ok"`
		m.Expect.Headers = map[string]string{"X-Version": "build"}
		expect(t, run(t, m), model.StatusUp, "")
		m.Expect.BodyNotContains = "count"
		expect(t, run(t, m), model.StatusDown, `body contains "count"`)
		m.Expect.BodyNotContains = ""
		m.Expect.Headers = map[string]string{"X-Region": "tw"}
		expect(t, run(t, m), model.StatusDown, "header X-Region is missing")
	})
	t.Run("regex", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/ok")
		m.Expect.BodyRegex = `"count":\s*[0-9]+`
		expect(t, run(t, m), model.StatusUp, "")
		m.Expect.BodyRegex = `"count":\s*9`
		expect(t, run(t, m), model.StatusDown, "does not match")
	})
	t.Run("json", func(t *testing.T) {
		yes, no := true, false
		m := httpMonitor(srv.URL + "/ok")
		m.Expect.JSON = []config.JSONExpect{{Path: "status", Equals: "ok"}, {Path: "checks[0].db", Equals: "up"}, {Path: "count", Equals: 3}, {Path: "error", Exists: &no}, {Path: "status", Exists: &yes}}
		expect(t, run(t, m), model.StatusUp, "")
		m.Expect.JSON = []config.JSONExpect{{Path: "count", Equals: 4}}
		expect(t, run(t, m), model.StatusDown, "JSON count is 3, expected 4")
		m.Expect.JSON = []config.JSONExpect{{Path: "checks[3].db", Equals: "up"}}
		expect(t, run(t, m), model.StatusDown, "JSON checks[3].db is missing")
		m.URL = srv.URL + "/slow"
		expect(t, run(t, m), model.StatusDown, "not valid JSON")
	})
	t.Run("degraded when slow", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/slow")
		m.Expect.MaxLatency = dur(50 * time.Millisecond)
		c := run(t, m)
		expect(t, c, model.StatusDegraded, "slow response")
	})
	t.Run("timeout", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/hang")
		m.Timeout = dur(200 * time.Millisecond)
		start := time.Now()
		expect(t, run(t, m), model.StatusDown, "timed out after 200ms")
		if time.Since(start) > 2*time.Second {
			t.Error("timeout not enforced")
		}
	})
	t.Run("redirects", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/redirect")
		m.Expect.BodyContains = "status"
		expect(t, run(t, m), model.StatusUp, "")
		no := false
		m.FollowRedirects = &no
		m.Expect = config.Expect{Status: []string{"302"}}
		expect(t, run(t, m), model.StatusUp, "")
	})
	t.Run("method headers host", func(t *testing.T) {
		m := httpMonitor(srv.URL + "/echo")
		m.Method = "POST"
		m.Body = "{}"
		m.Headers = map[string]string{"X-Token": "abc", "Host": "internal.example"}
		m.Expect.BodyContains = "POST abc internal.example"
		expect(t, run(t, m), model.StatusUp, "")
	})
	t.Run("connection refused", func(t *testing.T) {
		expect(t, run(t, httpMonitor("http://"+closedAddr(t))), model.StatusDown, "connection refused")
	})
}

func TestHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hi") }))
	defer srv.Close()
	m := httpMonitor(srv.URL)
	expect(t, run(t, m), model.StatusDown, "unknown authority")
	m.InsecureSkipVerify = true
	c := run(t, m)
	expect(t, c, model.StatusUp, "")
	if c.CertExpiry.IsZero() || c.Timing.TLS <= 0 {
		t.Errorf("cert expiry %v, tls timing %v", c.CertExpiry, c.Timing.TLS)
	}
	// Plain HTTP spoken to a TLS port.
	plain := httpMonitor(strings.Replace(srv.URL, "https://", "http://", 1))
	c = run(t, plain)
	if c.Status != model.StatusDown {
		t.Errorf("plain HTTP to TLS port: %s %q", c.Status, c.Message)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprint(c, "SSH-2.0-OpenSSH_9.6\r\n")
			c.Close()
		}
	}()
	m := config.Monitor{ID: "t", Type: config.TypeTCP, Address: ln.Addr().String(), Timeout: dur(time.Second)}
	c := run(t, m)
	expect(t, c, model.StatusUp, "")
	if c.Timing == nil || c.Timing.Connect <= 0 {
		t.Error("connect timing missing")
	}
	m.Expect.Banner = "SSH-2.0"
	expect(t, run(t, m), model.StatusUp, "")
	m.Expect.Banner = "220 smtp"
	expect(t, run(t, m), model.StatusDown, `banner "SSH-2.0-OpenSSH_9.6" does not contain "220 smtp"`)
	m.Address = closedAddr(t)
	m.Expect.Banner = ""
	expect(t, run(t, m), model.StatusDown, "connection refused")
}

func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	m := config.Monitor{ID: "t", Type: config.TypeTLS, Address: addr, Timeout: dur(2 * time.Second)}
	expect(t, run(t, m), model.StatusDown, "unknown authority")
	m.InsecureSkipVerify = true
	c := run(t, m)
	expect(t, c, model.StatusUp, "")
	if c.CertExpiry.IsZero() {
		t.Fatal("no cert expiry")
	}
	m.Expect.MinDaysValid = 1_000_000
	expect(t, run(t, m), model.StatusDown, "certificate expires in")
}

// dnsServer answers a fixed zone over UDP.
func dnsServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RecursionAvailable: true})
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
			switch name := q.Name.String(); {
			case name == "app.test." && q.Type == dnsmessage.TypeA:
				_ = b.AResource(rh, dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}})
				_ = b.AResource(rh, dnsmessage.AResource{A: [4]byte{192, 0, 2, 11}})
			case name == "app.test." && q.Type == dnsmessage.TypeTXT:
				_ = b.TXTResource(rh, dnsmessage.TXTResource{TXT: []string{"v=spf1 Include:Mail"}})
			case name == "app.test." && q.Type == dnsmessage.TypeMX:
				_ = b.MXResource(rh, dnsmessage.MXResource{Pref: 10, MX: dnsmessage.MustNewName("MX1.App.Test.")})
			}
			msg, _ := b.Finish()
			_, _ = pc.WriteTo(msg, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	resolver := dnsServer(t)
	m := config.Monitor{ID: "t", Type: config.TypeDNS, Query: "app.test", Record: "A", Resolver: resolver, Timeout: dur(2 * time.Second)}
	expect(t, run(t, m), model.StatusUp, "")
	m.Expect.Values = []string{"192.0.2.11"}
	expect(t, run(t, m), model.StatusUp, "")
	m.Expect.Values = []string{"192.0.2.99"}
	expect(t, run(t, m), model.StatusDown, "expected 192.0.2.99")

	m.Record, m.Expect.Values = "MX", []string{"mx1.app.test"}
	expect(t, run(t, m), model.StatusUp, "")

	m.Record, m.Expect.Values = "TXT", []string{"v=spf1 Include:Mail"}
	expect(t, run(t, m), model.StatusUp, "")
	m.Expect.Values = []string{"v=spf1 include:mail"}
	expect(t, run(t, m), model.StatusDown, "expected")

	m.Record, m.Expect.Values, m.Query = "A", nil, "missing.test"
	c := run(t, m)
	if c.Status != model.StatusDown {
		t.Errorf("empty answer should be down: %+v", c)
	}
}

func TestPingLoopback(t *testing.T) {
	m := config.Monitor{ID: "t", Type: config.TypePing, Address: "127.0.0.1", Timeout: dur(2 * time.Second)}
	c := run(t, m)
	// ICMP depends on the host: Linux needs ping_group_range, and macOS
	// drops loopback echo when the firewall's stealth mode is on. CI sets
	// PHAROS_TEST_PING=1 on runners where ping is known to work.
	if os.Getenv("PHAROS_TEST_PING") != "1" && c.Status == model.StatusDown {
		t.Skip("ICMP unavailable here (set PHAROS_TEST_PING=1 to require it): " + c.Message)
	}
	expect(t, c, model.StatusUp, "")
	if c.Latency < 0 || c.Latency > time.Second { // loopback can measure 0 on coarse clocks
		t.Errorf("latency %v", c.Latency)
	}
}

func TestDescribeDNSFailure(t *testing.T) {
	c := run(t, httpMonitor("http://pharos-does-not-exist.invalid/"))
	if c.Status != model.StatusDown || !strings.Contains(c.Message, "DNS") && !strings.Contains(c.Message, "timed out") {
		t.Errorf("got %q", c.Message)
	}
}

func TestParseStatus(t *testing.T) {
	for in, want := range map[string]statusRange{"200": {200, 200}, "2xx": {200, 299}, "5XX": {500, 599}, "200-204": {200, 204}} {
		got, err := parseStatus(in)
		if err != nil || got != want {
			t.Errorf("parseStatus(%q) = %v %v", in, got, err)
		}
	}
	if _, err := parseStatus("204-200"); err == nil {
		t.Error("inverted range should fail")
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestFailureMessagesNeverContainTheURL(t *testing.T) {
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String(), http.StatusFound) // redirect forever
	}))
	defer loop.Close()
	secret := strings.Repeat("sk_live_", 30)
	c := run(t, httpMonitor(loop.URL+"/v1/health?api_key="+secret))
	if c.Status != model.StatusDown || strings.Contains(c.Message, "sk_live_") || !strings.Contains(c.Message, "redirects") {
		t.Errorf("message %q", c.Message)
	}
}
