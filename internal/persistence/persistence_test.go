package persistence

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPersistenceEngineConcurrent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_persistence.db")

	engine, err := NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatalf("failed to initialize persistence engine: %v", err)
	}
	defer engine.Close()

	// Test concurrent enqueues from 10 workers
	var wg sync.WaitGroup
	numWorkers := 10
	filesPerWorker := 20

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			for f := 0; f < filesPerWorker; f++ {
				path := fmt.Sprintf("/media/video_w%d_f%d.mkv", workerID, f)
				_, err := engine.Enqueue(path, 50)
				if err != nil {
					t.Errorf("worker %d failed enqueue: %v", workerID, err)
				}
			}
		}()
	}
	wg.Wait()

	pending, _, _, _, err := engine.GetQueueCounts()
	if err != nil {
		t.Fatalf("failed to get queue counts: %v", err)
	}
	expectedTotal := int64(numWorkers * filesPerWorker)
	if pending != expectedTotal {
		t.Fatalf("expected %d pending entries, got %d", expectedTotal, pending)
	}

	// Test lease batch
	batch, err := engine.LeaseBatch("worker-1", 15, 10*time.Second)
	if err != nil {
		t.Fatalf("failed to lease batch: %v", err)
	}
	if len(batch) != 15 {
		t.Fatalf("expected 15 leased items, got %d", len(batch))
	}

	// Verify leased count
	pending, leased, _, _, err := engine.GetQueueCounts()
	if err != nil {
		t.Fatalf("failed to get counts: %v", err)
	}
	if pending != expectedTotal-15 || leased != 15 {
		t.Fatalf("unexpected counts: pending=%d, leased=%d", pending, leased)
	}

	// Save metadata for one item
	firstItem := batch[0]
	err = engine.SaveMetadata(&JobMetadata{
		QueueID:        firstItem.ID,
		VideoCodec:     "h264",
		Resolution:     "1920x1080",
		Bitrate:        5000000,
		Duration:       120.5,
		AudioTracks:    "aac",
		Subtitles:      "srt",
		DecisionAction: "transcode",
		Preset:         "balanced-hevc",
		StreamMapJSON:  `{"video":"transcode","audio":"copy"}`,
		NormalizedJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("failed to save metadata: %v", err)
	}

	meta, err := engine.GetMetadata(firstItem.ID)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}
	if meta.VideoCodec != "h264" || meta.DecisionAction != "transcode" {
		t.Fatalf("unexpected metadata content: %+v", meta)
	}

	// Complete the first item
	err = engine.UpdateJobState(firstItem.ID, StateCompleted, "")
	if err != nil {
		t.Fatalf("failed to update state: %v", err)
	}

	// Test Crash Recovery: Artificially reset lease expiration to the past
	_, err = engine.execWrite(func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec("UPDATE queue_entries SET lease_expires_at = datetime('now', '-10 minutes') WHERE state = ?", StateLeased)
		return nil, err
	})
	if err != nil {
		t.Fatalf("failed to expire lease: %v", err)
	}

	recovered, err := engine.CrashRecovery()
	if err != nil {
		t.Fatalf("crash recovery failed: %v", err)
	}
	if recovered != 14 { // 15 originally leased, 1 completed, so 14 expired
		t.Fatalf("expected 14 recovered entries, got %d", recovered)
	}

	// Test Audit Logging
	err = engine.LogAudit("job_completed", "info", `{"file":"test.mkv"}`)
	if err != nil {
		t.Fatalf("failed to log audit: %v", err)
	}
	logs, err := engine.GetRecentAuditLogs(5)
	if err != nil || len(logs) == 0 {
		t.Fatalf("failed to retrieve audit logs: %v", err)
	}
}
