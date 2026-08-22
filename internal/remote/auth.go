package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"iar/internal/accounts"
)

// sessionCookie carries the login session id.
const sessionCookie = "iar_session"

// userHandler is a handler that runs on behalf of a logged-in account.
type userHandler func(w http.ResponseWriter, r *http.Request, u accounts.User)

// Permission predicates.
var (
	permSteer     = func(p accounts.Permissions) bool { return p.Steer }
	permNewPrompt = func(p accounts.Permissions) bool { return p.NewPrompt }
	permSave      = func(p accounts.Permissions) bool { return p.Save }
	permAdmin     = func(p accounts.Permissions) bool { return p.Admin }
)

// currentUser resolves the session cookie to an account. The session id
// is only ever looked up by its hash, so the comparison leaks nothing.
func (s *Server) currentUser(r *http.Request) (accounts.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return accounts.User{}, false
	}
	sess, ok := s.sessions.Lookup(c.Value)
	if !ok {
		return accounts.User{}, false
	}
	u, ok := s.users.Get(sess.Email)
	if !ok || !u.Active {
		return accounts.User{}, false
	}
	return u, true
}

// page wraps a browser-facing handler: without a login it redirects to
// the login page (remembering where to return).
func (s *Server) page(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			next := ""
			if r.Method == http.MethodGet && r.URL.Path != "/" {
				next = "?next=" + r.URL.RequestURI()
			}
			http.Redirect(w, r, "/login"+next, http.StatusFound)
			return
		}
		h(w, r, u)
	}
}

// api wraps a data handler: without a login it answers 401.
func (s *Server) api(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		h(w, r, u)
	}
}

// apiPerm is api plus a permission check (403 and a log line on denial).
func (s *Server) apiPerm(action string, allowed func(accounts.Permissions) bool, h userHandler) http.HandlerFunc {
	return s.api(func(w http.ResponseWriter, r *http.Request, u accounts.User) {
		if !allowed(u.Perms) {
			s.denied(u, action, r)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "your account may not " + action})
			return
		}
		h(w, r, u)
	})
}

// pagePerm is page plus a permission check.
func (s *Server) pagePerm(action string, allowed func(accounts.Permissions) bool, h userHandler) http.HandlerFunc {
	return s.page(func(w http.ResponseWriter, r *http.Request, u accounts.User) {
		if !allowed(u.Perms) {
			s.denied(u, action, r)
			s.render(w, http.StatusForbidden, "message.html", map[string]any{
				"Nav":     s.nav(u, ""),
				"Title":   "Not allowed",
				"Message": "Your account does not have permission for this (" + action + "). Ask an admin.",
			})
			return
		}
		h(w, r, u)
	})
}

// denied logs a permission denial.
func (s *Server) denied(u accounts.User, action string, r *http.Request) {
	s.log.Warn("remote permission denied", "event", "remote_denied", "user", u.Email, "action", action, "path", r.URL.Path, "from", r.RemoteAddr)
}

// clientIP extracts the peer address for rate limiting.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isHTTPS reports whether the request arrived over TLS (directly or via
// a proxy that says so), which decides the cookie's Secure flag.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie logs the browser in.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
		MaxAge: int(accounts.DefaultSessionTTL / time.Second),
	})
}

// clearSessionCookie logs the browser out.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// safeNext accepts only same-site relative paths as post-login targets.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

// loginData feeds the login page.
type loginData struct {
	Product string
	Error   string
	Next    string
	Email   string
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "login.html", loginData{Product: ProductName, Next: safeNext(r.URL.Query().Get("next"))})
}

// loginFailure is the single message every failed login gets, so the
// page never reveals whether an email exists.
const loginFailure = "Wrong email or password."

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := textField(r, "email")
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	ip := clientIP(r)
	acct, _ := accounts.NormalizeEmail(email)
	if acct == "" {
		acct = strings.ToLower(strings.TrimSpace(email))
	}
	fail := func(status int, msg string) {
		s.render(w, status, "login.html", loginData{Product: ProductName, Error: msg, Next: next, Email: email})
	}
	if ok, wait := s.ipLimit.Allowed(ip); !ok {
		s.log.Warn("remote login throttled", "event", "remote_login_throttled", "from", ip, "email", acct)
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %s.", fmtWait(wait)))
		return
	}
	if ok, wait := s.acctLimit.Allowed(acct); !ok {
		s.log.Warn("remote login throttled", "event", "remote_login_throttled", "from", ip, "email", acct)
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many attempts for this account. Try again in %s.", fmtWait(wait)))
		return
	}
	u, ok := s.users.Verify(email, password)
	if !ok {
		s.ipLimit.Fail(ip)
		s.acctLimit.Fail(acct)
		s.log.Warn("remote login failed", "event", "remote_login_failed", "from", ip, "email", acct)
		fail(http.StatusUnauthorized, loginFailure)
		return
	}
	s.ipLimit.Reset(ip)
	s.acctLimit.Reset(acct)
	id, err := s.sessions.Create(u.Email)
	if err != nil {
		s.log.Error("session create failed", "event", "remote_session_failed", "error", err.Error())
		fail(http.StatusInternalServerError, "Could not start a session; see the log.")
		return
	}
	s.setSessionCookie(w, r, id)
	s.log.Info("remote login", "event", "remote_login", "user", u.Email, "from", ip)
	http.Redirect(w, r, next, http.StatusFound)
}

func fmtWait(d time.Duration) string {
	m := int(d.Minutes()) + 1
	if m <= 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, u accounts.User) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Revoke(c.Value)
	}
	clearSessionCookie(w)
	s.log.Info("remote logout", "event", "remote_logout", "user", u.Email)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// setPasswordData feeds the invite/reset page.
type setPasswordData struct {
	Product string
	Email   string
	Invite  bool
	Error   string
	Invalid bool
}

func (s *Server) handleSetPasswordPage(w http.ResponseWriter, r *http.Request) {
	link, ok := s.users.LookupLink(r.PathValue("secret"))
	if !ok {
		s.render(w, http.StatusGone, "setpassword.html", setPasswordData{Product: ProductName, Invalid: true})
		return
	}
	s.render(w, http.StatusOK, "setpassword.html", setPasswordData{Product: ProductName, Email: link.Email, Invite: link.Kind == accounts.LinkInvite})
}

func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	secret := r.PathValue("secret")
	link, ok := s.users.LookupLink(secret)
	if !ok {
		s.render(w, http.StatusGone, "setpassword.html", setPasswordData{Product: ProductName, Invalid: true})
		return
	}
	data := setPasswordData{Product: ProductName, Email: link.Email, Invite: link.Kind == accounts.LinkInvite}
	p1, p2 := r.PostFormValue("password"), r.PostFormValue("password2")
	if p1 != p2 {
		data.Error = "The two passwords do not match."
		s.render(w, http.StatusBadRequest, "setpassword.html", data)
		return
	}
	u, err := s.users.Redeem(secret, p1)
	if err != nil {
		if errors.Is(err, accounts.ErrLinkInvalid) {
			s.render(w, http.StatusGone, "setpassword.html", setPasswordData{Product: ProductName, Invalid: true})
			return
		}
		data.Error = err.Error()
		s.render(w, http.StatusBadRequest, "setpassword.html", data)
		return
	}
	// A reset link ends every other session of the account.
	s.sessions.RevokeUser(u.Email, "")
	id, err := s.sessions.Create(u.Email)
	if err != nil {
		s.log.Error("session create failed", "event", "remote_session_failed", "error", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.setSessionCookie(w, r, id)
	s.log.Info("remote link redeemed", "event", "remote_link_redeemed", "user", u.Email, "kind", string(link.Kind), "from", clientIP(r))
	http.Redirect(w, r, "/", http.StatusFound)
}

// accountData feeds the account page.
type accountData struct {
	Nav     navData
	Message string
	Error   string
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request, u accounts.User) {
	s.render(w, http.StatusOK, "account.html", accountData{Nav: s.nav(u, "account")})
}

func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request, u accounts.User) {
	current := r.PostFormValue("current")
	p1, p2 := r.PostFormValue("password"), r.PostFormValue("password2")
	data := accountData{Nav: s.nav(u, "account")}
	if _, ok := s.users.Verify(u.Email, current); !ok {
		data.Error = "The current password is wrong."
		s.render(w, http.StatusBadRequest, "account.html", data)
		return
	}
	if p1 != p2 {
		data.Error = "The two new passwords do not match."
		s.render(w, http.StatusBadRequest, "account.html", data)
		return
	}
	if err := s.users.SetPassword(u.Email, p1); err != nil {
		data.Error = err.Error()
		s.render(w, http.StatusBadRequest, "account.html", data)
		return
	}
	// Other browsers are logged out; this one stays.
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	s.sessions.RevokeUser(u.Email, keep)
	s.log.Info("remote password changed", "event", "remote_password_changed", "user", u.Email)
	data.Message = "Password changed. Other devices will need to log in again."
	s.render(w, http.StatusOK, "account.html", data)
}

// flash is a one-shot message for the next page load (post/redirect/get
// without losing the generated link).
type flash struct {
	Message string
	Error   string
	// Link, when set, is a freshly issued invite or reset link to show
	// with a copy control.
	Link        string
	LinkEmail   string
	LinkKind    string
	LinkExpires time.Time
	Emailed     bool
	EmailError  string
}

// flashStore keeps flashes per login session, in memory.
type flashStore struct {
	mu sync.Mutex
	m  map[string]flash
}

func newFlashStore() *flashStore { return &flashStore{m: map[string]flash{}} }

func (f *flashStore) put(key string, fl flash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = fl
}

func (f *flashStore) take(key string) (flash, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.m[key]
	delete(f.m, key)
	return fl, ok
}

// flashKey identifies the browser for flashes: the session cookie.
func flashKey(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return clientIP(r)
}

// withTimeout bounds blocking side work (sending email) so a page never
// hangs on a slow mail server.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
