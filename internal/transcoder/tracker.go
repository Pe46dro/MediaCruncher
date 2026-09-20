package transcoder

import (
	"fmt"
	"math"
	"sync"
	"time"
)

type JobPhase string

const (
	PhaseEvaluating    JobPhase = "evaluating"
	PhaseTranscoding   JobPhase = "transcoding"
	PhaseVerifyingVMAF JobPhase = "verifying_vmaf"
	PhasePromoting     JobPhase = "promoting"
	PhaseCompleted     JobPhase = "completed"
	PhaseFailed        JobPhase = "failed"
)

type ActiveProgress struct {
	JobID            int64     `json:"job_id"`
	Phase            JobPhase  `json:"phase"`
	PhaseDetail      string    `json:"phase_detail,omitempty"`
	Frames           int64     `json:"frames"`
	FPS              float64   `json:"fps"`
	Speed            float64   `json:"speed"`
	CurrentSec       float64   `json:"current_sec"`
	TotalSec         float64   `json:"total_sec"`
	Percentage       float64   `json:"percentage"`
	Bitrate          string    `json:"bitrate"`
	ETASeconds       int64     `json:"eta_seconds"`
	StartedAt        time.Time `json:"started_at"`
	LastUpdate       time.Time `json:"last_update"`
	IsStuck          bool      `json:"is_stuck"`
	StuckDurationSec int64     `json:"stuck_duration_sec,omitempty"`
}

// ProgressTracker maintains thread-safe live execution states for in-flight jobs.
type ProgressTracker struct {
	mu           sync.RWMutex
	items        map[int64]*ActiveProgress
	stuckTimeout time.Duration
}

// NewProgressTracker creates a new concurrent progress tracker.
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		items:        make(map[int64]*ActiveProgress),
		stuckTimeout: 45 * time.Second,
	}
}

// SetPhase transitions a job to a specific execution phase.
func (pt *ProgressTracker) SetPhase(jobID int64, phase JobPhase, detail string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now().UTC()
	item, exists := pt.items[jobID]
	if !exists {
		item = &ActiveProgress{
			JobID:     jobID,
			StartedAt: now,
		}
		pt.items[jobID] = item
	}

	item.Phase = phase
	item.PhaseDetail = detail
	item.LastUpdate = now
	item.IsStuck = false
	item.StuckDurationSec = 0
}

// UpdateTranscode updates the telemetry from ffmpeg progress line.
func (pt *ProgressTracker) UpdateTranscode(jobID int64, prog TranscodeProgress) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now().UTC()
	item, exists := pt.items[jobID]
	if !exists {
		item = &ActiveProgress{
			JobID:     jobID,
			StartedAt: now,
		}
		pt.items[jobID] = item
	}

	item.Phase = PhaseTranscoding
	item.Frames = prog.Frames
	item.FPS = prog.FPS
	item.Speed = prog.Speed
	item.CurrentSec = prog.CurrentSec
	item.TotalSec = prog.TotalSec
	item.Percentage = prog.Percentage
	item.Bitrate = prog.Bitrate
	item.LastUpdate = now
	item.IsStuck = false
	item.StuckDurationSec = 0

	// Calculate ETA
	if prog.Speed > 0 && prog.TotalSec > prog.CurrentSec {
		remainingSec := (prog.TotalSec - prog.CurrentSec) / prog.Speed
		item.ETASeconds = int64(math.Round(remainingSec))
	} else if prog.Percentage >= 99.9 {
		item.ETASeconds = 0
	}
}

// UpdateVMAF reports progress during stratified VMAF quality verification.
func (pt *ProgressTracker) UpdateVMAF(jobID int64, currentSegment, totalSegments int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now().UTC()
	item, exists := pt.items[jobID]
	if !exists {
		item = &ActiveProgress{
			JobID:     jobID,
			StartedAt: now,
		}
		pt.items[jobID] = item
	}

	item.Phase = PhaseVerifyingVMAF
	if totalSegments > 0 {
		item.PhaseDetail = fmt.Sprintf("VMAF analysis segment %d/%d", currentSegment, totalSegments)
		// Allocate 95% - 99% range for VMAF
		item.Percentage = 95.0 + (float64(currentSegment)/float64(totalSegments))*4.0
	} else {
		item.PhaseDetail = "VMAF quality verification in progress"
	}
	item.LastUpdate = now
	item.IsStuck = false
	item.StuckDurationSec = 0
}

// Get returns a clone of the progress item for a job with stuck detection applied.
func (pt *ProgressTracker) Get(jobID int64) *ActiveProgress {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	item, exists := pt.items[jobID]
	if !exists {
		return nil
	}

	clone := *item
	pt.checkStuck(&clone)
	return &clone
}

// GetAll returns a snapshot of all tracked active jobs with stuck status computed.
func (pt *ProgressTracker) GetAll() map[int64]*ActiveProgress {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	result := make(map[int64]*ActiveProgress, len(pt.items))
	for id, item := range pt.items {
		clone := *item
		pt.checkStuck(&clone)
		result[id] = &clone
	}
	return result
}

// Remove drops a job from the tracker upon terminal completion.
func (pt *ProgressTracker) Remove(jobID int64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	delete(pt.items, jobID)
}

func (pt *ProgressTracker) checkStuck(item *ActiveProgress) {
	// If the job is transcoding or verifying and hasn't reported progress within threshold
	threshold := pt.stuckTimeout
	if item.Phase == PhaseVerifyingVMAF {
		// VMAF segments can take up to 90s per segment
		threshold = 120 * time.Second
	}

	now := time.Now().UTC()
	silence := now.Sub(item.LastUpdate)
	if silence > threshold {
		item.IsStuck = true
		item.StuckDurationSec = int64(silence.Seconds())
	}
}
