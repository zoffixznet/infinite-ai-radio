package remote

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"unicode/utf8"

	"iar/internal/accounts"
	"iar/internal/engine"
	"iar/internal/player"
	"iar/internal/prompting"
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
	LyricsGen(name string) string
	Languages() []player.LanguageState
	SetLanguage(name string, on bool) string
	SetLanguages(names []string) string
	NewSession(prompt string) string
	Skip() string
	ToggleLoop() string
	ToggleStandby() string
	Retitle(which, title string) string
	DeleteAutoSessions(olderThanDays int) string
	SaveSnippet(which, tag string) string
	NameSession(name string) string
	LoadByName(name string) string
	DeleteSession(name string) string
	Listing() session.Listing
	CurrentName() string
	Status() player.Status
	QueueTracks() (int, []player.QueueTrack)
	TrackData(id string) (*engine.Track, bool)
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
	trackMP3 *mp3Cache
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
		trackMP3:  newMP3Cache(),
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
	mux.HandleFunc("GET /api/queue", s.api(s.handleQueueList))
	mux.HandleFunc("GET /queue/{file}", s.api(s.handleQueueTrack))
	mux.HandleFunc("GET /chunks/{tag}/{file}", s.api(s.handleChunkFile))
	// Curating the saved library: renaming and regrouping are saves;
	// deleting follows the sessions precedent and stays admin-only.
	mux.HandleFunc("POST /chunks/rename", s.apiPerm("rename saved tracks", permSave, s.handleChunkRename))
	mux.HandleFunc("POST /chunks/move", s.apiPerm("move saved tracks", permSave, s.handleChunkMove))
	mux.HandleFunc("POST /chunks/delete", s.apiPerm("delete saved tracks", permAdmin, s.handleChunkDelete))
	mux.HandleFunc("GET /account", s.page(s.handleAccountPage))
	mux.HandleFunc("POST /account/password", s.page(s.handleAccountPassword))
	// Per-permission actions.
	mux.HandleFunc("POST /steer", s.apiPerm("steer", permSteer, s.handleSteer))
	mux.HandleFunc("POST /lyrics-gen", s.apiPerm("steer", permSteer, s.handleLyricsGen))
	// Switching a configured language on or off is steering; editing
	// the configured list writes the machine's config file.
	mux.HandleFunc("POST /language", s.apiPerm("steer", permSteer, s.handleLanguage))
	mux.HandleFunc("POST /languages", s.apiPerm("languages", permAdmin, s.handleLanguages))
	mux.HandleFunc("POST /next", s.apiPerm("steer", permSteer, s.handleNext))
	mux.HandleFunc("POST /loop", s.apiPerm("steer", permSteer, s.handleLoop))
	// Holding the whole radio is a steering-level act: it decides what
	// everyone hears, or stops hearing.
	mux.HandleFunc("POST /standby", s.apiPerm("steer", permSteer, s.handleStandby))
	mux.HandleFunc("POST /new", s.apiPerm("new prompt", permNewPrompt, s.handleNew))
	mux.HandleFunc("POST /save", s.apiPerm("save", permSave, s.handleSave))
	// Renaming what is playing is the same act as renaming it in the
	// saved list, and answers to the same permission.
	mux.HandleFunc("POST /retitle", s.apiPerm("rename songs", permSave, s.handleRetitle))
	// Sessions: listing for everyone; loading changes what everyone
	// hears (new-prompt permission); naming is a save; deleting is
	// admin-only.
	mux.HandleFunc("GET /api/sessions", s.api(s.handleSessions))
	mux.HandleFunc("POST /sessions/save", s.apiPerm("save", permSave, s.handleSessionSave))
	mux.HandleFunc("POST /sessions/load", s.apiPerm("new prompt", permNewPrompt, s.handleSessionLoad))
	mux.HandleFunc("POST /sessions/delete", s.apiPerm("delete sessions", permAdmin, s.handleSessionDelete))
	mux.HandleFunc("POST /sessions/delete-auto", s.apiPerm("delete sessions", permAdmin, s.handleSessionDeleteAuto))
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

// staticAsset serves one embedded file, revalidated on every load.
//
// The pages themselves are no-store and the stylesheet is inlined into
// them, so the page always arrives current - but the page's script did
// not. Served with a plain max-age and no validator, a phone kept
// running an hour-old app.js against a freshly updated page, which
// looks exactly like a fix that did not work. no-cache still lets the
// browser keep the bytes; it just has to ask first, and the ETag makes
// the answer a 304 almost every time.
func (s *Server) staticAsset(name, contentType string) http.HandlerFunc {
	data, err := assetFS.ReadFile(name)
	if err != nil {
		return func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	}
	etag := assetETag(data)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(data)
	}
}

// assetETag is a strong validator over the embedded bytes, computed
// once at startup because the assets cannot change while we run.
func assetETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches reports whether an If-None-Match header names this tag.
// The header may carry several tags, and a cache may weaken them.
func etagMatches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || tag == etag || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
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

// permData is what the player page needs to decide, at render time,
// which controls this account may see. Rendering them hidden server-side
// keeps the page from reflowing under a thumb when /me comes back.
type permData struct {
	Steer     bool
	NewPrompt bool
	Save      bool
	Admin     bool
}

// handlePlayer serves the main page.
func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request, u accounts.User) {
	s.render(w, http.StatusOK, "player.html", map[string]any{
		"Nav": s.nav(u, "player"),
		"Perms": permData{
			Steer: u.Perms.Steer, NewPrompt: u.Perms.NewPrompt,
			Save: u.Perms.Save, Admin: u.Perms.Admin,
		},
	})
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

// stateJSON is the shared now-playing and steering payload the page
// polls: every client and every reload renders the same state.
type stateJSON struct {
	State     string `json:"state"`
	Source    string `json:"source"`
	Session   string `json:"session"`
	Phase     string `json:"phase"`
	PhaseInfo string `json:"phase_info"`
	Elapsed   string `json:"elapsed"`
	Duration  string `json:"duration"`
	Queued    int    `json:"queued"`
	// Ready is how much music is actually secured, in the listener's
	// words ("11 songs, 37m"). Queued counts only what is decoded in
	// memory, which under phased generation is a fixed two and says
	// nothing about whether the radio is keeping up.
	Ready      string `json:"ready"`
	Generating bool   `json:"generating"`
	Paused     bool   `json:"paused"`
	Volume     int    `json:"volume"`
	Underruns  int64  `json:"underruns"`
	Listeners  int    `json:"listeners"`
	// Epoch identifies the steering context; it bumps whenever steering
	// changes what will be generated next.
	Epoch       int         `json:"epoch"`
	SessionDesc string      `json:"session_desc"`
	BasePrompt  string      `json:"base_prompt"`
	Tweaks      []tweakJSON `json:"tweaks"`
	Vocals      bool        `json:"vocals"`
	// LyricsGenerator is the effective lyric writer for vocal tracks;
	// LyricsGenerators lists the ones that can be switched to.
	LyricsGenerator  string    `json:"lyrics_generator"`
	LyricsGenerators []genJSON `json:"lyrics_generators"`
	// Track is the playing generated track (absent for stopgap audio);
	// Prev is the one before it.
	Track *trackJSON `json:"track,omitempty"`
	Prev  *trackJSON `json:"prev,omitempty"`
	// SavedIDs lists track ids already saved as snippets this run, so
	// clients grey their save buttons for whatever THEY are playing.
	SavedIDs []string `json:"saved_ids"`
	// Languages lists the configured vocal languages and which of them
	// the session sings in; empty means the engine chooses.
	Languages []langJSON `json:"languages"`
	// Looping reports the playing track is being replayed for want of
	// anything newer, so the page can say so instead of presenting it
	// as a fresh track.
	Looping bool `json:"looping,omitempty"`
	// LoopOn reports a listener asked the radio to repeat the playing
	// track, so every client's loop button shows the same state.
	LoopOn bool `json:"loop_on,omitempty"`
	// Standby reports the radio is held: nothing playing, nothing
	// generating. Every client says so, and warns a listener still
	// hearing their own banked songs.
	Standby bool `json:"standby,omitempty"`
	// Switching reports a steering or language change whose first fresh
	// track is still generating.
	Switching bool `json:"switching,omitempty"`
}

// langJSON is one configured vocal language as the page renders it.
type langJSON struct {
	Name string `json:"name"`
	// Engine reports the music engine has a tag for the language; the
	// others are sung from lyrics written in them, untagged.
	Engine bool `json:"engine"`
	On     bool `json:"on"`
	// Configured reports the language is in the machine's own list. One
	// that is not comes from the session, which was saved singing in
	// it; the page marks those so a listener can see why a language
	// they no longer have configured is being sung, and switch it off.
	Configured bool `json:"configured"`
}

// genJSON is one selectable lyric writer as the page renders it.
type genJSON struct {
	Name  string `json:"name"`
	Blurb string `json:"blurb"`
}

// tweakJSON is one steering input as the page renders it.
type tweakJSON struct {
	Raw         string `json:"raw"`
	Interpreted string `json:"interpreted"`
	Time        string `json:"time"`
}

// trackJSON identifies one playable track.
type trackJSON struct {
	ID        string  `json:"id"`
	Prompt    string  `json:"prompt"`
	DurationS float64 `json:"duration_s"`
	// Title and Subtitle are the short display names; Number is the
	// per-process play number ("Track N").
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Number   int    `json:"number,omitempty"`
	// Saved reports the track is already saved as a snippet.
	Saved bool `json:"saved"`
	// Lang names the language the track is sung in, in the listener's
	// own wording; empty when the music engine chose for itself.
	Lang string `json:"lang,omitempty"`
	// Lyrics is what the track is singing, absent for instrumentals.
	Lyrics string `json:"lyrics,omitempty"`
}

// readySummary says how much music is secured, in one short phrase.
// Phased generation buffers to disk, so the honest number is songs and
// minutes there, not the size of the in-memory prefetch.
func readySummary(st player.Status) string {
	if !st.Phased {
		return fmt.Sprintf("%d ready", st.Queued)
	}
	if st.BufferedTracks == 0 {
		return "0 ready"
	}
	mins := int(st.BufferedSeconds / 60)
	if mins >= 60 {
		return fmt.Sprintf("%d ready · %dh%02dm", st.BufferedTracks, mins/60, mins%60)
	}
	return fmt.Sprintf("%d ready · %dm", st.BufferedTracks, mins)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, u accounts.User) {
	st := s.ctl.Status()
	out := stateJSON{
		State:           st.State,
		Source:          st.Source,
		Session:         st.Session,
		Queued:          st.Queued,
		Ready:           readySummary(st),
		Generating:      st.Generating,
		Paused:          st.Paused,
		Volume:          st.Volume,
		Underruns:       st.Underruns,
		Listeners:       s.streamer.Listeners(),
		Epoch:           st.Epoch,
		SessionDesc:     st.SessionDesc,
		BasePrompt:      st.BasePrompt,
		Tweaks:          []tweakJSON{},
		Vocals:          st.Vocal,
		LyricsGenerator: st.LyricsGenerator,
	}
	for _, g := range prompting.Generators() {
		out.LyricsGenerators = append(out.LyricsGenerators, genJSON{Name: g.Name(), Blurb: g.Blurb()})
	}
	for _, tw := range st.Tweaks {
		out.Tweaks = append(out.Tweaks, tweakJSON{
			Raw: tw.Raw, Interpreted: tw.Interpreted, Time: tw.Time.Format(time.RFC3339),
		})
	}
	if st.TrackID != "" {
		out.Track = &trackJSON{
			ID: st.TrackID, Prompt: st.TrackPrompt, DurationS: st.Duration.Seconds(),
			Title: st.TrackTitle, Subtitle: st.TrackSubtitle, Number: st.TrackNum,
			Saved: st.TrackSaved, Lang: st.TrackLanguage, Lyrics: st.TrackLyrics,
		}
	}
	if st.PrevTrackID != "" {
		out.Prev = &trackJSON{ID: st.PrevTrackID, Prompt: st.PrevTrackPrompt, Title: st.PrevTrackTitle, Saved: st.PrevTrackSaved}
	}
	out.SavedIDs = st.SavedTrackIDs
	if out.SavedIDs == nil {
		out.SavedIDs = []string{}
	}
	out.Languages = []langJSON{}
	for _, l := range st.Languages {
		out.Languages = append(out.Languages, langJSON{
			Name: l.Name, Engine: l.Engine, On: l.On, Configured: l.Configured,
		})
	}
	out.Looping = st.Looping
	out.LoopOn = st.LoopOn
	out.Standby = st.Standby
	out.Switching = st.Switching
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
	// Saved answers /save without the page having to read English: the
	// track is in the snippets, or on its way there.
	Saved bool `json:"saved,omitempty"`
}

func (s *Server) reply(w http.ResponseWriter, ack string) {
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Ack: ack})
}

// textField pulls a form/query text field with a length cap.
func textField(r *http.Request, name string) string {
	return textFieldN(r, name, 300)
}

// textFieldN reads one form field, capped at max bytes. Fields that
// carry a list rather than a phrase need a larger cap: cutting one in
// half leaves a fragment that still parses as a value, and the caller
// then reports it as accepted.
func textFieldN(r *http.Request, name string, max int) string {
	r.ParseForm()
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		v = strings.TrimSpace(r.FormValue(name))
	}
	if len(v) > max {
		// Song names are written in their own language's script, where
		// one letter is several bytes: cutting on a byte would leave
		// half a letter behind and the name would render as a replacement
		// character.
		for max > 0 && !utf8.RuneStart(v[max]) {
			max--
		}
		v = strings.TrimSpace(v[:max])
	}
	return v
}

// maxTitleBytes bounds a song name. Generous in bytes because a name in
// a non-Latin script costs three of them a letter.
const maxTitleBytes = 240

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

func (s *Server) handleLyricsGen(w http.ResponseWriter, r *http.Request, u accounts.User) {
	name := textField(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.LyricsGen(name)
	s.ctl.Announce("remote lyric writer by " + u.Email + ": " + name + " -> " + ack)
	s.reply(w, ack)
}

// handleLanguage switches one configured vocal language on or off.
func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request, u accounts.User) {
	name := textField(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	on := textField(r, "on") == "1"
	ack := s.ctl.SetLanguage(name, on)
	s.ctl.Announce("remote language by " + u.Email + ": " + name + " -> " + ack)
	s.reply(w, ack)
}

// handleLanguages replaces the configured vocal-language list. The
// field is one comma-separated line, the way it is typed.
func (s *Server) handleLanguages(w http.ResponseWriter, r *http.Request, u accounts.User) {
	var names []string
	for _, part := range strings.Split(textFieldN(r, "names", 1200), ",") {
		if p := strings.TrimSpace(part); p != "" {
			names = append(names, p)
		}
	}
	ack := s.ctl.SetLanguages(names)
	s.ctl.Announce("remote languages by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request, u accounts.User) {
	ack := s.ctl.Skip()
	s.ctl.Announce("remote skip by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleLoop(w http.ResponseWriter, r *http.Request, u accounts.User) {
	ack := s.ctl.ToggleLoop()
	s.ctl.Announce("remote loop by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

func (s *Server) handleStandby(w http.ResponseWriter, r *http.Request, u accounts.User) {
	ack := s.ctl.ToggleStandby()
	s.ctl.Announce("remote standby by " + u.Email + ": " + ack)
	s.reply(w, ack)
}

// handleRetitle renames the song a listener is hearing. id is the
// track their own device is playing, when it has one of its own;
// without it the machine's current song is renamed.
func (s *Server) handleRetitle(w http.ResponseWriter, r *http.Request, u accounts.User) {
	title := textFieldN(r, "title", maxTitleBytes)
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	ack := s.ctl.Retitle(textField(r, "id"), title)
	s.ctl.Announce("remote rename by " + u.Email + ": " + ack)
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
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Ack: ack, Saved: savedAck(ack)})
}

// savedAck reports whether an acknowledgment from SaveSnippet means the
// track is in the snippets or on its way there, so the page can grey
// its save control without matching on English.
func savedAck(ack string) bool {
	return strings.HasPrefix(ack, "saving this track to ") || strings.HasPrefix(ack, "already saved:")
}

// sessionJSON is one row of the web session picker.
type sessionJSON struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Played says when the session last played ("" for presets).
	Played  string `json:"played"`
	Current bool   `json:"current"`
	// Group is the energy group (presets only).
	Group string `json:"group,omitempty"`
}

// sessionsJSON is the grouped listing: user-named, presets, auto-named.
type sessionsJSON struct {
	Current string `json:"current"`
	// CurrentPreset names the preset the playing session came from
	// ("" when it did not), so the picker can open its group.
	CurrentPreset string        `json:"current_preset"`
	Named         []sessionJSON `json:"named"`
	Presets       []sessionJSON `json:"presets"`
	Auto          []sessionJSON `json:"auto"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request, u accounts.User) {
	l := s.ctl.Listing()
	now := time.Now()
	cur := s.ctl.CurrentName()
	out := sessionsJSON{Current: cur, Named: []sessionJSON{}, Presets: []sessionJSON{}, Auto: []sessionJSON{}}
	row := func(sess *session.Session) sessionJSON {
		if sess.Name == cur {
			out.CurrentPreset = sess.Preset
		}
		return sessionJSON{Name: sess.Name, Summary: session.Summary(sess, 80), Played: session.Ago(now, sess.Played()), Current: sess.Name == cur}
	}
	for _, sess := range l.Named {
		out.Named = append(out.Named, row(sess))
	}
	for _, p := range l.Presets {
		out.Presets = append(out.Presets, sessionJSON{Name: p.Name, Summary: p.Description, Group: p.Group})
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

// handleSessionDeleteAuto clears out the sessions nobody named. They
// accumulate one per change to the sound, on purpose; this is the one
// gesture that empties them. An optional days field keeps the recent
// ones.
func (s *Server) handleSessionDeleteAuto(w http.ResponseWriter, r *http.Request, u accounts.User) {
	days, _ := strconv.Atoi(textField(r, "days"))
	if days < 0 {
		days = 0
	}
	ack := s.ctl.DeleteAutoSessions(days)
	s.log.Info("remote automatic session delete", "event", "remote_sessions_auto_delete",
		"by", u.Email, "days", days, "ack", ack)
	s.ctl.Announce("remote delete by " + u.Email + ": " + ack)
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
