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
	Stage         string    `json:"stage"` // "analyzing", "transcoding", "verifying_vmaf", "finalizing"
	StartTime     time.Time `json:"start_time"`
	DurationMs    int64     `json:"duration_ms"`
	WorkerID      string    `json:"worker_id"`
	EstimatedProg int       `json:"estimated_progress"` // 0 - 100
}

// StateTracker manages runtime state for real-time dashboard visualization.
type StateTracker struct {
	mu            sync.RWMutex
	startTime     time.Time
	status        string // "idle", "scanning", "processing"
	activeJobs    map[string]*ActiveJob
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
		activeJobs: make(map[string]*ActiveJob),
	}
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

// AddActiveJob tracks a newly started job.
func (s *StateTracker) AddActiveJob(job *ActiveJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeJobs[job.JobID] = job
	s.status = "processing"
}

// UpdateJobStage updates the stage or estimated progress of an active job.
func (s *StateTracker) UpdateJobStage(jobID, stage string, progress int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.activeJobs[jobID]; ok {
		job.Stage = stage
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
	if activeCount > 0 {
		status = "processing"
	}

	return map[string]interface{}{
		"status":          status,
		"uptime_seconds":  int64(time.Since(s.startTime).Seconds()),
		"active_jobs":     activeCount,
		"last_scan":       s.lastScanTime,
		"total_scans":     s.totalScans,
		"hardware_accel":  s.hardwareAccel,
		"target_codec":    s.targetCodec,
		"vmaf_threshold":  s.vmafThreshold,
		"max_quality_drop": s.maxDrop,
	}
}
