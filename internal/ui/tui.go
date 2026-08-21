package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"bgm/internal/player"
	"bgm/internal/session"
)

// RunTUI runs the full-screen terminal interface until the user quits or
// ctx ends.
func RunTUI(ctx context.Context, c *Controller) error {
	input := textinput.New()
	input.Placeholder = "type steering text or a command ('help')"
	input.CharLimit = 200
	// Focus here: Init works on a copy of the model, so focusing there
	// would be lost.
	input.Focus()
	m := tuiModel{c: c, input: input, styles: newStyles()}
	m.push("built-in presets (switch with 'preset NAME'):")
	for _, p := range session.Presets() {
		m.push(fmt.Sprintf("  %-12s %s", p.Name, p.Description))
	}
	m.push("steer with plain text ('calmer', 'add vocals about winning'),")
	m.push("start fresh with 'new <prompt>', save the playing track with 'save'")
	prog := tea.NewProgram(m, tea.WithContext(ctx))
	_, err := prog.Run()
	if err != nil && ctx.Err() != nil {
		return nil // canceled from outside; not an error
	}
	return err
}

// styles holds the color scheme. With NO_COLOR set (or when colors are
// unwanted) every style is a no-op, keeping output plain.
type styles struct {
	title   lipgloss.Style
	playing lipgloss.Style
	waiting lipgloss.Style
	warn    lipgloss.Style
	label   lipgloss.Style
	value   lipgloss.Style
	barOn   lipgloss.Style
	barOff  lipgloss.Style
	panel   lipgloss.Style
	muted   lipgloss.Style
}

// newStyles picks high-contrast basic ANSI colors, which stay readable on
// both light and dark terminal palettes.
func newStyles() styles {
	if os.Getenv("NO_COLOR") != "" {
		plain := lipgloss.NewStyle()
		return styles{
			title: plain, playing: plain, waiting: plain, warn: plain,
			label: plain, value: plain, barOn: plain, barOff: plain,
			panel: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).PaddingLeft(1).PaddingRight(1),
			muted: plain,
		}
	}
	return styles{
		title:   lipgloss.NewStyle().Bold(true),
		playing: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green),
		waiting: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Yellow),
		warn:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Red),
		label:   lipgloss.NewStyle().Foreground(lipgloss.Cyan),
		value:   lipgloss.NewStyle(),
		barOn:   lipgloss.NewStyle().Foreground(lipgloss.Green),
		barOff:  lipgloss.NewStyle().Foreground(lipgloss.Blue),
		panel: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Cyan).PaddingLeft(1).PaddingRight(1),
		muted: lipgloss.NewStyle().Foreground(lipgloss.Magenta),
	}
}

// tuiModel is the Bubble Tea model of the main screen.
type tuiModel struct {
	c      *Controller
	input  textinput.Model
	status player.Status
	styles styles
	msgs   []string
	width  int
	ticks  int
}

// Messages driving periodic refresh and stream events.
type (
	tickMsg  time.Time
	eventMsg player.Event
)

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		tickCmd(),
		listenEvents(m.c.O.Events()),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func listenEvents(ch <-chan player.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		// Guard against tiny or unreported terminal sizes; a negative
		// width makes the text input unusable.
		if w := msg.Width - 4; w >= 10 {
			m.input.SetWidth(w)
		}
		return m, nil
	case tickMsg:
		m.status = m.c.O.Status()
		m.ticks++
		return m, tickCmd()
	case eventMsg:
		m.push("* " + msg.Text)
		return m, listenEvents(m.c.O.Events())
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "enter":
			line := m.input.Value()
			m.input.Reset()
			if strings.TrimSpace(line) == "" {
				return m, nil
			}
			m.push("> " + line)
			resp, quit := m.c.Handle(line)
			if resp != "" {
				for _, l := range strings.Split(resp, "\n") {
					m.push("  " + l)
				}
			}
			if quit {
				return m, tea.Quit
			}
			m.status = m.c.O.Status()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// push appends a message line, keeping a small scrollback.
func (m *tuiModel) push(line string) {
	m.msgs = append(m.msgs, line)
	if len(m.msgs) > 200 {
		m.msgs = m.msgs[len(m.msgs)-200:]
	}
}

// bar renders a determinate progress bar of the given width.
func (m tuiModel) bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	on := int(frac*float64(width) + 0.5)
	return m.styles.barOn.Render(strings.Repeat("█", on)) +
		m.styles.barOff.Render(strings.Repeat("░", width-on))
}

// pulseBar renders an indeterminate animated activity bar.
func (m tuiModel) pulseBar(width int) string {
	pos := m.ticks % (2 * width)
	if pos >= width {
		pos = 2*width - pos - 1
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i == pos {
			b.WriteString(m.styles.barOn.Render("█"))
		} else {
			b.WriteString(m.styles.barOff.Render("░"))
		}
	}
	return b.String()
}

func (m tuiModel) View() tea.View {
	st := m.status
	s := m.styles
	var b strings.Builder

	// Header: name, state badge, volume, underruns.
	stateStyle := s.playing
	if st.State != "playing" && st.State != "noise" {
		stateStyle = s.waiting
	}
	header := s.title.Render("bgm") + "  " + stateStyle.Render(strings.ToUpper(st.State))
	if st.Paused {
		header += "  " + s.warn.Render("[paused]")
	}
	header += "   " + s.label.Render("vol") + fmt.Sprintf(" %d%%", st.Volume)
	under := fmt.Sprintf("underruns %d", st.Underruns)
	if st.Underruns > 0 {
		header += "   " + s.warn.Render(under)
	} else {
		header += "   " + s.muted.Render(under)
	}
	b.WriteString(header + "\n")

	// Now-playing panel.
	var panel strings.Builder
	panel.WriteString(s.label.Render("now     ") + s.value.Render(st.Source) + "\n")
	line := s.label.Render("session ") + s.value.Render(st.Session)
	if st.Duration > 0 {
		line += s.value.Render(fmt.Sprintf("   %s / %s", fmtDur(st.Elapsed), fmtDur(st.Duration)))
	}
	panel.WriteString(line + "\n")
	next := fmt.Sprintf("%d track(s) ready", st.Queued)
	if st.Generating {
		next += " · generating"
	}
	if st.Exporting != "" {
		next += " · exporting " + st.Exporting
	}
	panel.WriteString(s.label.Render("next    ") + s.value.Render(next))
	b.WriteString(s.panel.Render(panel.String()) + "\n")

	// Startup phase progress (determinate, from recorded expectations).
	if st.Phase != "" && st.Phase != "playing" {
		frac := 0.0
		if st.PhaseExpected > 0 {
			frac = float64(st.PhaseElapsed) / float64(st.PhaseExpected)
		}
		line := s.label.Render(fmt.Sprintf("%-8s", "status")) +
			m.bar(frac, 24) +
			fmt.Sprintf(" %s  %s", st.Phase, fmtDur(st.PhaseElapsed))
		if st.PhaseExpected > 0 {
			line += fmt.Sprintf(" of ~%s", fmtDur(st.PhaseExpected))
		}
		if st.PhaseSlow {
			line += " " + s.warn.Render("(longer than usual - see 'bgm doctor')")
		}
		b.WriteString(line + "\n")
	}

	// Generation activity and buffer gauge.
	if st.Generating {
		b.WriteString(s.label.Render(fmt.Sprintf("%-8s", "gen")) + m.pulseBar(24) +
			" generating next track\n")
	}
	maxBuf := st.BufferTarget
	if maxBuf > 0 {
		b.WriteString(s.label.Render(fmt.Sprintf("%-8s", "buffer")) +
			m.bar(float64(st.Queued)/float64(maxBuf), 24) +
			fmt.Sprintf(" %d/%d buffered\n", st.Queued, maxBuf))
	}

	b.WriteString(s.muted.Render(strings.Repeat("─", max(20, min(m.width, 78)))) + "\n")

	// Recent messages: last 10 lines.
	msgs := m.msgs
	if len(msgs) > 10 {
		msgs = msgs[len(msgs)-10:]
	}
	for _, l := range msgs {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + m.input.View() + "\n")
	return tea.NewView(b.String())
}
