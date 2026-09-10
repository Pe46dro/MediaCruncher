package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
)

func setupTestDB(t *testing.T) (*Engine, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "mc_db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "test_mediacruncher.db")

	cfg := &config.PersistenceSettings{
		DatabasePath:  dbPath,
		Synchronous:   "normal",
		MigrationAuto: true,
	}
	logger := observability.NewStdLogger(observability.DebugLevel, "test")
	engine := New(cfg, logger, nil)
	if engine == nil {
		t.Fatal("failed to initialize persistence engine")
	}

	cleanup := func() {
		engine.Close()
		os.RemoveAll(tmpDir)
	}
	return engine, cleanup
}

func TestProcessedMediaPersistence(t *testing.T) {
	engine, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Verify schema migration v2
	if err := engine.Validate(); err != nil {
		t.Fatalf("database schema validation failed: %v", err)
	}

	hash1 := "test-hash-video-1"
	rec1 := &ProcessedMediaRecord{
		SourcePath: "/media/video1.mp4",
		FileHash:   hash1,
		FileSize:   1000000,
		Status:     "completed",
		OutputPath:       "/media/video1_optimized.mp4",
		VMAFScore:        94.5,
		DurationMs:       1200,
		ReplacedOriginal: true,
	}

	// 1. Initial query should be nil
	found, err := engine.GetProcessedMediaByHash(ctx, hash1)
	if err != nil {
		t.Fatalf("query before insert error: %v", err)
	}
	if found != nil {
		t.Fatalf("expected nil, got %+v", found)
	}

	// 2. Record media
	if err := engine.RecordProcessedMedia(ctx, rec1); err != nil {
		t.Fatalf("record processed media error: %v", err)
	}

	// 3. Query after insert
	found, err = engine.GetProcessedMediaByHash(ctx, hash1)
	if err != nil {
		t.Fatalf("query after insert error: %v", err)
	}
	if found == nil {
		t.Fatal("expected found record, got nil")
	}
	if found.SourcePath != rec1.SourcePath || found.Status != "completed" || found.VMAFScore != 94.5 || !found.ReplacedOriginal {
		t.Fatalf("record data mismatch: %+v", found)
	}

	// 4. Update (idempotent conflict update)
	rec1.Status = "skipped_quality"
	rec1.ErrorMessage = "quality drop too high"
	if err := engine.RecordProcessedMedia(ctx, rec1); err != nil {
		t.Fatalf("update processed media error: %v", err)
	}
	updated, err := engine.GetProcessedMediaByHash(ctx, hash1)
	if err != nil {
		t.Fatalf("query updated error: %v", err)
	}
	if updated.Status != "skipped_quality" || updated.ErrorMessage != "quality drop too high" {
		t.Fatalf("expected updated record, got %+v", updated)
	}

	// 5. List and stats
	list, total, err := engine.ListProcessedMedia(ctx, 10, 0, "")
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("expected 1 record, got total %d, len %d", total, len(list))
	}

	stats, err := engine.GetProcessedStats(ctx)
	if err != nil {
		t.Fatalf("stats error: %v", err)
	}
	if stats.TotalFiles != 1 || stats.SkippedQuality != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
