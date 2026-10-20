// Package langchain provides an inference provider backed by LangChain Go.
//
// LangChain Go exposes an OpenAI-compatible HTTP API that can serve models
// via llama.cpp, Ollama, vLLM, or any other backend LangChain supports.
// This provider lets k8s-monitor route its analysis prompts through that
// gateway instead of talking directly to llama.cpp.
package langchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
)

// Provider implements the analyzer.Provider interface using LangChain Go.
type Provider struct {
	cfg     config.InferenceConfig
	logger  *slog.Logger
	client  *http.Client
	model   string
}

// New creates a new LangChain Go provider.
func New(cfg config.InferenceConfig, logger *slog.Logger) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("langchain endpoint is required")
	}

	return &Provider{
		cfg:    cfg,
		logger: logger,
		model:  cfg.Model,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSecs) * time.Second,
		},
	}, nil
}

// Name returns the provider name.
func (p *Provider) Name() string {
	return "langchain-go"
}

// Model returns the model name.
func (p *Provider) Model() string {
	return p.model
}

// Query sends a prompt to the LangChain Go endpoint and returns the response.
func (p *Provider) Query(ctx context.Context, prompt string) (string, error) {
	url := p.cfg.Endpoint
	if url == "" {
		url = "http://localhost:8080"
	}
	url = fmt.Sprintf("%s/v1/chat/completions", url)

	payload := map[string]interface{}{
		"model":       p.model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": p.cfg.Temperature,
		"max_tokens":  p.cfg.MaxTokens,
	}

	if p.cfg.MaxPromptChars > 0 {
		payload["max_prompt_chars"] = p.cfg.MaxPromptChars
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty response from langchain-go")
	}

	return result.Choices[0].Message.Content, nil
}

// HealthCheck verifies connectivity to the LangChain Go endpoint.
func (p *Provider) HealthCheck(ctx context.Context) error {
	url := p.cfg.Endpoint
	if url == "" {
		url = "http://localhost:8080"
	}
	url = fmt.Sprintf("%s/v1/models", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating health check request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("health check returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
