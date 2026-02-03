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
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/lthms/vee/internal/kb"
)

// Generator is the interface for text generation (judgment evaluation).
// This is separate from kb.Model which also requires Embed().
type Generator interface {
	Generate(prompt string) (string, error)
}

// UserConfig holds user-level configuration loaded from ~/.config/vee/config.toml.
type UserConfig struct {
	Judgment  JudgmentConfig  `toml:"judgment"`
	Knowledge KnowledgeConfig `toml:"knowledge"`
	Identity  *IdentityConfig `toml:"identity"`
}

// IdentityConfig configures the assistant's identity (name + git author).
type IdentityConfig struct {
	Name    string `toml:"name"`
	Email   string `toml:"email"`
	Disable bool   `toml:"disable"`
}

// ProjectTOML represents the top-level .vee/config.toml structure.
type ProjectTOML struct {
	Ephemeral *EphemeralConfig `toml:"ephemeral"`
	Identity  *IdentityConfig  `toml:"identity"`
}

// readProjectTOML reads and parses .vee/config.toml from the current directory.
func readProjectTOML() (*ProjectTOML, error) {
	var cfg ProjectTOML
	_, err := toml.DecodeFile(".vee/config.toml", &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
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
	URL   string `toml:"url"`
	Model string `toml:"model"`
}

// KnowledgeConfig configures knowledge base settings.
type KnowledgeConfig struct {
	EmbeddingModel string  `toml:"embedding_model"`
	Threshold      float64 `toml:"threshold"`
	MaxResults     int     `toml:"max_results"`
}

// loadUserConfig reads ~/.config/vee/config.toml and returns the parsed config
// with defaults applied. If the file does not exist, defaults are returned with
// no error.
func loadUserConfig() (*UserConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}

	path := filepath.Join(home, ".config", "vee", "config.toml")

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

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Re-apply defaults for empty fields
	if cfg.Judgment.URL == "" {
		cfg.Judgment.URL = "http://localhost:11434"
	}
	if cfg.Judgment.Model == "" {
		cfg.Judgment.Model = "claude:haiku"
	}
	if cfg.Knowledge.EmbeddingModel == "" {
		cfg.Knowledge.EmbeddingModel = "nomic-embed-text"
	}
	if cfg.Knowledge.Threshold == 0 {
		cfg.Knowledge.Threshold = 0.3
	}
	if cfg.Knowledge.MaxResults == 0 {
		cfg.Knowledge.MaxResults = 10
	}

	return cfg, nil
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
