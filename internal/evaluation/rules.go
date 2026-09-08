package evaluation

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// RuleAction defines the action to take for a matched rule.
type RuleAction string

const (
	ActionTranscode RuleAction = "transcode"
	ActionStreamCopy RuleAction = "stream_copy"
	ActionSkip      RuleAction = "skip"
	ActionReview    RuleAction = "review"
)

// Condition represents a single matching condition on normalized metadata.
type Condition struct {
	Field    string      `json:"field"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value"`
}

// Rule is a user-defined evaluation rule.
type Rule struct {
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions"`
	Action     RuleAction  `json:"action"`
	Preset     string      `json:"preset"`
	Priority   int         `json:"priority"`
	Enabled    bool        `json:"enabled"`
}

// RuleSet is a collection of rules with a default fallback.
type RuleSet struct {
	Name         string  `json:"name"`
	DefaultAction RuleAction `json:"default_action"`
	Rules        []Rule  `json:"rules"`
	Errors       []string `json:"errors,omitempty"`
}

// RuleSetCache holds the cached rule set in memory.
type RuleSetCache struct {
	mu      sync.RWMutex
	ruleSet *RuleSet
}

// NewRuleSetCache creates a new empty rule set cache.
func NewRuleSetCache() *RuleSetCache {
	return &RuleSetCache{
		ruleSet: &RuleSet{
			DefaultAction: ActionTranscode,
		},
	}
}

// LoadRuleSet loads a rule set from a JSON file.
func LoadRuleSet(path string) (*RuleSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rule set file: %w", err)
	}

	var ruleSet RuleSet
	if err := json.Unmarshal(data, &ruleSet); err != nil {
		return nil, fmt.Errorf("parse rule set JSON: %w", err)
	}

	if err := ruleSet.Validate(); err != nil {
		return nil, fmt.Errorf("validate rule set: %w", err)
	}

	return &ruleSet, nil
}

// Set updates the cached rule set.
func (c *RuleSetCache) Set(ruleSet *RuleSet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ruleSet = ruleSet
}

// Get returns a copy of the current cached rule set.
func (c *RuleSetCache) Get() *RuleSet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ruleSet == nil {
		return &RuleSet{
			DefaultAction: ActionTranscode,
		}
	}
	cp := *c.ruleSet
	cp.Rules = make([]Rule, len(c.ruleSet.Rules))
	copy(cp.Rules, c.ruleSet.Rules)
	return &cp
}

// Validate checks the rule set structure and returns errors for malformed rules.
func (rs *RuleSet) Validate() error {
	if rs.Name == "" {
		rs.Name = "default"
	}

	validActions := map[RuleAction]bool{
		ActionTranscode: true,
		ActionStreamCopy: true,
		ActionSkip: true,
		ActionReview: true,
	}

	if rs.DefaultAction != "" && !validActions[rs.DefaultAction] {
		return fmt.Errorf("invalid default action: %s", rs.DefaultAction)
	}

	for i, rule := range rs.Rules {
		if rule.Name == "" {
			rule.Name = fmt.Sprintf("rule_%d", i)
		}
		if !validActions[rule.Action] {
			rs.Errors = append(rs.Errors, fmt.Sprintf("rule %q: invalid action %q", rule.Name, rule.Action))
			rule.Enabled = false
		}
		if len(rule.Conditions) == 0 {
			rs.Errors = append(rs.Errors, fmt.Sprintf("rule %q: no conditions defined", rule.Name))
			rule.Enabled = false
		}
		for j, cond := range rule.Conditions {
			if cond.Field == "" {
				rs.Errors = append(rs.Errors, fmt.Sprintf("rule %q condition %d: missing field", rule.Name, j))
			}
			if cond.Operator == "" {
				rs.Errors = append(rs.Errors, fmt.Sprintf("rule %q condition %d: missing operator", rule.Name, j))
			}
		}
	}

	return nil
}

// ReloadReloads the rule set from file if a path is provided.
func ReloadRuleSet(path string) (*RuleSet, error) {
	return LoadRuleSet(path)
}

// IsValidOperator checks if the given operator is a valid comparison operator.
func IsValidOperator(op string) bool {
	valid := map[string]bool{
		"eq": true, "neq": true,
		"gt": true, "gte": true, "lt": true, "lte": true,
		"contains": true, "startswith": true, "endswith": true,
		"exists": true, "in": true,
	}
	return valid[strings.ToLower(op)]
}

// CompareValue compares a normalized metadata value against a condition value.
func CompareValue(actual interface{}, operator string, expected interface{}) bool {
	switch strings.ToLower(operator) {
	case "eq":
		return fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
	case "neq":
		return fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expected)
	case "gt":
		return toFloat(actual) > toFloat(expected)
	case "gte":
		return toFloat(actual) >= toFloat(expected)
	case "lt":
		return toFloat(actual) < toFloat(expected)
	case "lte":
		return toFloat(actual) <= toFloat(expected)
	case "contains":
		return strings.Contains(fmt.Sprintf("%v", actual), fmt.Sprintf("%v", expected))
	case "startswith":
		return strings.HasPrefix(fmt.Sprintf("%v", actual), fmt.Sprintf("%v", expected))
	case "endswith":
		return strings.HasSuffix(fmt.Sprintf("%v", actual), fmt.Sprintf("%v", expected))
	case "exists":
		return actual != nil && actual != ""
	case "in":
		if arr, ok := expected.([]interface{}); ok {
			for _, v := range arr {
				if fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", v) {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

func toFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	default:
		return 0
	}
}
