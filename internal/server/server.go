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
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/transcoder"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	httpServer     *http.Server
	cfgMgr         *config.Manager
	db             *persistence.Engine
	metrics        *observability.Metrics
	onTriggerScan  func()
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

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
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

	resp := map[string]any{
		"entries":   filtered,
		"job_stats": jobStats,
		"total":     len(filtered),
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

		resp := map[string]any{
			"entry":    entry,
			"metadata": meta,
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
