package main

import (
	"regexp"
	"strings"
)

// xmlTagRe matches block-level XML tags used in system prompts.
var xmlTagRe = regexp.MustCompile(`^(\s*)<(/?)(rule|script|example|environment|project_setup)(\s[^>]*)?>(.*)$`)

// renderPromptLines renders a raw markdown/XML system prompt into
// ANSI-styled, word-wrapped lines ready for the scrolling viewport.
func renderPromptLines(raw string, maxWidth int) []string {
	if maxWidth < 1 {
		maxWidth = 1
	}

	rawLines := strings.Split(raw, "\n")
	var result []string
	inFencedBlock := false

	for _, line := range rawLines {
		// Fenced code blocks: toggle on ``` delimiters
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFencedBlock = !inFencedBlock
			// Render the delimiter itself as dim
			for _, w := range wrapOrSingle(line, maxWidth) {
				result = append(result, ansiDim+w+ansiReset)
			}
			continue
		}

		if inFencedBlock {
			for _, w := range wrapOrSingle(line, maxWidth) {
				result = append(result, ansiDim+w+ansiReset)
			}
			continue
		}

		// XML tags
		if m := xmlTagRe.FindStringSubmatch(line); m != nil {
			result = append(result, renderXMLTag(m, maxWidth)...)
			continue
		}

		// Headings
		if heading, level := parseHeading(line); level > 0 {
			for _, w := range wrapOrSingle(heading, maxWidth) {
				result = append(result, ansiAccent+ansiBold+w+ansiReset)
			}
			continue
		}

		// Bullet lists
		if indent, rest, ok := parseBullet(line); ok {
			wrapped := wrapOrSingle(rest, maxWidth-len(indent)-2)
			for i, w := range wrapped {
				if i == 0 {
					result = append(result, indent+"- "+renderInlineMarkdown(w))
				} else {
					result = append(result, indent+"  "+renderInlineMarkdown(w))
				}
			}
			continue
		}

		// Default: inline markdown
		if line == "" {
			result = append(result, "")
			continue
		}
		for _, w := range wrapOrSingle(line, maxWidth) {
			result = append(result, renderInlineMarkdown(w))
		}
	}

	return result
}

// xmlAttrRe splits an attribute string into name=, value pairs.
var xmlAttrRe = regexp.MustCompile(`(\w+=)("(?:[^"\\]|\\.)*")`)

// renderXMLTag styles an XML tag match. Groups: [full, indent, slash, name, attrs, tail].
func renderXMLTag(m []string, maxWidth int) []string {
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

	// XML tag lines are typically short; wrap is unlikely but handled.
	return []string{sb.String()}
}

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
