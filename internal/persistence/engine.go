package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
)

// State represents the processing state of a queue entry.
type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
	StateQueued     State = "queued"
	StateReview     State = "review"
)

// Priority represents the priority level of a queue entry.
type Priority int

const (
	PriorityLow    Priority = 1
	PriorityNormal Priority = 2
	PriorityHigh   Priority = 3
	PriorityCritical Priority = 4
)

// Engine provides durable, transactional storage for the application's queue entries,
// job metadata, system settings, and audit logs.
type Engine struct {
	db       *sql.DB
	logger   *observability.Logger
	cfg      *config.PersistenceSettings
	mu       sync.Mutex
	closed   bool
	metrics  *engineMetrics
}

type engineMetrics struct {
	transactionCount   *observability.Counter
	transactionLatency *observability.Histogram
	queueDepth         *observability.Gauge
}

// New creates a new persistence engine connected to the configured database.
// It runs schema migrations if needed and validates the connection.
func New(cfg *config.PersistenceSettings, logger *observability.Logger, metrics *observability.Registry) *Engine {
	if metrics == nil {
		metrics = observability.NewRegistry("")
	}

	em := &engineMetrics{
		transactionCount:   metrics.RegisterCounter("db_transactions", "total database transactions"),
		transactionLatency: metrics.RegisterHistogram("db_transaction_duration_ms", "transaction duration in milliseconds", []float64{5, 10, 25, 50, 100, 250, 500}),
		queueDepth:         metrics.RegisterGauge("queue_depth_pending", "number of pending queue entries"),
	}

	e := &Engine{
		logger: logger.WithFields(observability.Field{Key: "module", Value: "persistence"}),
		cfg:    cfg,
		metrics: em,
	}

	if err := e.open(); err != nil {
		logger.WithFields(observability.Field{Key: "error", Value: err}).Critical("failed to open database")
		return nil
	}

	return e
}

// open opens the database connection, runs migrations, and validates connectivity.
func (e *Engine) open() error {
	db, err := sql.Open("sqlite", e.cfg.DatabasePath+"?_journal=WAL&_synchronous="+e.cfg.Synchronous+"&cache=shared&_txlock=immediate")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return fmt.Errorf("ping database: %w", err)
	}

	if e.cfg.MigrationAuto {
		if err := migrate(db); err != nil {
			db.Close()
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	e.db = db
	e.logger.WithFields(
		observability.Field{Key: "path", Value: e.cfg.DatabasePath},
		observability.Field{Key: "synchronous", Value: e.cfg.Synchronous},
	).Info("database connected")

	return nil
}

// Close cleans up the database connection.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil
	}
	e.closed = true

	if e.db != nil {
		if err := e.db.Close(); err != nil {
			return fmt.Errorf("close database: %w", err)
		}
	}

	e.logger.Info("database connection closed")
	return nil
}

// DB returns the underlying *sql.DB for direct access when needed.
func (e *Engine) DB() *sql.DB {
	return e.db
}

// InTransaction executes fn within a transaction.
// On any error from fn, the transaction is automatically rolled back.
// On success, the transaction is committed.
func (e *Engine) InTransaction(ctx context.Context, fn func(tx *sql.Tx) error) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("engine closed")
	}
	e.mu.Unlock()

	start := time.Now()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	e.metrics.transactionCount.Inc()

	defer func() {
		if rec := recover(); rec != nil {
			_ = tx.Rollback()
			panic(rec)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			e.logger.WithFields(observability.Field{Key: "error", Value: rbErr}).Error("rollback failed")
		}
		return fmt.Errorf("transaction failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	e.metrics.transactionLatency.Observe(latency)

	return nil
}

// RecoverProcessing resets any queue entries left in processing state back to pending
// with incremented retry count. Called on startup after an unclean shutdown.
func (e *Engine) RecoverProcessing(ctx context.Context) (int, error) {
	count := 0
	if err := e.InTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, source_path, retry_count FROM queue_entries WHERE state = ?`,
			string(StateProcessing),
		)
		if err != nil {
			return fmt.Errorf("query processing entries: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var id int64
			var path string
			var retryCount int
			if err := rows.Scan(&id, &path, &retryCount); err != nil {
				return fmt.Errorf("scan queue entry: %w", err)
			}

			newRetry := retryCount + 1
			_, err := tx.ExecContext(ctx,
				`UPDATE queue_entries SET state = ?, retry_count = ?, updated_at = datetime('now') WHERE id = ?`,
				string(StatePending), newRetry, id,
			)
			if err != nil {
				return fmt.Errorf("requeue entry %d: %w", id, err)
			}

			// Append audit log for recovery
			payload, marshalErr := json.Marshal(map[string]interface{}{
				"event":       "recovery_requeue",
				"entry_id":    id,
				"path":        path,
				"retry_count": newRetry,
			})
			if marshalErr != nil {
				return fmt.Errorf("marshal recovery audit payload: %w", marshalErr)
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO audit_logs (sequence, timestamp, event_type, severity, payload)
				 VALUES ((SELECT COALESCE(MAX(sequence),0)+1 FROM audit_logs), datetime('now'), 'recovery', 'info', ?)`,
				string(payload),
			)
			if err != nil {
				e.logger.WithFields(observability.Field{Key: "entry_id", Value: id}).Warn("failed to write recovery audit log")
			}

			count++
		}

		return rows.Err()
	}); err != nil {
		return count, fmt.Errorf("recover processing entries: %w", err)
	}

	if count > 0 {
		e.logger.WithFields(observability.Field{Key: "count", Value: count}).Info("recovered processing entries")
	}

	return count, nil
}

// Validate checks that the database schema is at the expected version.
func (e *Engine) Validate() error {
	var version int
	err := e.db.QueryRow("SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&version)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no schema migrations found")
		}
		return fmt.Errorf("query schema version: %w", err)
	}

	if version != currentSchemaVersion {
		return fmt.Errorf("schema version mismatch: database=%d, expected=%d", version, currentSchemaVersion)
	}

	return nil
}


