package ui

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"iar/internal/config"
	"iar/internal/engine/enginetest"
	"iar/internal/player"
	"iar/internal/prompting"
	"iar/internal/session"
)

// discardPlayer swallows audio with light pacing.
type discardPlayer struct{}

func (d *discardPlayer) Write(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return len(b), nil
}
func (d *discardPlayer) Close() error { return nil }
func (d *discardPlayer) Name() string { return "discard" }

func newController(t *testing.T) *Controller {
	t.Helper()
	cfg := config.Default()
	cfg.TrackSeconds = 2
	cfg.CrossfadeSeconds = 0.5
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := player.New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, log),
		session.NewStore(t.TempDir()), session.New(), &discardPlayer{}, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })
	return &Controller{O: o, ExportsDir: t.TempDir()}
}

func TestControllerCommandsWorkWithAndWithoutSlash(t *testing.T) {
	c := newController(t)
	for _, in := range []string{"/help", "help"} {
		resp, quit := c.Handle(in)
		if quit || !strings.Contains(resp, "steering") {
			t.Fatalf("help via %q = %q", in, resp)
		}
	}
	resp, _ := c.Handle("/volume 30")
	if !strings.Contains(resp, "30%") {
		t.Fatalf("volume resp = %q", resp)
	}
	resp, _ = c.Handle("presets")
	if !strings.Contains(resp, "lofi-study") || !strings.Contains(resp, "grind") {
		t.Fatalf("presets resp = %q", resp)
	}
	resp, quit := c.Handle("quit")
	if !quit {
		t.Fatalf("quit did not quit (resp %q)", resp)
	}
}

func TestControllerFreeTextSteers(t *testing.T) {
	c := newController(t)
	resp, quit := c.Handle("make it dreamy and slow")
	if quit || !strings.Contains(resp, "steering") {
		t.Fatalf("steer resp = %q", resp)
	}
	resp, _ = c.Handle("clear")
	if !strings.Contains(resp, "cleared") {
		t.Fatalf("clear resp = %q", resp)
	}
}

func TestControllerExportValidation(t *testing.T) {
	c := newController(t)
	resp, _ := c.Handle("mp3")
	if !strings.Contains(resp, "usage") {
		t.Fatalf("mp3 usage resp = %q", resp)
	}
	resp, _ = c.Handle("mp3 nope")
	if !strings.Contains(resp, "usage") {
		t.Fatalf("mp3 bad minutes resp = %q", resp)
	}
	resp, _ = c.Handle("mp3 9999")
	if !strings.Contains(resp, "between 1 and") {
		t.Fatalf("mp3 cap resp = %q", resp)
	}
}

func TestControllerSessionFlow(t *testing.T) {
	c := newController(t)
	resp, _ := c.Handle("name focus time")
	if !strings.Contains(resp, "focus-time") {
		t.Fatalf("name resp = %q", resp)
	}
	resp, _ = c.Handle("sessions")
	if !strings.Contains(resp, "focus-time") {
		t.Fatalf("sessions resp = %q", resp)
	}
	resp, _ = c.Handle("load focus-time")
	if !strings.Contains(resp, "focus-time") {
		t.Fatalf("load resp = %q", resp)
	}
	resp, _ = c.Handle("preset pink-noise")
	if !strings.Contains(resp, "pink") {
		t.Fatalf("preset resp = %q", resp)
	}
	resp, _ = c.Handle("status")
	if !strings.Contains(resp, "session:") {
		t.Fatalf("status resp = %q", resp)
	}
}

func TestControllerDeleteConfirmation(t *testing.T) {
	c := newController(t)
	c.Handle("name keeper")
	c.Handle("preset pink-noise") // switch away so keeper can be deleted
	resp, _ := c.Handle("delete")
	if !strings.Contains(resp, "usage") {
		t.Fatalf("delete usage = %q", resp)
	}
	// Cancel: anything but y/yes keeps the session.
	resp, _ = c.Handle("delete keeper")
	if !strings.Contains(resp, "Delete session keeper?") {
		t.Fatalf("delete prompt = %q", resp)
	}
	resp, _ = c.Handle("no")
	if !strings.Contains(resp, "cancelled") {
		t.Fatalf("cancel resp = %q", resp)
	}
	if resp, _ := c.Handle("sessions"); !strings.Contains(resp, "keeper") {
		t.Fatalf("session gone after cancel: %q", resp)
	}
	// Confirm with y.
	c.Handle("delete keeper")
	resp, _ = c.Handle("y")
	if resp != "session keeper deleted" {
		t.Fatalf("confirm resp = %q", resp)
	}
	if resp, _ := c.Handle("sessions"); strings.Contains(resp, "  keeper") {
		t.Fatalf("session still listed: %q", resp)
	}
	// The playing session is refused before any confirmation.
	resp, _ = c.Handle("delete " + c.O.CurrentName())
	if !strings.Contains(resp, "playing right now") {
		t.Fatalf("delete current = %q", resp)
	}
	if resp, _ := c.Handle("y"); strings.Contains(resp, "deleted") {
		t.Fatalf("stray y did something: %q", resp)
	}
	// Presets are deletable (tombstoned) and restored via the store.
	c.Handle("delete sleep")
	if resp, _ := c.Handle("yes"); !strings.Contains(resp, "preset sleep deleted") {
		t.Fatalf("preset delete = %q", resp)
	}
	if resp, _ := c.Handle("presets"); strings.Contains(resp, "\n  sleep ") {
		t.Fatalf("tombstoned preset listed: %q", resp)
	}
}

func TestControllerSessionsListingIsGrouped(t *testing.T) {
	c := newController(t)
	c.Handle("name focus time")
	resp, _ := c.Handle("sessions")
	for _, want := range []string{session.LabelNamed + ":", session.LabelPresets + ":", session.LabelAuto + ":", "focus-time", "jazz-club", "just now"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("sessions listing missing %q:\n%s", want, resp)
		}
	}
	if strings.Index(resp, session.LabelNamed) > strings.Index(resp, session.LabelPresets) ||
		strings.Index(resp, session.LabelPresets) > strings.Index(resp, session.LabelAuto) {
		t.Fatalf("group order wrong:\n%s", resp)
	}
	resp, _ = c.Handle("help")
	if !strings.Contains(resp, "delete <n|autos>") {
		t.Fatalf("help lacks delete: %q", resp)
	}
	if n := strings.Count(resp, "\n") + 1; n > 10 {
		t.Fatalf("help grew to %d lines; it must fit an 80x24 terminal", n)
	}
}

func TestParseSave(t *testing.T) {
	for _, tc := range []struct{ in, which, tag string }{
		{"", "", ""},
		{"prev", "prev", ""},
		{"previous gym", "prev", "gym"},
		{"last late night drive", "prev", "late night drive"},
		{"gym", "", "gym"},
		{"late night drive", "", "late night drive"},
		{"Prev Favourites", "prev", "Favourites"},
	} {
		which, tag := parseSave(tc.in)
		if which != tc.which || tag != tc.tag {
			t.Errorf("parseSave(%q) = (%q, %q); want (%q, %q)", tc.in, which, tag, tc.which, tc.tag)
		}
	}
}

func TestControllerLyricsCommand(t *testing.T) {
	c := newController(t)
	out, quit := c.Handle("lyrics")
	if quit || !strings.Contains(out, "scribe") || !strings.Contains(out, "smoothbrain") {
		t.Fatalf("lyrics listing = %q", out)
	}
	out, _ = c.Handle("/lyrics smoothbrain")
	if !strings.Contains(out, "smoothbrain") {
		t.Fatalf("lyrics switch = %q", out)
	}
	out, _ = c.Handle("lyrics nonsense")
	if !strings.Contains(out, "unknown") {
		t.Fatalf("lyrics unknown = %q", out)
	}
}

// The languages command covers the whole surface: listing, configuring,
// switching one on or off, and handing the choice back to the engine.
func TestControllerLanguagesCommand(t *testing.T) {
	c := newController(t)

	if out, _ := c.Handle("languages"); !strings.Contains(out, "no vocal languages configured") {
		t.Fatalf("empty listing = %q", out)
	}
	if out, _ := c.Handle("languages English, Russian, Bisaya (Cebuano), Klingon"); !strings.Contains(out, "English") {
		t.Fatalf("configure = %q", out)
	}
	// Configuring offers them; this session sings in none of them yet,
	// so the engine is still choosing.
	out, _ := c.Handle("languages")
	if strings.Contains(out, "\n * ") {
		t.Fatalf("configuring a list switched languages on by itself:\n%s", out)
	}
	c.Handle("lang +English")
	c.Handle("lang +Russian")
	c.Handle("lang +Bisaya (Cebuano)")
	out, _ = c.Handle("languages")
	for _, want := range []string{"* English", "* Russian", "* Bisaya (Cebuano)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
	// A language the engine has no voice tag for is still usable, and
	// the listing says so rather than hiding it.
	if !strings.Contains(out, "sung untagged") {
		t.Fatalf("listing does not flag the untagged language:\n%s", out)
	}
	if out, _ := c.Handle("lang -Russian"); strings.Contains(out, "Russian") {
		t.Fatalf("switching Russian off should drop it from the answer: %q", out)
	}
	out, _ = c.Handle("languages")
	if !strings.Contains(out, "  Russian") || strings.Contains(out, "* Russian") {
		t.Fatalf("Russian should be listed but unmarked:\n%s", out)
	}
	if out, _ := c.Handle("lang +Russian"); !strings.Contains(out, "Russian") {
		t.Fatalf("switching Russian back on = %q", out)
	}
	// "none" is about this session, not the machine's list: it stops
	// asking for a language, and the engine picks per song.
	if out, _ := c.Handle("languages none"); !strings.Contains(out, "whatever language the music engine picks") {
		t.Fatalf("clearing = %q", out)
	}
	if out, _ := c.Handle("languages"); strings.Contains(out, "\n * ") {
		t.Fatalf("none left a language switched on:\n%s", out)
	}
	if out, _ := c.Handle("languages +Elvish"); !strings.Contains(out, "not one of the configured") {
		t.Fatalf("switching an unconfigured language = %q", out)
	}
	if out, _ := c.Handle("help"); !strings.Contains(out, "languages") {
		t.Fatalf("help does not list the command:\n%s", out)
	}
	// status reports what this session sings in, and says nothing while
	// the engine is choosing.
	if out, _ := c.Handle("status"); strings.Contains(out, "sung in:") {
		t.Fatalf("status names languages with none switched on:\n%s", out)
	}
	c.Handle("lang +English")
	c.Handle("lang +Russian")
	if out, _ := c.Handle("status"); !strings.Contains(out, "sung in:  English, Russian") {
		t.Fatalf("status does not report the languages:\n%s", out)
	}
}
