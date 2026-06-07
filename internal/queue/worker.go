// Package queue runs a worker pool that drains the persistent job queue.
package queue

import (
	"context"
	"errors"
	"log"
	"strings"
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
	store       model.Store
	proc        Processor
	workers     int
	logger      *log.Logger
	pollEvery   time.Duration
	maxAttempts int

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
		store:       store,
		proc:        proc,
		workers:     workers,
		logger:      logger,
		pollEvery:   2 * time.Second,
		maxAttempts: 3,
		wake:        make(chan struct{}, 1),
		keyLocks:    map[string]*sync.Mutex{},
		cancels:     map[string]*cancelEntry{},
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
	finish := model.JobFinish{Status: model.JobDone}
	if err != nil {
		if jobCtx.Err() != nil {
			finish.Status = model.JobSuperseded
		} else {
			classified := classifyError(err)
			finish.Status = model.JobFailed
			finish.ErrorType = classified.errorType
			finish.Retryable = classified.retryable
			if classified.retryable && job.Attempts < q.maxAttempts {
				delay := retryDelay(job.Attempts)
				next := time.Now().UTC().Add(delay)
				finish.Status = model.JobPending
				finish.NextAttemptAt = &next
				q.jobLog(job.ID, "queue", "retry scheduled in "+delay.String()+" ("+string(classified.errorType)+")")
			}
		}
		finish.Error = err.Error()
		q.logf("job %d (%s#%d) %s: %v", job.ID, key, job.PR.Number, finish.Status, err)
	}
	if ferr := q.store.FinishJobDetailed(context.Background(), job.ID, finish); ferr != nil {
		q.logf("finish job %d: %v", job.ID, ferr)
	} else {
		q.jobLog(job.ID, "queue", "finished with status "+string(finish.Status))
		if finish.Status == model.JobPending {
			q.Notify()
		}
	}
}

type classifiedError struct {
	errorType model.ErrorType
	retryable bool
}

func classifyError(err error) classifiedError {
	if err == nil {
		return classifiedError{}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classifiedError{errorType: model.ErrorTypeTimeout, retryable: true}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return classifiedError{errorType: model.ErrorTypeTimeout, retryable: true}
	case strings.Contains(msg, "auth"), strings.Contains(msg, "login"), strings.Contains(msg, "api key"), strings.Contains(msg, "unauthorized"), strings.Contains(msg, "401"):
		return classifiedError{errorType: model.ErrorTypeAuth, retryable: false}
	case strings.Contains(msg, "no reviewers configured"), strings.Contains(msg, "invalid config"), strings.Contains(msg, "not configured"):
		return classifiedError{errorType: model.ErrorTypeConfig, retryable: false}
	case strings.Contains(msg, "git prepare"), strings.Contains(msg, "rev-parse"), strings.Contains(msg, "git fetch"), strings.Contains(msg, "git checkout"):
		return classifiedError{errorType: model.ErrorTypeGit, retryable: isTransient(msg)}
	case strings.Contains(msg, "gitea:"), strings.Contains(msg, "get diff"), strings.Contains(msg, "post review"), strings.Contains(msg, "post comment"), strings.Contains(msg, "check pr status"):
		return classifiedError{errorType: model.ErrorTypeGitea, retryable: isTransient(msg)}
	case strings.Contains(msg, "no available channel"), strings.Contains(msg, "api error"), strings.Contains(msg, "relay"), strings.Contains(msg, "upstream"):
		return classifiedError{errorType: model.ErrorTypeUpstream, retryable: true}
	case strings.Contains(msg, "codex review"), strings.Contains(msg, "claude review"), strings.Contains(msg, "reviewer"):
		return classifiedError{errorType: model.ErrorTypeReviewer, retryable: isTransient(msg)}
	default:
		return classifiedError{errorType: model.ErrorTypeUnknown, retryable: isTransient(msg)}
	}
}

func isTransient(msg string) bool {
	for _, needle := range []string{
		"503", "502", "504", "429", "temporarily", "temporary", "timeout",
		"deadline exceeded", "connection reset", "connection refused", "network",
		"no available channel", "rate limit", "too many requests",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Duration(1<<(attempts-1)) * 30 * time.Second
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}
