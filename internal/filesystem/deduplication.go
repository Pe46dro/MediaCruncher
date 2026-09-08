package filesystem

import (
	"sync"
)

// DeduplicationEngine manages an in-memory hash index for deduplication.
type DeduplicationEngine struct {
	mu       sync.RWMutex
	indices  map[string]bool // hash -> exists
	policy   DedupPolicy
}

// NewDeduplicationEngine creates a new deduplication engine.
func NewDeduplicationEngine(policy DedupPolicy) *DeduplicationEngine {
	return &DeduplicationEngine{
		indices: make(map[string]bool),
		policy:  policy,
	}
}

// Check checks if a file with the given hash already exists in the index.
func (d *DeduplicationEngine) Check(hash string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.indices[hash]
}

// Add adds a hash to the index.
func (d *DeduplicationEngine) Add(hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.indices[hash] = true
}

// Count returns the number of entries in the index.
func (d *DeduplicationEngine) Count() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.indices)
}

// Reset clears the deduplication index.
func (d *DeduplicationEngine) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.indices = make(map[string]bool)
}
