package transcoder

import (
	"context"
	"fmt"
	"os"
	"time"

	"mediacruncher/internal/observability"
)

// Engine is the main transcoder engine.
type Engine struct {
	hardwareProfile   *HardwareProfile
	negotiator        *Negotiator
	encoder           *Encoder
	vmafVerifier      *VMAFVerifier
	corruptionDetector *CorruptionDetector
	stagingManager    *StagingManager
	logger            *observability.Logger
	vmafThreshold     float64
	maxEncodingTime   time.Duration
	binaryPath        string
}

// Config holds configuration for the transcoder engine.
type Config struct {
	HardwareAcceleration bool
	PreferredDevice      int
	VMAFThreshold        float64
	MaxEncodingDuration  time.Duration
	BinaryPath           string
	StagingDir           string
	Logger               *observability.Logger
}

// New creates a new transcoder engine.
func New(cfg Config) *Engine {
	if cfg.VMAFThreshold <= 0 {
		cfg.VMAFThreshold = 90.0
	}
	if cfg.MaxEncodingDuration <= 0 {
		cfg.MaxEncodingDuration = 2 * time.Hour
	}

	eng := &Engine{
		vmafThreshold:   cfg.VMAFThreshold,
		maxEncodingTime: cfg.MaxEncodingDuration,
		binaryPath:      cfg.BinaryPath,
		logger:          cfg.Logger,
	}

	if eng.logger == nil {
		eng.logger = observability.NewStdLogger(observability.DebugLevel, "transcoder")
	}

	if cfg.HardwareAcceleration {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		disc := Discovery(ctx, cfg.BinaryPath, cfg.PreferredDevice)
		if disc != nil && disc.Profile != nil {
			eng.hardwareProfile = disc.Profile
			if eng.logger != nil {
				eng.logger.WithFields(
					observability.Field{Key: "devices_found", Value: disc.DevicesFound},
					observability.Field{Key: "discovery_time", Value: disc.DiscoveryTime.String()},
				).Info("hardware acceleration profile initialized")
				for _, dev := range disc.Profile.Devices {
					eng.logger.WithFields(
						observability.Field{Key: "name", Value: dev.Name},
						observability.Field{Key: "accel", Value: dev.Acceleration},
					).Info("detected hardware encoder device")
				}
			}
		}
	}

	eng.negotiator = NewNegotiator(eng.hardwareProfile)
	eng.encoder = NewEncoder(cfg.BinaryPath, cfg.StagingDir)
	eng.vmafVerifier = NewVMAFVerifier(cfg.BinaryPath, cfg.VMAFThreshold, "libsvm")
	eng.corruptionDetector = NewCorruptionDetector("ffprobe")
	eng.stagingManager = NewStagingManager(cfg.StagingDir, 24*time.Hour)

	return eng
}

// Transcode executes the full transcoding pipeline for a job.
func (e *Engine) Transcode(ctx context.Context, job *TranscodeJob) *TranscodeOutcome {
	start := time.Now()
	outcome := &TranscodeOutcome{
		JobID:      job.JobID,
		SourcePath: job.SourcePath,
		Attempt:    job.Attempt,
		IsRetry:    job.IsRetry,
	}

	if !e.validateSource(job) {
		outcome.Error = "source file validation failed"
		outcome.Status = TranscodeStatusFailed
		return outcome
	}

	stagingDir, err := e.stagingManager.Allocate(job.JobID)
	if err != nil {
		outcome.Error = fmt.Sprintf("allocate staging: %v", err)
		outcome.Status = TranscodeStatusFailed
		e.logError("staging allocation", err)
		return outcome
	}
	job.StagingDir = stagingDir

	negotiation := e.negotiator.Negotiate(job.Preset)
	if len(negotiation.ValidationErrors) > 0 {
		outcome.Error = "preset validation failed"
		outcome.Status = TranscodeStatusFailed
		return outcome
	}

	job.NegotiatedCodec = negotiation.NegotiatedCodec

	encodingCtx, cancel := context.WithTimeout(ctx, e.maxEncodingTime)
	defer cancel()
	job.Cancellation = cancel

	encodingResult := e.encoder.Execute(encodingCtx, job)
	outcome.Encoding = encodingResult

	if !encodingResult.Success {
		isHW := (job.NegotiatedCodec != nil && job.NegotiatedCodec.Acceleration != "sw") || e.encoder.IsHardwareError(encodingResult.Error)
		if !job.IsRetry && isHW {
			if e.logger != nil {
				e.logger.WithFields(
					observability.Field{Key: "error", Value: encodingResult.Error},
					observability.Field{Key: "job_id", Value: job.JobID},
				).Warn("hardware encoding failed, retrying with software fallback")
			}
			retryJob := &TranscodeJob{
				JobID:         job.JobID,
				SourcePath:    job.SourcePath,
				OutputPath:    job.OutputPath,
				StagingDir:    stagingDir,
				VMAFThreshold: job.VMAFThreshold,
				MaxDuration:   job.MaxDuration,
				Attempt:       job.Attempt + 1,
				IsRetry:       true,
			}
			if job.Preset != nil {
				p := *job.Preset
				p.HardwareAcceleration = false
				p.QualityLevel = max(20, p.QualityLevel-3)
				if p.PresetSpeed == "fast" || p.PresetSpeed == "veryfast" {
					p.PresetSpeed = "medium"
				}
				retryJob.Preset = &p
			} else {
				retryJob.Preset = &EncodingPreset{
					TargetCodec:          "h.265",
					QualityLevel:         18,
					PresetSpeed:          "slow",
					HardwareAcceleration: false,
				}
			}
			retryResult := e.Transcode(ctx, retryJob)
			outcome.Status = retryResult.Status
			outcome.Error = retryResult.Error
			outcome.Verification = retryResult.Verification
			outcome.OutputPath = retryResult.OutputPath
			outcome.Duration = time.Since(start)
			return outcome
		}
		outcome.Status = TranscodeStatusFailed
		outcome.Error = encodingResult.Error
		e.stagingManager.Cleanup(job.JobID)
		return outcome
	}

	vmafResult := e.vmafVerifier.VerifyWithFallback(ctx, job.SourcePath, encodingResult.OutputPath)
	outcome.Verification = vmafResult

	if !vmafResult.Passed {
		if !job.IsRetry {
			retryJob := &TranscodeJob{
				JobID:         job.JobID,
				SourcePath:    job.SourcePath,
				OutputPath:    job.OutputPath,
				StagingDir:    stagingDir,
				VMAFThreshold: job.VMAFThreshold,
				MaxDuration:   job.MaxDuration,
				Attempt:       job.Attempt + 1,
				IsRetry:       true,
			}
			if job.Preset != nil {
				p := *job.Preset
				p.QualityLevel = max(20, p.QualityLevel-3)
				if p.PresetSpeed == "fast" || p.PresetSpeed == "veryfast" {
					p.PresetSpeed = "medium"
				}
				retryJob.Preset = &p
			} else {
				retryJob.Preset = &EncodingPreset{
					TargetCodec:  "h.265",
					QualityLevel: 18,
					PresetSpeed:  "slow",
				}
			}
			retryResult := e.Transcode(ctx, retryJob)
			outcome.Status = retryResult.Status
			outcome.Error = retryResult.Error
			outcome.Verification = retryResult.Verification
			outcome.OutputPath = retryResult.OutputPath
			outcome.Duration = time.Since(start)
			return outcome
		}
		outcome.Status = TranscodeStatusQualityFailed
		e.stagingManager.Preserve(job.JobID)
		return outcome
	}

	if ok, msg := e.corruptionDetector.CheckAndReport(ctx, encodingResult.OutputPath); !ok {
		outcome.Status = TranscodeStatusFailed
		outcome.Error = msg
		e.stagingManager.Preserve(job.JobID)
		return outcome
	}

	if err := e.stagingManager.Commit(stagingDir, job.OutputPath); err != nil {
		e.stagingManager.Preserve(job.JobID)
		outcome.Status = TranscodeStatusFailed
		outcome.Error = fmt.Sprintf("commit output: %v", err)
		return outcome
	}

	outcome.Status = TranscodeStatusCompleted
	outcome.OutputPath = job.OutputPath
	outcome.Duration = time.Since(start)

	e.stagingManager.Cleanup(job.JobID)

	return outcome
}

// validateSource checks if the source file exists and is readable.
func (e *Engine) validateSource(job *TranscodeJob) bool {
	if job.SourcePath == "" {
		return false
	}
	info, err := os.Stat(job.SourcePath)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	return true
}

// GetHardwareProfile returns the current hardware profile.
func (e *Engine) GetHardwareProfile() *HardwareProfile {
	return e.hardwareProfile
}

// RefreshHardwareProfile refreshes the hardware capability profile.
func (e *Engine) RefreshHardwareProfile(ctx context.Context) *DiscoveryResult {
	return Discovery(ctx, e.binaryPath, 0)
}

// logError logs an encoding error.
func (e *Engine) logError(operation string, err error) {
	if e.logger != nil {
		e.logger.Error(fmt.Sprintf("%s failed: %v", operation, err))
	}
}

// TranscodeStatus represents the outcome status of a transcoding job.
type TranscodeStatus string

const (
	TranscodeStatusCompleted TranscodeStatus = "completed"
	TranscodeStatusFailed    TranscodeStatus = "failed"
	TranscodeStatusQualityFailed TranscodeStatus = "quality_failed"
)

// TranscodeOutcome reports the result of a transcoding operation.
type TranscodeOutcome struct {
	JobID        string            `json:"job_id"`
	SourcePath   string            `json:"source_path"`
	OutputPath   string            `json:"output_path"`
	Status       TranscodeStatus   `json:"status"`
	Error        string            `json:"error,omitempty"`
	Encoding     *EncodingResult   `json:"encoding"`
	Verification *VerificationResult `json:"verification"`
	Duration     time.Duration     `json:"duration"`
	Attempt      int               `json:"attempt"`
	IsRetry      bool              `json:"is_retry"`
}

// IsSuccess returns true if the transcode completed successfully.
func (o *TranscodeOutcome) IsSuccess() bool {
	return o.Status == TranscodeStatusCompleted
}

// NeedsReview returns true if the job needs manual review.
func (o *TranscodeOutcome) NeedsReview() bool {
	return o.Status == TranscodeStatusQualityFailed
}

// IsFailed returns true if the job failed.
func (o *TranscodeOutcome) IsFailed() bool {
	return o.Status == TranscodeStatusFailed
}

// max returns the larger of two integers.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
