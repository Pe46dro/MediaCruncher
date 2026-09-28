// Package evaluation implements the media file analysis and decision pipeline
// for MediaCruncher.
//
// The pipeline evaluates each media file through four stages:
//
//	1. ProbeFile (ffprobe) — Extract raw stream metadata:
//	   container format, duration, video/audio/subtitle codecs,
//	   resolution, bitrates, HDR format, frame rate, language tags
//
//	2. Normalize — Convert ffprobe output into a unified NormalizedMetadata
//	   struct. Resolves codec aliases (e.g. "h264"/"avc1" → "h264"),
//	   computes human-readable resolution tags (1080p, 4K),
//	   and aggregates audio/subtitle track lists.
//
//	3. RuleEngine.Evaluate — Match against configurable rules:
//	   Each rule has a name, priority, conditions (codec, resolution,
//	   bitrate, HDR), and action (transcode, copy, skip, review).
//	   Rules are sorted by priority descending; first match wins.
//	   Conditions use AND logic within a rule, OR logic between
//	   comma-separated values (codec: "h264,av1" matches either).
//
//	4. SynthesizeStreamPlan — Build the FFmpeg command arguments:
//	   - Stream maps (-map) based on source layout and selected actions
//	   - Audio codec arguments (a stereo AAC downgrade, copy for passthrough,
//	     opus for web targets)
//	   - Subtitle handling (burn-in, copy, or drop)
//	   - Video filter chain (scale, fps, HDR tone-map)
//	   - Preset-specific extra arguments
//
// Rule matching condition keys:
//	codec:        video codec (h264, hevc, av1, vp9)
//	min_resolution: minimum resolution tag (720p, 1080p, 4K)
//	max_resolution: maximum resolution tag (upscale guard)
//	min_bitrate:  minimum bitrate threshold
//	max_bitrate:  maximum bitrate threshold (high-bitrate skip)
//	hdr:          HDR presence (true/false)
//	duration:     duration range (e.g. ">3600" for >1h)
package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mediacruncher/internal/config"
)

type DecisionRecord struct {
	FilePath       string              `json:"file_path"`
	Metadata       *NormalizedMetadata `json:"metadata"`
	Decision       MatchedDecision     `json:"decision"`
	StreamPlan     StreamPlan          `json:"stream_plan"`
	NormalizedJSON string              `json:"normalized_json"`
	StreamMapJSON  string              `json:"stream_map_json"`
	Duration       time.Duration       `json:"analysis_duration"`
}

type Pipeline struct {
	cfg        config.EvaluationConfig
	ruleEngine *RuleEngine
}

func NewPipeline(cfg config.EvaluationConfig) *Pipeline {
	return &Pipeline{
		cfg:        cfg,
		ruleEngine: NewRuleEngine(cfg),
	}
}

// EvaluateFile performs the end-to-end evaluation lifecycle on a media file.
func (p *Pipeline) EvaluateFile(ctx context.Context, filePath string) (*DecisionRecord, error) {
	start := time.Now()

	// 1. ffprobe analysis
	raw, err := ProbeFile(ctx, filePath, 45*time.Second)
	if err != nil {
		return nil, fmt.Errorf("evaluation probe failed: %w", err)
	}

	// 2. Normalization
	meta := Normalize(raw, filePath, p.cfg.CodecAliases)

	// 3. Rule matching
	decision := p.ruleEngine.Evaluate(meta)

	// 4. Multi-stream preservation synthesis
	streamPlan := SynthesizeStreamPlan(meta, decision, p.cfg.AudioPreservation)

	normBytes, _ := json.Marshal(meta)
	planBytes, _ := json.Marshal(streamPlan)

	return &DecisionRecord{
		FilePath:       filePath,
		Metadata:       meta,
		Decision:       decision,
		StreamPlan:     streamPlan,
		NormalizedJSON: string(normBytes),
		StreamMapJSON:  string(planBytes),
		Duration:       time.Since(start),
	}, nil
}
