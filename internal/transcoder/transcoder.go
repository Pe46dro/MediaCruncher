package transcoder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
	"mediacruncher/internal/proc"
)

var (
	frameRegex   = regexp.MustCompile(`frame=\s*(\d+)`)
	fpsRegex     = regexp.MustCompile(`fps=\s*([\d\.]+)`)
	timeRegex    = regexp.MustCompile(`time=\s*(\d+):(\d+):([\d\.]+)`)
	speedRegex   = regexp.MustCompile(`speed=\s*([\d\.]+)x`)
	bitrateRegex = regexp.MustCompile(`bitrate=\s*([\d\.]+k?bits/s)`)
)

type TranscodeProgress struct {
	JobID       string  `json:"job_id"`
	Frames      int64   `json:"frames"`
	FPS         float64 `json:"fps"`
	Speed       float64 `json:"speed"`
	CurrentSec  float64 `json:"current_sec"`
	TotalSec    float64 `json:"total_sec"`
	Percentage  float64 `json:"percentage"`
	Bitrate     string  `json:"bitrate"`
}

type TranscodeJob struct {
	ID         string
	SourcePath string
	DestPath   string
	Plan       evaluation.StreamPlan
	Preset     config.PresetConfig
	Duration   float64
	SourceSize int64
}

type TranscodeResult struct {
	JobID              string  `json:"job_id"`
	SourcePath         string  `json:"source_path"`
	DestPath           string  `json:"dest_path"`
	EncoderUsed        string  `json:"encoder_used"`
	IsHardware         bool    `json:"is_hardware"`
	OriginalSize       int64   `json:"original_size"`
	NewSize            int64   `json:"new_size"`
	SavedBytes         int64   `json:"saved_bytes"`
	CompressionRatio   float64 `json:"compression_ratio"`
	DurationSec        float64 `json:"duration_sec"`
	VMAFScore          float64 `json:"vmaf_score"`
	SSIMScore          float64 `json:"ssim_score"`
	Verified           bool    `json:"verified"`
	VerificationReason string  `json:"verification_reason"`
	SkippedSizeGrowth  bool    `json:"skipped_size_growth"`
	FailedQualityCheck bool    `json:"failed_quality_check"`
}

type Transcoder struct {
	cfg            config.TranscoderConfig
	hwProfile      *HardwareProfile
	onProgress     func(TranscodeProgress)
	onVMAFProgress func(jobID string, current, total int)
}

func NewTranscoder(cfg config.TranscoderConfig, onProgress func(TranscodeProgress)) *Transcoder {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hw := DetectHardwareCapabilities(ctx)

	return &Transcoder{
		cfg:        cfg,
		hwProfile:  hw,
		onProgress: onProgress,
	}
}

// SetVMAFProgressCallback sets the callback for VMAF segment verification progress.
func (t *Transcoder) SetVMAFProgressCallback(fn func(jobID string, current, total int)) {
	t.onVMAFProgress = fn
}

// GetConfig returns the current transcoder configuration.
func (t *Transcoder) GetConfig() config.TranscoderConfig {
	return t.cfg
}

// SelectPreset returns the configured preset by name, or a default fallback.
func (t *Transcoder) SelectPreset(name string) config.PresetConfig {
	for _, p := range t.cfg.Presets {
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	if len(t.cfg.Presets) > 0 {
		return t.cfg.Presets[0]
	}
	return config.PresetConfig{
		Name:        "balanced-hevc",
		VideoCodec:  "hevc",
		QualityCRF:  22,
		PresetSpeed: "medium",
		AudioCodec:  "copy",
	}
}

// Execute performs end-to-end transcoding of a job with staging, quality verification, and atomic promotion.
func (t *Transcoder) Execute(ctx context.Context, job *TranscodeJob) (*TranscodeResult, error) {
	start := time.Now()

	// Ensure staging directory exists
	stagingDir := t.cfg.StagingDir
	if stagingDir == "" {
		stagingDir = filepath.Join(os.TempDir(), "mediacruncher_staging")
	}
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create staging directory %s: %w", stagingDir, err)
	}

	ext := filepath.Ext(job.DestPath)
	if ext == "" {
		ext = ".mp4"
	}
	stagedPath := filepath.Join(stagingDir, fmt.Sprintf("%s_%d%s", job.ID, time.Now().UnixNano(), ext))
	defer os.Remove(stagedPath) // Clean up staging file if promotion fails or aborts

	// Negotiate hardware encoder
	encoder, isHW := t.hwProfile.SelectEncoder(job.Preset.VideoCodec, t.cfg.HardwareAcceleration)

	// Build FFmpeg argument vector
	args := []string{
		"-hide_banner",
		"-y",
	}

	if isHW && strings.Contains(encoder, "vaapi") {
		renderDevice := "/dev/dri/renderD128"
		if _, err := os.Stat(renderDevice); err == nil {
			args = append(args, "-init_hw_device", "vaapi=va:"+renderDevice, "-filter_hw_device", "va")
		} else {
			args = append(args, "-init_hw_device", "vaapi=va", "-filter_hw_device", "va")
		}
	}

	args = append(args, "-i", job.SourcePath)

	// Apply stream mapping
	if len(job.Plan.MapArgs) > 0 {
		args = append(args, job.Plan.MapArgs...)
	} else {
		args = append(args, "-map", "0")
	}

	// Configure Video Encoder
	args = append(args, "-c:v", encoder)
	if isHW && strings.Contains(encoder, "vaapi") {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	crf := job.Preset.QualityCRF
	if crf <= 0 {
		crf = 22
	}
	speed := job.Preset.PresetSpeed
	if speed == "" {
		speed = "medium"
	}

	if isHW {
		if strings.Contains(encoder, "nvenc") {
			args = append(args, "-cq", strconv.Itoa(crf), "-preset", "p5", "-spatial-aq", "1", "-temporal-aq", "1")
		} else if strings.Contains(encoder, "qsv") {
			args = append(args, "-global_quality", strconv.Itoa(crf), "-preset", speed)
		} else if strings.Contains(encoder, "amf") {
			args = append(args, "-rc", "cqp", "-qp_p", strconv.Itoa(crf), "-qp_i", strconv.Itoa(crf))
		} else {
			args = append(args, "-crf", strconv.Itoa(crf))
		}
	} else {
		if strings.Contains(encoder, "libsvtav1") {
			args = append(args, "-crf", strconv.Itoa(crf), "-preset", "6", "-svtav1-params", "tune=0")
		} else {
			args = append(args, "-crf", strconv.Itoa(crf), "-preset", speed)
		}
	}

	// Configure Audio & Subtitles from StreamPlan
	if len(job.Plan.AudioCodecArgs) > 0 {
		args = append(args, job.Plan.AudioCodecArgs...)
	} else {
		args = append(args, "-c:a", "copy")
	}

	if len(job.Plan.SubtitleArgs) > 0 {
		args = append(args, job.Plan.SubtitleArgs...)
	} else {
		args = append(args, "-c:s", "copy")
	}

	// Optional extra flags from preset
	if job.Preset.ExtraFFmpeg != "" {
		extraParts := strings.Fields(job.Preset.ExtraFFmpeg)
		args = append(args, extraParts...)
	}

	// Muxing queue limit to prevent frame dropping in multi-stream containers
	args = append(args, "-max_muxing_queue_size", "1024", stagedPath)

	// Execute ffmpeg under OS supervisor with progress streaming
	progressCallback := func(line string) {
		if t.onProgress == nil {
			return
		}
		prog := parseProgressLine(line, job.ID, job.Duration)
		if prog != nil {
			t.onProgress(*prog)
		}
	}

	// Apply job max duration if configured
	jobCtx := ctx
	if t.cfg.MaxJobDuration > 0 {
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithTimeout(ctx, t.cfg.MaxJobDuration)
		defer cancel()
	}

	_, _, err := proc.RunCommandWithProgress(jobCtx, progressCallback, "ffmpeg", args...)
	if err != nil {
		return nil, fmt.Errorf("transcode execution failed: %w", err)
	}

	// Verify output exists and has content
	stagedInfo, err := os.Stat(stagedPath)
	if err != nil || stagedInfo.Size() == 0 {
		return nil, fmt.Errorf("transcode output is empty or was not created")
	}
	newSize := stagedInfo.Size()

	// Check size growth: if transcode grew significantly and original was already compressed
	if t.cfg.SkipIfLarger && job.SourceSize > 0 && newSize >= job.SourceSize {
		// Log size growth
		res := &TranscodeResult{
			JobID:             job.ID,
			SourcePath:        job.SourcePath,
			DestPath:          job.DestPath,
			EncoderUsed:       encoder,
			IsHardware:        isHW,
			OriginalSize:      job.SourceSize,
			NewSize:           newSize,
			CompressionRatio:  float64(newSize) / float64(job.SourceSize),
			SavedBytes:        job.SourceSize - newSize,
			DurationSec:       time.Since(start).Seconds(),
			SkippedSizeGrowth: true,
			Verified:          false,
			VerificationReason: fmt.Sprintf("output file size (%d bytes) larger than source (%d bytes)", newSize, job.SourceSize),
		}
		return res, nil
	}

	// Run Quality Verification (VMAF / SSIM) if enabled
	vmafScore := 0.0
	ssimScore := 0.0
	verified := true
	reason := "quality verification skipped or passed"

	if t.cfg.VMAFEnabled && job.Duration > 5.0 {
		var vmafCb func(int, int)
		if t.onVMAFProgress != nil {
			vmafCb = func(curr, tot int) {
				t.onVMAFProgress(job.ID, curr, tot)
			}
		}
		verifyRes, err := RunQualityVerification(
			ctx,
			job.SourcePath,
			stagedPath,
			job.Duration,
			t.cfg.VMAFThreshold,
			t.cfg.VMAFSampleCount,
			t.cfg.VMAFSampleDuration,
			vmafCb,
		)
		if ctx.Err() != nil {
			_ = os.Remove(stagedPath)
			return nil, ctx.Err()
		}
		if err != nil {
			reason = fmt.Sprintf("quality verification error: %v", err)
			verified = false
		} else if verifyRes != nil {
			vmafScore = verifyRes.AverageVMAF
			ssimScore = verifyRes.SSIMScore
			verified = verifyRes.Passed
			reason = verifyRes.Reason
		}

		if !verified {
			_ = os.Remove(stagedPath)
			return &TranscodeResult{
				JobID:              job.ID,
				SourcePath:         job.SourcePath,
				DestPath:           job.DestPath,
				EncoderUsed:        encoder,
				IsHardware:         isHW,
				OriginalSize:       job.SourceSize,
				NewSize:            newSize,
				SavedBytes:         job.SourceSize - newSize,
				DurationSec:        time.Since(start).Seconds(),
				VMAFScore:          vmafScore,
				SSIMScore:          ssimScore,
				Verified:           false,
				VerificationReason: reason,
				FailedQualityCheck: true,
			}, nil
		}
	}

	if ctx.Err() != nil {
		_ = os.Remove(stagedPath)
		return nil, ctx.Err()
	}

	// Atomic promotion to final destination
	inPlace := (job.SourcePath == job.DestPath)
	if err := PromoteFile(stagedPath, job.DestPath, inPlace); err != nil {
		_ = os.Remove(stagedPath)
		return nil, fmt.Errorf("failed to promote transcoded file to destination: %w", err)
	}

	ratio := 0.0
	if job.SourceSize > 0 {
		ratio = float64(newSize) / float64(job.SourceSize)
	}

	return &TranscodeResult{
		JobID:              job.ID,
		SourcePath:         job.SourcePath,
		DestPath:           job.DestPath,
		EncoderUsed:        encoder,
		IsHardware:         isHW,
		OriginalSize:       job.SourceSize,
		NewSize:            newSize,
		SavedBytes:         job.SourceSize - newSize,
		CompressionRatio:   ratio,
		DurationSec:        time.Since(start).Seconds(),
		VMAFScore:          vmafScore,
		SSIMScore:          ssimScore,
		Verified:           verified,
		VerificationReason: reason,
		SkippedSizeGrowth:  false,
	}, nil
}

func parseProgressLine(line string, jobID string, totalDuration float64) *TranscodeProgress {
	if !strings.Contains(line, "frame=") && !strings.Contains(line, "time=") {
		return nil
	}

	prog := &TranscodeProgress{
		JobID:    jobID,
		TotalSec: totalDuration,
	}

	if m := frameRegex.FindStringSubmatch(line); len(m) >= 2 {
		prog.Frames, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := fpsRegex.FindStringSubmatch(line); len(m) >= 2 {
		prog.FPS, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := speedRegex.FindStringSubmatch(line); len(m) >= 2 {
		prog.Speed, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := bitrateRegex.FindStringSubmatch(line); len(m) >= 2 {
		prog.Bitrate = m[1]
	}
	if m := timeRegex.FindStringSubmatch(line); len(m) >= 4 {
		h, _ := strconv.ParseFloat(m[1], 64)
		min, _ := strconv.ParseFloat(m[2], 64)
		sec, _ := strconv.ParseFloat(m[3], 64)
		curSec := h*3600 + min*60 + sec
		prog.CurrentSec = curSec
		if totalDuration > 0 {
			pct := (curSec / totalDuration) * 100.0
			if pct > 100.0 {
				pct = 100.0
			}
			prog.Percentage = pct
		}
	}

	return prog
}
