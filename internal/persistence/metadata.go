package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// JobMetadata stores the analysis results and decision for a media file.
type JobMetadata struct {
	ID           int64
	QueueEntryID int64
	AnalysisData map[string]interface{}
	Decision     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UpsertJobMetadata creates a new metadata record or updates an existing one
// for the given queue entry ID.
func (e *Engine) UpsertJobMetadata(ctx context.Context, entryID int64, analysisData map[string]interface{}, decision string) (int64, error) {
	analysisJSON, err := json.Marshal(analysisData)
	if err != nil {
		return 0, fmt.Errorf("marshal analysis data: %w", err)
	}

	var id int64
	err = e.InTransaction(ctx, func(tx *sql.Tx) error {
		// Try to update existing
		result, err := tx.ExecContext(ctx,
			`UPDATE job_metadata SET analysis_data = ?, decision = ?, updated_at = datetime('now')
			 WHERE queue_entry_id = ?`,
			string(analysisJSON), decision, entryID,
		)
		if err != nil {
			return fmt.Errorf("update metadata: %w", err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}

		if rows > 0 {
			// Get existing ID
			return tx.QueryRowContext(ctx,
				`SELECT id FROM job_metadata WHERE queue_entry_id = ?`, entryID,
			).Scan(&id)
		}

		// Insert new
		result, err = tx.ExecContext(ctx,
			`INSERT INTO job_metadata (queue_entry_id, analysis_data, decision, created_at, updated_at)
			 VALUES (?, ?, ?, datetime('now'), datetime('now'))`,
			entryID, string(analysisJSON), decision,
		)
		if err != nil {
			return fmt.Errorf("insert metadata: %w", err)
		}
		id, err = result.LastInsertId()
		return err
	})

	return id, err
}

// GetJobMetadata retrieves full metadata for a given queue entry.
func (e *Engine) GetJobMetadata(ctx context.Context, entryID int64) (*JobMetadata, error) {
	var meta JobMetadata
	var analysisJSON string

	err := e.db.QueryRowContext(ctx,
		`SELECT id, queue_entry_id, analysis_data, decision, created_at, updated_at
		 FROM job_metadata WHERE queue_entry_id = ?`, entryID,
	).Scan(&meta.ID, &meta.QueueEntryID, &analysisJSON, &meta.Decision, &meta.CreatedAt, &meta.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrMetadataNotFound
		}
		return nil, fmt.Errorf("query metadata: %w", err)
	}

	if err := json.Unmarshal([]byte(analysisJSON), &meta.AnalysisData); err != nil {
		return nil, fmt.Errorf("unmarshal analysis data: %w", err)
	}

	return &meta, nil
}

// BulkGetJobMetadata retrieves metadata for a set of queue entry IDs.
func (e *Engine) BulkGetJobMetadata(ctx context.Context, entryIDs []int64) ([]JobMetadata, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(entryIDs))
	args := make([]interface{}, len(entryIDs))
	for i, id := range entryIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := e.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, queue_entry_id, analysis_data, decision, created_at, updated_at
					 FROM job_metadata WHERE queue_entry_id IN (%s)`,
			joinPlaceholders(placeholders)), args...)
	if err != nil {
		return nil, fmt.Errorf("bulk query metadata: %w", err)
	}
	defer rows.Close()

	var results []JobMetadata
	for rows.Next() {
		var meta JobMetadata
		var analysisJSON string
		if err := rows.Scan(&meta.ID, &meta.QueueEntryID, &analysisJSON, &meta.Decision, &meta.CreatedAt, &meta.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan metadata: %w", err)
		}
		if err := json.Unmarshal([]byte(analysisJSON), &meta.AnalysisData); err != nil {
			return nil, fmt.Errorf("unmarshal analysis data: %w", err)
		}
		results = append(results, meta)
	}

	return results, rows.Err()
}

// DeleteJobMetadata removes metadata for a given queue entry.
func (e *Engine) DeleteJobMetadata(ctx context.Context, entryID int64) error {
	result, err := e.db.ExecContext(ctx,
		`DELETE FROM job_metadata WHERE queue_entry_id = ?`, entryID,
	)
	if err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrMetadataNotFound
	}

	return nil
}

// ErrMetadataNotFound is returned when no metadata exists for the entry.
var ErrMetadataNotFound = fmt.Errorf("job metadata not found")

func joinPlaceholders(p []string) string {
	result := make([]byte, 0, len(p)*2-1)
	for i, s := range p {
		if i > 0 {
			result = append(result, ',')
		}
		result = append(result, s...)
	}
	return string(result)
}
