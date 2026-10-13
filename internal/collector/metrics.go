package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/k8s"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

// MetricsCollector periodically collects CPU/memory metrics from all contexts.
type MetricsCollector struct {
	clients  []*k8s.Client
	db       *storage.DB
	interval time.Duration
	logger   *slog.Logger
}

func NewMetricsCollector(clients []*k8s.Client, db *storage.DB, intervalSecs int, logger *slog.Logger) *MetricsCollector {
	return &MetricsCollector{
		clients:  clients,
		db:       db,
		interval: time.Duration(intervalSecs) * time.Second,
		logger:   logger,
	}
}

// Run starts the metrics collection loop. Blocks until ctx is cancelled.
func (m *MetricsCollector) Run(ctx context.Context) {
	m.logger.Info("starting metrics collector", "interval", m.interval)
	m.collectAll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("metrics collector stopped")
			return
		case <-ticker.C:
			m.collectAll(ctx)
		}
	}
}

func (m *MetricsCollector) collectAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, client := range m.clients {
		wg.Add(1)
		go func(cl *k8s.Client) {
			defer wg.Done()
			m.collectFromContext(ctx, cl)
		}(client)
	}
	wg.Wait()
}

func (m *MetricsCollector) collectFromContext(ctx context.Context, client *k8s.Client) {
	// Pod metrics
	podMetrics, err := client.GetPodMetrics(ctx)
	if err != nil {
		m.logger.Warn("failed to get pod metrics (metrics-server may not be installed)",
			"context", client.ContextName, "error", err)
	} else {
		var entries []storage.MetricsEntry
		for _, pm := range podMetrics {
			entries = append(entries, storage.MetricsEntry{
				Context:   pm.Context,
				Namespace: pm.Namespace,
				Pod:       pm.Pod,
				Workload:  pm.Workload,
				Container: pm.Container,
				CPUMilli:  pm.CPUMilli,
				MemoryMB:  pm.MemoryMB,
			})
		}
		if len(entries) > 0 {
			if err := m.db.InsertMetrics(entries); err != nil {
				m.logger.Error("failed to insert pod metrics", "error", err)
			} else {
				m.logger.Info("pod metrics collected", "context", client.ContextName, "entries", len(entries))
			}
		}
	}

	// Pod resource requests/limits (used for rightsizing / cost insights)
	resourceSpecs, err := client.GetPodResourceSpecs(ctx)
	if err != nil {
		m.logger.Warn("failed to get pod resource specs", "context", client.ContextName, "error", err)
	} else {
		var entries []storage.ResourceSpecEntry
		for _, rs := range resourceSpecs {
			entries = append(entries, storage.ResourceSpecEntry{
				Context:         rs.Context,
				Namespace:       rs.Namespace,
				Pod:             rs.Pod,
				Workload:        rs.Workload,
				Container:       rs.Container,
				CPURequestMilli: rs.CPURequestMilli,
				CPULimitMilli:   rs.CPULimitMilli,
				MemRequestMB:    rs.MemRequestMB,
				MemLimitMB:      rs.MemLimitMB,
			})
		}
		if len(entries) > 0 {
			if err := m.db.InsertPodResources(entries); err != nil {
				m.logger.Error("failed to insert pod resource specs", "error", err)
			} else {
				m.logger.Info("pod resource specs collected", "context", client.ContextName, "entries", len(entries))
			}
		}
	}

	// Node metrics
	nodeMetrics, err := client.GetNodeMetrics(ctx)
	if err != nil {
		m.logger.Debug("failed to get node metrics", "context", client.ContextName, "error", err)
		return
	}
	for _, nm := range nodeMetrics {
		if err := m.db.InsertNodeMetrics(nm.Context, nm.Node, nm.CPUMilli, nm.MemoryMB); err != nil {
			m.logger.Error("failed to insert node metrics", "error", err)
		}
	}
	if len(nodeMetrics) > 0 {
		m.logger.Info("node metrics collected", "context", client.ContextName, "nodes", len(nodeMetrics))
	}
}
