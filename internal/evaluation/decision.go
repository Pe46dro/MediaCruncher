package evaluation

import (
	"fmt"
	"time"
)

// DecisionAction is the action chosen by the evaluation pipeline.
type DecisionAction string

const (
	DecisionTranscode DecisionAction = "transcode"
	DecisionStreamCopy DecisionAction = "stream_copy"
	DecisionSkip      DecisionAction = "skip"
	DecisionReview    DecisionAction = "review"
)

// DecisionRecord is the final output of the evaluation pipeline for a file.
type DecisionRecord struct {
	FilePath        string            `json:"file_path"`
	RuleID          string            `json:"rule_id"`
	Action          DecisionAction    `json:"action"`
	Preset          string            `json:"preset"`
	Flags           []string          `json:"flags"`
	Normalized      *NormalizedMetadata `json:"normalized_metadata"`
	EvaluationTime  time.Duration     `json:"evaluation_time"`
	Timestamp       time.Time         `json:"timestamp"`
	QueueEntryID    int64             `json:"queue_entry_id,omitempty"`
}

// BuildDecision creates a DecisionRecord from a rule match and normalized metadata.
func BuildDecision(filePath string, matchedRule *Rule, metadata *NormalizedMetadata, queueEntryID int64, evaluationTime time.Duration) *DecisionRecord {
	record := &DecisionRecord{
		FilePath:       filePath,
		RuleID:         matchedRule.Name,
		Action:         DecisionAction(matchedRule.Action),
		Preset:         matchedRule.Preset,
		Normalized:     metadata,
		EvaluationTime: evaluationTime,
		Timestamp:      time.Now(),
		QueueEntryID:   queueEntryID,
	}

	if matchedRule.Name == "default" {
		record.Flags = append(record.Flags, "default_rule")
	}

	if metadata != nil {
		for _, flag := range metadata.AnomalyFlags {
			if flag != "" {
				record.Flags = append(record.Flags, fmt.Sprintf("anomaly:%s", flag))
			}
		}
	}

	return record
}

// ToDecisionString converts the decision action to a persistence-compatible string.
func (d *DecisionRecord) ToDecisionString() string {
	switch d.Action {
	case DecisionTranscode:
		return "transcode"
	case DecisionStreamCopy:
		return "stream_copy"
	case DecisionSkip:
		return "skip"
	case DecisionReview:
		return "review"
	default:
		return "review"
	}
}

// NeedsTranscode returns true if the decision is to transcode the file.
func (d *DecisionRecord) NeedsTranscode() bool {
	return d.Action == DecisionTranscode
}

// NeedsReview returns true if the file needs manual review.
func (d *DecisionRecord) NeedsReview() bool {
	return d.Action == DecisionReview
}

// IsSkip returns true if the file should be skipped.
func (d *DecisionRecord) IsSkip() bool {
	return d.Action == DecisionSkip
}

// IsStreamCopy returns true if the file should be stream copied.
func (d *DecisionRecord) IsStreamCopy() bool {
	return d.Action == DecisionStreamCopy
}
