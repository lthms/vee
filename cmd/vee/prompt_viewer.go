package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/term"
)

// PromptViewerCmd is the internal subcommand that displays the system prompt
// for the session in the current tmux window, rendered inside a tmux display-popup.
type PromptViewerCmd struct {
	Port     int    `short:"p" default:"2700" name:"port"`
	WindowID string `required:"" name:"window-id"`
}

type promptViewerState struct {
	lines      []string
	scroll     int
	termWidth  int
	termHeight int
	mode       string
	indicator  string
}

func (cmd *PromptViewerCmd) Run() error {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\033[?25l") // hide cursor
	defer fmt.Print("\033[?25h")

	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 40 {
		w = 90
	}
	if err != nil || h < 10 {
		h = 30
	}

	// Fetch the system prompt from the daemon
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/session/prompt?window=%s",
		cmd.Port, url.QueryEscape(cmd.WindowID)))
	if err != nil {
		printRawMessage(w, h, "Could not reach the daemon.")
		return waitForDismiss()
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		printRawMessage(w, h, "No session in this window.")
		return waitForDismiss()
	}
	if resp.StatusCode != http.StatusOK {
		printRawMessage(w, h, fmt.Sprintf("Daemon returned %d.", resp.StatusCode))
		return waitForDismiss()
	}

	var result struct {
		Mode         string `json:"mode"`
		Indicator    string `json:"indicator"`
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		printRawMessage(w, h, "Failed to decode response.")
		return waitForDismiss()
	}

	if result.SystemPrompt == "" {
		printRawMessage(w, h, "No system prompt stored for this session.")
		return waitForDismiss()
	}

	// Wrap lines to terminal width
	ps := &promptViewerState{
		lines:      wrapText(result.SystemPrompt, w-2),
		termWidth:  w,
		termHeight: h,
		mode:       result.Mode,
		indicator:  result.Indicator,
	}

	ps.render()

	// Input loop
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		key := buf[:n]

		maxScroll := ps.maxScroll()

		switch {
		case key[0] == 27 || key[0] == 'q':
			return nil
		case key[0] == 'j' || (len(key) == 3 && key[0] == 27 && key[1] == '[' && key[2] == 'B'):
			// Down
			if ps.scroll < maxScroll {
				ps.scroll++
				ps.render()
			}
		case key[0] == 'k' || (len(key) == 3 && key[0] == 27 && key[1] == '[' && key[2] == 'A'):
			// Up
			if ps.scroll > 0 {
				ps.scroll--
				ps.render()
			}
		case key[0] == 'd' && len(key) == 1:
			// Half-page down
			ps.scroll += ps.termHeight / 2
			if ps.scroll > maxScroll {
				ps.scroll = maxScroll
			}
			ps.render()
		case key[0] == 'u' && len(key) == 1:
			// Half-page up
			ps.scroll -= ps.termHeight / 2
			if ps.scroll < 0 {
				ps.scroll = 0
			}
			ps.render()
		case key[0] == 'g':
			// Top
			ps.scroll = 0
			ps.render()
		case key[0] == 'G':
			// Bottom
			ps.scroll = maxScroll
			ps.render()
		case len(key) == 4 && key[0] == 27 && key[1] == '[' && key[2] == '5' && key[3] == '~':
			// Page up
			ps.scroll -= ps.termHeight - 2
			if ps.scroll < 0 {
				ps.scroll = 0
			}
			ps.render()
		case len(key) == 4 && key[0] == 27 && key[1] == '[' && key[2] == '6' && key[3] == '~':
			// Page down
			ps.scroll += ps.termHeight - 2
			if ps.scroll > maxScroll {
				ps.scroll = maxScroll
			}
			ps.render()
		}
	}
}

func (ps *promptViewerState) maxScroll() int {
	// Reserve 2 lines for header, 1 for footer
	viewable := ps.termHeight - 3
	if viewable < 1 {
		viewable = 1
	}
	max := len(ps.lines) - viewable
	if max < 0 {
		return 0
	}
	return max
}

func (ps *promptViewerState) render() {
	var sb strings.Builder

	// Clear screen
	sb.WriteString("\033[2J\033[H")

	w := ps.termWidth
	viewable := ps.termHeight - 3 // header (2 lines) + footer (1 line)
	if viewable < 1 {
		viewable = 1
	}

	// Header
	title := fmt.Sprintf(" %s %s — System Prompt", ps.indicator, ps.mode)
	sb.WriteString("\033[1m\033[38;2;137;180;250m") // bold, accent blue
	sb.WriteString(truncateLine(title, w))
	sb.WriteString("\033[0m\r\n")
	sb.WriteString("\033[2m")
	sb.WriteString(strings.Repeat("─", w))
	sb.WriteString("\033[0m\r\n")

	// Content
	end := ps.scroll + viewable
	if end > len(ps.lines) {
		end = len(ps.lines)
	}
	visible := ps.lines[ps.scroll:end]
	for _, line := range visible {
		sb.WriteString(truncateLine(line, w))
		sb.WriteString("\r\n")
	}

	// Pad remaining space
	for range viewable - len(visible) {
		sb.WriteString("\r\n")
	}

	// Footer
	position := ""
	if len(ps.lines) > viewable {
		pct := 0
		if ps.maxScroll() > 0 {
			pct = ps.scroll * 100 / ps.maxScroll()
		}
		position = fmt.Sprintf(" %d%% ", pct)
	}
	footer := fmt.Sprintf(" j/k scroll  d/u half-page  g/G top/bottom  q quit%s", position)
	sb.WriteString("\033[2m")
	sb.WriteString(truncateLine(footer, w))
	sb.WriteString("\033[0m")

	fmt.Print(sb.String())
}

// wrapText splits text into lines, wrapping at maxWidth.
func wrapText(text string, maxWidth int) []string {
	if maxWidth < 1 {
		maxWidth = 1
	}

	rawLines := strings.Split(text, "\n")
	var result []string

	for _, line := range rawLines {
		if line == "" {
			result = append(result, "")
			continue
		}
		for len(line) > maxWidth {
			// Try to break at a space
			breakAt := maxWidth
			for i := maxWidth; i > maxWidth/2; i-- {
				if line[i] == ' ' {
					breakAt = i
					break
				}
			}
			result = append(result, line[:breakAt])
			line = line[breakAt:]
			if len(line) > 0 && line[0] == ' ' {
				line = line[1:]
			}
		}
		result = append(result, line)
	}

	return result
}

// truncateLine truncates a line to fit within maxWidth.
func truncateLine(line string, maxWidth int) string {
	runes := []rune(line)
	if len(runes) > maxWidth {
		return string(runes[:maxWidth])
	}
	return line
}

// printRawMessage prints a centered message in raw terminal mode.
func printRawMessage(w, h int, msg string) {
	fmt.Print("\033[2J\033[H") // clear screen
	row := h / 2
	col := (w - len(msg)) / 2
	if col < 0 {
		col = 0
	}
	fmt.Printf("\033[%d;%dH\033[2m%s\033[0m", row, col+1, msg)
}

// waitForDismiss waits for Esc or q in raw terminal mode.
func waitForDismiss() error {
	buf := make([]byte, 1)
	for {
		os.Stdin.Read(buf)
		if buf[0] == 27 || buf[0] == 'q' {
			return nil
		}
	}
}
