package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SystemSetting holds a single key-value configuration setting with version stamp.
type SystemSetting struct {
	Key       string
	Value     string
	Version   int
	UpdatedAt time.Time
}

// LoadAllSettings loads all system settings into a map keyed by setting name.
func (e *Engine) LoadAllSettings(ctx context.Context) (map[string]SystemSetting, error) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT key, value, version, updated_at FROM system_settings`,
	)
	if err != nil {
		return nil, fmt.Errorf("query settings: %w", err)
	}
	defer rows.Close()

	settings := make(map[string]SystemSetting)
	for rows.Next() {
		var s SystemSetting
		if err := rows.Scan(&s.Key, &s.Value, &s.Version, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		settings[s.Key] = s
	}

	return settings, rows.Err()
}

// GetSetting retrieves a single setting by key.
func (e *Engine) GetSetting(ctx context.Context, key string) (*SystemSetting, error) {
	var s SystemSetting
	err := e.db.QueryRowContext(ctx,
		`SELECT key, value, version, updated_at FROM system_settings WHERE key = ?`, key,
	).Scan(&s.Key, &s.Value, &s.Version, &s.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrSettingNotFound
		}
		return nil, fmt.Errorf("query setting: %w", err)
	}

	return &s, nil
}

// SetSetting updates a setting by key with optimistic concurrency via version stamp.
// The currentVersion parameter is the version the caller believes is active.
// It returns an error if the setting was modified by another operation since then.
func (e *Engine) SetSetting(ctx context.Context, key, value string, currentVersion int) error {
	var newVersion int
	err := e.InTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE system_settings SET value = ?, version = version + 1, updated_at = datetime('now')
			 WHERE key = ? AND version = ?`,
			value, key, currentVersion,
		)
		if err != nil {
			return fmt.Errorf("update setting: %w", err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("setting %q was modified by another operation", key)
		}

		err = tx.QueryRowContext(ctx,
			`SELECT version FROM system_settings WHERE key = ?`, key,
		).Scan(&newVersion)
		return err
	})

	return err
}

// UpsertSetting inserts a new setting or updates an existing one.
// Unlike SetSetting, this does not enforce optimistic concurrency on insert.
func (e *Engine) UpsertSetting(ctx context.Context, key, value string) error {
	return e.InTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO system_settings (key, value, version, updated_at)
			 VALUES (?, ?, 1, datetime('now'))
			 ON CONFLICT(key) DO UPDATE SET
				 value = excluded.value,
				 version = version + 1,
				 updated_at = datetime('now')`,
			key, value,
		)
		return err
	})
}

// DeleteSetting removes a setting by key.
func (e *Engine) DeleteSetting(ctx context.Context, key string) error {
	result, err := e.db.ExecContext(ctx,
		`DELETE FROM system_settings WHERE key = ?`, key,
	)
	if err != nil {
		return fmt.Errorf("delete setting: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrSettingNotFound
	}

	return nil
}

// Reload refreshes the settings by reloading them from the database.
// Callers should use LoadAllSettings to get the refreshed values.
func (e *Engine) Reload() error {
	return e.Validate()
}

// ErrSettingNotFound is returned when a setting does not exist.
var ErrSettingNotFound = fmt.Errorf("system setting not found")
