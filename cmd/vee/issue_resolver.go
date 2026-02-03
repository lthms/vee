package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/term"
)

// IssueResolverCmd is the internal subcommand that shows an interactive
// issue resolver, rendered inside a tmux display-popup.
type IssueResolverCmd struct {
	Port int `short:"p" default:"2700" name:"port"`
}

// resolverIssue mirrors kb.Issue for JSON decoding.
type resolverIssue struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	StatementA string  `json:"statement_a"`
	StatementB string  `json:"statement_b"`
	Score      float64 `json:"score"`
	CreatedAt  string  `json:"created_at"`
	ContentA   string  `json:"content_a"`
	ContentB   string  `json:"content_b"`
	SourceA    string  `json:"source_a"`
	SourceB    string  `json:"source_b"`
}

const (
	resolverStateList   = 0
	resolverStateDetail = 1
)

type resolverState struct {
	port int

	state    int // resolverStateList or resolverStateDetail
	issues   []resolverIssue
	selected int
	message  string // transient status message

	// Detail view
	detailLines  []string
	detailScroll int

	termWidth  int
	termHeight int
}

func (cmd *IssueResolverCmd) Run() error {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\033[?25l") // hide cursor
	defer fmt.Print("\033[?25h")

	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 40 {
		w = 100
	}
	if err != nil || h < 10 {
		h = 40
	}

	rs := &resolverState{
		port:       cmd.Port,
		termWidth:  w,
		termHeight: h,
	}

	rs.fetchIssues()
	rs.render()

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

	for {
		input, ok := <-inputCh
		if !ok {
			return nil
		}

		if rs.state == resolverStateList {
			if rs.handleListInput(input) {
				return nil
			}
		} else {
			if rs.handleDetailInput(input) {
				return nil
			}
		}

		rs.render()
	}
}

// handleListInput processes input in the list view.
// Returns true if the resolver should exit.
func (rs *resolverState) handleListInput(input []byte) bool {
	if len(input) == 1 {
		switch input[0] {
		case 'q', 27: // q or Esc
			return true
		case 10, 13: // Enter — open detail
			if len(rs.issues) > 0 && rs.selected >= 0 && rs.selected < len(rs.issues) {
				rs.openDetail()
			}
		case 'j':
			rs.moveSelection(1)
		case 'k':
			rs.moveSelection(-1)
		case 'a':
			rs.resolveSelected("keep_a")
		case 'b':
			rs.resolveSelected("keep_b")
		case 'K':
			rs.resolveSelected("keep_both")
		case 'd':
			rs.resolveSelected("delete_both")
		}
	} else if len(input) == 3 && input[0] == 27 && input[1] == 91 {
		switch input[2] {
		case 65: // Up
			rs.moveSelection(-1)
		case 66: // Down
			rs.moveSelection(1)
		}
	} else if len(input) == 2 && input[0] == 27 {
		return true
	}
	return false
}

// handleDetailInput processes input in the detail view.
// Returns true if the resolver should exit.
func (rs *resolverState) handleDetailInput(input []byte) bool {
	if len(input) == 1 {
		switch input[0] {
		case 'q':
			return true
		case 27: // Esc — back to list
			rs.state = resolverStateList
		case 'j':
			rs.scrollDetail(1)
		case 'k':
			rs.scrollDetail(-1)
		case 'a':
			rs.resolveSelected("keep_a")
		case 'b':
			rs.resolveSelected("keep_b")
		case 'K':
			rs.resolveSelected("keep_both")
		case 'd':
			rs.resolveSelected("delete_both")
		}
	} else if len(input) == 3 && input[0] == 27 && input[1] == 91 {
		switch input[2] {
		case 65: // Up
			rs.scrollDetail(-1)
		case 66: // Down
			rs.scrollDetail(1)
		}
	} else if len(input) == 2 && input[0] == 27 {
		rs.state = resolverStateList
	}
	return false
}

func (rs *resolverState) moveSelection(delta int) {
	if len(rs.issues) == 0 {
		return
	}
	rs.selected += delta
	if rs.selected < 0 {
		rs.selected = 0
	}
	if rs.selected >= len(rs.issues) {
		rs.selected = len(rs.issues) - 1
	}
}

func (rs *resolverState) scrollDetail(delta int) {
	rs.detailScroll += delta
	if rs.detailScroll < 0 {
		rs.detailScroll = 0
	}
	maxScroll := len(rs.detailLines) - (rs.termHeight - 8)
	if maxScroll < 0 {
		maxScroll = 0
	}
	if rs.detailScroll > maxScroll {
		rs.detailScroll = maxScroll
	}
}

func (rs *resolverState) fetchIssues() {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/kb/issues", rs.port))
	if err != nil {
		rs.issues = nil
		return
	}
	defer resp.Body.Close()

	var issues []resolverIssue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		rs.issues = nil
		return
	}

	rs.issues = issues
	if rs.selected >= len(rs.issues) {
		rs.selected = len(rs.issues) - 1
	}
	if rs.selected < 0 {
		rs.selected = 0
	}
}

func (rs *resolverState) openDetail() {
	iss := rs.issues[rs.selected]

	contentWidth := rs.termWidth - 8

	var lines []string

	// Statement A
	lines = append(lines, ansiAccent+ansiBold+"Statement A"+ansiReset)
	if iss.SourceA != "" {
		lines = append(lines, ansiMuted+"Source: "+iss.SourceA+ansiReset)
	}
	lines = append(lines, "")
	for _, line := range strings.Split(iss.ContentA, "\n") {
		if len(line) <= contentWidth {
			lines = append(lines, renderInlineMarkdown(line))
		} else {
			for _, wrapped := range wrapLine(line, contentWidth) {
				lines = append(lines, renderInlineMarkdown(wrapped))
			}
		}
	}

	// Separator
	lines = append(lines, "")
	lines = append(lines, ansiDim+strings.Repeat("─", contentWidth)+ansiReset)
	lines = append(lines, "")

	// Statement B
	lines = append(lines, ansiAccent+ansiBold+"Statement B"+ansiReset)
	if iss.SourceB != "" {
		lines = append(lines, ansiMuted+"Source: "+iss.SourceB+ansiReset)
	}
	lines = append(lines, "")
	for _, line := range strings.Split(iss.ContentB, "\n") {
		if len(line) <= contentWidth {
			lines = append(lines, renderInlineMarkdown(line))
		} else {
			for _, wrapped := range wrapLine(line, contentWidth) {
				lines = append(lines, renderInlineMarkdown(wrapped))
			}
		}
	}

	// Score
	lines = append(lines, "")
	lines = append(lines, ansiDim+strings.Repeat("─", contentWidth)+ansiReset)
	lines = append(lines, ansiMuted+fmt.Sprintf("Similarity: %.1f%%", iss.Score*100)+ansiReset)

	rs.detailLines = lines
	rs.detailScroll = 0
	rs.state = resolverStateDetail
}

func (rs *resolverState) resolveSelected(action string) {
	if len(rs.issues) == 0 || rs.selected < 0 || rs.selected >= len(rs.issues) {
		return
	}

	iss := rs.issues[rs.selected]

	body, _ := json.Marshal(map[string]string{"action": action})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/kb/issues/resolve?id=%s", rs.port, iss.ID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		rs.message = "Error: " + err.Error()
		return
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rs.message = fmt.Sprintf("Error: HTTP %d", resp.StatusCode)
		return
	}

	rs.message = fmt.Sprintf("Resolved: %s", action)
	rs.state = resolverStateList
	rs.fetchIssues()
}

// render draws the current state to the terminal.
func (rs *resolverState) render() {
	var sb strings.Builder
	sb.WriteString("\033[2J\033[H")

	if rs.state == resolverStateList {
		rs.renderList(&sb)
	} else {
		rs.renderDetail(&sb)
	}

	fmt.Print(sb.String())
}


func (rs *resolverState) renderList(sb *strings.Builder) {
	w := rs.termWidth

	// Header
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiAccent)
	sb.WriteString(ansiBold)
	sb.WriteString("Issue Resolver")
	sb.WriteString(ansiReset)

	if len(rs.issues) > 0 {
		sb.WriteString(ansiMuted)
		sb.WriteString(fmt.Sprintf("  %d open", len(rs.issues)))
		sb.WriteString(ansiReset)
	}
	sb.WriteString("\r\n\r\n")

	// Status message
	if rs.message != "" {
		sb.WriteString("  ")
		sb.WriteString(ansiGreen)
		sb.WriteString(rs.message)
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n\r\n")
		rs.message = ""
	}

	if len(rs.issues) == 0 {
		sb.WriteString("  ")
		sb.WriteString(ansiMuted)
		sb.WriteString(ansiItalic)
		sb.WriteString("No open issues")
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n")
	} else {
		// Calculate visible items
		maxVisible := (rs.termHeight - 10) / 3
		if maxVisible < 1 {
			maxVisible = 1
		}
		if maxVisible > len(rs.issues) {
			maxVisible = len(rs.issues)
		}

		start := 0
		if rs.selected >= maxVisible {
			start = rs.selected - maxVisible + 1
		}
		end := start + maxVisible
		if end > len(rs.issues) {
			end = len(rs.issues)
			start = end - maxVisible
			if start < 0 {
				start = 0
			}
		}

		for i := start; i < end; i++ {
			iss := rs.issues[i]

			// Selection indicator
			if i == rs.selected {
				sb.WriteString("  ")
				sb.WriteString(ansiAccent)
				sb.WriteString("▸")
				sb.WriteString(ansiReset)
				sb.WriteString(" ")
			} else {
				sb.WriteString("    ")
			}

			// Type badge + score
			sb.WriteString(ansiOrange)
			sb.WriteString("DUP")
			sb.WriteString(ansiReset)
			sb.WriteString(ansiMuted)
			sb.WriteString(fmt.Sprintf(" %.0f%%", iss.Score*100))
			sb.WriteString(ansiReset)

			// Preview of statement A
			previewA := firstLine(iss.ContentA)
			maxPreview := w - 20
			if maxPreview > 0 && len(previewA) > maxPreview {
				previewA = previewA[:maxPreview-3] + "..."
			}
			sb.WriteString("  ")
			if i == rs.selected {
				sb.WriteString(ansiBold)
			}
			sb.WriteString(previewA)
			if i == rs.selected {
				sb.WriteString(ansiReset)
			}
			sb.WriteString("\r\n")

			// Preview of statement B (indented)
			previewB := firstLine(iss.ContentB)
			if maxPreview > 0 && len(previewB) > maxPreview {
				previewB = previewB[:maxPreview-3] + "..."
			}
			sb.WriteString("             ")
			sb.WriteString(ansiDim)
			sb.WriteString("vs ")
			sb.WriteString(previewB)
			sb.WriteString(ansiReset)
			sb.WriteString("\r\n\r\n")
		}
	}

	// Footer
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("↑↓/jk")
	sb.WriteString(ansiReset)
	sb.WriteString(" navigate  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Enter")
	sb.WriteString(ansiReset)
	sb.WriteString(" detail  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("a")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep A  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("b")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep B  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("K")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep both  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("d")
	sb.WriteString(ansiReset)
	sb.WriteString(" delete both  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("q")
	sb.WriteString(ansiReset)
	sb.WriteString(" quit")
	sb.WriteString("\r\n")
}

func (rs *resolverState) renderDetail(sb *strings.Builder) {
	// Header
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("← Esc back")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n\r\n")

	// Content (scrolled)
	visibleLines := rs.termHeight - 8
	if visibleLines < 1 {
		visibleLines = 1
	}

	start := rs.detailScroll
	end := start + visibleLines
	if end > len(rs.detailLines) {
		end = len(rs.detailLines)
	}

	for i := start; i < end; i++ {
		sb.WriteString("    ")
		sb.WriteString(rs.detailLines[i])
		sb.WriteString("\r\n")
	}

	// Pad remaining
	for i := end - start; i < visibleLines; i++ {
		sb.WriteString("\r\n")
	}

	// Footer
	sb.WriteString("\r\n  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("↑↓/jk")
	sb.WriteString(ansiReset)
	sb.WriteString(" scroll  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("a")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep A  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("b")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep B  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("K")
	sb.WriteString(ansiReset)
	sb.WriteString(" keep both  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("d")
	sb.WriteString(ansiReset)
	sb.WriteString(" delete both  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Esc")
	sb.WriteString(ansiReset)
	sb.WriteString(" back  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("q")
	sb.WriteString(ansiReset)
	sb.WriteString(" quit")
	sb.WriteString("\r\n")
}

// firstLine returns the first line of s, truncated if needed.
func firstLine(s string) string {
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		return s[:nl]
	}
	return s
}
