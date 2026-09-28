// Package concurrency provides the Prefetcher, Semaphore, and WorkerPool that
// form the job execution backbone of MediaCruncher.
//
// Prefetcher — Buffer between DB and workers:
//	The Prefetcher runs as a background goroutine that polls SQLite for pending
//	queue entries, leases them (state=pending → state=leased), and pushes them
//	into a bounded channel. This decouples the DB read loop from worker consumption,
//	preventing workers from blocking on I/O while the DB handles enqueues from
//	the filesystem scanner.
//
//	- Poll interval: 1 second (configurable via pollInterval)
//	- Batch size: 10 entries per poll
//	- Lease duration: 30 minutes (entries past this are recovered as orphans)
//	- Wake channel: external Trigger() wakes the loop immediately without waiting
//	  for the next poll tick (used when new files are enqueued by the scanner)
//
// Semaphore — Hardware-constrained concurrency:
//	A lightweight semaphore that limits concurrent goroutine access to a shared
//	resource (GPU encoder or CPU cores). Each worker acquires the semaphore before
//	starting a transcode job and releases it when complete. When full, workers
//	block until a slot frees up.
//
//	GPU semaphore: used when the selected preset uses hardware encoders (nvenc,
//	qsv, amf, vaapi). Limits concurrent GPU encodes to avoid exceeding VRAM.
//	CPU semaphore: used for software encoders (libx264, libx265, libsvtav1, etc.).
//	Limits concurrent CPU encodes based on available cores.
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
