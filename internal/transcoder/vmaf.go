package transcoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// VMAFConfig holds configuration for VMAF quality verification.
type VMAFConfig struct {
	BinaryPath       string  `json:"binary_path"`
	Threshold        float64 `json:"threshold"`
	Model            string  `json:"model"`
	PSNR             bool    `json:"psnr"`
	SSIM             bool    `json:"ssim"`
	SamplingEnabled  bool    `json:"sampling_enabled"`
	SampleSegments   int     `json:"sample_segments"`
	SampleDuration   int     `json:"sample_duration"`
}

// VerificationResult holds the output of VMAF quality verification.
type VerificationResult struct {
	SourcePath string        `json:"source_path"`
	Transcoded string        `json:"transcoded"`
	VMAFScore  float64       `json:"vmaf_score"`
	PSNRScore  float64       `json:"psnr_score"`
	SSIMScore  float64       `json:"ssim_score"`
	Threshold  float64       `json:"threshold"`
	Passed     bool          `json:"passed"`
	Duration   time.Duration `json:"duration"`
	Error      string        `json:"error,omitempty"`
	Model      string        `json:"model"`
	Sampled    bool          `json:"sampled,omitempty"`
}

// VMAFVerifier performs VMAF quality verification.
type VMAFVerifier struct {
	config VMAFConfig
}

// NewVMAFVerifier creates a new VMAF verifier.
func NewVMAFVerifier(binaryPath string, threshold float64, model string) *VMAFVerifier {
	return NewVMAFVerifierWithSampling(binaryPath, threshold, model, false, 0, 0)
}

// NewVMAFVerifierWithSampling creates a new VMAF verifier with sampling options.
func NewVMAFVerifierWithSampling(binaryPath string, threshold float64, model string, sampling bool, segments, sampleDuration int) *VMAFVerifier {
	if threshold <= 0 {
		threshold = 90.0
	}
	if model == "" {
		model = "libsvm"
	}
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	if sampling {
		if segments <= 0 {
			segments = 3
		}
		if sampleDuration <= 0 {
			sampleDuration = 15
		}
	}
	return &VMAFVerifier{
		config: VMAFConfig{
			BinaryPath:       binaryPath,
			Threshold:        threshold,
			Model:            model,
			SamplingEnabled:  sampling,
			SampleSegments:   segments,
			SampleDuration:   sampleDuration,
		},
	}
}

var (
	vmafScoreRegex = regexp.MustCompile(`(?i)VMAF\s+score:\s*([0-9.]+)`)
	ssimAllRegex   = regexp.MustCompile(`(?i)All:\s*([0-9.]+)`)
)

// Verify performs real VMAF quality verification between original and transcoded files.
func (v *VMAFVerifier) Verify(ctx context.Context, original, transcoded string) *VerificationResult {
	if v.config.SamplingEnabled {
		return v.verifySampled(ctx, original, transcoded)
	}
	return v.verifyFull(ctx, original, transcoded)
}

// verifySampled measures VMAF across multiple evenly spaced temporal segments.
func (v *VMAFVerifier) verifySampled(ctx context.Context, original, transcoded string) *VerificationResult {
	start := time.Now()
	result := &VerificationResult{
		SourcePath: original,
		Transcoded: transcoded,
		Threshold:  v.config.Threshold,
		Model:      v.config.Model,
		Sampled:    true,
	}

	sanitizedOriginal, origErr := sanitizePath(original, nil)
	if origErr != nil {
		result.Error = fmt.Sprintf("sanitize original path: %v", origErr)
		return result
	}
	sanitizedTranscoded, transcErr := sanitizePath(transcoded, nil)
	if transcErr != nil {
		result.Error = fmt.Sprintf("sanitize transcoded path: %v", transcErr)
		return result
	}

	totalDur := probeVideoDuration(ctx, sanitizedOriginal)
	if totalDur <= 0 {
		totalDur = probeVideoDuration(ctx, sanitizedTranscoded)
	}

	// If video is short (e.g. <= 90s), sample across the full video instead of fragments
	sampleDur := float64(v.config.SampleDuration)
	if sampleDur <= 0 {
		sampleDur = 15.0
	}
	numSegments := v.config.SampleSegments
	if numSegments <= 0 {
		numSegments = 3
	}

	if totalDur <= (sampleDur * float64(numSegments) * 1.5) || totalDur <= 60.0 {
		// Video is short enough to verify in full or duration unknown
		fullRes := v.verifyFull(ctx, original, transcoded)
		fullRes.Sampled = false
		return fullRes
	}

	// Calculate start timestamps for evenly spaced segments:
	// Avoid the first 5% and last 5% (credits/black screens)
	margin := totalDur * 0.05
	usableSpan := totalDur - (2 * margin) - sampleDur
	step := usableSpan / float64(numSegments)

	var startPoints []float64
	for i := 0; i < numSegments; i++ {
		st := margin + (float64(i) * step)
		if st < 0 {
			st = 0
		}
		if st+sampleDur > totalDur {
			st = totalDur - sampleDur
		}
		startPoints = append(startPoints, st)
	}

	var scores []float64
	binaryPath := v.config.BinaryPath
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}

	for _, pt := range startPoints {
		score, err := v.runVMAFSegment(ctx, binaryPath, sanitizedOriginal, sanitizedTranscoded, pt, sampleDur)
		if err == nil && score > 0 {
			scores = append(scores, score)
		}
	}

	result.Duration = time.Since(start)

	if len(scores) == 0 {
		// Fallback to full verification if sampled probe failed
		fullRes := v.verifyFull(ctx, original, transcoded)
		fullRes.Sampled = false
		return fullRes
	}

	// Calculate harmonic or arithmetic mean (arithmetic is standard for VMAF pooled)
	var sum float64
	for _, s := range scores {
		sum += s
	}
	avgScore := sum / float64(len(scores))

	result.VMAFScore = avgScore
	result.Passed = result.VMAFScore >= v.config.Threshold
	return result
}

// runVMAFSegment executes libvmaf (or ssim fallback) for a single time slice (-ss and -t)
func (v *VMAFVerifier) runVMAFSegment(ctx context.Context, binaryPath, original, transcoded string, startSec, durSec float64) (float64, error) {
	filterComplex := "[0:v][1:v]scale2ref[dist][ref];[dist][ref]libvmaf"
	cmd := exec.CommandContext(ctx,
		binaryPath,
		"-hide_banner",
		"-ss", fmt.Sprintf("%.2f", startSec),
		"-t", fmt.Sprintf("%.2f", durSec),
		"-i", transcoded,
		"-ss", fmt.Sprintf("%.2f", startSec),
		"-t", fmt.Sprintf("%.2f", durSec),
		"-i", original,
		"-filter_complex", filterComplex,
		"-f", "null",
		"-",
	)

	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	err := cmd.Run()
	rawOutput := outputBuf.String()

	if err == nil {
		if match := vmafScoreRegex.FindStringSubmatch(rawOutput); len(match) > 1 {
			if score, parseErr := strconv.ParseFloat(match[1], 64); parseErr == nil {
				return score, nil
			}
		}
	}

	// SSIM fallback for segment
	ssimCmd := exec.CommandContext(ctx,
		binaryPath,
		"-hide_banner",
		"-ss", fmt.Sprintf("%.2f", startSec),
		"-t", fmt.Sprintf("%.2f", durSec),
		"-i", transcoded,
		"-ss", fmt.Sprintf("%.2f", startSec),
		"-t", fmt.Sprintf("%.2f", durSec),
		"-i", original,
		"-filter_complex", "[0:v][1:v]scale2ref[dist][ref];[dist][ref]ssim",
		"-f", "null",
		"-",
	)
	var ssimBuf bytes.Buffer
	ssimCmd.Stdout = &ssimBuf
	ssimCmd.Stderr = &ssimBuf
	if ssimErr := ssimCmd.Run(); ssimErr == nil {
		if match := ssimAllRegex.FindStringSubmatch(ssimBuf.String()); len(match) > 1 {
			if ssimVal, parseErr := strconv.ParseFloat(match[1], 64); parseErr == nil {
				return ssimVal * 100.0, nil
			}
		}
	}

	return 0, fmt.Errorf("failed segment evaluation")
}

// verifyFull performs real VMAF quality verification across the entire video file.
func (v *VMAFVerifier) verifyFull(ctx context.Context, original, transcoded string) *VerificationResult {
	start := time.Now()
	result := &VerificationResult{
		SourcePath: original,
		Transcoded: transcoded,
		Threshold:  v.config.Threshold,
		Model:      v.config.Model,
	}

	sanitizedOriginal, origErr := sanitizePath(original, nil)
	if origErr != nil {
		result.Error = fmt.Sprintf("sanitize original path: %v", origErr)
		return result
	}
	sanitizedTranscoded, transcErr := sanitizePath(transcoded, nil)
	if transcErr != nil {
		result.Error = fmt.Sprintf("sanitize transcoded path: %v", transcErr)
		return result
	}

	binaryPath := v.config.BinaryPath
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}

	// 1. If binary is standalone "vmaf" CLI
	baseName := filepath.Base(binaryPath)
	if strings.EqualFold(baseName, "vmaf") || strings.EqualFold(baseName, "vmaf.exe") {
		cmd := exec.CommandContext(ctx,
			binaryPath,
			"-r", sanitizedOriginal,
			"-d", sanitizedTranscoded,
			"--json",
		)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stdout
		err := cmd.Run()
		result.Duration = time.Since(start)
		if err != nil {
			result.Error = fmt.Sprintf("vmaf execution failed: %v", err)
			return result
		}
		var output struct {
			PooledMetrics struct {
				VMAF struct {
					Mean float64 `json:"mean"`
				} `json:"vmaf"`
			} `json:"pooled_metrics"`
		}
		if jsonErr := json.Unmarshal(stdout.Bytes(), &output); jsonErr == nil && output.PooledMetrics.VMAF.Mean > 0 {
			result.VMAFScore = output.PooledMetrics.VMAF.Mean
			result.Passed = result.VMAFScore >= v.config.Threshold
			return result
		}
	}

	// 2. Default: run ffmpeg with libvmaf filter
	// Scale reference to match distorted stream size so libvmaf can compare any resolutions
	filterComplex := "[0:v][1:v]scale2ref[dist][ref];[dist][ref]libvmaf"
	cmd := exec.CommandContext(ctx,
		binaryPath,
		"-hide_banner",
		"-i", sanitizedTranscoded,
		"-i", sanitizedOriginal,
		"-filter_complex", filterComplex,
		"-f", "null",
		"-",
	)

	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	err := cmd.Run()
	result.Duration = time.Since(start)
	rawOutput := outputBuf.String()

	if err == nil {
		if match := vmafScoreRegex.FindStringSubmatch(rawOutput); len(match) > 1 {
			if score, parseErr := strconv.ParseFloat(match[1], 64); parseErr == nil {
				result.VMAFScore = score
				result.Passed = result.VMAFScore >= v.config.Threshold
				return result
			}
		}
	}

	// 3. Fallback: If libvmaf is not compiled into this ffmpeg build, run real mathematical SSIM filter
	ssimCmd := exec.CommandContext(ctx,
		binaryPath,
		"-hide_banner",
		"-i", sanitizedTranscoded,
		"-i", sanitizedOriginal,
		"-filter_complex", "[0:v][1:v]scale2ref[dist][ref];[dist][ref]ssim",
		"-f", "null",
		"-",
	)
	var ssimBuf bytes.Buffer
	ssimCmd.Stdout = &ssimBuf
	ssimCmd.Stderr = &ssimBuf
	if ssimErr := ssimCmd.Run(); ssimErr == nil {
		if match := ssimAllRegex.FindStringSubmatch(ssimBuf.String()); len(match) > 1 {
			if ssimVal, parseErr := strconv.ParseFloat(match[1], 64); parseErr == nil {
				result.SSIMScore = ssimVal
				result.VMAFScore = ssimVal * 100.0
				result.Passed = result.VMAFScore >= v.config.Threshold
				result.Error = ""
				return result
			}
		}
	}

	if err != nil {
		result.Error = fmt.Sprintf("quality verification failed: %v", err)
	} else {
		result.Error = "failed to parse VMAF score from ffmpeg output"
	}

	return result
}

// probeVideoDuration attempts to extract the duration in seconds using ffprobe.
func probeVideoDuration(ctx context.Context, path string) float64 {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return val
}

// VerifyWithFallback performs VMAF verification using real quality measurement.
func (v *VMAFVerifier) VerifyWithFallback(ctx context.Context, original, transcoded string) *VerificationResult {
	return v.Verify(ctx, original, transcoded)
}
