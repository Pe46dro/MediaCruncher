package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
)

// Engine is the main evaluation pipeline engine.
type Engine struct {
	probeConfig  ProbeConfig
	ruleSetCache *RuleSetCache
	persistence  persistenceEngine
	logger       *observability.Logger
	config       *config.EvaluationSettings
	mu           sync.RWMutex
}

// persistenceEngine is an interface to the persistence layer.
type persistenceEngine interface {
	UpsertJobMetadata(ctx context.Context, entryID int64, analysisData map[string]interface{}, decision string) (int64, error)
	AppendAuditLog(ctx context.Context, eventType, severity, payload string) error
}

// Config holds configuration for the evaluation engine.
type Config struct {
	ProbeTimeout   time.Duration
	BinaryPath     string
	RuleSetFile    string
	RuleSetCache   *RuleSetCache
	Persistence    persistenceEngine
	Logger         *observability.Logger
	Config         *config.EvaluationSettings
}

// New creates a new evaluation engine.
func New(cfg Config) *Engine {
	e := &Engine{
		probeConfig: ProbeConfig{
			Timeout:    cfg.ProbeTimeout,
			BinaryPath: cfg.BinaryPath,
		},
		ruleSetCache: cfg.RuleSetCache,
		logger:       cfg.Logger,
		config:       cfg.Config,
	}

	if e.probeConfig.Timeout <= 0 {
		e.probeConfig.Timeout = 30 * time.Second
	}

	if e.ruleSetCache == nil {
		e.ruleSetCache = NewRuleSetCache()
	}

	if e.logger == nil {
		e.logger = observability.NewStdLogger(observability.DebugLevel, "evaluation")
	}

	e.persistence = cfg.Persistence

	return e
}

// AnalyzeFile performs the full evaluation pipeline on a single file.
func (e *Engine) AnalyzeFile(ctx context.Context, filePath string, queueEntryID int64) (*DecisionRecord, error) {
	start := time.Now()

	probeResult := Probe(ctx, filePath, e.probeConfig)

	rawData := make(map[string]interface{})
	if probeResult != nil {
		probeJSON, _ := json.Marshal(probeResult)
		_ = json.Unmarshal(probeJSON, &rawData)
	}

	analysisResult := BuildAnalysisResult(filePath, probeResult, rawData)

	normalized := Normalize(analysisResult)

	ruleSet := e.getRuleSet()
	matcher := NewMatcher(ruleSet)
	matchResult, matchedRule := matcher.Match(normalized)

	if matchResult != nil && matchedRule != nil {
		decision := BuildDecision(filePath, matchedRule, normalized, queueEntryID, time.Since(start))

		if e.persistence != nil {
			analysisJSON, _ := json.Marshal(normalized)
			analysisData := map[string]interface{}{
				"analysis": string(analysisJSON),
				"rule_id":  decision.RuleID,
			}
			if _, err := e.persistence.UpsertJobMetadata(ctx, queueEntryID, analysisData, decision.ToDecisionString()); err != nil {
				if e.logger != nil {
					e.logger.Error(fmt.Sprintf("failed to persist decision: %v", err))
				}
			}
		}

		return decision, nil
	}

	decision := BuildDecision(filePath, &Rule{Name: "default", Action: ActionReview}, normalized, queueEntryID, time.Since(start))
	return decision, nil
}

// EvaluateQueueEntry evaluates a file linked to a queue entry.
func (e *Engine) EvaluateQueueEntry(ctx context.Context, filePath string, queueEntryID int64) (*DecisionRecord, error) {
	return e.AnalyzeFile(ctx, filePath, queueEntryID)
}

// SetRuleSet sets the rule set in the cache.
func (e *Engine) SetRuleSet(ruleSet *RuleSet) {
	e.ruleSetCache.Set(ruleSet)
}

// GetRuleSet returns the current rule set.
func (e *Engine) GetRuleSet() *RuleSet {
	return e.ruleSetCache.Get()
}

// getRuleSet returns the rule set, loading from file if necessary.
func (e *Engine) getRuleSet() *RuleSet {
	ruleSet := e.ruleSetCache.Get()

	if len(ruleSet.Rules) == 0 && e.config != nil && e.config.RuleSetFile != "" {
		loaded, err := LoadRuleSet(e.config.RuleSetFile)
		if err != nil {
			if e.logger != nil {
				e.logger.Error(fmt.Sprintf("failed to load rule set: %v", err))
			}
		} else {
			ruleSet = loaded
			e.ruleSetCache.Set(ruleSet)
		}
	}

	if ruleSet.DefaultAction == "" {
		if e.config != nil && e.config.DefaultAction != "" {
			ruleSet.DefaultAction = RuleAction(e.config.DefaultAction)
		} else {
			ruleSet.DefaultAction = ActionTranscode
		}
	}

	return ruleSet
}

// ReloadRuleSet reloads the rule set from the configured file path.
func (e *Engine) ReloadRuleSet() error {
	if e.config == nil || e.config.RuleSetFile == "" {
		return fmt.Errorf("no rule set file configured")
	}

	ruleSet, err := LoadRuleSet(e.config.RuleSetFile)
	if err != nil {
		return fmt.Errorf("reload rule set: %w", err)
	}

	e.ruleSetCache.Set(ruleSet)

	if e.logger != nil {
		e.logger.Info(fmt.Sprintf("rule set reloaded from %s with %d rules", e.config.RuleSetFile, len(ruleSet.Rules)))
	}

	return nil
}

// Validate returns validation errors for the current rule set.
func (e *Engine) Validate() []string {
	ruleSet := e.ruleSetCache.Get()
	matcher := NewMatcher(ruleSet)
	return matcher.ValidateRules()
}
