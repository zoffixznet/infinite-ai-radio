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

	"iar/internal/player"
	"iar/internal/session"
)

// Controller interprets user input lines and drives the orchestrator.
type Controller struct {
	// O is the running stream.
	O *player.Orchestrator
	// ExportsDir is where MP3 exports land by default.
	ExportsDir string

	// pendingDelete is a session awaiting the user's confirmation on
	// the next input line.
	pendingDelete string
}

// Handle processes one input line. It returns the response to display and
// whether the application should quit. Free text steers the music;
// commands work with or without a leading slash.
func (c *Controller) Handle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if name := c.pendingDelete; name != "" {
		c.pendingDelete = ""
		switch strings.ToLower(line) {
		case "y", "yes":
			return c.O.DeleteSession(name), false
		}
		return "delete cancelled; " + name + " kept", false
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
	case "new":
		if rest == "" {
			return "usage: new <prompt>  (e.g. new dark techno with vocals)", false
		}
		return c.O.NewSession(rest), false
	case "save", "snippet":
		which, tag := parseSave(rest)
		return c.O.SaveSnippet(which, tag), false
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
		return c.O.Listing().Render(time.Now()), false
	case "delete", "rm":
		if rest == "" {
			return "usage: delete <session-or-preset-name>", false
		}
		name := session.SanitizeName(rest)
		if name == c.O.CurrentName() {
			return "cannot delete " + name + ": it is playing right now (switch to something else first)", false
		}
		c.pendingDelete = name
		return "Delete session " + name + "? type y to confirm, anything else cancels", false
	case "presets":
		return c.presetsText(), false
	case "load", "session":
		if rest == "" {
			return "usage: load <session-name>", false
		}
		return c.O.LoadSession(rest), false
	case "preset":
		if rest == "" {
			return c.presetsText(), false
		}
		return c.O.LoadPreset(rest), false
	case "mp3", "export":
		return c.export(rest), false
	case "lyrics", "writer":
		return c.O.LyricsGen(rest), false
	case "lang", "languages":
		return c.languages(rest), false
	case "status":
		return statusText(c.O.Status()), false
	case "engine":
		st := c.O.Status()
		out := statusText(st)
		if n := len(st.EngineTail); n > 0 {
			tail := st.EngineTail
			if n > 8 {
				tail = tail[n-8:]
			}
			out += "\nrecent engine output:\n  " + strings.Join(tail, "\n  ")
		}
		return out, false
	default:
		// Free text: steer the stream.
		return c.O.Steer(line), false
	}
}

// parseSave splits "save [prev] [tag words]" into which track to save
// and the tag to file it under.
func parseSave(rest string) (which, tag string) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", ""
	}
	switch strings.ToLower(fields[0]) {
	case "prev", "previous", "last":
		return "prev", strings.Join(fields[1:], " ")
	}
	return "", strings.Join(fields, " ")
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

func (c *Controller) presetsText() string {
	var b strings.Builder
	b.WriteString("presets:\n")
	for _, g := range session.GroupPresets(c.O.Presets()) {
		fmt.Fprintf(&b, " %s:\n", g.Name)
		for _, p := range g.Presets {
			fmt.Fprintf(&b, "  %-18s %s\n", p.Name, p.Description)
		}
	}
	b.WriteString("use: preset <name>")
	return b.String()
}

// languages lists, switches or replaces the configured vocal
// languages. Bare: the list. "+name"/"-name": switch one on or off.
// Anything else: the new comma-separated list.
func (c *Controller) languages(rest string) string {
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "":
		states := c.O.Languages()
		if len(states) == 0 {
			return "no vocal languages configured; the music engine picks the language.\n" +
				"use: languages English, Russian, French"
		}
		var b strings.Builder
		b.WriteString("vocal languages (each song picks one at random):\n")
		for _, l := range states {
			mark := " "
			if l.On {
				mark = "*"
			}
			note := ""
			if !l.Engine {
				note = "   (sung untagged: the engine has no voice tag for it)"
			}
			fmt.Fprintf(&b, " %s %s%s\n", mark, l.Name, note)
		}
		b.WriteString("use: languages +English | languages -Russian | languages English, Russian")
		return b.String()
	case strings.HasPrefix(rest, "+"):
		return c.O.SetLanguage(strings.TrimSpace(rest[1:]), true)
	case strings.HasPrefix(rest, "-"):
		return c.O.SetLanguage(strings.TrimSpace(rest[1:]), false)
	case rest == "none", rest == "off":
		return c.O.SetLanguages(nil)
	default:
		var names []string
		for _, part := range strings.Split(rest, ",") {
			if p := strings.TrimSpace(part); p != "" {
				names = append(names, p)
			}
		}
		return c.O.SetLanguages(names)
	}
}

// sungIn lists the vocal languages currently switched on, empty when
// none are configured (the engine then picks the language itself).
func sungIn(states []player.LanguageState) string {
	var on []string
	for _, l := range states {
		if l.On {
			on = append(on, l.Name)
		}
	}
	return strings.Join(on, ", ")
}

// statusText renders a multi-line status snapshot.
func statusText(st player.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "state:    %s\n", st.State)
	if st.Phase != "" && st.Phase != "playing" {
		fmt.Fprintf(&b, "phase:    %s (%s elapsed, usually ~%s)\n",
			st.Phase, st.PhaseElapsed.Round(time.Second), st.PhaseExpected.Round(time.Second))
	}
	fmt.Fprintf(&b, "source:   %s\n", st.Source)
	if st.Duration > 0 {
		fmt.Fprintf(&b, "position: %s / %s\n", fmtDur(st.Elapsed), fmtDur(st.Duration))
	}
	fmt.Fprintf(&b, "session:  %s (%s)\n", st.Session, st.SessionDesc)
	if st.LyricsGenerator != "" {
		fmt.Fprintf(&b, "lyrics:   %s\n", st.LyricsGenerator)
	}
	if sung := sungIn(st.Languages); sung != "" {
		fmt.Fprintf(&b, "sung in:  %s\n", sung)
	}
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
	if st.FailStreak > 0 {
		fmt.Fprintf(&b, "trouble:  %d generation failure(s) in a row; last: %s\n", st.FailStreak, st.LastFailure)
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

// helpText is kept compact (two command columns) so the whole block fits
// a standard 80x24 terminal's message area in one screen.
const helpText = `steer with plain text ("calmer", "add vocals about winning", "pink noise")
commands (leading / optional):
  clear             wipe steering        name <name>      save this session
  new <prompt>      fresh session        sessions         list saved + presets
  save [prev] [tag] track -> MP3         load <name>      resume a session
  mp3 <min> [file]  export MP3           preset <name>    switch preset
  skip              next track           delete <name>    delete a session
  pause | resume    pause / continue     volume <0-100>   set volume
  lyrics [name]     pick lyric writer    status | engine  show status
  languages [list]  sung languages       help | quit      this list / exit`
