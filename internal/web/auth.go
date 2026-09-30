package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "pharos_session"
	sessionTTL    = 7 * 24 * time.Hour
)

type session struct {
	user    string
	expires time.Time
	nonce   string
}

// Sessions are stateless signed cookies: base64(user|expiry|nonce).base64(mac).
// The MAC also covers the configured password hash, so changing or removing
// the password signs everyone out. Rotating the database secret does too.
func (s *Server) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	mac.Write([]byte{0})
	mac.Write([]byte(s.cfg().Server.Admin.PasswordHash))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) newSessionCookie(r *http.Request, user string) *http.Cookie {
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	exp := s.now().Add(sessionTTL)
	payload := user + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + base64.RawURLEncoding.EncodeToString(nb)
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.sign(payload)
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Expires: exp, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode}
}

func (s *Server) session(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	enc, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return session{}, false
	}
	payload := string(raw)
	if subtle.ConstantTimeCompare([]byte(s.sign(payload)), []byte(sig)) != 1 {
		return session{}, false
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return session{}, false
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().After(time.Unix(unix, 0)) {
		return session{}, false
	}
	// A changed admin username invalidates old sessions.
	if parts[0] != s.cfg().Server.Admin.Username {
		return session{}, false
	}
	return session{user: parts[0], expires: time.Unix(unix, 0), nonce: parts[2]}, true
}

func (s *Server) csrfToken(sess session) string {
	return s.sign("csrf|" + sess.nonce)
}

// authMode reports how the dashboard is protected.
func (s *Server) passwordSet() bool { return s.cfg().Server.Admin.PasswordHash != "" }

// localRequest reports whether the request comes straight from this
// machine, addressed to this machine. It never relies on forwarded
// headers: behind a local proxy every request would look local. Requiring
// a local Host also defeats DNS rebinding, where a web page makes the
// victim's browser talk to 127.0.0.1 under the attacker's host name.
func (s *Server) localRequest(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "X-Forwarded-Host"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	ip := net.ParseIP(remoteHost(r))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	hip := net.ParseIP(host)
	return hip != nil && hip.IsLoopback()
}

// localSession is the implicit identity used when no password is set and
// the request is local.
func localSession() session { return session{user: "local", nonce: "local"} }

// currentSession returns the signed-in session, or the implicit local
// session when no password is configured and the client is on loopback (or
// anywhere, in demo mode).
func (s *Server) currentSession(r *http.Request) (session, bool) {
	if sess, ok := s.session(r); ok {
		return sess, true
	}
	if !s.passwordSet() && (s.open || s.localRequest(r)) {
		return localSession(), true
	}
	return session{}, false
}

// admin wraps dashboard handlers: it requires a session (or, without a
// configured password, a loopback client) and a CSRF token on writes.
func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
				s.jsonError(w, http.StatusUnauthorized, "sign in required")
				return
			}
			if !s.passwordSet() {
				s.errorPage(w, r, http.StatusForbidden, "error.local_only")
				return
			}
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("_csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.csrfToken(sess))) != 1 || !s.sameOrigin(r) {
				s.jsonError(w, http.StatusForbidden, "invalid or missing CSRF token")
				return
			}
		}
		next(w, r)
	})
}

// sameOrigin rejects cross-site writes when the browser tells us the origin.
// The origin may match the Host header, the proxy's X-Forwarded-Host (with
// trust_proxy; many proxies rewrite Host) or the configured base_url.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients; the CSRF token still applies
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	allowed := []string{r.Host}
	cfg := s.cfg().Server
	if cfg.TrustProxy {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			allowed = append(allowed, strings.TrimSpace(strings.Split(fh, ",")[0]))
		}
	}
	if b, err := url.Parse(cfg.BaseURL); err == nil && b.Host != "" {
		allowed = append(allowed, b.Host)
	}
	for _, h := range allowed {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	if !s.passwordSet() {
		if s.open || s.localRequest(r) {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		s.errorPage(w, r, http.StatusForbidden, "error.local_only")
		return
	}
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, errKey string) {
	p := s.basePage(r, "")
	p.Title = p.T("login.title")
	msg := ""
	if errKey != "" {
		msg = p.T(errKey)
	}
	s.render(w, r, status, "login", struct {
		page
		Error string
		Next  string
		User  string
	}{p, msg, safeNext(r.URL.Query().Get("next")), r.PostFormValue("username")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.passwordSet() {
		s.errorPage(w, r, http.StatusForbidden, "error.local_only")
		return
	}
	ip := s.clientIP(r)
	if !s.limiter.allow(ip, s.now()) {
		s.renderLogin(w, r, http.StatusTooManyRequests, "login.too_many")
		return
	}
	if !s.sameOrigin(r) {
		s.renderLogin(w, r, http.StatusForbidden, "login.failed")
		return
	}
	cfg := s.cfg().Server.Admin
	user := r.PostFormValue("username")
	pass := r.PostFormValue("password")
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.Username)) == 1
	// Always run bcrypt so timing does not reveal whether the user exists.
	passOK := bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(pass)) == nil
	if !userOK || !passOK {
		s.log.Warn("failed sign-in", "ip", ip, "user", user)
		s.renderLogin(w, r, http.StatusUnauthorized, "login.failed")
		return
	}
	s.limiter.reset(ip)
	http.SetCookie(w, s.newSessionCookie(r, cfg.Username))
	s.log.Info("signed in", "ip", ip, "user", cfg.Username)
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeNext only allows local redirect targets under /admin.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return "/admin"
}

// limiter allows n attempts per window per key. Its memory is bounded:
// under a spray of addresses it forgets expired entries, and if that is
// not enough it starts over, which costs an attacker nothing they could
// not already get by changing address.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

const limiterMaxKeys = 10000

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > limiterMaxKeys {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
		if len(l.hits) > limiterMaxKeys/2 {
			l.hits = map[string][]time.Time{key: l.hits[key]}
		}
	}
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}
