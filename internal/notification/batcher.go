package notification

import (
	"sync"
	"time"
)

// BatcherConfig holds configuration for the batcher.
type BatcherConfig struct {
	MaxSize    int
	MaxAge     time.Duration
	FlushInterval time.Duration
}

// DefaultBatcherConfig returns a standard batcher configuration.
func DefaultBatcherConfig() *BatcherConfig {
	return &BatcherConfig{
		MaxSize:       50,
		MaxAge:        30 * time.Second,
		FlushInterval: 10 * time.Second,
	}
}

// MessageBatch holds a batch of messages for a channel.
type MessageBatch struct {
	Channel    string
	Messages   []*Message
	CreatedAt  time.Time
	LastFlush  time.Time
	MaxSize    int
	MaxAge     time.Duration
	mu         sync.Mutex
	flushCh    chan *MessageBatch
}

// NewMessageBatch creates a new message batch.
func NewMessageBatch(channel string, cfg *BatcherConfig) *MessageBatch {
	if cfg == nil {
		cfg = DefaultBatcherConfig()
	}
	return &MessageBatch{
		Channel:   channel,
		Messages:  make([]*Message, 0),
		CreatedAt: time.Now(),
		LastFlush: time.Now(),
		MaxSize:   cfg.MaxSize,
		MaxAge:    cfg.MaxAge,
		flushCh:   make(chan *MessageBatch, 1),
	}
}

// Add adds a message to the batch.
func (b *MessageBatch) Add(msg *Message) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.Messages) >= b.MaxSize {
		return false
	}

	b.Messages = append(b.Messages, msg)
	b.LastFlush = time.Now()

	select {
	case b.flushCh <- b:
	default:
	}

	return true
}

// IsFull returns true if the batch has reached its size limit.
func (b *MessageBatch) IsFull() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.Messages) >= b.MaxSize
}

// IsExpired returns true if the batch has exceeded its age limit.
func (b *MessageBatch) IsExpired() bool {
	return time.Since(b.CreatedAt) > b.MaxAge
}

// Flush returns all messages and clears the batch.
func (b *MessageBatch) Flush() []*Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	messages := b.Messages
	b.Messages = make([]*Message, 0)
	b.LastFlush = time.Now()

	return messages
}

// MessageCount returns the current batch message count.
func (b *MessageBatch) MessageCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.Messages)
}
// Batcher manages message batches for multiple channels.
type Batcher struct {
	mu       sync.Mutex
	batches  map[string]*MessageBatch
	cfg      *BatcherConfig
	handlers map[string]func([]*Message)
}

// NewBatcher creates a new message batcher.
func NewBatcher(cfg *BatcherConfig) *Batcher {
	if cfg == nil {
		cfg = DefaultBatcherConfig()
	}
	return &Batcher{
		batches:  make(map[string]*MessageBatch),
		cfg:      cfg,
		handlers: make(map[string]func([]*Message)),
	}
}

// SetHandler sets the delivery handler for a channel.
func (b *Batcher) SetHandler(channel string, handler func([]*Message)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[channel] = handler
}

// GetOrCreateBatch returns or creates a batch for a channel.
func (b *Batcher) GetOrCreateBatch(channel string) *MessageBatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	if batch, exists := b.batches[channel]; exists {
		return batch
	}

	batch := NewMessageBatch(channel, b.cfg)
	b.batches[channel] = batch
	return batch
}

// AddMessage adds a message to a batch and checks if it should be flushed.
func (b *Batcher) AddMessage(channel string, msg *Message) ([]*Message, bool) {
	batch := b.GetOrCreateBatch(channel)

	batch.mu.Lock()
	if batch.IsFull() || batch.IsExpired() {
		messages := batch.Flush()
		batch.mu.Unlock()
		return messages, true
	}

	batch.Add(msg)
	batch.mu.Unlock()
	return nil, false
}

// FlushBatch forcefully flushes a channel's batch.
func (b *Batcher) FlushBatch(channel string) []*Message {
	b.mu.Lock()
	batch, exists := b.batches[channel]
	b.mu.Unlock()

	if !exists {
		return nil
	}

	return batch.Flush()
}

// FlushAll flushes all pending batches.
func (b *Batcher) FlushAll() map[string][]*Message {
	b.mu.Lock()
	result := make(map[string][]*Message)
	for channel, batch := range b.batches {
		messages := batch.Flush()
		if len(messages) > 0 {
			result[channel] = messages
		}
	}
	b.batches = make(map[string]*MessageBatch)
	b.mu.Unlock()
	return result
}

// GetBatch returns a batch for a channel.
func (b *Batcher) GetBatch(channel string) *MessageBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.batches[channel]
}

// CountBatches returns the number of active batches.
func (b *Batcher) CountBatches() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.batches)
}
