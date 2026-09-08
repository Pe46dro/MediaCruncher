package notification

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mediacruncher/internal/observability"
)

// Engine is the main notification engine.
type Engine struct {
	adapters        map[string]Adapter
	router          *Router
	batcher         *Batcher
	limiter         *Limiter
	deadLetterQueue *DeadLetterQueue
	logger          *observability.Logger
	config          *EngineConfig
	mu              sync.Mutex
	started         bool
	stopped         bool
	batchTimer      *time.Ticker
	drainTimer      *time.Timer
	totalSent       int64
	totalFailed     int64
	totalBatched    int64
}

// EngineConfig holds configuration for the notification engine.
type EngineConfig struct {
	Batcher      *BatcherConfig
	RateLimit    *RateLimitConfig
	DeadLetter   *DeadLetterConfig
	Logger       *observability.Logger
}

// DeadLetterConfig holds configuration for the dead-letter queue.
type DeadLetterConfig struct {
	MaxSize   int
	MaxRetries int
	Retention time.Duration
}

// DefaultDeadLetterConfig returns a standard dead-letter configuration.
func DefaultDeadLetterConfig() *DeadLetterConfig {
	return &DeadLetterConfig{
		MaxSize:    10000,
		MaxRetries: 3,
		Retention:  7 * 24 * time.Hour,
	}
}

// New creates a new notification engine.
func New(cfg *EngineConfig) *Engine {
	if cfg == nil {
		cfg = &EngineConfig{}
	}

	engine := &Engine{
		adapters: make(map[string]Adapter),
		router:   NewRouter(),
		batcher:  NewBatcher(cfg.Batcher),
		limiter:  NewLimiter(cfg.RateLimit),
		logger:   cfg.Logger,
		config:   cfg,
	}

	if cfg.DeadLetter == nil {
		cfg.DeadLetter = DefaultDeadLetterConfig()
	}
	engine.deadLetterQueue = NewDeadLetterQueue(cfg.DeadLetter.MaxSize, cfg.DeadLetter.MaxRetries, cfg.DeadLetter.Retention)

	return engine
}

// RegisterAdapter registers a channel adapter.
func (e *Engine) RegisterAdapter(name string, adapter Adapter) error {
	if err := adapter.Validate(); err != nil {
		return fmt.Errorf("validate adapter %s: %w", name, err)
	}

	e.mu.Lock()
	e.adapters[name] = adapter
	e.router.AddChannel(name)
	e.mu.Unlock()

	if e.logger != nil {
		e.logger.Info(fmt.Sprintf("registered adapter: %s (type=%s)", name, adapter.GetType()))
	}

	return nil
}

// Send delivers an event to configured channels.
func (e *Engine) Send(evt *Event) error {
	routes := e.router.Route(evt)

	for _, route := range routes {
		if err := e.deliverToChannel(evt, route); err != nil {
			if e.logger != nil {
				e.logger.Error(fmt.Sprintf("delivery failed for %s: %v", evt.ID, err))
			}
		}
	}

	return nil
}

// SendImmediate delivers an event immediately, bypassing batching.
func (e *Engine) SendImmediate(evt *Event) error {
	immediateChannels := e.router.GetImmediateChannels(evt)

	for _, channel := range immediateChannels {
		adapter, ok := e.adapters[channel]
		if !ok {
			continue
		}

		msg := BuildMessage(evt)
		msg.WithChannel(channel)

		result, err := adapter.Send(msg)
		if err != nil {
			e.recordFailure(evt, channel, adapter.GetName(), result, err)
			continue
		}

		if !result.Success {
			e.recordFailure(evt, channel, adapter.GetName(), result, fmt.Errorf("delivery returned error status: %d", result.StatusCode))
			continue
		}

		e.limiter.Record(channel)
		e.mu.Lock()
		e.totalSent++
		e.mu.Unlock()
	}

	return nil
}

// Start launches the notification engine's background tasks.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.stopped = false
	e.mu.Unlock()

	e.batchTimer = time.NewTicker(10 * time.Second)
	go e.batchDispatcher(ctx)
}

// Stop gracefully shuts down the notification engine.
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	e.mu.Unlock()

	if e.batchTimer != nil {
		e.batchTimer.Stop()
	}

	flushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	e.flushPendingBatches(flushCtx)

	e.mu.Lock()
	e.started = false
	e.mu.Unlock()

	return nil
}

// IsRunning returns true if the engine is running.
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started && !e.stopped
}

// GetAdapters returns all registered adapters.
func (e *Engine) GetAdapters() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	names := make([]string, 0, len(e.adapters))
	for name := range e.adapters {
		names = append(names, name)
	}
	return names
}

// GetStats returns engine statistics.
func (e *Engine) GetStats() map[string]interface{} {
	e.mu.Lock()
	defer e.mu.Unlock()

	return map[string]interface{}{
		"running":     e.started && !e.stopped,
		"stopped":     e.stopped,
		"total_sent":  e.totalSent,
		"total_failed": e.totalFailed,
		"total_batched": e.totalBatched,
		"adapters":    len(e.adapters),
		"batcher_batches": e.batcher.CountBatches(),
		"dlq_count":   e.deadLetterQueue.Count(),
		"dlq_stats":   e.deadLetterQueue.Stats(),
	}
}

// deliverToChannel delivers an event to a specific channel.
func (e *Engine) deliverToChannel(evt *Event, route ChannelRoute) error {
	adapter, ok := e.adapters[route.Channel]
	if !ok {
		return fmt.Errorf("adapter not found: %s", route.Channel)
	}

	if !e.limiter.Allow(route.Channel) {
		waitTime := e.limiter.Wait(route.Channel)
		if e.logger != nil {
			e.logger.Warn(fmt.Sprintf("rate limited on %s, waiting %v", route.Channel, waitTime))
		}
		time.Sleep(waitTime)
	}

	msg := BuildMessage(evt)
	msg.WithChannel(route.Channel)

	if route.Batched && !evt.IsImmediate() {
		messages, flushed := e.batcher.AddMessage(route.Channel, msg)
		if flushed && len(messages) > 0 {
			e.flushBatch(route.Channel, adapter, messages)
		}
		return nil
	}

	return e.sendDirect(route.Channel, adapter, msg)
}

// sendDirect sends a message directly to an adapter.
func (e *Engine) sendDirect(channel string, adapter Adapter, msg *Message) error {
	result, err := adapter.Send(msg)

	if err != nil {
		e.recordFailure(msg.Event, channel, adapter.GetName(), result, err)
		return err
	}

	if !result.Success {
		e.recordFailure(msg.Event, channel, adapter.GetName(), result, fmt.Errorf("delivery failed with status %d", result.StatusCode))
		return fmt.Errorf("delivery failed with status %d", result.StatusCode)
	}

	e.limiter.Record(channel)
	e.mu.Lock()
	e.totalSent++
	e.mu.Unlock()

	return nil
}

// flushBatch sends a batch of messages to an adapter.
func (e *Engine) flushBatch(channel string, adapter Adapter, messages []*Message) {
	e.mu.Lock()
	e.totalBatched += int64(len(messages))
	e.mu.Unlock()

	results, err := adapter.SendBatch(messages)
	if err != nil {
		failEvent := &Event{
			ID:        fmt.Sprintf("batch-fail-%d", time.Now().UnixNano()),
			Type:      EventTypeSystemError,
			Severity:  SeverityError,
			Timestamp: time.Now(),
		}
		failEvent.Context = map[string]interface{}{
			"channel": channel,
			"error":   err.Error(),
		}
		e.recordFailure(failEvent, channel, adapter.GetName(), nil, err)
		return
	}

	for _, result := range results {
		if result.Success {
			e.limiter.Record(channel)
			e.mu.Lock()
			e.totalSent++
			e.mu.Unlock()
		} else {
			failEvent := &Event{
				ID:        fmt.Sprintf("batch-delivery-fail-%d", time.Now().UnixNano()),
				Type:      EventTypeSystemError,
				Severity:  SeverityError,
				Timestamp: time.Now(),
			}
			failEvent.Context = map[string]interface{}{
				"channel": channel,
				"error":   result.Error,
			}
			e.recordFailure(failEvent, channel, adapter.GetName(), result, fmt.Errorf("batch delivery failed: %s", result.Error))
		}
	}
}

// recordFailure records a delivery failure to the dead-letter queue.
func (e *Engine) recordFailure(evt *Event, channel string, adapterName string, delivery *DeliveryResult, err error) {
	e.mu.Lock()
	e.totalFailed++
	e.mu.Unlock()

	if delivery == nil {
		delivery = &DeliveryResult{
			Success:   false,
			Error:     err.Error(),
			Timestamp: time.Now(),
		}
	}

	evt.RetryCount++
	e.deadLetterQueue.Add(evt, channel, adapterName, delivery, evt.RetryCount)

	if e.logger != nil {
		e.logger.Error(fmt.Sprintf("delivery failed: event=%s, channel=%s, error=%v", evt.ID, channel, err))
	}
}

// batchDispatcher periodically flushes pending batches.
func (e *Engine) batchDispatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.batchTimer.C:
			e.flushExpiredBatches()
		}
	}
}

// flushExpiredBatches flushes all expired batches.
func (e *Engine) flushExpiredBatches() {
	batches := e.batcher.FlushAll()
	for channel, messages := range batches {
		adapter, ok := e.adapters[channel]
		if !ok {
			continue
		}
		e.flushBatch(channel, adapter, messages)
	}
}

// flushPendingBatches flushes all pending batches with a context deadline.
func (e *Engine) flushPendingBatches(ctx context.Context) {
	select {
	case <-ctx.Done():
		batches := e.batcher.FlushAll()
		for channel, messages := range batches {
			adapter, ok := e.adapters[channel]
			if !ok {
				continue
			}
			e.flushBatch(channel, adapter, messages)
		}
	case <-time.After(5 * time.Second):
		batches := e.batcher.FlushAll()
		for channel, messages := range batches {
			adapter, ok := e.adapters[channel]
			if !ok {
				continue
			}
			e.flushBatch(channel, adapter, messages)
		}
	}
}

// AddRoute adds a routing rule.
func (e *Engine) AddRoute(rule RoutingRule) {
	e.router.AddRule(rule)
}

// DeadLetterSnapshot is a read-only snapshot of dead-letter queue entries.
type DeadLetterSnapshot struct {
	Records map[string]*DeadLetterRecord
}

// GetDeadLetterQueue returns a read-only snapshot of the dead-letter queue.
func (e *Engine) GetDeadLetterQueue() DeadLetterSnapshot {
	e.deadLetterQueue.mu.Lock()
	defer e.deadLetterQueue.mu.Unlock()

	snapshot := DeadLetterSnapshot{
		Records: make(map[string]*DeadLetterRecord, len(e.deadLetterQueue.records)),
	}
	for k, v := range e.deadLetterQueue.records {
		recCopy := *v
		snapshot.Records[k] = &recCopy
	}
	return snapshot
}
