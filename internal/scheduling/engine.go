// Package scheduling provides a time-based scheduling engine for automated
// filesystem scans and media processing.
//
// Architecture:
//
//	The Engine runs two goroutines:
//	  - processLoop:   Handles manual triggers, job cancellations, and periodic
//	                   checks of scheduled entries (every 5s).
//	  - tickLoop:      Fires on a 30-second ticker; checks configured time windows
//	                   and triggers scans when current minute matches window start/end.
//
// Entry lifecycle:
//	  1. AddSchedule() — enqueues entry with optional FilePathTargets or empty (full scope scan)
//	  2. checkScheduledJobs() — on 5s interval, finds entries past their Scheduled time
//	  3. executeSchedule() — deduplicates, enqueues paths to DB, increments metrics
//	  4. Entries are marked Executed=true and remain in the list (purged manually via RemoveSchedule)
//
// Timeout behavior:
//	  checkScheduledJobs releases the mutex while calling executeSchedule to allow
//	  concurrent AddSchedule/RemoveSchedule. The entry is re-locked afterward.
//
// Configuration-driven window scans:
//	  If SchedulingConfig.ScanWindow is set, the tickLoop automatically triggers
//	  scans at the configured start time and cancels work at the configured end time.
//	  Times are parsed as "HH:MM" or "HH:MM:SS".
//
// Thread safety:
//	  All public methods lock e.mu. The triggerCh/cancelCh channels are buffered (cap 10)
//	  to prevent goroutine leaks when the processLoop is busy.
package scheduling

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/dedupe"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
)

// ScheduleEntry represents a single scheduled transcode job.
type ScheduleEntry struct {
	ID        string
	FilePaths []string // list of file paths to schedule
	Priority  int
	Scheduled time.Time
	Executed  bool
	CreatedAt time.Time
}

// Engine manages scheduled/transcode jobs for the daemon.
type Engine struct {
	mu              sync.Mutex
	scheduled       []*ScheduleEntry // ordered by Scheduled time
	queuedJobs      map[string]bool  // prevent duplicate scheduling
	triggerCh       chan ScheduleEntry
	cancelCh        chan string     // cancel scheduled job by ID
	workerCount     int
	schedulingCfg   config.SchedulingConfig
	dedupe          *dedupe.Index
	db              *persistence.Engine
	metrics         *observability.Metrics
	onScheduled     func(ScheduleEntry)
	onCancel        func(string)
	triggerScan     func()              // triggered when scheduling wants a filesystem scan
}

// SetTriggerScan attaches the callback that fires when scheduling wants a scan.
func (e *Engine) SetTriggerScan(fn func()) {
	e.triggerScan = fn
}

// NewEngine creates a new scheduling engine.
func NewEngine(cfg config.SchedulingConfig, workerCount int, dedupeIdx *dedupe.Index, db *persistence.Engine, metrics *observability.Metrics) *Engine {
	return &Engine{
		scheduled:       make([]*ScheduleEntry, 0),
		queuedJobs:      make(map[string]bool),
		triggerCh:       make(chan ScheduleEntry, 10),
		cancelCh:        make(chan string, 10),
		workerCount:     workerCount,
		schedulingCfg:   cfg,
		dedupe:          dedupeIdx,
		db:              db,
		metrics:         metrics,
	}
}

// Start starts the engine and registers scheduled scanning jobs on a ticker.
func (e *Engine) Start(ctx context.Context) {
	if !e.schedulingCfg.Enabled {
		return
	}

	go e.processLoop(ctx)
	go e.tickLoop(ctx)
}

// Stop stops the engine and cancels all scheduled jobs.
func (e *Engine) Stop() {
}

// AddSchedule enqueues a one-shot scheduled job.
func (e *Engine) AddSchedule(entry ScheduleEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if entry.Priority <= 0 {
		entry.Priority = 50
	}
	if entry.Scheduled.IsZero() || entry.Scheduled.Before(time.Now()) {
		entry.Scheduled = time.Now().Add(1 * time.Second)
	}

	e.scheduled = append(e.scheduled, &entry)

	if e.onScheduled != nil {
		e.onScheduled(entry)
	}
}

// RemoveSchedule removes a scheduled job by ID.
func (e *Engine) RemoveSchedule(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, entry := range e.scheduled {
		if entry.ID == id {
			e.scheduled = append(e.scheduled[:i], e.scheduled[i+1:]...)
			break
		}
	}

	if e.onCancel != nil {
		e.onCancel(id)
	}
}

// ListScheduled returns all pending/queued scheduled entries.
func (e *Engine) ListScheduled() []ScheduleEntry {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := make([]ScheduleEntry, len(e.scheduled))
	for i, entry := range e.scheduled {
		result[i] = *entry
	}
	return result
}

// StartManualScan triggers a one-time manual scan immediately.
func (e *Engine) StartManualScan(ctx context.Context) {
	entry := ScheduleEntry{
		ID:        "manual-" + time.Now().Format("20060102150405"),
		Priority:  75,
		Scheduled: time.Now(),
		CreatedAt: time.Now(),
	}

	select {
	case e.triggerCh <- entry:
	default:
	}
}

func (e *Engine) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case <-e.triggerCh:
			if e.triggerScan != nil {
				e.triggerScan()
			}
			e.metrics.FilesScannedTotal.Add(1)

		case id := <-e.cancelCh:
			e.cancelJob(id)

		case <-time.After(5 * time.Second):
			e.checkScheduledJobs(ctx)
		}
	}
}

func (e *Engine) executeSchedule(ctx context.Context, entry ScheduleEntry) {
	if entry.FilePaths != nil {
		// Scan specific file paths
		for _, path := range entry.FilePaths {
			if !filepath.IsAbs(path) {
				continue
			}

			e.queuedJobs[path] = true
			if e.dedupe != nil {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					_, _, err := e.dedupe.CheckAndRecord(path, info.Size())
					if err != nil {
						slog.Warn("schedule: dedupe check error", "path", path, "error", err)
					}
				}
			}

			if _, err := e.db.Enqueue(path, entry.Priority); err != nil {
				slog.Warn("schedule: failed to enqueue", "path", path, "error", err)
			}
		}
	} else {
		// Scan all configured filesystem scopes
		e.metrics.FilesScannedTotal.Add(1)
	}

	observability.GetMetrics().JobsSubmittedTotal.Add(1)
}

func (e *Engine) cancelJob(id string) {
	if e.onCancel != nil {
		e.onCancel(id)
	}
	slog.Info("schedule: job cancelled", "id", id)
}

func (e *Engine) checkScheduledJobs(ctx context.Context) {
	now := time.Now()

	e.mu.Lock()
	defer e.mu.Unlock()

	for i, entry := range e.scheduled {
		if entry.Executed || entry.Scheduled.After(now) {
			continue
		}

		e.mu.Unlock()
		e.executeSchedule(ctx, *entry)
		e.mu.Lock()

		entry.Executed = true
		e.scheduled[i] = entry
	}
}

// parseTimeMinutes parses "HH:MM" or "HH:MM:SS" into minutes since midnight.
func parseTimeMinutes(timeStr string) (int, error) {
	var h, m, s int
	n, err := fmt.Sscanf(timeStr, "%d:%d:%d", &h, &m, &s)
	if n < 2 || err != nil {
		// Fallback to HH:MM format
		n, err = fmt.Sscanf(timeStr, "%d:%d", &h, &m)
	}
	if n < 2 {
		return 0, fmt.Errorf("unable to parse time string '%s'", timeStr)
	}
	if err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("unable to parse time string '%s'", timeStr)
	}
	return h*60 + m, nil
}

// tickLoop checks for time-based window triggers from the scheduling config.
func (e *Engine) tickLoop(ctx context.Context) {
	// Check every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.checkWindows(ctx)
		}
	}
}

func (e *Engine) checkWindows(ctx context.Context) {
	now := time.Now()
	currentMinute := now.Hour()*60 + now.Minute()

	for _, window := range e.schedulingCfg.ScanWindow {
		if window.Start == "" || window.End == "" {
			continue
		}

		startMinutes, err := parseTimeMinutes(window.Start)
		if err != nil {
			continue
		}

		endMinutes, err := parseTimeMinutes(window.End)
		if err != nil {
			continue
		}

		// Check if we're exactly at the start minute
		if currentMinute >= startMinutes && currentMinute < startMinutes+1 {
			select {
			case e.triggerCh <- ScheduleEntry{
				ID:        "window-" + window.Start,
				Scheduled: now,
				Priority:  50,
				CreatedAt: now,
			}:
			default:
			}
			slog.Info("scheduling: scan window started", "start", window.Start, "end", window.End)
		}

		// Check if we're at the end minute
		if currentMinute >= endMinutes && currentMinute < endMinutes+1 {
			select {
			case e.cancelCh <- "window-ended":
			default:
			}
			slog.Info("scheduling: scan window ended", "start", window.Start, "end", window.End)
		}
	}
}
