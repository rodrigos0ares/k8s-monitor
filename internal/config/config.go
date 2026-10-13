package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	// Kubernetes contexts to monitor
	Contexts []string `json:"contexts"`

	// Log retention in days
	RetentionDays int `json:"retention_days"`

	// Collection interval in seconds
	CollectIntervalSecs int `json:"collect_interval_secs"`

	// Analysis interval in minutes
	AnalysisIntervalMins int `json:"analysis_interval_mins"`

	// Inference configuration (provider/model/endpoint)
	Inference InferenceConfig `json:"inference"`

	// Storage configuration
	Storage StorageConfig `json:"storage"`

	// Web server configuration
	Web WebConfig `json:"web"`

	// Cost model used to estimate resource savings
	Cost CostConfig `json:"cost"`

	// Alert configuration
	Alerts AlertConfig `json:"alerts"`
}

type InferenceConfig struct {
	Provider    string  `json:"provider"`
	Endpoint    string  `json:"endpoint"`
	Model       string  `json:"model"`
	APIKey      string  `json:"api_key,omitempty"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	TimeoutSecs int     `json:"timeout_secs"`
	// MaxPromptChars caps the size of a prompt sent to the model so it stays
	// within the server's context window. 0 means use the default.
	MaxPromptChars int `json:"max_prompt_chars"`
}

type InferenceProfile struct {
	Name        string
	Description string
	Config      InferenceConfig
}

type StorageConfig struct {
	DBPath string `json:"db_path"`
}

type WebConfig struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"`
}

// CostConfig holds the pricing model used to translate reclaimable resources
// into estimated monetary savings.
type CostConfig struct {
	Currency string `json:"currency"`
	// CPUCoreHour is the cost of one vCPU core for one hour.
	CPUCoreHour float64 `json:"cpu_core_hour"`
	// MemGBHour is the cost of one GiB of memory for one hour.
	MemGBHour float64 `json:"mem_gb_hour"`
}

type AlertConfig struct {
	Slack   SlackConfig   `json:"slack"`
	Discord DiscordConfig `json:"discord"`
}

type SlackConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
}

type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Contexts:             []string{},
		RetentionDays:        15,
		CollectIntervalSecs:  60,
		AnalysisIntervalMins: 30,
		Inference: InferenceConfig{
			Provider:    "llama.cpp",
			Endpoint:    "http://localhost:8080",
			Model:       "qwen3.5-9b",
			Temperature:    0.2,
			MaxTokens:      1024,
			TimeoutSecs:    300,
			MaxPromptChars: 16000,
		},
		Storage: StorageConfig{
			DBPath: filepath.Join(home, ".k8s-monitor", "data.db"),
		},
		Web: WebConfig{
			Enabled: true,
			Address: ":3333",
		},
		Cost: CostConfig{
			Currency:    "USD",
			CPUCoreHour: 0.0316,
			MemGBHour:   0.0042,
		},
		Alerts: AlertConfig{},
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".k8s-monitor", "config.json")
}

func Load() (*Config, error) {
	cfg := DefaultConfig()
	path := configPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	migrateLegacy(cfg)
	return cfg, nil
}

func migrateLegacy(cfg *Config) {
	if cfg.Inference.Provider == "" {
		cfg.Inference.Provider = "llama.cpp"
	}

	if cfg.Inference.Endpoint == "" {
		cfg.Inference.Endpoint = "http://localhost:8080"
	}

	if cfg.Inference.Model == "" {
		cfg.Inference.Model = "qwen3.5-9b"
	}

	if cfg.Inference.Temperature == 0 {
		cfg.Inference.Temperature = 0.2
	}

	if cfg.Inference.MaxTokens == 0 {
		cfg.Inference.MaxTokens = 1024
	}

	if cfg.Inference.TimeoutSecs == 0 {
		cfg.Inference.TimeoutSecs = 300
	}

	if cfg.Inference.MaxPromptChars == 0 {
		cfg.Inference.MaxPromptChars = 16000
	}

	if cfg.Cost.Currency == "" {
		cfg.Cost.Currency = "USD"
	}
	if cfg.Cost.CPUCoreHour == 0 {
		cfg.Cost.CPUCoreHour = 0.0316
	}
	if cfg.Cost.MemGBHour == 0 {
		cfg.Cost.MemGBHour = 0.0042
	}
}

func BuiltinInferenceProfiles() map[string]InferenceProfile {
	return map[string]InferenceProfile{
		"balanced": {
			Name:        "balanced",
			Description: "Balanced coding/devops quality for RTX 4060-class GPUs",
			Config: InferenceConfig{
				Provider:    "llama.cpp",
				Endpoint:    "http://localhost:8080",
				Model:       "qwen3.5-9b",
				Temperature: 0.2,
				MaxTokens:   1024,
				TimeoutSecs: 300,
			},
		},
		"fast": {
			Name:        "fast",
			Description: "Lower latency fallback for constrained environments",
			Config: InferenceConfig{
				Provider:    "llama.cpp",
				Endpoint:    "http://localhost:8080",
				Model:       "phi3.5",
				Temperature: 0.1,
				MaxTokens:   768,
				TimeoutSecs: 180,
			},
		},
		"quality": {
			Name:        "quality",
			Description: "Higher quality profile for deeper analysis (use Q5 if available)",
			Config: InferenceConfig{
				Provider:    "llama.cpp",
				Endpoint:    "http://localhost:8080",
				Model:       "qwen3.5-9b-q5",
				Temperature: 0.2,
				MaxTokens:   1536,
				TimeoutSecs: 360,
			},
		},
	}
}

func ApplyInferenceProfile(c *Config, name string) error {
	profiles := BuiltinInferenceProfiles()
	key := strings.ToLower(strings.TrimSpace(name))
	p, ok := profiles[key]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}

	c.Inference = p.Config
	return nil
}

func (c *Config) Save() error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}
