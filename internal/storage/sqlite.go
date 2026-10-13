package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	db *sql.DB
}

func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0750); err != nil {
		return nil, fmt.Errorf("creating db dir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			namespace TEXT NOT NULL,
			pod TEXT NOT NULL,
			container TEXT NOT NULL,
			log_line TEXT NOT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			collected_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_context ON logs(context)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_collected ON logs(collected_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_severity ON logs(severity)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_pod ON logs(namespace, pod)`,

		`CREATE TABLE IF NOT EXISTS metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			namespace TEXT NOT NULL,
			pod TEXT NOT NULL,
			container TEXT NOT NULL,
			cpu_milli INTEGER NOT NULL,
			memory_mb INTEGER NOT NULL,
			collected_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_collected ON metrics(collected_at)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_pod ON metrics(namespace, pod)`,

		`CREATE TABLE IF NOT EXISTS pod_resources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			namespace TEXT NOT NULL,
			pod TEXT NOT NULL,
			container TEXT NOT NULL,
			cpu_request_milli INTEGER NOT NULL DEFAULT 0,
			cpu_limit_milli INTEGER NOT NULL DEFAULT 0,
			mem_request_mb INTEGER NOT NULL DEFAULT 0,
			mem_limit_mb INTEGER NOT NULL DEFAULT 0,
			collected_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pod_resources_collected ON pod_resources(collected_at)`,
		`CREATE INDEX IF NOT EXISTS idx_pod_resources_key ON pod_resources(context, namespace, pod, container)`,

		`CREATE TABLE IF NOT EXISTS node_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			node TEXT NOT NULL,
			cpu_milli INTEGER NOT NULL,
			memory_mb INTEGER NOT NULL,
			collected_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS analyses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			analysis_type TEXT NOT NULL,
			summary TEXT NOT NULL,
			details TEXT NOT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_analyses_created ON analyses(created_at)`,

		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			context TEXT NOT NULL,
			namespace TEXT NOT NULL,
			involved_object TEXT NOT NULL,
			reason TEXT NOT NULL,
			message TEXT NOT NULL,
			event_time DATETIME NOT NULL,
			collected_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_time ON events(event_time)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, s)
		}
	}

	// Idempotently add the workload column to existing databases. The workload
	// is the owning controller (e.g. Deployment) name and is used as a stable
	// key for rightsizing so historical usage survives pod restarts/rollouts.
	for _, t := range []string{"metrics", "pod_resources"} {
		if err := addColumnIfMissing(db, t, "workload", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}

	workloadIdx := []string{
		`CREATE INDEX IF NOT EXISTS idx_metrics_workload ON metrics(context, namespace, workload, container)`,
		`CREATE INDEX IF NOT EXISTS idx_pod_resources_workload ON pod_resources(context, namespace, workload, container)`,
	}
	for _, s := range workloadIdx {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// addColumnIfMissing adds a column to a table only when it does not already
// exist, so migrations are safe to run repeatedly against existing databases.
func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return fmt.Errorf("scanning %s columns: %w", table, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("adding column %s.%s: %w", table, column, err)
	}
	return nil
}

// LogEntry represents a collected log line.
type LogEntry struct {
	ID          int64
	Context     string
	Namespace   string
	Pod         string
	Container   string
	LogLine     string
	Severity    string
	CollectedAt time.Time
}

// InsertLogs bulk-inserts log entries.
func (d *DB) InsertLogs(entries []LogEntry) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO logs (context, namespace, pod, container, log_line, severity, collected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		_, err := stmt.Exec(e.Context, e.Namespace, e.Pod, e.Container, e.LogLine, e.Severity, e.CollectedAt)
		if err != nil {
			return fmt.Errorf("inserting log: %w", err)
		}
	}
	return tx.Commit()
}

// MetricsEntry represents a collected metrics snapshot.
type MetricsEntry struct {
	Context   string
	Namespace string
	Pod       string
	Workload  string
	Container string
	CPUMilli  int64
	MemoryMB  int64
}

// InsertMetrics bulk-inserts metrics entries.
func (d *DB) InsertMetrics(entries []MetricsEntry) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO metrics (context, namespace, pod, workload, container, cpu_milli, memory_mb)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.Context, e.Namespace, e.Pod, e.Workload, e.Container, e.CPUMilli, e.MemoryMB); err != nil {
			return fmt.Errorf("inserting metric: %w", err)
		}
	}
	return tx.Commit()
}

// ResourceSpecEntry holds the configured requests/limits for a container.
type ResourceSpecEntry struct {
	Context         string
	Namespace       string
	Pod             string
	Workload        string
	Container       string
	CPURequestMilli int64
	CPULimitMilli   int64
	MemRequestMB    int64
	MemLimitMB      int64
}

// InsertPodResources bulk-inserts pod resource request/limit snapshots.
func (d *DB) InsertPodResources(entries []ResourceSpecEntry) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO pod_resources
		(context, namespace, pod, workload, container, cpu_request_milli, cpu_limit_milli, mem_request_mb, mem_limit_mb)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.Context, e.Namespace, e.Pod, e.Workload, e.Container,
			e.CPURequestMilli, e.CPULimitMilli, e.MemRequestMB, e.MemLimitMB); err != nil {
			return fmt.Errorf("inserting pod resource: %w", err)
		}
	}
	return tx.Commit()
}

// RightsizingRow joins observed usage against the latest configured
// requests/limits for a single container. It carries usage aggregated over two
// windows: a recent window (for current behaviour) and a longer historical
// window (so recommendations don't shrink resources based on a quiet period).
type RightsizingRow struct {
	Context   string
	Namespace string
	// Workload is the owning controller (e.g. Deployment) name that usage is
	// aggregated by, so historical data survives pod restarts. Falls back to the
	// pod name when the pod has no controller.
	Workload  string
	// Pod is a representative pod name for display only.
	Pod       string
	Container string

	// Recent window aggregates.
	Samples     int
	AvgCPUMilli int64
	MaxCPUMilli int64
	AvgMemMB    int64
	MaxMemMB    int64

	// Historical (longer) window aggregates. Zero if no historical data.
	HistSamples     int
	HistAvgCPUMilli int64
	HistMaxCPUMilli int64
	HistAvgMemMB    int64
	HistMaxMemMB    int64

	CPURequestMilli int64
	CPULimitMilli   int64
	MemRequestMB    int64
	MemLimitMB      int64
}

// GetRightsizingRows returns per-container usage aggregated over a recent
// window (hours) and a longer historical window (histHours), joined with the
// most recent configured requests/limits. Usage is grouped by the owning
// workload (e.g. Deployment) so historical data survives pod restarts and
// rollouts; pods with no controller fall back to grouping by pod name. If
// histHours <= hours it is treated as equal to hours.
func (d *DB) GetRightsizingRows(hours, histHours int, context string) ([]RightsizingRow, error) {
	if histHours < hours {
		histHours = hours
	}

	query := `SELECT
		rec.context, rec.namespace, rec.workload, rec.pod, rec.container,
		rec.samples, rec.avg_cpu, rec.max_cpu, rec.avg_mem, rec.max_mem,
		COALESCE(hist.samples, 0), COALESCE(hist.avg_cpu, 0), COALESCE(hist.max_cpu, 0),
		COALESCE(hist.avg_mem, 0), COALESCE(hist.max_mem, 0),
		r.cpu_request_milli, r.cpu_limit_milli, r.mem_request_mb, r.mem_limit_mb
	FROM (
		SELECT context, namespace,
			COALESCE(NULLIF(workload, ''), pod) AS workload,
			MAX(pod) AS pod,
			container,
			COUNT(*) AS samples,
			CAST(AVG(cpu_milli) AS INTEGER) AS avg_cpu,
			MAX(cpu_milli) AS max_cpu,
			CAST(AVG(memory_mb) AS INTEGER) AS avg_mem,
			MAX(memory_mb) AS max_mem
		FROM metrics
		WHERE collected_at > datetime('now', ?)
		GROUP BY context, namespace, COALESCE(NULLIF(workload, ''), pod), container
	) rec
	LEFT JOIN (
		SELECT context, namespace,
			COALESCE(NULLIF(workload, ''), pod) AS workload,
			container,
			COUNT(*) AS samples,
			CAST(AVG(cpu_milli) AS INTEGER) AS avg_cpu,
			MAX(cpu_milli) AS max_cpu,
			CAST(AVG(memory_mb) AS INTEGER) AS avg_mem,
			MAX(memory_mb) AS max_mem
		FROM metrics
		WHERE collected_at > datetime('now', ?)
		GROUP BY context, namespace, COALESCE(NULLIF(workload, ''), pod), container
	) hist
	ON hist.context = rec.context AND hist.namespace = rec.namespace
		AND hist.workload = rec.workload AND hist.container = rec.container
	JOIN (
		SELECT context, namespace,
			COALESCE(NULLIF(workload, ''), pod) AS workload,
			container,
			cpu_request_milli, cpu_limit_milli, mem_request_mb, mem_limit_mb,
			ROW_NUMBER() OVER (
				PARTITION BY context, namespace, COALESCE(NULLIF(workload, ''), pod), container
				ORDER BY collected_at DESC
			) AS rn
		FROM pod_resources
	) r
	ON r.context = rec.context AND r.namespace = rec.namespace
		AND r.workload = rec.workload AND r.container = rec.container AND r.rn = 1
	WHERE 1 = 1`
	args := []interface{}{
		fmt.Sprintf("-%d hours", hours),
		fmt.Sprintf("-%d hours", histHours),
	}

	if context != "" {
		query += " AND rec.context = ?"
		args = append(args, context)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RightsizingRow
	for rows.Next() {
		var r RightsizingRow
		if err := rows.Scan(&r.Context, &r.Namespace, &r.Workload, &r.Pod, &r.Container,
			&r.Samples, &r.AvgCPUMilli, &r.MaxCPUMilli, &r.AvgMemMB, &r.MaxMemMB,
			&r.HistSamples, &r.HistAvgCPUMilli, &r.HistMaxCPUMilli, &r.HistAvgMemMB, &r.HistMaxMemMB,
			&r.CPURequestMilli, &r.CPULimitMilli, &r.MemRequestMB, &r.MemLimitMB); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// InsertNodeMetrics inserts node-level metrics.
func (d *DB) InsertNodeMetrics(ctx, node string, cpuMilli, memoryMB int64) error {
	_, err := d.db.Exec(`INSERT INTO node_metrics (context, node, cpu_milli, memory_mb) VALUES (?, ?, ?, ?)`,
		ctx, node, cpuMilli, memoryMB)
	return err
}

// InsertEvent inserts a K8s event.
func (d *DB) InsertEvent(ctx, namespace, involvedObject, reason, message string, eventTime time.Time) error {
	_, err := d.db.Exec(`INSERT INTO events (context, namespace, involved_object, reason, message, event_time)
		VALUES (?, ?, ?, ?, ?, ?)`, ctx, namespace, involvedObject, reason, message, eventTime)
	return err
}

// Analysis represents an LLM analysis result.
type Analysis struct {
	ID           int64
	Context      string
	AnalysisType string
	Summary      string
	Details      string
	Severity     string
	CreatedAt    time.Time
}

// InsertAnalysis stores an LLM analysis result.
func (d *DB) InsertAnalysis(a Analysis) error {
	_, err := d.db.Exec(`INSERT INTO analyses (context, analysis_type, summary, details, severity)
		VALUES (?, ?, ?, ?, ?)`, a.Context, a.AnalysisType, a.Summary, a.Details, a.Severity)
	return err
}

// GetRecentLogs returns logs from the last N hours, optionally filtered.
func (d *DB) GetRecentLogs(hours int, context, severity string, limit int) ([]LogEntry, error) {
	query := `SELECT id, context, namespace, pod, container, log_line, severity, collected_at
		FROM logs WHERE collected_at > datetime('now', ?)`
	args := []interface{}{fmt.Sprintf("-%d hours", hours)}

	if context != "" {
		query += " AND context = ?"
		args = append(args, context)
	}
	if severity != "" {
		query += " AND severity = ?"
		args = append(args, severity)
	}
	query += " ORDER BY collected_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.Context, &e.Namespace, &e.Pod, &e.Container, &e.LogLine, &e.Severity, &e.CollectedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// GetRecentAnalyses returns the latest analyses.
func (d *DB) GetRecentAnalyses(limit int) ([]Analysis, error) {
	rows, err := d.db.Query(`SELECT id, context, analysis_type, summary, details, severity, created_at
		FROM analyses ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var analyses []Analysis
	for rows.Next() {
		var a Analysis
		if err := rows.Scan(&a.ID, &a.Context, &a.AnalysisType, &a.Summary, &a.Details, &a.Severity, &a.CreatedAt); err != nil {
			return nil, err
		}
		analyses = append(analyses, a)
	}
	return analyses, rows.Err()
}

// ErrorSummary holds aggregated error counts with a sample log line.
type ErrorSummary struct {
	Context   string
	Namespace string
	Pod       string
	Severity  string
	Count     int
	SampleLog string
}

// GetErrorSummary returns aggregated error/warning counts per pod with a sample log.
func (d *DB) GetErrorSummary(hours int, context string) ([]ErrorSummary, error) {
	query := `SELECT context, namespace, pod, severity, COUNT(*) as cnt,
		(SELECT log_line FROM logs l2
		 WHERE l2.context = logs.context AND l2.namespace = logs.namespace
		   AND l2.pod = logs.pod AND l2.severity = logs.severity
		 ORDER BY l2.collected_at DESC LIMIT 1) as sample
		FROM logs
		WHERE severity IN ('error', 'warning') AND collected_at > datetime('now', ?)`
	args := []interface{}{fmt.Sprintf("-%d hours", hours)}

	if context != "" {
		query += " AND context = ?"
		args = append(args, context)
	}
	query += ` GROUP BY context, namespace, pod, severity ORDER BY cnt DESC LIMIT 100`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []ErrorSummary
	for rows.Next() {
		var s ErrorSummary
		if err := rows.Scan(&s.Context, &s.Namespace, &s.Pod, &s.Severity, &s.Count, &s.SampleLog); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// GetDistinctContexts returns all unique context names from stored logs.
func (d *DB) GetDistinctContexts() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT context FROM logs ORDER BY context`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contexts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		contexts = append(contexts, c)
	}
	return contexts, rows.Err()
}

// GetAllStoredContexts returns every context that has data in any table, with
// the number of records stored for it. This reflects what the dashboard's
// context selector shows, including stale/renamed contexts.
func (d *DB) GetAllStoredContexts() (map[string]int64, error) {
	counts := make(map[string]int64)
	tables := []string{"logs", "metrics", "node_metrics", "pod_resources", "events", "analyses"}
	for _, t := range tables {
		rows, err := d.db.Query(fmt.Sprintf("SELECT context, COUNT(*) FROM %s GROUP BY context", t))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ctx string
			var n int64
			if err := rows.Scan(&ctx, &n); err != nil {
				rows.Close()
				return nil, err
			}
			counts[ctx] += n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return counts, nil
}

// DeleteContextData removes all stored data for the given context across every
// table. It returns the number of rows deleted per table.
func (d *DB) DeleteContextData(context string) (map[string]int64, error) {
	deleted := make(map[string]int64)
	tables := []string{"logs", "metrics", "node_metrics", "pod_resources", "events", "analyses"}

	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, t := range tables {
		res, err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE context = ?", t), context)
		if err != nil {
			return nil, fmt.Errorf("deleting from %s: %w", t, err)
		}
		n, _ := res.RowsAffected()
		deleted[t] = n
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

// PodRef identifies a single pod within a context.
type PodRef struct {
	Context   string
	Namespace string
	Pod       string
}

// GetDistinctLogPods returns the distinct (context, namespace, pod) tuples that
// have logs stored. If context is non-empty it is limited to that context.
func (d *DB) GetDistinctLogPods(context string) ([]PodRef, error) {
	query := `SELECT DISTINCT context, namespace, pod FROM logs`
	var args []interface{}
	if context != "" {
		query += ` WHERE context = ?`
		args = append(args, context)
	}
	query += ` ORDER BY context, namespace, pod`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pods []PodRef
	for rows.Next() {
		var p PodRef
		if err := rows.Scan(&p.Context, &p.Namespace, &p.Pod); err != nil {
			return nil, err
		}
		pods = append(pods, p)
	}
	return pods, rows.Err()
}

// DeleteLogsForPod removes all logs for a single pod and returns the number of
// rows deleted.
func (d *DB) DeleteLogsForPod(context, namespace, pod string) (int64, error) {
	res, err := d.db.Exec(
		`DELETE FROM logs WHERE context = ? AND namespace = ? AND pod = ?`,
		context, namespace, pod)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// WorkloadRef identifies a single workload within a context. Workload is the
// owning-controller key used by the rightsizing report
// (COALESCE(NULLIF(workload,''), pod)).
type WorkloadRef struct {
	Context   string
	Namespace string
	Workload  string
}

// GetDistinctResourceWorkloads returns the distinct (context, namespace, workload)
// tuples that have metrics or resource-spec data stored, using the same workload
// key as the rightsizing report. If context is non-empty it is limited to that
// context.
func (d *DB) GetDistinctResourceWorkloads(context string) ([]WorkloadRef, error) {
	query := `SELECT DISTINCT context, namespace, COALESCE(NULLIF(workload,''), pod) AS wl FROM (
		SELECT context, namespace, pod, workload FROM metrics
		UNION ALL
		SELECT context, namespace, pod, workload FROM pod_resources
	)`
	var args []interface{}
	if context != "" {
		query += ` WHERE context = ?`
		args = append(args, context)
	}
	query += ` ORDER BY context, namespace, wl`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workloads []WorkloadRef
	for rows.Next() {
		var w WorkloadRef
		if err := rows.Scan(&w.Context, &w.Namespace, &w.Workload); err != nil {
			return nil, err
		}
		workloads = append(workloads, w)
	}
	return workloads, rows.Err()
}

// DeleteResourceDataForWorkload removes all metrics and resource specs for a
// single workload (matched by the rightsizing workload key) and returns the
// total number of rows deleted across those tables.
func (d *DB) DeleteResourceDataForWorkload(context, namespace, workload string) (int64, error) {
	var total int64
	for _, table := range []string{"metrics", "pod_resources"} {
		res, err := d.db.Exec(
			`DELETE FROM `+table+` WHERE context = ? AND namespace = ? AND COALESCE(NULLIF(workload,''), pod) = ?`,
			context, namespace, workload)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
type ContextStats struct {
	Context       string
	TotalLogs     int
	ErrorCount    int
	WarningCount  int
	PodCount      int
	AvgCPUMilli   int64
	AvgMemoryMB   int64
	LatestCollect time.Time
}

// GetContextStats returns aggregated stats per context.
func (d *DB) GetContextStats() ([]ContextStats, error) {
	rows, err := d.db.Query(`SELECT
		context,
		COUNT(*) as total,
		SUM(CASE WHEN severity = 'error' THEN 1 ELSE 0 END) as errors,
		SUM(CASE WHEN severity = 'warning' THEN 1 ELSE 0 END) as warnings,
		COUNT(DISTINCT pod) as pods,
		CAST(strftime('%s', MAX(collected_at)) AS INTEGER) as latest_unix
		FROM logs
		WHERE collected_at > datetime('now', '-24 hours')
		GROUP BY context`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []ContextStats
	for rows.Next() {
		var s ContextStats
		var latestUnix sql.NullInt64
		if err := rows.Scan(&s.Context, &s.TotalLogs, &s.ErrorCount, &s.WarningCount, &s.PodCount, &latestUnix); err != nil {
			return nil, err
		}
		if latestUnix.Valid {
			s.LatestCollect = time.Unix(latestUnix.Int64, 0).UTC()
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// PurgeOldData deletes data older than the given number of days.
func (d *DB) PurgeOldData(retentionDays int) error {
	cutoff := fmt.Sprintf("-%d days", retentionDays)
	tables := []string{"logs", "metrics", "node_metrics", "pod_resources", "events", "analyses"}
	for _, t := range tables {
		col := "collected_at"
		if t == "analyses" {
			col = "created_at"
		}
		if t == "events" {
			col = "event_time"
		}
		query := fmt.Sprintf("DELETE FROM %s WHERE %s < datetime('now', ?)", t, col)
		if _, err := d.db.Exec(query, cutoff); err != nil {
			return fmt.Errorf("purging %s: %w", t, err)
		}
	}
	return nil
}

// GetDBStats returns database size info.
func (d *DB) GetDBStats() (map[string]int64, error) {
	stats := make(map[string]int64)
	tables := []string{"logs", "metrics", "node_metrics", "pod_resources", "events", "analyses"}
	for _, t := range tables {
		var count int64
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", t)
		if err := d.db.QueryRow(query).Scan(&count); err != nil {
			return nil, err
		}
		stats[t] = count
	}
	return stats, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}
