package concurrency

import (
	"sync"
	"time"
)

// RetryPolicy defines the retry configuration.
type RetryPolicy struct {
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxRetries    int
	Multiplier    float64
	Jitter        bool
}

// DefaultRetryPolicy returns a standard retry configuration.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		BaseDelay:  5 * time.Second,
		MaxDelay:   5 * time.Minute,
		MaxRetries: 3,
		Multiplier: 2.0,
		Jitter:     true,
	}
}

// RetryRecord tracks retry metadata for a failed task.
type RetryRecord struct {
	TaskID       int64     `json:"task_id"`
	Attempt      int       `json:"attempt"`
	MaxRetries   int       `json:"max_retries"`
	NextRetryAt  time.Time `json:"next_retry_at"`
	LastError    string    `json:"last_error"`
	FailedAt     time.Time `json:"failed_at"`
	Permanent    bool      `json:"permanent"`
	BackoffSteps []int64   `json:"backoff_steps"`
}

// RetryManager handles task retry logic with exponential backoff.
type RetryManager struct {
	mu        sync.Mutex
	records   map[int64]*RetryRecord
	policy    *RetryPolicy
	maxRecords int
}

// NewRetryManager creates a new retry manager.
func NewRetryManager(policy *RetryPolicy, maxRecords int) *RetryManager {
	if policy == nil {
		policy = DefaultRetryPolicy()
	}
	if maxRecords <= 0 {
		maxRecords = 10000
	}
	return &RetryManager{
		records:    make(map[int64]*RetryRecord),
		policy:     policy,
		maxRecords: maxRecords,
	}
}

// RecordFailure records a task failure and computes the next retry time.
func (m *RetryManager) RecordFailure(taskID int64, taskMaxRetries int, errMsg string) *RetryRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, exists := m.records[taskID]
	if !exists {
		record = &RetryRecord{
			TaskID:     taskID,
			MaxRetries: taskMaxRetries,
			FailedAt:   time.Now(),
		}
		m.records[taskID] = record
	}

	record.Attempt++
	record.LastError = errMsg
	record.FailedAt = time.Now()

	if record.Attempt >= record.MaxRetries {
		record.Permanent = true
		return record
	}

	delay := m.computeBackoff(record.Attempt)
	record.NextRetryAt = time.Now().Add(delay)
	record.BackoffSteps = append(record.BackoffSteps, int64(delay))

	return record
}

// GetRecord returns the retry record for a task.
func (m *RetryManager) GetRecord(taskID int64) (*RetryRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, exists := m.records[taskID]
	return record, exists
}

// ClearRecord removes the retry record for a task.
func (m *RetryManager) ClearRecord(taskID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, taskID)
}

// GetPendingRetries returns all records that are ready for retry.
func (m *RetryManager) GetPendingRetries() []*RetryRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	var pending []*RetryRecord
	for _, record := range m.records {
		if !record.Permanent && time.Now().After(record.NextRetryAt) {
			pending = append(pending, record)
		}
	}
	return pending
}

// GetPermanentFailures returns all permanently failed tasks.
func (m *RetryManager) GetPermanentFailures() []*RetryRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	var failures []*RetryRecord
	for _, record := range m.records {
		if record.Permanent {
			failures = append(failures, record)
		}
	}
	return failures
}

// Count returns the number of active retry records.
func (m *RetryManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

// computeBackoff calculates the delay for a given attempt using exponential backoff.
func (m *RetryManager) computeBackoff(attempt int) time.Duration {
	delay := float64(m.policy.BaseDelay)
	for i := 0; i < attempt-1; i++ {
		delay *= m.policy.Multiplier
		if delay > float64(m.policy.MaxDelay) {
			delay = float64(m.policy.MaxDelay)
			break
		}
	}

	if m.policy.Jitter {
		jitter := delay * 0.1
		delay = delay + (delay - jitter)*(float64(attempt%3)-1)/3
	}

	return time.Duration(delay)
}

// ShouldRetry returns true if the task should be retried.
func (m *RetryManager) ShouldRetry(taskID int64, taskMaxRetries int) bool {
	record, exists := m.GetRecord(taskID)
	if !exists {
		return taskMaxRetries > 0
	}
	return !record.Permanent
}

// PruneOldRecords removes records older than the specified duration.
func (m *RetryManager) PruneOldRecords(keepDuration time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-keepDuration)
	pruned := 0
	for taskID, record := range m.records {
		if record.FailedAt.Before(cutoff) {
			delete(m.records, taskID)
			pruned++
		}
	}
	return pruned
}
