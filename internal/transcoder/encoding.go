package transcoder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	encodedDir := e.StagingDir
	if job.StagingDir != "" {
		encodedDir = job.StagingDir
	}
	encodedPath := filepath.Join(encodedDir, filepath.Base(job.OutputPath))

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
		stderrMsg := strings.TrimSpace(stderr.String())
		if stderrMsg != "" {
			result.Error = fmt.Sprintf("%v: %s", err, stderrMsg)
		} else {
			result.Error = err.Error()
		}
		result.Success = false

		if e.IsHardwareError(result.Error) {
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
		codec = &NegotiatedCodec{Codec: "h.265", Acceleration: "sw"}
	}

	args := []string{
		"-y",
		"-hide_banner",
	}

	if codec.Acceleration != "sw" && codec.Acceleration != "" {
		if codec.Acceleration == "cuda" || codec.Acceleration == "qsv" || codec.Acceleration == "vaapi" || codec.Acceleration == "d3d11va" || codec.Acceleration == "dxva2" {
			args = append(args, "-hwaccel", codec.Acceleration)
			if preset != nil && preset.PreferredDevice >= 0 && codec.Acceleration != "vaapi" {
				args = append(args, "-hwaccel_device", fmt.Sprintf("%d", preset.PreferredDevice))
			}
		}
	}

	sanitizedSource, srcErr := sanitizePath(job.SourcePath, nil)
	if srcErr != nil {
		return nil, fmt.Errorf("sanitize source path: %w", srcErr)
	}
	args = append(args, "-i", sanitizedSource)

	normalizedCodec := normalizeCodec(codec.Codec)
	var videoEncoder string

	switch normalizedCodec {
	case "h.264":
		switch codec.Acceleration {
		case "cuda":
			videoEncoder = "h264_nvenc"
		case "qsv":
			videoEncoder = "h264_qsv"
		case "amf":
			videoEncoder = "h264_amf"
		case "vaapi":
			videoEncoder = "h264_vaapi"
		case "videotoolbox":
			videoEncoder = "h264_videotoolbox"
		default:
			videoEncoder = "libx264"
		}
	case "h.265":
		switch codec.Acceleration {
		case "cuda":
			videoEncoder = "hevc_nvenc"
		case "qsv":
			videoEncoder = "hevc_qsv"
		case "amf":
			videoEncoder = "hevc_amf"
		case "vaapi":
			videoEncoder = "hevc_vaapi"
		case "videotoolbox":
			videoEncoder = "hevc_videotoolbox"
		default:
			videoEncoder = "libx265"
		}
	case "av1":
		switch codec.Acceleration {
		case "cuda":
			videoEncoder = "av1_nvenc"
		case "qsv":
			videoEncoder = "av1_qsv"
		case "amf":
			videoEncoder = "av1_amf"
		case "vaapi":
			videoEncoder = "av1_vaapi"
		case "videotoolbox":
			videoEncoder = "av1_videotoolbox"
		default:
			videoEncoder = "libsvtav1"
		}
	default:
		videoEncoder = codec.Codec
	}

	args = append(args, "-c:v", videoEncoder)

	if preset != nil && preset.QualityLevel > 0 {
		switch codec.Acceleration {
		case "cuda":
			args = append(args, "-cq", fmt.Sprintf("%d", preset.QualityLevel))
		case "qsv":
			args = append(args, "-global_quality", fmt.Sprintf("%d", preset.QualityLevel))
		case "videotoolbox":
			args = append(args, "-q:v", fmt.Sprintf("%d", preset.QualityLevel))
		default:
			args = append(args, "-crf", fmt.Sprintf("%d", preset.QualityLevel))
		}
	}

	if preset != nil && preset.PresetSpeed != "" {
		args = append(args, "-preset", preset.PresetSpeed)
	}

	if preset != nil && preset.Width > 0 && preset.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", preset.Width, preset.Height))
	}

	if preset != nil && preset.BitRate > 0 {
		args = append(args, "-b:v", fmt.Sprintf("%dk", preset.BitRate/1000))
	}

	if normalizedCodec == "h.265" && strings.HasSuffix(strings.ToLower(outputPath), ".mp4") {
		args = append(args, "-tag:v", "hvc1")
	}

	switch normalizedCodec {
	case "h.264", "h.265", "av1":
		args = append(args, "-c:a", "aac")
		if preset != nil && preset.AudioBitRate > 0 {
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
		"nvenc",
		"qsv",
		"amf",
		"vaapi",
		"videotoolbox",
		"failed to initialise",
		"cannot load",
		"driver",
		"no device",
		"unknown encoder",
		"device creation failed",
		"function not implemented",
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
