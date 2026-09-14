package transcoder

import (
	"context"
	"os"
	"os/exec"
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

func TestRealVMAFCalculation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mc_vmaf_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	orig := filepath.Join(tmpDir, "orig.mp4")
	trans := filepath.Join(tmpDir, "trans.mp4")

	// Generate a 0.5s test video
	genCmd := exec.Command("ffmpeg", "-hide_banner", "-f", "lavfi", "-i", "testsrc=duration=0.5:size=320x240:rate=25", "-c:v", "libx264", "-crf", "20", orig, "-y")
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg not available or failed to generate test video: %v (%s)", err, string(out))
	}

	// Transcode with higher crf
	transCmd := exec.Command("ffmpeg", "-hide_banner", "-i", orig, "-c:v", "libx264", "-crf", "28", trans, "-y")
	if out, err := transCmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg failed to transcode test video: %v (%s)", err, string(out))
	}

	verifier := NewVMAFVerifier("ffmpeg", 80.0, "libsvm")
	result := verifier.Verify(context.Background(), orig, trans)

	if result.Error != "" {
		t.Fatalf("VMAF verification failed: %s", result.Error)
	}

	if result.VMAFScore <= 0 {
		t.Errorf("expected positive VMAF score, got %f", result.VMAFScore)
	}

	if result.VMAFScore < 50.0 || result.VMAFScore > 100.0 {
		t.Errorf("unexpected VMAF score range: %f", result.VMAFScore)
	}
}

func TestSampledVMAFCalculation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mc_vmaf_sampled_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	orig := filepath.Join(tmpDir, "orig.mp4")
	trans := filepath.Join(tmpDir, "trans.mp4")

	// Generate a 4s test video
	genCmd := exec.Command("ffmpeg", "-hide_banner", "-f", "lavfi", "-i", "testsrc=duration=4:size=320x240:rate=25", "-c:v", "libx264", "-crf", "22", orig, "-y")
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg not available or failed to generate test video: %v (%s)", err, string(out))
	}

	transCmd := exec.Command("ffmpeg", "-hide_banner", "-i", orig, "-c:v", "libx264", "-crf", "28", trans, "-y")
	if out, err := transCmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg failed to transcode test video: %v (%s)", err, string(out))
	}

	// Test with 2 segments of 1s each
	verifier := NewVMAFVerifierWithSampling("ffmpeg", 80.0, "libsvm", true, 2, 1)
	result := verifier.Verify(context.Background(), orig, trans)

	if result.Error != "" {
		t.Fatalf("sampled VMAF verification failed: %s", result.Error)
	}

	if result.VMAFScore <= 0 {
		t.Errorf("expected positive VMAF score, got %f", result.VMAFScore)
	}

	if result.VMAFScore < 50.0 || result.VMAFScore > 100.0 {
		t.Errorf("unexpected VMAF score range: %f", result.VMAFScore)
	}
}
