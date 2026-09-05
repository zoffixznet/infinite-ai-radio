package session

import (
	"testing"
	"time"
)

// A branch's name has to say where it came from and stay a name: chain
// enough changes together and a name built by appending would grow
// without end.
func TestForkNameSaysWhereItCameFromWithoutGrowing(t *testing.T) {
	now := time.Date(2026, 9, 5, 4, 15, 0, 0, time.UTC)
	stamp := "20260905-041500"
	for _, tc := range []struct{ from, want string }{
		{"gym-grind", "gym-grind-" + stamp},
		{"session-20260101-090000", "session-" + stamp},
		{"prompt-dark-techno-090000", "prompt-dark-techno-" + stamp},
		{"gym-grind-20260101-090000", "gym-grind-" + stamp},
		{"", "session-" + stamp},
	} {
		if got := ForkName(tc.from, now); got != tc.want {
			t.Errorf("ForkName(%q) = %q, want %q", tc.from, got, tc.want)
		}
	}
	// The result is a generated name, so the bulk delete offers it.
	s := &Session{Name: ForkName("gym-grind", now)}
	if !s.AutoNamed() {
		t.Fatalf("%q does not read as automatic", s.Name)
	}
}

// The bulk delete takes the sessions nobody named, keeps the one
// playing, and can be narrowed to the ones that have gone quiet.
func TestDeleteAutoKeepsWhatMatters(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Now()
	mk := func(name string, named bool, played time.Time) {
		s := New()
		s.Name = name
		s.Named = named
		s.LastPlayed = played
		if err := st.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	mk("gym-grind", true, now.Add(-90*24*time.Hour)) // named: never touched
	mk("session-20260101-000000", false, now.Add(-30*24*time.Hour))
	mk("session-20260102-000000", false, now) // played today
	mk("session-20260103-000000", false, now) // the one playing
	mk("prompt-dark-techno-090000", false, now.Add(-30*24*time.Hour))

	removed, err := st.DeleteAuto(now, 7*24*time.Hour, "session-20260103-000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("a 7-day window removed %v", removed)
	}
	removed, err = st.DeleteAuto(now, 0, "session-20260103-000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "session-20260102-000000" {
		t.Fatalf("the full sweep removed %v", removed)
	}
	left, _ := st.List()
	if len(left) != 2 {
		t.Fatalf("%d sessions left; the named one and the playing one must stay", len(left))
	}
	if !st.Exists("gym-grind") || !st.Exists("session-20260103-000000") {
		t.Fatal("the named or the playing session was deleted")
	}
}
