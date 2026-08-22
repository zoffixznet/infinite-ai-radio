package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Session is one logged-in browser.
type Session struct {
	// Email is the account the session belongs to.
	Email   string    `json:"email"`
	Created time.Time `json:"created"`
	// Expires is when the session stops being accepted; it slides
	// forward while the session is in use.
	Expires time.Time `json:"expires"`
}

// DefaultSessionTTL keeps a browser logged in for a month of inactivity:
// it is a radio, not a bank.
const DefaultSessionTTL = 30 * 24 * time.Hour

// Sessions persists login sessions so a restart does not log everyone
// out. Only the SHA-256 of each session id is stored: the file never
// contains anything that could be replayed as a cookie, and lookups are
// map reads on the hash, so timing reveals nothing about the secret.
type Sessions struct {
	path string
	ttl  time.Duration

	mu   sync.Mutex
	byID map[string]Session // key: hex sha256 of the session id
	seen fileStamp
	now  func() time.Time
}

// NewSessions returns a session store backed by the file at path.
func NewSessions(path string, ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Sessions{path: path, ttl: ttl, byID: map[string]Session{}, now: time.Now}
}

type sessionsFile struct {
	Version  int                `json:"version"`
	Sessions map[string]Session `json:"sessions"`
}

func hashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// reload re-reads the file when it changed. Callers hold s.mu.
func (s *Sessions) reload() error {
	if !s.seen.changed(s.path) {
		return nil
	}
	var f sessionsFile
	st, err := readJSON(s.path, &f)
	if err != nil {
		return fmt.Errorf("reading sessions file: %w", err)
	}
	s.byID = f.Sessions
	if s.byID == nil {
		s.byID = map[string]Session{}
	}
	s.seen = st
	return nil
}

// persist drops expired sessions and writes the file. Callers hold s.mu.
func (s *Sessions) persist() error {
	now := s.now()
	for k, v := range s.byID {
		if !v.Expires.After(now) {
			delete(s.byID, k)
		}
	}
	st, err := writeJSON(s.path, sessionsFile{Version: 1, Sessions: s.byID})
	if err != nil {
		return fmt.Errorf("writing sessions file: %w", err)
	}
	s.seen = st
	return nil
}

// Create starts a session for email and returns the new session id (the
// cookie value): 256 random bits, URL-safe base64.
func (s *Sessions) Create(email string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return "", err
	}
	now := s.now()
	s.byID[hashID(id)] = Session{Email: email, Created: now, Expires: now.Add(s.ttl)}
	return id, s.persist()
}

// renewAfter is how much of the TTL must have elapsed before a lookup
// slides the expiry forward (keeps writes rare).
const renewAfter = time.Hour

// Lookup resolves a session id. Expired or unknown ids fail. Active
// sessions slide their expiry forward.
func (s *Sessions) Lookup(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return Session{}, false
	}
	key := hashID(id)
	sess, ok := s.byID[key]
	now := s.now()
	if !ok || !sess.Expires.After(now) {
		return Session{}, false
	}
	if sess.Expires.Sub(now) < s.ttl-renewAfter {
		sess.Expires = now.Add(s.ttl)
		s.byID[key] = sess
		s.persist()
	}
	return sess, true
}

// Revoke ends one session (logout).
func (s *Sessions) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	delete(s.byID, hashID(id))
	return s.persist()
}

// RevokeUser ends every session of an account except keepID (empty
// keeps none): used after password changes, resets and deletions.
func (s *Sessions) RevokeUser(email, keepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	keep := ""
	if keepID != "" {
		keep = hashID(keepID)
	}
	for k, v := range s.byID {
		if v.Email == email && k != keep {
			delete(s.byID, k)
		}
	}
	return s.persist()
}

// Count reports active (unexpired) sessions.
func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return 0
	}
	n := 0
	now := s.now()
	for _, v := range s.byID {
		if v.Expires.After(now) {
			n++
		}
	}
	return n
}
