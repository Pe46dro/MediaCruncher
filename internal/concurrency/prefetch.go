package concurrency

import (
	"context"
	"sync"
	"time"

	"mediacruncher/internal/persistence"
)

// Prefetcher continuously leases batches of tasks from SQLite and feeds them into a bounded channel.
type Prefetcher struct {
	db            *persistence.Engine
	workerID      string
	batchSize     int
	leaseDuration time.Duration
	pollInterval  time.Duration
	outCh         chan *persistence.QueueEntry
	wakeCh        chan struct{}
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

func NewPrefetcher(db *persistence.Engine, workerID string, bufferDepth int, leaseDuration time.Duration) *Prefetcher {
	if bufferDepth <= 0 {
		bufferDepth = 50
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Minute
	}

	return &Prefetcher{
		db:            db,
		workerID:      workerID,
		batchSize:     10,
		leaseDuration: leaseDuration,
		pollInterval:  1 * time.Second,
		outCh:         make(chan *persistence.QueueEntry, bufferDepth),
		wakeCh:        make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
	}
}

// Queue returns the readable channel of pre-leased jobs.
func (p *Prefetcher) Queue() <-chan *persistence.QueueEntry {
	return p.outCh
}

// Trigger wakes the prefetch loop immediately to check for pending jobs.
func (p *Prefetcher) Trigger() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

// Start begins background prefetch polling and lease recovery.
func (p *Prefetcher) Start(ctx context.Context) {
	// Immediate initial orphan recovery on boot
	_, _ = p.db.ResetInFlightJobs()

	p.wg.Add(1)
	go p.loop(ctx)
}

// Stop terminates prefetching and waits for loop exit.
func (p *Prefetcher) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

func (p *Prefetcher) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.pollInterval)
	orphanTicker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()
	defer orphanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-orphanTicker.C:
			_, _ = p.db.RecoverOrphanedLeases()
		case <-p.wakeCh:
			p.fetchBatch(ctx)
		case <-ticker.C:
			p.fetchBatch(ctx)
		}
	}
}

func (p *Prefetcher) fetchBatch(ctx context.Context) {
	available := cap(p.outCh) - len(p.outCh)
	if available <= 0 {
		return
	}

	batchLimit := p.batchSize
	if batchLimit > available {
		batchLimit = available
	}

	entries, err := p.db.LeaseBatch(p.workerID, batchLimit, p.leaseDuration)
	if err != nil || len(entries) == 0 {
		return
	}

	for _, entry := range entries {
		select {
		case p.outCh <- entry:
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		}
	}
}
