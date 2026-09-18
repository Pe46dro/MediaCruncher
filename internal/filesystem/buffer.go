package filesystem

import (
	"context"
	"time"
)

type FileRecord struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	Extension   string    `json:"extension"`
	IsDuplicate bool      `json:"is_duplicate"`
	Original    string    `json:"original,omitempty"`
	Warning     string    `json:"warning,omitempty"`
}

// IngestionBuffer acts as a bounded backpressure queue between filesystem discovery and worker queues.
type IngestionBuffer struct {
	records chan *FileRecord
}

func NewIngestionBuffer(capacity int) *IngestionBuffer {
	if capacity <= 0 {
		capacity = 5000
	}
	return &IngestionBuffer{
		records: make(chan *FileRecord, capacity),
	}
}

// Push pushes a record into the buffer, blocking if full to apply natural backpressure.
func (b *IngestionBuffer) Push(ctx context.Context, record *FileRecord) error {
	select {
	case b.records <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Records returns the read-only channel for consumer engines.
func (b *IngestionBuffer) Records() <-chan *FileRecord {
	return b.records
}

// Close closes the ingestion buffer channel.
func (b *IngestionBuffer) Close() {
	close(b.records)
}

// Len returns current buffer depth.
func (b *IngestionBuffer) Len() int {
	return len(b.records)
}
