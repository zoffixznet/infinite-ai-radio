package remote

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"iar/internal/player"
)

//go:embed assets/index.html
var pageHTML []byte

// Controls is what the remote needs from the player; the orchestrator
// implements it (the same methods the terminal UI drives).
type Controls interface {
	Steer(text string) string
	NewSession(prompt string) string
	SaveSnippet(which string) string
	Status() player.Status
	Announce(text string)
}

// Config configures the remote server.
type Config struct {
	// Port to listen on.
	Port int
	// Token, when non-empty, is required on every request.
	Token string
	// Binds lists extra addresses to listen on (additive to localhost
	// and the tailnet address). Any non-loopback entry requires Token.
	Binds []string
	// AllowedHosts adds hostnames/IPs to the Host-header allowlist.
	AllowedHosts []string
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

	// Addrs are the addresses actually listening; TailnetIP is the
	// detected Tailscale address ("" when absent).
	Addrs     []string
	TailnetIP string
}

// csrfHeader must accompany every mutating request. Cross-site senders
// cannot add it without a CORS preflight, which is never granted.
const csrfHeader = "X-IAR-Remote"

// tokenCookie carries the shared secret after the first ?token= visit.
const tokenCookie = "iar_token"

// ValidateBinds rejects insecure combinations before anything listens:
// bind entries beyond loopback make the remote reachable by hosts
// outside the default trust boundary, so they require a token.
func ValidateBinds(binds []string, token string) error {
	if token != "" {
		return nil
	}
	for _, b := range binds {
		b = strings.TrimSpace(b)
		if b == "" || isLoopbackHost(b) {
			continue
		}
		return fmt.Errorf("remote.bind entry %q may expose the remote beyond localhost and your tailnet; set remote.token to protect it, or remove the entry", b)
	}
	return nil
}

// Start resolves bind addresses, starts the shared encoder and serves on
// every address. It returns after the listeners are accepting.
func Start(ctx context.Context, cfg Config, ctl Controls, streamer *Streamer, log *slog.Logger) (*Server, error) {
	if err := ValidateBinds(cfg.Binds, cfg.Token); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, ctl: ctl, streamer: streamer, log: log}

	binding := ResolveBinding(cfg.Binds, cfg.Port)
	addrs := binding.Addrs
	s.TailnetIP = binding.TailnetIP
	s.buildAllowedHosts(binding.ExtraHosts)
	handler := s.buildHandler()

	if err := streamer.Start(ctx); err != nil {
		return nil, err
	}

	var listeners []net.Listener
	for _, addr := range addrs {
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
	log.Info("remote listening", "event", "remote_up", "addrs", strings.Join(s.Addrs, ","), "tailnet_ip", s.TailnetIP, "token", cfg.Token != "")
	return s, nil
}

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
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /stream.mp3", s.handleStream)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("POST /steer", s.handleSteer)
	mux.HandleFunc("POST /new", s.handleNew)
	mux.HandleFunc("POST /save", s.handleSave)
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
	if err != nil || u.Scheme != "http" {
		return false
	}
	return s.hostAllowed(u.Host)
}

// gate applies, in order: the Host allowlist, the Origin check, the CSRF
// header requirement on mutations, and the shared-secret token.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			s.log.Warn("remote request rejected: host not allowed", "event", "remote_host_rejected", "host", r.Host, "from", r.RemoteAddr)
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); !s.originAllowed(origin) {
			s.log.Warn("remote request rejected: origin not allowed", "event", "remote_origin_rejected", "origin", origin, "from", r.RemoteAddr)
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			s.log.Warn("remote request rejected: missing header", "event", "remote_csrf_rejected", "path", r.URL.Path, "from", r.RemoteAddr)
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if s.cfg.Token != "" {
			if !s.tokenOK(r) {
				http.Error(w, "missing or wrong token", http.StatusUnauthorized)
				return
			}
			// First visit with ?token= in the URL: move the secret
			// into a cookie and strip it from the address bar.
			if r.Method == http.MethodGet && r.URL.Query().Get("token") != "" && r.URL.Path == "/" {
				http.SetCookie(w, &http.Cookie{
					Name: tokenCookie, Value: r.URL.Query().Get("token"),
					Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// tokenOK checks the shared secret from, in order of preference, the
// Authorization header, the cookie, or a token query parameter. Values
// are never logged.
func (s *Server) tokenOK(r *http.Request) bool {
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		got = c.Value
	} else {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pageHTML)
}

// handleStream serves the shared MP3 stream to one client.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	pre, ch, cancel := s.streamer.Subscribe()
	defer cancel()
	s.log.Info("stream client connected", "event", "remote_stream_open", "from", r.RemoteAddr, "listeners", s.streamer.Listeners())
	defer s.log.Info("stream client left", "event", "remote_stream_close", "from", r.RemoteAddr)

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

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func fmtClock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// actionResponse is the reply to control posts.
type actionResponse struct {
	OK  bool   `json:"ok"`
	Ack string `json:"ack"`
}

func (s *Server) reply(w http.ResponseWriter, ack string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(actionResponse{OK: true, Ack: ack})
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

func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	text := textField(r, "text")
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.Steer(text)
	s.ctl.Announce("remote steer: " + text + " -> " + ack)
	s.reply(w, ack)
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	prompt := textField(r, "prompt")
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.NewSession(prompt)
	s.ctl.Announce("remote new session: " + prompt)
	s.reply(w, ack)
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	ack := s.ctl.SaveSnippet(textField(r, "which"))
	s.ctl.Announce("remote save: " + ack)
	s.reply(w, ack)
}
