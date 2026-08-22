// Package accounts holds the remote's user accounts: who may log in and
// what each account is allowed to do. Accounts live in one small JSON
// file (atomic writes, owner-only permissions); passwords are stored as
// bcrypt hashes. New accounts and password resets go through single-use
// links, so nobody but the account holder ever sees a password. The
// package also provides persisted login sessions and a login-attempt
// rate limiter.
package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Permissions are the independent capabilities an account may hold.
// Listening is implied for every account. Admin does not imply the other
// three: an admin can grant themselves whatever they need.
type Permissions struct {
	// Admin may create, edit and delete users and issue reset links.
	Admin bool `json:"admin"`
	// Steer may send steering text.
	Steer bool `json:"steer"`
	// NewPrompt may start a fresh session from a prompt.
	NewPrompt bool `json:"new_prompt"`
	// Save may save the currently playing track.
	Save bool `json:"save"`
}

// AllPermissions grants everything.
var AllPermissions = Permissions{Admin: true, Steer: true, NewPrompt: true, Save: true}

// User is one account.
type User struct {
	// Email is the login name, always lowercase.
	Email string `json:"email"`
	// Hash is the bcrypt password hash; empty until the account holder
	// has chosen a password through their invite link.
	Hash string `json:"password_hash"`
	// Perms are the account's capabilities.
	Perms   Permissions `json:"permissions"`
	Created time.Time   `json:"created"`
	Updated time.Time   `json:"updated"`
	// Active reports whether a password has been set (derived; never
	// stored).
	Active bool `json:"-"`
}

// LinkKind distinguishes the two single-use links.
type LinkKind string

// Link kinds.
const (
	// LinkInvite activates a new account: the invitee picks a password.
	LinkInvite LinkKind = "invite"
	// LinkReset lets an existing account holder pick a new password.
	LinkReset LinkKind = "reset"
)

// Link lifetimes.
const (
	InviteTTL = 7 * 24 * time.Hour
	ResetTTL  = 24 * time.Hour
)

// Link is a pending single-use invite or reset. Only the SHA-256 of the
// secret is stored; the secret itself exists only in the URL handed out.
type Link struct {
	Email   string    `json:"email"`
	Kind    LinkKind  `json:"kind"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// Errors returned by the store.
var (
	ErrNotFound     = errors.New("no account with that email")
	ErrExists       = errors.New("an account with that email already exists")
	ErrInvalidEmail = errors.New("that is not a valid email address")
	ErrWeakPassword = errors.New("the password must be at least 8 characters")
	ErrSelfDelete   = errors.New("you cannot delete your own account")
	ErrLastAdmin    = errors.New("that would remove the last admin")
	ErrLinkInvalid  = errors.New("that link is invalid, already used, or expired")
)

// MinPasswordLength is the shortest password accepted.
const MinPasswordLength = 8

// Cost is the bcrypt work factor; tests lower it.
var Cost = bcrypt.DefaultCost

// emailRe is deliberately simple: one @, no whitespace, a dot in the
// domain. Deliverability is the mail server's problem.
var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// NormalizeEmail trims, lowercases and validates an email address.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 254 || !emailRe.MatchString(s) {
		return "", ErrInvalidEmail
	}
	return s, nil
}

// usersFile is the on-disk shape.
type usersFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
	Links   []Link `json:"links,omitempty"`
}

// Store is the users file. It is safe for concurrent use and re-reads
// the file when another process changed it (for example `iar remote
// setup` run while the player is serving).
type Store struct {
	path string

	mu    sync.Mutex
	users []User // sorted by email
	links []Link
	seen  fileStamp
	now   func() time.Time
}

// NewStore returns a store backed by the users file at path (created on
// the first write).
func NewStore(path string) *Store {
	return &Store{path: path, now: time.Now}
}

// Path returns the users file location.
func (s *Store) Path() string { return s.path }

// reload re-reads the file when it changed on disk. Callers hold s.mu.
func (s *Store) reload() error {
	if !s.seen.changed(s.path) {
		return nil
	}
	var f usersFile
	st, err := readJSON(s.path, &f)
	if err != nil {
		return fmt.Errorf("reading users file: %w", err)
	}
	s.users = f.Users
	s.links = f.Links
	s.sortLocked()
	s.seen = st
	return nil
}

// persist writes the users file atomically, dropping expired links.
// Callers hold s.mu.
func (s *Store) persist() error {
	s.sortLocked()
	now := s.now()
	kept := s.links[:0]
	for _, l := range s.links {
		if l.Expires.After(now) {
			kept = append(kept, l)
		}
	}
	s.links = kept
	st, err := writeJSON(s.path, usersFile{Version: 1, Users: s.users, Links: s.links})
	if err != nil {
		return fmt.Errorf("writing users file: %w", err)
	}
	s.seen = st
	return nil
}

func (s *Store) sortLocked() {
	sort.Slice(s.users, func(i, j int) bool { return s.users[i].Email < s.users[j].Email })
	sort.Slice(s.links, func(i, j int) bool { return s.links[i].Email < s.links[j].Email })
}

func (s *Store) indexLocked(email string) int {
	for i := range s.users {
		if s.users[i].Email == email {
			return i
		}
	}
	return -1
}

func (s *Store) adminCountLocked() int {
	n := 0
	for _, u := range s.users {
		if u.Perms.Admin {
			n++
		}
	}
	return n
}

// dropLinksLocked removes every pending link for email.
func (s *Store) dropLinksLocked(email string) {
	kept := s.links[:0]
	for _, l := range s.links {
		if l.Email != email {
			kept = append(kept, l)
		}
	}
	s.links = kept
}

// public returns a copy safe to hand out: no hash, Active derived.
func public(u User) User {
	u.Active = u.Hash != ""
	u.Hash = ""
	return u
}

// Count returns how many accounts exist.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return 0
	}
	return len(s.users)
}

// List returns every account sorted by email, without password hashes.
func (s *Store) List() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil
	}
	out := make([]User, len(s.users))
	for i, u := range s.users {
		out[i] = public(u)
	}
	return out
}

// Get returns one account (without its password hash).
func (s *Store) Get(email string) (User, bool) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return User{}, false
	}
	i := s.indexLocked(email)
	if i < 0 {
		return User{}, false
	}
	return public(s.users[i]), true
}

// hashPassword hashes a password after checking the policy.
func hashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), Cost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// Create adds an inactive account: it cannot log in until its holder
// sets a password through an invite link (see IssueLink).
func (s *Store) Create(email string, perms Permissions) (User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return User{}, err
	}
	if s.indexLocked(email) >= 0 {
		return User{}, ErrExists
	}
	now := s.now()
	u := User{Email: email, Perms: perms, Created: now, Updated: now}
	s.users = append(s.users, u)
	if err := s.persist(); err != nil {
		return User{}, err
	}
	return public(u), nil
}

// EnsureAdmin creates email as an admin with every permission and the
// given password, or, when the account exists, resets its password and
// grants it every permission. It is the terminal-side bootstrap and the
// lockout recovery. The boolean reports whether the account was newly
// created.
func (s *Store) EnsureAdmin(email, password string) (bool, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return false, err
	}
	now := s.now()
	i := s.indexLocked(email)
	created := i < 0
	if created {
		s.users = append(s.users, User{Email: email, Hash: hash, Perms: AllPermissions, Created: now, Updated: now})
	} else {
		s.users[i].Hash = hash
		s.users[i].Perms = AllPermissions
		s.users[i].Updated = now
	}
	s.dropLinksLocked(email)
	return created, s.persist()
}

// SetPermissions replaces an account's permissions. Removing admin from
// the last admin is refused.
func (s *Store) SetPermissions(email string, perms Permissions) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	i := s.indexLocked(email)
	if i < 0 {
		return ErrNotFound
	}
	if s.users[i].Perms.Admin && !perms.Admin && s.adminCountLocked() == 1 {
		return ErrLastAdmin
	}
	s.users[i].Perms = perms
	s.users[i].Updated = s.now()
	return s.persist()
}

// Delete removes an account and its pending links. An account cannot
// delete itself (actor is the email of whoever asks), and the last admin
// cannot be deleted.
func (s *Store) Delete(actor, email string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if a, err := NormalizeEmail(actor); err == nil && a == email {
		return ErrSelfDelete
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	i := s.indexLocked(email)
	if i < 0 {
		return ErrNotFound
	}
	if s.users[i].Perms.Admin && s.adminCountLocked() == 1 {
		return ErrLastAdmin
	}
	s.users = append(s.users[:i], s.users[i+1:]...)
	s.dropLinksLocked(email)
	return s.persist()
}

// SetPassword replaces an account's password (the account page). Any
// pending reset link for the account is dropped.
func (s *Store) SetPassword(email, password string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	i := s.indexLocked(email)
	if i < 0 {
		return ErrNotFound
	}
	s.users[i].Hash = hash
	s.users[i].Updated = s.now()
	s.dropLinksLocked(email)
	return s.persist()
}

// dummyHash is compared against when the email is unknown or the
// account has no password yet, so a login attempt costs the same either
// way and reveals nothing.
var dummyHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("no such account"), bcrypt.MinCost)
	return h
}()

// Verify checks a password. It returns the account (without hash) on
// success; on failure it reveals nothing about whether the email exists
// or is activated, and takes the same time either way.
func (s *Store) Verify(email, password string) (User, bool) {
	norm, err := NormalizeEmail(email)
	s.mu.Lock()
	var u User
	found := false
	if err == nil && s.reload() == nil {
		if i := s.indexLocked(norm); i >= 0 && s.users[i].Hash != "" {
			u = s.users[i]
			found = true
		}
	}
	s.mu.Unlock()
	hash := dummyHash
	if found {
		hash = []byte(u.Hash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !found {
		return User{}, false
	}
	return public(u), true
}

// hashSecret is how link secrets are stored: a one-way hash, so the
// file never contains a usable link and lookups are plain map-style
// matches on the hash rather than comparisons of the secret.
func hashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newSecret returns 256 random bits as a URL-safe string.
func newSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// IssueLink creates a single-use link for an account, replacing any
// pending one: an invite (7 days) while the account has no password
// yet, a reset (24 hours) otherwise. It returns the secret to embed in
// the URL and the link's public description.
func (s *Store) IssueLink(email string) (string, Link, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return "", Link{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return "", Link{}, err
	}
	i := s.indexLocked(email)
	if i < 0 {
		return "", Link{}, ErrNotFound
	}
	secret, err := newSecret()
	if err != nil {
		return "", Link{}, err
	}
	now := s.now()
	l := Link{Email: email, Kind: LinkInvite, Hash: hashSecret(secret), Created: now, Expires: now.Add(InviteTTL)}
	if s.users[i].Hash != "" {
		l.Kind = LinkReset
		l.Expires = now.Add(ResetTTL)
	}
	s.dropLinksLocked(email)
	s.links = append(s.links, l)
	if err := s.persist(); err != nil {
		return "", Link{}, err
	}
	l.Hash = ""
	return secret, l, nil
}

// Links lists pending (unexpired) links without their hashes, sorted by
// email.
func (s *Store) Links() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil
	}
	now := s.now()
	var out []Link
	for _, l := range s.links {
		if l.Expires.After(now) {
			l.Hash = ""
			out = append(out, l)
		}
	}
	return out
}

// LookupLink resolves a link secret to its pending link (hash omitted).
func (s *Store) LookupLink(secret string) (Link, bool) {
	if secret == "" {
		return Link{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return Link{}, false
	}
	h := hashSecret(secret)
	now := s.now()
	for _, l := range s.links {
		if l.Hash == h && l.Expires.After(now) {
			l.Hash = ""
			return l, true
		}
	}
	return Link{}, false
}

// Redeem consumes a link: the account's password becomes password, the
// link is destroyed, and the (now active) account is returned so the
// caller can log it in.
func (s *Store) Redeem(secret, password string) (User, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return User{}, err
	}
	h := hashSecret(secret)
	now := s.now()
	for _, l := range s.links {
		if l.Hash != h || !l.Expires.After(now) {
			continue
		}
		i := s.indexLocked(l.Email)
		if i < 0 {
			break
		}
		s.users[i].Hash = hash
		s.users[i].Updated = now
		s.dropLinksLocked(l.Email)
		if err := s.persist(); err != nil {
			return User{}, err
		}
		return public(s.users[i]), nil
	}
	return User{}, ErrLinkInvalid
}

// RevokeLink drops an account's pending link.
func (s *Store) RevokeLink(email string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	s.dropLinksLocked(email)
	return s.persist()
}
