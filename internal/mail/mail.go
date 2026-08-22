// Package mail sends the remote's account emails (invite and reset
// links) through a user-configured SMTP server. It is optional
// convenience: when no server is configured the links are simply shown
// to the admin to pass on.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config is the SMTP setup from the configuration file.
type Config struct {
	// Host is the SMTP server; empty disables email.
	Host string `json:"host"`
	// Port defaults by TLS mode: 587 for starttls, 465 for tls, 25 for
	// none.
	Port int `json:"port"`
	// Username and Password authenticate with PLAIN when Username is set.
	Username string `json:"username"`
	Password string `json:"password"`
	// From is the sender address.
	From string `json:"from"`
	// TLS is "starttls" (default), "tls" (implicit TLS) or "none".
	TLS string `json:"tls"`
}

// Configured reports whether a server is set up.
func (c Config) Configured() bool { return strings.TrimSpace(c.Host) != "" }

// mode returns the normalized TLS mode.
func (c Config) mode() string {
	switch strings.ToLower(strings.TrimSpace(c.TLS)) {
	case "tls", "implicit", "ssl":
		return "tls"
	case "none", "plain":
		return "none"
	default:
		return "starttls"
	}
}

// port returns the configured or default port.
func (c Config) port() int {
	if c.Port > 0 {
		return c.Port
	}
	switch c.mode() {
	case "tls":
		return 465
	case "none":
		return 25
	default:
		return 587
	}
}

// Validate reports configuration problems before any connection.
func (c Config) Validate() error {
	if !c.Configured() {
		return errors.New("no SMTP server configured (remote.smtp.host)")
	}
	if strings.TrimSpace(c.From) == "" {
		return errors.New("remote.smtp.from is required")
	}
	switch strings.ToLower(strings.TrimSpace(c.TLS)) {
	case "", "starttls", "tls", "implicit", "ssl", "none", "plain":
	default:
		return fmt.Errorf("remote.smtp.tls must be starttls, tls or none (got %q)", c.TLS)
	}
	return nil
}

// Sender sends plain-text messages.
type Sender struct {
	cfg Config
	// tlsConfig lets tests trust a self-signed server certificate.
	tlsConfig *tls.Config
	// timeout bounds the whole exchange.
	timeout time.Duration
}

// New returns a sender for cfg.
func New(cfg Config) *Sender {
	return &Sender{cfg: cfg, timeout: 30 * time.Second}
}

// Configured reports whether sending is possible.
func (s *Sender) Configured() bool { return s != nil && s.cfg.Configured() }

// Send delivers one plain-text message.
func (s *Sender) Send(ctx context.Context, to, subject, body string) error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	host := strings.TrimSpace(s.cfg.Host)
	addr := net.JoinHostPort(host, fmt.Sprint(s.cfg.port()))
	tcfg := s.tlsConfig
	if tcfg == nil {
		tcfg = &tls.Config{ServerName: host}
	}

	dialer := &net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(s.timeout))
	if s.cfg.mode() == "tls" {
		conn = tls.Client(conn, tcfg)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting from %s: %w", addr, err)
	}
	defer c.Close()

	if s.cfg.mode() == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS; set remote.smtp.tls to \"tls\" or \"none\" if that is intended", addr)
		}
		if err := c.StartTLS(tcfg); err != nil {
			return fmt.Errorf("STARTTLS with %s: %w", addr, err)
		}
	}
	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP login as %s failed: %w", s.cfg.Username, err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("sender %s rejected: %w", s.cfg.From, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("recipient %s rejected: %w", to, err)
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(Message(s.cfg.From, to, subject, body))); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message rejected: %w", err)
	}
	return c.Quit()
}

// Message assembles an RFC 5322 plain-text message.
func Message(from, to, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", strings.ReplaceAll(strings.ReplaceAll(subject, "\r", " "), "\n", " "))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%d.iar@%s>\r\n", time.Now().UnixNano(), domainOf(from))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line // dot-stuffing
		}
		b.WriteString(line + "\r\n")
	}
	return b.String()
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.Trim(addr[i+1:], "<> ")
	}
	return "localhost"
}
