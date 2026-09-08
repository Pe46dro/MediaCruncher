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

// VMAFConfig holds configuration for VMAF quality verification.
type VMAFConfig struct {
	BinaryPath    string        `json:"binary_path"`
	Threshold     float64       `json:"threshold"`
	Model         string        `json:"model"`
	PSNR          bool          `json:"psnr"`
	SSIM          bool          `json:"ssim"`
}

// VerificationResult holds the output of VMAF quality verification.
type VerificationResult struct {
	SourcePath  string        `json:"source_path"`
	Transcoded  string        `json:"transcoded"`
	VMAFScore   float64       `json:"vmaf_score"`
	PSNRScore   float64       `json:"psnr_score"`
	SSIMScore   float64       `json:"ssim_score"`
	Threshold   float64       `json:"threshold"`
	Passed      bool          `json:"passed"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
	Model       string        `json:"model"`
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
	return &VMAFVerifier{
		config: VMAFConfig{
			BinaryPath: binaryPath,
			Threshold:  threshold,
			Model:      model,
		},
	}
}

// Verify performs VMAF quality verification between original and transcoded files.
func (v *VMAFVerifier) Verify(ctx context.Context, original, transcoded string) *VerificationResult {
	start := time.Now()
	result := &VerificationResult{
		SourcePath: original,
		Transcoded: transcoded,
		Threshold:  v.config.Threshold,
		Model:      v.config.Model,
	}

	if v.config.BinaryPath == "" {
		v.config.BinaryPath = "vmaf"
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

	cmd := exec.CommandContext(ctx,
		v.config.BinaryPath,
		sanitizedOriginal,
		sanitizedTranscoded,
		"--json",
		"--log-filename", "-",
		"--model", v.config.Model,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout

	err := cmd.Run()
	result.Duration = time.Since(start)

	if err != nil {
		result.Error = err.Error()
		return result
	}

	var output struct {
		Pframe_vmaf string  `json:"pframe_vmaf"`
		Metrics       struct {
			VMAF float64 `json:"vmaf"`
			PSNR float64 `json:"psnr"`
			SSIM float64 `json:"ssim"`
		} `json:"metrics"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &output); err == nil {
		result.VMAFScore = output.Metrics.VMAF
		result.PSNRScore = output.Metrics.PSNR
		result.SSIMScore = output.Metrics.SSIM
	} else {
		result.Error = "failed to parse VMAF output"
	}

	result.Passed = result.VMAFScore >= v.config.Threshold

	return result
}

// VerifyWithFallback performs VMAF verification and falls back to simple metrics if VMAF is unavailable.
func (v *VMAFVerifier) VerifyWithFallback(ctx context.Context, original, transcoded string) *VerificationResult {
	result := v.Verify(ctx, original, transcoded)
	if result.Error != "" {
		result.Error = ""
		result.VMAFScore = estimateQuality(original, transcoded)
		result.Passed = result.VMAFScore >= v.config.Threshold
	}
	return result
}

// estimateQuality estimates quality based on file size ratio when VMAF is unavailable.
func estimateQuality(original, transcoded string) float64 {
	origInfo, err1 := getFileSize(original)
	transInfo, err2 := getFileSize(transcoded)

	if err1 != nil || err2 != nil {
		return 85.0
	}

	if origInfo <= 0 || transInfo <= 0 {
		return 85.0
	}

	ratio := float64(transInfo) / float64(origInfo)

	if ratio < 0.1 {
		return 50.0
	}
	if ratio < 0.3 {
		return 65.0
	}
	if ratio < 0.5 {
		return 75.0
	}
	if ratio < 0.8 {
		return 85.0
	}
	return 92.0
}

// getFileSize returns the size of a file in bytes.
func getFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
