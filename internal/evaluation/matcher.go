package evaluation

import "fmt"

// MatchResult holds the outcome of a rule matching evaluation.
type MatchResult struct {
	MatchedRule      *Rule
	Specificity      int
	MatchedConditions int
}

// Matcher evaluates normalized metadata against a rule set.
type Matcher struct {
	ruleSet *RuleSet
}

// NewMatcher creates a new rule matcher with the given rule set.
func NewMatcher(ruleSet *RuleSet) *Matcher {
	return &Matcher{
		ruleSet: ruleSet,
	}
}

// Match evaluates the normalized metadata against all rules and returns the best match.
func (m *Matcher) Match(metadata *NormalizedMetadata) (*MatchResult, *Rule) {
	if metadata == nil {
		return nil, nil
	}

	if m.ruleSet == nil {
		return nil, nil
	}

	var bestMatch *MatchResult

	for i := range m.ruleSet.Rules {
		rule := &m.ruleSet.Rules[i]
		if !rule.Enabled {
			continue
		}

		match, specific, conditions := m.evaluateRule(metadata, rule)
		if match {
			if bestMatch == nil || rule.Priority > bestMatch.MatchedRule.Priority || (rule.Priority == bestMatch.MatchedRule.Priority && specific > bestMatch.Specificity) {
				bestMatch = &MatchResult{
					MatchedRule:       rule,
					Specificity:       specific,
					MatchedConditions: conditions,
				}
			}
		}
	}

	if bestMatch != nil {
		return bestMatch, bestMatch.MatchedRule
	}

	defaultAction := m.ruleSet.DefaultAction
	if defaultAction == "" {
		defaultAction = ActionTranscode
	}

	defaultRule := &Rule{
		Name:   "default",
		Action: defaultAction,
		Preset: "default",
	}

	return &MatchResult{
		MatchedRule:       defaultRule,
		Specificity:       0,
		MatchedConditions: 0,
	}, defaultRule
}

// evaluateRule checks if all conditions in a rule match the given metadata.
func (m *Matcher) evaluateRule(metadata *NormalizedMetadata, rule *Rule) (bool, int, int) {
	conditionMap := buildConditionMap(metadata)
	var matched int
	var specific int

	for _, cond := range rule.Conditions {
		actual, ok := conditionMap[cond.Field]
		if !ok {
			if cond.Operator == "exists" {
				if CompareValue(nil, cond.Operator, cond.Value) {
					matched++
					specific++
				}
			}
			return false, specific, matched
		}

		if CompareValue(actual, cond.Operator, cond.Value) {
			matched++
			specific++
		} else {
			return false, specific, matched
		}
	}

	return matched > 0, specific, matched
}

// buildConditionMap creates a flat map of normalized metadata fields for condition matching.
func buildConditionMap(metadata *NormalizedMetadata) map[string]interface{} {
	m := make(map[string]interface{}, 16)
	m["video_codec"] = metadata.VideoCodec
	m["audio_codec"] = metadata.AudioCodec
	m["resolution"] = metadata.Resolution
	m["width"] = metadata.Width
	m["height"] = metadata.Height
	m["frame_rate"] = metadata.FrameRate
	m["bit_rate"] = metadata.BitRate
	m["duration"] = metadata.Duration
	m["color_space"] = metadata.ColorSpace
	m["is_hdr"] = metadata.IsHDR
	m["high_bit_depth"] = metadata.HighBitDepth
	m["container_format"] = metadata.ContainerFormat
	m["stream_count"] = metadata.StreamCount
	m["has_subtitles"] = metadata.HasSubtitles

	languages := metadata.Languages
	if languages == nil {
		languages = []string{}
	}
	m["languages"] = languages

	return m
}

// CountRuleMatches returns how many rules in the set match the given metadata.
func (m *Matcher) CountRuleMatches(metadata *NormalizedMetadata) int {
	if metadata == nil || m.ruleSet == nil {
		return 0
	}
	count := 0
	for _, rule := range m.ruleSet.Rules {
		if !rule.Enabled {
			continue
		}
		match, _, _ := m.evaluateRule(metadata, &rule)
		if match {
			count++
		}
	}
	return count
}

// GetRuleCount returns the total number of rules in the rule set.
func (m *Matcher) GetRuleCount() int {
	if m.ruleSet == nil {
		return 0
	}
	return len(m.ruleSet.Rules)
}

// ValidateRules validates all rules and returns a list of error messages.
func (m *Matcher) ValidateRules() []string {
	if m.ruleSet == nil {
		return []string{"no rule set loaded"}
	}
	var errors []string
	for _, rule := range m.ruleSet.Rules {
		if !rule.Enabled {
			continue
		}
		for j, cond := range rule.Conditions {
			if !IsValidOperator(cond.Operator) {
				errors = append(errors, fmt.Sprintf("rule %q condition %d: invalid operator %q", rule.Name, j, cond.Operator))
			}
		}
	}
	return errors
}
