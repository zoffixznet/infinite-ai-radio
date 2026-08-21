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
	"strings"
	"time"

	"bgm/internal/player"
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
	// BindOverride replaces the default localhost+tailnet binding.
	BindOverride string
}

// Server is the running remote.
type Server struct {
	cfg      Config
	ctl      Controls
	streamer *Streamer
	log      *slog.Logger

	// Addrs are the addresses actually listening; TailnetIP is the
	// detected Tailscale address ("" when absent).
	Addrs     []string
	TailnetIP string
}

// Start resolves bind addresses, starts the shared encoder and serves on
// every address. It returns after the listeners are accepting.
func Start(ctx context.Context, cfg Config, ctl Controls, streamer *Streamer, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, ctl: ctl, streamer: streamer, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /stream.mp3", s.handleStream)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("POST /steer", s.handleSteer)
	mux.HandleFunc("POST /new", s.handleNew)
	mux.HandleFunc("POST /save", s.handleSave)

	handler := s.gate(mux)
	addrs, tailnetIP := BindAddrs(cfg.BindOverride, cfg.Port)
	s.TailnetIP = tailnetIP

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
	log.Info("remote listening", "event", "remote_up", "addrs", strings.Join(s.Addrs, ","), "tailnet_ip", tailnetIP, "token", cfg.Token != "")
	return s, nil
}

// gate enforces the shared-secret token when one is configured.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got := r.URL.Query().Get("token")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				http.Error(w, "missing or wrong token", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
