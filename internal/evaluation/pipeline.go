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
