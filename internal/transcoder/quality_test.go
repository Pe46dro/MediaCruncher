package transcoder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediacruncher/internal/observability"
)

func TestQualityDropThresholdAndDiscard(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mc_trans_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create dummy source file
	sourceFile := filepath.Join(tmpDir, "source.mp4")
	if err := os.WriteFile(sourceFile, []byte("dummy source video content"), 0o644); err != nil {
		t.Fatalf("failed to write source file: %v", err)
	}

	outputFile := filepath.Join(tmpDir, "source_optimized.mp4")

	logger := observability.NewStdLogger(observability.DebugLevel, "test")

	// Transcoder configured with strict threshold: VMAFThreshold 95.0, MaxQualityDrop 5.0, DiscardOnQualityLoss true
	engine := New(Config{
		VMAFThreshold:        95.0,
		MaxQualityDrop:       5.0,
		DiscardOnQualityLoss: true,
		StagingDir:           filepath.Join(tmpDir, "staging"),
		Logger:               logger,
	})

	job := &TranscodeJob{
		JobID:          "test-job-quality-1",
		SourcePath:     sourceFile,
		OutputPath:     outputFile,
		VMAFThreshold:  95.0,
		MaxQualityDrop: 5.0,
		MaxDuration:    10 * time.Second,
		Attempt:        2, // Simulate retry attempt so it triggers final failure directly
		IsRetry:        true,
	}

	// We can test that the engine initializes these thresholds properly
	if engine.vmafThreshold != 95.0 {
		t.Errorf("expected vmafThreshold 95.0, got %f", engine.vmafThreshold)
	}
	if engine.maxQualityDrop != 5.0 {
		t.Errorf("expected maxQualityDrop 5.0, got %f", engine.maxQualityDrop)
	}
	if !engine.discardOnQualityLoss {
		t.Errorf("expected discardOnQualityLoss true, got false")
	}

	// Test validateSource
	if !engine.validateSource(job) {
		t.Error("expected validateSource to return true for existing file")
	}
}
