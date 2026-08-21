// Package ui provides the two user interfaces over the running stream: a
// full-screen terminal UI and a plain line-based mode for pipes and
// scripts. Both share one command controller, so every feature works with
// plain typed words and Enter.
package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"bgm/internal/player"
	"bgm/internal/session"
)

// Controller interprets user input lines and drives the orchestrator.
type Controller struct {
	// O is the running stream.
	O *player.Orchestrator
	// ExportsDir is where MP3 exports land by default.
	ExportsDir string
}

// Handle processes one input line. It returns the response to display and
// whether the application should quit. Free text steers the music;
// commands work with or without a leading slash.
func (c *Controller) Handle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	cmd := strings.TrimPrefix(line, "/")
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", false
	}
	head := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, fields[0]))

	switch head {
	case "quit", "exit", "q":
		return "goodbye", true
	case "help", "h", "?":
		return helpText, false
	case "clear":
		return c.O.Clear(), false
	case "skip", "next":
		return c.O.Skip(), false
	case "pause":
		return c.O.TogglePause(), false
	case "resume", "play":
		return c.O.Resume(), false
	case "volume", "vol":
		if rest == "" {
			return fmt.Sprintf("volume is %d%% (use: volume 0-100)", c.O.Status().Volume), false
		}
		v, err := strconv.Atoi(rest)
		if err != nil {
			return "usage: volume 0-100", false
		}
		return c.O.SetVolume(v), false
	case "name":
		if rest == "" {
			return "usage: name <session-name>", false
		}
		return c.O.NameSession(rest), false
	case "sessions":
		return c.sessionsText(), false
	case "presets":
		return presetsText(), false
	case "load", "session":
		if rest == "" {
			return "usage: load <session-name>", false
		}
		return c.O.LoadSession(rest), false
	case "preset":
		if rest == "" {
			return presetsText(), false
		}
		return c.O.LoadPreset(rest), false
	case "mp3", "export":
		return c.export(rest), false
	case "engine", "status":
		return statusText(c.O.Status()), false
	default:
		// Free text: steer the stream.
		return c.O.Steer(line), false
	}
}

// export parses "mp3 <minutes> [file]".
func (c *Controller) export(rest string) string {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "usage: mp3 <minutes> [file.mp3]"
	}
	minutes, err := strconv.Atoi(fields[0])
	if err != nil || minutes < 1 {
		return "usage: mp3 <minutes> [file.mp3]"
	}
	out := ""
	if len(fields) > 1 {
		out = strings.Join(fields[1:], " ")
	}
	return c.O.Export(minutes, out, c.ExportsDir)
}

func (c *Controller) sessionsText() string {
	var b strings.Builder
	b.WriteString("presets:\n")
	for _, p := range session.Presets() {
		fmt.Fprintf(&b, "  %-12s %s\n", p.Name, p.Description)
	}
	names := c.O.SessionNames()
	if len(names) == 0 {
		b.WriteString("no saved sessions yet (type: name <something>)")
		return b.String()
	}
	b.WriteString("saved sessions:\n")
	for _, n := range names {
		b.WriteString("  " + n + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func presetsText() string {
	var b strings.Builder
	b.WriteString("presets:\n")
	for _, p := range session.Presets() {
		fmt.Fprintf(&b, "  %-12s %s\n", p.Name, p.Description)
	}
	b.WriteString("use: preset <name>")
	return b.String()
}

// statusText renders a multi-line status snapshot.
func statusText(st player.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "state:    %s\n", st.State)
	fmt.Fprintf(&b, "source:   %s\n", st.Source)
	if st.Duration > 0 {
		fmt.Fprintf(&b, "position: %s / %s\n", fmtDur(st.Elapsed), fmtDur(st.Duration))
	}
	fmt.Fprintf(&b, "session:  %s (%s)\n", st.Session, st.SessionDesc)
	engine := "none (noise only)"
	if st.EngineName != "" {
		switch {
		case st.EngineReady:
			engine = st.EngineName + " ready"
		default:
			engine = st.EngineName + " starting (this can take a while on first run)"
		}
	}
	fmt.Fprintf(&b, "engine:   %s\n", engine)
	fmt.Fprintf(&b, "buffer:   %d track(s) queued, generating: %v\n", st.Queued, st.Generating)
	if st.GenCount > 0 {
		fmt.Fprintf(&b, "gen:      %d tracks, last took %s\n", st.GenCount, st.LastGenTime.Round(time.Second))
	}
	if st.Exporting != "" {
		fmt.Fprintf(&b, "export:   %s running\n", st.Exporting)
	}
	fmt.Fprintf(&b, "volume:   %d%%", st.Volume)
	return b.String()
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

const helpText = `steering: just type what you want, e.g.
  make it more energetic | calmer | faster | add vocals about winning
  switch to piano | generate pink noise
commands (leading / optional):
  clear                wipe the steering context
  name <name>          save this session under a name
  sessions             list presets and saved sessions
  load <name>          resume a saved session
  preset <name>        switch to a built-in preset
  mp3 <minutes> [file] export minutes of this vibe to an MP3
  skip                 jump to the next track
  pause | resume       pause or continue playback
  volume <0-100>       set output volume
  status               show engine and buffer status
  quit                 exit`
