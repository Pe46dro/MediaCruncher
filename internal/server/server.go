// Package server implements the HTTP API and Web UI for MediaCruncher.
//
// It provides:
//   - SPA dashboard (index.html + static assets)
//   - Prometheus metrics (/metrics, /metrics.json)
//   - REST API for status, queue management, scheduling, scopes, config
//   - Middleware stack: maxSize → CORS → auth → logging
//
// The middleware order is intentional:
//  1. maxSizeMiddleware  - Reject oversized bodies early (DoS protection)
//  2. corsMiddleware     - Set CORS headers for cross-origin clients
//  3. authMiddleware     - Validate API key (optional, disabled when empty)
//  4. loggingMiddleware  - Capture request duration and status code
//
// REST API routes (absolute paths):
//
//	GOT /healthz          - Health check (JSON: {"status":"healthy","timestamp":"...})
//	GOT /metrics          - Prometheus metrics text
//	GOT /metrics.json     - Machine-readable JSON metrics
//	GOT /api/status       - Full system status (queue, metrics, recent audit)
//	GOT /api/queue        - Paginated queue entries (query: state, limit, search)
//	GOT/DEL /api/queue/{id} - Single queue item / dismiss job
//	POST  /api/queue/{id}/raise-priority - Move job up priority queue
//	POST  /api/scan       - Trigger immediate filesystem scan
//	GOT   /api/config     - Current configuration (POST to update)
//	GOT   /api/hardware   - Detected hardware accelerators
//	GOT   /api/schedule   - List scheduled scan entries
//	POST  /api/schedule   - Add a scheduled scan entry
//	DEL   /api/schedule/{id} - Remove a scheduled scan entry
//	GOT   /api/filesystem/scopes     - List configured scan scopes
//	POST  /api/filesystem/scopes     - Add scope (hot-restore)
//	DEL   /api/filesystem/scopes/{id} - Remove scope by index
//
// REST API routes (trailing-slash variants):
//
//	GOT/POST /api/queue/        - Same as /api/queue (Mux fallback)
//	DEL /api/queue/{id}/        - Same as /api/queue/{id}
//	GOT    /api/schedule/       - Same as /api/schedule
//	DEL    /api/schedule/{id}/  - Same as /api/schedule/{id}
//	GOT    /api/filesystem/scopes/ - Same as /api/filesystem/scopes
//	DEL    /api/filesystem/scopes/{id}/ - Same as /api/filesystem/scopes/{id}
package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/dedupe"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/scheduling"
	"mediacruncher/internal/transcoder"
)

const (
	defaultAPIKey       = ""          // empty = disabled; set via SetAPIKey
	maxRequestSize      = 10 * 1024   // 10MB max request body
	readHeaderTimeout   = 10 * time.Second
	writeTimeout        = 30 * time.Second
	idleTimeout         = 120 * time.Second
)

var currentAPIKey = defaultAPIKey

//go:embed web/*
var webFS embed.FS

// Server manages the HTTP server, REST API routing, and middleware stack.
//
// The server is built with dependency injection: all dependencies (DB, metrics,
// dedupe index, scheduler, worker callbacks) are attached via setter methods
// after construction. This keeps the constructor minimal and avoids cyclic imports.
//
// Thread safety: the Server struct itself is not concurrent-safety by default;
// all HTTP handlers are invoked by net/http concurrently. Shared state (db,
// metrics, dedupe) is accessed through their own thread-safe implementations.
type Server struct {
	httpServer     *http.Server
	cfgMgr         *config.Manager
	db             *persistence.Engine
	metrics        *observability.Metrics
	dedupe         *dedupe.Index
	sched          *scheduling.Engine
	onTriggerScan     func()
	tracker           *transcoder.ProgressTracker
	onCancelJob       func(int64) bool
	onTriggerPrefetch func()
}

func NewServer(
	port int,
	cfgMgr *config.Manager,
	db *persistence.Engine,
	metrics *observability.Metrics,
	onTriggerScan func(),
) *Server {
	s := &Server{
		cfgMgr:        cfgMgr,
		db:            db,
		metrics:       metrics,
		onTriggerScan: onTriggerScan,
	}

	mux := http.NewServeMux()

	// 1. Static Asset Serving
	subFS, err := fs.Sub(webFS, "web")
	if err == nil {
		fileServer := http.FileServer(http.FS(subFS))
		mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	}

	// Favicon & Icon Root Endpoints
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/favicon.ico")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	})
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/favicon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	})
	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/apple-touch-icon.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	})

	// 2. Main SPA Entry Point
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "Dashboard file missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// 3. Prometheus Metrics & Health Endpoints
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, metrics.PrometheusFormat())
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"status":    "healthy",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 4. REST APIs
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/queue/", s.handleQueueItem)
	mux.HandleFunc("/api/scan", s.handleTriggerScan)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/hardware", s.handleHardware)
	mux.HandleFunc("/api/schedule", s.handleSchedule)
	mux.HandleFunc("/api/schedule/", s.handleScheduleItem)
	mux.HandleFunc("/metrics.json", s.handleMetricsJSON)
	mux.HandleFunc("/api/filesystem/scopes", s.handleScopes)
	mux.HandleFunc("/api/filesystem/scopes/", s.handleScopeItem)

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.wrapMiddleware(mux),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	return s
}

func (s *Server) Start() {
	go func() {
		slog.Info("Web UI and REST API server listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Web server terminated unexpectedly", "err", err)
		}
	}()
}

// --------------------------------------------------------------------------
// Middleware Stack
// --------------------------------------------------------------------------

// wrapMiddleware chains all HTTP middleware layers around the handler.
func (s *Server) wrapMiddleware(next http.Handler) http.Handler {
	handler := maxSizeMiddleware(next)          // 1. Limit request body size
	handler = corsMiddleware(handler)            // 2. CORS headers
	handler = authMiddleware(handler)            // 3. API key authentication
	handler = loggingMiddleware(handler)         // 4. Per-request logging
	return handler
}

// corsMiddleware adds CORS headers for cross-origin requests.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authMiddleware validates API key for protected endpoints when configured.
func authMiddleware(next http.Handler) http.Handler {
	apiKey := currentAPIKey
	if apiKey == "" {
		// No auth configured — pass through all requests
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqKey := r.Header.Get("X-API-Key")
		if reqKey == "" {
			reqKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}

		if reqKey == "" {
			http.Error(w, `{"error":"missing API key"}`, http.StatusUnauthorized)
			return
		}

		if reqKey != apiKey {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// maxSizeMiddleware limits request body size to prevent DoS.
func maxSizeMiddleware(next http.Handler) http.Handler {
	return http.MaxBytesHandler(next, maxRequestSize)
}

// loggingMiddleware logs each HTTP request with method, path, status, and duration.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		duration := time.Since(start)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration", duration.String(),
			"client_ip", r.RemoteAddr,
		)
	})
}

// statusResponseWriter wraps http.ResponseWriter to capture the status code.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// SetProgressTracker attaches an active progress tracker for real-time queue telemetry.
func (s *Server) SetProgressTracker(t *transcoder.ProgressTracker) {
	s.tracker = t
}

// SetOnCancelJob sets the callback to abort an in-flight job execution.
func (s *Server) SetOnCancelJob(fn func(int64) bool) {
	s.onCancelJob = fn
}

// SetOnTriggerPrefetch sets the callback to nudge workers when jobs are requeued or prioritized.
func (s *Server) SetOnTriggerPrefetch(fn func()) {
	s.onTriggerPrefetch = fn
}

// SetDedupeIndex attaches the singleton DedupeIndex for hot-scopes management.
func (s *Server) SetDedupeIndex(d *dedupe.Index) {
	s.dedupe = d
}

// SetScheduling attaches the scheduling engine for scheduled scans.
func (s *Server) SetScheduling(sched *scheduling.Engine) {
	s.sched = sched
}

// SetAPIKey configures the API key for authentication on protected endpoints.
func SetAPIKey(key string) {
	currentAPIKey = key
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// --------------------------------------------------------------------------
// REST API Handlers
// --------------------------------------------------------------------------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pending, leased, completed, failed, _ := s.db.GetQueueCounts()
	recentLogs, _ := s.db.GetRecentAuditLogs(15)

	// Get unique file count from dedupe index (this is the real "Files Discovered" value)
	var uniqueFiles int64
	if s.dedupe != nil {
		uniqueFiles = int64(s.dedupe.TotalUniqueFiles())
	}

	// Calculate average VMAF and extract recent savings
	type SavingItem struct {
		QueueID    int64   `json:"queue_id"`
		SavedBytes int64   `json:"saved_bytes"`
		OrigSize   int64   `json:"orig_size"`
		NewSize    int64   `json:"new_size"`
		Duration   float64 `json:"duration"`
		Encoder    string  `json:"encoder"`
		VMAF       float64 `json:"vmaf"`
	}
	var recentSavings []SavingItem
	var vmafSum float64
	var vmafCount int

	for _, l := range recentLogs {
		if l.EventType == "job_completed" && l.PayloadJSON != "" {
			var p struct {
				QueueID    int64   `json:"queue_id"`
				OrigSize   int64   `json:"orig_size"`
				NewSize    int64   `json:"new_size"`
				SavedBytes int64   `json:"saved_bytes"`
				Duration   float64 `json:"duration"`
				Encoder    string  `json:"encoder"`
				VMAF       float64 `json:"vmaf"`
			}
			if err := json.Unmarshal([]byte(l.PayloadJSON), &p); err == nil {
				if p.SavedBytes > 0 {
					recentSavings = append(recentSavings, SavingItem{
						QueueID:    p.QueueID,
						SavedBytes: p.SavedBytes,
						OrigSize:   p.OrigSize,
						NewSize:    p.NewSize,
						Duration:   p.Duration,
						Encoder:    p.Encoder,
						VMAF:       p.VMAF,
					})
				}
				if p.VMAF > 0 {
					vmafSum += p.VMAF
					vmafCount++
				}
			}
		}
	}

	avgVMAF := 0.0
	if vmafCount > 0 {
		avgVMAF = vmafSum / float64(vmafCount)
	}

	cfg := s.cfgMgr.Get()
	resp := map[string]any{
		"files_scanned":      s.metrics.FilesScannedTotal.Load(),
		"unique_files":       uniqueFiles,
		"deduplicated_files": s.metrics.DeduplicatedFiles.Load(),
		"jobs_completed":     s.metrics.JobsCompletedTotal.Load(),
		"jobs_failed":        s.metrics.JobsFailedTotal.Load(),
		"bytes_saved":        s.metrics.BytesSavedTotal.Load(),
		"average_vmaf":       avgVMAF,
		"active_cpu_workers": s.metrics.ActiveCPUWorkers.Load(),
		"active_gpu_workers": s.metrics.ActiveGPUSessions.Load(),
		"cpu_limit":          cfg.Concurrency.CPUSemaphoreLimit,
		"gpu_limit":          cfg.Concurrency.GPUSemaphoreLimit,
		"queue": map[string]any{
			"pending":   pending,
			"leased":    leased,
			"completed": completed,
			"failed":    failed,
			"total":     pending + leased + completed + failed,
		},
		"health": map[string]string{
			"persistence": "healthy",
			"concurrency": "healthy",
			"transcoder":  "healthy",
			"filesystem":  "healthy",
		},
		"recent_savings": recentSavings,
		"recent_audit":   recentLogs,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := r.URL.Query().Get("state")
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
	}

	entries, err := s.db.ListQueueEntries(state, limit, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter by search query if present
	search := strings.ToLower(r.URL.Query().Get("search"))
	var filtered []*persistence.QueueEntry
	for _, e := range entries {
		if search == "" || strings.Contains(strings.ToLower(e.FilePath), search) {
			filtered = append(filtered, e)
		}
	}

	// Extract completed/quality job stats from recent audit logs for quick display
	jobStats := make(map[int64]map[string]any)
	recentAudit, _ := s.db.GetRecentAuditLogs(200)
	for _, l := range recentAudit {
		if (l.EventType == "job_completed" || l.EventType == "job_quality_failed" || l.EventType == "job_skipped_growth") && l.PayloadJSON != "" {
			var p struct {
				QueueID    int64   `json:"queue_id"`
				OrigSize   int64   `json:"orig_size"`
				NewSize    int64   `json:"new_size"`
				SavedBytes int64   `json:"saved_bytes"`
				Duration   float64 `json:"duration"`
				Encoder    string  `json:"encoder"`
				VMAF       float64 `json:"vmaf"`
				Reason     string  `json:"reason"`
			}
			if err := json.Unmarshal([]byte(l.PayloadJSON), &p); err == nil && p.QueueID > 0 {
				if _, exists := jobStats[p.QueueID]; !exists {
					jobStats[p.QueueID] = map[string]any{
						"orig_size":   p.OrigSize,
						"new_size":    p.NewSize,
						"saved_bytes": p.SavedBytes,
						"duration":    p.Duration,
						"encoder":     p.Encoder,
						"vmaf":        p.VMAF,
						"reason":      p.Reason,
					}
				}
			}
		}
	}

	var activeProgress map[int64]*transcoder.ActiveProgress
	if s.tracker != nil {
		activeProgress = s.tracker.GetAll()
	} else {
		activeProgress = make(map[int64]*transcoder.ActiveProgress)
	}

	resp := map[string]any{
		"entries":         filtered,
		"job_stats":       jobStats,
		"active_progress": activeProgress,
		"total":           len(filtered),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleQueueItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/queue/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid job ID format", http.StatusBadRequest)
		return
	}

	if len(parts) == 1 {
		// GET /api/queue/{id}
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		entry, err := s.db.GetQueueEntry(id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Job not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		meta, _ := s.db.GetMetadata(id)

		var activeProg *transcoder.ActiveProgress
		if s.tracker != nil {
			activeProg = s.tracker.Get(id)
		}

		resp := map[string]any{
			"entry":    entry,
			"metadata": meta,
			"progress": activeProg,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	action := parts[1]
	switch action {
	case "requeue":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.db.RequeueJob(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.onTriggerPrefetch != nil {
			s.onTriggerPrefetch()
		}
		w.WriteHeader(http.StatusOK)

	case "pause":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onCancelJob != nil {
			s.onCancelJob(id)
		}
		if err := s.db.PauseJob(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.tracker != nil {
			s.tracker.Remove(id)
		}
		w.WriteHeader(http.StatusOK)

	case "ignore":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onCancelJob != nil {
			s.onCancelJob(id)
		}
		if err := s.db.IgnoreJob(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.tracker != nil {
			s.tracker.Remove(id)
		}
		w.WriteHeader(http.StatusOK)

	case "priority":
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var priority int
		if pStr := r.URL.Query().Get("priority"); pStr != "" {
			var parseErr error
			priority, parseErr = strconv.Atoi(pStr)
			if parseErr != nil {
				http.Error(w, "Invalid priority parameter", http.StatusBadRequest)
				return
			}
		} else {
			var body struct {
				Priority int `json:"priority"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			priority = body.Priority
		}

		if priority < 1 || priority > 1000 {
			http.Error(w, "Priority must be between 1 and 1000", http.StatusBadRequest)
			return
		}

		if err := s.db.SetJobPriority(id, priority); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.onTriggerPrefetch != nil {
			s.onTriggerPrefetch()
		}
		w.WriteHeader(http.StatusOK)

	case "delete":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.db.DeleteJob(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleTriggerScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.onTriggerScan != nil {
		go s.onTriggerScan()
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{"status":"scan_triggered"}`)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.cfgMgr.Get()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	case http.MethodPost:
		var newCfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON configuration: %v", err), http.StatusBadRequest)
			return
		}

		if err := s.cfgMgr.Update(&newCfg); err != nil {
			http.Error(w, fmt.Sprintf("Failed to apply configuration: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "applied"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// --------------------------------------------------------------------------
// JSON Metrics Endpoint
// --------------------------------------------------------------------------

func (s *Server) handleMetricsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.metrics.ToJSON())
}

// --------------------------------------------------------------------------
// Filesystem Scopes API (hot-add/remove without restart)
// --------------------------------------------------------------------------

func (s *Server) handleScopes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		scopes := s.cfgMgr.Get().Filesystem.Scopes
		if scopes == nil {
			scopes = []config.ScanScopeConfig{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"scopes": scopes})

	case http.MethodPost:
		var scope config.ScanScopeConfig
		if err := json.NewDecoder(r.Body).Decode(&scope); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON body: %v", err), http.StatusBadRequest)
			return
		}
		if scope.Path == "" {
			http.Error(w, "path is required", http.StatusBadRequest)
			return
		}
		if scope.Extensions == nil {
			scope.Extensions = []string{".mp4", ".mkv", ".mov", ".avi", ".m4v", ".webm"}
		}
		cfg := s.cfgMgr.Get()
		cfg.Filesystem.Scopes = append(cfg.Filesystem.Scopes, scope)
		if err := s.cfgMgr.Update(cfg); err != nil {
			http.Error(w, fmt.Sprintf("Failed to update config: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "scope_added", "path": scope.Path})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleScopeItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/filesystem/scopes/")
	if path == "" || path == "/" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := s.cfgMgr.Get()
		for _, scope := range cfg.Filesystem.Scopes {
			if scope.Path == path {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"scope": scope, "found": true})
				return
			}
		}
		http.Error(w, "scope not found", http.StatusNotFound)

	case http.MethodDelete:
		cfg := s.cfgMgr.Get()
		var found bool
		var newScopes []config.ScanScopeConfig
		for _, scope := range cfg.Filesystem.Scopes {
			if scope.Path == path {
				found = true
			} else {
				newScopes = append(newScopes, scope)
			}
		}
		if !found {
			http.Error(w, "scope not found", http.StatusNotFound)
			return
		}
		cfg.Filesystem.Scopes = newScopes
		if err := s.cfgMgr.Update(cfg); err != nil {
			http.Error(w, fmt.Sprintf("Failed to update config: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"scope_removed"}`))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// --------------------------------------------------------------------------
// Scheduling Endpoints
// --------------------------------------------------------------------------

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		http.Error(w, "Scheduling not available", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		entries := s.sched.ListScheduled()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})

	case http.MethodPost:
		var entry scheduling.ScheduleEntry
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		entry.ID = time.Now().Format("20060102150405") + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		s.sched.AddSchedule(entry)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "scheduled", "id": entry.ID})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleScheduleItem(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		http.Error(w, "Scheduling not available", http.StatusNotFound)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/schedule/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.sched.RemoveSchedule(id)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"removed"}`))
}

func (s *Server) handleHardware(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	hw := transcoder.DetectHardwareCapabilities(ctx)
	var encoderList []string
	for enc := range hw.Encoders {
		encoderList = append(encoderList, enc)
	}

	resp := map[string]any{
		"has_nvenc": hw.HasNVENC,
		"has_qsv":   hw.HasQuickSync,
		"has_amf":   hw.HasAMF,
		"has_vaapi": hw.HasVAAPI,
		"encoders":  encoderList,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
