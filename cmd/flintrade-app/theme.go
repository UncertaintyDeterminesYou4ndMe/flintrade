package main

import "github.com/egoist/mygo/ui"

// A market-data look (after Google Finance): near-black or white page,
// soft cards, large regular-weight type, gains green and losses red with
// arrow chips, sparklines with a fading area fill over a dotted baseline.
// Success / Danger carry up / down; Accent is the interactive blue.
var (
	financeDark = finance(ui.DarkTheme(), palette{
		bg: "#131314", surface: "#1e1f20", hover: "#28292b", pressed: "#333537", border: "#2f3133",
		text: "#e3e3e3", muted: "#9aa0a6", accent: "#a8c7fa", accentText: "#062e6f",
		up: "#81c995", down: "#f28b82",
	})
	financeLight = finance(ui.LightTheme(), palette{
		bg: "#ffffff", surface: "#f6f8fc", hover: "#eef1f6", pressed: "#e3e7ee", border: "#e2e5ea",
		text: "#1f1f1f", muted: "#5f6368", accent: "#0b57d0", accentText: "#ffffff",
		up: "#137333", down: "#c5221f",
	})
)

type palette struct {
	bg, surface, hover, pressed, border, text, muted, accent, accentText, up, down string
}

func finance(base *ui.Theme, p palette) *ui.Theme {
	t := *base
	t.Background, t.Surface, t.SurfaceHover, t.SurfacePressed = ui.Hex(p.bg), ui.Hex(p.surface), ui.Hex(p.hover), ui.Hex(p.pressed)
	t.Border, t.Text, t.TextMuted = ui.Hex(p.border), ui.Hex(p.text), ui.Hex(p.muted)
	t.Accent, t.AccentText = ui.Hex(p.accent), ui.Hex(p.accentText)
	t.AccentHover, t.AccentPressed = t.Accent.Mix(t.Text, 0.12), t.Accent.Mix(t.Text, 0.24)
	t.Success, t.Danger, t.Warning = ui.Hex(p.up), ui.Hex(p.down), ui.Hex(p.down)
	t.Selection = t.Accent.Alpha(0.3)
	t.Focus = t.Accent.Alpha(0.6)
	t.Radius = 8
	return &t
}

// useTheme follows the system's appearance.
func useTheme(c *ui.Context) *ui.Theme {
	t := financeLight
	if c.Theme().Dark {
		t = financeDark
	}
	c.SetTheme(t)
	return t
}

// pnlColor: up green, down red, flat ink.
func pnlColor(t *ui.Theme, v float64) ui.Color {
	switch {
	case v > 0:
		return t.Success
	case v < 0:
		return t.Danger
	}
	return t.Text
}

var neutralLight, neutralDark = neutral(financeLight), neutral(financeDark)

// neutral is the theme for tables: a selected row is a grey band, not a
// colour that could read as up or down.
func neutral(base *ui.Theme) *ui.Theme {
	t := *base
	t.Accent = base.SurfacePressed
	t.AccentHover, t.AccentPressed, t.InactiveAccent = t.Accent, t.Accent, t.Accent
	t.AccentText = base.Text
	return &t
}

func tables(c *ui.Context, fn func()) ui.Element {
	t := neutralLight
	if c.Theme().Dark {
		t = neutralDark
	}
	return ui.Themed(c, t).Grow(1).Children(fn)
}
