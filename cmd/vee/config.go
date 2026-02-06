package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	gcfg "github.com/go-git/gcfg/v2"
	"github.com/lthms/vee/internal/kb"
)

// UserConfig holds user-level configuration loaded from ~/.config/vee/config.
type UserConfig struct {
	Embedding EmbeddingConfig
	Identity  *IdentityConfig
	Feedback  FeedbackConfig
}

// FeedbackConfig configures mode feedback sampling.
type FeedbackConfig struct {
	MaxExamples int
}

// IdentityConfig configures the assistant's identity (name + git author).
type IdentityConfig struct {
	Name    string
	Email   string
	Disable bool
}

// PlatformsConfig holds the [platforms] section of .vee/config.
type PlatformsConfig struct {
	Forge  string
	Issues string
}

// ProjectConfig represents the top-level .vee/config structure.
type ProjectConfig struct {
	Ephemeral *EphemeralConfig
	Identity  *IdentityConfig
	Platforms *PlatformsConfig
}

// readProjectTOML reads and parses .vee/config from the current directory.
func readProjectTOML() (*ProjectConfig, error) {
	m, err := parseConfig(".vee/config", nil)
	if err != nil {
		return nil, err
	}
	return hydrateProjectConfig(m), nil
}

// parseConfig reads a git-config-format file and returns a flat map of
// "section.key" → []string values. It handles [include] and [includeIf]
// directives recursively. The seen map prevents infinite include cycles.
func parseConfig(path string, seen map[string]bool) (map[string][]string, error) {
	if seen == nil {
		seen = make(map[string]bool)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if seen[absPath] {
		return make(map[string][]string), nil
	}
	seen[absPath] = true

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string][]string)
	dir := filepath.Dir(absPath)

	var currentSection string
	var currentSubsection string

	err = gcfg.ReadWithCallback(f, func(section, subsection, key, value string, blank bool) error {
		if key == "" {
			// Section or subsection header
			currentSection = strings.ToLower(section)
			currentSubsection = subsection
			return nil
		}

		sec := currentSection
		sub := currentSubsection

		// Handle [include] path = ...
		if sec == "include" && strings.ToLower(key) == "path" && !blank {
			incPath := resolveIncludePath(value, dir)
			included, err := parseConfig(incPath, seen)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil // silently skip missing includes
				}
				return err
			}
			mergeConfig(result, included)
			return nil
		}

		// Handle [includeIf "gitdir:PATTERN"] path = ...
		if sec == "includeif" && strings.ToLower(key) == "path" && !blank && sub != "" {
			if cond, ok := strings.CutPrefix(sub, "gitdir:"); ok {
				if matchGitdir(cond) {
					incPath := resolveIncludePath(value, dir)
					included, err := parseConfig(incPath, seen)
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							return nil
						}
						return err
					}
					mergeConfig(result, included)
				}
			}
			return nil
		}

		mapKey := sec + "." + strings.ToLower(key)
		if blank {
			return nil
		}
		result[mapKey] = append(result[mapKey], value)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// resolveIncludePath expands ~ and resolves relative paths against baseDir.
func resolveIncludePath(path, baseDir string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	return path
}

// matchGitdir checks whether the current repo's .git directory matches a
// gitdir pattern, following git's includeIf gitdir rules:
//   - Match target is the .git directory (via git rev-parse --absolute-git-dir)
//   - ~/  → expand to $HOME
//   - Pattern not starting with ~/, ./, or / → prepend **/
//   - Trailing / → append **
func matchGitdir(pattern string) bool {
	out, err := exec.Command("git", "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return false
	}
	gitDir := strings.TrimRight(string(out), "\n")

	// Expand ~ in pattern
	if strings.HasPrefix(pattern, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		pattern = filepath.Join(home, pattern[2:])
	}

	// Trailing "/" means recursive match into subdirs → append **
	if strings.HasSuffix(pattern, "/") {
		pattern = pattern + "**"
	}

	// Relative patterns (not starting with / or ./) match anywhere → prepend **/
	if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "./") {
		pattern = "**/" + pattern
	}

	return matchPath(pattern, gitDir)
}

// matchPath performs **-aware glob matching by splitting pattern and path into
// /-separated components. ** matches zero or more path components; single
// segments are matched with filepath.Match.
func matchPath(pattern, name string) bool {
	patParts := strings.Split(pattern, "/")
	nameParts := strings.Split(name, "/")
	return matchParts(patParts, nameParts)
}

func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true // trailing ** matches everything
			}
			// Try matching the rest of the pattern against every suffix of name
			for i := 0; i <= len(name); i++ {
				if matchParts(pat, name[i:]) {
					return true
				}
			}
			return false
		}

		if len(name) == 0 {
			return false
		}

		matched, err := filepath.Match(pat[0], name[0])
		if err != nil || !matched {
			return false
		}

		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// mergeConfig merges src into dst, appending values.
func mergeConfig(dst, src map[string][]string) {
	for k, v := range src {
		dst[k] = append(dst[k], v...)
	}
}


// lastValue returns the last value for a key, or "" if absent.
func lastValue(m map[string][]string, key string) string {
	vals := m[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// hydrateProjectConfig populates a ProjectConfig from the flat map.
func hydrateProjectConfig(m map[string][]string) *ProjectConfig {
	cfg := &ProjectConfig{}

	// [ephemeral]
	if df := lastValue(m, "ephemeral.dockerfile"); df != "" {
		if cfg.Ephemeral == nil {
			cfg.Ephemeral = &EphemeralConfig{}
		}
		cfg.Ephemeral.Dockerfile = df
	}
	if compose := lastValue(m, "ephemeral.compose"); compose != "" {
		if cfg.Ephemeral == nil {
			cfg.Ephemeral = &EphemeralConfig{}
		}
		cfg.Ephemeral.Compose = compose
	}
	if envs := m["ephemeral.env"]; len(envs) > 0 {
		if cfg.Ephemeral == nil {
			cfg.Ephemeral = &EphemeralConfig{}
		}
		cfg.Ephemeral.Env = envs
	}
	if args := m["ephemeral.extraargs"]; len(args) > 0 {
		if cfg.Ephemeral == nil {
			cfg.Ephemeral = &EphemeralConfig{}
		}
		cfg.Ephemeral.ExtraArgs = args
	}
	if mounts := m["ephemeral.mount"]; len(mounts) > 0 {
		if cfg.Ephemeral == nil {
			cfg.Ephemeral = &EphemeralConfig{}
		}
		for _, raw := range mounts {
			ms, err := parseMountSpec(raw)
			if err != nil {
				slog.Warn("invalid mount spec, skipping", "spec", raw, "error", err)
				continue
			}
			cfg.Ephemeral.Mounts = append(cfg.Ephemeral.Mounts, ms)
		}
	}

	// [identity]
	if name := lastValue(m, "identity.name"); name != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Name = name
	}
	if email := lastValue(m, "identity.email"); email != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Email = email
	}
	if disable := lastValue(m, "identity.disable"); disable != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Disable = disable == "true"
	}

	// [platforms]
	if forge := lastValue(m, "platforms.forge"); forge != "" {
		if cfg.Platforms == nil {
			cfg.Platforms = &PlatformsConfig{}
		}
		cfg.Platforms.Forge = forge
	}
	if issues := lastValue(m, "platforms.issues"); issues != "" {
		if cfg.Platforms == nil {
			cfg.Platforms = &PlatformsConfig{}
		}
		cfg.Platforms.Issues = issues
	}

	return cfg
}

// parseMountSpec parses a "source:target[:mode]" string into a MountSpec.
func parseMountSpec(s string) (MountSpec, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return MountSpec{}, fmt.Errorf("mount spec requires at least source:target, got %q", s)
	}
	ms := MountSpec{
		Source: parts[0],
		Target: parts[1],
	}
	if len(parts) == 3 {
		ms.Mount = parts[2]
	}
	return ms, nil
}

// hydrateUserConfig populates a UserConfig from the flat map, applying defaults.
func hydrateUserConfig(m map[string][]string) *UserConfig {
	cfg := &UserConfig{
		Embedding: EmbeddingConfig{
			URL:          "http://localhost:11434",
			Model:        "nomic-embed-text",
			Threshold:    0.3,
			MaxResults:   10,
			DupThreshold: 0.85,
		},
		Feedback: FeedbackConfig{
			MaxExamples: 5,
		},
	}

	// [embedding]
	if url := lastValue(m, "embedding.url"); url != "" {
		cfg.Embedding.URL = url
	}
	if model := lastValue(m, "embedding.model"); model != "" {
		cfg.Embedding.Model = model
	}
	if th := lastValue(m, "embedding.threshold"); th != "" {
		if v, err := strconv.ParseFloat(th, 64); err == nil {
			cfg.Embedding.Threshold = v
		}
	}
	if mr := lastValue(m, "embedding.maxresults"); mr != "" {
		if v, err := strconv.Atoi(mr); err == nil {
			cfg.Embedding.MaxResults = v
		}
	}
	if dt := lastValue(m, "embedding.dupthreshold"); dt != "" {
		if v, err := strconv.ParseFloat(dt, 64); err == nil {
			cfg.Embedding.DupThreshold = v
		}
	}

	// [identity]
	if name := lastValue(m, "identity.name"); name != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Name = name
	}
	if email := lastValue(m, "identity.email"); email != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Email = email
	}
	if disable := lastValue(m, "identity.disable"); disable != "" {
		if cfg.Identity == nil {
			cfg.Identity = &IdentityConfig{}
		}
		cfg.Identity.Disable = disable == "true"
	}

	// [feedback]
	if me := lastValue(m, "feedback.maxexamples"); me != "" {
		if v, err := strconv.Atoi(me); err == nil {
			cfg.Feedback.MaxExamples = v
		}
	}

	return cfg
}

// resolveIdentity merges user-level and project-level identity configs.
// Project disable=true suppresses identity entirely.
// Field-level merge: user provides defaults, project overrides non-empty fields.
func resolveIdentity(user, project *IdentityConfig) *IdentityConfig {
	if user == nil && project == nil {
		return nil
	}
	if project != nil && project.Disable {
		return nil
	}

	result := &IdentityConfig{}

	// Start with user values
	if user != nil {
		result.Name = user.Name
		result.Email = user.Email
	}

	// Override with non-empty project values
	if project != nil {
		if project.Name != "" {
			result.Name = project.Name
		}
		if project.Email != "" {
			result.Email = project.Email
		}
	}

	return result
}

// validateIdentity returns an error if identity is non-nil and not disabled
// but name or email is empty.
func validateIdentity(cfg *IdentityConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.Disable {
		return nil
	}
	if cfg.Name == "" {
		return fmt.Errorf("identity: name is required when identity is configured")
	}
	if cfg.Email == "" {
		return fmt.Errorf("identity: email is required when identity is configured")
	}
	return nil
}

// identityRule returns the rendered <rule> block for the system prompt,
// or "" if identity is nil.
func identityRule(cfg *IdentityConfig) string {
	if cfg == nil {
		return ""
	}
	return fmt.Sprintf("<rule object=\"Identity\">\nYour name is %s.\nALWAYS use `git commit` with `--author \"%s <%s>\"`.\n</rule>", cfg.Name, cfg.Name, cfg.Email)
}

// platformsRule returns the rendered <rule> block for the system prompt,
// or "" if cfg is nil or has no URLs configured.
func platformsRule(cfg *PlatformsConfig) string {
	if cfg == nil {
		return ""
	}
	var lines []string
	if cfg.Forge != "" {
		lines = append(lines, fmt.Sprintf("Use %s as the project's forge (source code, pull requests).", cfg.Forge))
	}
	if cfg.Issues != "" {
		lines = append(lines, fmt.Sprintf("Use %s as the project's issue tracker.", cfg.Issues))
	}
	if len(lines) == 0 {
		return ""
	}
	return "<rule object=\"Platforms\">\n" + strings.Join(lines, "\n") + "\n</rule>"
}

// EmbeddingConfig configures the embedding backend and knowledge base settings.
type EmbeddingConfig struct {
	URL          string  // Ollama base URL (default "http://localhost:11434")
	Model        string  // embedding model name (default "nomic-embed-text")
	Threshold    float64 // minimum cosine similarity to include in query results (default 0.3)
	MaxResults   int     // max query results returned (default 10)
	DupThreshold float64 // cosine similarity above which a pair is flagged as duplicate (default 0.85)
}

// loadUserConfig reads ~/.config/vee/config and returns the parsed config
// with defaults applied. If the file does not exist, defaults are returned with
// no error.
func loadUserConfig() (*UserConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}

	path := filepath.Join(home, ".config", "vee", "config")

	m, err := parseConfig(path, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hydrateUserConfig(nil), nil
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return hydrateUserConfig(m), nil
}

// OllamaModel implements kb.Model via the Ollama HTTP API.
type OllamaModel struct {
	URL   string
	Model string
}

// Embed sends texts to Ollama's embedding endpoint and returns the embeddings.
func (o *OllamaModel) Embed(texts []string) ([][]float64, error) {
	model := o.Model
	if model == "" {
		model = "nomic-embed-text"
	}

	reqBody, err := json.Marshal(map[string]any{
		"model": model,
		"input": texts,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	resp, err := http.Post(o.URL+"/api/embed", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ollama embed response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse ollama embed response: %w", err)
	}

	return result.Embeddings, nil
}

func ensureOllamaModel(baseURL, model string) error {
	reqBody, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := http.Post(baseURL+"/api/show", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		slog.Info("ollama model not found locally, pulling", "model", model)
		return pullOllamaModel(baseURL, model)
	}

	return fmt.Errorf("ollama /api/show returned unexpected status %d for %s", resp.StatusCode, model)
}

// openKB creates the embedding model (Ollama), ensures the model is available,
// and opens the knowledge base.
func openKB(userCfg *UserConfig) (*kb.KnowledgeBase, error) {
	embedModel := &OllamaModel{
		URL:   userCfg.Embedding.URL,
		Model: userCfg.Embedding.Model,
	}

	if err := ensureOllamaModel(embedModel.URL, userCfg.Embedding.Model); err != nil {
		return nil, fmt.Errorf("ensure ollama embedding model: %w", err)
	}

	stateDir, err := stateDir()
	if err != nil {
		return nil, err
	}

	kbase, err := kb.Open(kb.Config{
		DBPath:         filepath.Join(stateDir, "kb.db"),
		Model:          embedModel,
		EmbeddingModel: userCfg.Embedding.Model,
		Threshold:      userCfg.Embedding.Threshold,
		MaxResults:     userCfg.Embedding.MaxResults,
		DupThreshold:   userCfg.Embedding.DupThreshold,
	})
	if err != nil {
		return nil, err
	}

	return kbase, nil
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".local", "state", "vee")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

func pullOllamaModel(baseURL, model string) error {
	reqBody, err := json.Marshal(map[string]any{
		"name":   model,
		"stream": false,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	slog.Info("pulling ollama model (this may take a while)", "model", model)

	resp, err := http.Post(baseURL+"/api/pull", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("ollama pull request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read pull response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama pull failed with status %d: %s", resp.StatusCode, string(body))
	}

	slog.Info("ollama model pulled successfully", "model", model)
	return nil
}
