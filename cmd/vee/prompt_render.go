package main

import (
	"regexp"
	"strings"
)

// promptLineKind identifies how a line should be styled.
type promptLineKind int

const (
	lineKindPlain promptLineKind = iota
	lineKindHeading
	lineKindBullet
	lineKindBulletCont // continuation of wrapped bullet
	lineKindFenced
	lineKindXMLTag
)

// promptLine holds a pre-processed line with its styling metadata.
type promptLine struct {
	text   string         // the raw text (may be wrapped segment)
	kind   promptLineKind // how to style this line
	indent string         // for bullets: the leading indent
}

// xmlTagRe matches block-level XML tags used in system prompts.
var xmlTagRe = regexp.MustCompile(`^(\s*)<(/?)(rule|script|example|environment|project_setup)(\s[^>]*)?>(.*)$`)

// preparePromptLines pre-processes raw prompt content into lines with metadata.
// The returned lines are wrapped to maxWidth and tagged with their kind.
func preparePromptLines(raw string, maxWidth int) []promptLine {
	if maxWidth < 1 {
		maxWidth = 1
	}

	rawLines := strings.Split(raw, "\n")
	var result []promptLine
	inFencedBlock := false

	for _, line := range rawLines {
		// Fenced code blocks: toggle on ``` delimiters
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFencedBlock = !inFencedBlock
			for _, w := range wrapOrSingle(line, maxWidth) {
				result = append(result, promptLine{text: w, kind: lineKindFenced})
			}
			continue
		}

		if inFencedBlock {
			for _, w := range wrapOrSingle(line, maxWidth) {
				result = append(result, promptLine{text: w, kind: lineKindFenced})
			}
			continue
		}

		// XML tags
		if m := xmlTagRe.FindStringSubmatch(line); m != nil {
			result = append(result, promptLine{text: line, kind: lineKindXMLTag})
			continue
		}

		// Headings
		if heading, level := parseHeading(line); level > 0 {
			for _, w := range wrapOrSingle(heading, maxWidth) {
				result = append(result, promptLine{text: w, kind: lineKindHeading})
			}
			continue
		}

		// Bullet lists
		if indent, rest, ok := parseBullet(line); ok {
			wrapped := wrapOrSingle(rest, maxWidth-len(indent)-2)
			for i, w := range wrapped {
				if i == 0 {
					result = append(result, promptLine{text: w, kind: lineKindBullet, indent: indent})
				} else {
					result = append(result, promptLine{text: w, kind: lineKindBulletCont, indent: indent})
				}
			}
			continue
		}

		// Default: plain text
		if line == "" {
			result = append(result, promptLine{text: "", kind: lineKindPlain})
			continue
		}
		for _, w := range wrapOrSingle(line, maxWidth) {
			result = append(result, promptLine{text: w, kind: lineKindPlain})
		}
	}

	return result
}

// renderPromptLine applies styling to a single line, optionally highlighting filter matches.
func renderPromptLine(pl promptLine, filter string) string {
	var styled string

	switch pl.kind {
	case lineKindFenced:
		styled = ansiDim + pl.text + ansiReset
	case lineKindHeading:
		styled = ansiAccent + ansiBold + pl.text + ansiReset
	case lineKindBullet:
		styled = pl.indent + "- " + renderInlineMarkdown(pl.text)
	case lineKindBulletCont:
		styled = pl.indent + "  " + renderInlineMarkdown(pl.text)
	case lineKindXMLTag:
		styled = renderXMLTagLine(pl.text)
	default:
		if pl.text == "" {
			return ""
		}
		styled = renderInlineMarkdown(pl.text)
	}

	// Apply search highlighting if filter is set
	if filter != "" {
		styled = highlightFilter(styled, filter)
	}

	return styled
}

// renderXMLTagLine styles an XML tag line.
func renderXMLTagLine(line string) string {
	m := xmlTagRe.FindStringSubmatch(line)
	if m == nil {
		return line
	}

	indent := m[1]
	slash := m[2]
	name := m[3]
	attrs := m[4]
	tail := m[5]

	var sb strings.Builder
	sb.WriteString(indent)
	sb.WriteString(ansiTeal + ansiBold + "<" + slash + name + ansiReset)
	if attrs != "" {
		styled := xmlAttrRe.ReplaceAllStringFunc(attrs, func(match string) string {
			parts := xmlAttrRe.FindStringSubmatch(match)
			return ansiTeal + ansiBold + parts[1] + ansiReset + ansiOrange + parts[2] + ansiReset
		})
		sb.WriteString(styled)
	}
	sb.WriteString(ansiTeal + ansiBold + ">" + ansiReset)
	if tail != "" {
		sb.WriteString(renderInlineMarkdown(tail))
	}

	return sb.String()
}

// renderPromptLines renders a raw markdown/XML system prompt into
// ANSI-styled, word-wrapped lines ready for the scrolling viewport.
// Kept for backward compatibility.
func renderPromptLines(raw string, maxWidth int) []string {
	lines := preparePromptLines(raw, maxWidth)
	result := make([]string, len(lines))
	for i, pl := range lines {
		result[i] = renderPromptLine(pl, "")
	}
	return result
}

// xmlAttrRe splits an attribute string into name=, value pairs.
var xmlAttrRe = regexp.MustCompile(`(\w+=)("(?:[^"\\]|\\.)*")`)

// parseHeading checks if a line is a markdown heading (# through ####).
// Returns the heading text (without #) and the level, or 0 if not a heading.
func parseHeading(line string) (string, int) {
	level := 0
	for level < len(line) && level < 4 && line[level] == '#' {
		level++
	}
	if level == 0 || level >= len(line) || line[level] != ' ' {
		return "", 0
	}
	return line[level+1:], level
}

// parseBullet checks if a line is a markdown bullet list item.
// Returns (indent, rest-of-line, true) or ("", "", false).
func parseBullet(line string) (string, string, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i < len(line) && (line[i] == '-' || line[i] == '*') && i+1 < len(line) && line[i+1] == ' ' {
		return line[:i], line[i+2:], true
	}
	return "", "", false
}

// wrapOrSingle wraps a line using wrapLine, falling back to returning
// the line as-is when it fits or is empty.
func wrapOrSingle(line string, maxWidth int) []string {
	if len(line) <= maxWidth || line == "" {
		return []string{line}
	}
	return wrapLine(line, maxWidth)
}
