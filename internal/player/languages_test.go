package player

import (
	"strings"
	"testing"

	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
)

// The point of the whole arrangement: two saved sessions, each with its
// own languages, and switching between them gives you each one's music.
// The shape before this kept the switches beside the machine's list
// instead of in the session, so loading the other one sang the wrong
// language until you noticed and fixed it by hand.
func TestLoadingASessionSingsWhatItWasSavedWith(t *testing.T) {
	store := session.NewStore(t.TempDir())
	b := prompting.NewBuilder(nil, testLogger())
	b.SetLanguages([]string{"Tagalog", "Russian", "English"})

	tagalog := heardSession("tagalog-nu-metal")
	tagalog.Vocal = true
	tagalog.SungLanguages = []string{"Tagalog"}
	russian := heardSession("russian-dance-music")
	russian.Vocal = true
	russian.SungLanguages = []string{"Russian"}
	for _, s := range []*session.Session{tagalog, russian} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}

	o := New(testConfig(), enginetest.NewMock(), b, store, session.New(), &capturePlayer{}, testLogger())
	sung := func() []string {
		var on []string
		for _, l := range o.Languages() {
			if l.On {
				on = append(on, l.Name)
			}
		}
		return on
	}
	for i := 0; i < 2; i++ {
		o.LoadSession("tagalog-nu-metal")
		if got := sung(); len(got) != 1 || got[0] != "Tagalog" {
			t.Fatalf("round %d: tagalog-nu-metal sings %v", i, got)
		}
		o.LoadSession("russian-dance-music")
		if got := sung(); len(got) != 1 || got[0] != "Russian" {
			t.Fatalf("round %d: russian-dance-music sings %v", i, got)
		}
	}
}

// A preset that names a language starts its session singing in it, so
// the pills say what the songs will be; one that names none leaves the
// choice to the engine, whatever the machine's list holds.
func TestAPresetStartsWithItsOwnLanguage(t *testing.T) {
	b := prompting.NewBuilder(nil, testLogger())
	b.SetLanguages([]string{"Tagalog", "English"})
	o := New(testConfig(), enginetest.NewMock(), b, session.NewStore(t.TempDir()),
		session.New(), &capturePlayer{}, testLogger())

	// nu-metal declares English of its own.
	o.LoadPreset("nu-metal")
	var on []string
	for _, l := range o.Languages() {
		if l.On {
			on = append(on, l.Name)
		}
	}
	if len(on) != 1 || on[0] != "English" {
		t.Fatalf("nu-metal sings %v; the preset names English", on)
	}

	// pink-noise names none - and is not sung at all.
	o.LoadPreset("pink-noise")
	for _, l := range o.Languages() {
		if l.On {
			t.Fatalf("a preset with no language of its own switched %s on", l.Name)
		}
	}
}

// A language taken out of the machine's list keeps being sung by a
// session that names it - that session was saved that way - and says it
// is no longer configured, which is what lets a listener switch it off.
// Switching it off is a change to the sound like any other, so it
// branches the session.
func TestALanguageNoLongerConfiguredStaysSungUntilSwitchedOff(t *testing.T) {
	store := session.NewStore(t.TempDir())
	b := prompting.NewBuilder(nil, testLogger())
	b.SetLanguages([]string{"English"})
	sess := heardSession("gym-grind")
	sess.Vocal = true
	sess.SungLanguages = []string{"Japanese"}
	o := New(testConfig(), enginetest.NewMock(), b, store, sess, &capturePlayer{}, testLogger())

	states := o.Languages()
	if len(states) != 2 {
		t.Fatalf("the session's own language should be listed too: %+v", states)
	}
	var jp LanguageState
	for _, l := range states {
		if l.Name == "Japanese" {
			jp = l
		}
	}
	if !jp.On || jp.Configured {
		t.Fatalf("Japanese should be sung and marked unconfigured: %+v", jp)
	}

	ack := o.SetLanguage("Japanese", false)
	if !strings.Contains(ack, "engine picks") {
		t.Fatalf("switching off the only language should hand the choice back: %q", ack)
	}
	if o.CurrentName() == "gym-grind" {
		t.Fatal("a language change did not branch the session")
	}
	if _, err := store.Load("gym-grind"); err != nil {
		t.Fatalf("the session before the change is gone: %v", err)
	}
	for _, l := range o.Languages() {
		if l.Name == "Japanese" {
			t.Fatalf("Japanese is still listed after being switched off: %+v", l)
		}
	}
}
