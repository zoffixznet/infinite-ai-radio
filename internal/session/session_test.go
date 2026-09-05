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

// Sessions used to record which of the configured languages were
// switched OFF, so a language the machine gained later was silently
// sung by every session that had never heard of it. They now carry the
// list they sing, and an old file is read once against the catalogue it
// was saved under.
func TestOldLanguageMapBecomesTheListItSings(t *testing.T) {
	// The shape on disk today: Tagalog unlisted (so, on), the rest off.
	s := &Session{Languages: map[string]bool{
		"Russian": false, "English": false, "French": false, "Bisaya (Cebuano)": false,
	}}
	s.AdoptLanguages([]string{"Russian", "Tagalog", "Bisaya (Cebuano)", "English", "French"})
	if len(s.SungLanguages) != 1 || s.SungLanguages[0] != "Tagalog" {
		t.Fatalf("adopted %v, want just Tagalog", s.SungLanguages)
	}
	if s.Languages != nil {
		t.Fatal("the old map must be dropped once it is read")
	}

	// Every configured language switched off meant the engine's own
	// choice, which is an empty list.
	off := &Session{Languages: map[string]bool{"English": false}}
	off.AdoptLanguages([]string{"English"})
	if len(off.SungLanguages) != 0 {
		t.Fatalf("all-off adopted %v", off.SungLanguages)
	}

	// A session that already carries a list is left alone.
	kept := &Session{SungLanguages: []string{"Japanese"}, Languages: map[string]bool{"English": false}}
	kept.AdoptLanguages([]string{"English", "Japanese"})
	if len(kept.SungLanguages) != 1 || kept.SungLanguages[0] != "Japanese" {
		t.Fatalf("an existing list was rewritten: %v", kept.SungLanguages)
	}
}

// The list is the session's own, so switching is by name and a
// snapshot does not share it.
func TestSungLanguageSwitching(t *testing.T) {
	s := New()
	s.SetSung("Tagalog", true)
	s.SetSung("tagalog", true) // same language, said differently
	if len(s.SungLanguages) != 1 || !s.Sings("TAGALOG") {
		t.Fatalf("switching on twice = %v", s.SungLanguages)
	}
	cp := s.Snapshot()
	cp.SetSung("Russian", true)
	if s.Sings("Russian") {
		t.Fatal("a snapshot shares the caller's list")
	}
	s.SetSung("Tagalog", false)
	if len(s.SungLanguages) != 0 || s.Sings("Tagalog") {
		t.Fatalf("switching off left %v", s.SungLanguages)
	}
}
