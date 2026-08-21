package ui

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"bgm/internal/config"
	"bgm/internal/engine/enginetest"
	"bgm/internal/player"
	"bgm/internal/prompting"
	"bgm/internal/session"
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
	if quit || !strings.Contains(resp, "steering with") {
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
