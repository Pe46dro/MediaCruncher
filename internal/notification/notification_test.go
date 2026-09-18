package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/persistence"
)

func TestGenericWebhookAdapterWithHMAC(t *testing.T) {
	secret := "super-secret-key"
	receivedHeader := ""
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("X-Signature-SHA256")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	chCfg := config.ChannelConfig{
		Type:   "webhook",
		Target: server.URL,
		Token:  secret,
	}

	adapter := NewGenericWebhookAdapter(chCfg, server.Client())

	payload := NotificationPayload{
		EventID:   "evt-123",
		EventType: "job_completed",
		Title:     "Movie Transcoded",
		Message:   "Saved 500 MB",
		Timestamp: time.Now().UTC(),
	}

	ctx := context.Background()
	if err := adapter.Send(ctx, payload); err != nil {
		t.Fatalf("webhook send failed: %v", err)
	}

	// Verify HMAC signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(receivedBody)
	expectedSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if receivedHeader != expectedSig {
		t.Fatalf("HMAC signature mismatch: expected %s, got %s", expectedSig, receivedHeader)
	}
}

func TestDiscordAdapterEmbed(t *testing.T) {
	var bodyRead []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyRead, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	chCfg := config.ChannelConfig{
		Type:   "discord",
		Target: server.URL,
	}

	adapter := NewDiscordAdapter(chCfg, server.Client())
	payload := NotificationPayload{
		EventID:     "evt-discord",
		EventType:   "job_completed",
		Title:       "Test Movie Transcode",
		Message:     "Finished in 45s",
		SavedBytes:  1024 * 1024 * 250,
		AverageVMAF: 95.5,
		Timestamp:   time.Now().UTC(),
	}

	if err := adapter.Send(context.Background(), payload); err != nil {
		t.Fatalf("discord send failed: %v", err)
	}

	if len(bodyRead) == 0 {
		t.Fatal("expected non-empty discord request body")
	}
}

func TestNotificationEngineBatchAndDLQ(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "notif_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "notif.db")
	db, err := persistence.NewEngine(dbPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create a failing server to trigger DLQ
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failServer.Close()

	cfg := config.NotificationConfig{
		Enabled:      true,
		BatchWindow:  100 * time.Millisecond,
		BatchMaxSize: 2,
		Channels: []config.ChannelConfig{
			{
				Type:   "webhook",
				Target: failServer.URL,
				Token:  "test-token",
			},
		},
	}

	engine := NewEngine(cfg, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine.Start(ctx)

	// Dispatch 2 events to reach BatchMaxSize immediately
	engine.Dispatch("job_completed", map[string]any{
		"saved_bytes": int64(1024 * 1024 * 100),
		"vmaf":        94.2,
	})
	engine.Dispatch("job_completed", map[string]any{
		"saved_bytes": int64(1024 * 1024 * 200),
		"vmaf":        96.1,
	})

	// Wait for batch processing and flush
	time.Sleep(300 * time.Millisecond)
	engine.Stop()

	// Verify dead letter queue recorded the failure
	var dlqCount int
	err = db.ReadDB().QueryRow("SELECT COUNT(*) FROM dead_letter_queue").Scan(&dlqCount)
	if err != nil {
		t.Fatal(err)
	}
	if dlqCount == 0 {
		t.Errorf("expected dead letter queue entry for failed notification, got %d", dlqCount)
	}
}
