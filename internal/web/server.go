package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
)

//go:embed static/*
var staticFS embed.FS

// Server is the HTTP server for the real-time web dashboard and REST/SSE APIs.
type Server struct {
	httpServer      *http.Server
	broker          *Broker
	state           *StateTracker
	db              *persistence.Engine
	triggerScanFunc func()
	logger          *observability.Logger
	addr            string
}

// Config holds configuration for the web server.
type Config struct {
	Addr            string
	Broker          *Broker
	State           *StateTracker
	Persistence     *persistence.Engine
	TriggerScanFunc func()
	Logger          *observability.Logger
}

// NewServer creates a new web server instance.
func NewServer(cfg Config) *Server {
	addr := cfg.Addr
	if addr == "" {
		addr = "0.0.0.0:8080"
	}

	s := &Server{
		addr:            addr,
		broker:          cfg.Broker,
		state:           cfg.State,
		db:              cfg.Persistence,
		triggerScanFunc: cfg.TriggerScanFunc,
		logger:          cfg.Logger,
	}

	mux := http.NewServeMux()

	// Static assets embedded
	subFS, err := fs.Sub(staticFS, "static")
	if err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(subFS))))
	}

	// Dashboard Root
	mux.HandleFunc("/", s.handleIndex)

	// REST APIs
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/jobs", s.handleJobs)
	mux.HandleFunc("/api/media", s.handleMedia)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/scan", s.handleScanTrigger)

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // 0 for streaming SSE
	}

	return s
}

// Start launches the HTTP server in a separate goroutine.
func (s *Server) Start() error {
	if s.logger != nil {
		s.logger.WithFields(
			observability.Field{Key: "addr", Value: s.addr},
		).Info("starting real-time web dashboard server")
	}

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if s.logger != nil {
				s.logger.WithFields(observability.Field{Key: "error", Value: err}).Error("web server error")
			}
		}
	}()

	return nil
}

// Shutdown gracefully terminates the HTTP server and client connections.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.broker != nil {
		s.broker.Close()
	}
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// handleIndex serves the embedded index.html.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleStatus returns the current runtime and persistent stats snapshot.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	snapshot := make(map[string]interface{})
	if s.state != nil {
		snapshot = s.state.GetStatusSnapshot()
	}

	// Add SQLite persistence statistics
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if stats, err := s.db.GetProcessedStats(ctx); err == nil {
			snapshot["stats"] = stats
		}
		if depth, err := s.db.GetPendingDepth(ctx); err == nil {
			snapshot["queue_depth"] = depth
		}
	}

	json.NewEncoder(w).Encode(snapshot)
}

// handleJobs returns the active transcoding jobs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var jobs []*ActiveJob
	if s.state != nil {
		jobs = s.state.GetActiveJobs()
	}
	if jobs == nil {
		jobs = []*ActiveJob{}
	}
	json.NewEncoder(w).Encode(jobs)
}

// handleMedia returns paginated processed media from the database.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.db == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"records": []interface{}{}, "total": 0})
		return
	}

	limit := 50
	offset := 0
	status := r.URL.Query().Get("status")

	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}
	if oStr := r.URL.Query().Get("offset"); oStr != "" {
		if o, err := strconv.Atoi(oStr); err == nil && o >= 0 {
			offset = o
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	records, total, err := s.db.ListProcessedMedia(ctx, limit, offset, status)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []persistence.ProcessedMediaRecord{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"records": records,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// handleEvents streams Server-Sent Events (SSE) to connected clients.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if s.broker == nil {
		fmt.Fprintf(w, ": broker not configured\n\n")
		flusher.Flush()
		return
	}

	ch, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()

	// Initial connect frame
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			w.Write(msg)
			flusher.Flush()
		}
	}
}

// handleScanTrigger handles an on-demand trigger to scan the filesystem scopes.
func (s *Server) handleScanTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.triggerScanFunc != nil {
		go s.triggerScanFunc()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "scan_triggered"})
}
