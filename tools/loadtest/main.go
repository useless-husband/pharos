// Command loadtest measures Pharos with many monitors: resource use while
// checking, and dashboard response times over a realistic amount of stored
// history. It runs entirely on this machine against a local target server.
//
//	go run ./tools/loadtest -monitors 500 -interval 30s -history 30d -duration 3m
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/web"
)

func main() {
	monitors := flag.Int("monitors", 500, "number of HTTP monitors")
	interval := flag.Duration("interval", 30*time.Second, "check interval")
	historyFlag := flag.String("history", "30d", "history to seed before starting")
	duration := flag.Duration("duration", 3*time.Minute, "how long to run live checks")
	dir := flag.String("dir", "", "working directory (default: a temporary one)")
	verbose := flag.Bool("v", false, "log warnings and errors from Pharos")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of the page requests to this file")
	concurrency := flag.Int("concurrency", 0, "checks allowed at once (default: the engine's default)")
	hang := flag.Float64("hang", 0, "fraction of targets that never answer, as in a network outage (0 to 1)")
	flag.Parse()
	history, err := config.ParseDuration(*historyFlag)
	must(err)
	if *dir == "" {
		*dir, err = os.MkdirTemp("", "pharos-loadtest-")
		must(err)
		defer os.RemoveAll(*dir)
	}
	ctx := context.Background()

	// A local target that answers like a typical health endpoint.
	hangN := int(*hang * float64(*monitors))
	target := startTarget(hangN)
	fmt.Printf("target %s, %d monitors every %s, %s of history", target, *monitors, *interval, *historyFlag)
	if hangN > 0 {
		fmt.Printf(", %d of them unreachable", hangN)
	}
	fmt.Print("\n\n")

	cfg := buildConfig(filepath.Join(*dir, "pharos.db"), target, *monitors, *interval)
	st, err := store.Open(ctx, cfg.Storage.Path)
	must(err)
	defer st.Close()

	t0 := time.Now()
	rows := seed(ctx, st, cfg, time.Now(), history, *interval)
	must(st.Rollup(ctx, time.Now()))
	must(st.Checkpoint(ctx))
	fi, _ := os.Stat(cfg.Storage.Path)
	fmt.Printf("seeded %s rows in %s; database %.0f MB\n", thousands(rows), time.Since(t0).Round(time.Second), float64(fi.Size())/1e6)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *verbose {
		quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	eng := engine.New(engine.Options{Store: st, Logger: quiet, MaxConcurrent: *concurrency})
	ectx, stop := context.WithCancel(ctx)
	must(eng.Start(ectx, cfg))
	srv, err := web.New(ctx, web.Options{Engine: eng, Store: st, Logger: quiet, Version: "loadtest"})
	must(err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, srv.Handler()) //nolint:errcheck
	base := "http://localhost:" + portOf(ln.Addr().String())

	cpuBefore := cpuTime()
	start := time.Now()
	var peakHeap, peakRSS uint64
	var peakG int
	downAt := map[string]time.Duration{} // unreachable monitor -> when it was confirmed down
	for time.Since(start) < *duration {
		time.Sleep(time.Second)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peakHeap = max(peakHeap, ms.HeapInuse)
		peakRSS = max(peakRSS, rss())
		peakG = max(peakG, runtime.NumGoroutine())
		if hangN > 0 {
			for _, m := range eng.Snapshot() {
				if _, seen := downAt[m.Monitor.ID]; !seen && m.Status == model.StatusDown {
					downAt[m.Monitor.ID] = time.Since(start)
				}
			}
		}
	}
	elapsed := time.Since(start)
	// Count by timestamp: the hourly prune deletes expired rows during the
	// run, so a difference of totals would undercount.
	checks := countChecks(ctx, st, start)
	cpu := cpuTime() - cpuBefore

	fmt.Printf("\n## Live checking (%s)\n\n", elapsed.Round(time.Second))
	fmt.Printf("| Metric | Value |\n|---|---|\n")
	// Without queueing, a healthy monitor is checked every interval and an
	// unreachable one every timeout + retry interval.
	mon := cfg.Monitors[0]
	expected := float64(*monitors-hangN)/interval.Seconds() + float64(hangN)/(mon.Timeout.D()+mon.RetryInterval.D()).Seconds()
	fmt.Printf("| Checks completed | %s (%.1f/s; without queueing %.1f/s) |\n", thousands(checks), float64(checks)/elapsed.Seconds(), expected)
	if hangN > 0 {
		var ds []time.Duration
		for _, d := range downAt {
			ds = append(ds, d)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		line := fmt.Sprintf("%d of %d", len(ds), hangN)
		if len(ds) > 0 {
			line += fmt.Sprintf("; median %s, slowest %s after the start", ds[len(ds)/2].Round(time.Second), ds[len(ds)-1].Round(time.Second))
		}
		fmt.Printf("| Unreachable monitors confirmed down | %s |\n", line)
	}
	if cpuBefore >= 0 {
		fmt.Printf("| CPU time | %s (%.1f%% of one core) |\n", cpu.Round(time.Millisecond), 100*cpu.Seconds()/elapsed.Seconds())
	}
	fmt.Printf("| Peak Go heap in use | %.0f MB |\n", float64(peakHeap)/1e6)
	if peakRSS > 0 {
		fmt.Printf("| Peak resident memory (whole process) | %.0f MB |\n", float64(peakRSS)/1e6)
	}
	fmt.Printf("| Peak goroutines | %d |\n", peakG)

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		must(err)
		must(pprof.StartCPUProfile(f))
		defer pprof.StopCPUProfile()
	}
	fmt.Printf("\n## Page response times (20 requests each, while checking)\n\n| Page | median | p95 |\n|---|---|---|\n")
	for _, p := range []struct{ name, path string }{
		{"Status page", "/"},
		{"Status JSON", "/api/v1/status"},
		{"Dashboard overview", "/admin"},
		{"Monitor, 24h chart", "/admin/monitors/m-0001"},
		{"Monitor, 30d chart", "/admin/monitors/m-0001?range=30d"},
		{"Metrics", "/metrics"},
	} {
		med, p95 := timeRequests(base+p.path, 20)
		fmt.Printf("| %s | %.1f ms | %.1f ms |\n", p.name, float64(med.Microseconds())/1000, float64(p95.Microseconds())/1000)
	}
	stop()
	eng.Wait()
}

func buildConfig(dbPath, target string, n int, interval time.Duration) *config.Config {
	var b strings.Builder
	fmt.Fprintf(&b, "storage: {path: %q}\nstatus_page:\n  groups:\n    - name: Public\n      monitors: [", dbPath)
	public := max(1, n/5)
	for i := 0; i < public; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "m-%04d", i+1)
	}
	fmt.Fprintf(&b, "]\ndefaults: {interval: %s}\nmonitors:\n", config.FormatDuration(interval))
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  - {id: m-%04d, type: http, url: \"%s/svc/%d\", expect: {max_latency: 1s}}\n", i+1, target, i+1)
	}
	cfg, err := config.Parse([]byte(b.String()), "", func(string) (string, bool) { return "", false })
	must(err)
	return cfg
}

// seed writes checks with a few outages over the history. The last 25 hours
// are written at the real check interval, because that is what the raw-data
// queries (24-hour charts, recent checks) read; older history, which pages
// only read through hourly aggregates, is written every 10 minutes to keep
// seeding fast. Row counts, and so the database size, are therefore smaller
// than a real instance with the same retention: see docs/performance.md.
//
// Checks are inserted in time order across monitors, as a running instance
// writes them, so the database's layout on disk matches a real one.
func seed(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time, history, interval time.Duration) int {
	rng := rand.New(rand.NewPCG(1, 2))
	from := now.Add(-history)
	// Status periods cover 90 days, like the status page's history bars,
	// with about one outage per monitor every two weeks.
	periodsFrom := now.Add(-max(history, 90*24*time.Hour))
	for _, m := range cfg.Monitors {
		must(st.EnsureMonitor(ctx, m.ID, periodsFrom))
		_, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: periodsFrom})
		must(err)
		for t := periodsFrom.Add(time.Duration(rng.Int64N(int64(14 * 24 * time.Hour)))); t.Before(now.Add(-2 * time.Hour)); t = t.Add(time.Duration(7*24+rng.IntN(14*24)) * time.Hour) {
			_, err = st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusDown, At: t, Cause: "HTTP 503 Service Unavailable"})
			must(err)
			_, err = st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: t.Add(time.Duration(2+rng.IntN(60)) * time.Minute)})
			must(err)
		}
	}
	total := 0
	var batch []model.Check
	dense := now.Add(-25 * time.Hour)
	n := len(cfg.Monitors)
	for t := from; t.Before(now); {
		step := 10 * time.Minute
		if !t.Before(dense) {
			step = interval
		}
		// Spread the monitors over the step, as start-up jitter does.
		for i, m := range cfg.Monitors {
			at := t.Add(step * time.Duration(i) / time.Duration(n))
			if !at.Before(now) {
				break
			}
			c := model.Check{MonitorID: m.ID, At: at, Status: model.StatusUp,
				Latency: time.Duration(20+rng.IntN(60)) * time.Millisecond, Timing: &model.Timing{DNS: time.Millisecond, Connect: time.Millisecond, FirstByte: 15 * time.Millisecond}}
			if rng.IntN(2000) == 0 {
				c.Status, c.Message = model.StatusDown, "HTTP 503 Service Unavailable"
			}
			batch = append(batch, c)
		}
		if len(batch) >= 20000 {
			must(st.InsertChecks(ctx, batch))
			total += len(batch)
			batch = batch[:0]
		}
		t = t.Add(step)
	}
	must(st.InsertChecks(ctx, batch))
	return total + len(batch)
}

// startTarget serves the monitored endpoints /svc/1 to /svc/N. The first
// hang of them never answer, like hosts behind a failed network link: the
// check waits for its full timeout.
func startTarget(hang int) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:errcheck
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/svc/%d", &n); err == nil && n <= hang {
			<-r.Context().Done()
			return
		}
		time.Sleep(time.Duration(2+rand.IntN(20)) * time.Millisecond)
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	return "http://" + ln.Addr().String()
}

func countChecks(ctx context.Context, st *store.Store, since time.Time) int {
	n, err := st.CountChecks(ctx, since)
	must(err)
	return n
}

func timeRequests(url string, n int) (median, p95 time.Duration) {
	var ds []time.Duration
	client := &http.Client{Timeout: time.Minute}
	for i := 0; i < n; i++ {
		t := time.Now()
		resp, err := client.Get(url)
		must(err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			must(fmt.Errorf("%s: HTTP %d", url, resp.StatusCode))
		}
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2], ds[(n*95+99)/100-1]
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}
