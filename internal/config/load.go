package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/useless-husband/pharos/internal/jsonpath"
)

// Problem is one validation failure, located in the source file when possible.
type Problem struct {
	Line int
	Msg  string
}

// Error collects every problem found in a configuration file, so a user can
// fix them all in one pass instead of one per run.
type Error struct {
	Path     string
	Problems []Problem
}

func (e *Error) Error() string {
	var b strings.Builder
	name := e.Path
	if name == "" {
		name = "config"
	}
	fmt.Fprintf(&b, "%s: %d problem(s)", name, len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		if p.Line > 0 {
			fmt.Fprintf(&b, "line %d: ", p.Line)
		}
		b.WriteString(p.Msg)
	}
	return b.String()
}

// Load reads, parses and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return Parse(data, abs, os.LookupEnv)
}

// Parse parses and validates configuration from memory. lookup resolves
// ${VAR} references; path is used to resolve relative storage paths and in
// error messages.
func Parse(data []byte, path string, lookup func(string) (string, bool)) (*Config, error) {
	cerr := &Error{Path: path}

	// Pass 1: unknown keys. Decode the raw text strictly and keep only the
	// "field not found" errors; type errors are reported after env
	// expansion, where values like ${PORT} have been substituted.
	var probe Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&probe); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, msg := range te.Errors {
				if strings.Contains(msg, "not found in type") {
					cerr.Problems = append(cerr.Problems, unknownField(msg))
				}
			}
		} else {
			cerr.Problems = append(cerr.Problems, Problem{Msg: err.Error()})
			return nil, cerr
		}
	}

	// Pass 2: expand ${VAR} in scalar values and decode for real.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		cerr.Problems = append(cerr.Problems, Problem{Msg: err.Error()})
		return nil, cerr
	}
	if len(root.Content) == 0 {
		cerr.Problems = append(cerr.Problems, Problem{Msg: "the file is empty"})
		return nil, cerr
	}
	expandEnv(&root, lookup, cerr)

	cfg := &Config{Path: path}
	if err := root.Decode(cfg); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, msg := range te.Errors {
				if !strings.Contains(msg, "not found in type") {
					cerr.Problems = append(cerr.Problems, lineProblem(msg))
				}
			}
		} else {
			cerr.Problems = append(cerr.Problems, lineProblem(err.Error()))
		}
	}
	if len(cerr.Problems) > 0 {
		sortProblems(cerr)
		return nil, cerr
	}

	recordLines(root.Content[0], cfg)
	if cfg.Storage.Path == "" {
		// Containers point the database at a volume without editing the file.
		if v, ok := lookup("PHAROS_STORAGE_PATH"); ok && v != "" {
			cfg.Storage.Path = v
		}
	}
	applyDefaults(cfg, path)
	validate(cfg, root.Content[0], cerr)
	if len(cerr.Problems) > 0 {
		sortProblems(cerr)
		return nil, cerr
	}
	cfg.index = make(map[string]int, len(cfg.Monitors))
	for i, m := range cfg.Monitors {
		cfg.index[m.ID] = i
	}
	return cfg, nil
}

var lineRE = regexp.MustCompile(`^line (\d+): (.*)$`)

func lineProblem(msg string) Problem {
	msg = strings.TrimPrefix(msg, "yaml: ")
	if m := lineRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		// Nested duration errors carry their own "line N:" prefix.
		inner := m[2]
		if m2 := lineRE.FindStringSubmatch(inner); m2 != nil {
			inner = m2[2]
		}
		return Problem{Line: n, Msg: inner}
	}
	return Problem{Msg: msg}
}

var unknownRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type config\.(\w+)$`)

func unknownField(msg string) Problem {
	if m := unknownRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Problem{Line: n, Msg: fmt.Sprintf("unknown setting %q in %s", m[2], sectionName(m[3]))}
	}
	return lineProblem(msg)
}

func sectionName(typ string) string {
	switch typ {
	case "Config":
		return "the top level"
	case "StatusPage":
		return "status_page"
	case "SMTP":
		return "smtp"
	case "JSONExpect":
		return "expect.json"
	}
	return strings.ToLower(typ)
}

func sortProblems(e *Error) {
	sort.SliceStable(e.Problems, func(i, j int) bool { return e.Problems[i].Line < e.Problems[j].Line })
}

var envRE = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} in every scalar value.
// "$$" produces a literal "$".
func expandEnv(n *yaml.Node, lookup func(string) (string, bool), cerr *Error) {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "$") {
		n.Value = envRE.ReplaceAllStringFunc(n.Value, func(tok string) string {
			if tok == "$$" {
				return "$"
			}
			m := envRE.FindStringSubmatch(tok)
			if v, ok := lookup(m[1]); ok && v != "" {
				return v
			}
			if m[2] != "" {
				return m[3]
			}
			cerr.Problems = append(cerr.Problems, Problem{Line: n.Line,
				Msg: fmt.Sprintf("environment variable %s is not set (use ${%s:-default} to provide a fallback)", m[1], m[1])})
			return ""
		})
		// A substituted value may now be a number or boolean; let the
		// decoder re-resolve the tag.
		if n.Style == 0 {
			n.Tag = ""
		}
	}
	for _, c := range n.Content {
		expandEnv(c, lookup, cerr)
	}
}

// mapValue returns the value node for key in a mapping node.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// keyLine returns the line of key inside a mapping node, or the node's own line.
func keyLine(n *yaml.Node, key string) int {
	if n == nil {
		return 0
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i].Line
			}
		}
	}
	return n.Line
}

func seqItem(n *yaml.Node, i int) *yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode || i >= len(n.Content) {
		return nil
	}
	return n.Content[i]
}

func recordLines(doc *yaml.Node, cfg *Config) {
	mons := mapValue(doc, "monitors")
	for i := range cfg.Monitors {
		if it := seqItem(mons, i); it != nil {
			cfg.Monitors[i].Line = it.Line
		}
	}
}

func applyDefaults(c *Config, path string) {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.Admin.Username == "" {
		c.Server.Admin.Username = "admin"
	}
	c.Server.BaseURL = strings.TrimRight(c.Server.BaseURL, "/")

	if c.Storage.Path == "" {
		c.Storage.Path = "pharos.db"
	}
	if c.Storage.Path != ":memory:" && !filepath.IsAbs(c.Storage.Path) && path != "" {
		c.Storage.Path = filepath.Join(filepath.Dir(path), c.Storage.Path)
	}
	if c.Storage.Retention == 0 {
		c.Storage.Retention = Duration(30 * 24 * time.Hour)
	}

	sp := &c.StatusPage
	if sp.Title == "" {
		sp.Title = "Service Status"
	}
	if sp.Language == "" {
		sp.Language = "en"
	}
	if sp.HistoryDays == 0 {
		sp.HistoryDays = 90
	}
	if sp.IncidentDays == 0 {
		sp.IncidentDays = 14
	}

	d := &c.Defaults
	if d.Interval == 0 {
		d.Interval = Duration(time.Minute)
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(10 * time.Second)
	}
	if d.RetryInterval == 0 {
		d.RetryInterval = Duration(15 * time.Second)
	}
	if d.Confirm.Down == 0 {
		d.Confirm.Down = 3
	}
	if d.Confirm.Up == 0 {
		d.Confirm.Up = 2
	}
	if d.CertExpiryWarn == 0 {
		d.CertExpiryWarn = Duration(14 * 24 * time.Hour)
	}

	for i := range c.Monitors {
		m := &c.Monitors[i]
		m.Type = strings.ToLower(strings.TrimSpace(m.Type))
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.Interval == 0 {
			m.Interval = d.Interval
		}
		if m.Timeout == 0 {
			m.Timeout = min(d.Timeout, m.Interval)
		}
		if m.RetryInterval == 0 {
			m.RetryInterval = min(d.RetryInterval, m.Interval)
		}
		if m.Confirm != nil {
			if m.Confirm.Down == 0 {
				m.Confirm.Down = d.Confirm.Down
			}
			if m.Confirm.Up == 0 {
				m.Confirm.Up = d.Confirm.Up
			}
		}
		switch m.Type {
		case TypeHTTP:
			if m.Method == "" {
				m.Method = "GET"
			}
			m.Method = strings.ToUpper(m.Method)
		case TypeDNS:
			if m.Record == "" {
				m.Record = "A"
			}
			m.Record = strings.ToUpper(m.Record)
			if m.Resolver != "" {
				if _, _, err := net.SplitHostPort(m.Resolver); err != nil {
					m.Resolver = net.JoinHostPort(m.Resolver, "53")
				}
			}
		case TypeTLS:
			if m.Address != "" {
				if _, _, err := net.SplitHostPort(m.Address); err != nil {
					m.Address = net.JoinHostPort(m.Address, "443")
				}
			}
		case TypePush:
			if m.Grace == 0 {
				g := m.Heartbeat.D() / 10
				g = max(g, 30*time.Second)
				g = min(g, time.Hour)
				m.Grace = Duration(g)
			}
			// A push monitor evaluates its deadline on this cadence.
			m.Interval = Duration(min(max(m.Heartbeat.D()/20, time.Second), 30*time.Second))
		}
	}

	for i := range c.Notifiers {
		n := &c.Notifiers[i]
		n.Type = strings.ToLower(strings.TrimSpace(n.Type))
		if n.Type == NotifyEmail {
			s := &n.SMTP
			s.Security = strings.ToLower(s.Security)
			if s.Port == 0 {
				switch s.Security {
				case "tls":
					s.Port = 465
				case "none":
					s.Port = 25
				default:
					s.Port = 587
				}
			}
			if s.Security == "" {
				if s.Port == 465 {
					s.Security = "tls"
				} else {
					s.Security = "starttls"
				}
			}
		}
	}
}

var (
	idRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	statusRE   = regexp.MustCompile(`^([1-5][0-9][0-9]|[1-5]xx|[1-5][0-9][0-9]-[1-5][0-9][0-9])$`)
	bcryptRE   = regexp.MustCompile(`^\$2[aby]\$\d\d\$[./A-Za-z0-9]{53}$`)
	validTypes = map[string]bool{TypeHTTP: true, TypeTCP: true, TypeDNS: true, TypeTLS: true, TypePing: true, TypePush: true}
	dnsRecords = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "MX": true, "TXT": true, "NS": true}
	eventNames = map[string]bool{EventDown: true, EventUp: true, EventDegraded: true, EventReminder: true, EventCert: true}
	methods    = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}
)

func validate(c *Config, doc *yaml.Node, e *Error) {
	add := func(line int, format string, args ...any) {
		e.Problems = append(e.Problems, Problem{Line: line, Msg: fmt.Sprintf(format, args...)})
	}

	server := mapValue(doc, "server")
	if c.Server.BaseURL != "" {
		if u, err := url.Parse(c.Server.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add(keyLine(server, "base_url"), "server.base_url must be an absolute http(s) URL")
		}
	}
	if h := c.Server.Admin.PasswordHash; h != "" && !bcryptRE.MatchString(h) {
		add(keyLine(mapValue(server, "admin"), "password_hash"),
			"server.admin.password_hash must be a bcrypt hash; create one with `pharos hash-password`")
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		add(keyLine(server, "listen"), "server.listen must be host:port, e.g. \":8080\" or \"127.0.0.1:8080\"")
	}

	sp := mapValue(doc, "status_page")
	if c.StatusPage.Language != "en" && c.StatusPage.Language != "zh-TW" {
		add(keyLine(sp, "language"), "status_page.language must be \"en\" or \"zh-TW\"")
	}
	if tz := c.StatusPage.Timezone; tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			add(keyLine(sp, "timezone"), "unknown timezone %q", tz)
		} else {
			c.StatusPage.location = loc
		}
	}
	if d := c.StatusPage.HistoryDays; d < 7 || d > 365 {
		add(keyLine(sp, "history_days"), "status_page.history_days must be between 7 and 365")
	}
	if c.Storage.Retention.D() < 24*time.Hour {
		add(keyLine(mapValue(doc, "storage"), "retention"), "storage.retention must be at least 1d")
	}

	defs := mapValue(doc, "defaults")
	validateConfirm(c.Defaults.Confirm, keyLine(defs, "confirm"), "defaults.confirm", add)

	// Notifiers.
	notifNodes := mapValue(doc, "notifiers")
	notifNames := map[string]bool{}
	for i, n := range c.Notifiers {
		node := seqItem(notifNodes, i)
		line := keyLine(node, "name")
		where := fmt.Sprintf("notifier %q", n.Name)
		switch {
		case n.Name == "":
			add(line, "notifiers[%d]: name is required", i)
			where = fmt.Sprintf("notifiers[%d]", i)
		case notifNames[n.Name]:
			add(line, "notifier name %q is used twice", n.Name)
		}
		notifNames[n.Name] = true
		tl := keyLine(node, "type")
		switch n.Type {
		case NotifyWebhook, NotifySlack, NotifyDiscord, NotifyNtfy:
			if !isHTTPURL(n.URL) {
				add(keyLine(node, "url"), "%s: url must be an http(s) URL", where)
			}
		case NotifyTelegram:
			if n.Token == "" || n.ChatID == "" {
				add(tl, "%s: telegram needs token and chat_id", where)
			}
		case NotifyEmail:
			if n.SMTP.Host == "" {
				add(tl, "%s: smtp.host is required", where)
			}
			if n.From == "" || len(n.To) == 0 {
				add(tl, "%s: email needs from and at least one to address", where)
			}
			if s := n.SMTP.Security; s != "starttls" && s != "tls" && s != "none" {
				add(keyLine(mapValue(node, "smtp"), "security"), "%s: smtp.security must be starttls, tls or none", where)
			}
		case "":
			add(line, "%s: type is required", where)
		default:
			add(tl, "%s: unknown type %q (use webhook, slack, discord, telegram, email or ntfy)", where, n.Type)
		}
		if g := n.GroupInterval; g != nil {
			gl := keyLine(node, "group_interval")
			switch {
			case n.Type == NotifyWebhook && g.D() != 0:
				add(gl, "%s: group_interval is not supported for webhooks, whose payload describes one event", where)
			case g.D() < 0 || g.D() > time.Hour:
				add(gl, "%s: group_interval must be between 0 and 1h", where)
			}
		}
		for _, ev := range n.Events {
			if !eventNames[ev] {
				add(keyLine(node, "events"), "%s: unknown event %q (use down, up, degraded, reminder, cert)", where, ev)
			}
		}
	}
	for _, name := range c.Defaults.Notify {
		if !notifNames[name] {
			add(keyLine(defs, "notify"), "defaults.notify refers to unknown notifier %q", name)
		}
	}

	// Monitors.
	monNodes := mapValue(doc, "monitors")
	ids := map[string]bool{}
	tokens := map[string]string{}
	if len(c.Monitors) == 0 {
		add(0, "no monitors defined; add at least one under `monitors:`")
	}
	for i := range c.Monitors {
		m := &c.Monitors[i]
		node := seqItem(monNodes, i)
		where := fmt.Sprintf("monitor %q", m.ID)
		switch {
		case m.ID == "":
			add(m.Line, "monitors[%d]: id is required", i)
			where = fmt.Sprintf("monitors[%d]", i)
		case !idRE.MatchString(m.ID):
			add(keyLine(node, "id"), "%s: id may only contain lowercase letters, digits, '.', '_' and '-'", where)
		case ids[m.ID]:
			add(keyLine(node, "id"), "monitor id %q is used twice", m.ID)
		}
		ids[m.ID] = true
		if !validTypes[m.Type] {
			if m.Type == "" {
				add(m.Line, "%s: type is required (http, tcp, dns, tls, ping or push)", where)
			} else {
				add(keyLine(node, "type"), "%s: unknown type %q (use http, tcp, dns, tls, ping or push)", where, m.Type)
			}
			continue
		}
		if m.Type != TypePush {
			if m.Interval.D() < time.Second {
				add(keyLine(node, "interval"), "%s: interval must be at least 1s", where)
			}
			if m.Timeout.D() <= 0 || m.Timeout > m.Interval {
				add(keyLine(node, "timeout"), "%s: timeout must be positive and not longer than the interval (%s)", where, m.Interval)
			}
		}
		if m.Confirm != nil {
			validateConfirm(*m.Confirm, keyLine(node, "confirm"), where+": confirm", add)
		}
		if m.Notify != nil {
			for _, name := range *m.Notify {
				if !notifNames[name] {
					add(keyLine(node, "notify"), "%s: unknown notifier %q", where, name)
				}
			}
		}
		exp := mapValue(node, "expect")
		if m.Expect.MaxLatency < 0 || (m.Expect.MaxLatency > 0 && m.Expect.MaxLatency >= m.Timeout && m.Type != TypePush) {
			add(keyLine(exp, "max_latency"), "%s: expect.max_latency must be shorter than the timeout (%s)", where, m.Timeout)
		}
		switch m.Type {
		case TypeHTTP:
			if !isHTTPURL(m.URL) {
				add(keyLine(node, "url"), "%s: url must be an absolute http(s) URL", where)
			}
			if !methods[m.Method] {
				add(keyLine(node, "method"), "%s: unsupported method %q", where, m.Method)
			}
			for _, s := range m.Expect.Status {
				if !statusRE.MatchString(strings.ToLower(s)) {
					add(keyLine(exp, "status"), "%s: invalid status matcher %q (use 200, 2xx or 200-299)", where, s)
				}
			}
			if m.Expect.BodyRegex != "" {
				if _, err := regexp.Compile(m.Expect.BodyRegex); err != nil {
					add(keyLine(exp, "body_regex"), "%s: body_regex: %v", where, err)
				}
			}
			for _, j := range m.Expect.JSON {
				if _, err := jsonpath.Parse(j.Path); err != nil {
					add(keyLine(exp, "json"), "%s: %v", where, err)
				}
				if j.Equals == nil && j.Exists == nil {
					add(keyLine(exp, "json"), "%s: json assertion on %q needs equals or exists", where, j.Path)
				}
			}
		case TypeTCP, TypeTLS:
			if _, port, err := net.SplitHostPort(m.Address); err != nil || port == "" {
				add(keyLine(node, "address"), "%s: address must be host:port", where)
			}
		case TypePing:
			if m.Address == "" || strings.Contains(m.Address, "/") {
				add(keyLine(node, "address"), "%s: address must be a host name or IP address", where)
			}
		case TypeDNS:
			if m.Query == "" {
				add(m.Line, "%s: query (the name to resolve) is required", where)
			}
			if !dnsRecords[m.Record] {
				add(keyLine(node, "record"), "%s: record must be one of A, AAAA, CNAME, MX, TXT, NS", where)
			}
		case TypePush:
			if m.Heartbeat.D() < 10*time.Second {
				add(keyLine(node, "heartbeat"), "%s: push monitors need heartbeat of at least 10s", where)
			}
			if m.Token != "" {
				if len(m.Token) < 16 {
					add(keyLine(node, "token"), "%s: token must be at least 16 characters", where)
				}
				if other, dup := tokens[m.Token]; dup {
					add(keyLine(node, "token"), "%s: token is already used by %q", where, other)
				}
				tokens[m.Token] = m.ID
			}
		}
		if v := m.IPVersion; v != "" && v != "4" && v != "6" {
			add(keyLine(node, "ip_version"), "%s: ip_version must be \"4\" or \"6\"", where)
		}
	}

	// Groups.
	grpNodes := mapValue(sp, "groups")
	for i, g := range c.StatusPage.Groups {
		node := seqItem(grpNodes, i)
		if g.Name == "" {
			add(keyLine(node, "name"), "status_page.groups[%d]: name is required", i)
		}
		for _, id := range g.Monitors {
			if !ids[id] {
				add(keyLine(node, "monitors"), "status page group %q refers to unknown monitor %q", g.Name, id)
			}
		}
	}

	// Maintenance.
	mntNodes := mapValue(doc, "maintenance")
	for i := range c.Maintenance {
		mw := &c.Maintenance[i]
		node := seqItem(mntNodes, i)
		name := mw.Name
		if name == "" {
			name = fmt.Sprintf("maintenance[%d]", i)
		}
		if err := mw.compile(c.StatusPage.Location()); err != nil {
			add(nodeLine(node), "%s: %v", name, err)
		}
		for _, id := range mw.Monitors {
			if !ids[id] {
				add(keyLine(node, "monitors"), "%s: unknown monitor %q", name, id)
			}
		}
	}
}

func nodeLine(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

func validateConfirm(c Confirm, line int, where string, add func(int, string, ...any)) {
	if c.Down < 1 || c.Down > 20 || c.Up < 1 || c.Up > 20 {
		add(line, "%s: down and up must be between 1 and 20", where)
	}
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
