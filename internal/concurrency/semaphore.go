package concurrency

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Semaphore limits concurrent execution of resource-constrained tasks.
type Semaphore interface {
	Acquire(ctx context.Context) error
	Release()
	TryAcquire() bool
	Available() int
	InUse() int
}

type ChannelSemaphore struct {
	ch       chan struct{}
	limit    int
	acquired atomic.Int64
}

// NewSemaphore creates a counting semaphore with the given resource limit.
func NewSemaphore(limit int) *ChannelSemaphore {
	if limit <= 0 {
		limit = 1
	}
	return &ChannelSemaphore{
		ch:    make(chan struct{}, limit),
		limit: limit,
	}
}

// Acquire reserves a slot in the semaphore or blocks until available or ctx is canceled.
func (s *ChannelSemaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		s.acquired.Add(1)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("semaphore acquisition canceled: %w", ctx.Err())
	}
}

// Release frees an acquired slot in the semaphore.
func (s *ChannelSemaphore) Release() {
	select {
	case <-s.ch:
		s.acquired.Add(-1)
	default:
		// Prevent panic on over-release
	}
}

// TryAcquire attempts non-blocking slot acquisition.
func (s *ChannelSemaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		s.acquired.Add(1)
		return true
	default:
		return false
	}
}

// Available returns currently free slots.
func (s *ChannelSemaphore) Available() int {
	return s.limit - int(s.acquired.Load())
}

// InUse returns currently occupied slots.
func (s *ChannelSemaphore) InUse() int {
	return int(s.acquired.Load())
}
