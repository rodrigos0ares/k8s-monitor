package insights

import (
	"testing"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

func TestUnderProvisionedPrioritized(t *testing.T) {
	cost := config.CostConfig{Currency: "USD", CPUCoreHour: 0.03, MemGBHour: 0.004}
	rows := []storage.RightsizingRow{
		{ // over-provisioned, big savings
			Context: "c", Namespace: "n", Workload: "over", Container: "x", Samples: 10,
			MaxCPUMilli: 5, MaxMemMB: 100, CPURequestMilli: 1000, MemRequestMB: 1024,
			HistMaxCPUMilli: 5, HistMaxMemMB: 100,
		},
		{ // under-provisioned on memory (peak 900 > req 512)
			Context: "c", Namespace: "n", Workload: "under", Container: "x", Samples: 10,
			MaxCPUMilli: 50, MaxMemMB: 900, CPURequestMilli: 500, MemRequestMB: 512,
			HistMaxCPUMilli: 50, HistMaxMemMB: 900,
		},
	}
	rep := BuildReport(rows, cost)
	if rep.ContainersUnderProvisioned != 1 {
		t.Fatalf("expected 1 under-provisioned, got %d", rep.ContainersUnderProvisioned)
	}
	if len(rep.Recommendations) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(rep.Recommendations))
	}
	first := rep.Recommendations[0]
	if first.Kind != KindUnderProvisioned {
		t.Fatalf("expected under-provisioned first, got %s (savings=%.2f)", first.Kind, first.MonthlySavings)
	}
	if first.AdditionalMemMB <= 0 {
		t.Fatalf("expected positive additional memory, got %d", first.AdditionalMemMB)
	}
	if first.MonthlySavings != 0 {
		t.Fatalf("under-provisioned should not advertise savings, got %.2f", first.MonthlySavings)
	}
	t.Logf("under rec: +%dMB needed, recommend %dMB", first.AdditionalMemMB, first.RecommendedMemMB)
}
