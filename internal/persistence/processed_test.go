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

	// 6. Test skipped_quality filter matches skipped_quality
	qList, qTotal, err := engine.ListProcessedMedia(ctx, 10, 0, "skipped_quality")
	if err != nil {
		t.Fatalf("list skipped_quality error: %v", err)
	}
	if qTotal != 1 || len(qList) != 1 {
		t.Fatalf("expected 1 skipped_quality record, got total %d, len %d", qTotal, len(qList))
	}

	// 7. Add skipped_larger record and check both are returned by skipped_quality filter
	rec2 := &ProcessedMediaRecord{
		SourcePath: "/media/video2.mp4",
		FileHash:   "test-hash-video-2",
		FileSize:   2000000,
		Status:     "skipped_larger",
	}
	if err := engine.RecordProcessedMedia(ctx, rec2); err != nil {
		t.Fatalf("record rec2 error: %v", err)
	}
	qList, qTotal, err = engine.ListProcessedMedia(ctx, 10, 0, "skipped_quality")
	if err != nil {
		t.Fatalf("list skipped_quality with skipped_larger error: %v", err)
	}
	if qTotal != 2 || len(qList) != 2 {
		t.Fatalf("expected 2 records for skipped_quality filter, got total %d, len %d", qTotal, len(qList))
	}

	stats, err := engine.GetProcessedStats(ctx)
	if err != nil {
		t.Fatalf("stats error: %v", err)
	}
	if stats.TotalFiles != 2 || stats.SkippedQuality != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestQueueTransitionsAndReset(t *testing.T) {
	engine, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Enqueue two items
	id1, err := engine.Enqueue(ctx, "/media/video1.mp4", PriorityNormal)
	if err != nil {
		t.Fatalf("enqueue id1 failed: %v", err)
	}
	id2, err := engine.Enqueue(ctx, "/media/video2.mp4", PriorityNormal)
	if err != nil {
		t.Fatalf("enqueue id2 failed: %v", err)
	}

	depth, err := engine.GetPendingDepth(ctx)
	if err != nil {
		t.Fatalf("get pending depth error: %v", err)
	}
	if depth != 2 {
		t.Errorf("expected pending depth 2, got %d", depth)
	}

	// 2. Set item 1 to processing
	if err := engine.SetProcessing(ctx, id1, "worker-1"); err != nil {
		t.Fatalf("set processing failed: %v", err)
	}
	depth, _ = engine.GetPendingDepth(ctx)
	if depth != 1 {
		t.Errorf("expected pending depth 1 after processing, got %d", depth)
	}

	// 3. Complete item 1 from processing
	if err := engine.Complete(ctx, id1, 0); err != nil {
		t.Fatalf("complete id1 failed: %v", err)
	}

	// 4. Complete item 2 directly from pending (e.g. ignored or copied)
	if err := engine.Complete(ctx, id2, 0); err != nil {
		t.Fatalf("complete id2 failed: %v", err)
	}

	// 5. Pending queue MUST now be 0!
	depth, _ = engine.GetPendingDepth(ctx)
	if depth != 0 {
		t.Errorf("expected pending depth 0 after completing all items, got %d", depth)
	}

	// 6. Test CleanupStaleQueue
	id3, _ := engine.Enqueue(ctx, "/media/already_done.mp4", PriorityNormal)
	_ = engine.RecordProcessedMedia(ctx, &ProcessedMediaRecord{
		SourcePath: "/media/already_done.mp4",
		FileHash:   "hash-already-done",
		FileSize:   5000,
		Status:     "completed",
	})
	cleaned, err := engine.CleanupStaleQueue(ctx)
	if err != nil {
		t.Fatalf("cleanup stale queue failed: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("expected 1 cleaned item, got %d", cleaned)
	}

	entry3, err := engine.GetEntry(ctx, id3)
	if err != nil {
		t.Fatalf("get entry error: %v", err)
	}
	if entry3.State != StateCompleted {
		t.Errorf("expected entry3 state completed, got %s", entry3.State)
	}

	depth, _ = engine.GetPendingDepth(ctx)
	if depth != 0 {
		t.Errorf("expected pending depth 0, got %d", depth)
	}
}

func TestProcessedStatsSavingsCalculation(t *testing.T) {
	engine, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// File 1: 10MB -> 4MB (saved 6MB)
	_ = engine.RecordProcessedMedia(ctx, &ProcessedMediaRecord{
		SourcePath: "/media/video1.mp4",
		FileHash:   "hash-v1",
		FileSize:   10000000,
		OutputSize: 4000000,
		Status:     "completed",
		VMAFScore:  95.0,
	})

	// File 2: 5MB -> 2MB (saved 3MB)
	_ = engine.RecordProcessedMedia(ctx, &ProcessedMediaRecord{
		SourcePath: "/media/video2.mp4",
		FileHash:   "hash-v2",
		FileSize:   5000000,
		OutputSize: 2000000,
		Status:     "completed",
		VMAFScore:  93.0,
	})

	// File 3: 1MB (skipped_larger, preserved original)
	_ = engine.RecordProcessedMedia(ctx, &ProcessedMediaRecord{
		SourcePath: "/media/video3.mp4",
		FileHash:   "hash-v3",
		FileSize:   1000000,
		OutputSize: 1000000,
		Status:     "skipped_larger",
	})

	stats, err := engine.GetProcessedStats(ctx)
	if err != nil {
		t.Fatalf("get processed stats failed: %v", err)
	}

	if stats.TotalFiles != 3 {
		t.Errorf("expected total files 3, got %d", stats.TotalFiles)
	}
	if stats.Completed != 2 {
		t.Errorf("expected completed 2, got %d", stats.Completed)
	}
	if stats.SkippedQuality != 1 {
		t.Errorf("expected skipped quality 1, got %d", stats.SkippedQuality)
	}
	if stats.TotalOriginalBytes != 15000000 {
		t.Errorf("expected total original bytes 15000000, got %d", stats.TotalOriginalBytes)
	}
	if stats.TotalTranscodedBytes != 6000000 {
		t.Errorf("expected total transcoded bytes 6000000, got %d", stats.TotalTranscodedBytes)
	}
	if stats.BytesSaved != 9000000 {
		t.Errorf("expected bytes saved 9000000, got %d", stats.BytesSaved)
	}
	if stats.SavingsPercentage != 60.0 {
		t.Errorf("expected savings percentage 60.0, got %f", stats.SavingsPercentage)
	}
	if stats.AvgVMAF != 94.0 {
		t.Errorf("expected avg vmaf 94.0, got %f", stats.AvgVMAF)
	}
}
