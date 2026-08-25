package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoNamedInference(t *testing.T) {
	cases := []struct {
		name  string
		named bool
		auto  bool
	}{
		{"session-20260821-220425", false, true},
		{"prompt-dark-techno-120001", false, true},
		{"grind-20260821-081009", false, true},
		{"lofi-study-20260821-093503", false, true},
		{"gym-grind", false, false},
		{"multi-lang-rock", false, false},
		{"chill-2026", false, false},
		// An explicit name wins over the pattern (renamed to look like one).
		{"session-20260821-220425", true, false},
	}
	for _, tc := range cases {
		s := &Session{Name: tc.name, Named: tc.named}
		if got := s.AutoNamed(); got != tc.auto {
			t.Errorf("AutoNamed(%q, named=%v) = %v, want %v", tc.name, tc.named, got, tc.auto)
		}
	}
	// Played falls back to Updated for older files.
	s := &Session{Updated: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	if !s.Played().Equal(s.Updated) {
		t.Fatal("Played did not fall back to Updated")
	}
	s.LastPlayed = s.Updated.Add(time.Hour)
	if !s.Played().Equal(s.LastPlayed) {
		t.Fatal("Played ignored LastPlayed")
	}
}

// mk saves a session with the given name and last-played age.
func mk(t *testing.T, st *Store, name string, named bool, playedAgo time.Duration, now time.Time) {
	t.Helper()
	s := New()
	s.Name = name
	s.Named = named
	s.Created = now.Add(-playedAgo - time.Hour)
	s.Updated = now.Add(-playedAgo)
	s.LastPlayed = now.Add(-playedAgo)
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
}

func names(list []*Session) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestSweepBoundariesAndExclusions(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	retention := 48 * time.Hour
	mk(t, st, "session-20260819-120000", false, retention, now)             // exactly at the edge: kept
	mk(t, st, "session-20260819-115959", false, retention+time.Second, now) // just over: swept
	mk(t, st, "prompt-dark-techno-100000", false, 10*24*time.Hour, now)     // old: swept
	mk(t, st, "grind-20260810-081009", false, 10*24*time.Hour, now)         // old, but the one playing: kept
	mk(t, st, "gym-grind", false, 30*24*time.Hour, now)                     // user-named (pattern): kept
	mk(t, st, "session-20260101-000000", true, 30*24*time.Hour, now)        // user-named (flag): kept
	mk(t, st, "session-20260821-110000", false, time.Hour, now)             // fresh: kept

	removed, err := st.Sweep(now, retention, "grind-20260810-081009")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(removed, ","); got != "session-20260819-115959,prompt-dark-techno-100000" {
		t.Fatalf("removed = %q", got)
	}
	left, _ := st.List()
	if len(left) != 5 {
		t.Fatalf("left = %s", names(left))
	}
	for _, n := range []string{"session-20260819-120000", "grind-20260810-081009", "gym-grind", "session-20260101-000000", "session-20260821-110000"} {
		if _, err := st.Load(n); err != nil {
			t.Errorf("%s was swept", n)
		}
	}
	// Retention 0 disables the sweep entirely.
	removed, err = st.Sweep(now.Add(365*24*time.Hour), 0, "")
	if err != nil || len(removed) != 0 {
		t.Fatalf("retention 0 removed %v (%v)", removed, err)
	}
	// A second run is a no-op.
	removed, _ = st.Sweep(now, retention, "grind-20260810-081009")
	if len(removed) != 0 {
		t.Fatalf("second sweep removed %v", removed)
	}
	// Presets are not files in the store, so they are never candidates;
	// and a missing directory is not an error.
	if _, err := NewStore(filepath.Join(t.TempDir(), "missing")).Sweep(now, retention, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGroupOrderAndRender(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk(t, st, "gym-grind", true, 3*time.Hour, now)
	mk(t, st, "evening-chill", true, 2*24*time.Hour, now)
	mk(t, st, "late-night", true, 5*time.Minute, now)
	mk(t, st, "session-20260820-100000", false, 26*time.Hour, now)
	mk(t, st, "session-20260821-110000", false, time.Hour, now)
	mk(t, st, "prompt-dark-techno-090000", false, 3*time.Hour, now)

	l, err := st.Listing()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(l.Named); got != "late-night,gym-grind,evening-chill" {
		t.Fatalf("named order = %q (want most recently played first)", got)
	}
	if got := names(l.Auto); got != "session-20260821-110000,prompt-dark-techno-090000,session-20260820-100000" {
		t.Fatalf("auto order = %q (want newest first)", got)
	}
	var presets []string
	for _, p := range l.Presets {
		presets = append(presets, p.Name)
	}
	want := "grind,hard-rock,liquid-dnb,nu-metal,pop-punk," +
		"chiptune,deep-house,funk-soul,sunshine-pop," +
		"boom-bap,epic-score,night-drive,reggae-dub,roadhouse-country," +
		"chamber-strings,deep-focus,jazz-club,lofi-study," +
		"pink-noise,sleep"
	if got := strings.Join(presets, ","); got != want {
		t.Fatalf("preset order = %q\nwant %q", got, want)
	}

	text := l.Render(now)
	lines := strings.Split(text, "\n")
	// Three labeled groups in order.
	idx := func(label string) int {
		for i, ln := range lines {
			if ln == label+":" {
				return i
			}
		}
		t.Fatalf("label %q missing in:\n%s", label, text)
		return -1
	}
	if !(idx(LabelNamed) < idx(LabelPresets) && idx(LabelPresets) < idx(LabelAuto)) {
		t.Fatalf("group order wrong:\n%s", text)
	}
	// Rows are aligned: name column, summary column, last played.
	if !strings.Contains(text, "  late-night               lofi chill beats") || !strings.Contains(text, "5m ago") {
		t.Fatalf("row format:\n%s", text)
	}
	if !strings.Contains(text, "  gym-grind") || !strings.Contains(text, "3h ago") || !strings.Contains(text, "2d ago") {
		t.Fatalf("last played missing:\n%s", text)
	}
	// Presets render under their energy-group headers, in the fixed
	// group order.
	if !strings.Contains(text, "    jazz-club              Late-night jazz combo") {
		t.Fatalf("preset row format:\n%s", text)
	}
	for _, g := range GroupOrder {
		if !strings.Contains(text, "  "+g+":") {
			t.Fatalf("group header %q missing:\n%s", g, text)
		}
	}
	if strings.Index(text, "  high-energy:") > strings.Index(text, "  sleep-noise:") {
		t.Fatalf("preset groups out of order:\n%s", text)
	}
	for _, ln := range lines {
		if len(ln) > 80 {
			t.Fatalf("line wider than 80 columns: %q", ln)
		}
	}
	// Empty groups say so.
	empty := Group(nil, nil).Render(now)
	if !strings.Contains(empty, "(none yet") || !strings.Contains(empty, "(all deleted") || !strings.Contains(empty, "(none)") {
		t.Fatalf("empty rendering:\n%s", empty)
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		30 * time.Second:     "just now",
		5 * time.Minute:      "5m ago",
		3 * time.Hour:        "3h ago",
		40 * time.Hour:       "40h ago",
		3 * 24 * time.Hour:   "3d ago",
		30 * 24 * time.Hour:  "Jul 22",
		100 * 24 * time.Hour: "May 13",
	} {
		if got := Ago(now, now.Add(-d)); got != want {
			t.Errorf("Ago(-%v) = %q, want %q", d, got, want)
		}
	}
	if Ago(now, time.Time{}) != "-" {
		t.Error("zero time should render as -")
	}
}

func TestPresetTombstones(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	all := len(st.Presets())
	if all != len(Presets()) || all < 5 {
		t.Fatalf("visible presets = %d", all)
	}
	if err := st.HidePreset("Sleep"); err != nil {
		t.Fatal(err)
	}
	if err := st.HidePreset("sleep"); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := st.HidePreset("no-such"); err == nil {
		t.Fatal("hiding an unknown preset succeeded")
	}
	if len(st.Presets()) != all-1 {
		t.Fatalf("hidden preset still listed: %d", len(st.Presets()))
	}
	if _, err := st.LookupPreset("sleep"); err == nil || !strings.Contains(err.Error(), "restore-presets") {
		t.Fatalf("hidden preset lookup err = %v", err)
	}
	if _, err := LookupPreset("sleep"); err != nil {
		t.Fatal("the package-level lookup must still know the preset")
	}
	l, _ := st.Listing()
	for _, p := range l.Presets {
		if p.Name == "sleep" {
			t.Fatal("listing shows the hidden preset")
		}
	}
	// The tombstone file is not mistaken for a session.
	sessions, _ := st.List()
	if len(sessions) != 0 {
		t.Fatalf("tombstone file listed as a session: %v", names(sessions))
	}
	// Persistent across store instances.
	if _, err := NewStore(dir).LookupPreset("sleep"); err == nil {
		t.Fatal("tombstone not persisted")
	}
	n, err := st.RestorePresets()
	if err != nil || n != 1 {
		t.Fatalf("restore = %d, %v", n, err)
	}
	if len(st.Presets()) != all {
		t.Fatal("presets not restored")
	}
	n, err = st.RestorePresets()
	if err != nil || n != 0 {
		t.Fatalf("second restore = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deleted-presets")); err == nil {
		t.Fatal("tombstone file left behind")
	}
}

func TestDeleteReportsMissing(t *testing.T) {
	st := NewStore(t.TempDir())
	if err := st.Delete("ghost"); err == nil {
		t.Fatal("deleting a missing session succeeded")
	}
	s := New()
	s.Name = "keep-me"
	st.Save(s)
	if err := st.Delete("keep-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load("keep-me"); err == nil {
		t.Fatal("session survived delete")
	}
}
