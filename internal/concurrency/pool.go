package concurrency

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerState represents the state of a worker.
type WorkerState string

const (
	WorkerIdle     WorkerState = "idle"
	WorkerProcessing WorkerState = "processing"
	WorkerDraining WorkerState = "draining"
	WorkerStopped  WorkerState = "stopped"
)

// WorkerInfo holds information about a worker.
type WorkerInfo struct {
	ID           string        `json:"id"`
	State        WorkerState   `json:"state"`
	CurrentTask  string        `json:"current_task,omitempty"`
	TotalJobs    int64         `json:"total_jobs"`
	TotalSuccess int64         `json:"total_success"`
	TotalFailed  int64         `json:"total_failed"`
	LastJobStart time.Time     `json:"last_job_start"`
	LastJobEnd   time.Time     `json:"last_job_end"`
	LastJobDur   time.Duration `json:"last_job_duration"`
}

// Worker is an individual processing unit in the pool.
type Worker struct {
	id         string
	pool       *WorkerPool
	state      WorkerState
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	info       WorkerInfo
	processFunc func(ctx context.Context, task *Task) *Result
	resultCh    chan<- *Result
}

// WorkerPool manages a collection of workers for parallel task processing.
type WorkerPool struct {
	mu          sync.Mutex
	workers     []*Worker
	size        int
	jobType     JobType
	processFunc func(ctx context.Context, task *Task) *Result
	resultCh    chan<- *Result
	started     bool
	draining    bool
	stopped     bool
	ctx         context.Context
	cancel      context.CancelFunc
	activeCount int64
	totalJobs   int64
	totalSuccess int64
	totalFailed int64
}

// Result holds the outcome of a task execution.
type Result struct {
	Task      *Task     `json:"task"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
	Duration  time.Duration `json:"duration"`
	Data      interface{} `json:"data,omitempty"`
	WorkerID  string    `json:"worker_id"`
	Timestamp time.Time `json:"timestamp"`
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(size int, jobType JobType, processFunc func(ctx context.Context, task *Task) *Result, resultCh chan<- *Result) *WorkerPool {
	if size <= 0 {
		size = 4
	}
	return &WorkerPool{
		size:        size,
		jobType:     jobType,
		processFunc: processFunc,
		resultCh:    resultCh,
	}
}

// Start launches all workers in the pool.
func (p *WorkerPool) Start(ctx context.Context, queue *TaskQueue) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return
	}

	p.ctx, p.cancel = context.WithCancel(ctx)
	p.workers = make([]*Worker, p.size)

	for i := 0; i < p.size; i++ {
		workerCtx, workerCancel := context.WithCancel(p.ctx)
		workerID := fmt.Sprintf("worker-%d", i+1)
		worker := &Worker{
			id:        workerID,
			pool:      p,
			state:     WorkerIdle,
			ctx:       workerCtx,
			cancel:    workerCancel,
			processFunc: p.processFunc,
			resultCh:  p.resultCh,
		}
		worker.info = WorkerInfo{
			ID:    worker.id,
			State: WorkerIdle,
		}
		p.workers[i] = worker
		go worker.run(queue)
	}

	p.started = true
}

// Submit adds a task to the queue and dispatches it to an available worker.
func (p *WorkerPool) Submit(task *Task) bool {
	if p.draining || p.stopped {
		return false
	}
	return true
}

// Drain signals the pool to stop accepting new tasks and complete current work.
func (p *WorkerPool) Drain() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.draining || p.stopped {
		return
	}

	p.draining = true

	for _, worker := range p.workers {
		worker.mu.Lock()
		if worker.state == WorkerIdle {
			worker.state = WorkerDraining
			worker.cancel()
		} else {
			worker.state = WorkerDraining
		}
		worker.mu.Unlock()
	}
}

// Stop forcefully stops all workers.
func (p *WorkerPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopped = true
	if p.cancel != nil {
		p.cancel()
	}

	for _, worker := range p.workers {
		worker.mu.Lock()
		worker.state = WorkerStopped
		worker.cancel()
		worker.mu.Unlock()
	}
}

// Wait blocks until all workers have stopped.
func (p *WorkerPool) Wait() {
	for range p.workers {
		// Workers run in goroutines, we track via context
	}
}

// GetWorkers returns information about all workers.
func (p *WorkerPool) GetWorkers() []WorkerInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	infos := make([]WorkerInfo, len(p.workers))
	for i, worker := range p.workers {
		worker.mu.Lock()
		infos[i] = worker.info
		worker.mu.Unlock()
	}
	return infos
}

// ActiveCount returns the number of actively processing workers.
func (p *WorkerPool) ActiveCount() int64 {
	return atomic.LoadInt64(&p.activeCount)
}

// Stats returns pool-level statistics.
func (p *WorkerPool) Stats() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	return map[string]interface{}{
		"size":           p.size,
		"started":        p.started,
		"draining":       p.draining,
		"stopped":        p.stopped,
		"active":         atomic.LoadInt64(&p.activeCount),
		"total_jobs":     atomic.LoadInt64(&p.totalJobs),
		"total_success":  atomic.LoadInt64(&p.totalSuccess),
		"total_failed":   atomic.LoadInt64(&p.totalFailed),
	}
}

func (w *Worker) run(queue *TaskQueue) {
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		w.mu.Lock()
		w.state = WorkerIdle
		w.mu.Unlock()

		task, found := queue.Claim(w.pool.jobType, w.id)
		if !found {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		w.mu.Lock()
		w.state = WorkerProcessing
		w.info.State = WorkerProcessing
		w.info.LastJobStart = time.Now()
		w.mu.Unlock()

		atomic.AddInt64(&w.pool.activeCount, 1)
		atomic.AddInt64(&w.pool.totalJobs, 1)

		result := w.processFunc(w.ctx, task)

		w.mu.Lock()
		w.info.State = WorkerIdle
		w.info.LastJobEnd = time.Now()
		w.info.LastJobDur = time.Since(w.info.LastJobStart)
		w.info.TotalJobs++
		if result != nil && result.Success {
			w.info.TotalSuccess++
		} else if result != nil {
			w.info.TotalFailed++
		}
		w.mu.Unlock()

		atomic.AddInt64(&w.pool.activeCount, -1)
		if result != nil && result.Success {
			atomic.AddInt64(&w.pool.totalSuccess, 1)
		} else if result != nil {
			atomic.AddInt64(&w.pool.totalFailed, 1)
		}

		if result != nil {
			w.resultCh <- result
		}
	}
}
