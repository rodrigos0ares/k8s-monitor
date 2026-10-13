package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
)

type BenchmarkPrompt struct {
	Name   string
	Prompt string
}

type BenchmarkResult struct {
	Model         string
	PromptName    string
	Duration      time.Duration
	OutputChars   int
	QualityScore  int
	Error         string
	OutputPreview string
}

type BenchmarkSummary struct {
	Model              string
	Runs               int
	SuccessfulRuns     int
	AverageDuration    time.Duration
	AverageQuality     float64
	AverageOutputChars float64
}

func DefaultBenchmarkPrompts() []BenchmarkPrompt {
	return []BenchmarkPrompt{
		{
			Name: "CrashLoopBackOff diagnosis",
			Prompt: "Kubernetes pod in CrashLoopBackOff with repeated readiness probe failures and OOMKilled events. Provide root cause hypotheses and a prioritized remediation plan.",
		},
		{
			Name: "Node pressure triage",
			Prompt: "Cluster has increasing memory pressure and frequent pod evictions in one node pool. Provide immediate mitigation steps and 24h follow-up actions.",
		},
		{
			Name: "Latency regression",
			Prompt: "Service latency increased 4x after a rollout. Logs show intermittent 503 and timeout errors. Explain likely causes and give a rollback/canary response plan.",
		},
	}
}

func RunBenchmark(ctx context.Context, cfg config.InferenceConfig, prompts []BenchmarkPrompt) ([]BenchmarkResult, BenchmarkSummary, error) {
	provider, err := newProvider(cfg)
	if err != nil {
		return nil, BenchmarkSummary{}, err
	}

	if err := provider.HealthCheck(ctx); err != nil {
		return nil, BenchmarkSummary{}, fmt.Errorf("provider health check failed: %w", err)
	}

	results := make([]BenchmarkResult, 0, len(prompts))
	var totalDuration time.Duration
	var totalQuality int
	var totalChars int
	success := 0

	for _, p := range prompts {
		start := time.Now()
		response, qErr := provider.Query(ctx, p.Prompt)
		dur := time.Since(start)

		if qErr != nil {
			results = append(results, BenchmarkResult{
				Model:      cfg.Model,
				PromptName: p.Name,
				Duration:   dur,
				Error:      qErr.Error(),
			})
			continue
		}

		score := heuristicQualityScore(response)
		preview := strings.TrimSpace(response)
		if len(preview) > 160 {
			preview = preview[:160] + "..."
		}

		results = append(results, BenchmarkResult{
			Model:         cfg.Model,
			PromptName:    p.Name,
			Duration:      dur,
			OutputChars:   len(response),
			QualityScore:  score,
			OutputPreview: preview,
		})
		totalDuration += dur
		totalQuality += score
		totalChars += len(response)
		success++
	}

	summary := BenchmarkSummary{
		Model:          cfg.Model,
		Runs:           len(prompts),
		SuccessfulRuns: success,
	}
	if success > 0 {
		summary.AverageDuration = totalDuration / time.Duration(success)
		summary.AverageQuality = float64(totalQuality) / float64(success)
		summary.AverageOutputChars = float64(totalChars) / float64(success)
	}

	return results, summary, nil
}

func heuristicQualityScore(text string) int {
	lower := strings.ToLower(text)
	score := 0

	if len(strings.TrimSpace(text)) > 200 {
		score += 20
	}
	if len(strings.TrimSpace(text)) > 600 {
		score += 10
	}

	keywords := []string{
		"root cause", "recommend", "mitigation", "rollback", "canary", "resource",
		"kubernetes", "pod", "node", "latency", "timeout", "error", "warning",
	}
	for _, k := range keywords {
		if strings.Contains(lower, k) {
			score += 5
		}
	}

	if strings.Count(text, "\n") > 4 {
		score += 10
	}
	if strings.Contains(text, "1.") || strings.Contains(text, "- ") {
		score += 10
	}

	if score > 100 {
		return 100
	}
	return score
}
