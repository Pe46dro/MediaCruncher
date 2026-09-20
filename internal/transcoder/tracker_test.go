package transcoder

import (
	"testing"
	"time"
)

func TestProgressTracker(t *testing.T) {
	tracker := NewProgressTracker()

	// 1. Initial State / Not Found
	if tracker.Get(100) != nil {
		t.Fatalf("expected nil for non-existent job")
	}

	// 2. Set Phase (Evaluating)
	tracker.SetPhase(100, PhaseEvaluating, "Probing streams")
	p := tracker.Get(100)
	if p == nil {
		t.Fatalf("expected progress for job 100")
	}
	if p.Phase != PhaseEvaluating || p.PhaseDetail != "Probing streams" {
		t.Errorf("unexpected phase or detail: %s, %s", p.Phase, p.PhaseDetail)
	}

	// 3. Update Transcode Progress and ETA
	tracker.UpdateTranscode(100, TranscodeProgress{
		JobID:      "job-100",
		Frames:     500,
		FPS:        50.0,
		Speed:      2.0,
		CurrentSec: 20.0,
		TotalSec:   100.0,
		Percentage: 20.0,
		Bitrate:    "2500kbits/s",
	})

	p = tracker.Get(100)
	if p.Phase != PhaseTranscoding {
		t.Errorf("expected phase transcoding, got %s", p.Phase)
	}
	if p.Percentage != 20.0 {
		t.Errorf("expected 20%%, got %f", p.Percentage)
	}
	// ETA = (100 - 20) / 2.0 = 40 seconds
	if p.ETASeconds != 40 {
		t.Errorf("expected ETA 40 seconds, got %d", p.ETASeconds)
	}
	if p.IsStuck {
		t.Errorf("job should not be stuck right after update")
	}

	// 4. Update VMAF Progress
	tracker.UpdateVMAF(100, 2, 3)
	p = tracker.Get(100)
	if p.Phase != PhaseVerifyingVMAF {
		t.Errorf("expected phase verifying_vmaf, got %s", p.Phase)
	}
	if p.PhaseDetail != "VMAF analysis segment 2/3" {
		t.Errorf("unexpected vmaf detail: %s", p.PhaseDetail)
	}
	// Percentage: 95 + (2/3)*4 = 97.666%
	if p.Percentage < 97.0 || p.Percentage > 98.0 {
		t.Errorf("expected vmaf percentage around 97.6%%, got %f", p.Percentage)
	}

	// 5. Stuck Detection
	// Artificially simulate silence
	tracker.mu.Lock()
	tracker.items[100].LastUpdate = time.Now().UTC().Add(-150 * time.Second)
	tracker.mu.Unlock()

	p = tracker.Get(100)
	if !p.IsStuck {
		t.Errorf("expected job to be marked stuck after 150s silence")
	}
	if p.StuckDurationSec < 140 {
		t.Errorf("expected stuck duration > 140s, got %d", p.StuckDurationSec)
	}

	// 6. GetAll and Remove
	all := tracker.GetAll()
	if len(all) != 1 || all[100] == nil {
		t.Errorf("expected 1 item in GetAll, got %d", len(all))
	}

	tracker.Remove(100)
	if tracker.Get(100) != nil {
		t.Errorf("expected nil after remove")
	}
}
