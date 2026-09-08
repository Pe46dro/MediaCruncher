package transcoder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// TranscodeJob is the execution unit for the transcoder.
type TranscodeJob struct {
	SourcePath         string            `json:"source_path"`
	OutputPath         string            `json:"output_path"`
	Preset             *EncodingPreset   `json:"preset"`
	NegotiatedCodec    *NegotiatedCodec  `json:"negotiated_codec"`
	StagingDir         string            `json:"staging_dir"`
	JobID              string            `json:"job_id"`
	VMAFThreshold      float64           `json:"vmaf_threshold"`
	MaxDuration        time.Duration     `json:"max_duration"`
	Cancellation       context.CancelFunc `json:"-"`
	Attempt            int               `json:"attempt"`
	IsRetry            bool              `json:"is_retry"`
}

// EncodingResult reports the outcome of a transcoding operation.
type EncodingResult struct {
	JobID         string        `json:"job_id"`
	SourcePath    string        `json:"source_path"`
	OutputPath    string        `json:"output_path"`
	Duration      time.Duration `json:"duration"`
	OutputSize    int64         `json:"output_size"`
	ExitCode      int           `json:"exit_code"`
	Success       bool          `json:"success"`
	Error         string        `json:"error,omitempty"`
	Warnings      []string      `json:"warnings,omitempty"`
	CodecUsed     string        `json:"codec_used"`
	Acceleration  string        `json:"acceleration"`
	RetryAttempt  bool          `json:"retry_attempt"`
}

// Encoder executes ffmpeg encoding operations.
type Encoder struct {
	BinaryPath string
	StagingDir string
}

// NewEncoder creates a new encoder.
func NewEncoder(binaryPath, stagingDir string) *Encoder {
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	if stagingDir == "" {
		stagingDir = filepath.Join(os.TempDir(), "mediacruncher-staging")
	}
	return &Encoder{
		BinaryPath: binaryPath,
		StagingDir: stagingDir,
	}
}

// Execute runs the encoding process for the given job.
func (e *Encoder) Execute(ctx context.Context, job *TranscodeJob) *EncodingResult {
	start := time.Now()
	result := &EncodingResult{
		JobID:        job.JobID,
		SourcePath:   job.SourcePath,
		OutputPath:   job.OutputPath,
		CodecUsed:    job.NegotiatedCodec.Codec,
		Acceleration: job.NegotiatedCodec.Acceleration,
		RetryAttempt: job.IsRetry,
	}

	if job.NegotiatedCodec != nil {
		result.CodecUsed = job.NegotiatedCodec.Codec
		result.Acceleration = job.NegotiatedCodec.Acceleration
	}

	if job.Preset == nil {
		result.Error = "nil preset"
		result.Duration = time.Since(start)
		return result
	}

	encodedPath := filepath.Join(e.StagingDir, job.JobID+".out"+filepath.Ext(job.SourcePath))

	cmd, err := e.buildCommand(ctx, job, encodedPath)
	if err != nil {
		result.Error = fmt.Sprintf("build command: %v", err)
		result.Duration = time.Since(start)
		return result
	}

	var stderr bytes.Buffer
	if job.Cancellation != nil {
		go func() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
			}
		}()
	}

	cmd.Stderr = &stderr
	err = cmd.Run()
	result.Duration = time.Since(start)

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		result.Error = err.Error()
		result.Success = false

		if e.IsHardwareError(stderr.String()) {
			result.Warnings = append(result.Warnings, "hardware encoding failure, retry with software")
		}

		return result
	}

	result.ExitCode = 0
	result.Success = true

	info, err := os.Stat(encodedPath)
	if err == nil {
		result.OutputSize = info.Size()
		result.OutputPath = encodedPath
	} else {
		result.Warnings = append(result.Warnings, "could not stat output file")
	}

	return result
}

// buildCommand constructs the ffmpeg command for the given job.
func (e *Encoder) buildCommand(ctx context.Context, job *TranscodeJob, outputPath string) (*exec.Cmd, error) {
	preset := job.Preset
	codec := job.NegotiatedCodec
	if codec == nil {
		codec = &NegotiatedCodec{Codec: "h.264", Acceleration: "sw"}
	}

	args := []string{
		"-y",
		"-hide_banner",
	}

	if job.NegotiatedCodec.Acceleration != "sw" {
		args = append(args, "-hwaccel", job.NegotiatedCodec.Acceleration)
	if job.Preset != nil && job.Preset.PreferredDevice >= 0 {
			args = append(args, "-hwaccel_device", fmt.Sprintf("%d", job.Preset.PreferredDevice))
		}
	}

	sanitizedSource, srcErr := sanitizePath(job.SourcePath, []string{job.StagingDir})
	if srcErr != nil {
		return nil, fmt.Errorf("sanitize source path: %w", srcErr)
	}
	args = append(args, "-i", sanitizedSource)

	switch codec.Codec {
	case "h.264":
		if job.NegotiatedCodec.Acceleration == "cuda" {
			args = append(args, "-c:v", "h264_nvenc")
		} else if job.NegotiatedCodec.Acceleration == "qsv" {
			args = append(args, "-c:v", "h264_qsv")
		} else if job.NegotiatedCodec.Acceleration == "videotoolbox" {
			args = append(args, "-c:v", "h264_videotoolbox")
		} else {
			args = append(args, "-c:v", "libx264")
		}
	case "h.265":
		if job.NegotiatedCodec.Acceleration == "cuda" {
			args = append(args, "-c:v", "hevc_nvenc")
		} else if job.NegotiatedCodec.Acceleration == "qsv" {
			args = append(args, "-c:v", "hevc_qsv")
		} else if job.NegotiatedCodec.Acceleration == "videotoolbox" {
			args = append(args, "-c:v", "hevc_videotoolbox")
		} else {
			args = append(args, "-c:v", "libx265")
		}
	case "av1":
		if job.NegotiatedCodec.Acceleration == "qsv" {
			args = append(args, "-c:v", "av1_qsv")
		} else if job.NegotiatedCodec.Acceleration == "videotoolbox" {
			args = append(args, "-c:v", "av1_videotoolbox")
		} else {
			args = append(args, "-c:v", "libaom-av1")
		}
	default:
		args = append(args, "-c:v", codec.Codec)
	}

	if preset.QualityLevel > 0 {
		args = append(args, "-crf", fmt.Sprintf("%d", preset.QualityLevel))
	}

	if preset.PresetSpeed != "" {
		args = append(args, "-preset", preset.PresetSpeed)
	}

	if preset.Width > 0 && preset.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", preset.Width, preset.Height))
	}

	if preset.BitRate > 0 {
		args = append(args, "-b:v", fmt.Sprintf("%dk", preset.BitRate/1000))
	}

	switch job.NegotiatedCodec.Codec {
	case "h.264", "h.265", "av1":
		args = append(args, "-c:a", "aac")
		if preset.AudioBitRate > 0 {
			args = append(args, "-b:a", fmt.Sprintf("%dk", preset.AudioBitRate))
		}
	default:
		args = append(args, "-c:a", "copy")
	}

	sanitizedOutput, outErr := sanitizePath(outputPath, []string{job.StagingDir})
	if outErr != nil {
		return nil, fmt.Errorf("sanitize output path: %w", outErr)
	}
	args = append(args, sanitizedOutput)

	return exec.CommandContext(ctx, e.BinaryPath, args...), nil
}

// IsHardwareError checks if an ffmpeg error indicates a hardware device issue.
func (e *Encoder) IsHardwareError(stderr string) bool {
	hardwareErrors := []string{
		"device reset",
		"out of memory",
		"gpu",
		"hardware",
		"dxva",
		"cuda",
		"qsv",
		"vaapi",
		"videotoolbox",
		"failed to initialise",
	}
	lower := lowercase(stderr)
	for _, err := range hardwareErrors {
		if contains(lower, err) {
			return true
		}
	}
	return false
}

func lowercase(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c = c + ('a' - 'A')
		}
		result[i] = c
	}
	return string(result)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
