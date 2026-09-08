package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// QueueEntry represents a pending transcoding job in the queue.
type QueueEntry struct {
	ID          int64
	SourcePath  string
	State       State
	WorkerID    string
	Priority    Priority
	RetryCount  int
	CreatedAt   time.Time
	ScheduledAt *time.Time
	UpdatedAt   time.Time
}

// Enqueue adds a new job to the queue with initialized state and timestamp.
func (e *Engine) Enqueue(ctx context.Context, sourcePath string, priority Priority) (int64, error) {
	var id int64
	err := e.InTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`INSERT INTO queue_entries (source_path, state, priority, created_at, scheduled_at, updated_at)
			 VALUES (?, ?, ?, datetime('now'), datetime('now'), datetime('now'))`,
			sourcePath, string(StatePending), int(priority),
		)
		if err != nil {
			return fmt.Errorf("insert queue entry: %w", err)
		}
		id, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return 0, err
	}

	e.metrics.queueDepth.Inc()
	return id, nil
}

// Claim takes the highest-priority available job for a given worker,
// atomically transitioning it from pending to processing state.
func (e *Engine) Claim(ctx context.Context, workerID string) (*QueueEntry, error) {
	var entry QueueEntry
	err := e.InTransaction(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT id, source_path, state, worker_id, priority, retry_count,
				 created_at, scheduled_at, updated_at
			 FROM queue_entries
			 WHERE state = ?
			 ORDER BY priority DESC, created_at ASC
			 LIMIT 1`,
			string(StatePending),
		)

		var scheduledAtStr sql.NullString
		if err := row.Scan(&entry.ID, &entry.SourcePath, &entry.State, &entry.WorkerID,
			&entry.Priority, &entry.RetryCount, &entry.CreatedAt, &scheduledAtStr, &entry.UpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return ErrQueueEmpty
			}
			return fmt.Errorf("scan queue entry: %w", err)
		}

		if scheduledAtStr.Valid {
			t, err := time.Parse(time.RFC3339, scheduledAtStr.String)
			if err == nil {
				entry.ScheduledAt = &t
			}
		}

		_, err := tx.ExecContext(ctx,
			`UPDATE queue_entries SET state = ?, worker_id = ?, updated_at = datetime('now') WHERE id = ?`,
			string(StateProcessing), workerID, entry.ID,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	e.metrics.queueDepth.Dec()
	return &entry, nil
}

// Complete transitions a queue entry to completed state with metadata linkage.
func (e *Engine) Complete(ctx context.Context, entryID int64, metadataID int64) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE queue_entries SET state = ?, updated_at = datetime('now') WHERE id = ? AND state = ?`,
			string(StateCompleted), entryID, string(StateProcessing),
		)
		if err != nil {
			return fmt.Errorf("update queue entry state: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("entry %d not in processing state", entryID)
		}
		return nil
	})
}

// Fail transitions a queue entry to failed state.
func (e *Engine) Fail(ctx context.Context, entryID int64, reason string) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE queue_entries SET state = ?, updated_at = datetime('now') WHERE id = ? AND state = ?`,
			string(StateFailed), entryID, string(StateProcessing),
		)
		if err != nil {
			return fmt.Errorf("update queue entry state: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("entry %d not in processing state", entryID)
		}

		// Append audit log
		payload, marshalErr := json.Marshal(map[string]interface{}{
			"event":  "job_failed",
			"entry_id": entryID,
			"reason": reason,
		})
		if marshalErr != nil {
			return fmt.Errorf("marshal failure audit payload: %w", marshalErr)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO audit_logs (sequence, timestamp, event_type, severity, payload)
			 VALUES ((SELECT COALESCE(MAX(sequence),0)+1 FROM audit_logs), datetime('now'), 'job_failed', 'error', ?)`,
			string(payload),
		)
		return err
	})
}

// Requeue moves a failed entry back to pending with incremented retry count and backoff delay.
func (e *Engine) Requeue(ctx context.Context, entryID int64, baseBackoff, maxBackoff time.Duration) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		var retryCount int
		var state string
		err := tx.QueryRowContext(ctx,
			`SELECT retry_count, state FROM queue_entries WHERE id = ?`, entryID,
		).Scan(&retryCount, &state)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("entry %d not found", entryID)
			}
			return fmt.Errorf("query entry: %w", err)
		}

		newRetry := retryCount + 1
		backoff := calculateBackoff(retryCount, baseBackoff, maxBackoff)

		scheduledAt := time.Now().Add(backoff).Format(time.RFC3339)

		_, err = tx.ExecContext(ctx,
			`UPDATE queue_entries SET state = ?, retry_count = ?, scheduled_at = ?, updated_at = datetime('now')
			 WHERE id = ? AND state = ?`,
			string(StatePending), newRetry, scheduledAt, entryID, state,
		)
		if err != nil {
			return fmt.Errorf("requeue entry: %w", err)
		}

		return nil
	})
}

// RequeueNow moves a failed entry back to pending immediately (no backoff).
func (e *Engine) RequeueNow(ctx context.Context, entryID int64) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		var state string
		var retryCount int
		err := tx.QueryRowContext(ctx,
			`SELECT state, retry_count FROM queue_entries WHERE id = ?`, entryID,
		).Scan(&state, &retryCount)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("entry %d not found", entryID)
			}
			return fmt.Errorf("query entry: %w", err)
		}

		newRetry := retryCount + 1
		_, err = tx.ExecContext(ctx,
			`UPDATE queue_entries SET state = ?, retry_count = ?, scheduled_at = NULL, updated_at = datetime('now')
			 WHERE id = ? AND state = ?`,
			string(StatePending), newRetry, entryID, state,
		)
		if err != nil {
			return fmt.Errorf("requeue entry: %w", err)
		}

		e.metrics.queueDepth.Inc()
		return nil
	})
}

// GetPendingDepth returns the number of entries currently in pending state.
func (e *Engine) GetPendingDepth(ctx context.Context) (int64, error) {
	var count int64
	err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM queue_entries WHERE state = ?`, string(StatePending),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending entries: %w", err)
	}
	e.metrics.queueDepth.Set(count)
	return count, nil
}

// GetEntry returns a queue entry by ID.
func (e *Engine) GetEntry(ctx context.Context, id int64) (*QueueEntry, error) {
	var entry QueueEntry
	var scheduledAtStr sql.NullString

	err := e.db.QueryRowContext(ctx,
		`SELECT id, source_path, state, worker_id, priority, retry_count,
			 created_at, scheduled_at, updated_at
		 FROM queue_entries WHERE id = ?`, id,
	).Scan(&entry.ID, &entry.SourcePath, &entry.State, &entry.WorkerID,
		&entry.Priority, &entry.RetryCount, &entry.CreatedAt, &scheduledAtStr, &entry.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrEntryNotFound
		}
		return nil, fmt.Errorf("query entry: %w", err)
	}

	if scheduledAtStr.Valid {
		t, err := time.Parse(time.RFC3339, scheduledAtStr.String)
		if err == nil {
			entry.ScheduledAt = &t
		}
	}

	return &entry, nil
}

// ErrQueueEmpty is returned when no pending entries are available for claiming.
var ErrQueueEmpty = errors.New("no pending entries in queue")

// ErrEntryNotFound is returned when a queue entry does not exist.
var ErrEntryNotFound = errors.New("queue entry not found")

// calculateBackoff computes the exponential backoff delay for a retry.
func calculateBackoff(retryCount int, base, max time.Duration) time.Duration {
	backoff := base
	for i := 0; i < retryCount; i++ {
		backoff *= 2
		if backoff >= max {
			return max
		}
	}
	return backoff
}
