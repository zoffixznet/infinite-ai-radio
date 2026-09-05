package player

import (
	"strings"
	"testing"
	"time"

	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
)

// heardSession is a session that has played before, which is what makes
// changing it worth keeping the old sound for.
func heardSession(name string) *session.Session {
	s := session.New()
	s.Name = name
	s.Named = true
	s.LastPlayed = time.Now().Add(-time.Hour)
	return s
}

// The sound a listener settled on used to be overwritten by the next
// thing they typed. Now the change branches: the old sound keeps its
// name and stays on disk, and what is playing carries on under a
// generated name, so "go back and try again" is a session load.
func TestSteeringBranchesTheSessionItChanges(t *testing.T) {
	store := session.NewStore(t.TempDir())
	o, _ := newTestOrchestratorWithStore(t, enginetest.NewMock(), heardSession("road-trip"), store)
	waitFor(t, 10*time.Second, "a track playing", func() bool { return o.Status().TrackID != "" })

	ack := o.Steer("calmer")
	if !strings.Contains(ack, "kept as road-trip") {
		t.Fatalf("steer ack does not say where the old sound went: %q", ack)
	}
	name := o.CurrentName()
	if name == "road-trip" {
		t.Fatal("steering wrote over the session it was given")
	}
	if !strings.HasPrefix(name, "road-trip-") {
		t.Fatalf("the branch does not say what it came from: %q", name)
	}
	// A branch is a session nobody named, whatever the clock did.
	if fork, err := store.Load(name); err != nil || !fork.AutoNamed() {
		t.Fatalf("the branch does not read as automatic: %+v (%v)", fork, err)
	}

	// The old name still holds the sound from before the steer.
	old, err := store.Load("road-trip")
	if err != nil {
		t.Fatalf("the previous session is gone: %v", err)
	}
	if len(old.Tweaks) != 0 {
		t.Fatalf("the previous session carries the change: %+v", old.Tweaks)
	}
	// ...and the branch carries it, under a generated name that the
	// bulk delete will offer to clear.
	fork, err := store.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(fork.Tweaks) == 0 || fork.ForkedFrom != "road-trip" || !fork.AutoNamed() {
		t.Fatalf("branch: %+v", fork)
	}
}

// Changing the sound twice before anything has been heard leaves one
// session, not three: a state nobody heard is not one anyone wants back.
func TestChangesBeforeAnythingIsHeardDoNotBranch(t *testing.T) {
	store := session.NewStore(t.TempDir())
	b := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), enginetest.NewMock(), b, store, session.New(), &capturePlayer{}, testLogger())
	first := o.CurrentName()

	o.Steer("calmer")
	o.Steer("brighter")
	if o.CurrentName() != first {
		t.Fatalf("an unheard session was branched: %q -> %q", first, o.CurrentName())
	}
	all, _ := store.List()
	if len(all) != 1 {
		t.Fatalf("%d sessions on disk; unheard states are not worth keeping", len(all))
	}
}

// Language and lyric-writer changes are changes to what is generated,
// so they branch too.
func TestLanguageAndWriterChangesBranch(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := heardSession("gym-grind")
	sess.Vocal = true
	o, _ := newTestOrchestratorWithStore(t, enginetest.NewMock(), sess, store)
	waitFor(t, 10*time.Second, "a track playing", func() bool { return o.Status().TrackID != "" })

	langs := o.Languages()
	if len(langs) == 0 {
		t.Skip("no vocal languages configured in the test build")
	}
	o.SetLanguage(langs[0].Name, !langs[0].On)
	afterLang := o.CurrentName()
	if afterLang == "gym-grind" {
		t.Fatal("a language switch wrote over the session")
	}
	if _, err := store.Load("gym-grind"); err != nil {
		t.Fatalf("the previous session is gone: %v", err)
	}

	// Nothing has been heard from the branch yet, so the next change
	// stays in it.
	o.LyricsGen("smoothbrain")
	if o.CurrentName() != afterLang {
		t.Fatalf("an unheard branch was branched again: %q -> %q", afterLang, o.CurrentName())
	}
}

// The generated names accumulate one per change, so there is one
// gesture that clears them - and it never takes the one playing or one
// the listener named.
func TestDeletingAutomaticSessions(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	old := session.New()
	old.Name = "session-20200101-000000"
	old.LastPlayed = time.Now().Add(-30 * 24 * time.Hour)
	recent := session.New()
	recent.Name = "session-20200102-000000"
	recent.LastPlayed = time.Now()
	named := heardSession("gym-grind")
	for _, s := range []*session.Session{old, recent, named} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	playing := heardSession("road-trip")
	playing.Named = false
	playing.Name = "session-20200103-000000"
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, playing, &capturePlayer{}, testLogger())
	if err := store.Save(playing); err != nil {
		t.Fatal(err)
	}

	// A window keeps the recent ones.
	if ack := o.DeleteAutoSessions(7); !strings.Contains(ack, "deleted 1") {
		t.Fatalf("windowed delete ack = %q", ack)
	}
	if _, err := store.Load("session-20200102-000000"); err != nil {
		t.Fatalf("a session played today was deleted by a 7-day window: %v", err)
	}

	// Without one, every generated name goes - except what is playing.
	if ack := o.DeleteAutoSessions(0); !strings.Contains(ack, "deleted 1") {
		t.Fatalf("full delete ack = %q", ack)
	}
	left, _ := store.List()
	names := map[string]bool{}
	for _, s := range left {
		names[s.Name] = true
	}
	if !names["gym-grind"] || !names[playing.Name] || len(left) != 2 {
		t.Fatalf("wrong sessions left: %v", names)
	}
	if ack := o.DeleteAutoSessions(0); !strings.Contains(ack, "no automatic sessions") {
		t.Fatalf("second delete ack = %q", ack)
	}
}

// Which languages get sung is a standing preference about the radio,
// not a property of one vibe: starting a preset used to lose it, and
// the songs came back in a language the listener had switched off.
func TestSwitchedOffLanguagesSurviveASessionSwitch(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := heardSession("gym-grind")
	sess.Vocal = true
	o, _ := newTestOrchestratorWithStore(t, enginetest.NewMock(), sess, store)

	langs := o.Languages()
	if len(langs) < 2 {
		t.Skip("needs at least two configured vocal languages")
	}
	off := langs[0].Name
	o.SetLanguage(off, false)

	for _, switchTo := range []func(){
		func() { o.LoadPreset("pink-noise") },
		func() { o.NewSession("dark techno with vocals") },
	} {
		switchTo()
		for _, l := range o.Languages() {
			if l.Name == off && l.On {
				t.Fatalf("%s came back on after switching sessions", off)
			}
		}
	}
}
