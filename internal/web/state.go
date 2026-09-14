package web

import (
	"sync"
	"time"
)

// ActiveJob represents a currently running transcode or evaluation job.
type ActiveJob struct {
	JobID         string    `json:"job_id"`
	SourcePath    string    `json:"source_path"`
	FileName      string    `json:"file_name"`
	TargetCodec   string    `json:"target_codec"`
	Preset        string    `json:"preset"`
	Stage         string    `json:"stage"`        // "analyzing", "transcoding", "verifying_vmaf", "checking_corruption", "finalizing"
	StageDetail   string    `json:"stage_detail"` // human readable detailed step description
	StartTime     time.Time `json:"start_time"`
	DurationMs    int64     `json:"duration_ms"`
	WorkerID      string    `json:"worker_id"`
	EstimatedProg int       `json:"estimated_progress"` // 0 - 100
}

// StateTracker manages runtime state for real-time dashboard visualization.
type StateTracker struct {
	mu            sync.RWMutex
	startTime     time.Time
	status        string // "idle", "scanning", "processing", "paused"
	isPaused      bool
	activeJobs    map[string]*ActiveJob
	jobCancels    map[string]func()
	lastScanTime  time.Time
	totalScans    int64
	hardwareAccel string
	targetCodec   string
	vmafThreshold float64
	maxDrop       float64
}

// NewStateTracker creates a new runtime state tracker.
func NewStateTracker() *StateTracker {
	return &StateTracker{
		startTime:  time.Now(),
		status:     "idle",
		isPaused:   false,
		activeJobs: make(map[string]*ActiveJob),
		jobCancels: make(map[string]func()),
	}
}

// Pause pauses daemon processing of new queue items.
func (s *StateTracker) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isPaused = true
}

// Resume resumes daemon processing.
func (s *StateTracker) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isPaused = false
}

// IsPaused reports if the daemon queue processing is paused.
func (s *StateTracker) IsPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isPaused
}

// SetSystemInfo sets static runtime parameters.
func (s *StateTracker) SetSystemInfo(hwAccel, targetCodec string, vmafThreshold, maxDrop float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hardwareAccel = hwAccel
	s.targetCodec = targetCodec
	s.vmafThreshold = vmafThreshold
	s.maxDrop = maxDrop
}

// SetStatus updates daemon status (e.g. "idle", "scanning", "processing").
func (s *StateTracker) SetStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// RecordScanCompleted records the completion of a scan.
func (s *StateTracker) RecordScanCompleted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScanTime = time.Now()
	s.totalScans++
	if len(s.activeJobs) == 0 {
		s.status = "idle"
	}
}

// RegisterJobCancel registers a cancellation function for an active job.
func (s *StateTracker) RegisterJobCancel(jobID string, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobCancels[jobID] = cancel
}

// CancelJob triggers cancellation for the given jobID and unregisters it.
func (s *StateTracker) CancelJob(jobID string) bool {
	s.mu.Lock()
	cancel, ok := s.jobCancels[jobID]
	if ok {
		delete(s.jobCancels, jobID)
	}
	s.mu.Unlock()

	if ok && cancel != nil {
		cancel()
		return true
	}
	return false
}

// AddActiveJob tracks a newly started job.
func (s *StateTracker) AddActiveJob(job *ActiveJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeJobs[job.JobID] = job
	s.status = "processing"
}

// UpdateJobStage updates the stage, detailed description, or estimated progress of an active job.
func (s *StateTracker) UpdateJobStage(jobID, stage, detail string, progress int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.activeJobs[jobID]; ok {
		if stage != "" {
			job.Stage = stage
		}
		if detail != "" {
			job.StageDetail = detail
		}
		if progress >= 0 {
			job.EstimatedProg = progress
		}
	}
}

// RemoveActiveJob clears an active job upon completion or failure.
func (s *StateTracker) RemoveActiveJob(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeJobs, jobID)
	delete(s.jobCancels, jobID)
	if len(s.activeJobs) == 0 && s.status == "processing" {
		s.status = "idle"
	}
}

// GetActiveJobs returns a slice of all currently running jobs.
func (s *StateTracker) GetActiveJobs() []*ActiveJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]*ActiveJob, 0, len(s.activeJobs))
	for _, job := range s.activeJobs {
		// Update duration
		jobCopy := *job
		jobCopy.DurationMs = time.Since(job.StartTime).Milliseconds()
		jobs = append(jobs, &jobCopy)
	}
	return jobs
}

// GetStatusSnapshot returns current status overview.
func (s *StateTracker) GetStatusSnapshot() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	activeCount := len(s.activeJobs)
	status := s.status
	if s.isPaused {
		status = "paused"
	} else if activeCount > 0 {
		status = "processing"
	}

	return map[string]interface{}{
		"status":           status,
		"is_paused":        s.isPaused,
		"uptime_seconds":   int64(time.Since(s.startTime).Seconds()),
		"active_jobs":      activeCount,
		"last_scan":        s.lastScanTime,
		"total_scans":      s.totalScans,
		"hardware_accel":   s.hardwareAccel,
		"target_codec":     s.targetCodec,
		"vmaf_threshold":   s.vmafThreshold,
		"max_quality_drop": s.maxDrop,
	}
}
