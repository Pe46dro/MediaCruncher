package concurrency

import (
	"container/heap"
	"fmt"
	"sync"
	"time"
)

// Priority represents task priority levels.
type Priority int

const (
	PriorityCritical Priority = 0
	PriorityHigh     Priority = 1
	PriorityNormal   Priority = 2
	PriorityLow      Priority = 3
)

// JobType classifies the type of work item.
type JobType string

const (
	JobTypeEvaluate JobType = "evaluate"
	JobTypeTranscode JobType = "transcode"
	JobTypeNotify   JobType = "notify"
)

// Task represents a work item in the queue.
type Task struct {
	ID         int64
	JobType    JobType
	Priority   Priority
	Data       interface{}
	Source     string
	EnqueueAt  time.Time
	Attempt    int
	MaxRetries int
	CreatedAt  time.Time
}

// TaskQueue is an in-memory priority queue for work items.
type TaskQueue struct {
	mu         sync.Mutex
	items      priorityQueue
	maxDepth   int64
	nextID     int64
	depth      int64
	capacity   int64
	fullSignal chan struct{}
}

type priorityQueue []*Task

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority < pq[j].Priority
	}
	return pq[i].EnqueueAt.Before(pq[j].EnqueueAt)
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].ID = int64(i)
	pq[j].ID = int64(j)
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*Task)
	item.ID = int64(n)
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.ID = 0
	*pq = old[:n-1]
	return item
}

// NewTaskQueue creates a new priority task queue.
func NewTaskQueue(maxDepth int64) *TaskQueue {
	if maxDepth <= 0 {
		maxDepth = 10000
	}
	pq := make(priorityQueue, 0)
	heap.Init(&pq)
	return &TaskQueue{
		items:      pq,
		maxDepth:   maxDepth,
		capacity:   maxDepth,
		fullSignal: make(chan struct{}, 1),
	}
}

// Enqueue adds a task to the queue. Returns true if accepted, false if queue is full.
func (q *TaskQueue) Enqueue(task *Task) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if int64(len(q.items)) >= q.capacity {
		select {
		case q.fullSignal <- struct{}{}:
		default:
		}
		return false
	}

	task.CreatedAt = time.Now()
	task.EnqueueAt = time.Now()
	if task.MaxRetries <= 0 {
		task.MaxRetries = 3
	}
	q.nextID++
	task.ID = q.nextID

	heap.Push(&q.items, task)
	q.depth = int64(len(q.items))

	if len(q.fullSignal) > 0 {
		<-q.fullSignal
	}

	return true
}

// Claim removes and returns the highest-priority task for the given job type.
func (q *TaskQueue) Claim(jobType JobType, workerID string) (*Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i := range q.items {
		if q.items[i].JobType == jobType {
			task := q.items[i]
			heap.Remove(&q.items, i)
			q.depth = int64(len(q.items))
			return task, true
		}
	}

	return nil, false
}

// ClaimAny removes and returns the highest-priority task regardless of job type.
func (q *TaskQueue) ClaimAny() (*Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return nil, false
	}

	task := heap.Pop(&q.items).(*Task)
	q.depth = int64(len(q.items))
	return task, true
}

// Depth returns the current queue depth.
func (q *TaskQueue) Depth() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// IsFull returns true if the queue is at capacity.
func (q *TaskQueue) IsFull() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return int64(len(q.items)) >= q.capacity
}

// Reset clears all tasks from the queue.
func (q *TaskQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = q.items[:0]
	q.depth = 0
	heap.Init(&q.items)
}

// PendingByType returns the count of pending tasks for a specific job type.
func (q *TaskQueue) PendingByType(jobType JobType) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	for _, task := range q.items {
		if task.JobType == jobType {
			count++
		}
	}
	return count
}

// Pending returns the total pending tasks across all types.
func (q *TaskQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Requeue adds a task back to the queue with incremented attempt count.
func (q *TaskQueue) Requeue(task *Task, delay time.Duration) bool {
	if task.Attempt >= task.MaxRetries {
		return false
	}

	task.Attempt++
	task.EnqueueAt = time.Now().Add(delay)

	return q.Enqueue(task)
}

// TopPriority returns the priority of the highest-priority task without removing it.
func (q *TaskQueue) TopPriority() (Priority, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return PriorityNormal, false
	}
	return q.items[0].Priority, true
}

// String returns a string representation of the queue state.
func (q *TaskQueue) String() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return fmt.Sprintf("queue{depth=%d, capacity=%d}", q.depth, q.capacity)
}
