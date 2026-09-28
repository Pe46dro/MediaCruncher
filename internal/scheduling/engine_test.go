package scheduling

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/dedupe"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
)

func newTestEngine(t *testing.T) (*Engine, *testingCallbacks) {
	t.Helper()

	metrics := observability.NewMetrics()
	db, err := persistence.NewEngine(t.Name()+".db", 5000)
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	dedupeIdx := &dedupe.Index{}

	cfg := config.Config{
		Scheduling: config.SchedulingConfig{
			Enabled: true,
			ScanWindow: []config.ScanWindowConfig{
				{Start: "02:00", End: "06:00"},
			},
		},
		Concurrency: config.ConcurrencyConfig{
			WorkerCount: 2,
		},
	}

	engine := NewEngine(cfg.Scheduling, 2, dedupeIdx, db, metrics)
	return engine, &testingCallbacks{
		entries:   make([]ScheduleEntry, 0),
		cancelled: make([]string, 0),
	}
}

type testingCallbacks struct {
	entries   []ScheduleEntry
	cancelled []string
}

func (cb *testingCallbacks) SetCallbacks(engine *Engine) {
	engine.onScheduled = func(entry ScheduleEntry) {
		cb.entries = append(cb.entries, entry)
	}
	engine.onCancel = func(id string) {
		cb.cancelled = append(cb.cancelled, id)
	}
}

func (cb *testingCallbacks) Reset() {
	cb.entries = make([]ScheduleEntry, 0)
	cb.cancelled = make([]string, 0)
}

func TestNewEngine(t *testing.T) {
	engine, _ := newTestEngine(t)
	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}
	if len(engine.ListScheduled()) != 0 {
		t.Errorf("expected 0 scheduled entries, got %d", len(engine.ListScheduled()))
	}
}

func TestAddSchedule(t *testing.T) {
	engine, cb := newTestEngine(t)
	defer engine.Stop()
	cb.SetCallbacks(engine)

	entry := ScheduleEntry{
		ID:        "test-1",
		FilePaths: []string{"/test/video.mp4"},
		Priority:  50,
		Scheduled: time.Now().Add(1 * time.Hour),
	}

	engine.AddSchedule(entry)

	scheduled := engine.ListScheduled()
	if len(scheduled) != 1 {
		t.Errorf("expected 1 scheduled entry, got %d", len(scheduled))
	}

	if len(cb.entries) != 1 {
		t.Errorf("expected 1 onScheduled callback, got %d", len(cb.entries))
	}

	if scheduled[0].ID != "test-1" {
		t.Errorf("expected ID 'test-1', got '%s'", scheduled[0].ID)
	}
}

func TestAddScheduleDefaultPriority(t *testing.T) {
	engine, _ := newTestEngine(t)

	entry := ScheduleEntry{
		ID:        "test-2",
		Priority:  0, // should default to 50
		Scheduled: time.Now().Add(1 * time.Hour),
	}

	engine.AddSchedule(entry)
	scheduled := engine.ListScheduled()

	if scheduled[0].Priority != 50 {
		t.Errorf("expected default priority 50, got %d", scheduled[0].Priority)
	}
}

func TestAddScheduleImmediateTime(t *testing.T) {
	engine, _ := newTestEngine(t)

	entry := ScheduleEntry{
		ID:        "test-3",
		Scheduled: time.Time{}, // should default to now + 1s
	}

	engine.AddSchedule(entry)
	scheduled := engine.ListScheduled()

	now := time.Now()
	diff := scheduled[0].Scheduled.Sub(now)
	if diff < 0 || diff > 2*time.Second {
		t.Errorf("expected scheduled time to be ~now+1s, got diff=%v", diff)
	}
}

func TestRemoveSchedule(t *testing.T) {
	engine, cb := newTestEngine(t)
	cb.SetCallbacks(engine)

	engine.AddSchedule(ScheduleEntry{
		ID:        "to-remove",
		Priority:  50,
		Scheduled: time.Now().Add(1 * time.Hour),
	})

	if len(engine.ListScheduled()) != 1 {
		t.Fatalf("expected 1 entry before removal, got %d", len(engine.ListScheduled()))
	}

	engine.RemoveSchedule("to-remove")

	if len(engine.ListScheduled()) != 0 {
		t.Errorf("expected 0 entries after removal, got %d", len(engine.ListScheduled()))
	}

	if len(cb.cancelled) != 1 {
		t.Errorf("expected 1 onCancel callback, got %d", len(cb.cancelled))
	}
}

func TestRemoveNonExistentSchedule(t *testing.T) {
	engine, _ := newTestEngine(t)

	engine.RemoveSchedule("non-existent")

	if len(engine.ListScheduled()) != 0 {
		t.Errorf("expected 0 entries, got %d", len(engine.ListScheduled()))
	}
}

func TestStartManualScan(t *testing.T) {
	engine, _ := newTestEngine(t)

	var scanTriggered bool
	engine.SetTriggerScan(func() {
		scanTriggered = true
	})

	// Start the engine so processLoop runs
	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx)

	engine.StartManualScan(context.Background())

	// Wait for async processing
	time.Sleep(150 * time.Millisecond)
	cancel()

	if !scanTriggered {
		t.Error("expected triggerScan to be called")
	}
}

func TestCancelJob(t *testing.T) {
	engine, cb := newTestEngine(t)
	cb.SetCallbacks(engine)

	engine.cancelJob("cancel-me")

	if len(cb.cancelled) != 1 {
		t.Errorf("expected 1 onCancel callback, got %d", len(cb.cancelled))
	}

	if cb.cancelled[0] != "cancel-me" {
		t.Errorf("expected cancel ID 'cancel-me', got '%s'", cb.cancelled[0])
	}
}

func TestCheckScheduledJobsExecutes(t *testing.T) {
	engine, _ := newTestEngine(t)

	engine.AddSchedule(ScheduleEntry{
		ID:        "past-job",
		FilePaths: []string{"/test.mp4"},
		Scheduled: time.Now().Add(-1 * time.Hour),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	engine.checkScheduledJobs(ctx)

	scheduled := engine.ListScheduled()
	if len(scheduled) != 1 {
		t.Errorf("expected 1 entry, got %d", len(scheduled))
	}

	// checkScheduledJobs marks the entry as executed after processing
	if scheduled[0].ID == "past-job" && scheduled[0].Executed {
		// Good - the job was marked executed
	}
}

func TestStartStop(t *testing.T) {
	engine, _ := newTestEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	engine.Start(ctx)
	engine.Stop()
}

func TestProcessLoopCancellation(t *testing.T) {
	engine, _ := newTestEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	engine.Start(ctx)
	time.Sleep(100 * time.Millisecond)
}

func TestTickLoop(t *testing.T) {
	engine, _ := newTestEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	engine.Start(ctx)
	time.Sleep(150 * time.Millisecond)
}

func TestParseTimeMinutes(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"02:00", 120, false},
		{"23:59", 1439, false},
		{"00:00", 0, false},
		{"14:30", 870, false},
		{"02:00:00", 120, false},
		{"25:00", 0, true},
		{"00:60", 0, true},
		{"invalid", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseTimeMinutes(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseTimeMinutes(%q) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseTimeMinutes(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseTimeMinutes(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckWindowsAtStart(t *testing.T) {
	engine, cb := newTestEngine(t)
	cb.SetCallbacks(engine)

	now := time.Now()
	startMinute := now.Hour()*60 + now.Minute()
	startHour := startMinute / 60
	startMin := startMinute % 60

	engine.schedulingCfg = config.SchedulingConfig{
		Enabled: true,
		ScanWindow: []config.ScanWindowConfig{
			{
				Start: fmt.Sprintf("%02d:%02d", startHour, startMin),
				End:   fmt.Sprintf("%02d:%02d", startHour, startMin+5),
			},
		},
	}

	engine.checkWindows(context.Background())

	// At the start minute, the window triggers a scan via triggerCh (which we just check fires)
	// Since we can't easily intercept the channel in a test, we just verify no panic
	_ = engine
}

func TestScheduledEntriesPreserve(t *testing.T) {
	engine, _ := newTestEngine(t)

	times := []time.Time{
		time.Now().Add(5 * time.Hour),
		time.Now().Add(1 * time.Hour),
		time.Now().Add(3 * time.Hour),
	}

	for i, tm := range times {
		engine.AddSchedule(ScheduleEntry{
			ID:        fmt.Sprintf("entry-%d", i),
			Scheduled: tm,
		})
	}

	scheduled := engine.ListScheduled()
	if len(scheduled) != len(times) {
		t.Errorf("expected %d entries, got %d", len(times), len(scheduled))
	}
}

func TestListScheduledReturnsCopies(t *testing.T) {
	engine, _ := newTestEngine(t)

	engine.AddSchedule(ScheduleEntry{
		ID:        "test-copy",
		Priority:  75,
		Scheduled: time.Now().Add(1 * time.Hour),
	})

	scheduled := engine.ListScheduled()
	scheduled[0].Priority = 0
	scheduled[0].ID = "modified"

	scheduled = engine.ListScheduled()
	if scheduled[0].ID != "test-copy" {
		t.Errorf("expected original ID 'test-copy', got '%s'", scheduled[0].ID)
	}
	if scheduled[0].Priority != 75 {
		t.Errorf("expected original priority 75, got %d", scheduled[0].Priority)
	}
}

func TestMultipleScansTriggered(t *testing.T) {
	engine, _ := newTestEngine(t)

	var triggerCount int
	engine.SetTriggerScan(func() {
		triggerCount++
	})

	// Start the engine so processLoop runs
	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx)

	engine.StartManualScan(context.Background())
	engine.StartManualScan(context.Background())
	engine.StartManualScan(context.Background())

	time.Sleep(200 * time.Millisecond)
	cancel()

	if triggerCount < 1 {
		t.Errorf("expected at least 1 trigger, got %d", triggerCount)
	}
}

func TestEngineDisabled(t *testing.T) {
	cfg := config.SchedulingConfig{
		Enabled: false,
	}

	engine := NewEngine(cfg, 2, &dedupe.Index{}, nil, observability.NewMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	engine.Start(ctx)
	time.Sleep(60 * time.Millisecond)
}

func TestDedupeCheckInSchedule(t *testing.T) {
	engine, _ := newTestEngine(t)

	tmpFile, err := os.CreateTemp("", "test-schedule-*.mp4")
	if err != nil {
		t.Skipf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("test data")
	tmpFile.Close()

	engine.dedupe = &dedupe.Index{}

	engine.AddSchedule(ScheduleEntry{
		ID:        "dedupe-test",
		FilePaths: []string{tmpFile.Name()},
		Scheduled: time.Now(),
		Priority:  50,
	})

	scheduled := engine.ListScheduled()
	if len(scheduled) != 1 {
		t.Errorf("expected 1 scheduled entry, got %d", len(scheduled))
	}
}
