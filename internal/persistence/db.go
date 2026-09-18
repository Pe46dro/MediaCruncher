package persistence

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// InitDB initializes SQLite with required WAL pragmas and applies all schema migrations.
func InitDB(dbPath string, busyTimeout int) (*sql.DB, *sql.DB, error) {
	if busyTimeout <= 0 {
		busyTimeout = 5000
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, fmt.Errorf("failed to create db directory %s: %w", dir, err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", dbPath, busyTimeout)

	// Write Connection (Strictly single connection to serialize writes)
	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	// Run PRAGMAs directly to ensure configuration
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeout),
		"PRAGMA foreign_keys = ON;",
	}
	for _, pragma := range pragmas {
		if _, err := writeDB.Exec(pragma); err != nil {
			writeDB.Close()
			return nil, nil, fmt.Errorf("failed to execute pragma '%s': %w", pragma, err)
		}
	}

	// Apply Schema Migrations
	if err := applyMigrations(writeDB); err != nil {
		writeDB.Close()
		return nil, nil, fmt.Errorf("migration failure: %w", err)
	}

	// Read Pool (Concurrent readers without blocking WAL writer)
	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		writeDB.Close()
		return nil, nil, fmt.Errorf("failed to open read db: %w", err)
	}
	readDB.SetMaxOpenConns(10)
	readDB.SetMaxIdleConns(5)

	return writeDB, readDB, nil
}

func applyMigrations(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS queue_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT NOT NULL UNIQUE,
		state TEXT NOT NULL,
		worker_id TEXT,
		priority INTEGER NOT NULL DEFAULT 50,
		retry_count INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL,
		leased_at DATETIME,
		lease_expires_at DATETIME,
		scheduled_at DATETIME NOT NULL,
		error_message TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_queue_state_priority ON queue_entries(state, priority DESC, scheduled_at ASC);
	CREATE INDEX IF NOT EXISTS idx_queue_lease_expires ON queue_entries(state, lease_expires_at);

	CREATE TABLE IF NOT EXISTS job_metadata (
		queue_id INTEGER PRIMARY KEY,
		video_codec TEXT,
		resolution TEXT,
		bitrate INTEGER,
		duration REAL,
		audio_tracks TEXT,
		subtitles TEXT,
		decision_action TEXT,
		preset TEXT,
		stream_map_json TEXT,
		normalized_json TEXT,
		created_at DATETIME NOT NULL,
		FOREIGN KEY(queue_id) REFERENCES queue_entries(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS system_settings (
		key TEXT PRIMARY KEY,
		value_json TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS audit_logs (
		seq_id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		event_type TEXT NOT NULL,
		severity TEXT NOT NULL,
		payload_json TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_audit_event ON audit_logs(event_type, timestamp DESC);

	CREATE TABLE IF NOT EXISTS delivery_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT,
		channel TEXT NOT NULL,
		status TEXT NOT NULL,
		response TEXT,
		timestamp DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS dead_letter_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_json TEXT NOT NULL,
		retry_count INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_at DATETIME NOT NULL
	);
	`

	_, err := db.Exec(schema)
	return err
}
