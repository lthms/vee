package main

import (
	"bytes"
	"context"
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
	"time"

	gcfg "github.com/go-git/gcfg/v2"
	"github.com/lthms/vee/internal/kb"
)

// Generator is the interface for text generation (judgment evaluation).
// This is separate from kb.Model which also requires Embed().
type Generator interface {
	Generate(prompt string) (string, error)
}

// UserConfig holds user-level configuration loaded from ~/.config/vee/config.
type UserConfig struct {
	Judgment  JudgmentConfig
	Knowledge KnowledgeConfig
	Identity  *IdentityConfig
}

// IdentityConfig configures the assistant's identity (name + git author).
type IdentityConfig struct {
	Name    string
	Email   string
	Disable bool
}

// ProjectConfig represents the top-level .vee/config structure.
type ProjectConfig struct {
	Ephemeral *EphemeralConfig
	Identity  *IdentityConfig
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
		Judgment: JudgmentConfig{
			URL:   "http://localhost:11434",
			Model: "claude:haiku",
		},
		Knowledge: KnowledgeConfig{
			EmbeddingModel: "nomic-embed-text",
			Threshold:      0.3,
			MaxResults:     10,
		},
	}

	// [judgment]
	if url := lastValue(m, "judgment.url"); url != "" {
		cfg.Judgment.URL = url
	}
	if model := lastValue(m, "judgment.model"); model != "" {
		cfg.Judgment.Model = model
	}

	// [knowledge]
	if em := lastValue(m, "knowledge.embeddingmodel"); em != "" {
		cfg.Knowledge.EmbeddingModel = em
	}
	if th := lastValue(m, "knowledge.threshold"); th != "" {
		if v, err := strconv.ParseFloat(th, 64); err == nil {
			cfg.Knowledge.Threshold = v
		}
	}
	if mr := lastValue(m, "knowledge.maxresults"); mr != "" {
		if v, err := strconv.Atoi(mr); err == nil {
			cfg.Knowledge.MaxResults = v
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

// JudgmentConfig configures the local LLM used for background KB operations.
type JudgmentConfig struct {
	URL   string
	Model string
}

// KnowledgeConfig configures knowledge base settings.
type KnowledgeConfig struct {
	EmbeddingModel string
	Threshold      float64
	MaxResults     int
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
	URL            string
	Model          string
	EmbeddingModel string
}

// Generate sends a prompt to Ollama and returns the trimmed response text.
// Uses a 2-minute timeout to prevent hanging indefinitely.
func (o *OllamaModel) Generate(prompt string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":  o.Model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/generate", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read ollama response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse ollama response: %w", err)
	}

	return strings.TrimSpace(result.Response), nil
}

// ClaudeModel implements Generator by shelling out to the Claude CLI.
type ClaudeModel struct {
	Model string // e.g. "haiku", "sonnet", "opus"
}

// Generate sends a prompt to Claude CLI via stdin and returns the response.
func (c *ClaudeModel) Generate(prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", c.Model)
	cmd.Stdin = strings.NewReader(prompt)

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude cli: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// parseJudgmentModel returns a Generator based on the configured model string.
// Models prefixed with "claude:" use the Claude CLI; everything else uses Ollama.
func parseJudgmentModel(model, ollamaURL string) Generator {
	if after, ok := strings.CutPrefix(model, "claude:"); ok {
		return &ClaudeModel{Model: after}
	}
	return &OllamaModel{URL: ollamaURL, Model: model}
}

// Embed sends texts to Ollama's embedding endpoint and returns the embeddings.
func (o *OllamaModel) Embed(texts []string) ([][]float64, error) {
	model := o.EmbeddingModel
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

// openKB creates the embedding model (always Ollama), a judgment Generator
// (Ollama or Claude CLI depending on config), ensures required Ollama models
// are available, and opens the knowledge base.
func openKB(userCfg *UserConfig) (*kb.KnowledgeBase, Generator, error) {
	// Embedding model is always Ollama
	embedModel := &OllamaModel{
		URL:            userCfg.Judgment.URL,
		EmbeddingModel: userCfg.Knowledge.EmbeddingModel,
	}

	if err := ensureOllamaModel(embedModel.URL, userCfg.Knowledge.EmbeddingModel); err != nil {
		return nil, nil, fmt.Errorf("ensure ollama embedding model: %w", err)
	}

	// Judgment generator: Claude CLI or Ollama depending on prefix
	judgment := parseJudgmentModel(userCfg.Judgment.Model, userCfg.Judgment.URL)

	// If judgment is Ollama, ensure that model too
	if om, ok := judgment.(*OllamaModel); ok {
		if om.Model != userCfg.Knowledge.EmbeddingModel {
			if err := ensureOllamaModel(om.URL, om.Model); err != nil {
				return nil, nil, fmt.Errorf("ensure ollama judgment model: %w", err)
			}
		}
	}

	stateDir, err := stateDir()
	if err != nil {
		return nil, nil, err
	}

	kbase, err := kb.Open(kb.Config{
		DBPath:         filepath.Join(stateDir, "kb.db"),
		Model:          embedModel,
		EmbeddingModel: userCfg.Knowledge.EmbeddingModel,
		Threshold:      userCfg.Knowledge.Threshold,
		MaxResults:     userCfg.Knowledge.MaxResults,
	})
	if err != nil {
		return nil, nil, err
	}

	return kbase, judgment, nil
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
