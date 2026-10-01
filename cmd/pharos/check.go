package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/probe"
)

const checkUsage = `Usage: pharos check [-c file] [-json] [monitor...]

Probe monitors once from this machine and print the result: status, response
time, the time spent in each phase and the reason for a failure. Nothing is
stored and no notification is sent. Without monitor ids, every monitor except
push monitors is checked.

Exits 0 when every check is up or degraded, 1 when one is down, 2 when the
configuration cannot be read or is invalid or a monitor id is unknown, and
130 when interrupted.

Flags:
`

// checkResult is one probe outcome, as printed by "pharos check -json".
type checkResult struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Target     string        `json:"target"`
	Status     model.Status  `json:"status"`
	LatencyMS  float64       `json:"latency_ms"`
	Message    string        `json:"message,omitempty"`
	Timing     *timingMS     `json:"timing_ms,omitempty"`
	CertExpiry *time.Time    `json:"cert_expiry,omitempty"`
	check      model.Check   `json:"-"`
	warnCert   time.Duration `json:"-"`
}

type timingMS struct {
	DNS       float64 `json:"dns"`
	Connect   float64 `json:"connect"`
	TLS       float64 `json:"tls"`
	FirstByte float64 `json:"first_byte"`
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	fs.StringVar(cfgPath, "c", defaultConfigPath(), "configuration file (shorthand)")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), checkUsage)
		fs.PrintDefaults()
	}
	ids := parseInterspersed(fs, args)
	usageError := func(err error) {
		var ce *config.Error
		if errors.As(err, &ce) {
			fmt.Fprintln(os.Stderr, ce.Error())
		} else {
			fmt.Fprintln(os.Stderr, "pharos:", err)
		}
		os.Exit(2)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		usageError(err) // including a missing or unreadable file
	}
	monitors, skipped, err := selectMonitors(cfg, ids)
	if err != nil {
		usageError(err)
	}
	setUserAgent()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err := runChecks(ctx, cfg, monitors)
	if errors.Is(err, context.Canceled) {
		// Checks cut short report a failure that did not happen: print none.
		fmt.Fprintln(os.Stderr, "pharos: interrupted")
		os.Exit(130)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		printChecks(os.Stdout, results, skipped, time.Now(), useColor(os.Stdout))
	}
	for _, r := range results {
		if r.Status == model.StatusDown {
			os.Exit(1)
		}
	}
	return nil
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments ("pharos check api -json"), which the flag package
// alone does not allow. A "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...)
		}
		if len(rest) == 0 {
			return pos
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// selectMonitors resolves the ids given on the command line, or picks every
// monitor that can be probed. skipped counts push monitors left out.
func selectMonitors(cfg *config.Config, ids []string) (ms []config.Monitor, skipped int, err error) {
	if len(ids) == 0 {
		for _, m := range cfg.Monitors {
			if m.Type == config.TypePush {
				skipped++
				continue
			}
			ms = append(ms, m)
		}
		if len(ms) == 0 {
			return nil, skipped, fmt.Errorf("no monitors to check: push monitors are checked by their heartbeats")
		}
		return ms, skipped, nil
	}
	seen := map[string]bool{}
	for _, id := range ids {
		m, ok := cfg.MonitorByID(id)
		if !ok {
			all := make([]string, len(cfg.Monitors))
			for i, m := range cfg.Monitors {
				all[i] = m.ID
			}
			return nil, 0, fmt.Errorf("no monitor %q (configured: %s)", id, strings.Join(all, ", "))
		}
		if m.Type == config.TypePush {
			return nil, 0, fmt.Errorf("%s is a push monitor: it is checked by its heartbeats, there is nothing to probe", id)
		}
		if !seen[id] {
			seen[id] = true
			ms = append(ms, m)
		}
	}
	return ms, 0, nil
}

// runChecks probes the monitors concurrently and returns the results in the
// order given.
func runChecks(ctx context.Context, cfg *config.Config, monitors []config.Monitor) ([]checkResult, error) {
	probers := make([]probe.Prober, len(monitors))
	for i, m := range monitors {
		p, err := probe.New(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.ID, err)
		}
		probers[i] = p
	}
	results := make([]checkResult, len(monitors))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, m := range monitors {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			c := probers[i].Probe(ctx)
			r := checkResult{ID: m.ID, Type: m.Type, Target: m.Target(), Status: c.Status,
				LatencyMS: msOf(c.Latency), Message: c.Message, check: c, warnCert: cfg.CertExpiryWarnFor(m)}
			if t := c.Timing; t != nil {
				r.Timing = &timingMS{DNS: msOf(t.DNS), Connect: msOf(t.Connect), TLS: msOf(t.TLS), FirstByte: msOf(t.FirstByte)}
			}
			if !c.CertExpiry.IsZero() {
				exp := c.CertExpiry.UTC()
				r.CertExpiry = &exp
			}
			results[i] = r
		})
	}
	wg.Wait()
	return results, ctx.Err()
}

func msOf(d time.Duration) float64 {
	return math.Round(float64(d.Microseconds())) / 1000
}

// printChecks writes one block per result:
//
//	api       up        142ms  https://api.example.com/health
//	                           dns 12ms · connect 20ms · tls 45ms · first byte 60ms
//	                           certificate valid until 2026-12-01 (61 days)
func printChecks(w io.Writer, results []checkResult, skipped int, now time.Time, color bool) {
	idw := len("MONITOR")
	for _, r := range results {
		idw = max(idw, len(r.ID))
	}
	indent := strings.Repeat(" ", idw+2+10+8)
	fmt.Fprintf(w, "%-*s  %-10s%-8s%s\n", idw, "MONITOR", "STATUS", "TIME", "TARGET")
	counts := map[model.Status]int{}
	for _, r := range results {
		counts[r.Status]++
		status := fmt.Sprintf("%-10s", r.Status)
		if color {
			status = paint(r.Status, status)
		}
		fmt.Fprintf(w, "%-*s  %s%-8s%s\n", idw, r.ID, status, fmtDur(r.check.Latency), r.Target)
		if t := r.check.Timing; t != nil {
			var phases []string
			for _, p := range []struct {
				name string
				d    time.Duration
			}{{"dns", t.DNS}, {"connect", t.Connect}, {"tls", t.TLS}, {"first byte", t.FirstByte}} {
				if p.d > 0 {
					phases = append(phases, p.name+" "+fmtDur(p.d))
				}
			}
			if len(phases) > 0 {
				fmt.Fprintf(w, "%s%s\n", indent, strings.Join(phases, " · "))
			}
		}
		if r.Message != "" {
			fmt.Fprintf(w, "%s%s\n", indent, r.Message)
		}
		if exp := r.check.CertExpiry; !exp.IsZero() {
			days := int(exp.Sub(now).Hours() / 24)
			line := fmt.Sprintf("certificate valid until %s (%s)", exp.UTC().Format("2006-01-02"), count(days, "day"))
			if exp.Sub(now) < r.warnCert {
				line += ", inside the warning period"
			}
			fmt.Fprintf(w, "%s%s\n", indent, line)
		}
	}
	if len(results) > 1 || skipped > 0 {
		var parts []string
		for _, s := range []model.Status{model.StatusUp, model.StatusDegraded, model.StatusDown} {
			if counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
			}
		}
		summary := fmt.Sprintf("\n%s checked: %s", count(len(results), "monitor"), strings.Join(parts, ", "))
		if skipped > 0 {
			summary += fmt.Sprintf("; %s skipped (checked by heartbeats)", count(skipped, "push monitor"))
		}
		fmt.Fprintln(w, summary+".")
	}
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= 10*time.Second:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
	default:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
}

func useColor(f *os.File) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd()))
}

func paint(s model.Status, text string) string {
	code := map[model.Status]string{model.StatusUp: "32", model.StatusDegraded: "33", model.StatusDown: "31"}[s]
	if code == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
