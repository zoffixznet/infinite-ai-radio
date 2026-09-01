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

	"iar/internal/player"
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
	m := tuiModel{c: c, input: input, styles: newStyles(), status: c.O.Status()}
	m.push("built-in presets (switch with 'preset NAME'):")
	for _, p := range c.O.Presets() {
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
	height int
	ticks  int
	// scroll is how many lines the backlog view is scrolled up from the
	// bottom; 0 means pinned to the latest output.
	scroll int
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
		m.height = msg.Height
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
		case "pgup":
			m.scroll += m.viewportSize() - 1
			m.clampScroll()
			return m, nil
		case "pgdown":
			m.scroll -= m.viewportSize() - 1
			m.clampScroll()
			return m, nil
		case "home":
			m.scroll = len(m.msgs) // clamped in View
			m.clampScroll()
			return m, nil
		case "end", "esc":
			m.scroll = 0
			return m, nil
		case "enter":
			line := m.input.Value()
			m.input.Reset()
			if strings.TrimSpace(line) == "" {
				return m, nil
			}
			m.scroll = 0
			m.push("> " + line)
			pushed := 1
			resp, quit := m.c.Handle(line)
			if resp != "" {
				for _, l := range strings.Split(resp, "\n") {
					m.push("  " + l)
					pushed++
				}
			}
			if quit {
				return m, tea.Quit
			}
			// A response taller than the view starts at its top with a
			// "more below" indicator, instead of showing only its tail.
			if vp := m.viewportSize(); pushed > vp {
				m.scroll = pushed - vp
			}
			m.status = m.c.O.Status()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// push appends a message line, keeping a small scrollback. While the
// user is scrolled up, arriving lines do not yank the view.
func (m *tuiModel) push(line string) {
	m.msgs = append(m.msgs, line)
	if m.scroll > 0 {
		m.scroll++
	}
	if len(m.msgs) > 400 {
		drop := len(m.msgs) - 400
		m.msgs = m.msgs[drop:]
		if m.scroll > len(m.msgs) {
			m.scroll = len(m.msgs)
		}
	}
}

// viewportSize is how many backlog lines fit between the chrome and the
// input line at the current terminal size.
func (m *tuiModel) viewportSize() int {
	if m.height <= 0 {
		return 10
	}
	chrome := strings.Count(m.renderChrome(), "\n")
	vp := m.height - chrome - 3 // indicator/blank + input + safety
	if vp < 3 {
		vp = 3
	}
	return vp
}

// clampScroll keeps the scroll offset inside the backlog.
func (m *tuiModel) clampScroll() {
	maxScroll := len(m.msgs) - m.viewportSize()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
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

// renderChrome renders everything above the message backlog: header,
// now-playing panel, phase/generation/buffer gauges and the separator.
func (m *tuiModel) renderChrome() string {
	st := m.status
	s := m.styles
	var b strings.Builder

	// Header: name, state badge, volume, underruns.
	stateStyle := s.playing
	if st.State != "playing" && st.State != "noise" {
		stateStyle = s.waiting
	}
	header := s.title.Render("Infinite AI Radio") + "  " + stateStyle.Render(strings.ToUpper(st.State))
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

	// Now-playing panel. Its width is fixed and its lines are clipped
	// rather than wrapped: a title one character too long would
	// otherwise add a row and shift everything below it.
	var panel strings.Builder
	room := m.panelWidth() - panelChrome - labelWidth
	panel.WriteString(s.label.Render("now     ") + s.value.Render(clip(nowLine(st), room)) + "\n")
	tail := ""
	if st.Duration > 0 {
		tail = fmt.Sprintf("   %s / %s", fmtDur(st.Elapsed), fmtDur(st.Duration))
	}
	line := s.label.Render("session ") + s.value.Render(clip(st.Session, room-len([]rune(tail)))+tail)
	panel.WriteString(line + "\n")
	// Generation is reported by its own row below, which is always
	// present; repeating it here only made the panel's border grow and
	// shrink several times a minute.
	next := bufferReady(st)
	if st.Exporting != "" {
		next += " · exporting " + st.Exporting
	}
	panel.WriteString(s.label.Render("next    ") + s.value.Render(clip(next, room)))
	// A fixed width keeps the border still when a long track title
	// follows a short one; the panel lines up with the separator below.
	b.WriteString(s.panel.Width(m.panelWidth()).Render(panel.String()) + "\n")

	// Startup phase progress (determinate, from recorded expectations).
	if st.Phase != "" && st.Phase != "playing" {
		frac := 0.0
		if st.PhaseExpected > 0 {
			frac = float64(st.PhaseElapsed) / float64(st.PhaseExpected)
		}
		line := s.label.Render(fmt.Sprintf("%-8s", "status")) +
			m.bar(frac, barWidth) +
			fmt.Sprintf(" %s  %s", st.Phase, fmtDur(st.PhaseElapsed))
		if st.PhaseExpected > 0 {
			line += fmt.Sprintf(" of ~%s", fmtDur(st.PhaseExpected))
		}
		if st.PhaseSlow {
			line += " " + s.warn.Render("(longer than usual - see 'iar doctor')")
		}
		b.WriteString(line + "\n")
	}

	if st.FailStreak > 0 {
		b.WriteString(s.warn.Render(fmt.Sprintf("%d generation failure(s) in a row - engine will restart itself", st.FailStreak)) + "\n")
	}

	// Generation activity and buffer gauge. The generation row is always
	// drawn, even when nothing is in flight: it toggles several times a
	// minute as the cycle moves between plan and render jobs, and a row
	// that appears and disappears shifts everything below it.
	genLine := s.label.Render(fmt.Sprintf("%-8s", "gen"))
	if st.Generating {
		genLine += m.pulseBar(barWidth) + " " + s.value.Render(clip("generating next track", m.textRoom()))
	} else {
		genLine += m.bar(0, barWidth) + " " + s.muted.Render(clip(genIdle(st), m.textRoom()))
	}
	b.WriteString(genLine + "\n")
	if frac, text, ok := bufferGauge(st); ok {
		b.WriteString(s.label.Render(fmt.Sprintf("%-8s", "buffer")) +
			m.bar(frac, barWidth) + " " + s.value.Render(clip(text, m.textRoom())) + "\n")
	}

	// Resource telemetry, when the radio was started with --telemetry:
	// what the machine has left, and what the engine is holding. Rows
	// without a gauge are indented past where the bars end, so every
	// value in the block starts in the same column.
	for _, row := range telemetryRows(st.Telemetry) {
		line := s.label.Render(fmt.Sprintf("%-8s", row.Label))
		if row.Frac >= 0 {
			line += m.bar(row.Frac, barWidth)
		} else {
			line += strings.Repeat(" ", barWidth)
		}
		b.WriteString(line + " " + s.value.Render(clip(row.Text, m.textRoom())) + "\n")
	}

	b.WriteString(s.muted.Render(strings.Repeat("─", m.panelWidth())) + "\n")
	return b.String()
}

const (
	// panelChrome is what the panel's own border and padding take out of
	// its declared width; lipgloss counts both inside Width.
	panelChrome = 4
	// labelWidth is the "now     " / "session " / "next    " column.
	labelWidth = 8
	// barWidth is every gauge bar's width.
	barWidth = 24
)

// textRoom is how many columns a gauge row has for its text once the
// label, the bar and their separating space are taken. Text past that
// wraps, which adds a line and shifts everything under it - the same
// jitter a disappearing row causes, just from the other direction.
func (m *tuiModel) textRoom() int {
	w := m.width
	if w <= 0 {
		w = 78
	}
	return w - labelWidth - barWidth - 1
}

// clip shortens text to n columns, marking the cut so a truncated title
// does not read as the whole one.
func clip(text string, n int) string {
	if n < 4 {
		n = 4
	}
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n-1]) + "…"
}

// panelWidth is the width the chrome lays itself out to: the terminal,
// capped so a very wide window does not stretch the panel across it.
func (m *tuiModel) panelWidth() int {
	return max(20, min(m.width, 78))
}

func (m tuiModel) View() tea.View {
	var b strings.Builder
	b.WriteString(m.renderChrome())

	// Backlog window: the last viewport lines, or wherever the user
	// scrolled to, with a position indicator when not pinned.
	m.clampScroll()
	vp := m.viewportSize()
	msgs := m.msgs
	total := len(msgs)
	showIndicator := m.scroll > 0
	lines := vp
	if showIndicator {
		lines = vp - 1
	}
	start := total - lines - m.scroll
	if start < 0 {
		start = 0
	}
	end := start + lines
	if end > total {
		end = total
	}
	for _, l := range msgs[start:end] {
		b.WriteString(l + "\n")
	}
	if showIndicator {
		below := total - end
		b.WriteString(m.styles.warn.Render(fmt.Sprintf(
			"▼ %d more line(s) below - PgDn or End to return ▼", below)) + "\n")
	}
	b.WriteString("\n" + m.input.View() + "\n")
	return tea.NewView(b.String())
}
