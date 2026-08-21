package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

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
	m := tuiModel{c: c, input: input}
	m.push("built-in presets (switch with 'preset NAME'):")
	for _, p := range session.Presets() {
		m.push(fmt.Sprintf("  %-12s %s", p.Name, p.Description))
	}
	m.push("type steering text ('calmer', 'add vocals about winning') or 'help'")
	prog := tea.NewProgram(m, tea.WithContext(ctx))
	_, err := prog.Run()
	if err != nil && ctx.Err() != nil {
		return nil // canceled from outside; not an error
	}
	return err
}

// tuiModel is the Bubble Tea model of the main screen.
type tuiModel struct {
	c      *Controller
	input  textinput.Model
	status player.Status
	msgs   []string
	width  int
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
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
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

func (m tuiModel) View() tea.View {
	var b strings.Builder
	st := m.status
	b.WriteString("bgm  ")
	b.WriteString(st.State)
	if st.Paused {
		b.WriteString("  [paused]")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  now:     %s\n", st.Source)
	if st.Duration > 0 {
		fmt.Fprintf(&b, "  time:    %s / %s\n", fmtDur(st.Elapsed), fmtDur(st.Duration))
	}
	fmt.Fprintf(&b, "  session: %s\n", st.Session)
	engineLine := "none (noise only)"
	if st.EngineName != "" {
		switch {
		case st.EngineReady:
			engineLine = st.EngineName + " ready"
		default:
			engineLine = st.EngineName + " starting..."
		}
	}
	genLine := ""
	if st.Generating {
		genLine = ", generating"
	}
	fmt.Fprintf(&b, "  engine:  %s (buffer %d%s)\n", engineLine, st.Queued, genLine)
	if st.Phase != "" && st.Phase != "playing" {
		phaseLine := fmt.Sprintf("  status:  %s... %s elapsed", st.Phase, st.PhaseElapsed.Round(time.Second))
		if st.PhaseExpected > 0 {
			phaseLine += fmt.Sprintf(" (usually ~%s)", st.PhaseExpected.Round(time.Second))
		}
		if st.PhaseSlow {
			phaseLine += " - longer than usual, see 'bgm doctor'"
		}
		b.WriteString(phaseLine + "\n")
	}
	if st.Exporting != "" {
		fmt.Fprintf(&b, "  export:  %s running\n", st.Exporting)
	}
	fmt.Fprintf(&b, "  volume:  %d%%\n", st.Volume)
	b.WriteString(strings.Repeat("-", max(20, min(m.width, 78))) + "\n")

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
