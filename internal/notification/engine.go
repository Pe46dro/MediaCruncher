package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
)

type NotificationEvent struct {
	ID        string
	Type      string // job_completed, job_failed, job_skipped, etc.
	Payload   any
	Timestamp time.Time
}

type Engine struct {
	cfg        config.NotificationConfig
	db         *persistence.Engine
	adapters   []Adapter
	incomingCh chan NotificationEvent
	stopCh     chan struct{}
	wg         sync.WaitGroup
	running    atomic.Bool
	client     *http.Client
}

func NewEngine(cfg config.NotificationConfig, db *persistence.Engine) *Engine {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var adapters []Adapter
	for _, ch := range cfg.Channels {
		switch strings.ToLower(ch.Type) {
		case "discord":
			adapters = append(adapters, NewDiscordAdapter(ch, client))
		case "telegram":
			adapters = append(adapters, NewTelegramAdapter(ch, client))
		case "slack":
			adapters = append(adapters, NewSlackAdapter(ch, client))
		case "webhook":
			adapters = append(adapters, NewGenericWebhookAdapter(ch, client))
		case "gotify":
			adapters = append(adapters, NewGotifyAdapter(ch, client))
		case "smtp":
			adapters = append(adapters, NewSMTPAdapter(ch))
		}
	}

	return &Engine{
		cfg:        cfg,
		db:         db,
		adapters:   adapters,
		incomingCh: make(chan NotificationEvent, 500),
		stopCh:     make(chan struct{}),
		client:     client,
	}
}

// Start begins the event dispatching and batching background loop.
func (e *Engine) Start(ctx context.Context) {
	if !e.cfg.Enabled || len(e.adapters) == 0 {
		return
	}
	if !e.running.CompareAndSwap(false, true) {
		return
	}

	e.wg.Add(1)
	go e.dispatcherLoop(ctx)
}

// Stop terminates the dispatcher loop and flushes pending events.
func (e *Engine) Stop() {
	if !e.running.CompareAndSwap(true, false) {
		return
	}
	close(e.stopCh)
	e.wg.Wait()
}

// Dispatch submits an event to the notification processing queue.
func (e *Engine) Dispatch(eventType string, payload any) {
	if !e.cfg.Enabled || len(e.adapters) == 0 {
		return
	}

	evt := NotificationEvent{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Type:      eventType,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}

	select {
	case e.incomingCh <- evt:
	default:
		slog.Warn("Notification queue full; dropping event", "type", eventType)
	}
}

func (e *Engine) dispatcherLoop(ctx context.Context) {
	defer e.wg.Done()

	batchWindow := e.cfg.BatchWindow
	if batchWindow <= 0 {
		batchWindow = 10 * time.Second
	}
	batchMaxSize := e.cfg.BatchMaxSize
	if batchMaxSize <= 0 {
		batchMaxSize = 10
	}

	var batch []NotificationEvent
	ticker := time.NewTicker(batchWindow)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.flushBatch(ctx, batch)
			return
		case <-e.stopCh:
			e.flushBatch(context.Background(), batch)
			return
		case evt := <-e.incomingCh:
			batch = append(batch, evt)
			if len(batch) >= batchMaxSize {
				e.flushBatch(ctx, batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				e.flushBatch(ctx, batch)
				batch = nil
			}
		}
	}
}

func (e *Engine) flushBatch(ctx context.Context, batch []NotificationEvent) {
	if len(batch) == 0 {
		return
	}

	// Aggregate metrics across the batch
	var totalSaved int64
	var vmafSum float64
	var vmafCount int
	completedCount := 0
	failedCount := 0

	for _, evt := range batch {
		if evt.Type == "job_completed" {
			completedCount++
			if m, ok := evt.Payload.(map[string]any); ok {
				if saved, ok := m["saved_bytes"].(int64); ok {
					totalSaved += saved
				}
				if vmaf, ok := m["vmaf"].(float64); ok && vmaf > 0 {
					vmafSum += vmaf
					vmafCount++
				}
			}
		} else if evt.Type == "job_failed" {
			failedCount++
		}
	}

	avgVMAF := 0.0
	if vmafCount > 0 {
		avgVMAF = vmafSum / float64(vmafCount)
	}

	payload := NotificationPayload{
		EventID:     fmt.Sprintf("batch-%d", time.Now().UnixNano()),
		EventType:   "batch_summary",
		Title:       fmt.Sprintf("Transcode Batch: %d processed", len(batch)),
		Message:     fmt.Sprintf("Batch finished: %d completed, %d failed. Saved: %s", completedCount, failedCount, formatBytes(totalSaved)),
		Timestamp:   time.Now().UTC(),
		TotalItems:  len(batch),
		SavedBytes:  totalSaved,
		AverageVMAF: avgVMAF,
	}

	// Dispatch to all configured adapters
	for _, adapter := range e.adapters {
		if !adapter.SupportsEvent(payload.EventType) {
			continue
		}

		err := adapter.Send(ctx, payload)
		payloadJSON, _ := json.Marshal(payload)

		if err != nil {
			slog.Error("Failed to deliver notification", "channel", adapter.Name(), "err", err)
			_ = e.db.SaveDeadLetter(string(payloadJSON), err.Error())
			_ = e.db.SaveDeliveryRecord(payload.EventID, adapter.Name(), "failed", err.Error())
		} else {
			_ = e.db.SaveDeliveryRecord(payload.EventID, adapter.Name(), "delivered", "OK")
			observability.GetMetrics().NotificationsSent.Add(1)
		}
	}
}
