// Package insights derives cost-saving / rightsizing recommendations by
// comparing observed pod resource usage against configured requests and limits.
package insights

import (
	"math"
	"sort"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

// hoursPerMonth is the average number of hours in a month, used to project an
// hourly price into a monthly estimate.
const hoursPerMonth = 730.0

// headroomFactor is the safety margin applied on top of observed peak usage
// when recommending a new request value (25% headroom).
const headroomFactor = 1.25

// Minimum recommended floors so we never suggest unschedulably tiny requests.
const (
	minCPUMilli int64 = 10
	minMemMB    int64 = 16
)

// Kind classifies the dominant finding for a container.
type Kind string

const (
	KindUnderProvisioned Kind = "under-provisioned"
	KindOverProvisioned  Kind = "over-provisioned"
	KindNoRequests       Kind = "no-requests"
	KindMemoryRisk       Kind = "memory-limit-risk"
	KindCPUThrottleRisk  Kind = "cpu-limit-risk"
	KindHealthy          Kind = "healthy"
)

// Recommendation is a per-container rightsizing finding with an estimated
// monthly saving (or risk note).
type Recommendation struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	// Workload is the owning controller (e.g. Deployment) the recommendation is
	// scoped to. Falls back to the pod name when there is no controller.
	Workload  string `json:"workload"`
	// Pod is a representative pod name for display only.
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Samples   int    `json:"samples"`

	AvgCPUMilli     int64 `json:"avg_cpu_milli"`
	MaxCPUMilli     int64 `json:"max_cpu_milli"`
	CPURequestMilli int64 `json:"cpu_request_milli"`
	CPULimitMilli   int64 `json:"cpu_limit_milli"`

	AvgMemMB     int64 `json:"avg_mem_mb"`
	MaxMemMB     int64 `json:"max_mem_mb"`
	MemRequestMB int64 `json:"mem_request_mb"`
	MemLimitMB   int64 `json:"mem_limit_mb"`

	// Historical (longer-window) usage. Recommendations are based on the larger
	// of recent vs. historical peaks so a quiet recent window does not cause
	// over-aggressive downsizing.
	HistSamples     int   `json:"hist_samples"`
	HistAvgCPUMilli int64 `json:"hist_avg_cpu_milli"`
	HistMaxCPUMilli int64 `json:"hist_max_cpu_milli"`
	HistAvgMemMB    int64 `json:"hist_avg_mem_mb"`
	HistMaxMemMB    int64 `json:"hist_max_mem_mb"`

	// PeakCPUMilli/PeakMemMB are the effective peaks used to size the
	// recommendation (max of recent and historical).
	PeakCPUMilli int64 `json:"peak_cpu_milli"`
	PeakMemMB    int64 `json:"peak_mem_mb"`

	// Utilization is peak usage as a percentage of the request (0 if no request).
	CPUUtilPct float64 `json:"cpu_util_pct"`
	MemUtilPct float64 `json:"mem_util_pct"`

	// Recommended new request values based on peak usage + headroom.
	RecommendedCPUMilli int64 `json:"recommended_cpu_milli"`
	RecommendedMemMB    int64 `json:"recommended_mem_mb"`

	// Reclaimable resources (request - recommended), never negative.
	ReclaimableCPUMilli int64 `json:"reclaimable_cpu_milli"`
	ReclaimableMemMB    int64 `json:"reclaimable_mem_mb"`

	// Additional resources needed (recommended - request) when the observed
	// peak exceeds the configured request. Never negative. Non-zero only for
	// under-provisioned workloads.
	AdditionalCPUMilli int64 `json:"additional_cpu_milli"`
	AdditionalMemMB    int64 `json:"additional_mem_mb"`

	MonthlySavings float64 `json:"monthly_savings"`
	Currency       string  `json:"currency"`

	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// Report is the aggregated rightsizing output.
type Report struct {
	Currency             string           `json:"currency"`
	TotalMonthlySavings  float64          `json:"total_monthly_savings"`
	ReclaimableCPUMilli  int64            `json:"reclaimable_cpu_milli"`
	ReclaimableMemMB     int64            `json:"reclaimable_mem_mb"`
	ContainersAnalyzed   int              `json:"containers_analyzed"`
	ContainersFlagged    int              `json:"containers_flagged"`
	// ContainersUnderProvisioned counts workloads whose observed peak usage
	// exceeds their configured request (a reliability red flag).
	ContainersUnderProvisioned int        `json:"containers_under_provisioned"`
	Recommendations      []Recommendation `json:"recommendations"`
}

// BuildReport converts raw rightsizing rows into ranked recommendations and an
// aggregate cost summary.
func BuildReport(rows []storage.RightsizingRow, cost config.CostConfig) Report {
	report := Report{Currency: cost.Currency}

	for _, row := range rows {
		report.ContainersAnalyzed++
		rec := evaluate(row, cost)
		if rec.Kind == KindHealthy {
			continue
		}
		report.ContainersFlagged++
		if rec.Kind == KindUnderProvisioned {
			report.ContainersUnderProvisioned++
		}
		report.TotalMonthlySavings += rec.MonthlySavings
		report.ReclaimableCPUMilli += rec.ReclaimableCPUMilli
		report.ReclaimableMemMB += rec.ReclaimableMemMB
		report.Recommendations = append(report.Recommendations, rec)
	}

	sort.SliceStable(report.Recommendations, func(i, j int) bool {
		a, b := report.Recommendations[i], report.Recommendations[j]
		// Under-provisioned workloads are a reliability red flag and are always
		// surfaced first, regardless of estimated savings.
		if au, bu := a.Kind == KindUnderProvisioned, b.Kind == KindUnderProvisioned; au != bu {
			return au
		}
		if a.MonthlySavings != b.MonthlySavings {
			return a.MonthlySavings > b.MonthlySavings
		}
		// Surface risk findings (no savings) ahead of healthy-ish ones.
		return kindRank(a.Kind) < kindRank(b.Kind)
	})

	return report
}

func kindRank(k Kind) int {
	switch k {
	case KindUnderProvisioned:
		return -1
	case KindOverProvisioned:
		return 0
	case KindMemoryRisk:
		return 1
	case KindCPUThrottleRisk:
		return 2
	case KindNoRequests:
		return 3
	default:
		return 4
	}
}

func evaluate(row storage.RightsizingRow, cost config.CostConfig) Recommendation {
	rec := Recommendation{
		Context:         row.Context,
		Namespace:       row.Namespace,
		Workload:        row.Workload,
		Pod:             row.Pod,
		Container:       row.Container,
		Samples:         row.Samples,
		AvgCPUMilli:     row.AvgCPUMilli,
		MaxCPUMilli:     row.MaxCPUMilli,
		CPURequestMilli: row.CPURequestMilli,
		CPULimitMilli:   row.CPULimitMilli,
		AvgMemMB:        row.AvgMemMB,
		MaxMemMB:        row.MaxMemMB,
		MemRequestMB:    row.MemRequestMB,
		MemLimitMB:      row.MemLimitMB,
		HistSamples:     row.HistSamples,
		HistAvgCPUMilli: row.HistAvgCPUMilli,
		HistMaxCPUMilli: row.HistMaxCPUMilli,
		HistAvgMemMB:    row.HistAvgMemMB,
		HistMaxMemMB:    row.HistMaxMemMB,
		Currency:        cost.Currency,
		Kind:            KindHealthy,
	}

	// Effective peaks: never size below what we have ever seen in the longer
	// historical window. This guards against trimming resources too far based
	// on a temporarily quiet recent window.
	rec.PeakCPUMilli = maxInt64(row.MaxCPUMilli, row.HistMaxCPUMilli)
	rec.PeakMemMB = maxInt64(row.MaxMemMB, row.HistMaxMemMB)

	// No requests configured: cannot be scheduled predictably or cost-controlled.
	if row.CPURequestMilli == 0 && row.MemRequestMB == 0 {
		rec.Kind = KindNoRequests
		rec.Message = "No CPU/memory requests set — set requests so the scheduler can bin-pack and right-size nodes."
		return rec
	}

	// Recommended requests = effective peak * headroom, with sane floors.
	if row.CPURequestMilli > 0 {
		rec.RecommendedCPUMilli = maxInt64(int64(math.Ceil(float64(rec.PeakCPUMilli)*headroomFactor)), minCPUMilli)
		rec.CPUUtilPct = pct(rec.PeakCPUMilli, row.CPURequestMilli)
		if reclaim := row.CPURequestMilli - rec.RecommendedCPUMilli; reclaim > 0 {
			rec.ReclaimableCPUMilli = reclaim
		}
	}
	if row.MemRequestMB > 0 {
		rec.RecommendedMemMB = maxInt64(int64(math.Ceil(float64(rec.PeakMemMB)*headroomFactor)), minMemMB)
		rec.MemUtilPct = pct(rec.PeakMemMB, row.MemRequestMB)
		if reclaim := row.MemRequestMB - rec.RecommendedMemMB; reclaim > 0 {
			rec.ReclaimableMemMB = reclaim
		}
	}

	rec.MonthlySavings = monthlyCost(rec.ReclaimableCPUMilli, rec.ReclaimableMemMB, cost)

	// Under-provisioned: observed peak usage exceeds the configured request, so
	// the request is set too low. This risks eviction/OOMKills (memory) or CPU
	// starvation/poor scheduling, and is treated as a reliability red flag that
	// outranks any cost-saving finding.
	cpuUnder := row.CPURequestMilli > 0 && rec.PeakCPUMilli > row.CPURequestMilli
	memUnder := row.MemRequestMB > 0 && rec.PeakMemMB > row.MemRequestMB
	if cpuUnder {
		rec.AdditionalCPUMilli = maxInt64(rec.RecommendedCPUMilli-row.CPURequestMilli, 0)
	}
	if memUnder {
		rec.AdditionalMemMB = maxInt64(rec.RecommendedMemMB-row.MemRequestMB, 0)
	}

	switch {
	case memUnder || cpuUnder:
		rec.Kind = KindUnderProvisioned
		rec.Message = "Observed peak usage exceeds the configured request — requests are too low, risking eviction/OOMKills (memory) or CPU starvation. Raise requests (and limits) to at least the recommended values."
		// Do not advertise cost savings on a reliability red flag.
		rec.MonthlySavings = 0
		rec.ReclaimableCPUMilli = 0
		rec.ReclaimableMemMB = 0
	case rec.ReclaimableCPUMilli > 0 || rec.ReclaimableMemMB > 0:
		rec.Kind = KindOverProvisioned
		rec.Message = "Requests far exceed observed peak usage (incl. historical) — lower requests to reclaim capacity and cut cost."
	case row.MemLimitMB > 0 && rec.PeakMemMB > 0 && float64(rec.PeakMemMB) >= 0.9*float64(row.MemLimitMB):
		rec.Kind = KindMemoryRisk
		rec.Message = "Peak memory usage is within 10% of the limit — raise the limit to avoid OOMKills."
	case row.CPULimitMilli > 0 && rec.PeakCPUMilli > 0 && float64(rec.PeakCPUMilli) >= 0.95*float64(row.CPULimitMilli):
		rec.Kind = KindCPUThrottleRisk
		rec.Message = "Peak CPU usage is near the limit — workloads may be throttled under load."
	}

	return rec
}

// monthlyCost translates reclaimable CPU (millicores) and memory (MB) into a
// projected monthly saving using the configured pricing.
func monthlyCost(cpuMilli, memMB int64, cost config.CostConfig) float64 {
	cpuCores := float64(cpuMilli) / 1000.0
	memGB := float64(memMB) / 1024.0
	return (cpuCores*cost.CPUCoreHour + memGB*cost.MemGBHour) * hoursPerMonth
}

func pct(usage, request int64) float64 {
	if request <= 0 {
		return 0
	}
	return float64(usage) / float64(request) * 100.0
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
