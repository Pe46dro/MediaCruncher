package concurrency

import (
	"fmt"
	"sync"
	"time"
)

// BatchJob represents a large job that needs to be split into sub-tasks.
type BatchJob struct {
	ID          string
	ParentTask  *Task
	SubTasks    []*Task
	TotalSub    int
	Completed   int
	Failed      int
	Results     []interface{}
	mu          sync.Mutex
	CompleteCh  chan *BatchResult
	CreatedAt   time.Time
}

// BatchResult holds the assembled result from a batch job.
type BatchResult struct {
	BatchJobID string        `json:"batch_job_id"`
	Success    bool          `json:"success"`
	Total      int           `json:"total"`
	Completed  int           `json:"completed"`
	Failed     int           `json:"failed"`
	Results    []interface{} `json:"results"`
	Duration   time.Duration `json:"duration"`
}

// BatchSplitter decomposes large jobs into sub-tasks.
type BatchSplitter struct {
	mu          sync.Mutex
	activeBatches map[string]*BatchJob
	maxBatchSize int
}

// NewBatchSplitter creates a new batch splitter.
func NewBatchSplitter(maxBatchSize int) *BatchSplitter {
	if maxBatchSize <= 0 {
		maxBatchSize = 100
	}
	return &BatchSplitter{
		activeBatches: make(map[string]*BatchJob),
		maxBatchSize:  maxBatchSize,
	}
}

// Split decomposes a parent task into sub-tasks based on the data type.
func (s *BatchSplitter) Split(parentTask *Task) (*BatchJob, bool) {
	if parentTask == nil {
		return nil, false
	}

	batchID := fmt.Sprintf("batch-%s-%d", parentTask.Source, parentTask.ID)
	batch := &BatchJob{
		ID:          batchID,
		ParentTask:  parentTask,
		CompleteCh:  make(chan *BatchResult, 1),
		CreatedAt:   time.Now(),
	}

	switch data := parentTask.Data.(type) {
	case []string:
		if len(data) <= s.maxBatchSize {
			for _, item := range data {
				subTask := &Task{
					JobType:  parentTask.JobType,
					Priority: parentTask.Priority,
					Data:     item,
					Source:   parentTask.Source,
					MaxRetries: parentTask.MaxRetries,
				}
				batch.SubTasks = append(batch.SubTasks, subTask)
			}
		} else {
			chunkSize := len(data) / s.maxBatchSize
			for i := 0; i < len(data); i += chunkSize {
				end := i + chunkSize
				if end > len(data) {
					end = len(data)
				}
				chunk := data[i:end]
				subTask := &Task{
					JobType:  parentTask.JobType,
					Priority: parentTask.Priority,
					Data:     chunk,
					Source:   parentTask.Source,
					MaxRetries: parentTask.MaxRetries,
				}
				batch.SubTasks = append(batch.SubTasks, subTask)
			}
		}
	case []interface{}:
		if len(data) <= s.maxBatchSize {
			for _, item := range data {
				subTask := &Task{
					JobType:  parentTask.JobType,
					Priority: parentTask.Priority,
					Data:     item,
					Source:   parentTask.Source,
					MaxRetries: parentTask.MaxRetries,
				}
				batch.SubTasks = append(batch.SubTasks, subTask)
			}
		} else {
			chunkSize := len(data) / s.maxBatchSize
			for i := 0; i < len(data); i += chunkSize {
				end := i + chunkSize
				if end > len(data) {
					end = len(data)
				}
				chunk := data[i:end]
				subTask := &Task{
					JobType:  parentTask.JobType,
					Priority: parentTask.Priority,
					Data:     chunk,
					Source:   parentTask.Source,
					MaxRetries: parentTask.MaxRetries,
				}
				batch.SubTasks = append(batch.SubTasks, subTask)
			}
		}
	default:
		return nil, false
	}

	batch.TotalSub = len(batch.SubTasks)

	s.mu.Lock()
	s.activeBatches[batchID] = batch
	s.mu.Unlock()

	return batch, true
}

// RecordSubTaskResult records a sub-task result and checks if the batch is complete.
func (s *BatchSplitter) RecordSubTaskResult(batchJobID string, result interface{}, success bool) *BatchResult {
	s.mu.Lock()
	batch, exists := s.activeBatches[batchJobID]
	s.mu.Unlock()

	if !exists {
		return nil
	}

	batch.mu.Lock()
	defer batch.mu.Unlock()

	batch.Results = append(batch.Results, result)
	if success {
		batch.Completed++
	} else {
		batch.Failed++
	}

	if batch.Completed+batch.Failed >= batch.TotalSub {
		batchResult := &BatchResult{
			BatchJobID: batchJobID,
			Success:    batch.Failed == 0,
			Total:      batch.TotalSub,
			Completed:  batch.Completed,
			Failed:     batch.Failed,
			Results:    batch.Results,
			Duration:   time.Since(batch.CreatedAt),
		}

		select {
		case batch.CompleteCh <- batchResult:
		default:
		}

		s.mu.Lock()
		delete(s.activeBatches, batchJobID)
		s.mu.Unlock()

		return batchResult
	}

	return nil
}

// GetActiveCount returns the number of active batch jobs.
func (s *BatchSplitter) GetActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.activeBatches)
}

// GetBatchResult waits for a batch result with a timeout.
func (s *BatchSplitter) GetBatchResult(batchJobID string, timeout time.Duration) (*BatchResult, bool) {
	s.mu.Lock()
	batch, exists := s.activeBatches[batchJobID]
	s.mu.Unlock()

	if !exists {
		return nil, false
	}

	select {
	case result := <-batch.CompleteCh:
		return result, true
	case <-time.After(timeout):
		return nil, false
	}
}
