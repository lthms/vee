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

// ProfileFrontmatter is the YAML frontmatter target for profile files.
type ProfileFrontmatter struct {
	Indicator         string `yaml:"indicator"`
	Description       string `yaml:"description"`
	Priority          *int   `yaml:"priority"`
	DefaultPrompt     string `yaml:"default_prompt"`
	PromptPlaceholder string `yaml:"prompt_placeholder"`
}

// parseProfileFile splits a profile file into frontmatter and body, parses the
// YAML, and returns a Profile. The profile name is derived from the filename.
func parseProfileFile(filename string, content []byte) (Profile, error) {
	name := strings.TrimSuffix(filepath.Base(filename), ".md")

	// Split frontmatter (between --- delimiters) from body.
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return Profile{}, fmt.Errorf("%s: missing frontmatter", filename)
	}

	// Find the closing ---
	rest := trimmed[3:]
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return Profile{}, fmt.Errorf("%s: unclosed frontmatter", filename)
	}

	fmBytes := rest[:idx]
	body := bytes.TrimSpace(rest[idx+4:]) // skip past \n---

	var fm ProfileFrontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return Profile{}, fmt.Errorf("%s: bad frontmatter: %w", filename, err)
	}

	priority := math.MaxInt
	if fm.Priority != nil {
		priority = *fm.Priority
	}

	return Profile{
		Name:              name,
		Indicator:         fm.Indicator,
		Description:       fm.Description,
		Priority:          priority,
		Prompt:            string(body),
		DefaultPrompt:     fm.DefaultPrompt,
		PromptPlaceholder: fm.PromptPlaceholder,
	}, nil
}

// wrapProfileBody wraps a profile body in XML tags for system prompt composition.
// It prepends a rule explaining the script's role, so the rule is only
// present when there is actually a script to follow.
func wrapProfileBody(indicator, body string) string {
	const scriptRule = `<rule object="Script">
Your system prompt contains a <script> block.
It defines the purpose and constraints of this session.
ALWAYS follow the directives in your <script> block. They take precedence over your default behavior.
</rule>`
	indicatorRule := fmt.Sprintf("<rule object=\"Indicator\">\nALWAYS prefix your messages with %s.\n</rule>", indicator)
	return fmt.Sprintf("%s\n\n%s\n\n<script>\n%s\n</script>", scriptRule, indicatorRule, body)
}

// loadProfilesFromDir reads all *.md files from a directory and parses them as profiles.
func loadProfilesFromDir(dir string) ([]Profile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var profiles []Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		profile, err := parseProfileFile(e.Name(), content)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}

	return profiles, nil
}

// initProfileRegistry loads profiles from the filesystem and rebuilds profileRegistry
// and profileOrder. It is called fresh on every invocation (picker open,
// new-pane, resume) so edits to profile files take effect without restarting.
//
// Profiles are merged from two directories:
//  1. veePath/profiles/ — installed defaults
//  2. ~/.config/vee/profiles/ — user overrides (same name wins, new names added)
func initProfileRegistry(veePath string) error {
	basePrompt, err := promptFS.ReadFile("prompts/base.md")
	if err != nil {
		return fmt.Errorf("read base prompt: %w", err)
	}

	// Start with installed defaults.
	byName := make(map[string]Profile)
	installedDir := filepath.Join(veePath, "profiles")
	if installed, err := loadProfilesFromDir(installedDir); err == nil {
		for _, m := range installed {
			byName[m.Name] = m
		}
	}

	// Merge user overrides on top.
	home, err := os.UserHomeDir()
	if err == nil {
		userDir := filepath.Join(home, ".config", "vee", "profiles")
		if userProfiles, err := loadProfilesFromDir(userDir); err == nil {
			for _, m := range userProfiles {
				byName[m.Name] = m
			}
		}
	}

	if len(byName) == 0 {
		return fmt.Errorf("no profile files found in %s or ~/.config/vee/profiles/", installedDir)
	}

	// Compose prompts and collect into a slice for sorting.
	profiles := make([]Profile, 0, len(byName))
	for _, m := range byName {
		if m.Prompt != "" {
			m.Prompt = string(basePrompt) + "\n\n" + wrapProfileBody(m.Indicator, m.Prompt)
		} else {
			m.Prompt = string(basePrompt)
		}
		profiles = append(profiles, m)
	}

	// Build profileOrder sorted by priority (ascending), then alphabetically.
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Priority != profiles[j].Priority {
			return profiles[i].Priority < profiles[j].Priority
		}
		return profiles[i].Name < profiles[j].Name
	})

	profileRegistry = make(map[string]Profile, len(profiles))
	profileOrder = make([]string, len(profiles))
	for i, m := range profiles {
		profileRegistry[m.Name] = m
		profileOrder[i] = m.Name
	}

	return nil
}
