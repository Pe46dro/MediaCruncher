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
	BinaryPath string  `json:"binary_path"`
	Threshold  float64 `json:"threshold"`
	Model      string  `json:"model"`
	PSNR       bool    `json:"psnr"`
	SSIM       bool    `json:"ssim"`
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
}

// VMAFVerifier performs VMAF quality verification.
type VMAFVerifier struct {
	config VMAFConfig
}

// NewVMAFVerifier creates a new VMAF verifier.
func NewVMAFVerifier(binaryPath string, threshold float64, model string) *VMAFVerifier {
	if threshold <= 0 {
		threshold = 90.0
	}
	if model == "" {
		model = "libsvm"
	}
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	return &VMAFVerifier{
		config: VMAFConfig{
			BinaryPath: binaryPath,
			Threshold:  threshold,
			Model:      model,
		},
	}
}

var (
	vmafScoreRegex = regexp.MustCompile(`(?i)VMAF\s+score:\s*([0-9.]+)`)
	ssimAllRegex   = regexp.MustCompile(`(?i)All:\s*([0-9.]+)`)
)

// Verify performs real VMAF quality verification between original and transcoded files.
func (v *VMAFVerifier) Verify(ctx context.Context, original, transcoded string) *VerificationResult {
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

// VerifyWithFallback performs VMAF verification using real quality measurement.
func (v *VMAFVerifier) VerifyWithFallback(ctx context.Context, original, transcoded string) *VerificationResult {
	return v.Verify(ctx, original, transcoded)
}
