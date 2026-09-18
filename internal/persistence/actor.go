package persistence

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"mediacruncher/internal/observability"
)

type writeReq struct {
	fn     func(tx *sql.Tx) (any, error)
	respCh chan writeResp
}

type writeResp struct {
	result any
	err    error
}

// Engine implements the Single-Writer Actor architecture backed by SQLite.
type Engine struct {
	writeDB *sql.DB
	readDB  *sql.DB
	writeCh chan writeReq
	done    chan struct{}
	wg      sync.WaitGroup
}

func NewEngine(dbPath string, busyTimeout int) (*Engine, error) {
	writeDB, readDB, err := InitDB(dbPath, busyTimeout)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		writeDB: writeDB,
		readDB:  readDB,
		writeCh: make(chan writeReq, 500),
		done:    make(chan struct{}),
	}

	e.wg.Add(1)
	go e.writerLoop()

	return e, nil
}

func (e *Engine) writerLoop() {
	defer e.wg.Done()

	metrics := observability.GetMetrics()

	for {
		select {
		case req := <-e.writeCh:
			metrics.WriteActorQueueSize.Store(int64(len(e.writeCh)))

			tx, err := e.writeDB.Begin()
			if err != nil {
				req.respCh <- writeResp{err: fmt.Errorf("failed to begin write tx: %w", err)}
				continue
			}

			res, err := req.fn(tx)
			if err != nil {
				tx.Rollback()
				req.respCh <- writeResp{err: err}
			} else {
				if commitErr := tx.Commit(); commitErr != nil {
					req.respCh <- writeResp{err: fmt.Errorf("failed to commit write tx: %w", commitErr)}
				} else {
					req.respCh <- writeResp{result: res}
				}
			}

		case <-e.done:
			// Drain remaining writes before exiting
			for {
				select {
				case req := <-e.writeCh:
					tx, err := e.writeDB.Begin()
					if err != nil {
						req.respCh <- writeResp{err: err}
						continue
					}
					res, err := req.fn(tx)
					if err != nil {
						tx.Rollback()
						req.respCh <- writeResp{err: err}
					} else {
						tx.Commit()
						req.respCh <- writeResp{result: res}
					}
				default:
					return
				}
			}
		}
	}
}

func (e *Engine) execWrite(fn func(tx *sql.Tx) (any, error)) (any, error) {
	respCh := make(chan writeResp, 1)
	select {
	case e.writeCh <- writeReq{fn: fn, respCh: respCh}:
	case <-e.done:
		return nil, fmt.Errorf("persistence engine is shutting down")
	}

	resp := <-respCh
	return resp.result, resp.err
}

// Enqueue inserts a new job into the queue, or ignores if already present.
func (e *Engine) Enqueue(filePath string, priority int) (int64, error) {
	res, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		now := time.Now().UTC()
		query := `
		INSERT INTO queue_entries (file_path, state, priority, retry_count, created_at, scheduled_at)
		VALUES (?, ?, ?, 0, ?, ?)
		ON CONFLICT(file_path) DO NOTHING
		`
		result, err := tx.Exec(query, filePath, StatePending, priority, now, now)
		if err != nil {
			return int64(0), err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil || rowsAffected == 0 {
			// Already enqueued
			return int64(0), nil
		}
		id, err := result.LastInsertId()
		return id, err
	})
	if err != nil {
		return 0, err
	}
	return res.(int64), nil
}

// LeaseBatch atomically claims the highest priority pending entries for a worker.
func (e *Engine) LeaseBatch(workerID string, limit int, leaseDuration time.Duration) ([]*QueueEntry, error) {
	res, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		now := time.Now().UTC()
		leaseExpires := now.Add(leaseDuration)

		selectQuery := `
		SELECT id, file_path, priority, retry_count, created_at, scheduled_at
		FROM queue_entries
		WHERE state = ? AND scheduled_at <= ?
		ORDER BY priority DESC, scheduled_at ASC
		LIMIT ?
		`
		rows, err := tx.Query(selectQuery, StatePending, now, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var entries []*QueueEntry
		var ids []any
		for rows.Next() {
			var item QueueEntry
			item.State = StateLeased
			item.WorkerID = workerID
			item.LeasedAt = &now
			item.LeaseExpiresAt = &leaseExpires
			if err := rows.Scan(&item.ID, &item.FilePath, &item.Priority, &item.RetryCount, &item.CreatedAt, &item.ScheduledAt); err != nil {
				return nil, err
			}
			entries = append(entries, &item)
			ids = append(ids, item.ID)
		}

		if len(ids) == 0 {
			return entries, nil
		}

		// Update state to leased
		updateQuery := fmt.Sprintf(`
		UPDATE queue_entries
		SET state = ?, worker_id = ?, leased_at = ?, lease_expires_at = ?
		WHERE id IN (%s)
		`, joinPlaceholders(len(ids)))

		args := append([]any{StateLeased, workerID, now, leaseExpires}, ids...)
		if _, err := tx.Exec(updateQuery, args...); err != nil {
			return nil, err
		}

		return entries, nil
	})

	if err != nil {
		return nil, err
	}
	return res.([]*QueueEntry), nil
}

func joinPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n*2-1)
	for i := 0; i < n; i++ {
		b[i*2] = '?'
		if i < n-1 {
			b[i*2+1] = ','
		}
	}
	return string(b)
}

// UpdateJobState updates state, worker assignment, and optional error message.
func (e *Engine) UpdateJobState(queueID int64, state JobState, errMsg string) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `UPDATE queue_entries SET state = ?, error_message = ? WHERE id = ?`
		_, err := tx.Exec(query, state, errMsg, queueID)
		return nil, err
	})
	return err
}

// RequeueWithBackoff transitions job back to pending with incremented retry count and delay.
func (e *Engine) RequeueWithBackoff(queueID int64, delay time.Duration, errMsg string) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		scheduled := time.Now().UTC().Add(delay)
		query := `
		UPDATE queue_entries 
		SET state = ?, retry_count = retry_count + 1, scheduled_at = ?, error_message = ?, worker_id = NULL, leased_at = NULL, lease_expires_at = NULL
		WHERE id = ?
		`
		_, err := tx.Exec(query, StatePending, scheduled, errMsg, queueID)
		return nil, err
	})
	return err
}

// SaveMetadata stores analysis results and decision records for a job.
func (e *Engine) SaveMetadata(meta *JobMetadata) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `
		INSERT INTO job_metadata (
			queue_id, video_codec, resolution, bitrate, duration, 
			audio_tracks, subtitles, decision_action, preset, stream_map_json, normalized_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(queue_id) DO UPDATE SET
			video_codec = excluded.video_codec,
			resolution = excluded.resolution,
			bitrate = excluded.bitrate,
			duration = excluded.duration,
			audio_tracks = excluded.audio_tracks,
			subtitles = excluded.subtitles,
			decision_action = excluded.decision_action,
			preset = excluded.preset,
			stream_map_json = excluded.stream_map_json,
			normalized_json = excluded.normalized_json
		`
		now := time.Now().UTC()
		_, err := tx.Exec(
			query,
			meta.QueueID, meta.VideoCodec, meta.Resolution, meta.Bitrate, meta.Duration,
			meta.AudioTracks, meta.Subtitles, meta.DecisionAction, meta.Preset, meta.StreamMapJSON, meta.NormalizedJSON, now,
		)
		return nil, err
	})
	return err
}

// LogAudit appends an audit event to the persistent audit log table.
func (e *Engine) LogAudit(eventType, severity, payload string) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `INSERT INTO audit_logs (timestamp, event_type, severity, payload_json) VALUES (?, ?, ?, ?)`
		_, err := tx.Exec(query, time.Now().UTC(), eventType, severity, payload)
		return nil, err
	})
	return err
}

// RecordAudit is a convenience wrapper for logging audit events from an AuditLog object.
func (e *Engine) RecordAudit(log *AuditLog) error {
	return e.LogAudit(log.EventType, log.Severity, log.PayloadJSON)
}

// SaveDeliveryRecord records a notification transmission result.
func (e *Engine) SaveDeliveryRecord(eventID, channel, status, response string) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `INSERT INTO delivery_records (event_id, channel, status, response, timestamp) VALUES (?, ?, ?, ?, ?)`
		_, err := tx.Exec(query, eventID, channel, status, response, time.Now().UTC())
		return nil, err
	})
	return err
}

// SaveDeadLetter enqueues a failed notification into the dead-letter queue.
func (e *Engine) SaveDeadLetter(eventJSON, lastErr string) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `INSERT INTO dead_letter_queue (event_json, retry_count, last_error, created_at) VALUES (?, 0, ?, ?)`
		_, err := tx.Exec(query, eventJSON, lastErr, time.Now().UTC())
		return nil, err
	})
	return err
}

// CrashRecovery resets any jobs left in leased or processing states where lease expired.
func (e *Engine) CrashRecovery() (int64, error) {
	res, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		now := time.Now().UTC()
		query := `
		UPDATE queue_entries 
		SET state = ?, retry_count = retry_count + 1, worker_id = NULL, leased_at = NULL, lease_expires_at = NULL
		WHERE (state = ? OR state = ? OR state = ? OR state = ?) AND (lease_expires_at IS NULL OR lease_expires_at <= ?)
		`
		result, err := tx.Exec(query, StatePending, StateLeased, StateProcessing, StateEvaluating, StateTranscoding, now)
		if err != nil {
			return int64(0), err
		}
		recovered, err := result.RowsAffected()
		if recovered > 0 {
			auditQuery := `INSERT INTO audit_logs (timestamp, event_type, severity, payload_json) VALUES (?, ?, ?, ?)`
			tx.Exec(auditQuery, now, "crash_recovery", "warn", fmt.Sprintf(`{"recovered_jobs": %d}`, recovered))
		}
		return recovered, err
	})
	if err != nil {
		return 0, err
	}
	return res.(int64), nil
}

// RecoverOrphanedLeases is an alias for CrashRecovery.
func (e *Engine) RecoverOrphanedLeases() (int64, error) {
	return e.CrashRecovery()
}

// ResetInFlightJobs resets all jobs in leased/evaluating/transcoding states back to pending.
// Called on startup to ensure work interrupted by an unexpected restart resumes immediately.
func (e *Engine) ResetInFlightJobs() (int64, error) {
	res, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		now := time.Now().UTC()
		query := `
		UPDATE queue_entries 
		SET state = ?, worker_id = NULL, leased_at = NULL, lease_expires_at = NULL
		WHERE state = ? OR state = ? OR state = ? OR state = ?
		`
		result, err := tx.Exec(query, StatePending, StateLeased, StateProcessing, StateEvaluating, StateTranscoding)
		if err != nil {
			return int64(0), err
		}
		recovered, err := result.RowsAffected()
		if recovered > 0 {
			auditQuery := `INSERT INTO audit_logs (timestamp, event_type, severity, payload_json) VALUES (?, ?, ?, ?)`
			tx.Exec(auditQuery, now, "startup_recovery", "info", fmt.Sprintf(`{"recovered_jobs": %d}`, recovered))
		}
		return recovered, err
	})
	if err != nil {
		return 0, err
	}
	return res.(int64), nil
}

type QueueStats struct {
	Pending   int64 `json:"pending"`
	Leased    int64 `json:"leased"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

func (e *Engine) GetStats() (*QueueStats, error) {
	p, l, c, f, err := e.GetQueueCounts()
	if err != nil {
		return nil, err
	}
	return &QueueStats{
		Pending:   p,
		Leased:    l,
		Completed: c,
		Failed:    f,
	}, nil
}

// Read Queries (run directly on readDB connection pool without blocking writer)

func (e *Engine) GetQueueCounts() (pending, leased, completed, failed int64, err error) {
	rows, err := e.readDB.Query(`SELECT state, COUNT(*) FROM queue_entries GROUP BY state`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			continue
		}
		switch JobState(state) {
		case StatePending:
			pending += count
		case StateLeased, StateProcessing, StateEvaluating, StateTranscoding:
			leased += count
		case StateCompleted, StateSkipped:
			completed += count
		case StateFailed, StateQualityFailed, StatePermanentlyFailed:
			failed += count
		}
	}
	return pending, leased, completed, failed, nil
}

func (e *Engine) GetQueueEntry(id int64) (*QueueEntry, error) {
	query := `SELECT id, file_path, state, worker_id, priority, retry_count, created_at, scheduled_at, error_message FROM queue_entries WHERE id = ?`
	row := e.readDB.QueryRow(query, id)
	var item QueueEntry
	var workerID, errMsg sql.NullString
	if err := row.Scan(&item.ID, &item.FilePath, &item.State, &workerID, &item.Priority, &item.RetryCount, &item.CreatedAt, &item.ScheduledAt, &errMsg); err != nil {
		return nil, err
	}
	if workerID.Valid {
		item.WorkerID = workerID.String
	}
	if errMsg.Valid {
		item.ErrorMessage = errMsg.String
	}
	return &item, nil
}

func (e *Engine) GetMetadata(queueID int64) (*JobMetadata, error) {
	query := `SELECT queue_id, video_codec, resolution, bitrate, duration, audio_tracks, subtitles, decision_action, preset, stream_map_json, normalized_json, created_at FROM job_metadata WHERE queue_id = ?`
	row := e.readDB.QueryRow(query, queueID)
	var meta JobMetadata
	if err := row.Scan(&meta.QueueID, &meta.VideoCodec, &meta.Resolution, &meta.Bitrate, &meta.Duration, &meta.AudioTracks, &meta.Subtitles, &meta.DecisionAction, &meta.Preset, &meta.StreamMapJSON, &meta.NormalizedJSON, &meta.CreatedAt); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (e *Engine) GetRecentAuditLogs(limit int) ([]*AuditLog, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT seq_id, timestamp, event_type, severity, payload_json FROM audit_logs ORDER BY seq_id DESC LIMIT ?`
	rows, err := e.readDB.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*AuditLog
	for rows.Next() {
		var l AuditLog
		var payload sql.NullString
		if err := rows.Scan(&l.SeqID, &l.Timestamp, &l.EventType, &l.Severity, &payload); err != nil {
			return nil, err
		}
		if payload.Valid {
			l.PayloadJSON = payload.String
		}
		logs = append(logs, &l)
	}
	return logs, nil
}

func (e *Engine) Close() error {
	slog.Info("closing persistence engine")
	close(e.done)
	e.wg.Wait()

	var errs []error
	if err := e.writeDB.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := e.readDB.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors closing database: %v", errs)
	}
	return nil
}

// ReadDB exposes the read connection pool for metrics and test queries.
func (e *Engine) ReadDB() *sql.DB {
	return e.readDB
}

// ListQueueEntries returns paged queue items with optional state filter.
func (e *Engine) ListQueueEntries(state string, limit, offset int) ([]*QueueEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var rows *sql.Rows
	var err error
	if state != "" && state != "all" {
		if state == "failed" {
			query := `SELECT id, file_path, state, worker_id, priority, retry_count, created_at, scheduled_at, error_message FROM queue_entries WHERE state IN ('failed', 'quality_failed', 'permanently_failed') ORDER BY id DESC LIMIT ? OFFSET ?`
			rows, err = e.readDB.Query(query, limit, offset)
		} else {
			query := `SELECT id, file_path, state, worker_id, priority, retry_count, created_at, scheduled_at, error_message FROM queue_entries WHERE state = ? ORDER BY id DESC LIMIT ? OFFSET ?`
			rows, err = e.readDB.Query(query, state, limit, offset)
		}
	} else {
		query := `SELECT id, file_path, state, worker_id, priority, retry_count, created_at, scheduled_at, error_message FROM queue_entries ORDER BY id DESC LIMIT ? OFFSET ?`
		rows, err = e.readDB.Query(query, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*QueueEntry
	for rows.Next() {
		var item QueueEntry
		var workerID, errMsg sql.NullString
		if err := rows.Scan(&item.ID, &item.FilePath, &item.State, &workerID, &item.Priority, &item.RetryCount, &item.CreatedAt, &item.ScheduledAt, &errMsg); err != nil {
			return nil, err
		}
		if workerID.Valid {
			item.WorkerID = workerID.String
		}
		if errMsg.Valid {
			item.ErrorMessage = errMsg.String
		}
		entries = append(entries, &item)
	}
	return entries, nil
}

// RequeueJob resets an existing job to pending status for re-execution.
func (e *Engine) RequeueJob(id int64) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `
		UPDATE queue_entries 
		SET state = ?, retry_count = 0, scheduled_at = ?, error_message = NULL, worker_id = NULL, leased_at = NULL, lease_expires_at = NULL
		WHERE id = ?
		`
		_, err := tx.Exec(query, StatePending, time.Now().UTC(), id)
		return nil, err
	})
	return err
}

// DeleteJob permanently removes a job from the queue.
func (e *Engine) DeleteJob(id int64) error {
	_, err := e.execWrite(func(tx *sql.Tx) (any, error) {
		query := `DELETE FROM queue_entries WHERE id = ?`
		_, err := tx.Exec(query, id)
		return nil, err
	})
	return err
}
