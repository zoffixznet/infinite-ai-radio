package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal SMTP server capturing one message. With a
// certificate it advertises and performs STARTTLS.
type fakeSMTP struct {
	ln   net.Listener
	cert *tls.Certificate

	mu      sync.Mutex
	auth    string
	from    string
	rcpt    string
	message string
	tlsUsed bool
}

func newFakeSMTP(t *testing.T, cert *tls.Certificate) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, cert: cert}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	f.handle(conn)
}

func (f *fakeSMTP) handle(conn net.Conn) {
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	say := func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
	say("220 fake ESMTP")
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			if f.cert != nil && !f.tlsUsed {
				say("250-fake")
				say("250-STARTTLS")
				say("250 AUTH PLAIN")
			} else {
				say("250-fake")
				say("250 AUTH PLAIN")
			}
		case up == "STARTTLS":
			say("220 go ahead")
			tconn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*f.cert}})
			if err := tconn.Handshake(); err != nil {
				return
			}
			f.mu.Lock()
			f.tlsUsed = true
			f.mu.Unlock()
			conn = tconn
			rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
			say = func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
		case strings.HasPrefix(up, "AUTH PLAIN"):
			f.mu.Lock()
			f.auth = strings.TrimSpace(line[len("AUTH PLAIN"):])
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.TrimSpace(line[len("MAIL FROM:"):])
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = strings.TrimSpace(line[len("RCPT TO:"):])
			f.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var msg strings.Builder
			for {
				l, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				msg.WriteString(l)
			}
			f.mu.Lock()
			f.message = msg.String()
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func selfSigned(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestSendPlainWithAuth(t *testing.T) {
	srv := newFakeSMTP(t, nil)
	s := New(Config{
		Host: "127.0.0.1", Port: srv.port(), TLS: "none",
		Username: "radio", Password: "hunter22", From: "radio@example.com",
	})
	if !s.Configured() {
		t.Fatal("sender not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := "Your link:\nhttp://127.0.0.1:8246/set-password/abc\n.leading dot line"
	if err := s.Send(ctx, "jane@example.com", "Your Infinite AI Radio invite", body); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.from != "<radio@example.com>" || srv.rcpt != "<jane@example.com>" {
		t.Fatalf("envelope = %q -> %q", srv.from, srv.rcpt)
	}
	creds, _ := base64.StdEncoding.DecodeString(srv.auth)
	if string(creds) != "\x00radio\x00hunter22" {
		t.Fatalf("auth = %q", creds)
	}
	for _, want := range []string{
		"Subject: Your Infinite AI Radio invite\r\n",
		"To: jane@example.com\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"http://127.0.0.1:8246/set-password/abc\r\n",
		"..leading dot line\r\n", // dot-stuffed on the wire
	} {
		if !strings.Contains(srv.message, want) {
			t.Fatalf("message missing %q:\n%s", want, srv.message)
		}
	}
}

func TestSendSTARTTLS(t *testing.T) {
	srv := newFakeSMTP(t, selfSigned(t))
	s := New(Config{Host: "127.0.0.1", Port: srv.port(), From: "radio@example.com"})
	// Trust the test's self-signed certificate the proper way.
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(srv.cert.Certificate[0])
	pool.AddCert(leaf)
	s.tlsConfig = &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Send(ctx, "jane@example.com", "hi", "body"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.tlsUsed {
		t.Fatal("STARTTLS was not negotiated")
	}
	if !strings.Contains(srv.message, "body\r\n") {
		t.Fatalf("message = %q", srv.message)
	}
}

func TestSTARTTLSRequiredByDefault(t *testing.T) {
	srv := newFakeSMTP(t, nil) // no STARTTLS offered
	s := New(Config{Host: "127.0.0.1", Port: srv.port(), From: "radio@example.com"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Send(ctx, "jane@example.com", "hi", "body")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected a STARTTLS refusal, got %v", err)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("empty config counts as configured")
	}
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("empty config validates")
	}
	if err := (Config{Host: "smtp.example.com"}).Validate(); err == nil {
		t.Fatal("missing from validates")
	}
	if err := (Config{Host: "smtp.example.com", From: "a@b.c", TLS: "maybe"}).Validate(); err == nil {
		t.Fatal("bad tls mode validates")
	}
	for _, tc := range []struct {
		tls  string
		port int
	}{{"", 587}, {"starttls", 587}, {"tls", 465}, {"none", 25}} {
		if got := (Config{TLS: tc.tls}).port(); got != tc.port {
			t.Errorf("default port for %q = %d, want %d", tc.tls, got, tc.port)
		}
	}
	if got := (Config{Port: 2525}).port(); got != 2525 {
		t.Errorf("explicit port ignored: %d", got)
	}
	if !strings.Contains(Message("a@x.org", "b@y.org", "s", "t"), "Message-ID: <") {
		t.Error("message lacks Message-ID")
	}
}
