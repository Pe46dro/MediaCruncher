package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics holds the application's runtime operational telemetry.
type Metrics struct {
	mu sync.RWMutex

	// Counters
	FilesScannedTotal   atomic.Uint64
	JobsSubmittedTotal  atomic.Uint64
	JobsCompletedTotal  atomic.Uint64
	JobsFailedTotal     atomic.Uint64
	QualityFailedTotal  atomic.Uint64
	BytesSavedTotal     atomic.Uint64
	DeduplicatedFiles   atomic.Uint64
	NotificationsSent   atomic.Uint64

	// Gauges
	QueuePendingDepth   atomic.Int64
	QueueLeasedDepth    atomic.Int64
	ActiveCPUWorkers    atomic.Int64
	ActiveGPUSessions   atomic.Int64
	WriteActorQueueSize atomic.Int64

	// Quality Tracking
	vmafSum   float64
	vmafCount int64

	// Module Health Map
	healthMap map[string]string
}

var globalMetrics = NewMetrics()

func GetMetrics() *Metrics {
	return globalMetrics
}

func NewMetrics() *Metrics {
	return &Metrics{
		healthMap: map[string]string{
			"persistence": "healthy",
			"concurrency": "healthy",
			"transcoder":  "healthy",
			"filesystem":  "healthy",
		},
	}
}

func (m *Metrics) SetModuleHealth(module, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthMap[module] = status
}

func (m *Metrics) RecordVMAF(score float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vmafSum += score
	m.vmafCount++
}

// PrometheusFormat exports current metrics in Prometheus text exposition format.
func (m *Metrics) PrometheusFormat() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	avgVMAF := 0.0
	if m.vmafCount > 0 {
		avgVMAF = m.vmafSum / float64(m.vmafCount)
	}

	return fmt.Sprintf(`# HELP mediacruncher_files_scanned_total Total media files discovered
# TYPE mediacruncher_files_scanned_total counter
mediacruncher_files_scanned_total %d

# HELP mediacruncher_jobs_submitted_total Total transcoding jobs submitted
# TYPE mediacruncher_jobs_submitted_total counter
mediacruncher_jobs_submitted_total %d

# HELP mediacruncher_jobs_completed_total Total transcoding jobs completed successfully
# TYPE mediacruncher_jobs_completed_total counter
mediacruncher_jobs_completed_total %d

# HELP mediacruncher_jobs_failed_total Total transcoding jobs failed permanently
# TYPE mediacruncher_jobs_failed_total counter
mediacruncher_jobs_failed_total %d

# HELP mediacruncher_quality_failed_total Total jobs that failed VMAF quality verification
# TYPE mediacruncher_quality_failed_total counter
mediacruncher_quality_failed_total %d

# HELP mediacruncher_deduplicated_files_total Total files skipped or flagged by deduplication
# TYPE mediacruncher_deduplicated_files_total counter
mediacruncher_deduplicated_files_total %d

# HELP mediacruncher_notifications_sent_total Total notifications delivered
# TYPE mediacruncher_notifications_sent_total counter
mediacruncher_notifications_sent_total %d

# HELP mediacruncher_queue_pending_depth Current pending entries in queue
# TYPE mediacruncher_queue_pending_depth gauge
mediacruncher_queue_pending_depth %d

# HELP mediacruncher_queue_leased_depth Current leased entries in worker prefetch
# TYPE mediacruncher_queue_leased_depth gauge
mediacruncher_queue_leased_depth %d

# HELP mediacruncher_active_cpu_workers Current active CPU workers
# TYPE mediacruncher_active_cpu_workers gauge
mediacruncher_active_cpu_workers %d

# HELP mediacruncher_active_gpu_sessions Current active GPU hardware encoding sessions
# TYPE mediacruncher_active_gpu_sessions gauge
mediacruncher_active_gpu_sessions %d

# HELP mediacruncher_write_actor_queue_size Pending write mutations in persistence channel
# TYPE mediacruncher_write_actor_queue_size gauge
mediacruncher_write_actor_queue_size %d

# HELP mediacruncher_vmaf_score_average Average VMAF score of transcoded media
# TYPE mediacruncher_vmaf_score_average gauge
mediacruncher_vmaf_score_average %.2f
`,
		m.FilesScannedTotal.Load(),
		m.JobsSubmittedTotal.Load(),
		m.JobsCompletedTotal.Load(),
		m.JobsFailedTotal.Load(),
		m.QualityFailedTotal.Load(),
		m.DeduplicatedFiles.Load(),
		m.NotificationsSent.Load(),
		m.QueuePendingDepth.Load(),
		m.QueueLeasedDepth.Load(),
		m.ActiveCPUWorkers.Load(),
		m.ActiveGPUSessions.Load(),
		m.WriteActorQueueSize.Load(),
		avgVMAF,
	)
}

// StartHTTPServer launches the metrics and health check HTTP listener.
func StartHTTPServer(port int, m *Metrics) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, m.PrometheusFormat())
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()

		overall := "healthy"
		for _, status := range m.healthMap {
			if status == "unhealthy" {
				overall = "unhealthy"
				break
			}
			if status == "degraded" && overall != "unhealthy" {
				overall = "degraded"
			}
		}

		response := map[string]any{
			"status":    overall,
			"modules":   m.healthMap,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"queue": map[string]any{
				"pending": m.QueuePendingDepth.Load(),
				"leased":  m.QueueLeasedDepth.Load(),
			},
			"workers": map[string]any{
				"cpu_active": m.ActiveCPUWorkers.Load(),
				"gpu_active": m.ActiveGPUSessions.Load(),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		if overall == "unhealthy" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		json.NewEncoder(w).Encode(response)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// server closed or failed
		}
	}()

	return srv
}

func StopHTTPServer(srv *http.Server, timeout time.Duration) error {
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}
