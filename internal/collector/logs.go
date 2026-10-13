package collector

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/k8s"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

// LogCollector periodically collects logs from all pods across all contexts.
type LogCollector struct {
	clients  []*k8s.Client
	db       *storage.DB
	interval time.Duration
	logger   *slog.Logger
}

func NewLogCollector(clients []*k8s.Client, db *storage.DB, intervalSecs int, logger *slog.Logger) *LogCollector {
	return &LogCollector{
		clients:  clients,
		db:       db,
		interval: time.Duration(intervalSecs) * time.Second,
		logger:   logger,
	}
}

// Run starts the collection loop. It blocks until ctx is cancelled.
func (c *LogCollector) Run(ctx context.Context) {
	c.logger.Info("starting log collector", "interval", c.interval, "contexts", len(c.clients))
	c.collectAll(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("log collector stopped")
			return
		case <-ticker.C:
			c.collectAll(ctx)
		}
	}
}

func (c *LogCollector) collectAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, client := range c.clients {
		wg.Add(1)
		go func(cl *k8s.Client) {
			defer wg.Done()
			c.collectFromContext(ctx, cl)
		}(client)
	}
	wg.Wait()
}

func (c *LogCollector) collectFromContext(ctx context.Context, client *k8s.Client) {
	pods, err := client.ListAllPods(ctx)
	if err != nil {
		c.logger.Error("failed to list pods", "context", client.ContextName, "error", err)
		return
	}

	c.logger.Info("collecting logs", "context", client.ContextName, "pods", len(pods))

	var allEntries []storage.LogEntry
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Use semaphore to limit concurrent log fetches
	sem := make(chan struct{}, 10)

	for _, pod := range pods {
		if pod.Status.Phase != "Running" {
			continue
		}
		for _, container := range pod.Spec.Containers {
			wg.Add(1)
			go func(ns, podName, containerName string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				logs, err := client.PodLogs(ctx, ns, podName, containerName, c.interval+10*time.Second)
				if err != nil {
					c.logger.Debug("failed to get logs", "pod", podName, "container", containerName, "error", err)
					return
				}

				if logs == "" {
					return
				}

				entries := parseLogLines(client.ContextName, ns, podName, containerName, logs)
				mu.Lock()
				allEntries = append(allEntries, entries...)
				mu.Unlock()
			}(pod.Namespace, pod.Name, container.Name)
		}
	}

	wg.Wait()

	if len(allEntries) > 0 {
		if err := c.db.InsertLogs(allEntries); err != nil {
			c.logger.Error("failed to insert logs", "context", client.ContextName, "error", err)
		} else {
			c.logger.Info("logs collected", "context", client.ContextName, "entries", len(allEntries))
		}
	}

	// Also collect events
	c.collectEvents(ctx, client)
}

func (c *LogCollector) collectEvents(ctx context.Context, client *k8s.Client) {
	events, err := client.GetEvents(ctx, c.interval+10*time.Second)
	if err != nil {
		c.logger.Error("failed to get events", "context", client.ContextName, "error", err)
		return
	}

	for _, e := range events {
		obj := e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
		if err := c.db.InsertEvent(client.ContextName, e.Namespace, obj, e.Reason, e.Message, e.LastTimestamp.Time); err != nil {
			c.logger.Error("failed to insert event", "error", err)
		}
	}

	if len(events) > 0 {
		c.logger.Info("events collected", "context", client.ContextName, "count", len(events))
	}
}

func parseLogLines(ctx, ns, pod, container, rawLogs string) []storage.LogEntry {
	lines := strings.Split(rawLogs, "\n")
	now := time.Now()

	var entries []storage.LogEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entries = append(entries, storage.LogEntry{
			Context:     ctx,
			Namespace:   ns,
			Pod:         pod,
			Container:   container,
			LogLine:     line,
			Severity:    classifySeverity(line),
			CollectedAt: now,
		})
	}
	return entries
}

func classifySeverity(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
		strings.Contains(lower, "panic") || strings.Contains(lower, "exception"):
		return "error"
	case strings.Contains(lower, "warn"):
		return "warning"
	case strings.Contains(lower, "debug") || strings.Contains(lower, "trace"):
		return "debug"
	default:
		return "info"
	}
}
