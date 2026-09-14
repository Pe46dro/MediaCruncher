package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mediacruncher/internal/observability"
)

func TestWebServerEndpoints(t *testing.T) {
	broker := NewBroker()
	state := NewStateTracker()
	state.SetSystemInfo("NVENC", "h.265", 90.0, 10.0)

	var scanTriggered atomic.Bool
	triggerFunc := func() {
		scanTriggered.Store(true)
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
	if !scanTriggered.Load() {
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

	// 6. Test Pause & Resume APIs
	pauseReq := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
	pauseRec := httptest.NewRecorder()
	srv.handlePause(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/pause, got %d", pauseRec.Code)
	}
	if !state.IsPaused() {
		t.Error("expected state to be paused")
	}

	resumeReq := httptest.NewRequest(http.MethodPost, "/api/resume", nil)
	resumeRec := httptest.NewRecorder()
	srv.handleResume(resumeRec, resumeReq)
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/resume, got %d", resumeRec.Code)
	}
	if state.IsPaused() {
		t.Error("expected state to be resumed")
	}

	// 7. Test Cancel Job
	var cancelled atomic.Bool
	state.RegisterJobCancel("test-job-1", func() {
		cancelled.Store(true)
	})
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/jobs/cancel?job_id=test-job-1", nil)
	cancelRec := httptest.NewRecorder()
	srv.handleCancelJob(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/jobs/cancel, got %d", cancelRec.Code)
	}
	if !cancelled.Load() {
		t.Error("expected cancel callback to be invoked")
	}

	// 8. Test Queue Endpoint
	queueReq := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	queueRec := httptest.NewRecorder()
	srv.handleQueue(queueRec, queueReq)
	if queueRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/queue, got %d", queueRec.Code)
	}

	// Test graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}
