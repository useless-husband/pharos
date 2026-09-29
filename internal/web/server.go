// Package web serves the public status page, the admin dashboard, the JSON
// API, badges and Prometheus metrics.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/notify"
	"github.com/useless-husband/pharos/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// Options configure a Server.
type Options struct {
	Engine  *engine.Engine
	Store   *store.Store
	Notify  *notify.Dispatcher
	Logger  *slog.Logger
	Version string
	// Reload re-reads the configuration file; wired to the dashboard's
	// reload button. May be nil.
	Reload func() error
	// Now overrides the clock (demo mode, tests).
	Now func() time.Time
}

// Server is the HTTP front end.
type Server struct {
	engine  *engine.Engine
	store   *store.Store
	notify  *notify.Dispatcher
	log     *slog.Logger
	version string
	reload  func() error
	now     func() time.Time

	pages       map[string]*template.Template
	assetHashes map[string]string
	secret      []byte
	limiter     *limiter
	started     time.Time
}

// New builds a server and parses its templates.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	secret, err := opts.Store.Secret(ctx, "session")
	if err != nil {
		return nil, fmt.Errorf("load session secret: %w", err)
	}
	s := &Server{
		engine: opts.Engine, store: opts.Store, notify: opts.Notify, log: opts.Logger,
		version: opts.Version, reload: opts.Reload, now: opts.Now,
		secret: secret, limiter: newLimiter(10, 10*time.Minute), started: opts.Now(),
	}
	if err := s.hashAssets(); err != nil {
		return nil, err
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) cfg() *config.Config { return s.engine.Config() }

func (s *Server) hashAssets() error {
	s.assetHashes = map[string]string{}
	return fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s.assetHashes[strings.TrimPrefix(p, "assets/")] = hex.EncodeToString(sum[:])[:10]
		return nil
	})
}

func (s *Server) assetURL(name string) string {
	return "/assets/" + name + "?v=" + s.assetHashes[name]
}

var pageNames = []string{"status", "history", "login", "error", "admin_overview", "admin_monitor", "admin_incidents", "admin_notifications", "admin_settings"}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"asset": s.assetURL,
		"json": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			return template.JS(b), err
		},
		"lower": strings.ToLower,
		"add":   func(a, b int) int { return a + b },
		"msOf":  func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 },
	}
	s.pages = map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("parse template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// render executes a page into a buffer first, so a template error becomes
// a clean 500 instead of half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].Execute(&buf, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) jsonError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// Handler returns the root handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("GET /{$}", s.handleStatusPage)
	mux.HandleFunc("GET /history/{id}", s.handleHistory)
	mux.HandleFunc("GET /badge/{id}/{kind}", s.handleBadge)
	mux.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
	mux.HandleFunc("GET /api/v1/incidents", s.handleAPIIncidents)
	mux.HandleFunc("/api/v1/push/{token}", s.handlePush)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /assets/{file...}", s.handleAsset)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.assetURL("favicon.svg"), http.StatusMovedPermanently)
	})

	// Admin
	mux.HandleFunc("GET /admin/login", s.handleLoginPage)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.Handle("POST /admin/logout", s.admin(s.handleLogout))
	mux.Handle("GET /admin", s.admin(s.handleOverview))
	mux.Handle("GET /admin/{$}", s.admin(s.handleOverview))
	mux.Handle("GET /admin/monitors/{id}", s.admin(s.handleMonitor))
	mux.Handle("POST /admin/monitors/{id}/{action}", s.admin(s.handleMonitorAction))
	mux.Handle("GET /admin/incidents", s.admin(s.handleIncidents))
	mux.Handle("GET /admin/notifications", s.admin(s.handleNotifications))
	mux.Handle("POST /admin/notifiers/{name}/test", s.admin(s.handleNotifierTest))
	mux.Handle("GET /admin/settings", s.admin(s.handleSettings))
	mux.Handle("POST /admin/reload", s.admin(s.handleReload))
	mux.Handle("GET /admin/events", s.admin(s.handleEvents))
	mux.Handle("GET /api/v1/admin/monitors", s.admin(s.handleAPIMonitors))
	mux.Handle("GET /api/v1/admin/monitors/{id}", s.admin(s.handleAPIMonitor))
	mux.Handle("GET /api/v1/admin/monitors/{id}/checks", s.admin(s.handleAPIChecks))
	mux.Handle("GET /api/v1/admin/monitors/{id}/latency", s.admin(s.handleAPILatency))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
	})
	return s.middleware(mux)
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if s.secure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				http.Error(rec, "internal error", http.StatusInternalServerError)
			}
			level := slog.LevelInfo
			if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/healthz" || r.URL.Path == "/admin/events" {
				level = slog.LevelDebug
			}
			path := r.URL.Path
			if strings.HasPrefix(path, "/api/v1/push/") {
				path = "/api/v1/push/…" // the token is a secret
			}
			s.log.Log(r.Context(), level, "request", "method", r.Method, "path", path, "status", rec.code,
				"duration_ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
		}()
		next.ServeHTTP(rec, r)
	})
}

// clientIP is the remote address. With trust_proxy it is the right-most
// X-Forwarded-For entry: the one our own proxy appended. Entries further
// left were supplied by the client and cannot be trusted.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg().Server.TrustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			hops := strings.Split(xff[len(xff)-1], ",")
			if ip := net.ParseIP(strings.TrimSpace(hops[len(hops)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return remoteHost(r)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// secure reports whether the client connection is HTTPS.
func (s *Server) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.cfg().Server.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(r.PathValue("file"))
	b, err := assetFS.ReadFile("assets/" + name)
	if err != nil || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("v") == s.assetHashes[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.Header().Set("ETag", `"`+s.assetHashes[name]+`"`)
	if r.Header.Get("If-None-Match") == `"`+s.assetHashes[name]+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version, "uptime_seconds": int(s.now().Sub(s.started).Seconds())})
}

// page is embedded in every template's data.
type page struct {
	Lang      string
	Title     string
	SiteTitle string
	Version   string
	CSRF      string
	Nav       string // active admin tab
	Admin     bool   // signed in
	Loc       *time.Location
	TZ        string
	Now       time.Time
	Links     []config.Link
	// Static is set when rendering files for `pharos export`: links become
	// relative so the pages work from any directory or sub-path.
	Static   bool
	HomeHref string
	root     string // path prefix back to the export root ("" or "../")
	assets   map[string]string
}

// Asset returns the cache-busted URL of a static file.
func (p page) Asset(name string) string {
	prefix := "/assets/"
	if p.Static {
		prefix = p.root + "assets/"
	}
	return prefix + name + "?v=" + p.assets[name]
}

// T translates a key.
func (p page) T(key string, args ...any) string { return i18n.T(p.Lang, key, args...) }

// Time formats an absolute time in the page's time zone.
func (p page) Time(t time.Time) string { return i18n.FormatTime(t, p.Loc) }

// HistoryHref links to a monitor's public history page.
func (p page) HistoryHref(id string) string {
	if p.Static {
		return p.root + "history/" + url.PathEscape(id) + ".html"
	}
	return "/history/" + url.PathEscape(id)
}

func (s *Server) basePage(r *http.Request, title string) page {
	cfg := s.cfg()
	loc := cfg.StatusPage.Location()
	p := page{
		Lang: cfg.StatusPage.Language, Title: title, SiteTitle: cfg.StatusPage.Title, Version: s.version,
		Loc: loc, TZ: tzName(loc, s.now()), Now: s.now(), Links: cfg.StatusPage.Links, HomeHref: "/",
		assets: s.assetHashes,
	}
	if sess, ok := s.currentSession(r); ok {
		p.Admin = true
		p.CSRF = s.csrfToken(sess)
	}
	return p
}

// tzName describes a zone for people: "Asia/Taipei (UTC+08:00)".
func tzName(loc *time.Location, now time.Time) string {
	name := loc.String()
	if name == "Local" {
		name, _ = now.In(loc).Zone()
	}
	return name + " (UTC" + now.In(loc).Format("-07:00") + ")"
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, key string) {
	p := s.basePage(r, "")
	p.Title = p.T(key)
	s.render(w, r, status, "error", struct {
		page
		Status  int
		Message string
	}{p, status, p.T(key + ".detail")})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	// Requests derive from base, which is cancelled when shutdown starts,
	// so long-lived event streams end instead of holding shutdown open.
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		BaseContext:       func(net.Listener) context.Context { return base },
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the dashboard's event stream is long-lived.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Info("http server listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	cancelBase()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
