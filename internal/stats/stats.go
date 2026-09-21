// Package stats holds the in-memory counters behind /stats and /stats/ui,
// and assembles them into one report. Nothing here is persisted, a
// restart resets every number.
package stats

import (
	"sync"
	"sync/atomic"
)

// Counters is one provider's cumulative tallies, safe for concurrent use.
// The integer fields are atomics, each an independent counter. The dollar
// total uses a mutex since float64 has no atomic add.
type Counters struct {
	requests  atomic.Int64
	succeeded atomic.Int64
	failed    atomic.Int64
	retried   atomic.Int64
	rejected  atomic.Int64
	unpriced  atomic.Int64

	mu    sync.Mutex
	spend float64
}

func (c *Counters) IncRequests() { c.requests.Add(1) }

func (c *Counters) IncSucceeded() { c.succeeded.Add(1) }

// IncFailed counts a call that did not succeed, including non-2xx
// responses, transport failures, and jobs abandoned before running.
func (c *Counters) IncFailed() { c.failed.Add(1) }

// IncRetried counts one repeat attempt, not one retried request.
func (c *Counters) IncRetried() { c.retried.Add(1) }

// IncRejected counts a request refused before it was ever queued.
func (c *Counters) IncRejected() { c.rejected.Add(1) }

// IncUnpriced counts a successful call whose cost could not be
// determined.
func (c *Counters) IncUnpriced() { c.unpriced.Add(1) }

func (c *Counters) AddSpend(usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spend += usd
}

// Counts is a Counters read at one instant.
type Counts struct {
	Requests  int64   `json:"requests"`
	Succeeded int64   `json:"succeeded"`
	Failed    int64   `json:"failed"`
	Retried   int64   `json:"retried"`
	Rejected  int64   `json:"rejected"`
	Unpriced  int64   `json:"unpriced"`
	SpendUSD  float64 `json:"spend_usd"`
}

func (c *Counters) Snapshot() Counts {
	c.mu.Lock()
	spend := c.spend
	c.mu.Unlock()

	return Counts{
		Requests:  c.requests.Load(),
		Succeeded: c.succeeded.Load(),
		Failed:    c.failed.Load(),
		Retried:   c.retried.Load(),
		Rejected:  c.rejected.Load(),
		Unpriced:  c.unpriced.Load(),
		SpendUSD:  spend,
	}
}

func (a Counts) Add(b Counts) Counts {
	return Counts{
		Requests:  a.Requests + b.Requests,
		Succeeded: a.Succeeded + b.Succeeded,
		Failed:    a.Failed + b.Failed,
		Retried:   a.Retried + b.Retried,
		Rejected:  a.Rejected + b.Rejected,
		Unpriced:  a.Unpriced + b.Unpriced,
		SpendUSD:  a.SpendUSD + b.SpendUSD,
	}
}

// RateLimit describes one provider's token bucket for display.
type RateLimit struct {
	PerMinute float64 `json:"per_minute"`
	Burst     float64 `json:"burst"`

	// Headroom is how many requests could start right now, a fraction
	// since the bucket refills continuously.
	Headroom float64 `json:"headroom"`
}

// Provider is everything /stats reports about one provider.
type Provider struct {
	Name          string    `json:"name"`
	Workers       int       `json:"workers"`
	InFlight      int       `json:"in_flight"`
	QueueDepth    int       `json:"queue_depth"`
	QueueCapacity int       `json:"queue_capacity"`
	RateLimit     RateLimit `json:"rate_limit"`
	Counts        Counts    `json:"counts"`
}

// Combined is every provider's numbers added together, plus the gauges.
type Combined struct {
	InFlight      int    `json:"in_flight"`
	QueueDepth    int    `json:"queue_depth"`
	QueueCapacity int    `json:"queue_capacity"`
	Counts        Counts `json:"counts"`
}

// Budget mirrors budget.Snapshot without importing that package, keeping
// stats a leaf package.
type Budget struct {
	CapUSD        float64 `json:"cap_usd"`
	SpentUSD      float64 `json:"spent_usd"`
	RemainingUSD  float64 `json:"remaining_usd"`
	Calls         int64   `json:"calls_priced"`
	UnpricedCalls int64   `json:"calls_unpriced"`
	Halted        bool    `json:"halted"`
	HaltReason    string  `json:"halt_reason,omitempty"`
}

// Report is the whole /stats document.
type Report struct {
	Uptime string `json:"uptime"`

	// PricingAsOf and PricingSource sit next to the spend figures since
	// those figures are only as good as the prices behind them.
	PricingAsOf   string `json:"pricing_as_of"`
	PricingSource string `json:"pricing_source"`

	Budget    Budget     `json:"budget"`
	Providers []Provider `json:"providers"`
	Combined  Combined   `json:"combined"`
}

// Combine adds up per-provider numbers. Providers should arrive in a
// stable order so the output does not reshuffle on every refresh.
func Combine(providers []Provider) Combined {
	var c Combined
	for _, p := range providers {
		c.InFlight += p.InFlight
		c.QueueDepth += p.QueueDepth
		c.QueueCapacity += p.QueueCapacity
		c.Counts = c.Counts.Add(p.Counts)
	}
	return c
}
