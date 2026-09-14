package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// ProcessedMediaRecord represents a persistent record of an evaluated or transcoded media file.
type ProcessedMediaRecord struct {
	ID               int64     `json:"id"`
	SourcePath       string    `json:"source_path"`
	FileHash         string    `json:"file_hash"`
	OriginalHash     string    `json:"original_hash,omitempty"`
	FileSize         int64     `json:"file_size"`
	OutputSize       int64     `json:"output_size,omitempty"`
	Status           string    `json:"status"` // "completed", "skipped_quality", "skipped_larger", "ignored", "failed"
	OutputPath       string    `json:"output_path,omitempty"`
	VMAFScore        float64   `json:"vmaf_score,omitempty"`
	DurationMs       int64     `json:"duration_ms,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	ReplacedOriginal bool      `json:"replaced_original"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PersistenceStats represents aggregated statistics of processed files.
type PersistenceStats struct {
	TotalFiles           int64   `json:"total_files"`
	Completed            int64   `json:"completed"`
	SkippedQuality       int64   `json:"skipped_quality"`
	Ignored              int64   `json:"ignored"`
	Failed               int64   `json:"failed"`
	TotalOriginalBytes   int64   `json:"total_original_bytes"`
	TotalTranscodedBytes int64   `json:"total_transcoded_bytes"`
	BytesSaved           int64   `json:"bytes_saved"`
	SavingsPercentage    float64 `json:"savings_percentage"`
	AvgVMAF              float64 `json:"avg_vmaf"`
}

// GetProcessedMediaByHash queries the database for a media record with the given file hash or original hash.
// If found, it returns the record; if not found, it returns (nil, nil).
func (e *Engine) GetProcessedMediaByHash(ctx context.Context, hash string) (*ProcessedMediaRecord, error) {
	if hash == "" {
		return nil, nil
	}

	row := e.db.QueryRowContext(ctx,
		`SELECT id, source_path, file_hash, COALESCE(original_hash, ''), file_size, COALESCE(output_size, 0), status,
		        COALESCE(output_path, ''), COALESCE(vmaf_score, 0),
		        COALESCE(duration_ms, 0), COALESCE(error_message, ''),
		        COALESCE(replaced_original, 0), created_at, updated_at
		 FROM processed_media
		 WHERE file_hash = ? OR original_hash = ?
		 LIMIT 1`,
		hash, hash,
	)

	var rec ProcessedMediaRecord
	var createdAtStr, updatedAtStr string
	var repOrig int
	err := row.Scan(
		&rec.ID, &rec.SourcePath, &rec.FileHash, &rec.OriginalHash, &rec.FileSize, &rec.OutputSize, &rec.Status,
		&rec.OutputPath, &rec.VMAFScore, &rec.DurationMs, &rec.ErrorMessage,
		&repOrig, &createdAtStr, &updatedAtStr,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query processed media by hash: %w", err)
	}
	rec.ReplacedOriginal = (repOrig == 1)

	if t, parseErr := time.Parse(time.RFC3339, createdAtStr); parseErr == nil {
		rec.CreatedAt = t
	} else if t, parseErr := time.Parse("2006-01-02 15:04:05", createdAtStr); parseErr == nil {
		rec.CreatedAt = t
	}
	if t, parseErr := time.Parse(time.RFC3339, updatedAtStr); parseErr == nil {
		rec.UpdatedAt = t
	} else if t, parseErr := time.Parse("2006-01-02 15:04:05", updatedAtStr); parseErr == nil {
		rec.UpdatedAt = t
	}

	return &rec, nil
}

// RecordProcessedMedia creates or updates a processed media record idempotently.
func (e *Engine) RecordProcessedMedia(ctx context.Context, rec *ProcessedMediaRecord) error {
	if rec == nil || rec.FileHash == "" {
		return fmt.Errorf("invalid processed media record: missing hash")
	}

	repOrigInt := 0
	if rec.ReplacedOriginal {
		repOrigInt = 1
	}

	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO processed_media (
				source_path, file_hash, original_hash, file_size, output_size, status, output_path,
				vmaf_score, duration_ms, error_message, replaced_original, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
			ON CONFLICT(file_hash) DO UPDATE SET
				source_path = excluded.source_path,
				original_hash = CASE WHEN excluded.original_hash != '' THEN excluded.original_hash ELSE processed_media.original_hash END,
				file_size = excluded.file_size,
				output_size = excluded.output_size,
				status = excluded.status,
				output_path = excluded.output_path,
				vmaf_score = excluded.vmaf_score,
				duration_ms = excluded.duration_ms,
				error_message = excluded.error_message,
				replaced_original = excluded.replaced_original,
				updated_at = datetime('now')`,
			rec.SourcePath, rec.FileHash, rec.OriginalHash, rec.FileSize, rec.OutputSize, rec.Status,
			rec.OutputPath, rec.VMAFScore, rec.DurationMs, rec.ErrorMessage,
			repOrigInt,
		)
		if err != nil {
			return fmt.Errorf("upsert processed media: %w", err)
		}
		return nil
	})
}

// ListProcessedMedia returns a paginated slice of processed media records, optionally filtered by status.
func (e *Engine) ListProcessedMedia(ctx context.Context, limit, offset int, statusFilter string) ([]ProcessedMediaRecord, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var countQuery string
	var query string
	var args []interface{}
	var countArgs []interface{}

	if statusFilter != "" && statusFilter != "all" {
		countQuery = `SELECT COUNT(*) FROM processed_media WHERE status = ?`
		countArgs = append(countArgs, statusFilter)
		query = `SELECT id, source_path, file_hash, COALESCE(original_hash, ''), file_size, COALESCE(output_size, 0), status,
				        COALESCE(output_path, ''), COALESCE(vmaf_score, 0),
				        COALESCE(duration_ms, 0), COALESCE(error_message, ''),
				        COALESCE(replaced_original, 0), created_at, updated_at
				 FROM processed_media
				 WHERE status = ?
				 ORDER BY updated_at DESC LIMIT ? OFFSET ?`
		args = append(args, statusFilter, limit, offset)
	} else {
		countQuery = `SELECT COUNT(*) FROM processed_media`
		query = `SELECT id, source_path, file_hash, COALESCE(original_hash, ''), file_size, COALESCE(output_size, 0), status,
				        COALESCE(output_path, ''), COALESCE(vmaf_score, 0),
				        COALESCE(duration_ms, 0), COALESCE(error_message, ''),
				        COALESCE(replaced_original, 0), created_at, updated_at
				 FROM processed_media
				 ORDER BY updated_at DESC LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}

	var total int64
	err := e.db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count processed media: %w", err)
	}

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query processed media list: %w", err)
	}
	defer rows.Close()

	var records []ProcessedMediaRecord
	for rows.Next() {
		var rec ProcessedMediaRecord
		var createdAtStr, updatedAtStr string
		var repOrig int
		if err := rows.Scan(
			&rec.ID, &rec.SourcePath, &rec.FileHash, &rec.OriginalHash, &rec.FileSize, &rec.OutputSize, &rec.Status,
			&rec.OutputPath, &rec.VMAFScore, &rec.DurationMs, &rec.ErrorMessage,
			&repOrig, &createdAtStr, &updatedAtStr,
		); err != nil {
			return nil, 0, fmt.Errorf("scan processed media row: %w", err)
		}
		rec.ReplacedOriginal = (repOrig == 1)
		if t, parseErr := time.Parse(time.RFC3339, createdAtStr); parseErr == nil {
			rec.CreatedAt = t
		} else if t, parseErr := time.Parse("2006-01-02 15:04:05", createdAtStr); parseErr == nil {
			rec.CreatedAt = t
		}
		if t, parseErr := time.Parse(time.RFC3339, updatedAtStr); parseErr == nil {
			rec.UpdatedAt = t
		} else if t, parseErr := time.Parse("2006-01-02 15:04:05", updatedAtStr); parseErr == nil {
			rec.UpdatedAt = t
		}
		records = append(records, rec)
	}

	return records, total, rows.Err()
}

// GetProcessedStats aggregates summary statistics across all processed media records.
func (e *Engine) GetProcessedStats(ctx context.Context) (*PersistenceStats, error) {
	stats := &PersistenceStats{}

	// Aggregate counts by status
	rows, err := e.db.QueryContext(ctx,
		`SELECT status, COUNT(*), COALESCE(SUM(file_size), 0), COALESCE(AVG(CASE WHEN vmaf_score > 0 THEN vmaf_score END), 0)
		 FROM processed_media
		 GROUP BY status`,
	)
	if err != nil {
		return nil, fmt.Errorf("query processed stats: %w", err)
	}
	defer rows.Close()

	var vmafSum float64
	var vmafCount int64

	for rows.Next() {
		var status string
		var count, origBytes int64
		var avgVMAF float64
		if err := rows.Scan(&status, &count, &origBytes, &avgVMAF); err != nil {
			return nil, fmt.Errorf("scan stats row: %w", err)
		}

		stats.TotalFiles += count
		switch status {
		case "completed":
			stats.Completed = count
			stats.TotalOriginalBytes += origBytes
			if avgVMAF > 0 {
				vmafSum += avgVMAF * float64(count)
				vmafCount += count
			}
		case "skipped_quality", "skipped_larger":
			stats.SkippedQuality += count
		case "ignored":
			stats.Ignored = count
		case "failed":
			stats.Failed = count
		}
	}

	if vmafCount > 0 {
		stats.AvgVMAF = vmafSum / float64(vmafCount)
	}

	// Calculate transcoded output bytes and bytes saved for completed files
	compRows, err := e.db.QueryContext(ctx,
		`SELECT file_size, output_size, COALESCE(output_path, '') FROM processed_media WHERE status = 'completed'`,
	)
	if err == nil {
		defer compRows.Close()
		for compRows.Next() {
			var origSize, outSize int64
			var outPath string
			if err := compRows.Scan(&origSize, &outSize, &outPath); err == nil {
				// If output_size was not saved directly in DB (legacy records), fallback to checking file
				if outSize <= 0 && outPath != "" {
					if fi, statErr := os.Stat(outPath); statErr == nil {
						outSize = fi.Size()
					} else if strings.HasPrefix(outPath, "/app/") {
						rel := strings.TrimPrefix(outPath, "/app/")
						if fi, statErr := os.Stat(rel); statErr == nil {
							outSize = fi.Size()
						}
					}
				}
				if outSize > 0 {
					stats.TotalTranscodedBytes += outSize
					if origSize > outSize {
						stats.BytesSaved += (origSize - outSize)
					}
				}
			}
		}
	}

	if stats.TotalOriginalBytes > 0 && stats.BytesSaved > 0 {
		stats.SavingsPercentage = (float64(stats.BytesSaved) / float64(stats.TotalOriginalBytes)) * 100.0
	}

	return stats, nil
}
