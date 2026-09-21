// Package pool implements one bounded worker pool per provider, with
// retries and a spend cap wired around each call.
package pool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/budget"
	"github.com/fouad-lxix/seatbelt/internal/provider"
	"github.com/fouad-lxix/seatbelt/internal/stats"
)

// Sentinel errors Submit can return, so callers can use errors.Is instead
// of matching on message text.
var (
	ErrQueueFull    = errors.New("queue is full for this provider")
	ErrShuttingDown = errors.New("gateway is shutting down")

	// ErrCallerGone means the job reached a worker only after the caller
	// gave up, so no upstream call was made.
	ErrCallerGone = errors.New("caller gave up before the request was sent")
)

type result struct {
	resp *provider.Response
	err  error
}

type job struct {
	// ctx belongs to the caller's HTTP request and travels with the job
	// across the goroutine boundary to the worker that will run it.
	ctx context.Context
	req provider.Request

	// result is buffered with capacity 1. If it were unbuffered and the
	// caller had already walked away, the worker would block forever
	// handing over a result nobody collects, permanently losing a worker
	// from the pool with no error to show for it.
	result chan result
}

// Limiter is the pacing dependency a worker waits on before calling the
// provider. Declared here rather than in ratelimit so the pool can be
// tested with a fake and ratelimit never needs to know the pool exists.
type Limiter interface {
	Wait(ctx context.Context) error
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context) error { return nil }

// Budget is the spend dependency. It deliberately exposes no way to read
// the current balance, only Begin to ask permission. A getter would invite
// check-then-act, which is the bug this shape avoids.
type Budget interface {
	Begin() error
	Record(u provider.Usage, pricer budget.Pricer) (float64, error)
}

type noopBudget struct{}

func (noopBudget) Begin() error { return nil }

func (noopBudget) Record(provider.Usage, budget.Pricer) (float64, error) { return 0, nil }

// Options configures a Pool. Zero values use sane defaults.
type Options struct {
	Workers    int
	QueueDepth int

	// Limiter paces outbound calls. Nil means no pacing, only appropriate
	// in tests.
	Limiter Limiter

	// Budget enforces the spend cap. Nil means unlimited spending, only
	// appropriate in tests.
	Budget Budget

	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// Pool is one provider's queue and its fixed set of workers.
type Pool struct {
	prov    provider.Provider
	jobs    chan *job
	workers int
	limiter Limiter
	budget  Budget
	retry   retryPolicy

	// mu guards closed. Sending on a closed channel panics, so Submit
	// takes the read lock and Shutdown takes the write lock to keep a
	// close and an in-progress send mutually exclusive.
	mu     sync.RWMutex
	closed bool

	wg sync.WaitGroup

	inFlight atomic.Int64
	counters stats.Counters
}

// New builds a pool. Call Start to put the workers to work.
func New(p provider.Provider, opts Options) *Pool {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.QueueDepth < 1 {
		opts.QueueDepth = 1
	}
	if opts.Limiter == nil {
		opts.Limiter = noopLimiter{}
	}
	if opts.Budget == nil {
		opts.Budget = noopBudget{}
	}

	rp := defaultRetryPolicy()
	if opts.MaxAttempts > 0 {
		rp.maxAttempts = opts.MaxAttempts
	}
	if opts.BaseBackoff > 0 {
		rp.base = opts.BaseBackoff
	}
	if opts.MaxBackoff > 0 {
		rp.max = opts.MaxBackoff
	}

	return &Pool{
		prov:    p,
		jobs:    make(chan *job, opts.QueueDepth),
		workers: opts.Workers,
		limiter: opts.Limiter,
		budget:  opts.Budget,
		retry:   rp,
	}
}

// Start launches the worker goroutines. Call it once.
func (p *Pool) Start() {
	p.wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go p.work()
	}
}

// Submit queues one request and blocks until its answer arrives.
func (p *Pool) Submit(ctx context.Context, req provider.Request) (*provider.Response, error) {
	j := &job{
		ctx:    ctx,
		req:    req,
		result: make(chan result, 1),
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		p.counters.IncRejected()
		return nil, ErrShuttingDown
	}
	select {
	case p.jobs <- j:
		p.mu.RUnlock()
		p.counters.IncRequests()
	default:
		p.mu.RUnlock()
		p.counters.IncRejected()
		return nil, ErrQueueFull
	}

	select {
	case res := <-j.result:
		return res.resp, res.err
	case <-ctx.Done():
		// The job carries on regardless. Its result lands on the
		// buffered channel where nothing collects it.
		return nil, ctx.Err()
	}
}

// Shutdown stops accepting new work, drains queued and running jobs, and
// returns once the workers have stopped or ctx expires.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: shutdown timed out with %d job(s) queued and %d in flight",
			p.Name(), p.QueueDepth(), p.InFlight())
	}
}

func (p *Pool) Name() string { return p.prov.Name() }

// Workers is the max number of calls in flight to this provider at once.
func (p *Pool) Workers() int { return p.workers }

func (p *Pool) QueueDepth() int { return len(p.jobs) }

func (p *Pool) InFlight() int { return int(p.inFlight.Load()) }

func (p *Pool) Counts() stats.Counts { return p.counters.Snapshot() }

func (p *Pool) Retries() int64 { return p.counters.Snapshot().Retried }

// Unpriced is the count of successful calls whose cost could not be
// determined. Any value above zero means the spend total understates
// reality.
func (p *Pool) Unpriced() int64 { return p.counters.Snapshot().Unpriced }

func (p *Pool) QueueCapacity() int { return cap(p.jobs) }

func (p *Pool) work() {
	defer p.wg.Done()
	for j := range p.jobs {
		p.run(j)
	}
}

func (p *Pool) run(j *job) {
	// A job can wait in the queue long enough that its caller is already
	// gone. This check is an optimisation, not a guarantee: the caller
	// can still leave a moment after it passes.
	if err := j.ctx.Err(); err != nil {
		p.counters.IncFailed()
		j.result <- result{err: fmt.Errorf("%w: %v", ErrCallerGone, err)}
		return
	}

	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	resp, err := p.call(j.ctx, j.req)

	if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		p.counters.IncSucceeded()
	} else {
		p.counters.IncFailed()
	}

	j.result <- result{resp: resp, err: err}
}

// call makes the upstream request, paced by the rate limiter and repeated
// on transient failures. Each attempt reacquires a rate limit token and a
// budget check, since each is a separate real request. The caller's
// context is the only retry budget.
func (p *Pool) call(ctx context.Context, req provider.Request) (*provider.Response, error) {
	var (
		lastResp *provider.Response
		lastErr  error
	)

	for attempt := 1; attempt <= p.retry.maxAttempts; attempt++ {
		if err := p.budget.Begin(); err != nil {
			return p.lastOr(lastResp, lastErr, err)
		}

		if err := p.limiter.Wait(ctx); err != nil {
			return p.lastOr(lastResp, lastErr, err)
		}

		resp, err := p.prov.Send(ctx, req)

		if resp != nil && resp.StatusCode == http.StatusOK {
			cost, rerr := p.budget.Record(resp.Usage, p.prov)
			if rerr != nil {
				p.counters.IncUnpriced()
			} else {
				p.counters.AddSpend(cost)
			}
		}

		if !retryable(resp, err) {
			return resp, err
		}

		lastResp, lastErr = resp, err
		if attempt == p.retry.maxAttempts {
			break
		}

		p.counters.IncRetried()
		timer := time.NewTimer(p.retry.delay(attempt, resp))
		select {
		case <-ctx.Done():
			timer.Stop()
			return p.lastOr(lastResp, lastErr, ctx.Err())
		case <-timer.C:
		}
	}

	// Return the provider's own last response rather than an error
	// Seatbelt invented, so the caller sees the real status and body.
	return lastResp, lastErr
}

func (p *Pool) lastOr(resp *provider.Response, respErr, fallback error) (*provider.Response, error) {
	if resp != nil {
		return resp, respErr
	}
	return nil, fallback
}
