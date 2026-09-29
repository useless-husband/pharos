// Package demo builds a self-contained demonstration: a configuration,
// ninety days of plausible history and simulated live checks. Nothing in it
// touches the network. It powers `pharos demo` and the public demo site.
package demo

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
	"github.com/useless-husband/pharos/internal/notify"
	"github.com/useless-husband/pharos/internal/probe"
	"github.com/useless-husband/pharos/internal/store"
)

// names localizes the demo's visible text.
var names = map[string]map[string]string{
	"en": {
		"title": "Acme Cloud Status", "desc": "Live status of Acme Cloud services. This is a demonstration with simulated data.",
		"g1": "Website and API", "g2": "Infrastructure", "support": "Support",
		"website": "Website", "api": "Public API", "auth": "Sign-in", "cdn": "Static files (CDN)", "dns": "DNS",
		"mail": "Email delivery", "db": "Primary database", "backup": "Nightly backup",
		"m1": "Database upgrade", "m2": "Mail relay migration",
	},
	"zh-TW": {
		"title": "Acme 雲端服務狀態", "desc": "Acme 雲端各項服務的即時狀態。本頁為示範，資料皆為模擬產生。",
		"g1": "網站與 API", "g2": "基礎設施", "support": "客服中心",
		"website": "官方網站", "api": "公開 API", "auth": "會員登入", "cdn": "靜態檔案（CDN）", "dns": "DNS",
		"mail": "電子郵件寄送", "db": "主資料庫", "backup": "每日備份",
		"m1": "資料庫升級", "m2": "郵件主機遷移",
	},
}

// Config returns the demo configuration, storing data at dbPath.
func Config(dbPath, lang, listen string) (*config.Config, error) {
	n := names["en"]
	if lang == "zh-TW" {
		n = names["zh-TW"]
	}
	src := fmt.Sprintf(`
server:
  listen: %q
storage:
  path: %q
status_page:
  title: %q
  description: %q
  language: %s
  timezone: Asia/Taipei
  groups:
    - name: %q
      monitors: [website, api, auth]
    - name: %q
      monitors: [cdn, dns, mail]
  links:
    - label: %q
      url: https://example.com/support
defaults:
  interval: 30s
  notify: [team-chat, on-call]
notifiers:
  - name: team-chat
    type: discord
    url: https://discord.com/api/webhooks/0/demo
  - name: on-call
    type: ntfy
    url: https://ntfy.sh/acme-demo-alerts
    events: [down, up, reminder]
monitors:
  - id: website
    name: %q
    type: http
    url: https://www.example.com/
    expect: { max_latency: 1500ms }
  - id: api
    name: %q
    type: http
    url: https://api.example.com/health
    expect:
      json: [{ path: status, equals: ok }]
      max_latency: 800ms
  - id: auth
    name: %q
    type: http
    url: https://login.example.com/healthz
  - id: cdn
    name: %q
    type: http
    url: https://cdn.example.com/ping
  - id: dns
    name: %q
    type: dns
    query: example.com
    record: A
  - id: mail
    name: %q
    type: tcp
    address: smtp.example.com:587
    expect: { banner: "220" }
  - id: db
    name: %q
    type: tcp
    address: db.internal:5432
  - id: backup
    name: %q
    type: push
    heartbeat: 24h
    grace: 1h
maintenance:
  - name: %q
    monitors: [db]
    days: [sun]
    at: "03:00"
    duration: 1h
  - name: %q
    monitors: [mail]
    start: %q
    end: %q
`, listen, dbPath, n["title"], n["desc"], lang, n["g1"], n["g2"], n["support"],
		n["website"], n["api"], n["auth"], n["cdn"], n["dns"], n["mail"], n["db"], n["backup"],
		n["m1"], n["m2"],
		time.Now().AddDate(0, 0, 2).Format("2006-01-02")+" 01:00", time.Now().AddDate(0, 0, 2).Format("2006-01-02")+" 02:30")
	return config.Parse([]byte(src), "", func(string) (string, bool) { return "", false })
}

type profile struct {
	base   time.Duration // typical latency
	spread float64       // noise factor
	timing bool
	cert   time.Duration // time until certificate expiry
}

var profiles = map[string]profile{
	"website": {140 * time.Millisecond, 0.25, true, 64 * 24 * time.Hour},
	"api":     {190 * time.Millisecond, 0.30, true, 64 * 24 * time.Hour},
	"auth":    {95 * time.Millisecond, 0.20, true, 64 * 24 * time.Hour},
	"cdn":     {38 * time.Millisecond, 0.35, true, 11 * 24 * time.Hour},
	"dns":     {14 * time.Millisecond, 0.40, false, 0},
	"mail":    {45 * time.Millisecond, 0.20, false, 0},
	"db":      {3 * time.Millisecond, 0.30, false, 0},
}

type episode struct {
	start, end time.Time
	status     model.Status // Down, Degraded or Maintenance
	cause      string
}

// history returns the scripted episodes of each monitor, relative to now.
func history(now time.Time) map[string][]episode {
	day := func(d int, h, m int) time.Time {
		t := now.AddDate(0, 0, -d).In(taipei)
		return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, taipei)
	}
	return map[string][]episode{
		"api": {
			{day(3, 14, 12), day(3, 14, 35), model.StatusDown, "HTTP 503 Service Unavailable"},
			{day(1, 9, 40), day(1, 11, 5), model.StatusDegraded, "slow response: 1.9s (limit 800ms)"},
			{day(27, 2, 5), day(27, 2, 19), model.StatusDown, `JSON status is "draining", expected ok`},
			{day(61, 18, 30), day(61, 19, 2), model.StatusDown, "HTTP 502 Bad Gateway"},
		},
		"website": {
			{day(12, 21, 3), day(12, 21, 11), model.StatusDown, "timed out after 10s"},
		},
		"auth": {
			{day(45, 10, 0), day(45, 10, 48), model.StatusDown, "HTTP 500 Internal Server Error"},
		},
		"cdn": {
			{day(40, 4, 10), day(40, 6, 2), model.StatusDown, "timed out after 10s"},
			{day(8, 20, 0), day(8, 20, 45), model.StatusDegraded, "slow response: 1.4s (limit 1s)"},
		},
		"mail": {
			{day(20, 1, 0), day(20, 2, 30), model.StatusMaintenance, ""},
		},
		"db": {
			{day(5, 7, 15), day(5, 7, 21), model.StatusDown, "connection refused"},
		},
	}
}

var taipei = func() *time.Location {
	l, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return l
}()

func statusAt(eps []episode, t time.Time) (model.Status, string) {
	for _, e := range eps {
		if !t.Before(e.start) && t.Before(e.end) {
			return e.status, e.cause
		}
	}
	return model.StatusUp, ""
}

// latency draws a plausible response time: a daily traffic curve plus
// log-normal noise.
func latency(rng *rand.Rand, p profile, t time.Time) time.Duration {
	hour := float64(t.In(taipei).Hour()) + float64(t.Minute())/60
	daily := 1 + 0.22*math.Sin((hour-8)/24*2*math.Pi)
	noise := math.Exp(rng.NormFloat64() * p.spread)
	if rng.Float64() < 0.01 {
		noise *= 2.5 + rng.Float64()*2 // occasional slow request
	}
	return time.Duration(float64(p.base) * daily * noise)
}

func timing(rng *rand.Rand, total time.Duration) *model.Timing {
	dns := time.Duration(2+rng.IntN(8)) * time.Millisecond
	conn := time.Duration(8+rng.IntN(12)) * time.Millisecond
	tlsD := time.Duration(18+rng.IntN(20)) * time.Millisecond
	rest := time.Duration(1+rng.IntN(4)) * time.Millisecond
	ttfb := total - dns - conn - tlsD - rest
	if ttfb < time.Millisecond {
		ttfb = time.Millisecond
	}
	return &model.Timing{DNS: dns, Connect: conn, TLS: tlsD, FirstByte: ttfb}
}

// Seed writes 90 days of history ending at now.
func Seed(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time) error {
	rng := rand.New(rand.NewPCG(20260929, 7))
	start := now.AddDate(0, 0, -90).Truncate(time.Hour)
	eps := history(now)
	for _, m := range cfg.Monitors {
		first := start
		if m.ID == "dns" {
			first = now.AddDate(0, 0, -34) // added later: shows "no data" days
		}
		if err := st.EnsureMonitor(ctx, m.ID, first); err != nil {
			return err
		}
		if m.Type == config.TypePush {
			if err := seedPush(ctx, st, m, first, now, rng); err != nil {
				return err
			}
			continue
		}
		p := profiles[m.ID]
		mine := eps[m.ID]
		sort.Slice(mine, func(i, j int) bool { return mine[i].start.Before(mine[j].start) })

		// Status history.
		if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: first}); err != nil {
			return err
		}
		for _, e := range mine {
			if e.start.Before(first) {
				continue
			}
			if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: e.status, At: e.start, Cause: e.cause}); err != nil {
				return err
			}
			res := "recovered"
			if e.status == model.StatusMaintenance {
				res = ""
			}
			if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: e.end, Resolution: res}); err != nil {
				return err
			}
			if e.status == model.StatusDown {
				seedDeliveries(ctx, st, m.ID, e)
			}
		}

		// Checks: every 10 minutes, and every minute for the last day.
		var batch []model.Check
		for t := first; t.Before(now); {
			step := 10 * time.Minute
			if now.Sub(t) <= 24*time.Hour {
				step = time.Minute
			}
			status, cause := statusAt(mine, t)
			c := model.Check{MonitorID: m.ID, At: t, Status: model.StatusUp, Latency: latency(rng, p, t)}
			switch status {
			case model.StatusDown:
				c.Status, c.Message = model.StatusDown, cause
				if cause == "timed out after 10s" {
					c.Latency = 10 * time.Second
				}
			case model.StatusDegraded:
				c.Status, c.Message = model.StatusDegraded, cause
				c.Latency = time.Duration(float64(c.Latency) * (6 + rng.Float64()*4))
			case model.StatusMaintenance:
				c.Maintenance = true
			}
			if p.timing && c.Status != model.StatusDown {
				c.Timing = timing(rng, c.Latency)
			}
			if p.cert > 0 {
				c.CertExpiry = now.Add(p.cert).Truncate(24 * time.Hour)
			}
			batch = append(batch, c)
			t = t.Add(step)
		}
		if err := st.InsertChecks(ctx, batch); err != nil {
			return err
		}
	}
	return st.Rollup(ctx, now)
}

func seedPush(ctx context.Context, st *store.Store, m config.Monitor, first, now time.Time, rng *rand.Rand) error {
	if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: first}); err != nil {
		return err
	}
	var batch []model.Check
	missed := now.AddDate(0, 0, -19)
	for d := first; d.Before(now); d = d.AddDate(0, 0, 1) {
		lt := d.In(taipei)
		run := time.Date(lt.Year(), lt.Month(), lt.Day(), 2, 10+rng.IntN(15), 0, 0, taipei)
		if run.After(now) || run.Before(first) {
			continue
		}
		if run.YearDay() == missed.In(taipei).YearDay() && run.Year() == missed.Year() {
			// The job hung: the heartbeat was late and the monitor went down.
			downAt := run.Add(-24*time.Hour + 25*time.Hour + time.Hour)
			if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusDown, At: downAt,
				Cause: "no heartbeat received for 1d1h (expected every 1d)"}); err != nil {
				return err
			}
			run = downAt.Add(95 * time.Minute)
			if _, err := st.ApplyTransition(ctx, store.Transition{MonitorID: m.ID, To: model.StatusUp, At: run, Resolution: "recovered"}); err != nil {
				return err
			}
		}
		batch = append(batch, model.Check{MonitorID: m.ID, At: run, Status: model.StatusUp,
			Latency: time.Duration(140+rng.IntN(90)) * time.Second})
	}
	return st.InsertChecks(ctx, batch)
}

func seedDeliveries(ctx context.Context, st *store.Store, id string, e episode) {
	for _, ev := range []struct {
		kind string
		at   time.Time
	}{{"down", e.start}, {"up", e.end}} {
		for _, n := range []string{"team-chat", "on-call"} {
			nid, err := st.AddNotification(ctx, store.NotificationLog{Created: ev.at, MonitorID: id, Event: ev.kind, Notifier: n})
			if err == nil {
				_ = st.UpdateNotification(ctx, nid, store.NotifyDelivered, 1, "", ev.at.Add(800*time.Millisecond))
			}
		}
	}
}

// Sender pretends to deliver notifications, so the demo never contacts the
// real services named in its configuration.
type Sender struct{}

func (Sender) Send(ctx context.Context, _ notify.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

// NewSender returns the fake sender for any notifier.
func NewSender(config.Notifier) (notify.Sender, error) { return Sender{}, nil }

// Prober simulates live checks for the demo monitors.
type Prober struct {
	m   config.Monitor
	rng *rand.Rand
}

// NewProber returns a simulated prober; it never touches the network.
func NewProber(m config.Monitor) (probe.Prober, error) {
	return &Prober{m: m, rng: rand.New(rand.NewPCG(uint64(len(m.ID))*977, uint64(time.Now().UnixNano())))}, nil
}

func (p *Prober) Probe(ctx context.Context) model.Check {
	pr := profiles[p.m.ID]
	lat := latency(p.rng, pr, time.Now())
	c := model.Check{Status: model.StatusUp, Latency: lat}
	if pr.timing {
		c.Timing = timing(p.rng, lat)
	}
	if pr.cert > 0 {
		c.CertExpiry = time.Now().Add(pr.cert).Truncate(24 * time.Hour)
	}
	select {
	case <-ctx.Done():
	case <-time.After(min(lat, 2*time.Second)):
	}
	return c
}
