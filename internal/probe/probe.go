// Package probe implements the network checks behind each monitor type.
//
// A Prober performs one check and reports a model.Check with Status, Latency,
// an optional phase Timing and a one-line Message explaining any failure.
// Probers never retry: confirming a failure is the engine's job.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// Prober performs a single check.
type Prober interface {
	Probe(ctx context.Context) model.Check
}

// UserAgent is sent with HTTP checks. It is overridden at build time.
var UserAgent = "Pharos (+https://github.com/useless-husband/pharos)"

// New returns the prober for a monitor. Push monitors have no prober.
func New(m config.Monitor) (Prober, error) {
	switch m.Type {
	case config.TypeHTTP:
		return newHTTP(m)
	case config.TypeTCP:
		return &tcpProber{m: m}, nil
	case config.TypeTLS:
		return &tlsProber{m: m}, nil
	case config.TypeDNS:
		return newDNS(m), nil
	case config.TypePing:
		return &pingProber{m: m}, nil
	}
	return nil, fmt.Errorf("monitor type %q has no prober", m.Type)
}

func network(base, ipVersion string) string {
	if ipVersion == "4" || ipVersion == "6" {
		return base + ipVersion
	}
	return base
}

func down(msg string, latency time.Duration) model.Check {
	return model.Check{Status: model.StatusDown, Message: msg, Latency: latency}
}

// applyLatency downgrades an otherwise healthy check that was too slow.
func applyLatency(c model.Check, limit time.Duration) model.Check {
	if c.Status == model.StatusUp && limit > 0 && c.Latency > limit {
		c.Status = model.StatusDegraded
		c.Message = fmt.Sprintf("slow response: %s (limit %s)", roundDur(c.Latency), roundDur(limit))
	}
	return c
}

func roundDur(d time.Duration) string {
	switch {
	case d >= 10*time.Second:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

// describe turns a network error into a short, specific sentence.
func describe(err error, timeout time.Duration) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	var certInvalid x509.CertificateInvalidError
	var unknownAuth x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var recordErr tls.RecordHeaderError
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err):
		return fmt.Sprintf("timed out after %s", roundDur(timeout))
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return fmt.Sprintf("DNS: no such host %s", dnsErr.Name)
		}
		if dnsErr.IsTimeout {
			return fmt.Sprintf("DNS lookup for %s timed out", dnsErr.Name)
		}
		return "DNS: " + dnsErr.Err
	case errors.As(err, &certInvalid):
		switch certInvalid.Reason {
		case x509.Expired:
			return "TLS: certificate has expired or is not yet valid"
		default:
			return "TLS: invalid certificate: " + certInvalid.Error()
		}
	case errors.As(err, &unknownAuth):
		return "TLS: certificate signed by an unknown authority"
	case errors.As(err, &hostnameErr):
		return "TLS: certificate is not valid for " + hostnameErr.Host
	case errors.As(err, &recordErr):
		return "TLS: server did not respond with TLS (is this port plain HTTP?)"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset by peer"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "server closed the connection without a response"
	case errors.As(err, &opErr) && opErr.Err != nil:
		return describe(opErr.Err, timeout)
	}
	msg := err.Error()
	// Strip Go's verbose request prefix: `Get "https://x": ...`.
	if i := strings.Index(msg, "\": "); i >= 0 && i < 200 {
		msg = msg[i+3:]
	}
	return msg
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
