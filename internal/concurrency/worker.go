package concurrency

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/transcoder"
)

type EventCallback func(event string, payload any)

// WorkerPool manages parallel worker goroutines, hardware semaphores, and lifecycle stages.
type WorkerPool struct {
	cfg           config.ConcurrencyConfig
	db            *persistence.Engine
	evalPipeline  *evaluation.Pipeline
	tc            *transcoder.Transcoder
	prefetcher    *Prefetcher
	gpuSem        Semaphore
	cpuSem        Semaphore
	activeWorkers atomic.Int64
	inFlight      sync.Map // entryID -> context.CancelFunc
	onEvent       EventCallback
	stopCh        chan struct{}
	wg            sync.WaitGroup
	running       atomic.Bool
}

func NewWorkerPool(
	cfg config.ConcurrencyConfig,
	db *persistence.Engine,
	eval *evaluation.Pipeline,
	tc *transcoder.Transcoder,
	onEvent EventCallback,
) *WorkerPool {
	prefetcher := NewPrefetcher(
		db,
		fmt.Sprintf("worker-pool-%d", os.Getpid()),
		cfg.PrefetchBufferDepth,
		30*time.Minute,
	)

	return &WorkerPool{
		cfg:          cfg,
		db:           db,
		evalPipeline: eval,
		tc:           tc,
		prefetcher:   prefetcher,
		gpuSem:       NewSemaphore(cfg.GPUSemaphoreLimit),
		cpuSem:       NewSemaphore(cfg.CPUSemaphoreLimit),
		onEvent:      onEvent,
		stopCh:       make(chan struct{}),
	}
}

// Start launches the prefetcher and configured number of worker routines.
func (wp *WorkerPool) Start(ctx context.Context) {
	if !wp.running.CompareAndSwap(false, true) {
		return
	}

	wp.prefetcher.Start(ctx)

	numWorkers := wp.cfg.WorkerCount
	if numWorkers <= 0 {
		numWorkers = 4
	}

	for i := 0; i < numWorkers; i++ {
		wp.wg.Add(1)
		go wp.workerRoutine(ctx, i)
	}
}

// TriggerPrefetch nudges the prefetcher to check for newly enqueued items immediately.
func (wp *WorkerPool) TriggerPrefetch() {
	wp.prefetcher.Trigger()
}

// DrainAndStop initiates graceful draining of in-flight jobs up to timeout.
func (wp *WorkerPool) DrainAndStop(timeout time.Duration) {
	if !wp.running.CompareAndSwap(true, false) {
		return
	}

	close(wp.stopCh)
	wp.prefetcher.Stop()

	// Wait for in-flight tasks with timeout
	done := make(chan struct{})
	go func() {
		wp.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("Worker pool gracefully drained all active jobs")
	case <-time.After(timeout):
		slog.Warn("Drain timeout exceeded; terminating in-flight jobs", "timeout", timeout)
		// Cancel any lingering tasks
		wp.inFlight.Range(func(key, val any) bool {
			if cancel, ok := val.(context.CancelFunc); ok {
				cancel()
			}
			return true
		})
		wp.wg.Wait()
	}
}

func (wp *WorkerPool) workerRoutine(parentCtx context.Context, workerID int) {
	defer wp.wg.Done()

	for {
		select {
		case <-parentCtx.Done():
			return
		case <-wp.stopCh:
			return
		case entry, ok := <-wp.prefetcher.Queue():
			if !ok {
				return
			}
			wp.activeWorkers.Add(1)
			wp.processEntry(parentCtx, entry)
			wp.activeWorkers.Add(-1)
		}
	}
}

func (wp *WorkerPool) processEntry(parentCtx context.Context, entry *persistence.QueueEntry) {
	taskCtx, cancel := context.WithCancel(parentCtx)
	wp.inFlight.Store(entry.ID, cancel)
	defer func() {
		wp.inFlight.Delete(entry.ID)
		cancel()
	}()

	// 1. Stage: Evaluation
	if err := wp.db.UpdateJobState(entry.ID, persistence.StateEvaluating, ""); err != nil {
		slog.Error("Failed to update job state to evaluating", "id", entry.ID, "err", err)
	}

	record, err := wp.evalPipeline.EvaluateFile(taskCtx, entry.FilePath)
	if err != nil {
		wp.handleFailure(entry, fmt.Errorf("evaluation failed: %w", err))
		return
	}

	// Save analysis metadata to database
	meta := &persistence.JobMetadata{
		QueueID:        entry.ID,
		VideoCodec:     record.Metadata.VideoCodec,
		Resolution:     record.Metadata.ResolutionTag,
		Bitrate:        record.Metadata.TotalBitrate,
		Duration:       record.Metadata.Duration,
		AudioTracks:    fmt.Sprintf("%d tracks", len(record.Metadata.AudioTracks)),
		Subtitles:      fmt.Sprintf("%d tracks", len(record.Metadata.SubtitleTracks)),
		DecisionAction: record.Decision.Action,
		Preset:         record.Decision.Preset,
		StreamMapJSON:  record.StreamMapJSON,
		NormalizedJSON: record.NormalizedJSON,
	}
	if err := wp.db.SaveMetadata(meta); err != nil {
		slog.Error("Failed to save job metadata", "id", entry.ID, "err", err)
	}

	// 2. Act on Decision
	switch record.Decision.Action {
	case "skip":
		wp.db.UpdateJobState(entry.ID, persistence.StateSkipped, record.Decision.MatchedRule)
		wp.db.RecordAudit(&persistence.AuditLog{
			EventType:   "job_skipped",
			Severity:    "info",
			PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"action":"skip","size":%d,"reason":%q}`, entry.ID, record.Metadata.FileSize, record.Decision.MatchedRule),
		})
		if wp.onEvent != nil {
			wp.onEvent("job_skipped", entry)
		}
		return

	case "review":
		wp.db.UpdateJobState(entry.ID, persistence.StateReviewRequired, "flagged for review")
		return

	case "copy":
		// Direct stream copy
		wp.db.UpdateJobState(entry.ID, persistence.StateCompleted, "stream copied")
		wp.db.RecordAudit(&persistence.AuditLog{
			EventType:   "job_copied",
			Severity:    "info",
			PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"action":"copy","size":%d}`, entry.ID, record.Metadata.FileSize),
		})
		if wp.onEvent != nil {
			wp.onEvent("job_completed", entry)
		}
		return

	case "transcode":
		wp.executeTranscode(taskCtx, entry, record)
	default:
		wp.executeTranscode(taskCtx, entry, record)
	}
}

func (wp *WorkerPool) executeTranscode(ctx context.Context, entry *persistence.QueueEntry, record *evaluation.DecisionRecord) {
	if err := wp.db.UpdateJobState(entry.ID, persistence.StateTranscoding, ""); err != nil {
		slog.Error("Failed to update job state to transcoding", "id", entry.ID, "err", err)
	}

	preset := wp.tc.SelectPreset(record.Decision.Preset)

	// Determine destination path based on overwrite configuration
	tcCfg := wp.tc.GetConfig()
	destPath := entry.FilePath
	overwrite := true
	if tcCfg.OverwriteSource != nil {
		overwrite = *tcCfg.OverwriteSource
	}

	if !overwrite {
		dir := filepath.Dir(entry.FilePath)
		if tcCfg.OutputDir != "" {
			dir = tcCfg.OutputDir
		}
		ext := filepath.Ext(entry.FilePath)
		base := strings.TrimSuffix(filepath.Base(entry.FilePath), ext)
		suffix := tcCfg.OutputSuffix
		if suffix == "" {
			suffix = "_transcoded"
		}
		destPath = filepath.Join(dir, base+suffix+ext)
	}

	job := &transcoder.TranscodeJob{
		ID:         fmt.Sprintf("job-%d", entry.ID),
		SourcePath: entry.FilePath,
		DestPath:   destPath,
		Plan:       record.StreamPlan,
		Preset:     preset,
		Duration:   record.Metadata.Duration,
		SourceSize: record.Metadata.FileSize,
	}

	// Acquire resource semaphore (GPU or CPU)
	sem := wp.cpuSem
	// If preset or system uses hardware acceleration, acquire GPU semaphore
	// Check hardware acceleration availability
	hwProfile := transcoder.DetectHardwareCapabilities(ctx)
	_, isHW := hwProfile.SelectEncoder(preset.VideoCodec, "auto")
	if isHW {
		sem = wp.gpuSem
	}

	if err := sem.Acquire(ctx); err != nil {
		wp.handleFailure(entry, fmt.Errorf("semaphore acquisition failed: %w", err))
		return
	}
	defer sem.Release()

	// Track active worker gauge in metrics
	metrics := observability.GetMetrics()
	if isHW {
		metrics.ActiveGPUSessions.Add(1)
		defer metrics.ActiveGPUSessions.Add(-1)
	} else {
		metrics.ActiveCPUWorkers.Add(1)
		defer metrics.ActiveCPUWorkers.Add(-1)
	}

	// Run transcode engine
	result, err := wp.tc.Execute(ctx, job)
	if err != nil {
		wp.handleFailure(entry, fmt.Errorf("transcode execution error: %w", err))
		return
	}

	// Check if skipped due to size growth
	if result.SkippedSizeGrowth {
		wp.db.UpdateJobState(entry.ID, persistence.StateSkipped, result.VerificationReason)
		wp.db.RecordAudit(&persistence.AuditLog{
			EventType:   "job_skipped_growth",
			Severity:    "warn",
			PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"orig_size":%d,"new_size":%d,"reason":%q}`, entry.ID, result.OriginalSize, result.NewSize, result.VerificationReason),
		})
		if wp.onEvent != nil {
			wp.onEvent("job_skipped_growth", result)
		}
		return
	}

	// Check if quality verification failed
	if result.FailedQualityCheck {
		wp.db.UpdateJobState(entry.ID, persistence.StateQualityFailed, result.VerificationReason)
		wp.db.RecordAudit(&persistence.AuditLog{
			EventType:   "job_quality_failed",
			Severity:    "warn",
			PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"orig_size":%d,"new_size":%d,"vmaf":%.2f,"reason":%q}`, entry.ID, result.OriginalSize, result.NewSize, result.VMAFScore, result.VerificationReason),
		})
		if wp.onEvent != nil {
			wp.onEvent("job_quality_failed", result)
		}
		return
	}

	// Success! Update DB and metrics
	wp.db.UpdateJobState(entry.ID, persistence.StateCompleted, "")
	wp.db.RecordAudit(&persistence.AuditLog{
		EventType:   "job_completed",
		Severity:    "info",
		PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"orig_size":%d,"new_size":%d,"saved_bytes":%d,"vmaf":%.2f,"duration":%.2f,"encoder":%q,"hw":%v}`, entry.ID, result.OriginalSize, result.NewSize, result.SavedBytes, result.VMAFScore, result.DurationSec, result.EncoderUsed, result.IsHardware),
	})

	metrics.JobsCompletedTotal.Add(1)
	if result.SavedBytes > 0 {
		metrics.BytesSavedTotal.Add(uint64(result.SavedBytes))
	}
	if result.VMAFScore > 0 {
		metrics.RecordVMAF(result.VMAFScore)
	}

	if wp.onEvent != nil {
		wp.onEvent("job_completed", result)
	}
}

func (wp *WorkerPool) handleFailure(entry *persistence.QueueEntry, err error) {
	metrics := observability.GetMetrics()
	metrics.JobsFailedTotal.Add(1)

	maxRetries := wp.cfg.RetryMaxAttempts
	if maxRetries <= 0 {
		maxRetries = 3
	}

	if entry.RetryCount < maxRetries {
		// Calculate exponential backoff with full jitter
		base := wp.cfg.RetryBaseInterval
		if base <= 0 {
			base = 15 * time.Second
		}
		backoff := float64(base) * math.Pow(2, float64(entry.RetryCount))
		jitter := randJitter(backoff * 0.2)
		delay := time.Duration(backoff + jitter)

		slog.Warn("Job failed, requeuing with exponential backoff",
			"id", entry.ID,
			"retry", entry.RetryCount+1,
			"max_retries", maxRetries,
			"delay", delay,
			"err", err,
		)

		_ = wp.db.RequeueWithBackoff(entry.ID, delay, err.Error())
		if wp.onEvent != nil {
			wp.onEvent("job_retried", entry)
		}
	} else {
		slog.Error("Job failed permanently after exceeding retry limit",
			"id", entry.ID,
			"retries", entry.RetryCount,
			"err", err,
		)

		_ = wp.db.UpdateJobState(entry.ID, persistence.StatePermanentlyFailed, err.Error())
		_ = wp.db.RecordAudit(&persistence.AuditLog{
			EventType:   "job_failed",
			Severity:    "error",
			PayloadJSON: fmt.Sprintf(`{"queue_id":%d,"error":%q}`, entry.ID, err.Error()),
		})
		if wp.onEvent != nil {
			wp.onEvent("job_failed", entry)
		}
	}
}

func randJitter(maxJitter float64) float64 {
	if maxJitter <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := binary.LittleEndian.Uint64(b[:])
	ratio := float64(n) / float64(math.MaxUint64)
	return ratio * maxJitter
}
