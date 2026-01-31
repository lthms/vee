package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#24283b")).
			Foreground(lipgloss.Color("#a9b1d6"))

	activeTabStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#7aa2f7")).
			Foreground(lipgloss.Color("#1a1b26")).
			Padding(0, 1)

	inactiveTabStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#24283b")).
				Foreground(lipgloss.Color("#a9b1d6")).
				Padding(0, 1)

	deadTabStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#24283b")).
			Foreground(lipgloss.Color("#565f89")).
			Padding(0, 1)

	helpHintStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#24283b")).
			Foreground(lipgloss.Color("#565f89"))

	commandModeStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#e0af68")).
				Foreground(lipgloss.Color("#1a1b26")).
				Padding(0, 1)
)

// renderStatusBar renders the status bar for the given panes and state.
func renderStatusBar(panes []*Pane, activeIdx int, showingLogs bool, commandMode bool, width int) string {
	var tabs []string

	for i, p := range panes {
		label := fmt.Sprintf("%d:%s %s", i, p.Mode.Indicator, p.Mode.Name)
		var style lipgloss.Style
		if !showingLogs && i == activeIdx {
			style = activeTabStyle
		} else if !p.isAlive() {
			label += " (dead)"
			style = deadTabStyle
		} else {
			style = inactiveTabStyle
		}
		tabs = append(tabs, style.Render(label))
	}

	// Log viewer tab
	logLabel := "L:📋 logs"
	if showingLogs {
		tabs = append(tabs, activeTabStyle.Render(logLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(logLabel))
	}

	left := strings.Join(tabs, "")

	var right string
	if commandMode {
		right = commandModeStyle.Render("COMMAND")
	} else {
		right = helpHintStyle.Render("C-b ? help")
	}

	// Pad the middle to fill the width
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := width - leftWidth - rightWidth
	if gap < 0 {
		gap = 0
	}

	bar := left + statusBarStyle.Render(strings.Repeat(" ", gap)) + right

	return statusBarStyle.Width(width).Render(bar)
}
