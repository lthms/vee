package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"
)

// KBExplorerCmd is the internal subcommand that shows an interactive
// knowledge base browser, rendered inside a tmux display-popup.
type KBExplorerCmd struct {
	Port int `short:"p" default:"2700" name:"port"`
}

// explorerResult is a single search hit from the KB query API.
type explorerResult struct {
	ID           string  `json:"id"`
	Content      string  `json:"content"`
	Source       string  `json:"source"`
	Score        float64 `json:"score"`
	LastVerified string  `json:"last_verified"`
}

// explorerStatement is a full statement returned by the KB fetch API.
type explorerStatement struct {
	ID           string `json:"id"`
	Content      string `json:"content"`
	Source       string `json:"source"`
	SourceType   string `json:"source_type"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	LastVerified string `json:"last_verified"`
}

const (
	explorerStateSearch  = 0
	explorerStateViewing = 1
)

var (
	boldRe     = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRe   = regexp.MustCompile(`\*([^*]+)\*`)
	codeSpanRe = regexp.MustCompile("`([^`]+)`")
)

type explorerState struct {
	port int

	state    int // explorerStateSearch or explorerStateViewing
	query    string
	results  []explorerResult
	selected int
	searched bool // true after at least one search has been performed

	// Note viewing state
	noteStack  []noteView         // navigation stack
	noteStmt   *explorerStatement // current statement being viewed
	noteLines  []string           // rendered lines of current note
	noteScroll int                // top line offset

	termWidth  int
	termHeight int
}

type noteView struct {
	id string
}

func (cmd *KBExplorerCmd) Run() error {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\033[?25h") // show cursor
	defer fmt.Print("\033[?25h")

	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 40 {
		w = 90
	}
	if err != nil || h < 10 {
		h = 30
	}

	es := &explorerState{
		port:       cmd.Port,
		termWidth:  w,
		termHeight: h,
	}

	es.render()

	inputCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(inputCh)
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			inputCh <- data
		}
	}()

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case input, ok := <-inputCh:
			if !ok {
				return nil
			}

			prevQuery := es.query

			if es.state == explorerStateSearch {
				if es.handleSearchInput(input) {
					return nil
				}
				if es.query != prevQuery {
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					if es.query != "" {
						debounceTimer = time.NewTimer(500 * time.Millisecond)
						debounceCh = debounceTimer.C
					} else {
						debounceCh = nil
						debounceTimer = nil
						es.results = nil
						es.searched = false
					}
				}
			} else {
				if es.handleViewInput(input) {
					return nil
				}
			}

			es.render()

		case <-debounceCh:
			debounceCh = nil
			debounceTimer = nil
			if es.state == explorerStateSearch && es.query != "" {
				es.search()
				es.render()
			}
		}
	}
}

// handleSearchInput processes input in the search state.
// Returns true if the explorer should exit.
func (es *explorerState) handleSearchInput(input []byte) bool {
	if len(input) == 1 {
		switch input[0] {
		case 27: // Esc
			return true
		case 10, 13: // Enter — open selected result
			if len(es.results) > 0 && es.selected >= 0 && es.selected < len(es.results) {
				es.openNote(es.results[es.selected].ID)
			}
		case 127, 8: // Backspace
			if len(es.query) > 0 {
				es.query = es.query[:len(es.query)-1]
			}
		case 21: // C-u — clear query
			es.query = ""
		case 23: // C-w — delete last word
			i := len(es.query)
			for i > 0 && es.query[i-1] == ' ' {
				i--
			}
			for i > 0 && es.query[i-1] != ' ' {
				i--
			}
			es.query = es.query[:i]
		default:
			if input[0] >= 32 && input[0] < 127 {
				es.query += string(input[0])
			}
		}
	} else if len(input) == 3 && input[0] == 27 && input[1] == 91 {
		switch input[2] {
		case 65: // Up
			es.moveSelection(-1)
		case 66: // Down
			es.moveSelection(1)
		}
	} else if len(input) == 2 && input[0] == 27 {
		// Esc + char — treat as Esc
		return true
	}
	return false
}

// handleViewInput processes input in the note viewing state.
// Returns true if the explorer should exit.
func (es *explorerState) handleViewInput(input []byte) bool {
	if len(input) == 1 {
		switch input[0] {
		case 'q':
			return true
		case 27: // Esc
			es.popNote()
		case 'j':
			es.scrollNote(1)
		case 'k':
			es.scrollNote(-1)
		}
	} else if len(input) == 3 && input[0] == 27 && input[1] == 91 {
		switch input[2] {
		case 65: // Up
			es.scrollNote(-1)
		case 66: // Down
			es.scrollNote(1)
		}
	} else if len(input) == 2 && input[0] == 27 {
		if input[1] == '[' {
			// Incomplete escape — ignore
			return false
		}
		// Esc + char — treat as Esc
		es.popNote()
	}
	return false
}

func (es *explorerState) moveSelection(delta int) {
	if len(es.results) == 0 {
		return
	}
	es.selected += delta
	if es.selected < 0 {
		es.selected = 0
	}
	if es.selected >= len(es.results) {
		es.selected = len(es.results) - 1
	}
}

func (es *explorerState) scrollNote(delta int) {
	es.noteScroll += delta
	if es.noteScroll < 0 {
		es.noteScroll = 0
	}
	maxScroll := len(es.noteLines) - (es.termHeight - 6) // reserve header + footer
	if maxScroll < 0 {
		maxScroll = 0
	}
	if es.noteScroll > maxScroll {
		es.noteScroll = maxScroll
	}
}

func (es *explorerState) search() {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/kb/query?q=%s", es.port, url.QueryEscape(es.query)))
	if err != nil {
		es.results = nil
		es.searched = true
		return
	}
	defer resp.Body.Close()

	var results []explorerResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		es.results = nil
		es.searched = true
		return
	}

	es.results = results
	es.selected = 0
	es.searched = true
}

func (es *explorerState) openNote(id string) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/kb/fetch?id=%s", es.port, url.QueryEscape(id)))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var stmt explorerStatement
	if err := json.NewDecoder(resp.Body).Decode(&stmt); err != nil {
		return
	}

	es.noteStack = append(es.noteStack, noteView{id: id})
	es.noteStmt = &stmt
	es.prepareNoteView(&stmt)
	es.state = explorerStateViewing
}

// renderInlineMarkdown applies ANSI styling for bold, italic, and code spans.
func renderInlineMarkdown(line string) string {
	line = codeSpanRe.ReplaceAllString(line, ansiCyan+"$1"+ansiReset)
	line = boldRe.ReplaceAllString(line, ansiBold+"$1"+ansiReset)
	line = italicRe.ReplaceAllString(line, ansiItalic+"$1"+ansiReset)
	return line
}

func (es *explorerState) prepareNoteView(stmt *explorerStatement) {
	contentWidth := es.termWidth - 6

	var lines []string

	// Render content with word wrapping
	for _, line := range strings.Split(stmt.Content, "\n") {
		if len(line) <= contentWidth {
			lines = append(lines, renderInlineMarkdown(line))
		} else {
			for _, wrapped := range wrapLine(line, contentWidth) {
				lines = append(lines, renderInlineMarkdown(wrapped))
			}
		}
	}

	// Metadata footer
	lines = append(lines, "")
	lines = append(lines, ansiDim+strings.Repeat("─", contentWidth)+ansiReset)
	if stmt.Source != "" {
		lines = append(lines, ansiMuted+"Source: "+stmt.Source+ansiReset)
	}
	if stmt.LastVerified != "" {
		lines = append(lines, ansiMuted+"Verified: "+stmt.LastVerified+ansiReset)
	}
	if stmt.CreatedAt != "" {
		lines = append(lines, ansiMuted+"Created: "+stmt.CreatedAt+ansiReset)
	}

	es.noteLines = lines
	es.noteScroll = 0
}

func (es *explorerState) popNote() {
	if len(es.noteStack) > 1 {
		// Pop current, re-open previous
		es.noteStack = es.noteStack[:len(es.noteStack)-1]
		prev := es.noteStack[len(es.noteStack)-1]
		es.noteStack = es.noteStack[:len(es.noteStack)-1]
		es.openNote(prev.id)
	} else {
		// Back to search results
		es.noteStack = nil
		es.noteStmt = nil
		es.noteLines = nil
		es.state = explorerStateSearch
	}
}

// wrapLine wraps a long line at word boundaries to fit within maxWidth.
func wrapLine(line string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{line}
	}

	var result []string
	for len(line) > maxWidth {
		// Find last space before maxWidth
		cut := maxWidth
		for cut > 0 && line[cut] != ' ' {
			cut--
		}
		if cut == 0 {
			cut = maxWidth // no space found, hard break
		}
		result = append(result, line[:cut])
		line = strings.TrimLeft(line[cut:], " ")
	}
	if line != "" {
		result = append(result, line)
	}
	return result
}

// render draws the current state to the terminal.
func (es *explorerState) render() {
	var sb strings.Builder

	// Clear screen, cursor home
	sb.WriteString("\033[2J\033[H")

	if es.state == explorerStateSearch {
		es.renderSearch(&sb)
	} else {
		es.renderNote(&sb)
	}

	fmt.Print(sb.String())
}

func (es *explorerState) renderSearch(sb *strings.Builder) {
	w := es.termWidth

	// Header
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiAccent)
	sb.WriteString(ansiBold)
	sb.WriteString("Knowledge Base")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n\r\n")

	// Search input
	sb.WriteString("  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Search: ")
	sb.WriteString(ansiReset)
	sb.WriteString(es.query)
	sb.WriteString("\033[s") // save cursor
	sb.WriteString("\r\n\r\n")

	if es.searched {
		if len(es.results) == 0 {
			sb.WriteString("  ")
			sb.WriteString(ansiMuted)
			sb.WriteString(ansiItalic)
			sb.WriteString("No results")
			sb.WriteString(ansiReset)
			sb.WriteString("\r\n")
		} else {
			// Calculate visible results based on terminal height
			// header(3) + search(2) + footer(3) = 8 lines overhead, each result = 2 lines
			maxVisible := (es.termHeight - 8) / 2
			if maxVisible < 1 {
				maxVisible = 1
			}
			if maxVisible > len(es.results) {
				maxVisible = len(es.results)
			}

			// Determine the visible window around selected
			start := 0
			if es.selected >= maxVisible {
				start = es.selected - maxVisible + 1
			}
			end := start + maxVisible
			if end > len(es.results) {
				end = len(es.results)
				start = end - maxVisible
				if start < 0 {
					start = 0
				}
			}

			for i := start; i < end; i++ {
				r := es.results[i]

				// Selection indicator
				if i == es.selected {
					sb.WriteString("  ")
					sb.WriteString(ansiAccent)
					sb.WriteString("▸")
					sb.WriteString(ansiReset)
					sb.WriteString(" ")
				} else {
					sb.WriteString("    ")
				}

				// Content preview: first line only + date (right)
				preview := r.Content
				if nl := strings.IndexByte(preview, '\n'); nl >= 0 {
					preview = preview[:nl]
				}
				date := formatVerifiedDate(r.LastVerified)
				maxPreview := w - 6 - len(date) - 2
				if maxPreview > 0 && len(preview) > maxPreview {
					preview = preview[:maxPreview-3] + "..."
				}

				if i == es.selected {
					sb.WriteString(ansiBold)
				}
				sb.WriteString(preview)
				if i == es.selected {
					sb.WriteString(ansiReset)
				}

				// Right-align date
				padding := w - 4 - len(preview) - len(date) - 2
				if padding < 2 {
					padding = 2
				}
				sb.WriteString(strings.Repeat(" ", padding))
				sb.WriteString(ansiMuted)
				sb.WriteString(date)
				sb.WriteString(ansiReset)
				sb.WriteString("\r\n\r\n")
			}
		}
	}

	// Footer
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("↑↓ navigate  Enter open  Esc quit")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n")

	// Restore cursor to search input
	sb.WriteString("\033[u\033[?25h")
}

func (es *explorerState) renderNote(sb *strings.Builder) {
	w := es.termWidth

	// Header: back nav + source label
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("← Esc back")
	sb.WriteString(ansiReset)

	if es.noteStmt != nil && es.noteStmt.Source != "" {
		label := es.noteStmt.Source
		maxLabel := w - 16
		if maxLabel > 0 && len(label) > maxLabel {
			label = label[:maxLabel-3] + "..."
		}
		padding := w - 12 - len(label) - 2
		if padding < 2 {
			padding = 2
		}
		sb.WriteString(strings.Repeat(" ", padding))
		sb.WriteString(ansiMuted)
		sb.WriteString(label)
		sb.WriteString(ansiReset)
	}
	sb.WriteString("\r\n\r\n")

	// Note content (scrolled)
	visibleLines := es.termHeight - 6 // header(3) + footer(3)
	if visibleLines < 1 {
		visibleLines = 1
	}

	start := es.noteScroll
	end := start + visibleLines
	if end > len(es.noteLines) {
		end = len(es.noteLines)
	}

	for i := start; i < end; i++ {
		sb.WriteString("   ")
		sb.WriteString(es.noteLines[i])
		sb.WriteString("\r\n")
	}

	// Pad remaining lines
	for i := end - start; i < visibleLines; i++ {
		sb.WriteString("\r\n")
	}

	// Footer
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("↑↓/jk scroll  Esc back  q quit")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n")

	// Hide cursor in note view
	sb.WriteString("\033[?25l")
}

// formatVerifiedDate formats a last_verified date for display.
// Extracts just the month and day parts (e.g., "Feb 01").
func formatVerifiedDate(dateStr string) string {
	if dateStr == "" {
		return ""
	}
	// dateStr is in YYYY-MM-DD format
	parts := strings.Split(dateStr, "-")
	if len(parts) != 3 {
		return dateStr
	}
	months := map[string]string{
		"01": "Jan", "02": "Feb", "03": "Mar", "04": "Apr",
		"05": "May", "06": "Jun", "07": "Jul", "08": "Aug",
		"09": "Sep", "10": "Oct", "11": "Nov", "12": "Dec",
	}
	month, ok := months[parts[1]]
	if !ok {
		return dateStr
	}
	return month + " " + parts[2]
}
