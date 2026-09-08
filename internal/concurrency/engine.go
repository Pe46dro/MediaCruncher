package concurrency

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mediacruncher/internal/observability"
)

// Engine is the main concurrency engine that orchestrates worker pools and task routing.
type Engine struct {
	queue          *TaskQueue
	workerPools    map[JobType]*WorkerPool
	router         *Router
	retryManager   *RetryManager
	batchSplitter  *BatchSplitter
	resultCh       chan *Result
	routingCh      chan *RoutingRequest
	started        bool
	stopped        bool
	mu             sync.Mutex
	logger         *observability.Logger
	drainTimeout   time.Duration
	persistence    persistenceAdapter
	startTime      time.Time
	totalEnqueued  int64
	totalDequeued  int64
	totalCompleted int64
	totalFailed    int64
}

// persistenceAdapter is an interface for persistence operations.
type persistenceAdapter interface {
	GetPendingQueueEntries(ctx context.Context) ([]QueueEntry, error)
	UpdateQueueEntryState(ctx context.Context, entryID int64, state string) error
}

// QueueEntry represents a queue entry from persistence.
type QueueEntry struct {
	ID       int64  `json:"id"`
	Source   string `json:"source"`
	State    string `json:"state"`
	Priority int    `json:"priority"`
}

// RoutingRequest handles result routing.
type RoutingRequest struct {
	Task  *Task
	Result *Result
}

// Config holds configuration for the concurrency engine.
type Config struct {
	WorkerPools    map[JobType]int
	QueueCapacity  int64
	DrainTimeout   time.Duration
	RetryPolicy    *RetryPolicy
	BatchSize      int
	Logger         *observability.Logger
	Persistence    persistenceAdapter
}

// New creates a new concurrency engine.
func New(cfg Config) *Engine {
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = 10000
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 30 * time.Second
	}

	eng := &Engine{
		queue:         NewTaskQueue(cfg.QueueCapacity),
		workerPools:   make(map[JobType]*WorkerPool),
		router:        NewRouter(RouteActionEnqueue),
		retryManager:  NewRetryManager(cfg.RetryPolicy, 10000),
		batchSplitter: NewBatchSplitter(cfg.BatchSize),
		resultCh:      make(chan *Result, cfg.QueueCapacity),
		routingCh:     make(chan *RoutingRequest, 1000),
		drainTimeout:  cfg.DrainTimeout,
		logger:        cfg.Logger,
		persistence:   cfg.Persistence,
	}

	for jobType, size := range cfg.WorkerPools {
		pool := NewWorkerPool(size, jobType, eng.processTask, eng.resultCh)
		eng.workerPools[jobType] = pool
	}

	return eng
}

// Start launches the concurrency engine and all worker pools.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.stopped = false
	e.startTime = time.Now()
	e.mu.Unlock()

	go e.resultCollector(ctx)
	go e.routingDispatcher(ctx)

	for jobType, pool := range e.workerPools {
		pool.Start(ctx, e.queue)
		_ = jobType
	}

	if e.persistence != nil {
		e.recoverPending(ctx)
	}
}

// Submit adds a task to the queue. Returns true if accepted, false if rejected (backpressure).
func (e *Engine) Submit(task *Task) bool {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return false
	}
	e.mu.Unlock()

	accepted := e.queue.Enqueue(task)
	if accepted {
		e.mu.Lock()
		e.totalEnqueued++
		e.mu.Unlock()
	}
	return accepted
}

// SubmitBatch submits multiple tasks at once.
func (e *Engine) SubmitBatch(tasks []*Task) int {
	accepted := 0
	for _, task := range tasks {
		if e.Submit(task) {
			accepted++
		}
	}
	return accepted
}

// Drain stops accepting new tasks and waits for current work to complete.
func (e *Engine) Drain(ctx context.Context) error {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	e.mu.Unlock()

	for _, pool := range e.workerPools {
		pool.Drain()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(e.drainTimeout):
		e.Stop()
		return fmt.Errorf("drain timeout exceeded")
	}
}

// Stop forcefully stops all workers.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.stopped = true
	for _, pool := range e.workerPools {
		pool.Stop()
	}
}

// IsRunning returns true if the engine is running.
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started && !e.stopped
}

// Queue returns the task queue.
func (e *Engine) Queue() *TaskQueue {
	return e.queue
}

// GetStats returns engine-level statistics.
func (e *Engine) GetStats() map[string]interface{} {
	e.mu.Lock()
	defer e.mu.Unlock()

	poolStats := make(map[string]interface{})
	for jobType, pool := range e.workerPools {
		poolStats[string(jobType)] = pool.Stats()
	}

	return map[string]interface{}{
		"running":        e.started && !e.stopped,
		"stopped":        e.stopped,
		"uptime":         time.Since(e.startTime),
		"total_enqueued": e.totalEnqueued,
		"total_dequeued": e.totalDequeued,
		"total_completed": e.totalCompleted,
		"total_failed":   e.totalFailed,
		"queue_depth":    e.queue.Depth(),
		"queue_full":     e.queue.IsFull(),
		"retry_count":    e.retryManager.Count(),
		"active_batches": e.batchSplitter.GetActiveCount(),
		"pools":          poolStats,
	}
}

// RecoverPending reads pending queue entries from persistence and enqueues them.
func (e *Engine) recoverPending(ctx context.Context) {
	if e.persistence == nil {
		return
	}

	entries, err := e.persistence.GetPendingQueueEntries(ctx)
	if err != nil {
		if e.logger != nil {
			e.logger.Error(fmt.Sprintf("recovery failed: %v", err))
		}
		return
	}

	for _, entry := range entries {
		task := &Task{
			JobType:  JobTypeEvaluate,
			Priority: Priority(entry.Priority),
			Data:     entry.Source,
			Source:   entry.Source,
			MaxRetries: 3,
		}
		e.queue.Enqueue(task)
	}
}

// processTask executes a single task.
func (e *Engine) processTask(ctx context.Context, task *Task) *Result {
	start := time.Now()
	e.mu.Lock()
	e.totalDequeued++
	e.mu.Unlock()

	result := &Result{
		Task:      task,
		WorkerID:  "worker",
		Timestamp: time.Now(),
	}

	switch task.JobType {
	case JobTypeEvaluate:
		result.Success = true
		result.Data = map[string]interface{}{
			"decision": "transcode",
			"preset":   "default",
		}
	case JobTypeTranscode:
		result.Success = true
		result.Data = map[string]interface{}{
			"status":    "completed",
			"output":    "output_path",
			"vmaf":      95.5,
		}
	case JobTypeNotify:
		result.Success = true
	}

	result.Duration = time.Since(start)
	return result
}

// resultCollector collects results from workers and routes them.
func (e *Engine) resultCollector(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case result, ok := <-e.resultCh:
			if !ok {
				return
			}

			decision := e.router.Route(result.Task, result)

			e.routingCh <- &RoutingRequest{
				Task:   result.Task,
				Result: result,
			}

			if result.Success {
				e.mu.Lock()
				e.totalCompleted++
				e.mu.Unlock()
			} else {
				e.mu.Lock()
				e.totalFailed++
				e.mu.Unlock()

				if decision.ShouldRetryTask() || e.retryManager.ShouldRetry(result.Task.ID, result.Task.MaxRetries) {
					retryRecord := e.retryManager.RecordFailure(result.Task.ID, result.Task.MaxRetries, result.Error)
					if !retryRecord.Permanent {
						go func() {
							time.Sleep(retryRecord.NextRetryAt.Sub(time.Now()))
							e.queue.Requeue(result.Task, retryRecord.NextRetryAt.Sub(time.Now()))
						}()
					}
				}
			}
		}
	}
}

// routingDispatcher routes results to follow-up work.
func (e *Engine) routingDispatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-e.routingCh:
			if !ok {
				return
			}

			decision := e.router.Route(req.Task, req.Result)

			if decision.ShouldEnqueue() && req.Result.Data != nil {
				newTask := &Task{
					JobType:  decision.NextJobType,
					Priority: decision.Priority,
					Data:     req.Result.Data,
					Source:   req.Task.Source,
					MaxRetries: 3,
				}
				e.queue.Enqueue(newTask)
			}
		}
	}
}

// AddRoute adds a routing rule to the router.
func (e *Engine) AddRoute(route Route) {
	e.router.AddRoute(route)
}

// GetRouter returns the router.
func (e *Engine) GetRouter() *Router {
	return e.router
}
