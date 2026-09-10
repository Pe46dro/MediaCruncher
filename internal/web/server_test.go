package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mediacruncher/internal/observability"
)

func TestWebServerEndpoints(t *testing.T) {
	broker := NewBroker()
	state := NewStateTracker()
	state.SetSystemInfo("NVENC", "h.265", 90.0, 10.0)

	var scanTriggered bool
	triggerFunc := func() {
		scanTriggered = true
	}

	logger := observability.NewStdLogger(observability.DebugLevel, "test")

	srv := NewServer(Config{
		Addr:            "127.0.0.1:0",
		Broker:          broker,
		State:           state,
		Persistence:     nil,
		TriggerScanFunc: triggerFunc,
		Logger:          logger,
	})

	// 1. Test Index Page (embedded)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.handleIndex(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for index, got %d", rec.Code)
	}

	// 2. Test /api/status
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec = httptest.NewRecorder()
	srv.handleStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/status, got %d", rec.Code)
	}
	var statusData map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusData); err != nil {
		t.Fatalf("failed to unmarshal status json: %v", err)
	}
	if statusData["hardware_accel"] != "NVENC" {
		t.Errorf("expected NVENC, got %v", statusData["hardware_accel"])
	}

	// 3. Test Active Jobs
	state.AddActiveJob(&ActiveJob{
		JobID:       "test-job-1",
		SourcePath:  "/media/test.mp4",
		FileName:    "test.mp4",
		TargetCodec: "h.265",
		Stage:       "transcoding",
		StartTime:   time.Now(),
		WorkerID:    "w1",
	})
	req = httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec = httptest.NewRecorder()
	srv.handleJobs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/jobs, got %d", rec.Code)
	}
	var jobsData []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobsData); err != nil {
		t.Fatalf("failed to unmarshal jobs json: %v", err)
	}
	if len(jobsData) != 1 || jobsData[0]["job_id"] != "test-job-1" {
		t.Errorf("unexpected jobs data: %+v", jobsData)
	}

	// 4. Test Scan Trigger
	req = httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	rec = httptest.NewRecorder()
	srv.handleScanTrigger(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/scan, got %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if !scanTriggered {
		t.Error("expected scan trigger to be called")
	}

	// 5. Test SSE Broadcast
	ch, unsub := broker.Subscribe()
	defer unsub()

	broker.Broadcast("test_event", "info", "test message", map[string]string{"foo": "bar"})

	select {
	case msg := <-ch:
		if len(msg) == 0 {
			t.Error("expected non-empty SSE message")
		}
	case <-time.After(1 * time.Second):
		t.Error("timed out waiting for SSE message")
	}

	// Test graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}
