package persistence

import (
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 1

// migration represents a single schema migration.
type migration struct {
	version int
	sql     string
}

// migrations defines all schema migrations in version order.
var migrations = []migration{
	{
		version: 1,
		sql: `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version INTEGER PRIMARY KEY
			);

			CREATE TABLE IF NOT EXISTS queue_entries (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				source_path TEXT NOT NULL,
				state TEXT NOT NULL DEFAULT 'pending',
				worker_id TEXT,
				priority INTEGER NOT NULL DEFAULT 2,
				retry_count INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				scheduled_at TEXT,
				updated_at TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE INDEX IF NOT EXISTS idx_queue_state ON queue_entries(state);
			CREATE INDEX IF NOT EXISTS idx_queue_priority ON queue_entries(priority DESC);
			CREATE INDEX IF NOT EXISTS idx_queue_worker ON queue_entries(worker_id);
			CREATE INDEX IF NOT EXISTS idx_queue_state_priority ON queue_entries(state, priority DESC);

			CREATE TABLE IF NOT EXISTS job_metadata (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				queue_entry_id INTEGER NOT NULL UNIQUE,
				analysis_data TEXT NOT NULL,
				decision TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at TEXT NOT NULL DEFAULT (datetime('now')),
				FOREIGN KEY (queue_entry_id) REFERENCES queue_entries(id) ON DELETE CASCADE
			);

			CREATE INDEX IF NOT EXISTS idx_metadata_queue ON job_metadata(queue_entry_id);

			CREATE TABLE IF NOT EXISTS system_settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				version INTEGER NOT NULL DEFAULT 1,
				updated_at TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE TABLE IF NOT EXISTS audit_logs (
				sequence INTEGER PRIMARY KEY AUTOINCREMENT,
				timestamp TEXT NOT NULL,
				event_type TEXT NOT NULL,
				severity TEXT NOT NULL,
				payload TEXT NOT NULL
			);

			CREATE INDEX IF NOT EXISTS idx_audit_type ON audit_logs(event_type);
			CREATE INDEX IF NOT EXISTS idx_audit_severity ON audit_logs(severity);
			CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_logs(timestamp);
			CREATE INDEX IF NOT EXISTS idx_audit_seq ON audit_logs(sequence DESC);
		`,
	},
}

// migrate applies all pending migrations to the database.
func migrate(db *sql.DB) error {
	// Create schema_migrations table if it doesn't exist
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY
		);
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// Find the current schema version
	var currentVersion int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("query current schema version: %w", err)
	}

	// Apply pending migrations
	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", m.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}

		currentVersion = m.version
	}

	return nil
}
