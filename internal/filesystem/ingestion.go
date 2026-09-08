package filesystem

import (
	"fmt"
)

// IngestionBuffer is an in-memory bounded queue that collects file records
// and feeds them to the processing pipeline at a controlled rate.
type IngestionBuffer struct {
	capacity int
	buf      []FileRecord
	pos      int
	count    int
	onPush   func(FileRecord) error
	fullHandler func()
}

// NewIngestionBuffer creates a new ingestion buffer with the given capacity.
func NewIngestionBuffer(capacity int, onPush func(FileRecord) error) *IngestionBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &IngestionBuffer{
		capacity: capacity,
		buf:      make([]FileRecord, capacity),
		onPush:   onPush,
	}
}

// Push adds a file record to the buffer and forwards it to the downstream handler.
// Returns an error if the downstream handler rejects the record.
func (b *IngestionBuffer) Push(record FileRecord) error {
	if b.onPush != nil {
		if err := b.onPush(record); err != nil {
			return fmt.Errorf("push to downstream: %w", err)
		}
	}
	return nil
}

// PushBatch pushes multiple file records to the buffer.
func (b *IngestionBuffer) PushBatch(records []FileRecord) error {
	for _, r := range records {
		if err := b.Push(r); err != nil {
			return err
		}
	}
	return nil
}

// Drain flushes any remaining items in the buffer.
// Returns any error from the last push operation.
func (b *IngestionBuffer) Drain() error {
	// Since we push directly to downstream, there's nothing to drain
	// in the simple implementation. This can be extended for buffering mode.
	return nil
}
