package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func init() { Cost = bcrypt.MinCost }

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "remote", "users.json"))
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"  Jane.Doe@Example.COM ", "jane.doe@example.com", true},
		{"a@b.co", "a@b.co", true},
		{"no-at-sign", "", false},
		{"two@@example.com", "", false},
		{"spaces in@example.com", "", false},
		{"@example.com", "", false},
		{"jane@localhost", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := NormalizeEmail(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestEnsureAdminVerifyAndFileMode(t *testing.T) {
	s := newStore(t)
	created, err := s.EnsureAdmin("Owner@Example.com", "correct horse")
	if err != nil || !created {
		t.Fatalf("EnsureAdmin = %v, %v", created, err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("users file mode = %o, want 0600", fi.Mode().Perm())
	}
	// The file holds a hash, never the password.
	raw, _ := os.ReadFile(s.Path())
	if strings.Contains(string(raw), "correct horse") || !strings.Contains(string(raw), "$2a$") {
		t.Fatalf("users file content unexpected: %s", raw)
	}

	got, ok := s.Verify("OWNER@example.com", "correct horse")
	if !ok || got.Email != "owner@example.com" || got.Hash != "" || !got.Active || got.Perms != AllPermissions {
		t.Fatalf("verify = %+v, %v", got, ok)
	}
	if _, ok := s.Verify("owner@example.com", "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	if _, ok := s.Verify("nobody@example.com", "correct horse"); ok {
		t.Fatal("unknown account accepted")
	}
	if _, ok := s.Verify("not-an-email", "correct horse"); ok {
		t.Fatal("invalid email accepted")
	}
	if _, err := s.EnsureAdmin("short@example.com", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password err = %v", err)
	}
	if _, err := s.EnsureAdmin("bad", "long enough"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("bad email err = %v", err)
	}
}

func TestEnsureAdminResetsExistingAccount(t *testing.T) {
	s := newStore(t)
	s.EnsureAdmin("owner@example.com", "first-pass")
	s.EnsureAdmin("second@example.com", "password1")
	// Demote the owner, then EnsureAdmin again: password reset, admin
	// restored, nothing created.
	if err := s.SetPermissions("owner@example.com", Permissions{}); err != nil {
		t.Fatal(err)
	}
	created, err := s.EnsureAdmin("OWNER@example.com", "second-pass")
	if err != nil || created {
		t.Fatalf("EnsureAdmin second = %v, %v", created, err)
	}
	if _, ok := s.Verify("owner@example.com", "first-pass"); ok {
		t.Fatal("old password still works")
	}
	u, ok := s.Verify("owner@example.com", "second-pass")
	if !ok || u.Perms != AllPermissions {
		t.Fatalf("reset user = %+v, %v", u, ok)
	}
	if s.Count() != 2 {
		t.Fatalf("count = %d", s.Count())
	}
}

func TestInviteFlow(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.EnsureAdmin("owner@example.com", "password1")

	u, err := s.Create("Jane@Example.com", Permissions{Steer: true})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "jane@example.com" || u.Active || u.Hash != "" {
		t.Fatalf("created user = %+v", u)
	}
	// An inactive account cannot log in with anything.
	if _, ok := s.Verify("jane@example.com", ""); ok {
		t.Fatal("inactive account logged in with an empty password")
	}
	if _, err := s.Create("jane@example.com", Permissions{}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create err = %v", err)
	}
	if _, err := s.Create("bad", Permissions{}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("bad email err = %v", err)
	}

	secret, link, err := s.IssueLink("jane@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if link.Kind != LinkInvite || link.Hash != "" || !link.Expires.Equal(now.Add(InviteTTL)) {
		t.Fatalf("invite link = %+v", link)
	}
	if len(secret) < 40 {
		t.Fatalf("secret too short: %q", secret)
	}
	raw, _ := os.ReadFile(s.Path())
	if strings.Contains(string(raw), secret) {
		t.Fatal("users file stores the raw link secret")
	}
	if got, ok := s.LookupLink(secret); !ok || got.Email != "jane@example.com" || got.Kind != LinkInvite {
		t.Fatalf("lookup = %+v, %v", got, ok)
	}
	if _, ok := s.LookupLink("bogus"); ok {
		t.Fatal("bogus secret resolved")
	}
	if links := s.Links(); len(links) != 1 || links[0].Email != "jane@example.com" || links[0].Hash != "" {
		t.Fatalf("pending links = %+v", links)
	}

	// Redeem: password policy applies; success activates and burns the
	// link.
	if _, err := s.Redeem(secret, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak redeem err = %v", err)
	}
	got, err := s.Redeem(secret, "jane-chooses-this")
	if err != nil || got.Email != "jane@example.com" || !got.Active || !got.Perms.Steer {
		t.Fatalf("redeem = %+v, %v", got, err)
	}
	if _, err := s.Redeem(secret, "jane-chooses-this"); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("second redeem err = %v (link must be single-use)", err)
	}
	if _, ok := s.LookupLink(secret); ok {
		t.Fatal("used link still resolves")
	}
	if len(s.Links()) != 0 {
		t.Fatalf("pending links after redeem = %+v", s.Links())
	}
	if _, ok := s.Verify("jane@example.com", "jane-chooses-this"); !ok {
		t.Fatal("activated account cannot log in")
	}
}

func TestResetLinkExpiryRegenerateRevoke(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.EnsureAdmin("owner@example.com", "password1")

	// An active account gets a RESET link with the shorter lifetime.
	secret1, link, err := s.IssueLink("owner@example.com")
	if err != nil || link.Kind != LinkReset || !link.Expires.Equal(now.Add(ResetTTL)) {
		t.Fatalf("reset link = %+v, %v", link, err)
	}
	// Regenerating invalidates the old secret.
	secret2, _, err := s.IssueLink("owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupLink(secret1); ok {
		t.Fatal("old secret survives regeneration")
	}
	if len(s.Links()) != 1 {
		t.Fatalf("links after regenerate = %+v", s.Links())
	}
	// Expiry.
	now = now.Add(ResetTTL + time.Minute)
	if _, ok := s.LookupLink(secret2); ok {
		t.Fatal("expired link resolves")
	}
	if _, err := s.Redeem(secret2, "new-password"); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("expired redeem err = %v", err)
	}
	if len(s.Links()) != 0 {
		t.Fatal("expired link still listed")
	}
	// Revoke.
	secret3, _, _ := s.IssueLink("owner@example.com")
	if err := s.RevokeLink("owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupLink(secret3); ok {
		t.Fatal("revoked link resolves")
	}
	// A self-service password change drops a pending reset link.
	secret4, _, _ := s.IssueLink("owner@example.com")
	if err := s.SetPassword("owner@example.com", "password2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupLink(secret4); ok {
		t.Fatal("reset link outlived a password change")
	}
	if _, ok := s.Verify("owner@example.com", "password2"); !ok {
		t.Fatal("new password rejected")
	}
	if _, _, err := s.IssueLink("ghost@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue for unknown err = %v", err)
	}
}

func TestGuardsLastAdminAndSelfDelete(t *testing.T) {
	s := newStore(t)
	s.EnsureAdmin("admin@example.com", "password1")
	s.Create("listener@example.com", Permissions{})
	secret, _, _ := s.IssueLink("listener@example.com")
	// The only admin cannot lose admin or be deleted.
	if err := s.SetPermissions("admin@example.com", Permissions{Steer: true}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin err = %v", err)
	}
	if err := s.Delete("listener@example.com", "admin@example.com"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("delete last admin err = %v", err)
	}
	// Nobody deletes themselves.
	if err := s.Delete("admin@example.com", "Admin@Example.com"); !errors.Is(err, ErrSelfDelete) {
		t.Fatalf("self delete err = %v", err)
	}
	// With a second admin, the first can step down and be deleted.
	if err := s.SetPermissions("listener@example.com", Permissions{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPermissions("admin@example.com", Permissions{Save: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("listener@example.com", "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 1 {
		t.Fatalf("count after delete = %d", s.Count())
	}
	if err := s.Delete("listener@example.com", "ghost@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown err = %v", err)
	}
	// Admin does not imply the other permissions.
	u, _ := s.Get("listener@example.com")
	if !u.Perms.Admin || u.Perms.Steer || u.Perms.NewPrompt || u.Perms.Save {
		t.Fatalf("admin perms = %+v", u.Perms)
	}
	// Deleting an account drops its pending link.
	s.Create("temp@example.com", Permissions{})
	secretTemp, _, _ := s.IssueLink("temp@example.com")
	if err := s.Delete("listener@example.com", "temp@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupLink(secretTemp); ok {
		t.Fatal("deleted account's link resolves")
	}
	// Promotion does not touch a pending invite: the listener still has
	// no password, so their link must keep working.
	if u, err := s.Redeem(secret, "whatever-pass"); err != nil || !u.Perms.Admin || !u.Active {
		t.Fatalf("invite after promotion: %+v, %v", u, err)
	}
}

func TestListOmitsHashesAndReportsActive(t *testing.T) {
	s := newStore(t)
	s.EnsureAdmin("b@example.com", "password1")
	s.Create("a@example.com", Permissions{})
	list := s.List()
	if len(list) != 2 || list[0].Email != "a@example.com" || list[1].Email != "b@example.com" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Active || !list[1].Active {
		t.Fatalf("active flags = %v %v", list[0].Active, list[1].Active)
	}
	for _, u := range list {
		if u.Hash != "" {
			t.Fatal("List leaked a password hash")
		}
	}
	if err := s.SetPassword("zz@example.com", "password2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set unknown err = %v", err)
	}
}

func TestStoreReloadsWhenAnotherProcessWrites(t *testing.T) {
	s := newStore(t)
	if s.Count() != 0 {
		t.Fatal("fresh store not empty")
	}
	// Simulate `iar remote setup` writing the file from another process.
	other := NewStore(s.Path())
	if _, err := other.EnsureAdmin("owner@example.com", "password1"); err != nil {
		t.Fatal(err)
	}
	// Ensure a distinguishable mtime even on coarse filesystems.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(s.Path(), future, future)
	if s.Count() != 1 {
		t.Fatalf("store did not pick up the external write: count=%d", s.Count())
	}
	if _, ok := s.Verify("owner@example.com", "password1"); !ok {
		t.Fatal("externally created account cannot log in")
	}
}

func TestCorruptUsersFileIsAnError(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.Path()), 0o700)
	os.WriteFile(s.Path(), []byte("{not json"), 0o600)
	if _, err := s.Create("a@example.com", Permissions{}); err == nil {
		t.Fatal("corrupt file silently overwritten")
	}
}

// --- sessions ---

func TestSessionsRoundTripPersistAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s := NewSessions(path, 48*time.Hour)
	s.now = func() time.Time { return now }

	id, err := s.Create("jane@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) < 40 {
		t.Fatalf("session id too short: %q", id)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("sessions file mode = %o", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), id) {
		t.Fatal("sessions file stores the raw session id")
	}
	if sess, ok := s.Lookup(id); !ok || sess.Email != "jane@example.com" {
		t.Fatalf("lookup = %+v, %v", sess, ok)
	}
	if _, ok := s.Lookup("bogus"); ok {
		t.Fatal("bogus id accepted")
	}

	// A restart (fresh store over the same file) keeps the session.
	s2 := NewSessions(path, 48*time.Hour)
	s2.now = func() time.Time { return now }
	if _, ok := s2.Lookup(id); !ok {
		t.Fatal("session lost across restart")
	}

	// Use slides the expiry: 70h after login it is still valid because
	// a lookup at 30h renewed it.
	s2.now = func() time.Time { return now.Add(30 * time.Hour) }
	if _, ok := s2.Lookup(id); !ok {
		t.Fatal("session expired early")
	}
	s2.now = func() time.Time { return now.Add(70 * time.Hour) }
	if _, ok := s2.Lookup(id); !ok {
		t.Fatal("renewed session not honored")
	}
	// Without use, it expires.
	s2.now = func() time.Time { return now.Add(200 * time.Hour) }
	if _, ok := s2.Lookup(id); ok {
		t.Fatal("expired session accepted")
	}
}

func TestSessionsRevoke(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "sessions.json"), time.Hour)
	a1, _ := s.Create("a@example.com")
	a2, _ := s.Create("a@example.com")
	b1, _ := s.Create("b@example.com")
	if s.Count() != 3 {
		t.Fatalf("count = %d", s.Count())
	}
	if err := s.Revoke(a1); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(a1); ok {
		t.Fatal("revoked session still valid")
	}
	// Revoke all of a's sessions except a2 (password change keeps the
	// current browser logged in).
	a3, _ := s.Create("a@example.com")
	if err := s.RevokeUser("a@example.com", a2); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(a3); ok {
		t.Fatal("other session of the user survived RevokeUser")
	}
	if _, ok := s.Lookup(a2); !ok {
		t.Fatal("kept session was revoked")
	}
	if _, ok := s.Lookup(b1); !ok {
		t.Fatal("another user's session was revoked")
	}
	if err := s.RevokeUser("a@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(a2); ok {
		t.Fatal("RevokeUser with no keep left a session")
	}
}

func TestSessionsFileShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s := NewSessions(path, time.Hour)
	s.Create("a@example.com")
	raw, _ := os.ReadFile(path)
	var f sessionsFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Sessions) != 1 {
		t.Fatalf("sessions file = %s (%v)", raw, err)
	}
}

// --- rate limiting ---

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(3, 10*time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allowed("ip"); !ok {
			t.Fatalf("attempt %d refused early", i)
		}
		l.Fail("ip")
	}
	ok, wait := l.Allowed("ip")
	if ok || wait <= 0 || wait > 10*time.Minute {
		t.Fatalf("after 3 failures: allowed=%v wait=%v", ok, wait)
	}
	// Other keys are independent.
	if ok, _ := l.Allowed("other"); !ok {
		t.Fatal("unrelated key throttled")
	}
	// The window slides: once the oldest failure ages out, one more
	// attempt is allowed.
	now = now.Add(10*time.Minute + time.Second)
	if ok, _ := l.Allowed("ip"); !ok {
		t.Fatal("failure did not age out")
	}
	// Success clears the key.
	l.Fail("ip")
	l.Fail("ip")
	l.Fail("ip")
	l.Reset("ip")
	if ok, _ := l.Allowed("ip"); !ok {
		t.Fatal("reset did not clear the key")
	}
}
