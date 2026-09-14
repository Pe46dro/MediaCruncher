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
	vmafThreshold        float64
	maxQualityDrop       float64
	discardOnQualityLoss bool
	maxEncodingTime      time.Duration
	binaryPath           string
}

// Config holds configuration for the transcoder engine.
type Config struct {
	HardwareAcceleration bool
	PreferredDevice      int
	VMAFThreshold        float64
	MaxQualityDrop       float64
	DiscardOnQualityLoss bool
	MaxEncodingDuration  time.Duration
	BinaryPath           string
	StagingDir           string
	Logger               *observability.Logger
	VMAFSampling         bool
	VMAFSampleSegments   int
	VMAFSampleDuration   int
}

// New creates a new transcoder engine.
func New(cfg Config) *Engine {
	if cfg.VMAFThreshold <= 0 {
		cfg.VMAFThreshold = 90.0
	}
	if cfg.MaxQualityDrop <= 0 {
		cfg.MaxQualityDrop = 10.0
	}
	if cfg.MaxEncodingDuration <= 0 {
		cfg.MaxEncodingDuration = 2 * time.Hour
	}

	eng := &Engine{
		vmafThreshold:        cfg.VMAFThreshold,
		maxQualityDrop:       cfg.MaxQualityDrop,
		discardOnQualityLoss: cfg.DiscardOnQualityLoss,
		maxEncodingTime:      cfg.MaxEncodingDuration,
		binaryPath:           cfg.BinaryPath,
		logger:               cfg.Logger,
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
					if dev.Healthy && dev.Acceleration != "sw" {
						eng.logger.WithFields(
							observability.Field{Key: "name", Value: dev.Name},
							observability.Field{Key: "accel", Value: dev.Acceleration},
						).Info("detected hardware encoder device")
					}
				}
			}
		}
	}

	eng.negotiator = NewNegotiator(eng.hardwareProfile)
	eng.encoder = NewEncoder(cfg.BinaryPath, cfg.StagingDir)
	eng.vmafVerifier = NewVMAFVerifierWithSampling(cfg.BinaryPath, cfg.VMAFThreshold, "libsvm", cfg.VMAFSampling, cfg.VMAFSampleSegments, cfg.VMAFSampleDuration)
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

	if job.StageCallback != nil {
		accel := "CPU/Software"
		codecName := "h.265"
		if job.NegotiatedCodec != nil {
			if job.NegotiatedCodec.Codec != "" {
				codecName = job.NegotiatedCodec.Codec
			}
			if job.NegotiatedCodec.Acceleration != "sw" {
				accel = fmt.Sprintf("Hardware (%s)", job.NegotiatedCodec.Acceleration)
			}
		}
		job.StageCallback("transcoding", fmt.Sprintf("Decodifica sorgente & Ottimizzazione in %s via %s", codecName, accel), 35)
	}

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
			if job.StageCallback != nil {
				job.StageCallback("transcoding", "Fallback su decodifica/codifica software CPU", 20)
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
				StageCallback: job.StageCallback,
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

	if job.StageCallback != nil {
		detail := "Calcolo metrica VMAF (confronto qualità video sorgente/output)"
		if e.vmafVerifier != nil && e.vmafVerifier.config.SampleSegments > 0 {
			detail = fmt.Sprintf("Calcolo VMAF campionato (%d segmenti da %ds)", e.vmafVerifier.config.SampleSegments, e.vmafVerifier.config.SampleDuration)
		}
		job.StageCallback("verifying_vmaf", detail, 75)
	}

	vmafResult := e.vmafVerifier.VerifyWithFallback(ctx, job.SourcePath, encodingResult.OutputPath)
	outcome.Verification = vmafResult

	effectiveThreshold := e.vmafThreshold
	if job.VMAFThreshold > 0 {
		effectiveThreshold = job.VMAFThreshold
	}
	effectiveMaxDrop := e.maxQualityDrop
	if job.MaxQualityDrop > 0 {
		effectiveMaxDrop = job.MaxQualityDrop
	}

	qualityDrop := 100.0 - vmafResult.VMAFScore
	qualityFailed := vmafResult.VMAFScore < effectiveThreshold || (effectiveMaxDrop > 0 && qualityDrop > effectiveMaxDrop)

	if qualityFailed {
		if !job.IsRetry {
			if job.StageCallback != nil {
				job.StageCallback("transcoding", "Retry automatico con parametri di qualità più elevati", 30)
			}
			retryJob := &TranscodeJob{
				JobID:          job.JobID,
				SourcePath:     job.SourcePath,
				OutputPath:     job.OutputPath,
				StagingDir:     stagingDir,
				VMAFThreshold:  effectiveThreshold,
				MaxQualityDrop: effectiveMaxDrop,
				MaxDuration:    job.MaxDuration,
				Attempt:        job.Attempt + 1,
				IsRetry:        true,
				StageCallback:  job.StageCallback,
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
		outcome.Error = fmt.Sprintf("quality loss exceeded threshold: VMAF %.2f (min %.2f), drop %.2f (max allowed: %.2f)", vmafResult.VMAFScore, effectiveThreshold, qualityDrop, effectiveMaxDrop)

		if e.discardOnQualityLoss {
			if e.logger != nil {
				e.logger.WithFields(
					observability.Field{Key: "job_id", Value: job.JobID},
					observability.Field{Key: "vmaf_score", Value: vmafResult.VMAFScore},
					observability.Field{Key: "quality_drop", Value: qualityDrop},
				).Warn("video quality drop exceeded threshold, deleting processed video and keeping original")
			}
			e.stagingManager.Cleanup(job.JobID)
			if job.OutputPath != "" {
				_ = os.Remove(job.OutputPath)
			}
		} else {
			e.stagingManager.Preserve(job.JobID)
		}
		return outcome
	}

	if job.StageCallback != nil {
		job.StageCallback("checking_corruption", "Controllo corruzione stream e integrità contenitore", 90)
	}

	if ok, msg := e.corruptionDetector.CheckAndReport(ctx, encodingResult.OutputPath); !ok {
		outcome.Status = TranscodeStatusFailed
		outcome.Error = msg
		e.stagingManager.Preserve(job.JobID)
		return outcome
	}

	if job.StageCallback != nil {
		job.StageCallback("finalizing", "Commit staging e finalizzazione file su disco", 95)
	}

	if err := e.stagingManager.Commit(stagingDir, job.OutputPath); err != nil {
		e.stagingManager.Preserve(job.JobID)
		outcome.Status = TranscodeStatusFailed
		outcome.Error = fmt.Sprintf("commit output: %v", err)
		return outcome
	}

	outcome.Status = TranscodeStatusCompleted
	outcome.OutputPath = job.OutputPath
	if encodingResult != nil && encodingResult.OutputSize > 0 {
		outcome.OutputSize = encodingResult.OutputSize
	} else if fi, statErr := os.Stat(job.OutputPath); statErr == nil {
		outcome.OutputSize = fi.Size()
	}
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
	OutputSize   int64             `json:"output_size,omitempty"`
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
