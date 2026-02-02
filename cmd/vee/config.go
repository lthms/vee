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
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/lthms/vee/internal/kb"
)

// UserConfig holds user-level configuration loaded from ~/.config/vee/config.toml.
type UserConfig struct {
	Judgment  JudgmentConfig  `toml:"judgment"`
	Knowledge KnowledgeConfig `toml:"knowledge"`
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
			Model: "qwen2.5:7b",
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
		cfg.Judgment.Model = "qwen2.5:7b"
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
func (o *OllamaModel) Generate(prompt string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":  o.Model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := http.Post(o.URL+"/api/generate", "application/json", bytes.NewReader(reqBody))
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

// ensureModels checks that both the judgment and embedding models are available
// in Ollama, pulling them if necessary.
func ensureModels(o *OllamaModel) error {
	models := []string{o.Model}
	if o.EmbeddingModel != "" && o.EmbeddingModel != o.Model {
		models = append(models, o.EmbeddingModel)
	}

	for _, model := range models {
		if err := ensureOllamaModel(o.URL, model); err != nil {
			return err
		}
	}
	return nil
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

// openKB creates the OllamaModel, ensures required models are available,
// and opens the knowledge base. Returns the KB and the judgment model separately
// so callers that need model.Generate don't go through the KB.
func openKB(userCfg *UserConfig) (*kb.KnowledgeBase, kb.Model, error) {
	model := &OllamaModel{
		URL:            userCfg.Judgment.URL,
		Model:          userCfg.Judgment.Model,
		EmbeddingModel: userCfg.Knowledge.EmbeddingModel,
	}

	if err := ensureModels(model); err != nil {
		return nil, nil, fmt.Errorf("ensure ollama models: %w", err)
	}

	stateDir, err := stateDir()
	if err != nil {
		return nil, nil, err
	}

	kbase, err := kb.Open(kb.Config{
		DBPath:         filepath.Join(stateDir, "kb.db"),
		Model:          model,
		EmbeddingModel: userCfg.Knowledge.EmbeddingModel,
		Threshold:      userCfg.Knowledge.Threshold,
		MaxResults:     userCfg.Knowledge.MaxResults,
	})
	if err != nil {
		return nil, nil, err
	}

	return kbase, model, nil
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
