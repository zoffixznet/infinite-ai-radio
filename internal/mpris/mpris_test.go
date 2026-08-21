package mpris

import (
	"io"
	"log/slog"
	"testing"

	"github.com/godbus/dbus/v5/prop"
)

// fakeControls records what the MPRIS surface asked the player to do.
type fakeControls struct {
	paused   bool
	volume   int
	skips    int
	toggles  int
	pauses   int
	resumes  int
	lastArgs []int
}

func (f *fakeControls) Pause() string       { f.pauses++; f.paused = true; return "" }
func (f *fakeControls) Resume() string      { f.resumes++; f.paused = false; return "" }
func (f *fakeControls) TogglePause() string { f.toggles++; f.paused = !f.paused; return "" }
func (f *fakeControls) Skip() string        { f.skips++; return "" }
func (f *fakeControls) SetVolume(v int) string {
	f.volume = v
	f.lastArgs = append(f.lastArgs, v)
	return ""
}
func (f *fakeControls) Snapshot() (bool, int, string) { return f.paused, f.volume, "test title" }

func TestPlayerMethodsDriveControls(t *testing.T) {
	f := &fakeControls{volume: 80}
	s := &Server{ctl: f, log: discard()}
	p := &mprisPlayer{s: s}
	p.Next()
	p.Pause()
	p.Play()
	p.PlayPause()
	if f.skips != 1 || f.pauses != 1 || f.resumes != 1 || f.toggles != 1 {
		t.Fatalf("controls not driven: %+v", f)
	}
	// Stop maps to pause (there is no stopped state).
	p.Stop()
	if f.pauses != 2 {
		t.Fatal("Stop should pause")
	}
}

func TestVolumeWriteClampsAndScales(t *testing.T) {
	f := &fakeControls{}
	s := &Server{ctl: f, log: discard()}
	if err := s.onVolume(&prop.Change{Value: 0.4}); err != nil {
		t.Fatal(err)
	}
	if f.volume != 40 {
		t.Fatalf("volume = %d; want 40", f.volume)
	}
	s.onVolume(&prop.Change{Value: 3.0})
	if f.volume != 100 {
		t.Fatalf("volume = %d; want clamped 100", f.volume)
	}
	s.onVolume(&prop.Change{Value: -1.0})
	if f.volume != 0 {
		t.Fatalf("volume = %d; want clamped 0", f.volume)
	}
	if err := s.onVolume(&prop.Change{Value: "loud"}); err == nil {
		t.Fatal("non-float volume accepted")
	}
}

func TestStatusAndMetadata(t *testing.T) {
	if statusString(true) != "Paused" || statusString(false) != "Playing" {
		t.Fatal("status strings wrong")
	}
	md := metadata("my prompt")
	if md["xesam:title"].Value() != "my prompt" {
		t.Fatalf("metadata title = %v", md["xesam:title"].Value())
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
