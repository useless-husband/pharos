// Package config loads, validates and describes the Pharos configuration file.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Monitor types.
const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"
	TypeDNS  = "dns"
	TypeTLS  = "tls"
	TypePing = "ping"
	TypePush = "push"
)

// Notifier types.
const (
	NotifyWebhook  = "webhook"
	NotifySlack    = "slack"
	NotifyDiscord  = "discord"
	NotifyTelegram = "telegram"
	NotifyEmail    = "email"
	NotifyNtfy     = "ntfy"
)

// Notification event names, usable in a notifier's `events` filter.
const (
	EventDown     = "down"
	EventUp       = "up"
	EventDegraded = "degraded"
	EventReminder = "reminder"
	EventCert     = "cert"
)

// Config is the complete, validated configuration.
type Config struct {
	Server      Server        `yaml:"server"`
	Storage     Storage       `yaml:"storage"`
	StatusPage  StatusPage    `yaml:"status_page"`
	Defaults    Defaults      `yaml:"defaults"`
	Notifiers   []Notifier    `yaml:"notifiers"`
	Monitors    []Monitor     `yaml:"monitors"`
	Maintenance []Maintenance `yaml:"maintenance"`

	// Path is the file the configuration was loaded from.
	Path string `yaml:"-"`
}

type Server struct {
	// Listen is the address the HTTP server binds to. Default ":8080".
	Listen string `yaml:"listen"`
	// BaseURL is the public URL of this instance, used in notification
	// links and the push monitor URLs shown in the dashboard.
	BaseURL string `yaml:"base_url"`
	// TrustProxy makes Pharos honor X-Forwarded-For and X-Forwarded-Proto.
	// Enable it only behind a reverse proxy you control.
	TrustProxy bool    `yaml:"trust_proxy"`
	Admin      Admin   `yaml:"admin"`
	Metrics    Metrics `yaml:"metrics"`
}

type Admin struct {
	Username string `yaml:"username"`
	// PasswordHash is a bcrypt hash; generate one with `pharos hash-password`.
	// When empty, the dashboard is only reachable from loopback addresses.
	PasswordHash string `yaml:"password_hash"`
}

type Metrics struct {
	// Enabled exposes Prometheus metrics at /metrics. Default true.
	Enabled *bool `yaml:"enabled"`
	// Token, when set, requires "Authorization: Bearer <token>".
	Token string `yaml:"token"`
}

type Storage struct {
	// Path of the SQLite database. Default "pharos.db" next to the config file.
	Path string `yaml:"path"`
	// Retention is how long individual check results are kept. Hourly
	// aggregates and status history are kept for 400 days. Default 30d.
	Retention Duration `yaml:"retention"`
}

type StatusPage struct {
	// Enabled serves the public status page at "/". Default true.
	Enabled     *bool  `yaml:"enabled"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	// Language of the status page and dashboard: "en" or "zh-TW".
	Language string `yaml:"language"`
	// Timezone used to draw daily history, e.g. "Asia/Taipei". Default: local.
	Timezone string `yaml:"timezone"`
	// Days of daily history bars to show. Default 90.
	HistoryDays int `yaml:"history_days"`
	// Days of past incidents to list. Default 14.
	IncidentDays int `yaml:"incident_days"`
	// ShowCauses publishes failure messages such as "connection refused"
	// on the public page. Off by default: they can reveal internal names.
	ShowCauses bool    `yaml:"show_causes"`
	Groups     []Group `yaml:"groups"`
	// Links shown in the page header, e.g. a support page.
	Links []Link `yaml:"links"`

	location *time.Location
}

// Location returns the configured timezone.
func (s StatusPage) Location() *time.Location {
	if s.location == nil {
		return time.Local
	}
	return s.location
}

type Group struct {
	Name     string   `yaml:"name"`
	Monitors []string `yaml:"monitors"`
}

type Link struct {
	Label string `yaml:"label"`
	URL   string `yaml:"url"`
}

type Defaults struct {
	Interval      Duration `yaml:"interval"`
	Timeout       Duration `yaml:"timeout"`
	RetryInterval Duration `yaml:"retry_interval"`
	Confirm       Confirm  `yaml:"confirm"`
	// Notify lists notifiers used by monitors that do not set their own.
	Notify []string `yaml:"notify"`
	// RemindEvery re-sends a down notification while an incident stays open.
	// Zero disables reminders.
	RemindEvery Duration `yaml:"remind_every"`
	// CertExpiryWarn raises a "cert" event when a TLS certificate expires
	// within this window. Default 14d.
	CertExpiryWarn Duration `yaml:"cert_expiry_warn"`
}

// Confirm sets how many consecutive checks are needed to change state.
type Confirm struct {
	// Down: consecutive failed checks before a monitor is marked down.
	Down int `yaml:"down"`
	// Up: consecutive successful checks before a down monitor recovers.
	Up int `yaml:"up"`
}

type Notifier struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`

	// webhook, slack, discord, ntfy
	URL string `yaml:"url"`
	// webhook: extra request headers
	Headers map[string]string `yaml:"headers"`
	// webhook: HMAC-SHA256 key used to sign the body (X-Pharos-Signature)
	Secret string `yaml:"secret"`
	// telegram: bot token; ntfy: access token
	Token string `yaml:"token"`
	// telegram
	ChatID string `yaml:"chat_id"`
	// email
	SMTP SMTP     `yaml:"smtp"`
	From string   `yaml:"from"`
	To   []string `yaml:"to"`

	// Events limits which events this notifier receives. Default: all
	// except "degraded".
	Events []string `yaml:"events"`
}

// Wants reports whether the notifier subscribes to an event.
func (n Notifier) Wants(event string) bool {
	if len(n.Events) == 0 {
		return event != EventDegraded
	}
	for _, e := range n.Events {
		if e == event {
			return true
		}
	}
	return false
}

type SMTP struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Security: "starttls" (default for port 587), "tls" (default for 465)
	// or "none".
	Security string `yaml:"security"`
}

type Monitor struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`

	Interval      Duration  `yaml:"interval"`
	Timeout       Duration  `yaml:"timeout"`
	RetryInterval Duration  `yaml:"retry_interval"`
	Confirm       *Confirm  `yaml:"confirm"`
	Notify        *[]string `yaml:"notify"`
	RemindEvery   *Duration `yaml:"remind_every"`

	// http
	URL             string            `yaml:"url"`
	Method          string            `yaml:"method"`
	Headers         map[string]string `yaml:"headers"`
	Body            string            `yaml:"body"`
	FollowRedirects *bool             `yaml:"follow_redirects"`
	// tcp, tls: host:port. ping: host.
	Address string `yaml:"address"`
	// tcp: data to send after connecting
	Send string `yaml:"send"`
	// http, tls: skip certificate verification (expiry is still reported)
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// tls: SNI server name, defaults to the host of Address
	ServerName string `yaml:"server_name"`
	// dns: the name to resolve, the record type (A, AAAA, CNAME, MX, TXT,
	// NS) and an optional resolver "host:port" (default: system resolver)
	Query    string `yaml:"query"`
	Record   string `yaml:"record"`
	Resolver string `yaml:"resolver"`
	// http, tcp, ping: force "4" or "6"
	IPVersion string `yaml:"ip_version"`
	// push
	Token     string   `yaml:"token"`
	Heartbeat Duration `yaml:"heartbeat"`
	Grace     Duration `yaml:"grace"`

	CertExpiryWarn *Duration `yaml:"cert_expiry_warn"`

	Expect Expect `yaml:"expect"`

	// Line is where the monitor is defined in the config file.
	Line int `yaml:"-"`
}

type Expect struct {
	// http: accepted status codes, e.g. [200, "2xx", "200-204"]. Default 2xx and 3xx.
	Status          []string          `yaml:"status"`
	BodyContains    string            `yaml:"body_contains"`
	BodyNotContains string            `yaml:"body_not_contains"`
	BodyRegex       string            `yaml:"body_regex"`
	Headers         map[string]string `yaml:"headers"`
	JSON            []JSONExpect      `yaml:"json"`
	// Checks slower than this are marked degraded instead of up.
	MaxLatency Duration `yaml:"max_latency"`
	// tcp: the server's first bytes must contain this string
	Banner string `yaml:"banner"`
	// dns: every listed value must appear in the answer
	Values []string `yaml:"values"`
	// tls: minimum days until expiry before the check fails
	MinDaysValid int `yaml:"min_days_valid"`
}

// JSONExpect asserts a value in a JSON response body.
type JSONExpect struct {
	// Path uses dot notation with indexes: "data.items[0].state".
	Path   string `yaml:"path"`
	Equals any    `yaml:"equals"`
	Exists *bool  `yaml:"exists"`
}

// Maintenance declares a window during which monitors are not alerted on
// and their downtime is excluded from availability.
type Maintenance struct {
	Name     string   `yaml:"name"`
	Monitors []string `yaml:"monitors"` // empty means every monitor

	// One-off window.
	Start string `yaml:"start"`
	End   string `yaml:"end"`

	// Recurring window: days of the week ("mon".."sun" or "daily"),
	// a start time ("02:00") and a duration, in Timezone.
	Days     []string `yaml:"days"`
	At       string   `yaml:"at"`
	Duration Duration `yaml:"duration"`
	Timezone string   `yaml:"timezone"`

	window window
}

// Fingerprint identifies the probe-relevant part of a monitor so a reload
// can tell whether its runner needs a restart.
func (m Monitor) Fingerprint() string {
	c := m
	c.Line = 0
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// MonitorByID returns the monitor with the given id.
func (c *Config) MonitorByID(id string) (Monitor, bool) {
	for _, m := range c.Monitors {
		if m.ID == id {
			return m, true
		}
	}
	return Monitor{}, false
}

// NotifierByName returns the notifier with the given name.
func (c *Config) NotifierByName(name string) (Notifier, bool) {
	for _, n := range c.Notifiers {
		if n.Name == name {
			return n, true
		}
	}
	return Notifier{}, false
}

// NotifiersFor returns the notifier names a monitor sends to.
func (c *Config) NotifiersFor(m Monitor) []string {
	if m.Notify != nil {
		return *m.Notify
	}
	return c.Defaults.Notify
}

// ConfirmFor returns the effective confirmation thresholds of a monitor.
func (c *Config) ConfirmFor(m Monitor) Confirm {
	if m.Confirm != nil {
		return *m.Confirm
	}
	return c.Defaults.Confirm
}

// RemindEveryFor returns the effective reminder interval of a monitor.
func (c *Config) RemindEveryFor(m Monitor) time.Duration {
	if m.RemindEvery != nil {
		return m.RemindEvery.D()
	}
	return c.Defaults.RemindEvery.D()
}

// CertExpiryWarnFor returns the effective certificate warning window.
func (c *Config) CertExpiryWarnFor(m Monitor) time.Duration {
	if m.CertExpiryWarn != nil {
		return m.CertExpiryWarn.D()
	}
	return c.Defaults.CertExpiryWarn.D()
}

// MetricsEnabled reports whether /metrics is served.
func (c *Config) MetricsEnabled() bool {
	return c.Server.Metrics.Enabled == nil || *c.Server.Metrics.Enabled
}

// StatusPageEnabled reports whether the public status page is served.
func (c *Config) StatusPageEnabled() bool {
	return c.StatusPage.Enabled == nil || *c.StatusPage.Enabled
}

// Target is a short human description of what a monitor checks.
func (m Monitor) Target() string {
	switch m.Type {
	case TypeHTTP:
		return m.URL
	case TypeTCP, TypeTLS, TypePing:
		return m.Address
	case TypeDNS:
		return m.Record + " " + m.Query
	case TypePush:
		return "heartbeat every " + m.Heartbeat.String()
	}
	return ""
}
