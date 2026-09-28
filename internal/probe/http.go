package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/jsonpath"
	"github.com/useless-husband/pharos/internal/model"
)

// maxBody is how much of a response body is read for assertions.
const maxBody = 1 << 20

type statusRange struct{ lo, hi int }

type jsonCheck struct {
	path   jsonpath.Path
	equals any
	exists *bool
}

type httpProber struct {
	m        config.Monitor
	client   *http.Client
	statuses []statusRange
	bodyRE   *regexp.Regexp
	json     []jsonCheck
	needBody bool
}

func newHTTP(m config.Monitor) (*httpProber, error) {
	p := &httpProber{m: m}
	for _, s := range m.Expect.Status {
		r, err := parseStatus(s)
		if err != nil {
			return nil, err
		}
		p.statuses = append(p.statuses, r)
	}
	if len(p.statuses) == 0 {
		p.statuses = []statusRange{{200, 399}}
	}
	if m.Expect.BodyRegex != "" {
		re, err := regexp.Compile(m.Expect.BodyRegex)
		if err != nil {
			return nil, err
		}
		p.bodyRE = re
	}
	for _, j := range m.Expect.JSON {
		path, err := jsonpath.Parse(j.Path)
		if err != nil {
			return nil, err
		}
		p.json = append(p.json, jsonCheck{path: path, equals: j.Equals, exists: j.Exists})
	}
	p.needBody = m.Expect.BodyContains != "" || m.Expect.BodyNotContains != "" || p.bodyRE != nil || len(p.json) > 0

	dialer := &net.Dialer{Timeout: m.Timeout.D()}
	netw := network("tcp", m.IPVersion)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, netw, addr)
		},
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: m.InsecureSkipVerify}, //nolint:gosec // opt-in per monitor
		DisableKeepAlives:      true,                                                  // every check measures a fresh connection
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    m.Timeout.D(),
		ResponseHeaderTimeout:  m.Timeout.D(),
		MaxResponseHeaderBytes: 1 << 20,
	}
	follow := m.FollowRedirects == nil || *m.FollowRedirects
	p.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !follow {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	return p, nil
}

func parseStatus(s string) (statusRange, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 3 && strings.HasSuffix(s, "xx") {
		c := int(s[0]-'0') * 100
		return statusRange{c, c + 99}, nil
	}
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || a > b {
			return statusRange{}, fmt.Errorf("invalid status range %q", s)
		}
		return statusRange{a, b}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return statusRange{}, fmt.Errorf("invalid status %q", s)
	}
	return statusRange{n, n}, nil
}

func (p *httpProber) statusOK(code int) bool {
	for _, r := range p.statuses {
		if code >= r.lo && code <= r.hi {
			return true
		}
	}
	return false
}

func (p *httpProber) Probe(ctx context.Context) model.Check {
	m := p.m
	ctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
	defer cancel()

	var body io.Reader
	if m.Body != "" {
		body = strings.NewReader(m.Body)
	}
	req, err := http.NewRequestWithContext(ctx, m.Method, m.URL, body)
	if err != nil {
		return down("invalid request: "+err.Error(), 0)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Cache-Control", "no-cache")
	for k, v := range m.Headers {
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}

	var (
		timing                                  model.Timing
		dnsStart, connStart, tlsStart, wroteReq time.Time
	)
	start := time.Now()
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:      func(httptrace.DNSDoneInfo) { timing.DNS = since(dnsStart) },
		ConnectStart: func(string, string) { connStart = time.Now() },
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				timing.Connect = since(connStart)
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				timing.TLS = since(tlsStart)
			}
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { wroteReq = time.Now() },
		GotFirstResponseByte: func() { timing.FirstByte = since(wroteReq) },
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := p.client.Do(req)
	if err != nil {
		return down(describe(err, m.Timeout.D()), time.Since(start))
	}
	defer resp.Body.Close()

	var buf []byte
	if p.needBody {
		buf, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	}
	latency := time.Since(start)
	c := model.Check{Status: model.StatusUp, Latency: latency, Timing: &timing}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		c.CertExpiry = resp.TLS.PeerCertificates[0].NotAfter
	}
	if err != nil {
		c.Status, c.Message = model.StatusDown, "reading body: "+describe(err, m.Timeout.D())
		return c
	}
	if msg := p.assert(resp, buf); msg != "" {
		c.Status, c.Message = model.StatusDown, msg
		return c
	}
	return applyLatency(c, m.Expect.MaxLatency.D())
}

func since(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// assert returns a failure message, or "" when every expectation holds.
func (p *httpProber) assert(resp *http.Response, body []byte) string {
	e := p.m.Expect
	if !p.statusOK(resp.StatusCode) {
		return fmt.Sprintf("HTTP %s", resp.Status)
	}
	for k, want := range e.Headers {
		if got := resp.Header.Get(k); !strings.Contains(got, want) {
			if got == "" {
				return fmt.Sprintf("header %s is missing", k)
			}
			return fmt.Sprintf("header %s is %q, expected it to contain %q", k, truncate(got, 60), want)
		}
	}
	if e.BodyContains != "" && !bytes.Contains(body, []byte(e.BodyContains)) {
		return fmt.Sprintf("body does not contain %q", truncate(e.BodyContains, 60))
	}
	if e.BodyNotContains != "" && bytes.Contains(body, []byte(e.BodyNotContains)) {
		return fmt.Sprintf("body contains %q", truncate(e.BodyNotContains, 60))
	}
	if p.bodyRE != nil && !p.bodyRE.Match(body) {
		return fmt.Sprintf("body does not match /%s/", truncate(p.bodyRE.String(), 60))
	}
	if len(p.json) > 0 {
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return "body is not valid JSON"
		}
		for _, j := range p.json {
			v, ok := j.path.Lookup(doc)
			if j.exists != nil {
				if ok != *j.exists {
					if ok {
						return fmt.Sprintf("JSON %s should not exist", j.path)
					}
					return fmt.Sprintf("JSON %s is missing", j.path)
				}
				if !ok {
					continue
				}
			}
			if j.equals != nil {
				if !ok {
					return fmt.Sprintf("JSON %s is missing", j.path)
				}
				if !jsonpath.Equal(v, j.equals) {
					got, _ := json.Marshal(v)
					return fmt.Sprintf("JSON %s is %s, expected %v", j.path, truncate(string(got), 60), j.equals)
				}
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
