package transcoder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
)

func TestParseProgressLine(t *testing.T) {
	line := "frame=  120 fps= 60.5 q=-0.0 size=    1024kB time=00:00:04.50 bitrate=1863.7kbits/s speed=2.27x"
	prog := parseProgressLine(line, "test-job-1", 10.0)
	if prog == nil {
		t.Fatalf("expected parsed progress, got nil")
	}

	if prog.Frames != 120 {
		t.Errorf("expected 120 frames, got %d", prog.Frames)
	}
	if prog.FPS != 60.5 {
		t.Errorf("expected 60.5 fps, got %f", prog.FPS)
	}
	if prog.CurrentSec != 4.5 {
		t.Errorf("expected 4.5 sec, got %f", prog.CurrentSec)
	}
	if prog.Percentage != 45.0 {
		t.Errorf("expected 45.0%%, got %f", prog.Percentage)
	}
	if prog.Speed != 2.27 {
		t.Errorf("expected 2.27x speed, got %f", prog.Speed)
	}
}

func TestHardwareSelection(t *testing.T) {
	hp := &HardwareProfile{
		HasNVENC: true,
		Encoders: map[string]bool{
			"hevc_nvenc": true,
			"h264_nvenc": true,
			"libx265":   true,
			"libx264":   true,
		},
	}

	enc, isHW := hp.SelectEncoder("hevc", "nvenc")
	if !isHW || enc != "hevc_nvenc" {
		t.Errorf("expected hevc_nvenc (hw=true), got %s (hw=%v)", enc, isHW)
	}

	encCPU, isHWCPU := hp.SelectEncoder("hevc", "cpu")
	if isHWCPU || encCPU != "libx265" {
		t.Errorf("expected libx265 (hw=false), got %s (hw=%v)", encCPU, isHWCPU)
	}
}

func TestPromoteFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "promote_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	staged := filepath.Join(tmpDir, "staged.mp4")
	dest := filepath.Join(tmpDir, "dest.mp4")

	if err := os.WriteFile(staged, []byte("transcoded video data content"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Initial promote
	if err := PromoteFile(staged, dest, false); err != nil {
		t.Fatalf("PromoteFile failed: %v", err)
	}

	if _, err := os.Stat(dest); err != nil {
		t.Errorf("destination file does not exist after promotion")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged file was not removed after promotion")
	}

	// 2. In-place overwrite promotion
	staged2 := filepath.Join(tmpDir, "staged2.mp4")
	if err := os.WriteFile(staged2, []byte("updated transcoded video data"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := PromoteFile(staged2, dest, true); err != nil {
		t.Fatalf("in-place PromoteFile failed: %v", err)
	}

	content, err := os.ReadFile(dest)
	if err != nil || string(content) != "updated transcoded video data" {
		t.Errorf("dest content mismatch after in-place promotion: %s", string(content))
	}
}

func TestTranscodeExecutionSynthetic(t *testing.T) {
	// Check if ffmpeg exists in environment
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found, skipping execution test")
	}

	tmpDir, err := os.MkdirTemp("", "transcode_synth_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	sourceFile := filepath.Join(tmpDir, "synth_src.mp4")
	destFile := filepath.Join(tmpDir, "synth_dst.mp4")

	// Generate a 1-second synthetic SMPTE colorbar video with sine wave audio
	genCmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=duration=1:frequency=440",
		"-c:v", "libx264", "-c:a", "aac",
		sourceFile,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic video: %v, out: %s", err, string(out))
	}

	srcInfo, err := os.Stat(sourceFile)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.TranscoderConfig{
		HardwareAcceleration: "cpu", // force software for deterministic test
		StagingDir:           filepath.Join(tmpDir, "staging"),
		VMAFEnabled:          false, // skip VMAF for 1s test clip
		SkipIfLarger:         false, // allow testing promotion even if synthetic clip is small
		Presets: []config.PresetConfig{
			{
				Name:        "test-fast",
				VideoCodec:  "h264",
				QualityCRF:  28,
				PresetSpeed: "ultrafast",
				AudioCodec:  "copy",
			},
		},
	}

	var progressEvents []TranscodeProgress
	tc := NewTranscoder(cfg, func(p TranscodeProgress) {
		progressEvents = append(progressEvents, p)
	})

	job := &TranscodeJob{
		ID:         "synth-job-1",
		SourcePath: sourceFile,
		DestPath:   destFile,
		Plan: evaluation.StreamPlan{
			MapArgs:        []string{"-map", "0:v:0", "-map", "0:a:0"},
			AudioCodecArgs: []string{"-c:a:0", "copy"},
		},
		Preset:     tc.SelectPreset("test-fast"),
		Duration:   1.0,
		SourceSize: srcInfo.Size(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := tc.Execute(ctx, job)
	if err != nil {
		t.Fatalf("Transcode execution failed: %v", err)
	}

	if result == nil {
		t.Fatal("expected result, got nil")
	}

	if _, err := os.Stat(destFile); err != nil {
		t.Errorf("destination file %s was not created: %v", destFile, err)
	}

	t.Logf("Transcode completed: Original=%d bytes, New=%d bytes, Duration=%.2fs",
		result.OriginalSize, result.NewSize, result.DurationSec)

	// Test SkipIfLarger = true
	cfgSkip := cfg
	cfgSkip.SkipIfLarger = true
	tcSkip := NewTranscoder(cfgSkip, nil)
	destSkip := filepath.Join(tmpDir, "synth_dst_skip.mp4")
	jobSkip := &TranscodeJob{
		ID:         "synth-job-skip",
		SourcePath: sourceFile,
		DestPath:   destSkip,
		Plan:       job.Plan,
		Preset:     job.Preset,
		Duration:   1.0,
		SourceSize: 100, // force small source size to trigger skip
	}
	resSkip, err := tcSkip.Execute(ctx, jobSkip)
	if err != nil {
		t.Fatalf("unexpected error on skip: %v", err)
	}
	if !resSkip.SkippedSizeGrowth {
		t.Errorf("expected SkippedSizeGrowth = true, got false")
	}
	if _, err := os.Stat(destSkip); !os.IsNotExist(err) {
		t.Errorf("expected skipped destination file to not exist, but it exists")
	}
}
