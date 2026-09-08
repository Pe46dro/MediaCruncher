package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuditEvent represents an audit log entry.
type AuditEvent struct {
	Sequence  int64
	Timestamp time.Time
	EventType string
	Severity  string
	Payload   string
}

// AppendAuditLog appends a single audit log entry.
func (e *Engine) AppendAuditLog(ctx context.Context, eventType, severity, payload string) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO audit_logs (sequence, timestamp, event_type, severity, payload)
			 VALUES ((SELECT COALESCE(MAX(sequence),0)+1 FROM audit_logs), datetime('now'), ?, ?, ?)`,
			eventType, severity, payload,
		)
		return err
	})
}

// BulkAppendAuditLog inserts multiple audit log entries within a single transaction.
func (e *Engine) BulkAppendAuditLog(ctx context.Context, events []AuditEvent) error {
	if len(events) == 0 {
		return nil
	}

	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		// Get next sequence
		var nextSeq int64
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sequence),0)+1 FROM audit_logs`,
		).Scan(&nextSeq)
		if err != nil {
			return fmt.Errorf("get next sequence: %w", err)
		}

		for i, ev := range events {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO audit_logs (sequence, timestamp, event_type, severity, payload)
				 VALUES (?, ?, ?, ?, ?)`,
				nextSeq+int64(i), ev.Timestamp.Format(time.RFC3339Nano), ev.EventType, ev.Severity, ev.Payload,
			)
			if err != nil {
				return fmt.Errorf("insert audit entry %d: %w", i, err)
			}
		}

		return nil
	})
}

// PurgeOldAuditLogs removes entries older than the retention period,
// retaining at least minEntries recent entries.
func (e *Engine) PurgeOldAuditLogs(ctx context.Context, retention time.Duration, minEntries int) (int64, error) {
	var toPurge int64
	err := e.InTransaction(ctx, func(tx *sql.Tx) error {
		// Count total entries
		var total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&total); err != nil {
			return fmt.Errorf("count audit entries: %w", err)
		}

		entriesToKeep := total - minEntries
		if entriesToKeep <= 0 {
			return nil
		}

		cutoff := time.Now().Add(-retention).Format(time.RFC3339Nano)
		result, err := tx.ExecContext(ctx,
			`DELETE FROM audit_logs WHERE timestamp < ? AND sequence NOT IN
			 (SELECT sequence FROM audit_logs ORDER BY sequence DESC LIMIT ?)`,
			cutoff, entriesToKeep,
		)
		if err != nil {
			return fmt.Errorf("purge old audit logs: %w", err)
		}

		toPurge, err = result.RowsAffected()
		return err
	})

	return toPurge, err
}

// QueryAuditLogs queries audit log entries by event type, severity, time range, or job context.
func (e *Engine) QueryAuditLogs(ctx context.Context, eventType, severity string, startTime, endTime *time.Time, limit int) ([]AuditEvent, error) {
	query := `SELECT sequence, timestamp, event_type, severity, payload FROM audit_logs WHERE 1=1`
	args := []interface{}{}

	if eventType != "" {
		query += " AND event_type = ?"
		args = append(args, eventType)
	}
	if severity != "" {
		query += " AND severity = ?"
		args = append(args, severity)
	}
	if startTime != nil {
		query += " AND timestamp >= ?"
		args = append(args, startTime.Format(time.RFC3339Nano))
	}
	if endTime != nil {
		query += " AND timestamp <= ?"
		args = append(args, endTime.Format(time.RFC3339Nano))
	}

	query += " ORDER BY sequence DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: %w", err)
	}
	defer rows.Close()

	var events []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var tsStr string
		if err := rows.Scan(&ev.Sequence, &tsStr, &ev.EventType, &ev.Severity, &ev.Payload); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		ev.Timestamp, err = time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			ev.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
		}
		events = append(events, ev)
	}

	return events, rows.Err()
}

// GetAuditEventCount returns the total number of audit log entries.
func (e *Engine) GetAuditEventCount(ctx context.Context) (int64, error) {
	var count int64
	err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count audit events: %w", err)
	}
	return count, nil
}
