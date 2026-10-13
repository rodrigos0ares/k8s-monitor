package alerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

// Notifier sends alerts to configured channels.
type Notifier struct {
	cfg    config.AlertConfig
	db     *storage.DB
	logger *slog.Logger
	client *http.Client
}

func NewNotifier(cfg config.AlertConfig, db *storage.DB, logger *slog.Logger) *Notifier {
	return &Notifier{
		cfg:    cfg,
		db:     db,
		logger: logger,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// NotifyAnalysis sends an alert for a new analysis result.
func (n *Notifier) NotifyAnalysis(a storage.Analysis) {
	if a.Severity == "low" || a.Severity == "info" {
		return // Only alert on medium+ severity
	}

	message := fmt.Sprintf("🔔 **K8s Monitor Alert** [%s]\n**Context:** %s\n**Type:** %s\n**Severity:** %s\n\n%s",
		time.Now().Format("2006-01-02 15:04"),
		a.Context, a.AnalysisType, a.Severity,
		truncate(a.Details, 1500))

	if n.cfg.Slack.Enabled {
		n.sendSlack(message)
	}
	if n.cfg.Discord.Enabled {
		n.sendDiscord(message)
	}
}

func (n *Notifier) sendSlack(message string) {
	payload := map[string]string{"text": message}
	data, _ := json.Marshal(payload)

	resp, err := n.client.Post(n.cfg.Slack.WebhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		n.logger.Error("failed to send Slack alert", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		n.logger.Error("Slack webhook returned non-200", "status", resp.StatusCode)
	}
}

func (n *Notifier) sendDiscord(message string) {
	payload := map[string]string{"content": message}
	data, _ := json.Marshal(payload)

	resp, err := n.client.Post(n.cfg.Discord.WebhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		n.logger.Error("failed to send Discord alert", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		n.logger.Error("Discord webhook returned error", "status", resp.StatusCode)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
