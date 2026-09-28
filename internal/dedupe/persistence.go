package dedupe

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"
)

// LoadFromDB populates the index with all records from the file_signatures table.
// This allows cold-start recovery: files seen in previous runs are preloaded so they
// don't count as "discovered" again on startup.
func (idx *Index) LoadFromDB(db *sql.DB) error {
	rows, err := db.Query("SELECT path, size, first_mb_hash, last_mb_hash, first_seen, last_scanned FROM file_signatures")
	if err != nil {
		return fmt.Errorf("load dedupe index from db: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var path string
		var size int64
		var firstMBHashB64, lastMBHashB64 string
		var firstSeen, lastScanned string

		if err := rows.Scan(&path, &size, &firstMBHashB64, &lastMBHashB64, &firstSeen, &lastScanned); err != nil {
			return fmt.Errorf("scan dedupe record: %w", err)
		}

		firstHash, err := base64.StdEncoding.DecodeString(firstMBHashB64)
		if err != nil {
			continue // skip corrupt
		}
		lastHash, err := base64.StdEncoding.DecodeString(lastMBHashB64)
		if err != nil {
			continue
		}

		rec := &FileRecord{
			Path:         path,
			Size:         size,
			FirstMBHash:  firstHash,
			LastMBHash:   lastHash,
			FirstSeen:    firstSeen,
			LastScanned:  lastScanned,
		}
		idx.AddDiskRecord(rec)
		count++
	}

	return rows.Err()
}

// UpsertRecords inserts or updates a batch of file signatures.
// Uses INSERT OR REPLACE so that last_scanned is refreshed on every scan cycle.
func (idx *Index) UpsertRecords(db *sql.DB, records []UpsertRecord) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin dedupe upsert tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO file_signatures (path, size, first_mb_hash, last_mb_hash, first_seen, last_scanned)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare dedupe upsert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)

	for _, r := range records {
		firstMBB64 := base64.StdEncoding.EncodeToString(r.FirstMBHash)
		lastMBB64 := base64.StdEncoding.EncodeToString(r.LastMBHash)

		firstSeen := r.FirstSeen
		if firstSeen == "" {
			firstSeen = now
		}

		_, err := stmt.Exec(r.Path, r.Size, firstMBB64, lastMBB64, firstSeen, now)
		if err != nil {
			return fmt.Errorf("exec dedupe upsert for %s: %w", r.Path, err)
		}
	}

	return tx.Commit()
}

// GetRecordsSince returns all file signatures recorded since the given timestamp.
func (idx *Index) GetRecordsSince(db *sql.DB, since time.Time) ([]*FileRecord, error) {
	after := since.Format(time.RFC3339)

	rows, err := db.Query(`
		SELECT path, size, first_mb_hash, last_mb_hash, first_seen, last_scanned
		FROM file_signatures WHERE last_scanned > ?
		ORDER BY last_scanned DESC
	`, after)
	if err != nil {
		return nil, fmt.Errorf("query dedupe records since %s: %w", after, err)
	}
	defer rows.Close()

	var result []*FileRecord
	for rows.Next() {
		var path string
		var size int64
		var firstMBHashB64, lastMBHashB64 string
		var firstSeen, lastScanned string

		if err := rows.Scan(&path, &size, &firstMBHashB64, &lastMBHashB64, &firstSeen, &lastScanned); err != nil {
			return nil, fmt.Errorf("scan dedupe record: %w", err)
		}

		firstHash, _ := base64.StdEncoding.DecodeString(firstMBHashB64)
		lastHash, _ := base64.StdEncoding.DecodeString(lastMBHashB64)

		result = append(result, &FileRecord{
			Path:         path,
			Size:         size,
			FirstMBHash:  firstHash,
			LastMBHash:   lastHash,
			FirstSeen:    firstSeen,
			LastScanned:  lastScanned,
		})
	}

	return result, rows.Err()
}

// GetUniqueCount returns the total number of unique files recorded in the DB.
func (idx *Index) GetUniqueCount(db *sql.DB) (int64, error) {
	var count int64
	err := db.QueryRow("SELECT COUNT(*) FROM file_signatures").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count dedupe records: %w", err)
	}
	return count, nil
}

// DeleteRecords removes file signatures for a given set of paths.
func (idx *Index) DeleteRecords(db *sql.DB, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin dedupe delete tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("DELETE FROM file_signatures WHERE path = ?")
	if err != nil {
		return fmt.Errorf("prepare dedupe delete: %w", err)
	}
	defer stmt.Close()

	for _, p := range paths {
		_, err := stmt.Exec(p)
		if err != nil {
			return fmt.Errorf("delete dedupe record for %s: %w", p, err)
		}
	}

	return tx.Commit()
}

// Truncate removes all file signatures from the DB.
func (idx *Index) Truncate(db *sql.DB) error {
	_, err := db.Exec("DELETE FROM file_signatures")
	return err
}
