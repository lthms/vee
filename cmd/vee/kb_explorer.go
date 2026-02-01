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
	Path         string `json:"path"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	LastVerified string `json:"last_verified"`
}

const (
	explorerStateSearch  = 0
	explorerStateViewing = 1
)

var (
	// wikiLinkRe matches [[wiki-links]] in note content.
	wikiLinkRe = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
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
	noteStack  []noteView // navigation stack
	noteLines  []string   // rendered lines of current note
	noteScroll int        // top line offset
	links      []string   // wiki-link titles extracted from current note
	linkIdx    int        // currently focused link (-1 = none)

	termWidth  int
	termHeight int
}

type noteView struct {
	title string
	path  string
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
		linkIdx:    -1,
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
				es.openNote(es.results[es.selected].Path, es.results[es.selected].Title)
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
		case 9: // Tab — next link
			if len(es.links) > 0 {
				es.linkIdx = (es.linkIdx + 1) % len(es.links)
			}
		case 10, 13: // Enter — follow focused link
			if es.linkIdx >= 0 && es.linkIdx < len(es.links) {
				es.followLink(es.links[es.linkIdx])
			}
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
	} else if len(input) == 4 && input[0] == 27 && input[1] == 91 && input[2] == 90 {
		// Shift-Tab (CSI Z)
		if len(es.links) > 0 {
			es.linkIdx--
			if es.linkIdx < 0 {
				es.linkIdx = len(es.links) - 1
			}
		}
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

func (es *explorerState) openNote(path, title string) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/kb/fetch?path=%s", es.port, url.QueryEscape(path)))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}

	es.noteStack = append(es.noteStack, noteView{title: title, path: path})
	es.prepareNoteView(sb.String())
	es.state = explorerStateViewing
}

// renderInlineMarkdown applies ANSI styling for bold, italic, and code spans.
func renderInlineMarkdown(line string) string {
	line = codeSpanRe.ReplaceAllString(line, ansiCyan+"$1"+ansiReset)
	line = boldRe.ReplaceAllString(line, ansiBold+"$1"+ansiReset)
	line = italicRe.ReplaceAllString(line, ansiItalic+"$1"+ansiReset)
	return line
}

func (es *explorerState) prepareNoteView(raw string) {
	body, meta := parseFrontmatter(raw)
	contentWidth := es.termWidth - 6

	var lines []string
	inCodeBlock := false

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			lines = append(lines, "")
			continue
		}

		if inCodeBlock {
			lines = append(lines, ansiDim+line+ansiReset)
			continue
		}

		// Headers
		if strings.HasPrefix(line, "### ") {
			text := strings.TrimPrefix(line, "### ")
			lines = append(lines, ansiItalic+ansiBold+text+ansiReset)
			continue
		}
		if strings.HasPrefix(line, "## ") {
			text := strings.TrimPrefix(line, "## ")
			lines = append(lines, ansiBold+text+ansiReset)
			continue
		}
		if strings.HasPrefix(line, "# ") {
			text := strings.TrimPrefix(line, "# ")
			lines = append(lines, ansiBold+ansiAccent+text+ansiReset)
			continue
		}

		// Horizontal rules
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			lines = append(lines, ansiDim+strings.Repeat("─", contentWidth)+ansiReset)
			continue
		}

		// Regular lines: wrap then apply inline formatting
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
	if meta.tags != "" {
		lines = append(lines, ansiMuted+"Tags: "+meta.tags+ansiReset)
	}
	if meta.sources != "" {
		lines = append(lines, ansiMuted+"Sources: "+meta.sources+ansiReset)
	}
	if meta.lastVerified != "" {
		lines = append(lines, ansiMuted+"Last verified: "+meta.lastVerified+ansiReset)
	}

	es.noteLines = lines
	es.noteScroll = 0

	// Extract wiki-links from raw body (before markdown rendering)
	es.links = nil
	es.linkIdx = -1
	matches := wikiLinkRe.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) > 1 && !seen[m[1]] {
			es.links = append(es.links, m[1])
			seen[m[1]] = true
		}
	}
	if len(es.links) > 0 {
		es.linkIdx = 0
	}
}

type noteMeta struct {
	tags         string
	sources      string
	lastVerified string
}

// parseFrontmatter strips YAML frontmatter and returns the body and parsed metadata.
func parseFrontmatter(raw string) (string, noteMeta) {
	var meta noteMeta

	if !strings.HasPrefix(raw, "---\n") {
		return raw, meta
	}

	end := strings.Index(raw[4:], "\n---\n")
	if end < 0 {
		return raw, meta
	}

	frontmatter := raw[4 : 4+end]
	body := strings.TrimLeft(raw[4+end+5:], "\n")

	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "tags:") {
			meta.tags = strings.TrimSpace(strings.TrimPrefix(line, "tags:"))
			// Clean up YAML array syntax
			meta.tags = strings.Trim(meta.tags, "[]")
			meta.tags = strings.ReplaceAll(meta.tags, "\"", "")
		} else if strings.HasPrefix(line, "last_verified:") {
			meta.lastVerified = strings.TrimSpace(strings.TrimPrefix(line, "last_verified:"))
		} else if strings.HasPrefix(line, "sources:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "sources:"))
			if val != "" && val != "[]" {
				meta.sources = strings.Trim(val, "[]")
				meta.sources = strings.ReplaceAll(meta.sources, "\"", "")
			}
		} else if strings.HasPrefix(line, "- ") && meta.sources == "" {
			// Multi-line sources list — collect
		}
	}

	// Re-parse for multi-line sources
	inSources := false
	var sourceItems []string
	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "sources:") {
			inSources = true
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "sources:"))
			if val != "" && val != "[]" && !strings.HasPrefix(val, "[") {
				sourceItems = append(sourceItems, strings.Trim(val, "\""))
			}
			continue
		}
		if inSources {
			if strings.HasPrefix(trimmed, "- ") {
				item := strings.TrimPrefix(trimmed, "- ")
				item = strings.Trim(item, "\"")
				sourceItems = append(sourceItems, item)
			} else {
				inSources = false
			}
		}
	}
	if len(sourceItems) > 0 {
		meta.sources = strings.Join(sourceItems, ", ")
	}

	return body, meta
}

func (es *explorerState) popNote() {
	if len(es.noteStack) > 1 {
		// Pop current, re-open previous
		es.noteStack = es.noteStack[:len(es.noteStack)-1]
		prev := es.noteStack[len(es.noteStack)-1]
		es.noteStack = es.noteStack[:len(es.noteStack)-1]
		es.openNote(prev.path, prev.title)
	} else {
		// Back to search results
		es.noteStack = nil
		es.noteLines = nil
		es.state = explorerStateSearch
	}
}

func (es *explorerState) followLink(title string) {
	// Find this title in the results first
	for _, r := range es.results {
		if r.Title == title {
			es.openNote(r.Path, r.Title)
			return
		}
	}
	// Otherwise, search for it
	path := sanitizeLinkToPath(title)
	es.openNote(path, title)
}

// sanitizeLinkToPath converts a wiki-link title to a vault file path.
func sanitizeLinkToPath(title string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-",
		"*", "", "?", "", "\"", "",
		"<", "", ">", "", "|", "",
	)
	name := strings.TrimSpace(replacer.Replace(title))
	if name == "" {
		name = "untitled"
	}
	return name + ".md"
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
			// header(3) + search(2) + footer(3) = 8 lines overhead, each result = 3 lines
			maxVisible := (es.termHeight - 8) / 3
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

				// Title (left) + date (right)
				title := r.Title
				date := formatVerifiedDate(r.LastVerified)
				maxTitle := w - 6 - len(date) - 2
				if maxTitle > 0 && len(title) > maxTitle {
					title = title[:maxTitle-3] + "..."
				}

				if i == es.selected {
					sb.WriteString(ansiBold)
				}
				sb.WriteString(title)
				if i == es.selected {
					sb.WriteString(ansiReset)
				}

				// Right-align date
				padding := w - 4 - len(title) - len(date) - 2
				if padding < 2 {
					padding = 2
				}
				sb.WriteString(strings.Repeat(" ", padding))
				sb.WriteString(ansiMuted)
				sb.WriteString(date)
				sb.WriteString(ansiReset)
				sb.WriteString("\r\n")

				// Summary line
				if r.Summary != "" {
					summary := r.Summary
					maxSummary := w - 6
					if len(summary) > maxSummary {
						summary = summary[:maxSummary-3] + "..."
					}
					sb.WriteString("    ")
					sb.WriteString(ansiDim)
					sb.WriteString(summary)
					sb.WriteString(ansiReset)
				}
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

	// Header: back nav + title
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("← Esc back")
	sb.WriteString(ansiReset)

	current := es.noteStack[len(es.noteStack)-1]
	title := current.title
	maxTitle := w - 16
	if maxTitle > 0 && len(title) > maxTitle {
		title = title[:maxTitle-3] + "..."
	}
	padding := w - 12 - len(title) - 2
	if padding < 2 {
		padding = 2
	}
	sb.WriteString(strings.Repeat(" ", padding))
	sb.WriteString(ansiBold)
	sb.WriteString(ansiAccent)
	sb.WriteString(title)
	sb.WriteString(ansiReset)
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
		line := es.noteLines[i]

		// Style wiki-links: focused gets bold, others get accent color
		line = wikiLinkRe.ReplaceAllStringFunc(line, func(match string) string {
			inner := match[2 : len(match)-2]
			if es.linkIdx >= 0 && es.linkIdx < len(es.links) && inner == es.links[es.linkIdx] {
				return ansiBold + ansiAccent + inner + ansiReset
			}
			return ansiAccent + inner + ansiReset
		})

		sb.WriteString("   ")
		sb.WriteString(line)
		sb.WriteString("\r\n")
	}

	// Pad remaining lines
	for i := end - start; i < visibleLines; i++ {
		sb.WriteString("\r\n")
	}

	// Footer
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	if len(es.links) > 0 {
		sb.WriteString("Tab/S-Tab links  ")
	}
	sb.WriteString("↑↓/jk scroll  Enter follow  Esc back  q quit")
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
