package remote

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"iar/internal/accounts"
	"iar/internal/player"
	"iar/internal/session"
	"iar/internal/snippets"
)

//go:embed assets/*.html assets/*.css assets/app.js assets/manifest.webmanifest assets/icons/*.png
var assetFS embed.FS

// ProductName is the name shown on every page and in emails.
const ProductName = "Infinite AI Radio"

// Controls is what the remote needs from the player; the orchestrator
// implements it (the same methods the terminal UI drives).
type Controls interface {
	Steer(text string) string
	NewSession(prompt string) string
	Skip() string
	SaveSnippet(which, tag string) string
	NameSession(name string) string
	LoadByName(name string) string
	DeleteSession(name string) string
	Listing() session.Listing
	CurrentName() string
	Status() player.Status
	Announce(text string)
}

// Config configures the remote server.
type Config struct {
	// Port to listen on.
	Port int
	// Binds lists extra addresses to listen on (additive to localhost
	// and the tailnet address).
	Binds []string
	// AllowedHosts adds hostnames/IPs to the Host-header allowlist.
	AllowedHosts []string
	// Users and Sessions are the account stores; both are required.
	Users    *accounts.Store
	Sessions *accounts.Sessions
	// Mailer, when configured, emails invite and reset links. Nil or an
	// unconfigured sender means links are handed over by the admin.
	Mailer Mailer
	// SnippetsDir is where saved chunks live (served in saved mode).
	SnippetsDir string
}

// Server is the running remote.
type Server struct {
	cfg      Config
	ctl      Controls
	streamer *Streamer
	log      *slog.Logger
	// allowedHosts is the lowercase hostname allowlist for the Host
	// header and Origin checks (DNS-rebinding defense).
	allowedHosts map[string]bool

	users    *accounts.Store
	sessions *accounts.Sessions
	mailer   Mailer
	catalog  *snippets.Catalog
	// ipLimit and acctLimit throttle failed logins per address and per
	// account.
	ipLimit   *accounts.Limiter
	acctLimit *accounts.Limiter
	tmpl      *template.Template
	flashes   *flashStore

	// Addrs are the addresses actually listening; TailnetIP is the
	// detected Tailscale address ("" when absent).
	Addrs     []string
	TailnetIP string
}

// Mailer sends the account emails; *mail.Sender implements it.
type Mailer interface {
	// Configured reports whether sending is possible at all.
	Configured() bool
	// Send delivers one plain-text message.
	Send(ctx context.Context, to, subject, body string) error
}

// emailConfigured reports whether links are also emailed.
func (s *Server) emailConfigured() bool {
	return s.mailer != nil && s.mailer.Configured()
}

// csrfHeader accompanies the page's own fetch requests. A mutation must
// carry it or an allowed Origin (which browsers attach to form posts);
// cross-site senders can do neither without a CORS preflight, which is
// never granted.
const csrfHeader = "X-IAR-Remote"

// Login throttling: failures per address and per account inside the
// window before attempts are refused.
const (
	loginAttempts = 10
	loginWindow   = 15 * time.Minute
)

// newServer wires a server without listening (tests use it directly).
func newServer(cfg Config, ctl Controls, streamer *Streamer, log *slog.Logger) (*Server, error) {
	if cfg.Users == nil || cfg.Sessions == nil {
		return nil, fmt.Errorf("remote: account stores are required")
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"clock": fmtClock,
		"until": fmtUntil,
	}).ParseFS(assetFS, "assets/*.html", "assets/*.css")
	if err != nil {
		return nil, fmt.Errorf("remote: parsing page templates: %w", err)
	}
	return &Server{
		cfg:       cfg,
		ctl:       ctl,
		streamer:  streamer,
		log:       log,
		users:     cfg.Users,
		sessions:  cfg.Sessions,
		mailer:    cfg.Mailer,
		catalog:   snippets.NewCatalog(cfg.SnippetsDir),
		ipLimit:   accounts.NewLimiter(loginAttempts, loginWindow),
		acctLimit: accounts.NewLimiter(loginAttempts, loginWindow),
		tmpl:      tmpl,
		flashes:   newFlashStore(),
	}, nil
}

// Start resolves bind addresses, starts the shared encoder and serves on
// every address. It returns after the listeners are accepting.
func Start(ctx context.Context, cfg Config, ctl Controls, streamer *Streamer, log *slog.Logger) (*Server, error) {
	s, err := newServer(cfg, ctl, streamer, log)
	if err != nil {
		return nil, err
	}
	binding := ResolveBinding(cfg.Binds, cfg.Port)
	s.TailnetIP = binding.TailnetIP
	s.buildAllowedHosts(binding.ExtraHosts)
	handler := s.buildHandler()

	if err := streamer.Start(ctx); err != nil {
		return nil, err
	}

	var listeners []net.Listener
	for _, addr := range binding.Addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			for _, prev := range listeners {
				prev.Close()
			}
			return nil, err
		}
		listeners = append(listeners, l)
		s.Addrs = append(s.Addrs, l.Addr().String())
	}
	srv := &http.Server{Handler: handler}
	for _, l := range listeners {
		go srv.Serve(l)
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	log.Info("remote listening", "event", "remote_up", "addrs", strings.Join(s.Addrs, ","),
		"tailnet_ip", s.TailnetIP, "accounts", s.users.Count(), "email", s.emailConfigured())
	return s, nil
}

// NeedsSetup reports whether no account exists yet (the remote then
// serves only the setup notice).
func (s *Server) NeedsSetup() bool { return s.users.Count() == 0 }

// buildAllowedHosts assembles the hostname allowlist: loopback names,
// the tailnet address, every explicitly bound address (or, for wildcard
// binds, all machine interface addresses), and configured extras.
func (s *Server) buildAllowedHosts(bound []string) {
	s.allowedHosts = map[string]bool{
		"localhost": true,
		"127.0.0.1": true,
		"::1":       true,
	}
	if s.TailnetIP != "" {
		s.allowedHosts[s.TailnetIP] = true
	}
	for _, h := range bound {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.allowedHosts[h] = true
		}
	}
	for _, h := range s.cfg.AllowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.allowedHosts[h] = true
		}
	}
}

// buildHandler wires the route mux behind the security middleware.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	// Public (no login): static assets (page code, icons, the app
	// manifest, which browsers fetch without credentials).
	mux.HandleFunc("GET /app.js", s.staticAsset("assets/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /manifest.webmanifest", s.staticAsset("assets/manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /icons/{file}", s.handleIcon)
	// Public (no login): the login page and the invite/reset links.
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /set-password/{secret}", s.handleSetPasswordPage)
	mux.HandleFunc("POST /set-password/{secret}", s.handleSetPassword)
	// Listening: any account.
	mux.HandleFunc("GET /{$}", s.page(s.handlePlayer))
	mux.HandleFunc("POST /logout", s.page(s.handleLogout))
	mux.HandleFunc("GET /me", s.api(s.handleMe))
	mux.HandleFunc("GET /state", s.api(s.handleState))
	mux.HandleFunc("GET /stream.mp3", s.api(s.handleStream))
	mux.HandleFunc("GET /api/chunks", s.api(s.handleChunks))
	mux.HandleFunc("GET /chunks/{tag}/{file}", s.api(s.handleChunkFile))
	mux.HandleFunc("GET /account", s.page(s.handleAccountPage))
	mux.HandleFunc("POST /account/password", s.page(s.handleAccountPassword))
	// Per-permission actions.
	mux.HandleFunc("POST /steer", s.apiPerm("steer", permSteer, s.handleSteer))
	mux.HandleFunc("POST /next", s.apiPerm("steer", permSteer, s.handleNext))
	mux.HandleFunc("POST /new", s.apiPerm("new prompt", permNewPrompt, s.handleNew))
	mux.HandleFunc("POST /save", s.apiPerm("save", permSave, s.handleSave))
	// Sessions: listing for everyone; loading changes what everyone
	// hears (new-prompt permission); naming is a save; deleting is
	// admin-only.
	mux.HandleFunc("GET /api/sessions", s.api(s.handleSessions))
	mux.HandleFunc("POST /sessions/save", s.apiPerm("save", permSave, s.handleSessionSave))
	mux.HandleFunc("POST /sessions/load", s.apiPerm("new prompt", permNewPrompt, s.handleSessionLoad))
	mux.HandleFunc("POST /sessions/delete", s.apiPerm("delete sessions", permAdmin, s.handleSessionDelete))
	// Admin.
	mux.HandleFunc("GET /users", s.pagePerm("users", permAdmin, s.handleUsersPage))
	mux.HandleFunc("POST /users/create", s.pagePerm("users", permAdmin, s.handleUserCreate))
	mux.HandleFunc("POST /users/update", s.pagePerm("users", permAdmin, s.handleUserUpdate))
	mux.HandleFunc("POST /users/delete", s.pagePerm("users", permAdmin, s.handleUserDelete))
	mux.HandleFunc("POST /users/link", s.pagePerm("users", permAdmin, s.handleUserLink))
	mux.HandleFunc("POST /users/revoke-link", s.pagePerm("users", permAdmin, s.handleUserRevokeLink))
	return s.gate(mux)
}

// hostAllowed validates the Host header against the allowlist (DNS
// rebinding defense: a foreign hostname resolved to the local machine must
// never reach the handlers).
func (s *Server) hostAllowed(hostHeader string) bool {
	host := hostHeader
	if h, p, err := net.SplitHostPort(hostHeader); err == nil {
		if p != strconv.Itoa(s.cfg.Port) {
			return false
		}
		host = h
	}
	return s.allowedHosts[strings.ToLower(strings.Trim(host, "[]"))]
}

// originAllowed validates an Origin header value (empty is allowed: not
// all requests carry one).
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return s.hostAllowed(u.Host)
}

// gate applies, in order: the Host allowlist, the Origin check, the
// cross-site guard on mutations, and the zero-accounts setup notice.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			s.log.Warn("remote request rejected: host not allowed", "event", "remote_host_rejected", "host", r.Host, "from", r.RemoteAddr)
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if !s.originAllowed(origin) {
			s.log.Warn("remote request rejected: origin not allowed", "event", "remote_origin_rejected", "origin", origin, "from", r.RemoteAddr)
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && origin == "" && r.Header.Get(csrfHeader) == "" {
			s.log.Warn("remote request rejected: no origin or header", "event", "remote_csrf_rejected", "path", r.URL.Path, "from", r.RemoteAddr)
			http.Error(w, "missing Origin or "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if s.NeedsSetup() && !staticPath(r.URL.Path) {
			s.log.Info("remote request before setup", "event", "remote_setup_needed", "path", r.URL.Path, "from", r.RemoteAddr)
			w.Header().Set("Cache-Control", "no-store")
			s.render(w, http.StatusServiceUnavailable, "setup.html", map[string]any{"Product": ProductName})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// staticPath reports whether a request path is a public static asset.
func staticPath(p string) bool {
	return p == "/app.js" || p == "/manifest.webmanifest" || strings.HasPrefix(p, "/icons/")
}

// staticAsset serves one embedded file with light caching.
func (s *Server) staticAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assetFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(data)
	}
}

// handleIcon serves one embedded icon by file name.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if strings.ContainsAny(file, "/\\") || !strings.HasSuffix(file, ".png") {
		http.NotFound(w, r)
		return
	}
	s.staticAsset("assets/icons/"+file, "image/png")(w, r)
}

// render executes a page template.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("page render failed", "event", "remote_render_failed", "page", name, "error", err.Error())
	}
}

// navData is what the shared navigation needs.
type navData struct {
	Product string
	Email   string
	Admin   bool
	Page    string
}

func (s *Server) nav(u accounts.User, page string) navData {
	return navData{Product: ProductName, Email: u.Email, Admin: u.Perms.Admin, Page: page}
}

// handlePlayer serves the main page.
func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request, u accounts.User) {
	s.render(w, http.StatusOK, "player.html", map[string]any{"Nav": s.nav(u, "player")})
}

// meJSON tells the page who is logged in and what they may do, so it can
// hide controls the server would refuse anyway.
type meJSON struct {
	Email     string `json:"email"`
	Admin     bool   `json:"admin"`
	Steer     bool   `json:"steer"`
	NewPrompt bool   `json:"new_prompt"`
	Save      bool   `json:"save"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u accounts.User) {
	writeJSON(w, http.StatusOK, meJSON{
		Email: u.Email, Admin: u.Perms.Admin, Steer: u.Perms.Steer,
		NewPrompt: u.Perms.NewPrompt, Save: u.Perms.Save,
	})
}

// handleStream serves the shared MP3 stream to one client.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, u accounts.User) {
	pre, ch, cancel := s.streamer.Subscribe()
	defer cancel()
	s.log.Info("stream client connected", "event", "remote_stream_open", "user", u.Email, "from", r.RemoteAddr, "listeners", s.streamer.Listeners())
	defer s.log.Info("stream client left", "event", "remote_stream_close", "user", u.Email, "from", r.RemoteAddr)

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	if len(pre) > 0 {
		if _, err := w.Write(pre); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// stateJSON is the now-playing payload the page polls.
type stateJSON struct {
	State      string `json:"state"`
	Source     string `json:"source"`
	Session    string `json:"session"`
	Phase      string `json:"phase"`
	PhaseInfo  string `json:"phase_info"`
	Elapsed    string `json:"elapsed"`
	Duration   string `json:"duration"`
	Queued     int    `json:"queued"`
	Generating bool   `json:"generating"`
	Paused     bool   `json:"paused"`
	Volume     int    `json:"volume"`
	Underruns  int64  `json:"underruns"`
	Listeners  int    `json:"listeners"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, u accounts.User) {
	st := s.ctl.Status()
	out := stateJSON{
		State:      st.State,
		Source:     st.Source,
		Session:    st.Session,
		Queued:     st.Queued,
		Generating: st.Generating,
		Paused:     st.Paused,
		Volume:     st.Volume,
		Underruns:  st.Underruns,
		Listeners:  s.streamer.Listeners(),
	}
	if st.Phase != "" && st.Phase != "playing" {
		out.Phase = st.Phase
		out.PhaseInfo = st.PhaseElapsed.Round(time.Second).String() + " of ~" + st.PhaseExpected.Round(time.Second).String()
	}
	if st.Duration > 0 {
		out.Elapsed = fmtClock(st.Elapsed)
		out.Duration = fmtClock(st.Duration)
	}
	writeJSON(w, http.StatusOK, out)
}

func fmtClock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// fmtUntil says how long until t, coarsely ("6 days", "23 hours").
func fmtUntil(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours())+1)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// actionResponse is the reply to control posts.
type actionResponse struct {
	OK  bool   `json:"ok"`
	Ack string `json:"ack"`
}

func (s *Server) reply(w http.ResponseWriter, ack string) {
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Ack: ack})
}

// textField pulls a form/query text field with a length cap.
func textField(r *http.Request, name string) string {
	r.ParseForm()
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		v = strings.TrimSpace(r.FormValue(name))
	}
	if len(v) > 300 {
		v = v[:300]
	}
	return v
}

func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request, u accounts.User) {
	text := textField(r, "text")
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.Steer(text)
	s.ctl.Announce("remote steer by " + u.Email + ": " + text + " -> " + ack)
	s.reply(w, ack)
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request, u accounts.User) {
	ack := s.ctl.Skip()
	s.ctl.Announce("remote skip by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request, u accounts.User) {
	prompt := textField(r, "prompt")
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.NewSession(prompt)
	s.ctl.Announce("remote new session by " + u.Email + ": " + prompt)
	s.reply(w, ack)
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request, u accounts.User) {
	ack := s.ctl.SaveSnippet(textField(r, "which"), textField(r, "tag"))
	s.ctl.Announce("remote save by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

// sessionJSON is one row of the web session picker.
type sessionJSON struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Played says when the session last played ("" for presets).
	Played  string `json:"played"`
	Current bool   `json:"current"`
}

// sessionsJSON is the grouped listing: user-named, presets, auto-named.
type sessionsJSON struct {
	Current string        `json:"current"`
	Named   []sessionJSON `json:"named"`
	Presets []sessionJSON `json:"presets"`
	Auto    []sessionJSON `json:"auto"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request, u accounts.User) {
	l := s.ctl.Listing()
	now := time.Now()
	cur := s.ctl.CurrentName()
	out := sessionsJSON{Current: cur, Named: []sessionJSON{}, Presets: []sessionJSON{}, Auto: []sessionJSON{}}
	row := func(sess *session.Session) sessionJSON {
		return sessionJSON{Name: sess.Name, Summary: session.Summary(sess, 80), Played: session.Ago(now, sess.Played()), Current: sess.Name == cur}
	}
	for _, sess := range l.Named {
		out.Named = append(out.Named, row(sess))
	}
	for _, p := range l.Presets {
		out.Presets = append(out.Presets, sessionJSON{Name: p.Name, Summary: p.Description})
	}
	for _, sess := range l.Auto {
		out.Auto = append(out.Auto, row(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSessionSave(w http.ResponseWriter, r *http.Request, u accounts.User) {
	name := textField(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.NameSession(name)
	s.ctl.Announce("remote session save by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleSessionLoad(w http.ResponseWriter, r *http.Request, u accounts.User) {
	name := textField(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.LoadByName(name)
	s.ctl.Announce("remote load by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request, u accounts.User) {
	name := textField(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.DeleteSession(name)
	s.log.Info("remote session delete", "event", "remote_session_delete", "by", u.Email, "name", name, "ack", ack)
	s.ctl.Announce("remote delete by " + u.Email + ": " + ack)
	s.reply(w, ack)
}
