package concurrency

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/transcoder"
)

func TestSemaphore(t *testing.T) {
	sem := NewSemaphore(2)

	if sem.Available() != 2 || sem.InUse() != 0 {
		t.Fatalf("expected 2 available, got %d", sem.Available())
	}

	ctx := context.Background()
	if err := sem.Acquire(ctx); err != nil {
		t.Fatal(err)
	}

	if sem.Available() != 1 || sem.InUse() != 1 {
		t.Fatalf("expected 1 available, 1 in use; got %d, %d", sem.Available(), sem.InUse())
	}

	if !sem.TryAcquire() {
		t.Fatalf("expected TryAcquire to succeed for second slot")
	}

	if sem.TryAcquire() {
		t.Fatalf("expected TryAcquire to fail when full")
	}

	// Test cancellation when blocked
	timeoutCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	err := sem.Acquire(timeoutCtx)
	if err == nil {
		t.Fatalf("expected acquisition to fail with timeout")
	}

	sem.Release()
	if sem.Available() != 1 {
		t.Fatalf("expected 1 available after release, got %d", sem.Available())
	}

	sem.Release()
	if sem.Available() != 2 {
		t.Fatalf("expected 2 available after second release, got %d", sem.Available())
	}
}

func TestPrefetcher(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "prefetch_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "prefetch.db")
	engine, err := persistence.NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Enqueue 3 test files
	for i := 1; i <= 3; i++ {
		_, err := engine.Enqueue(filepath.Join(tmpDir, "file"+string(rune('0'+i))+".mp4"), 10)
		if err != nil {
			t.Fatal(err)
		}
	}

	prefetcher := NewPrefetcher(engine, "test-worker", 10, 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prefetcher.Start(ctx)
	prefetcher.Trigger()

	received := 0
	timeout := time.After(3 * time.Second)

	for received < 3 {
		select {
		case entry := <-prefetcher.Queue():
			if entry == nil {
				t.Fatal("received nil entry")
			}
			received++
		case <-timeout:
			t.Fatalf("timed out waiting for prefetch; received %d of 3", received)
		}
	}

	prefetcher.Stop()
}

func TestWorkerPoolExecution(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available in test environment")
	}

	tmpDir, err := os.MkdirTemp("", "workerpool_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test video
	srcFile := filepath.Join(tmpDir, "sample.mp4")
	genCmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=duration=1:frequency=440",
		"-c:v", "libx264", "-c:a", "aac",
		srcFile,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create test clip: %v, out: %s", err, string(out))
	}

	dbPath := filepath.Join(tmpDir, "pool.db")
	db, err := persistence.NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	evalCfg := config.EvaluationConfig{
		DefaultAction: "transcode",
		DefaultPreset: "fast-test",
		Rules: []config.RuleConfig{
			{
				Name:       "Transcode All",
				Priority:   10,
				Action:     "transcode",
				Preset:     "fast-test",
				Conditions: map[string]string{},
			},
		},
	}
	evalPipeline := evaluation.NewPipeline(evalCfg)

	tcCfg := config.TranscoderConfig{
		HardwareAcceleration: "cpu",
		StagingDir:           filepath.Join(tmpDir, "staging"),
		VMAFEnabled:          false,
		SkipIfLarger:         false,
		Presets: []config.PresetConfig{
			{
				Name:        "fast-test",
				VideoCodec:  "h264",
				QualityCRF:  28,
				PresetSpeed: "ultrafast",
				AudioCodec:  "copy",
			},
		},
	}
	tc := transcoder.NewTranscoder(tcCfg, nil)

	concurrencyCfg := config.ConcurrencyConfig{
		WorkerCount:         2,
		GPUSemaphoreLimit:   1,
		CPUSemaphoreLimit:   2,
		PrefetchBufferDepth: 10,
		RetryMaxAttempts:    2,
		RetryBaseInterval:   100 * time.Millisecond,
	}

	events := make(chan string, 10)
	pool := NewWorkerPool(concurrencyCfg, db, evalPipeline, tc, func(event string, payload any) {
		events <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)

	// Enqueue test file
	qID, err := db.Enqueue(srcFile, 50)
	if err != nil {
		t.Fatal(err)
	}
	if qID == 0 {
		t.Fatal("expected positive queue ID")
	}

	pool.TriggerPrefetch()

	// Wait for completion event
	select {
	case ev := <-events:
		if ev != "job_completed" && ev != "job_skipped_growth" {
			t.Fatalf("unexpected event: %s", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for worker job execution")
	}

	// Verify DB state
	stats, err := db.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Stats: %+v", stats)

	pool.DrainAndStop(2 * time.Second)
}

func TestWorkerPoolPriorityOrdering(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "prio_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "prio.db")
	db, err := persistence.NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Enqueue job 1 with default priority 50
	id1, _ := db.Enqueue("/media/job1.mp4", 50)
	// Enqueue job 2 with default priority 50
	id2, _ := db.Enqueue("/media/job2.mp4", 50)
	// Enqueue job 3 with priority 10
	id3, _ := db.Enqueue("/media/job3.mp4", 10)

	// Now reprioritize job 2 to 100
	if err := db.SetJobPriority(id2, 100); err != nil {
		t.Fatalf("failed to reprioritize job 2: %v", err)
	}

	// First lease should pick job 2 (priority 100)
	batch1, err := db.LeaseBatch("w1", 1, 10*time.Minute)
	if err != nil || len(batch1) != 1 {
		t.Fatalf("expected 1 leased job, got %d, err: %v", len(batch1), err)
	}
	if batch1[0].ID != id2 {
		t.Fatalf("expected highest priority job (%d) to be leased first, got job %d (priority %d)", id2, batch1[0].ID, batch1[0].Priority)
	}

	// Second lease should pick job 1 (priority 50)
	batch2, err := db.LeaseBatch("w1", 1, 10*time.Minute)
	if err != nil || len(batch2) != 1 {
		t.Fatalf("expected 1 leased job, got %d, err: %v", len(batch2), err)
	}
	if batch2[0].ID != id1 {
		t.Fatalf("expected job 1 (priority 50) to be leased second, got job %d", batch2[0].ID)
	}

	// Third lease should pick job 3 (priority 10)
	batch3, err := db.LeaseBatch("w1", 1, 10*time.Minute)
	if err != nil || len(batch3) != 1 {
		t.Fatalf("expected 1 leased job, got %d, err: %v", len(batch3), err)
	}
	if batch3[0].ID != id3 {
		t.Fatalf("expected job 3 (priority 10) to be leased third, got job %d", batch3[0].ID)
	}
}
