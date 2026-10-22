// Package theme defines tobira's colour palettes and the lipgloss styles
// derived from them. Every style is built from the palette; no colour is
// hard-coded outside the palette definitions.
package theme

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme is a named palette plus the styles derived from it.
type Theme struct {
	Name                                          string
	Bg, Fg, Accent, Muted, Danger, Warn, OK, Null color.Color
	Styles                                        Styles
}

// Styles are the ready-made styles views use.
type Styles struct {
	Pane, PaneFocused, Title, Header, Cell, CellCursor, Null, StatusBar, Badge, Error, Dim lipgloss.Style
}

// palette is the set of raw colours a theme is built from.
type palette struct {
	name                                          string
	bg, fg, accent, muted, danger, warn, ok, null string
	surface                                       string // status bar and header background
	accentText                                    string // accent as text; darker on light so it stays readable
	cursorFg                                      string // text on the accent-filled cursor cell
}

var darkPalette = palette{
	name: "tobira-dark",
	bg:   "#0B1220", fg: "#E8ECF4", accent: "#5EEAD4", muted: "#94A3B8",
	danger: "#F87171", warn: "#FBBF24", ok: "#4ADE80", null: "#94A3B8",
	surface: "#162033", accentText: "#5EEAD4", cursorFg: "#0B1220",
}

var lightPalette = palette{
	name: "tobira-light",
	bg:   "#F8FAFC", fg: "#0F172A", accent: "#5EEAD4", muted: "#475569",
	danger: "#B91C1C", warn: "#B45309", ok: "#15803D", null: "#475569",
	surface: "#E2E8F0", accentText: "#115E59", cursorFg: "#0F172A",
}

// Dark returns the tobira-dark theme.
func Dark() Theme { return build(darkPalette) }

// Light returns the tobira-light theme, derived from the same accent.
func Light() Theme { return build(lightPalette) }

// Names lists the theme names ByName accepts. internal/config keeps its own
// copy (it must not import the TUI); a test keeps the two in step.
func Names() []string { return []string{"auto", "tobira-dark", "tobira-light"} }

// ByName resolves a theme name: auto (dark or light by the terminal
// background), tobira-dark or tobira-light.
func ByName(name string, darkBackground bool) (Theme, error) {
	switch name {
	case "auto":
		if darkBackground {
			return Dark(), nil
		}
		return Light(), nil
	case "tobira-dark":
		return Dark(), nil
	case "tobira-light":
		return Light(), nil
	}
	return Theme{}, fmt.Errorf("unknown theme %q (want auto, tobira-dark or tobira-light)", name)
}

func build(p palette) Theme {
	c := lipgloss.Color
	t := Theme{
		Name: p.name,
		Bg:   c(p.bg), Fg: c(p.fg), Accent: c(p.accent), Muted: c(p.muted),
		Danger: c(p.danger), Warn: c(p.warn), OK: c(p.ok), Null: c(p.null),
	}
	surface := c(p.surface)
	accentText := c(p.accentText)
	base := lipgloss.NewStyle().Foreground(t.Fg)
	t.Styles = Styles{
		Pane:        base.Border(lipgloss.RoundedBorder()).BorderForeground(t.Muted),
		PaneFocused: base.Border(lipgloss.RoundedBorder()).BorderForeground(accentText),
		Title:       lipgloss.NewStyle().Foreground(accentText).Bold(true),
		Header:      lipgloss.NewStyle().Foreground(accentText).Background(surface).Bold(true),
		Cell:        base,
		CellCursor:  lipgloss.NewStyle().Foreground(c(p.cursorFg)).Background(t.Accent),
		Null:        lipgloss.NewStyle().Foreground(t.Muted).Italic(true),
		StatusBar:   lipgloss.NewStyle().Foreground(t.Fg).Background(surface),
		Badge:       lipgloss.NewStyle().Foreground(t.Bg).Background(t.Warn).Bold(true).Padding(0, 1),
		Error:       lipgloss.NewStyle().Foreground(t.Danger),
		Dim:         lipgloss.NewStyle().Foreground(t.Muted),
	}
	return t
}
