package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/rodrigos0ares/k8s-monitor/internal/config"
	"github.com/rodrigos0ares/k8s-monitor/internal/insights"
	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

//go:embed templates/*
var templateFS embed.FS

// Server provides the web dashboard and API.
type Server struct {
	db     *storage.DB
	cost   config.CostConfig
	logger *slog.Logger
	tmpl   *template.Template
	repos  *repoIndex
}

func NewServer(db *storage.DB, cost config.CostConfig, logger *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"divf": func(value int64, divisor float64) float64 {
			if divisor == 0 {
				return 0
			}
			return float64(value) / divisor
		},
	}
	tmpl, err := template.New("dashboard").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{db: db, cost: cost, logger: logger, tmpl: tmpl, repos: newRepoIndex(discoverProjectsRoot())}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Dashboard pages
	mux.HandleFunc("/", s.handleDashboard)

	// API endpoints
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/api/logs", s.handleAPILogs)
	mux.HandleFunc("/api/analyses", s.handleAPIAnalyses)
	mux.HandleFunc("/api/errors", s.handleAPIErrors)
	mux.HandleFunc("/api/rightsizing", s.handleAPIRightsizing)

	return mux
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	selectedCtx := r.URL.Query().Get("context")

	stats, _ := s.db.GetContextStats()
	analyses, _ := s.db.GetRecentAnalyses(10)
	errors, _ := s.db.GetErrorSummary(24, selectedCtx)
	dbStats, _ := s.db.GetDBStats()
	contexts, _ := s.db.GetDistinctContexts()
	issues := buildDeploymentIssues(errors, s.repos)

	rsRows, _ := s.db.GetRightsizingRows(24, 168, selectedCtx)
	rsReport := insights.BuildReport(rsRows, s.cost)

	// Split findings into two lists so the dashboard can show under-provisioned
	// (reliability red flags) separately from over-provisioned / other findings.
	var underRecs, overRecs []insights.Recommendation
	for _, rec := range rsReport.Recommendations {
		if rec.Kind == insights.KindUnderProvisioned {
			underRecs = append(underRecs, rec)
		} else {
			overRecs = append(overRecs, rec)
		}
	}
	if len(underRecs) > 15 {
		underRecs = underRecs[:15]
	}
	if len(overRecs) > 15 {
		overRecs = overRecs[:15]
	}

	data := map[string]interface{}{
		"Stats":            stats,
		"Analyses":         analyses,
		"Errors":           errors,
		"DeploymentIssues": issues,
		"DBStats":          dbStats,
		"Contexts":         contexts,
		"SelectedContext":  selectedCtx,
		"Rightsizing":      rsReport,
		"UnderRecommendations": underRecs,
		"OverRecommendations":  overRecs,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		s.logger.Error("template error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetContextStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours == 0 {
		hours = 1
	}
	ctx := r.URL.Query().Get("context")
	severity := r.URL.Query().Get("severity")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 100
	}

	logs, err := s.db.GetRecentLogs(hours, ctx, severity, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, logs)
}

func (s *Server) handleAPIAnalyses(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 20
	}

	analyses, err := s.db.GetRecentAnalyses(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, analyses)
}

func (s *Server) handleAPIErrors(w http.ResponseWriter, r *http.Request) {
	ctxFilter := r.URL.Query().Get("context")
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours == 0 {
		hours = 24
	}

	errors, err := s.db.GetErrorSummary(hours, ctxFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, errors)
}

func (s *Server) handleAPIRightsizing(w http.ResponseWriter, r *http.Request) {
	ctxFilter := r.URL.Query().Get("context")
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours == 0 {
		hours = 24
	}
	histHours, _ := strconv.Atoi(r.URL.Query().Get("history_hours"))
	if histHours == 0 {
		histHours = 168
	}

	rows, err := s.db.GetRightsizingRows(hours, histHours, ctxFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, insights.BuildReport(rows, s.cost))
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
