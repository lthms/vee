package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModeFrontmatter is the YAML frontmatter target for mode files.
type ModeFrontmatter struct {
	Indicator   string `yaml:"indicator"`
	Description string `yaml:"description"`
	Priority    *int   `yaml:"priority"`
	NoPrompt    bool   `yaml:"no_prompt"`
}

// parseModeFile splits a mode file into frontmatter and body, parses the
// YAML, and returns a Mode. The mode name is derived from the filename.
func parseModeFile(filename string, content []byte) (Mode, error) {
	name := strings.TrimSuffix(filepath.Base(filename), ".md")

	// Split frontmatter (between --- delimiters) from body.
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return Mode{}, fmt.Errorf("%s: missing frontmatter", filename)
	}

	// Find the closing ---
	rest := trimmed[3:]
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return Mode{}, fmt.Errorf("%s: unclosed frontmatter", filename)
	}

	fmBytes := rest[:idx]
	body := bytes.TrimSpace(rest[idx+4:]) // skip past \n---

	var fm ModeFrontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return Mode{}, fmt.Errorf("%s: bad frontmatter: %w", filename, err)
	}

	priority := math.MaxInt
	if fm.Priority != nil {
		priority = *fm.Priority
	}

	return Mode{
		Name:        name,
		Indicator:   fm.Indicator,
		Description: fm.Description,
		Priority:    priority,
		Prompt:      string(body),
		NoPrompt:    fm.NoPrompt,
	}, nil
}

// wrapModeBody wraps a mode body in XML tags for system prompt composition.
func wrapModeBody(modeName, indicator, body string) string {
	return fmt.Sprintf("<mode name=%q><indicator value=%q />\n\n%s\n</mode>", modeName, indicator, body)
}

// loadModesFromDir reads all *.md files from a directory and parses them as modes.
func loadModesFromDir(dir string) ([]Mode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var modes []Mode
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		mode, err := parseModeFile(e.Name(), content)
		if err != nil {
			return nil, err
		}
		modes = append(modes, mode)
	}

	return modes, nil
}

// initModeRegistry loads modes from the filesystem and rebuilds modeRegistry
// and modeOrder. It is called fresh on every invocation (picker open,
// new-pane, resume) so edits to mode files take effect without restarting.
//
// Modes are merged from two directories:
//  1. veePath/modes/ — installed defaults
//  2. ~/.config/vee/modes/ — user overrides (same name wins, new names added)
func initModeRegistry(veePath string) error {
	basePrompt, err := promptFS.ReadFile("prompts/base.md")
	if err != nil {
		return fmt.Errorf("read base prompt: %w", err)
	}

	// Start with installed defaults.
	byName := make(map[string]Mode)
	installedDir := filepath.Join(veePath, "modes")
	if installed, err := loadModesFromDir(installedDir); err == nil {
		for _, m := range installed {
			byName[m.Name] = m
		}
	}

	// Merge user overrides on top.
	home, err := os.UserHomeDir()
	if err == nil {
		userDir := filepath.Join(home, ".config", "vee", "modes")
		if userModes, err := loadModesFromDir(userDir); err == nil {
			for _, m := range userModes {
				byName[m.Name] = m
			}
		}
	}

	if len(byName) == 0 {
		return fmt.Errorf("no mode files found in %s or ~/.config/vee/modes/", installedDir)
	}

	kbPrompt := extractSection(string(basePrompt), "<knowledge-base>", "</knowledge-base>")

	// Compose prompts and collect into a slice for sorting.
	modes := make([]Mode, 0, len(byName))
	for _, m := range byName {
		if m.NoPrompt {
			m.Prompt = kbPrompt
		} else {
			wrapped := wrapModeBody(m.Name, m.Indicator, m.Prompt)
			m.Prompt = string(basePrompt) + "\n\n" + wrapped
		}
		modes = append(modes, m)
	}

	// Build modeOrder sorted by priority (ascending), then alphabetically.
	sort.Slice(modes, func(i, j int) bool {
		if modes[i].Priority != modes[j].Priority {
			return modes[i].Priority < modes[j].Priority
		}
		return modes[i].Name < modes[j].Name
	})

	modeRegistry = make(map[string]Mode, len(modes))
	modeOrder = make([]string, len(modes))
	for i, m := range modes {
		modeRegistry[m.Name] = m
		modeOrder[i] = m.Name
	}

	return nil
}
