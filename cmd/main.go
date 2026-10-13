package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/alerts"
	"github.com/rodrigos0ares/k8s-monitor/internal/analyzer"
	"github.com/rodrigos0ares/k8s-monitor/internal/collector"
	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/insights"
	"github.com/rodrigos0ares/k8s-monitor/internal/k8s"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
	"github.com/rodrigos0ares/k8s-monitor/internal/web"
	"github.com/spf13/cobra"
)

var logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	rootCmd := &cobra.Command{
		Use:   "k8s-monitor",
		Short: "Local Kubernetes monitoring with AI-powered insights via local LLMs",
	}

	rootCmd.AddCommand(
		startCmd(),
		analyzeCmd(),
		benchmarkCmd(),
		statusCmd(),
		costsCmd(),
		configCmd(),
		contextsCmd(),
		purgeCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the monitoring daemon (log collection, metrics, analysis, web UI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if len(cfg.Contexts) == 0 {
				return fmt.Errorf("no contexts configured. Run: k8s-monitor config init")
			}

			// Start the local llama.cpp server so analysis works out of the box.
			startLlamaService()

			// Open database
			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			// Create K8s clients for each context
			clients, err := createClients(cfg.Contexts)
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Start log collector
			logCollector := collector.NewLogCollector(clients, db, cfg.CollectIntervalSecs, logger)
			go logCollector.Run(ctx)

			// Start metrics collector
			metricsCollector := collector.NewMetricsCollector(clients, db, cfg.CollectIntervalSecs, logger)
			go metricsCollector.Run(ctx)

			// Start analyzer
			az, err := analyzer.New(db, cfg.Inference, cfg.Cost, cfg.AnalysisIntervalMins, logger)
			if err != nil {
				return fmt.Errorf("creating analyzer: %w", err)
			}
			go az.Run(ctx)

			// Start purge routine (daily)
			go func() {
				ticker := time.NewTicker(24 * time.Hour)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := db.PurgeOldData(cfg.RetentionDays); err != nil {
							logger.Error("purge failed", "error", err)
						} else {
							logger.Info("purged old data", "retention_days", cfg.RetentionDays)
						}
					}
				}
			}()

			// Start stale-pod pruning routine so the dashboard drops pods (and
			// their deployments) that have been deleted from the cluster without
			// waiting for the 24h error window or retention purge. Runs once
			// shortly after startup, then periodically.
			go func() {
				firstRun := time.NewTimer(2 * time.Minute)
				defer firstRun.Stop()
				ticker := time.NewTicker(30 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-firstRun.C:
					case <-ticker.C:
					}
					pods, logRows := pruneStalePods(ctx, db, clients, logger)
					if pods > 0 {
						logger.Info("pruned stale pods", "pods", pods, "rows", logRows)
					}
				}
			}()

			// Start web server
			if cfg.Web.Enabled {
				webServer, err := web.NewServer(db, cfg.Cost, logger)
				if err != nil {
					return fmt.Errorf("creating web server: %w", err)
				}

				srv := &http.Server{
					Addr:              cfg.Web.Address,
					Handler:           webServer.Handler(),
					ReadHeaderTimeout: 10 * time.Second,
				}
				go func() {
					logger.Info("web dashboard started", "address", cfg.Web.Address)
					if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						logger.Error("web server error", "error", err)
					}
				}()
				defer func() {
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer shutdownCancel()
					srv.Shutdown(shutdownCtx)
				}()
			}

			logger.Info("k8s-monitor started",
				"contexts", cfg.Contexts,
				"retention_days", cfg.RetentionDays,
				"provider", cfg.Inference.Provider,
				"model", cfg.Inference.Model,
				"web_address", cfg.Web.Address)

			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Println("  ☸ K8s Monitor is running")
			fmt.Printf("  Contexts:  %s\n", strings.Join(cfg.Contexts, ", "))
			fmt.Printf("  Dashboard: http://localhost%s\n", cfg.Web.Address)
			fmt.Printf("  Model:     %s (via %s)\n", cfg.Inference.Model, cfg.Inference.Provider)
			fmt.Printf("  Retention: %d days\n", cfg.RetentionDays)
			fmt.Println("  Press Ctrl+C to stop")
			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			<-ctx.Done()
			logger.Info("shutting down...")
			return nil
		},
	}
}

func analyzeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "analyze",
		Short: "Run an AI analysis on collected data right now",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			// Check provider health
			fmt.Printf("Checking %s connection...\n", cfg.Inference.Provider)
			if err := analyzer.CheckProviderHealth(cfg.Inference); err != nil {
				return fmt.Errorf("provider check failed: %w", err)
			}
			fmt.Printf("✓ Provider connected (%s, model: %s)\n\n", cfg.Inference.Provider, cfg.Inference.Model)

			az, err := analyzer.New(db, cfg.Inference, cfg.Cost, 0, logger)
			if err != nil {
				return fmt.Errorf("creating analyzer: %w", err)
			}
			fmt.Println("Running analysis... (this may take a few minutes)")
			if err := az.RunOnce(context.Background()); err != nil {
				return err
			}

			// Show latest analyses
			analyses, err := db.GetRecentAnalyses(5)
			if err != nil {
				return err
			}

			for _, a := range analyses {
				printAnalysis(a)
			}
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current monitoring status and statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Println("  ☸ K8s Monitor Status")
			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			// DB stats
			dbStats, err := db.GetDBStats()
			if err == nil {
				fmt.Println("\n📊 Data Volume:")
				for table, count := range dbStats {
					fmt.Printf("   %-15s %d records\n", table, count)
				}
			}

			// Context stats
			stats, err := db.GetContextStats()
			if err == nil && len(stats) > 0 {
				fmt.Println("\n🔍 Cluster Overview (last 24h):")
				for _, s := range stats {
					fmt.Printf("   [%s] %d pods | %d logs | %d errors | %d warnings\n",
						s.Context, s.PodCount, s.TotalLogs, s.ErrorCount, s.WarningCount)
				}
			}

			// Error summary
			errors, err := db.GetErrorSummary(24, "")
			if err == nil && len(errors) > 0 {
				fmt.Println("\n⚠️  Top Error/Warning Pods:")
				for i, e := range errors {
					if i >= 10 {
						break
					}
					fmt.Printf("   [%s] %s/%s: %d %s\n", e.Context, e.Namespace, e.Pod, e.Count, e.Severity)
				}
			}

			// Recent analyses
			analyses, err := db.GetRecentAnalyses(3)
			if err == nil && len(analyses) > 0 {
				fmt.Println("\n🤖 Latest AI Insights:")
				for _, a := range analyses {
					printAnalysis(a)
				}
			}

			return nil
		},
	}
}

func costsCmd() *cobra.Command {
	var hours int
	var histHours int
	var ctxFilter string
	var limit int

	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Show pod rightsizing recommendations and estimated cost savings",
		Long:  "Compares observed pod CPU/memory usage against configured requests/limits to find over-provisioned workloads (savings) and under-provisioned workloads (reliability red flags), and estimates monthly savings.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := db.GetRightsizingRows(hours, histHours, ctxFilter)
			if err != nil {
				return err
			}

			report := insights.BuildReport(rows, cfg.Cost)

			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Println("  💰 Resource Efficiency & Cost Savings")
			fmt.Printf("  Window: last %dh | history: last %dh", hours, max(histHours, hours))
			if ctxFilter != "" {
				fmt.Printf(" | context: %s", ctxFilter)
			}
			fmt.Println()
			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			if report.ContainersAnalyzed == 0 {
				fmt.Println("\nNo usage+request data yet. Ensure metrics-server is installed and let the collector run.")
				return nil
			}

			fmt.Printf("\nEstimated monthly savings: %.2f %s\n", report.TotalMonthlySavings, report.Currency)
			fmt.Printf("Reclaimable: %d mCPU (~%.2f cores) | %d MB (~%.2f GB)\n",
				report.ReclaimableCPUMilli, float64(report.ReclaimableCPUMilli)/1000.0,
				report.ReclaimableMemMB, float64(report.ReclaimableMemMB)/1024.0)
			fmt.Printf("Flagged %d of %d containers analyzed\n", report.ContainersFlagged, report.ContainersAnalyzed)
			if report.ContainersUnderProvisioned > 0 {
				fmt.Printf("\U0001F6A9 %d UNDER-PROVISIONED workload(s) need more resources (shown first)\n", report.ContainersUnderProvisioned)
			}
			fmt.Println()

			recs := report.Recommendations
			if limit > 0 && len(recs) > limit {
				recs = recs[:limit]
			}

			printRec := func(r insights.Recommendation) {
				icon := map[insights.Kind]string{
					insights.KindUnderProvisioned: "🚩",
					insights.KindOverProvisioned:  "🟢",
					insights.KindMemoryRisk:       "🔴",
					insights.KindCPUThrottleRisk:  "🟠",
					insights.KindNoRequests:       "⚪",
				}[r.Kind]
				if icon == "" {
					icon = "•"
				}
				cpuNote := fmt.Sprintf("reclaim %dm", r.ReclaimableCPUMilli)
				if r.AdditionalCPUMilli > 0 {
					cpuNote = fmt.Sprintf("+%dm needed", r.AdditionalCPUMilli)
				}
				memNote := fmt.Sprintf("reclaim %dMB", r.ReclaimableMemMB)
				if r.AdditionalMemMB > 0 {
					memNote = fmt.Sprintf("+%dMB needed", r.AdditionalMemMB)
				}
				fmt.Printf("%s [%s] %s/%s/%s — %s\n", icon, r.Context, r.Namespace, r.Workload, r.Container, r.Kind)
				fmt.Printf("    CPU: avg %dm / peak %dm | hist avg %dm / peak %dm / req %dm (%.0f%% util) → recommend %dm (%s)\n",
					r.AvgCPUMilli, r.MaxCPUMilli, r.HistAvgCPUMilli, r.HistMaxCPUMilli,
					r.CPURequestMilli, r.CPUUtilPct, r.RecommendedCPUMilli, cpuNote)
				fmt.Printf("    MEM: avg %dMB / peak %dMB | hist avg %dMB / peak %dMB / req %dMB (%.0f%% util) → recommend %dMB (%s)\n",
					r.AvgMemMB, r.MaxMemMB, r.HistAvgMemMB, r.HistMaxMemMB,
					r.MemRequestMB, r.MemUtilPct, r.RecommendedMemMB, memNote)
				if r.MonthlySavings > 0 {
					fmt.Printf("    💵 est. savings: %.2f %s/mo\n", r.MonthlySavings, r.Currency)
				}
				fmt.Printf("    %s\n\n", r.Message)
			}

			var under, over []insights.Recommendation
			for _, r := range recs {
				if r.Kind == insights.KindUnderProvisioned {
					under = append(under, r)
				} else {
					over = append(over, r)
				}
			}

			if len(under) > 0 {
				fmt.Println("🚩 UNDER-PROVISIONED — usage exceeds request (fix first)")
				fmt.Println("────────────────────────────────────────────────────")
				for _, r := range under {
					printRec(r)
				}
			}
			if len(over) > 0 {
				fmt.Println("🟢 OVER-PROVISIONED & OTHER FINDINGS")
				fmt.Println("────────────────────────────────────────────────────")
				for _, r := range over {
					printRec(r)
				}
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&hours, "hours", 24, "Look-back window in hours for usage data")
	cmd.Flags().IntVar(&histHours, "history-hours", 168, "Historical window in hours used to avoid downsizing below past peaks (default 7d)")
	cmd.Flags().StringVar(&ctxFilter, "context", "", "Limit to a single kubectl context")
	cmd.Flags().IntVar(&limit, "limit", 25, "Max number of recommendations to print (0 = all)")
	return cmd
}

func benchmarkCmd() *cobra.Command {
	var profile string
	var outputPath string
	var saveBest bool

	cmd := &cobra.Command{
		Use:   "benchmark [model ...]",
		Short: "Benchmark one or more models for devops/coding analysis prompts",
		Long:  "Runs a small benchmark suite against current provider and endpoint. Pass models to compare candidates.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			if profile != "" {
				if err := config.ApplyInferenceProfile(cfg, profile); err != nil {
					return err
				}
			}

			models := args
			if len(models) == 0 {
				models = []string{cfg.Inference.Model}
			}

			prompts := analyzer.DefaultBenchmarkPrompts()
			type scoredSummary struct {
				model   string
				score   float64
				quality float64
				latency float64
				success string
			}
			type modelReport struct {
				Model   string                    `json:"model"`
				Score   float64                   `json:"score"`
				Summary analyzer.BenchmarkSummary `json:"summary"`
				Results []analyzer.BenchmarkResult `json:"results"`
			}
			type benchmarkReport struct {
				CreatedAt time.Time      `json:"created_at"`
				Provider  string         `json:"provider"`
				Endpoint  string         `json:"endpoint"`
				Profile   string         `json:"profile,omitempty"`
				Prompts   int            `json:"prompts"`
				Models    []modelReport  `json:"models"`
			}

			summaries := make([]scoredSummary, 0, len(models))
			report := benchmarkReport{
				CreatedAt: time.Now().UTC(),
				Provider:  cfg.Inference.Provider,
				Endpoint:  cfg.Inference.Endpoint,
				Profile:   profile,
				Prompts:   len(prompts),
				Models:    make([]modelReport, 0, len(models)),
			}

			fmt.Println("Running benchmark suite...")
			fmt.Printf("Provider: %s | Endpoint: %s | Prompts: %d\n\n", cfg.Inference.Provider, cfg.Inference.Endpoint, len(prompts))

			for _, model := range models {
				runCfg := cfg.Inference
				runCfg.Model = model

				fmt.Printf("▶ Model: %s\n", model)
				results, summary, err := analyzer.RunBenchmark(context.Background(), runCfg, prompts)
				if err != nil {
					fmt.Printf("  ✗ failed: %v\n\n", err)
					report.Models = append(report.Models, modelReport{Model: model, Score: -1})
					summaries = append(summaries, scoredSummary{
						model:   model,
						score:   -1,
						quality: 0,
						latency: 0,
						success: "0/0",
					})
					continue
				}

				for _, r := range results {
					if r.Error != "" {
						fmt.Printf("  - %-24s error: %s\n", r.PromptName, r.Error)
						continue
					}
					fmt.Printf("  - %-24s %6.2fs | quality %3d | %4d chars\n",
						r.PromptName,
						r.Duration.Seconds(),
						r.QualityScore,
						r.OutputChars,
					)
				}

				fmt.Printf("  Summary: success %d/%d | avg latency %.2fs | avg quality %.1f | avg chars %.0f\n\n",
					summary.SuccessfulRuns,
					summary.Runs,
					summary.AverageDuration.Seconds(),
					summary.AverageQuality,
					summary.AverageOutputChars,
				)

				latencySeconds := summary.AverageDuration.Seconds()
				if latencySeconds <= 0 {
					latencySeconds = 999
				}
				latencyScore := 100.0 / latencySeconds
				overall := (0.5 * summary.AverageQuality) + (0.3 * latencyScore) + (0.2 * (100.0 * float64(summary.SuccessfulRuns) / float64(summary.Runs)))

				report.Models = append(report.Models, modelReport{
					Model:   model,
					Score:   overall,
					Summary: summary,
					Results: results,
				})

				summaries = append(summaries, scoredSummary{
					model:   model,
					score:   overall,
					quality: summary.AverageQuality,
					latency: summary.AverageDuration.Seconds(),
					success: fmt.Sprintf("%d/%d", summary.SuccessfulRuns, summary.Runs),
				})
			}

			fmt.Println("Ranking (higher is better):")
			sort.Slice(summaries, func(i, j int) bool {
				return summaries[i].score > summaries[j].score
			})

			for i, s := range summaries {
				fmt.Printf("%d. %-24s score %7.2f | quality %5.1f | latency %6.2fs | success %s\n",
					i+1, s.model, s.score, s.quality, s.latency, s.success)
			}

			if saveBest && len(summaries) > 0 && summaries[0].score >= 0 {
				cfg.Inference.Model = summaries[0].model
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("saving best model to config: %w", err)
				}
				fmt.Printf("\n✓ Saved best model to config: %s\n", summaries[0].model)
			}

			if outputPath != "" {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling benchmark report: %w", err)
				}

				output := outputPath
				if !filepath.IsAbs(output) {
					cwd, cwdErr := os.Getwd()
					if cwdErr != nil {
						return fmt.Errorf("getting current dir: %w", cwdErr)
					}
					output = filepath.Join(cwd, output)
				}

				dir := filepath.Dir(output)
				if err := os.MkdirAll(dir, 0750); err != nil {
					return fmt.Errorf("creating output dir: %w", err)
				}

				if err := os.WriteFile(output, data, 0600); err != nil {
					return fmt.Errorf("writing benchmark report: %w", err)
				}
				fmt.Printf("\n✓ Benchmark report written to %s\n", output)
			}

			fmt.Println("\nNote: memory usage is not auto-measured in this command. Monitor VRAM/RAM externally while benchmarking.")
			return nil
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "Apply a built-in inference profile before running benchmarks")
	cmd.Flags().StringVar(&outputPath, "output", "", "Write benchmark report JSON to file")
	cmd.Flags().BoolVar(&saveBest, "save-best", false, "Save the top-ranked model into current config")
	return cmd
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Initialize configuration with auto-detected kubectl contexts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.DefaultConfig()

			contexts, err := k8s.GetAvailableContexts()
			if err != nil {
				return fmt.Errorf("detecting contexts: %w", err)
			}

			if len(contexts) == 0 {
				return fmt.Errorf("no kubectl contexts found in kubeconfig")
			}

			cfg.Contexts = contexts
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Println("✓ Configuration created!")
			fmt.Printf("  Contexts detected: %s\n", strings.Join(contexts, ", "))
			fmt.Printf("  Provider: %s\n", cfg.Inference.Provider)
			fmt.Printf("  Model: %s\n", cfg.Inference.Model)
			fmt.Printf("  Retention: %d days\n", cfg.RetentionDays)
			fmt.Printf("  Database: %s\n", cfg.Storage.DBPath)
			fmt.Printf("  Dashboard: http://localhost%s\n", cfg.Web.Address)
			fmt.Println("\nNext steps:")
			fmt.Printf("  1. Start your llama.cpp OpenAI-compatible server\n")
			fmt.Printf("  2. Verify endpoint: %s\n", cfg.Inference.Endpoint)
			fmt.Printf("  3. Start monitoring: k8s-monitor start\n")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show current configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Println("  ☸ K8s Monitor Configuration")
			fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Printf("  Contexts:      %s\n", strings.Join(cfg.Contexts, ", "))
			fmt.Printf("  Retention:     %d days\n", cfg.RetentionDays)
			fmt.Printf("  Collect every: %ds\n", cfg.CollectIntervalSecs)
			fmt.Printf("  Analyze every: %dm\n", cfg.AnalysisIntervalMins)
			fmt.Printf("  Provider:      %s\n", cfg.Inference.Provider)
			fmt.Printf("  Endpoint:      %s\n", cfg.Inference.Endpoint)
			fmt.Printf("  Model:         %s\n", cfg.Inference.Model)
			fmt.Printf("  Temperature:   %.2f\n", cfg.Inference.Temperature)
			fmt.Printf("  Max tokens:    %d\n", cfg.Inference.MaxTokens)
			fmt.Printf("  Timeout:       %ds\n", cfg.Inference.TimeoutSecs)
			fmt.Printf("  Database:      %s\n", cfg.Storage.DBPath)
			fmt.Printf("  Web enabled:   %v\n", cfg.Web.Enabled)
			fmt.Printf("  Web address:   %s\n", cfg.Web.Address)
			fmt.Printf("  Slack:         %v\n", cfg.Alerts.Slack.Enabled)
			fmt.Printf("  Discord:       %v\n", cfg.Alerts.Discord.Enabled)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-model [model]",
		Short: "Set the inference model (e.g., qwen3.5-9b, qwen3.5-9b-q5)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.Inference.Model = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("✓ Model set to %s\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-provider [provider]",
		Short: "Set inference provider (llama.cpp)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := strings.ToLower(strings.TrimSpace(args[0]))
			if provider != "llama.cpp" && provider != "llamacpp" {
				return fmt.Errorf("unsupported provider %q (supported: llama.cpp)", args[0])
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			provider = "llama.cpp"

			cfg.Inference.Provider = provider
			if cfg.Inference.Endpoint == "" {
				cfg.Inference.Endpoint = "http://localhost:8080"
			}

			if err := cfg.Save(); err != nil {
				return err
			}

			fmt.Printf("✓ Provider set to %s\n", provider)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list-profiles",
		Short: "List built-in inference profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			profiles := config.BuiltinInferenceProfiles()
			fmt.Println("Built-in inference profiles:")
			for _, name := range []string{"balanced", "quality", "fast"} {
				p := profiles[name]
				fmt.Printf("  %-15s provider=%-9s model=%-22s endpoint=%s\n", p.Name, p.Config.Provider, p.Config.Model, p.Config.Endpoint)
				fmt.Printf("  %-15s %s\n", "", p.Description)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-profile [name]",
		Short: "Apply a built-in inference profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			if err := config.ApplyInferenceProfile(cfg, args[0]); err != nil {
				return err
			}

			if err := cfg.Save(); err != nil {
				return err
			}

			fmt.Printf("✓ Profile applied: %s\n", args[0])
			fmt.Printf("  Provider: %s\n", cfg.Inference.Provider)
			fmt.Printf("  Endpoint: %s\n", cfg.Inference.Endpoint)
			fmt.Printf("  Model: %s\n", cfg.Inference.Model)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-endpoint [url]",
		Short: "Set inference endpoint (e.g., http://localhost:8080)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.Inference.Endpoint = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}

			fmt.Printf("✓ Endpoint set to %s\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-slack [webhook-url]",
		Short: "Enable Slack notifications with a webhook URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.Alerts.Slack.Enabled = true
			cfg.Alerts.Slack.WebhookURL = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Println("✓ Slack notifications enabled")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-discord [webhook-url]",
		Short: "Enable Discord notifications with a webhook URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.Alerts.Discord.Enabled = true
			cfg.Alerts.Discord.WebhookURL = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Println("✓ Discord notifications enabled")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set-cost [cpu-core-hour] [mem-gb-hour] [currency]",
		Short: "Set the cost model used to estimate resource savings",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cpuHour, err := strconv.ParseFloat(args[0], 64)
			if err != nil {
				return fmt.Errorf("invalid cpu-core-hour %q: %w", args[0], err)
			}
			memHour, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("invalid mem-gb-hour %q: %w", args[1], err)
			}

			cfg.Cost.CPUCoreHour = cpuHour
			cfg.Cost.MemGBHour = memHour
			if len(args) == 3 {
				cfg.Cost.Currency = args[2]
			}

			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("✓ Cost model set: %.4f %s/core-hour, %.4f %s/GB-hour\n",
				cfg.Cost.CPUCoreHour, cfg.Cost.Currency, cfg.Cost.MemGBHour, cfg.Cost.Currency)
			return nil
		},
	})

	return cmd
}

func contextsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contexts",
		Short: "List available kubectl contexts",
		RunE: func(cmd *cobra.Command, args []string) error {
			contexts, err := k8s.GetAvailableContexts()
			if err != nil {
				return err
			}

			cfg, _ := config.Load()
			monitored := make(map[string]bool)
			if cfg != nil {
				for _, c := range cfg.Contexts {
					monitored[c] = true
				}
			}

			fmt.Println("Available kubectl contexts:")
			for _, c := range contexts {
				marker := "  "
				if monitored[c] {
					marker = "✓ "
				}
				fmt.Printf("  %s%s\n", marker, c)
			}
			return nil
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list-stored",
		Short: "List contexts that have data stored in the database (as shown on the dashboard)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			counts, err := db.GetAllStoredContexts()
			if err != nil {
				return err
			}
			if len(counts) == 0 {
				fmt.Println("No stored context data yet.")
				return nil
			}

			monitored := make(map[string]bool)
			for _, c := range cfg.Contexts {
				monitored[c] = true
			}

			fmt.Println("Stored contexts (records | ✓ = currently monitored):")
			names := make([]string, 0, len(counts))
			for name := range counts {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				marker := "  "
				if monitored[name] {
					marker = "✓ "
				}
				fmt.Printf("  %s%-30s %d records\n", marker, name, counts[name])
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "delete [context]",
		Short: "Delete all stored data for a context (removes it from the dashboard)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			deleted, err := db.DeleteContextData(target)
			if err != nil {
				return err
			}

			var total int64
			for _, n := range deleted {
				total += n
			}
			if total == 0 {
				fmt.Printf("No stored data found for context %q.\n", target)
				return nil
			}

			fmt.Printf("✓ Deleted stored data for context %q\n", target)
			for table, n := range deleted {
				if n > 0 {
					fmt.Printf("  %-15s %d records\n", table, n)
				}
			}

			// Also drop it from the monitored list so it is not re-collected.
			filtered := cfg.Contexts[:0]
			removedFromConfig := false
			for _, c := range cfg.Contexts {
				if c == target {
					removedFromConfig = true
					continue
				}
				filtered = append(filtered, c)
			}
			if removedFromConfig {
				cfg.Contexts = filtered
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("removing context from config: %w", err)
				}
				fmt.Printf("  Also removed %q from monitored contexts in config.\n", target)
			}
			return nil
		},
	})

	return cmd
}

func purgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete data older than retention period",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			before, _ := db.GetDBStats()
			if err := db.PurgeOldData(cfg.RetentionDays); err != nil {
				return err
			}
			after, _ := db.GetDBStats()

			fmt.Printf("✓ Purged data older than %d days\n", cfg.RetentionDays)
			for table := range before {
				fmt.Printf("  %-15s %d → %d records\n", table, before[table], after[table])
			}
			return nil
		},
	}

	cmd.AddCommand(purgePodsCmd())
	return cmd
}

func purgePodsCmd() *cobra.Command {
	var ctxFilter string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "pods",
		Short: "Delete stored data for pods that no longer exist in the cluster",
		Long:  "Connects to each monitored kubectl context, lists live pods, and deletes stored logs, metrics, and resource specs for any pod that is no longer present. Contexts that cannot be reached are skipped (their data is never deleted).",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.Storage.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			// Determine which contexts to check.
			contexts := cfg.Contexts
			if ctxFilter != "" {
				contexts = []string{ctxFilter}
			}
			if len(contexts) == 0 {
				return fmt.Errorf("no contexts configured; set monitored contexts or pass --context")
			}

			ctx := context.Background()
			var totalPods, totalWorkloads, totalRows int64

			for _, ctxName := range contexts {
				client, err := k8s.NewClient(ctxName)
				if err != nil {
					fmt.Printf("⚠️  skipping context %q (cannot connect): %v\n", ctxName, err)
					continue
				}

				livePods, err := client.ListAllPods(ctx)
				if err != nil {
					fmt.Printf("⚠️  skipping context %q (cannot list pods): %v\n", ctxName, err)
					continue
				}

				live := make(map[string]bool, len(livePods))
				for _, p := range livePods {
					live[p.Namespace+"/"+p.Name] = true
				}
				liveWorkloads, err := client.ListLiveWorkloads(ctx)
				if err != nil {
					fmt.Printf("⚠️  skipping context %q (cannot list workloads): %v\n", ctxName, err)
					continue
				}

				// Prune logs for pods that no longer exist.
				storedPods, err := db.GetDistinctLogPods(ctxName)
				if err != nil {
					return fmt.Errorf("listing stored pods for %s: %w", ctxName, err)
				}
				var ctxPods, ctxRows int64
				for _, p := range storedPods {
					if live[p.Namespace+"/"+p.Pod] {
						continue
					}
					if dryRun {
						fmt.Printf("  would delete logs: [%s] %s/%s\n", ctxName, p.Namespace, p.Pod)
						ctxPods++
						continue
					}
					n, err := db.DeleteLogsForPod(p.Context, p.Namespace, p.Pod)
					if err != nil {
						return fmt.Errorf("deleting logs for %s/%s: %w", p.Namespace, p.Pod, err)
					}
					ctxPods++
					ctxRows += n
				}

				// Prune metrics/resource specs for workloads with no live pods
				// (rollouts keep at least one live pod, so their history is kept).
				storedWorkloads, err := db.GetDistinctResourceWorkloads(ctxName)
				if err != nil {
					return fmt.Errorf("listing stored workloads for %s: %w", ctxName, err)
				}
				var ctxWorkloads int64
				for _, w := range storedWorkloads {
					if liveWorkloads[w.Namespace+"/"+w.Workload] {
						continue
					}
					if dryRun {
						fmt.Printf("  would delete metrics: [%s] %s/%s\n", ctxName, w.Namespace, w.Workload)
						ctxWorkloads++
						continue
					}
					n, err := db.DeleteResourceDataForWorkload(w.Context, w.Namespace, w.Workload)
					if err != nil {
						return fmt.Errorf("deleting metrics for %s/%s: %w", w.Namespace, w.Workload, err)
					}
					ctxWorkloads++
					ctxRows += n
				}

				totalPods += ctxPods
				totalWorkloads += ctxWorkloads
				totalRows += ctxRows
				if dryRun {
					fmt.Printf("[%s] %d stale pod(s) + %d stale workload(s) would be pruned\n", ctxName, ctxPods, ctxWorkloads)
				} else {
					fmt.Printf("[%s] pruned %d stale pod(s) + %d stale workload(s), %d rows\n", ctxName, ctxPods, ctxWorkloads, ctxRows)
				}
			}

			if dryRun {
				fmt.Printf("\nDry run: %d stale pod(s) + %d stale workload(s) across all contexts. Re-run without --dry-run to delete.\n", totalPods, totalWorkloads)
			} else {
				fmt.Printf("\n✓ Pruned %d stale pod(s) + %d stale workload(s) (%d rows deleted).\n", totalPods, totalWorkloads, totalRows)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&ctxFilter, "context", "", "Limit to a single kubectl context")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be deleted without deleting")
	return cmd
}

const llamaServiceName = "k8s-monitor-llama.service"

// startLlamaService starts the local llama.cpp systemd user service so that
// inference is available when the monitor runs. It is best-effort: failures are
// logged but do not stop the monitor from starting.
func startLlamaService() {
	if _, err := exec.LookPath("systemctl"); err != nil {
		logger.Warn("systemctl not found; skipping llama.cpp service start", "error", err)
		return
	}

	cmd := exec.Command("systemctl", "--user", "start", llamaServiceName)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("could not start llama.cpp service",
			"service", llamaServiceName,
			"error", err,
			"output", strings.TrimSpace(string(out)))
		return
	}

	logger.Info("started llama.cpp service", "service", llamaServiceName)
}

func createClients(contexts []string) ([]*k8s.Client, error) {
	var clients []*k8s.Client
	for _, ctx := range contexts {
		client, err := k8s.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("creating client for context %s: %w", ctx, err)
		}
		clients = append(clients, client)
		logger.Info("connected to context", "context", ctx)
	}
	return clients, nil
}

// pruneStalePods deletes stored logs for pods that no longer exist in the
// cluster, across the given clients. This keeps the dashboard in sync when pods
// or their deployments are removed. It is best-effort: a context that cannot be
// listed is skipped so its history is preserved. Returns the number of stale
// pods and log rows deleted.
func pruneStalePods(ctx context.Context, db *storage.DB, clients []*k8s.Client, logger *slog.Logger) (int64, int64) {
	var totalItems, totalRows int64
	for _, client := range clients {
		livePods, err := client.ListAllPods(ctx)
		if err != nil {
			logger.Warn("skipping stale-pod prune (cannot list pods)",
				"context", client.ContextName, "error", err)
			continue
		}

		live := make(map[string]bool, len(livePods))
		for _, p := range livePods {
			live[p.Namespace+"/"+p.Name] = true
		}
		liveWorkloads, err := client.ListLiveWorkloads(ctx)
		if err != nil {
			logger.Warn("skipping stale-workload prune (cannot list workloads)",
				"context", client.ContextName, "error", err)
			continue
		}

		storedPods, err := db.GetDistinctLogPods(client.ContextName)
		if err != nil {
			logger.Error("listing stored pods failed", "context", client.ContextName, "error", err)
			continue
		}
		for _, p := range storedPods {
			if live[p.Namespace+"/"+p.Pod] {
				continue
			}
			n, err := db.DeleteLogsForPod(p.Context, p.Namespace, p.Pod)
			if err != nil {
				logger.Error("deleting stale pod logs failed",
					"context", p.Context, "pod", p.Namespace+"/"+p.Pod, "error", err)
				continue
			}
			totalItems++
			totalRows += n
		}

		storedWorkloads, err := db.GetDistinctResourceWorkloads(client.ContextName)
		if err != nil {
			logger.Error("listing stored workloads failed", "context", client.ContextName, "error", err)
			continue
		}
		for _, w := range storedWorkloads {
			if liveWorkloads[w.Namespace+"/"+w.Workload] {
				continue
			}
			n, err := db.DeleteResourceDataForWorkload(w.Context, w.Namespace, w.Workload)
			if err != nil {
				logger.Error("deleting stale workload metrics failed",
					"context", w.Context, "workload", w.Namespace+"/"+w.Workload, "error", err)
				continue
			}
			totalItems++
			totalRows += n
		}
	}
	return totalItems, totalRows
}

func printAnalysis(a storage.Analysis) {
	severityIcon := map[string]string{
		"critical": "🔴", "high": "🟠", "medium": "🟡", "low": "🟢", "info": "🔵",
	}
	icon := severityIcon[a.Severity]
	if icon == "" {
		icon = "⚪"
	}
	fmt.Printf("\n%s [%s] %s — %s\n", icon, a.Severity, a.AnalysisType, a.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println(a.Details)
	fmt.Println()
}

// ensure notifier import is used
var _ = alerts.NewNotifier
