package session

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Listing is every session and preset a user can pick, in display order:
// user-named sessions (most recently played first), presets
// (alphabetical), then auto-named sessions (newest first).
type Listing struct {
	Named   []*Session
	Presets []*Preset
	Auto    []*Session
}

// Listing groups the store's sessions and the visible presets.
func (st *Store) Listing() (Listing, error) {
	sessions, err := st.List()
	if err != nil {
		return Listing{}, err
	}
	return Group(sessions, st.Presets()), nil
}

// Group sorts sessions and presets into a Listing.
func Group(sessions []*Session, presets []*Preset) Listing {
	var l Listing
	for _, s := range sessions {
		if s.AutoNamed() {
			l.Auto = append(l.Auto, s)
		} else {
			l.Named = append(l.Named, s)
		}
	}
	sort.SliceStable(l.Named, func(i, j int) bool { return l.Named[i].Played().After(l.Named[j].Played()) })
	sort.SliceStable(l.Auto, func(i, j int) bool { return l.Auto[i].Created.After(l.Auto[j].Created) })
	l.Presets = append([]*Preset(nil), presets...)
	sort.Slice(l.Presets, func(i, j int) bool { return l.Presets[i].Name < l.Presets[j].Name })
	return l
}

// Group labels shared by every listing surface.
const (
	LabelNamed   = "your sessions"
	LabelPresets = "presets"
	LabelAuto    = "auto-saved sessions"
)

// Summary is a one-line description of a session's sound, truncated to
// max characters.
func Summary(s *Session, max int) string {
	return truncate(s.Describe(), max)
}

func truncate(text string, max int) string {
	if max <= 3 || len(text) <= max {
		return text
	}
	return text[:max-3] + "..."
}

// Ago renders how long ago t was, compactly.
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// Render draws the listing as aligned text: name, summary, last played.
func (l Listing) Render(now time.Time) string {
	var b strings.Builder
	row := func(name, summary, when string) {
		line := fmt.Sprintf("  %-24s %-44s %s", truncate(name, 24), truncate(summary, 44), when)
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	b.WriteString(LabelNamed + ":\n")
	if len(l.Named) == 0 {
		b.WriteString("  (none yet; in the player, type: name <something>)\n")
	}
	for _, s := range l.Named {
		row(s.Name, s.Describe(), Ago(now, s.Played()))
	}
	b.WriteString(LabelPresets + ":\n")
	if len(l.Presets) == 0 {
		b.WriteString("  (all deleted; iar sessions restore-presets brings them back)\n")
	}
	for _, p := range l.Presets {
		row(p.Name, p.Description, "")
	}
	b.WriteString(LabelAuto + ":\n")
	if len(l.Auto) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, s := range l.Auto {
		row(s.Name, s.Describe(), Ago(now, s.Played()))
	}
	return strings.TrimRight(b.String(), " \n")
}
