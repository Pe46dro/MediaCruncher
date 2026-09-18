package evaluation

import (
	"sort"
	"strconv"
	"strings"

	"mediacruncher/internal/config"
)

type MatchedDecision struct {
	MatchedRule    string `json:"matched_rule"`
	Action         string `json:"action"` // transcode, copy, skip, review
	Preset         string `json:"preset"`
	AudioAction    string `json:"audio_action"`
	RetainSubtitles bool   `json:"retain_subtitles"`
}

type RuleEngine struct {
	rules         []config.RuleConfig
	defaultAction string
	defaultPreset string
}

func NewRuleEngine(cfg config.EvaluationConfig) *RuleEngine {
	// Sort rules descending by priority
	sorted := make([]config.RuleConfig, len(cfg.Rules))
	copy(sorted, cfg.Rules)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority > sorted[j].Priority
	})

	defAction := cfg.DefaultAction
	if defAction == "" {
		defAction = "transcode"
	}
	defPreset := cfg.DefaultPreset
	if defPreset == "" {
		defPreset = "balanced-hevc"
	}

	return &RuleEngine{
		rules:         sorted,
		defaultAction: defAction,
		defaultPreset: defPreset,
	}
}

// Evaluate matches metadata against prioritized rules. First match wins.
func (re *RuleEngine) Evaluate(meta *NormalizedMetadata) MatchedDecision {
	for _, rule := range re.rules {
		if ruleMatches(rule, meta) {
			action := rule.Action
			if action == "" {
				action = re.defaultAction
			}
			preset := rule.Preset
			if preset == "" && action == "transcode" {
				preset = re.defaultPreset
			}
			return MatchedDecision{
				MatchedRule:     rule.Name,
				Action:          action,
				Preset:          preset,
				AudioAction:     rule.AudioAction,
				RetainSubtitles: rule.RetainSubtitles,
			}
		}
	}

	return MatchedDecision{
		MatchedRule:     "default",
		Action:          re.defaultAction,
		Preset:          re.defaultPreset,
		AudioAction:     "copy",
		RetainSubtitles: true,
	}
}

func ruleMatches(rule config.RuleConfig, meta *NormalizedMetadata) bool {
	if len(rule.Conditions) == 0 {
		return true
	}

	for k, v := range rule.Conditions {
		switch strings.ToLower(k) {
		case "codec":
			allowed := splitComma(v)
			if !containsMatch(allowed, meta.VideoCodec) {
				return false
			}

		case "resolution":
			allowed := splitComma(v)
			if !containsMatch(allowed, meta.ResolutionTag) {
				return false
			}

		case "is_hdr":
			wantHDR, _ := strconv.ParseBool(v)
			if meta.IsHDR != wantHDR {
				return false
			}

		case "min_bitrate":
			minBps, _ := strconv.ParseInt(v, 10, 64)
			if meta.TotalBitrate < minBps {
				return false
			}

		case "has_surround":
			wantSurround, _ := strconv.ParseBool(v)
			if meta.HasSurround != wantSurround {
				return false
			}
		}
	}

	return true
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	res := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(strings.ToLower(p)); trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return res
}

func containsMatch(list []string, val string) bool {
	val = strings.ToLower(val)
	for _, item := range list {
		if item == val {
			return true
		}
	}
	return false
}
