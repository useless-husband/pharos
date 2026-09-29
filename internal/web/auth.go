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
// Rotating the database secret signs everyone out.
func (s *Server) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
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

// localRequest reports whether the request comes straight from this machine.
// A request that went through a proxy is never local, even if the proxy is.
func (s *Server) localRequest(r *http.Request) bool {
	if !s.cfg().Server.TrustProxy && (r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-Ip") != "") {
		return false
	}
	ip := net.ParseIP(s.clientIP(r))
	return ip != nil && ip.IsLoopback()
}

// localSession is the implicit identity used when no password is set and
// the request is local.
func localSession() session { return session{user: "local", nonce: "local"} }

// currentSession returns the signed-in session, or the implicit local
// session when no password is configured and the client is on loopback.
func (s *Server) currentSession(r *http.Request) (session, bool) {
	if sess, ok := s.session(r); ok {
		return sess, true
	}
	if !s.passwordSet() && s.localRequest(r) {
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
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients; the CSRF token still applies
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	if !s.passwordSet() {
		if s.localRequest(r) {
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

// limiter allows n failed attempts per window per key.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

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
	if len(l.hits) > 10000 { // bound memory under a spray of addresses
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}
