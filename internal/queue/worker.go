// Package queue runs a worker pool that drains the persistent job queue.
package queue

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Processor handles one claimed job (implemented by review.Orchestrator).
type Processor interface {
	Process(ctx context.Context, job *model.Job) error
}

// Queue polls the store for pending jobs and dispatches them to workers.
type Queue struct {
	store     model.Store
	proc      Processor
	workers   int
	logger    *log.Logger
	pollEvery time.Duration

	wake chan struct{}

	mu       sync.Mutex
	keyLocks map[string]*sync.Mutex // per-PR serialization
	cancels  map[string]*cancelEntry
}

// cancelEntry wraps a cancel func so in-flight runs can be identified by
// pointer identity (funcs aren't comparable).
type cancelEntry struct {
	cancel context.CancelFunc
}

// New builds a Queue. workers<1 defaults to 1.
func New(store model.Store, proc Processor, workers int, logger *log.Logger) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{
		store:     store,
		proc:      proc,
		workers:   workers,
		logger:    logger,
		pollEvery: 2 * time.Second,
		wake:      make(chan struct{}, 1),
		keyLocks:  map[string]*sync.Mutex{},
		cancels:   map[string]*cancelEntry{},
	}
}

func (q *Queue) logf(format string, args ...any) {
	if q.logger != nil {
		q.logger.Printf(format, args...)
	}
}

func (q *Queue) jobLog(jobID int64, stage, message string) {
	if q.store != nil {
		if err := q.store.AppendJobLog(context.Background(), jobID, stage, message); err != nil {
			q.logf("job %d log %s: %v", jobID, stage, err)
		}
	}
	q.logf("job %d [%s] %s", jobID, stage, message)
}

// Notify wakes the pool to claim immediately (call after enqueuing).
func (q *Queue) Notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// CancelInFlight cancels a running job for the given PR (debounce on new push).
func (q *Queue) CancelInFlight(pr model.PRRef) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry, ok := q.cancels[pr.Key()]; ok {
		entry.cancel()
	}
}

func (q *Queue) keyLock(key string) *sync.Mutex {
	q.mu.Lock()
	defer q.mu.Unlock()
	m, ok := q.keyLocks[key]
	if !ok {
		m = &sync.Mutex{}
		q.keyLocks[key] = m
	}
	return m
}

// Run starts the worker pool and blocks until ctx is cancelled.
func (q *Queue) Run(ctx context.Context) error {
	if err := q.store.RecoverRunning(ctx); err != nil {
		q.logf("recover running jobs: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < q.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			q.worker(ctx, id)
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

func (q *Queue) worker(ctx context.Context, id int) {
	ticker := time.NewTicker(q.pollEvery)
	defer ticker.Stop()

	for {
		job, err := q.store.ClaimJob(ctx)
		if err != nil {
			q.logf("worker %d claim: %v", id, err)
		}
		if job != nil {
			q.run(ctx, job)
			continue // drain greedily
		}
		// nothing pending: wait for a wake or the next tick
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
	}
}

func (q *Queue) run(parent context.Context, job *model.Job) {
	key := job.PR.Key()

	// Serialize per PR (deterministic worktree path can't be shared).
	lock := q.keyLock(key)
	if !lock.TryLock() {
		q.jobLog(job.ID, "queue", "waiting for previous job on "+key)
		lock.Lock()
	}
	defer lock.Unlock()

	jobCtx, cancel := context.WithCancel(parent)
	entry := &cancelEntry{cancel: cancel}
	q.mu.Lock()
	q.cancels[key] = entry
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		if q.cancels[key] == entry {
			delete(q.cancels, key)
		}
		q.mu.Unlock()
		cancel()
	}()

	q.jobLog(job.ID, "queue", "started worker run")
	err := q.proc.Process(jobCtx, job)
	status := model.JobDone
	msg := ""
	if err != nil {
		if jobCtx.Err() != nil {
			status = model.JobSuperseded
		} else {
			status = model.JobFailed
		}
		msg = err.Error()
		q.logf("job %d (%s#%d) %s: %v", job.ID, key, job.PR.Number, status, err)
	}
	if ferr := q.store.FinishJob(context.Background(), job.ID, status, msg); ferr != nil {
		q.logf("finish job %d: %v", job.ID, ferr)
	} else {
		q.jobLog(job.ID, "queue", "finished with status "+string(status))
	}
}
