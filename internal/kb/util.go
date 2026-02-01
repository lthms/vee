package kb

import "strings"

// sanitizeFilename turns a title into a safe filename (no path separators, etc).
func sanitizeFilename(title string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	name := replacer.Replace(title)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "untitled"
	}
	return name
}

// stripFrontmatter removes YAML frontmatter delimited by "---" lines from markdown.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	// Find the closing "---"
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return s
	}
	// Skip past the closing "---\n"
	body := s[3+end+4:]
	return strings.TrimLeft(body, "\n")
}

// parseCSV splits a comma-separated string into trimmed non-empty tokens.
func parseCSV(s string) []string {
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}
