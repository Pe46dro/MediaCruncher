package transcoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// CorruptionCheckResult holds the outcome of a corruption check.
type CorruptionCheckResult struct {
	FilePath   string `json:"file_path"`
	Corrupted  bool   `json:"corrupted"`
	Streams    int    `json:"streams"`
	Format     string `json:"format"`
	Valid      bool   `json:"valid"`
	Error      string `json:"error,omitempty"`
	ProbeTime  time.Duration `json:"probe_time"`
	Duration   float64 `json:"duration"`
	FrameCount int   `json:"frame_count"`
}

// CorruptionDetector validates transcoded output files.
type CorruptionDetector struct {
	BinaryPath string
}

// NewCorruptionDetector creates a new corruption detector.
func NewCorruptionDetector(binaryPath string) *CorruptionDetector {
	if binaryPath == "" {
		binaryPath = "ffprobe"
	}
	return &CorruptionDetector{
		BinaryPath: binaryPath,
	}
}

// Check validates a file for corruption using ffprobe.
func (d *CorruptionDetector) Check(ctx context.Context, filePath string) *CorruptionCheckResult {
	start := time.Now()
	result := &CorruptionCheckResult{
		FilePath:  filePath,
		ProbeTime: time.Since(start),
	}

	if _, err := os.Stat(filePath); err != nil {
		result.Error = fmt.Sprintf("file not found: %v", err)
		result.Corrupted = true
		result.Valid = false
		return result
	}

	sanitizedPath, pathErr := sanitizePath(filePath, nil)
	if pathErr != nil {
		result.Error = fmt.Sprintf("sanitize path: %v", pathErr)
		return result
	}

	cmd := exec.CommandContext(ctx,
		d.BinaryPath,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		sanitizedPath,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout

	err := cmd.Run()
	result.ProbeTime = time.Since(start)

	if err != nil {
		result.Error = err.Error()
		result.Corrupted = true
		result.Valid = false
		return result
	}

	var probeResult struct {
		Format struct {
			Duration string `json:"duration"`
			Name   string `json:"format_name"`
			Streams int `json:"nb_streams"`
		} `json:"format"`
		Streams []struct {
			Type   string `json:"codec_type"`
			FrameCount string `json:"nb_frames"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &probeResult); err != nil {
		result.Error = fmt.Sprintf("parse probe output: %v", err)
		result.Corrupted = true
		result.Valid = false
		return result
	}

	result.Streams = probeResult.Format.Streams
	result.Format = probeResult.Format.Name

	if probeResult.Format.Streams == 0 {
		result.Error = "no streams found"
		result.Corrupted = true
		result.Valid = false
		return result
	}

	result.Valid = true
	result.Corrupted = false

	return result
}

// CheckAndReport validates a file and reports the result with a human-readable message.
func (d *CorruptionDetector) CheckAndReport(ctx context.Context, filePath string) (bool, string) {
	result := d.Check(ctx, filePath)
	if result.Error != "" {
		return false, fmt.Sprintf("corruption check failed: %s", result.Error)
	}
	if result.Corrupted {
		return false, fmt.Sprintf("file is corrupted: %s", filePath)
	}
	return true, fmt.Sprintf("file is valid: %s (streams=%d, format=%s)", filePath, result.Streams, result.Format)
}
