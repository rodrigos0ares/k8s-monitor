package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/insights"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

type Provider interface {
	Name() string
	Model() string
	Query(ctx context.Context, prompt string) (string, error)
	HealthCheck(ctx context.Context) error
}

// Analyzer uses a local LLM provider to analyze logs and provide insights.
type Analyzer struct {
	db       *storage.DB
	cfg      config.InferenceConfig
	cost     config.CostConfig
	provider Provider
	interval time.Duration
	logger   *slog.Logger
}


func New(db *storage.DB, cfg config.InferenceConfig, cost config.CostConfig, intervalMins int, logger *slog.Logger) (*Analyzer, error) {
	provider, err := newProvider(cfg)
	if err != nil {
		return nil, err
	}

	return &Analyzer{
		db:       db,
		cfg:      cfg,
		cost:     cost,
		provider: provider,
		interval: time.Duration(intervalMins) * time.Minute,
		logger:   logger,
	}, nil
}

// Run starts the periodic analysis loop. Blocks until ctx is cancelled.
func (a *Analyzer) Run(ctx context.Context) {
	a.logger.Info("starting analyzer", "interval", a.interval, "provider", a.provider.Name(), "model", a.provider.Model())

	// Wait a bit for initial data collection
	select {
	case <-time.After(2 * time.Minute):
	case <-ctx.Done():
		return
	}

	a.runAnalysis(ctx)

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("analyzer stopped")
			return
		case <-ticker.C:
			a.runAnalysis(ctx)
		}
	}
}

// RunOnce performs a single analysis cycle (used by CLI commands).
func (a *Analyzer) RunOnce(ctx context.Context) error {
	return a.runAnalysis(ctx)
}

// promptBudget returns the maximum number of characters a prompt may contain
// before it risks overflowing the model's context window.
func (a *Analyzer) promptBudget() int {
	if a.cfg.MaxPromptChars > 0 {
		return a.cfg.MaxPromptChars
	}
	return 16000
}

// writeLogLines appends formatted log lines to sb while *budget characters
// remain, decrementing budget as it goes. It returns the number of lines that
// were omitted because the budget was exhausted.
func writeLogLines(sb *strings.Builder, logs []storage.LogEntry, budget *int) int {
	omitted := 0
	for _, l := range logs {
		line := fmt.Sprintf("[%s] %s/%s/%s: %s\n", l.Context, l.Namespace, l.Pod, l.Container, truncate(l.LogLine, 300))
		if *budget-len(line) < 0 {
			omitted++
			continue
		}
		sb.WriteString(line)
		*budget -= len(line)
	}
	return omitted
}

func (a *Analyzer) runAnalysis(ctx context.Context) error {
	a.logger.Info("running analysis cycle")

	// Analyze errors and warnings from last 6 hours
	if err := a.analyzeErrors(ctx); err != nil {
		a.logger.Error("error analysis failed", "error", err)
	}

	// Analyze performance trends
	if err := a.analyzePerformance(ctx); err != nil {
		a.logger.Error("performance analysis failed", "error", err)
	}

	// Analyze resource efficiency / cost savings
	if err := a.analyzeResourceEfficiency(ctx); err != nil {
		a.logger.Error("resource efficiency analysis failed", "error", err)
	}

	a.logger.Info("analysis cycle complete")
	return nil
}

func (a *Analyzer) analyzeErrors(ctx context.Context) error {
	logs, err := a.db.GetRecentLogs(6, "", "error", 200)
	if err != nil {
		return fmt.Errorf("fetching error logs: %w", err)
	}

	warningLogs, err := a.db.GetRecentLogs(6, "", "warning", 100)
	if err != nil {
		return fmt.Errorf("fetching warning logs: %w", err)
	}

	if len(logs) == 0 && len(warningLogs) == 0 {
		a.logger.Info("no errors or warnings to analyze")
		return nil
	}

	// Build prompt with log samples
	var sb strings.Builder
	sb.WriteString("You are an expert Kubernetes SRE. Analyze the following error and warning logs from a Kubernetes cluster.\n\n")
	sb.WriteString("Provide:\n")
	sb.WriteString("1. A severity assessment (critical/high/medium/low)\n")
	sb.WriteString("2. A concise summary of the main issues\n")
	sb.WriteString("3. Root cause analysis for each distinct error pattern\n")
	sb.WriteString("4. Specific actionable recommendations to fix each issue\n")
	sb.WriteString("5. Any patterns that suggest systemic problems\n\n")

	// Reserve part of the prompt budget for the trailing summary section so it
	// is never dropped, and spend the rest on raw log lines.
	logBudget := a.promptBudget() - sb.Len() - 2000
	if logBudget < 1000 {
		logBudget = 1000
	}

	sb.WriteString("=== ERROR LOGS ===\n")
	errOmitted := writeLogLines(&sb, logs, &logBudget)

	sb.WriteString("\n=== WARNING LOGS ===\n")
	warnOmitted := writeLogLines(&sb, warningLogs, &logBudget)

	if errOmitted+warnOmitted > 0 {
		fmt.Fprintf(&sb, "\n(%d additional log lines omitted to fit the model context)\n", errOmitted+warnOmitted)
	}

	// Get error summary stats
	summary, err := a.db.GetErrorSummary(6, "")
	if err == nil && len(summary) > 0 {
		sb.WriteString("\n=== ERROR COUNTS PER POD (last 6h) ===\n")
		for _, s := range summary {
			fmt.Fprintf(&sb, "[%s] %s/%s: %d %s entries\n", s.Context, s.Namespace, s.Pod, s.Count, s.Severity)
		}
	}

	response, err := a.provider.Query(ctx, sb.String())
	if err != nil {
		return fmt.Errorf("querying %s: %w", a.provider.Name(), err)
	}

	severity := inferSeverity(response)
	ctxName := "all"
	if len(logs) > 0 {
		ctxName = logs[0].Context
	}

	return a.db.InsertAnalysis(storage.Analysis{
		Context:      ctxName,
		AnalysisType: "error_analysis",
		Summary:      extractFirstLine(response),
		Details:      response,
		Severity:     severity,
	})
}

func (a *Analyzer) analyzePerformance(ctx context.Context) error {
	stats, err := a.db.GetContextStats()
	if err != nil {
		return fmt.Errorf("fetching stats: %w", err)
	}
	if len(stats) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("You are an expert Kubernetes SRE. Analyze the following cluster performance data.\n\n")
	sb.WriteString("Provide:\n")
	sb.WriteString("1. Overall cluster health assessment\n")
	sb.WriteString("2. Performance bottlenecks or concerns\n")
	sb.WriteString("3. Resource optimization recommendations\n")
	sb.WriteString("4. Capacity planning insights\n")
	sb.WriteString("5. Any anomalies or trends to watch\n\n")

	sb.WriteString("=== CLUSTER STATS (last 24h) ===\n")
	for _, s := range stats {
		fmt.Fprintf(&sb, "Context: %s | Pods: %d | Total logs: %d | Errors: %d | Warnings: %d | Last collection: %s\n",
			s.Context, s.PodCount, s.TotalLogs, s.ErrorCount, s.WarningCount, s.LatestCollect.Format(time.RFC3339))
	}

	// Get DB stats for size insight
	dbStats, err := a.db.GetDBStats()
	if err == nil {
		sb.WriteString("\n=== DATA VOLUME ===\n")
		for table, count := range dbStats {
			fmt.Fprintf(&sb, "%s: %d records\n", table, count)
		}
	}

	response, err := a.provider.Query(ctx, sb.String())
	if err != nil {
		return fmt.Errorf("querying %s: %w", a.provider.Name(), err)
	}

	return a.db.InsertAnalysis(storage.Analysis{
		Context:      "all",
		AnalysisType: "performance_analysis",
		Summary:      extractFirstLine(response),
		Details:      response,
		Severity:     "info",
	})
}

// analyzeResourceEfficiency compares observed pod usage against configured
// requests/limits to surface cost-saving (rightsizing) opportunities.
func (a *Analyzer) analyzeResourceEfficiency(ctx context.Context) error {
	rows, err := a.db.GetRightsizingRows(24, 168, "")
	if err != nil {
		return fmt.Errorf("fetching rightsizing rows: %w", err)
	}
	if len(rows) == 0 {
		a.logger.Info("no resource/usage data to analyze for rightsizing")
		return nil
	}

	report := insights.BuildReport(rows, a.cost)
	if len(report.Recommendations) == 0 {
		a.logger.Info("no rightsizing opportunities detected")
		return nil
	}

	const topN = 20
	recs := report.Recommendations
	if len(recs) > topN {
		recs = recs[:topN]
	}

	var sb strings.Builder
	sb.WriteString("You are an expert Kubernetes FinOps/SRE engineer. Analyze these pod resource ")
	sb.WriteString("rightsizing findings (actual usage vs configured requests/limits) and produce a ")
	sb.WriteString("prioritized cost-optimization plan.\n\n")
	sb.WriteString("Provide:\n")
	sb.WriteString("1. URGENT: any under-provisioned workloads (kind=under-provisioned, peak usage exceeds request) that risk OOMKills/eviction or CPU starvation — list these first with recommended higher requests/limits\n")
	sb.WriteString("2. The biggest cost-saving opportunities and why\n")
	sb.WriteString("3. Concrete recommended CPU/memory request and limit values per workload\n")
	sb.WriteString("4. Any reliability risks (pods near memory/CPU limits) to fix before cutting\n")
	sb.WriteString("5. Workloads missing requests/limits that need them\n")
	sb.WriteString("6. A short summary of total estimated monthly savings\n\n")

	fmt.Fprintf(&sb, "=== ESTIMATED MONTHLY SAVINGS: %.2f %s ===\n", report.TotalMonthlySavings, report.Currency)
	fmt.Fprintf(&sb, "Reclaimable: %d mCPU, %d MB memory across %d/%d containers\n\n",
		report.ReclaimableCPUMilli, report.ReclaimableMemMB, report.ContainersFlagged, report.ContainersAnalyzed)

	sb.WriteString("=== TOP FINDINGS (last 24h) ===\n")
	for _, r := range recs {
		fmt.Fprintf(&sb, "[%s] %s/%s/%s | kind=%s\n", r.Context, r.Namespace, r.Workload, r.Container, r.Kind)
		fmt.Fprintf(&sb, "  CPU: peak %dm / req %dm (%.0f%% util) -> recommend %dm | reclaim %dm\n",
			r.MaxCPUMilli, r.CPURequestMilli, r.CPUUtilPct, r.RecommendedCPUMilli, r.ReclaimableCPUMilli)
		fmt.Fprintf(&sb, "  MEM: peak %dMB / req %dMB (%.0f%% util) -> recommend %dMB | reclaim %dMB\n",
			r.MaxMemMB, r.MemRequestMB, r.MemUtilPct, r.RecommendedMemMB, r.ReclaimableMemMB)
		fmt.Fprintf(&sb, "  est. monthly savings: %.2f %s\n", r.MonthlySavings, r.Currency)
	}

	response, err := a.provider.Query(ctx, sb.String())
	if err != nil {
		return fmt.Errorf("querying %s: %w", a.provider.Name(), err)
	}

	summary := fmt.Sprintf("Potential savings ~%.2f %s/mo across %d workloads",
		report.TotalMonthlySavings, report.Currency, report.ContainersFlagged)

	return a.db.InsertAnalysis(storage.Analysis{
		Context:      "all",
		AnalysisType: "resource_efficiency",
		Summary:      summary,
		Details:      response,
		Severity:     "info",
	})
}

type llamaCPPChatRequest struct {
	Model       string                    `json:"model"`
	Messages    []llamaCPPChatMessage     `json:"messages"`
	Temperature float64                   `json:"temperature,omitempty"`
	MaxTokens   int                       `json:"max_tokens,omitempty"`
	Stream      bool                      `json:"stream"`
}

type llamaCPPChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llamaCPPChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type llamaCPPModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type llamaCPPProvider struct {
	cfg    config.InferenceConfig
	client *http.Client
}

func (p *llamaCPPProvider) Name() string {
	return "llama.cpp"
}

func (p *llamaCPPProvider) Model() string {
	return p.cfg.Model
}

func (p *llamaCPPProvider) Query(ctx context.Context, prompt string) (string, error) {
	// Safety net: never send a prompt larger than the configured budget, which
	// would be rejected by the server for exceeding its context window.
	if max := p.cfg.MaxPromptChars; max > 0 && len(prompt) > max {
		prompt = prompt[:max] + "\n\n(prompt truncated to fit model context)"
	}
	url := strings.TrimRight(p.cfg.Endpoint, "/") + "/v1/chat/completions"
	reqBody := llamaCPPChatRequest{
		Model: p.cfg.Model,
		Messages: []llamaCPPChatMessage{
			{Role: "system", Content: "You are an expert Kubernetes SRE assistant. Be concise and actionable."},
			{Role: "user", Content: prompt},
		},
		Temperature: p.cfg.Temperature,
		MaxTokens:   p.cfg.MaxTokens,
		Stream:      false,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling llama.cpp at %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading llama.cpp response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llama.cpp returned status %d: %s", resp.StatusCode, string(body))
	}

	var result llamaCPPChatResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing llama.cpp response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("llama.cpp returned no choices")
	}

	return result.Choices[0].Message.Content, nil
}

func (p *llamaCPPProvider) HealthCheck(ctx context.Context) error {
	baseURL := strings.TrimRight(p.cfg.Endpoint, "/")
	healthURL := baseURL + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p.checkModelAvailable(ctx)
		}
	}

	return p.checkModelAvailable(ctx)
}

func (p *llamaCPPProvider) checkModelAvailable(ctx context.Context) error {
	url := strings.TrimRight(p.cfg.Endpoint, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot connect to llama.cpp at %s: %w", p.cfg.Endpoint, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp returned status %d: %s", resp.StatusCode, string(body))
	}

	if p.cfg.Model == "" {
		return nil
	}

	var models llamaCPPModelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		return fmt.Errorf("parsing llama.cpp models response: %w", err)
	}

	for _, m := range models.Data {
		if m.ID == p.cfg.Model || strings.HasPrefix(m.ID, p.cfg.Model) {
			return nil
		}
	}

	return fmt.Errorf("model %q not found in llama.cpp server /v1/models", p.cfg.Model)
}

func newProvider(cfg config.InferenceConfig) (Provider, error) {
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSecs) * time.Second}

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case "", "llama.cpp", "llamacpp":
		return &llamaCPPProvider{cfg: cfg, client: client}, nil
	case "langchain", "langchain-go":
		return langchain.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	default:
		return nil, fmt.Errorf("unsupported inference provider %q (supported: llama.cpp, langchain-go)", cfg.Provider)
	}
}

// CheckProviderHealth verifies the configured provider is reachable and model is available.
func CheckProviderHealth(cfg config.InferenceConfig) error {
	provider, err := newProvider(cfg)
	if err != nil {
		return err
	}
	return provider.HealthCheck(context.Background())
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractFirstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return truncate(s, 200)
}

func inferSeverity(response string) string {
	lower := strings.ToLower(response)
	switch {
	case strings.Contains(lower, "critical"):
		return "critical"
	case strings.Contains(lower, "high severity") || strings.Contains(lower, "high risk"):
		return "high"
	case strings.Contains(lower, "medium"):
		return "medium"
	default:
		return "low"
	}
}
