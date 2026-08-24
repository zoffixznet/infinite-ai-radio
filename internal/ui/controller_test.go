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
	if resp, _ := c.Handle("presets"); strings.Contains(resp, "sleep") {
		t.Fatalf("tombstoned preset listed: %q", resp)
	}
}

func TestControllerSessionsListingIsGrouped(t *testing.T) {
	c := newController(t)
	c.Handle("name focus time")
	resp, _ := c.Handle("sessions")
	for _, want := range []string{session.LabelNamed + ":", session.LabelPresets + ":", session.LabelAuto + ":", "focus-time", "calm-piano", "just now"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("sessions listing missing %q:\n%s", want, resp)
		}
	}
	if strings.Index(resp, session.LabelNamed) > strings.Index(resp, session.LabelPresets) ||
		strings.Index(resp, session.LabelPresets) > strings.Index(resp, session.LabelAuto) {
		t.Fatalf("group order wrong:\n%s", resp)
	}
	resp, _ = c.Handle("help")
	if !strings.Contains(resp, "delete <name>") {
		t.Fatalf("help lacks delete: %q", resp)
	}
	if n := strings.Count(resp, "\n") + 1; n > 10 {
		t.Fatalf("help grew to %d lines; it must fit an 80x24 terminal", n)
	}
}
