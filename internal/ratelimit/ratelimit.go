// Package ratelimit implements a token bucket rate limiter, safe for
// concurrent use.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// minWaitSlice is the floor on how long Wait sleeps before re-checking.
const minWaitSlice = time.Millisecond

// Bucket is one provider's rate limiter. The zero value is not usable, call
// New.
type Bucket struct {
	// mu guards tokens and last together. They form one invariant (tokens
	// is only meaningful alongside the moment it was accurate as of), so a
	// mutex is used instead of separate atomics.
	mu sync.Mutex

	// tokens is a float64 because refill is continuous: at one token per
	// second, 300ms of elapsed time is 0.3 of a token.
	tokens float64

	// last is the moment up to which elapsed time has already been
	// converted into tokens, not the time of the last request.
	last time.Time

	capacity     float64
	refillPerSec float64

	// now is injectable so tests can control time without sleeping.
	now func() time.Time
}

// New builds a bucket allowing perMinute requests per minute in the long
// run, with room to bank up to burst requests while idle. The bucket starts
// full.
func New(perMinute, burst int) *Bucket {
	if perMinute < 1 {
		perMinute = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Bucket{
		tokens:       float64(burst),
		last:         time.Now(),
		capacity:     float64(burst),
		refillPerSec: float64(perMinute) / 60,
		now:          time.Now,
	}
}

// Allow takes a token if one is available and reports whether it did. It
// never blocks.
func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Wait blocks until a token is available and takes it, or until ctx is
// done.
func (b *Bucket) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		delay, ok := b.take()
		if ok {
			return nil
		}
		if delay < minWaitSlice {
			delay = minWaitSlice
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Another goroutine may have taken the token this one
			// waited for. Loop and recompute rather than assume it is
			// still there.
		}
	}
}

// Tokens reports the current headroom, refilled to this instant.
func (b *Bucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked()
	return b.tokens
}

// Capacity is the maximum number of tokens the bucket can bank.
func (b *Bucket) Capacity() float64 { return b.capacity }

// PerMinute is the configured long-run rate.
func (b *Bucket) PerMinute() float64 { return b.refillPerSec * 60 }

// take removes a token if one is available, or reports how long until one
// will be.
func (b *Bucket) take() (wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked()
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}

	missing := 1 - b.tokens
	seconds := missing / b.refillPerSec
	return time.Duration(seconds * float64(time.Second)), false
}

// refillLocked credits the time elapsed since last and re-anchors last to
// now. Callers must hold b.mu.
func (b *Bucket) refillLocked() {
	now := b.now()
	elapsed := now.Sub(b.last)

	if elapsed <= 0 {
		b.last = now
		return
	}

	b.tokens += elapsed.Seconds() * b.refillPerSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}
