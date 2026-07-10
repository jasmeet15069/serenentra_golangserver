// Package worker provides the asynchronous worker pool from the architecture
// diagram ("Async Worker Pool — email · SMS · AI reports · invoice mail … all
// called via the Worker Pool, never on the request path").
//
// Before this, side-effecting work (email/SMS/invoice sends) was launched with
// ad-hoc `go func(){...}` per request: unbounded goroutines, no backpressure,
// no graceful drain, and any in-flight send was lost on shutdown. The pool
// replaces that with a fixed set of workers consuming a bounded queue, per-job
// timeouts, panic recovery, and a graceful drain on shutdown.
//
// The queue is in-process today. The Job shape (a named unit of work) is
// deliberately backend-agnostic so a Redis-list-backed durable queue can be
// slotted in later without touching call sites.
package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Job is one unit of background work.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Pool is a fixed-size worker pool draining a bounded job queue.
type Pool struct {
	jobs    chan Job
	workers int
	jobTTL  time.Duration
	log     *zap.Logger

	wg       sync.WaitGroup
	stopOnce sync.Once

	submitted atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	dropped   atomic.Int64
}

// Default is the process-wide pool. main wires it at startup so call sites can
// use the package-level Submit/SubmitOrRun helpers without threading the pool
// through every handler constructor.
var Default *Pool

// New builds a pool. workers is the goroutine count; queueSize is the buffered
// queue depth; jobTTL bounds how long any single job may run.
func New(workers, queueSize int, jobTTL time.Duration, log *zap.Logger) *Pool {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}
	if jobTTL <= 0 {
		jobTTL = 30 * time.Second
	}
	return &Pool{
		jobs:    make(chan Job, queueSize),
		workers: workers,
		jobTTL:  jobTTL,
		log:     log,
	}
}

// Start launches the worker goroutines. The supplied context is the parent for
// every job; cancelling it (or Shutdown) stops the pool.
func (p *Pool) Start(parent context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.loop(parent)
	}
	p.log.Info("worker: pool started", zap.Int("workers", p.workers), zap.Int("queue", cap(p.jobs)))
}

func (p *Pool) loop(parent context.Context) {
	defer p.wg.Done()
	for job := range p.jobs {
		p.exec(parent, job)
	}
}

func (p *Pool) exec(parent context.Context, job Job) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			p.failed.Add(1)
			p.log.Error("worker: job panicked", zap.String("job", job.Name), zap.Any("panic", r))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, p.jobTTL)
	defer cancel()

	if err := job.Run(ctx); err != nil {
		p.failed.Add(1)
		p.log.Warn("worker: job failed", zap.String("job", job.Name), zap.Error(err), zap.Duration("dur", time.Since(start)))
		return
	}
	p.completed.Add(1)
}

// Submit enqueues a job without blocking. It returns false if the queue is full,
// letting the caller decide whether to run the work inline or drop it.
func (p *Pool) Submit(job Job) bool {
	select {
	case p.jobs <- job:
		p.submitted.Add(1)
		return true
	default:
		p.dropped.Add(1)
		p.log.Warn("worker: queue full, job rejected", zap.String("job", job.Name))
		return false
	}
}

// Shutdown stops accepting work and waits for in-flight + queued jobs to drain,
// bounded by ctx. Returns ctx.Err() if the drain did not finish in time.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.jobs) })
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		p.log.Info("worker: pool drained",
			zap.Int64("submitted", p.submitted.Load()),
			zap.Int64("completed", p.completed.Load()),
			zap.Int64("failed", p.failed.Load()),
			zap.Int64("dropped", p.dropped.Load()))
		return nil
	case <-ctx.Done():
		p.log.Warn("worker: shutdown timed out before drain")
		return ctx.Err()
	}
}

// Stats returns a point-in-time snapshot of pool counters.
func (p *Pool) Stats() map[string]int64 {
	return map[string]int64{
		"submitted": p.submitted.Load(),
		"completed": p.completed.Load(),
		"failed":    p.failed.Load(),
		"dropped":   p.dropped.Load(),
		"queued":    int64(len(p.jobs)),
	}
}

// SubmitOrRun is the call-site helper. It enqueues onto Default; if Default is
// unset or its queue is full, it falls back to a detached goroutine so the work
// still happens off the request path (preserving prior behaviour). Either way it
// never blocks the caller.
func SubmitOrRun(name string, run func(ctx context.Context) error) {
	job := Job{Name: name, Run: run}
	if Default != nil && Default.Submit(job) {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = run(ctx)
	}()
}
