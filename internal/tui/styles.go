package tui

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// The Benchouse palette, kept subtle: ink is the terminal's own foreground,
// red marks the current step and selection, gray carries hints. Each colour
// has a lighter variant for dark terminals, where #a00000 is hard to read.
var (
	accent = lipgloss.AdaptiveColor{Light: "#a00000", Dark: "#e06666"}
	gray   = lipgloss.AdaptiveColor{Light: "#767676", Dark: "#9a9a9a"}
	rule   = lipgloss.AdaptiveColor{Light: "#cccccc", Dark: "#444444"}

	boldStyle   = lipgloss.NewStyle().Bold(true)
	accentStyle = lipgloss.NewStyle().Foreground(accent)
	accentBold  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	grayStyle   = lipgloss.NewStyle().Foreground(gray)
	ruleStyle   = lipgloss.NewStyle().Foreground(rule)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(rule).Padding(0, 1)
)

// theme is huh's base theme in the Benchouse palette.
func theme() *huh.Theme {
	t := huh.ThemeBase()
	t.Focused.Base = t.Focused.Base.BorderForeground(rule)
	t.Focused.Card = t.Focused.Base
	t.Focused.Title = t.Focused.Title.Bold(true)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Bold(true).MarginBottom(1)
	t.Focused.Description = t.Focused.Description.Foreground(gray)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(accent)
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(accent)
	t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(accent).SetString("❯ ")
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(accent)
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(accent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(accent)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#a00000"))
	t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(gray).Background(lipgloss.NoColor{})
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(accent)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(gray)

	t.Blurred = t.Focused
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.Title = t.Blurred.Title.Bold(false)
	t.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()
	t.Blurred.TextInput.Prompt = t.Blurred.TextInput.Prompt.Foreground(gray)
	return t
}
