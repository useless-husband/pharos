package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// tcpProber checks that a port accepts connections and, optionally, that
// the server's greeting contains an expected banner.
type tcpProber struct{ m config.Monitor }

func (p *tcpProber) Probe(ctx context.Context) model.Check {
	m := p.m
	ctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
	defer cancel()
	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, network("tcp", m.IPVersion), m.Address)
	if err != nil {
		return down(describe(err, m.Timeout.D()), time.Since(start))
	}
	defer conn.Close()
	connect := time.Since(start)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if m.Send != "" {
		if _, err := conn.Write([]byte(m.Send)); err != nil {
			return down("write: "+describe(err, m.Timeout.D()), time.Since(start))
		}
	}
	if want := m.Expect.Banner; want != "" {
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 1024)
		for !strings.Contains(string(buf), want) && len(buf) < cap(buf) {
			n, err := conn.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err != nil {
				break
			}
		}
		if !strings.Contains(string(buf), want) {
			got := strings.TrimSpace(string(buf))
			if got == "" {
				return down(fmt.Sprintf("no banner received (expected %q)", want), time.Since(start))
			}
			return down(fmt.Sprintf("banner %q does not contain %q", truncate(got, 60), want), time.Since(start))
		}
	}
	c := model.Check{Status: model.StatusUp, Latency: time.Since(start), Timing: &model.Timing{Connect: connect}}
	return applyLatency(c, m.Expect.MaxLatency.D())
}

// tlsProber performs a TLS handshake and inspects the certificate.
type tlsProber struct{ m config.Monitor }

func (p *tlsProber) Probe(ctx context.Context) model.Check {
	m := p.m
	ctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
	defer cancel()
	host, _, _ := net.SplitHostPort(m.Address)
	sni := m.ServerName
	if sni == "" {
		sni = host
	}
	start := time.Now()
	var nd net.Dialer
	raw, err := nd.DialContext(ctx, network("tcp", m.IPVersion), m.Address)
	if err != nil {
		return down(describe(err, m.Timeout.D()), time.Since(start))
	}
	defer raw.Close()
	connect := time.Since(start)
	conn := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: m.InsecureSkipVerify}) //nolint:gosec // opt-in per monitor
	hsStart := time.Now()
	if err := conn.HandshakeContext(ctx); err != nil {
		return down(describe(err, m.Timeout.D()), time.Since(start))
	}
	hs := time.Since(hsStart)
	st := conn.ConnectionState()
	c := model.Check{Status: model.StatusUp, Latency: time.Since(start), Timing: &model.Timing{Connect: connect, TLS: hs}}
	if len(st.PeerCertificates) == 0 {
		return down("TLS: server presented no certificate", c.Latency)
	}
	leaf := st.PeerCertificates[0]
	c.CertExpiry = leaf.NotAfter
	left := time.Until(leaf.NotAfter)
	if left <= 0 {
		c.Status, c.Message = model.StatusDown, "TLS: certificate expired "+leaf.NotAfter.UTC().Format("2006-01-02")
		return c
	}
	// Compare in days: a large minimum would overflow time.Duration.
	daysLeft := left.Hours() / 24
	if minDays := m.Expect.MinDaysValid; minDays > 0 && daysLeft < float64(minDays) {
		c.Status = model.StatusDown
		c.Message = fmt.Sprintf("TLS: certificate expires in %d days (minimum %d)", int(math.Floor(daysLeft)), minDays)
		return c
	}
	return applyLatency(c, m.Expect.MaxLatency.D())
}

// dnsProber resolves a name and checks the answer.
type dnsProber struct {
	m        config.Monitor
	resolver *net.Resolver
}

func newDNS(m config.Monitor) *dnsProber {
	r := net.DefaultResolver
	if m.Resolver != "" {
		server := m.Resolver
		r = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, netw, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, netw, server)
			},
		}
	}
	return &dnsProber{m: m, resolver: r}
}

func (p *dnsProber) Probe(ctx context.Context) model.Check {
	m := p.m
	ctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
	defer cancel()
	start := time.Now()
	values, err := p.lookup(ctx)
	latency := time.Since(start)
	if err != nil {
		return down(describe(err, m.Timeout.D()), latency)
	}
	if len(values) == 0 {
		return down(fmt.Sprintf("no %s records for %s", m.Record, m.Query), latency)
	}
	for _, want := range m.Expect.Values {
		w := strings.TrimSpace(want)
		if m.Record != "TXT" { // TXT data is case-sensitive; names and IPs are not
			w = normalizeDNS(w)
		}
		if !slices.Contains(values, w) {
			return down(fmt.Sprintf("%s %s answered %s, expected %s", m.Record, m.Query, truncate(strings.Join(values, ", "), 80), want), latency)
		}
	}
	c := model.Check{Status: model.StatusUp, Latency: latency, Timing: &model.Timing{DNS: latency}}
	return applyLatency(c, m.Expect.MaxLatency.D())
}

func (p *dnsProber) lookup(ctx context.Context) ([]string, error) {
	var out []string
	switch p.m.Record {
	case "A", "AAAA":
		fam := "ip4"
		if p.m.Record == "AAAA" {
			fam = "ip6"
		}
		ips, err := p.resolver.LookupIP(ctx, fam, p.m.Query)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			out = append(out, ip.String())
		}
	case "CNAME":
		cname, err := p.resolver.LookupCNAME(ctx, p.m.Query)
		if err != nil {
			return nil, err
		}
		out = append(out, normalizeDNS(cname))
	case "MX":
		mxs, err := p.resolver.LookupMX(ctx, p.m.Query)
		if err != nil {
			return nil, err
		}
		for _, mx := range mxs {
			out = append(out, normalizeDNS(mx.Host))
		}
	case "TXT":
		txts, err := p.resolver.LookupTXT(ctx, p.m.Query)
		if err != nil {
			return nil, err
		}
		out = append(out, txts...)
	case "NS":
		nss, err := p.resolver.LookupNS(ctx, p.m.Query)
		if err != nil {
			return nil, err
		}
		for _, ns := range nss {
			out = append(out, normalizeDNS(ns.Host))
		}
	default:
		return nil, fmt.Errorf("unsupported record type %s", p.m.Record)
	}
	return out, nil
}

// normalizeDNS makes names comparable: lower case, no trailing dot.
// IP addresses are canonicalized so "::0001" matches "::1".
func normalizeDNS(s string) string {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(strings.ToLower(s), ".")
}
