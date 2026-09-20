package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
)

func TestServerEndpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "server_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := persistence.NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	metrics := observability.NewMetrics()
	var scanned atomic.Bool
	s := NewServer(0, cfgMgr, db, metrics, func() {
		scanned.Store(true)
	})

	handler := s.httpServer.Handler

	// 1. Test Static Index.html
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("MediaCruncher Dashboard")) {
		t.Errorf("expected dashboard title in HTML, got %s", w.Body.String()[:100])
	}

	// 2. Test Static CSS
	reqCSS := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	wCSS := httptest.NewRecorder()
	handler.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /static/style.css, got %d", wCSS.Code)
	}

	// 3. Test API Status
	reqStatus := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	wStatus := httptest.NewRecorder()
	handler.ServeHTTP(wStatus, reqStatus)
	if wStatus.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/status, got %d", wStatus.Code)
	}
	var statusMap map[string]any
	if err := json.Unmarshal(wStatus.Body.Bytes(), &statusMap); err != nil {
		t.Fatalf("failed to decode /api/status JSON: %v", err)
	}
	if _, ok := statusMap["queue"]; !ok {
		t.Errorf("expected 'queue' field in status response")
	}

	// 4. Test API Queue
	reqQueue := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	wQueue := httptest.NewRecorder()
	handler.ServeHTTP(wQueue, reqQueue)
	if wQueue.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/queue, got %d", wQueue.Code)
	}
	var queueResp map[string]any
	if err := json.Unmarshal(wQueue.Body.Bytes(), &queueResp); err != nil {
		t.Fatalf("failed to decode /api/queue JSON: %v", err)
	}
	if _, ok := queueResp["active_progress"]; !ok {
		t.Errorf("expected 'active_progress' field in /api/queue response")
	}

	// 5. Test API Config GET & POST
	reqGetCfg := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	wGetCfg := httptest.NewRecorder()
	handler.ServeHTTP(wGetCfg, reqGetCfg)
	if wGetCfg.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET /api/config, got %d", wGetCfg.Code)
	}

	curCfg := cfgMgr.Get()
	curCfg.Concurrency.WorkerCount = 8
	data, _ := json.Marshal(curCfg)
	reqPostCfg := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(data))
	wPostCfg := httptest.NewRecorder()
	handler.ServeHTTP(wPostCfg, reqPostCfg)
	if wPostCfg.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST /api/config, got %d", wPostCfg.Code)
	}
	if cfgMgr.Get().Concurrency.WorkerCount != 8 {
		t.Errorf("expected WorkerCount updated to 8, got %d", cfgMgr.Get().Concurrency.WorkerCount)
	}

	// 6. Test API Hardware
	reqHW := httptest.NewRequest(http.MethodGet, "/api/hardware", nil)
	wHW := httptest.NewRecorder()
	handler.ServeHTTP(wHW, reqHW)
	if wHW.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/hardware, got %d", wHW.Code)
	}

	// 7. Test API Scan Trigger
	reqScan := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	wScan := httptest.NewRecorder()
	handler.ServeHTTP(wScan, reqScan)
	if wScan.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for /api/scan, got %d", wScan.Code)
	}
	// Wait brief moment for background goroutine to execute
	deadline := time.Now().Add(1 * time.Second)
	for !scanned.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !scanned.Load() {
		t.Errorf("expected scanned callback to be invoked")
	}

	_ = s.Shutdown(context.Background())
}
