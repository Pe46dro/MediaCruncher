package notification

import (
	"sync"
	"time"
)

// DeadLetterRecord represents a failed delivery in the dead-letter queue.
type DeadLetterRecord struct {
	ID          string    `json:"id"`
	Event       *Event    `json:"event"`
	Channel     string    `json:"channel"`
	AdapterName string    `json:"adapter_name"`
	LastError   string    `json:"last_error"`
	Delivery    *DeliveryResult `json:"delivery_result,omitempty"`
	RetryCount  int       `json:"retry_count"`
	MaxRetries  int       `json:"max_retries"`
	CreatedAt   time.Time `json:"created_at"`
	LastRetryAt time.Time `json:"last_retry_at"`
	FailedAt    time.Time `json:"failed_at"`
	Replayed    bool      `json:"replayed"`
}

// DeadLetterQueue holds failed deliveries for retry or manual intervention.
type DeadLetterQueue struct {
	mu           sync.Mutex
	records      map[string]*DeadLetterRecord
	maxSize      int
	maxRetries   int
	retention    time.Duration
	handlers     []func(*DeadLetterRecord)
}

// NewDeadLetterQueue creates a new dead-letter queue.
func NewDeadLetterQueue(maxSize int, maxRetries int, retention time.Duration) *DeadLetterQueue {
	if maxSize <= 0 {
		maxSize = 10000
	}
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	return &DeadLetterQueue{
		records:    make(map[string]*DeadLetterRecord),
		maxSize:    maxSize,
		maxRetries: maxRetries,
		retention:  retention,
	}
}

// Add records a failed delivery.
func (q *DeadLetterQueue) Add(evt *Event, channel string, adapterName string, delivery *DeliveryResult, retryCount int) *DeadLetterRecord {
	q.mu.Lock()
	defer q.mu.Unlock()

	record := &DeadLetterRecord{
		ID:          evt.ID + "-dlq-" + time.Now().Format("20060102150405"),
		Event:       evt,
		Channel:     channel,
		AdapterName: adapterName,
		LastError:   delivery.Error,
		Delivery:    delivery,
		RetryCount:  retryCount,
		MaxRetries:  q.maxRetries,
		CreatedAt:   time.Now(),
		LastRetryAt: time.Now(),
		FailedAt:    time.Now(),
		Replayed:    false,
	}

	if len(q.records) >= q.maxSize {
		q.pruneOldest()
	}

	q.records[record.ID] = record

	for _, handler := range q.handlers {
		handler(record)
	}

	return record
}

// Get returns a record by ID.
func (q *DeadLetterQueue) Get(id string) (*DeadLetterRecord, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	record, exists := q.records[id]
	return record, exists
}

// GetAll returns all records.
func (q *DeadLetterQueue) GetAll() []*DeadLetterRecord {
	q.mu.Lock()
	defer q.mu.Unlock()

	records := make([]*DeadLetterRecord, 0, len(q.records))
	for _, record := range q.records {
		records = append(records, record)
	}
	return records
}

// GetPending returns records that can be retried.
func (q *DeadLetterQueue) GetPending() []*DeadLetterRecord {
	q.mu.Lock()
	defer q.mu.Unlock()

	var pending []*DeadLetterRecord
	for _, record := range q.records {
		if record.RetryCount < record.MaxRetries && !record.Replayed {
			pending = append(pending, record)
		}
	}
	return pending
}

// MarkReplayed marks a record as replayed.
func (q *DeadLetterQueue) MarkReplayed(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	record, exists := q.records[id]
	if !exists {
		return false
	}

	record.Replayed = true
	return true
}

// Remove removes a record by ID.
func (q *DeadLetterQueue) Remove(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, exists := q.records[id]; !exists {
		return false
	}

	delete(q.records, id)
	return true
}

// Count returns the number of records in the queue.
func (q *DeadLetterQueue) Count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.records)
}

// PurgeOld removes records older than the retention period.
func (q *DeadLetterQueue) PurgeOld() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cutoff := time.Now().Add(-q.retention)
	purged := 0

	for id, record := range q.records {
		if record.CreatedAt.Before(cutoff) {
			delete(q.records, id)
			purged++
		}
	}

	return purged
}

// AddHandler adds a handler for new dead-letter records.
func (q *DeadLetterQueue) AddHandler(handler func(*DeadLetterRecord)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers = append(q.handlers, handler)
}

// Stats returns queue statistics.
func (q *DeadLetterQueue) Stats() map[string]interface{} {
	q.mu.Lock()
	defer q.mu.Unlock()

	replayed := 0
	pending := 0
	for _, record := range q.records {
		if record.Replayed {
			replayed++
		}
		if record.RetryCount < record.MaxRetries && !record.Replayed {
			pending++
		}
	}

	return map[string]interface{}{
		"total":    len(q.records),
		"pending":  pending,
		"replayed": replayed,
	}
}

// Empty checks if the queue is empty.
func (q *DeadLetterQueue) Empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.records) == 0
}

func (q *DeadLetterQueue) pruneOldest() {
	var oldestID string
	var oldestTime time.Time
	first := true

	for id, record := range q.records {
		if first || record.CreatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = record.CreatedAt
			first = false
		}
	}

	if oldestID != "" {
		delete(q.records, oldestID)
	}
}
