// Package budget turns reported token usage into dollars, keeps a
// thread-safe running total, and hard-enforces the spend cap.
//
// There is no exported way to read the balance and act on it. Begin is the
// only way to ask permission, and it checks and decides inside one lock.
// A check-then-act API here would let concurrent callers all pass the check
// before any of them records spend, blowing straight through the cap even
// though every access is individually synchronized.
package budget

import (
	"errors"
	"fmt"
	"sync"

	"github.com/fouad-lxix/seatbelt/internal/provider"
)

var (
	// ErrBudgetExceeded means the cap has been reached and no further
	// outbound calls will be made.
	ErrBudgetExceeded = errors.New("budget exceeded")

	// ErrUnpriced means a completed call could not be converted into
	// dollars, so the running total can no longer be trusted.
	ErrUnpriced = errors.New("call could not be priced")
)

// Pricer looks up what a model costs.
type Pricer interface {
	Price(model string) (provider.Price, bool)
}

// Tracker is the gateway's single spend ledger, shared by every provider.
// The cap is on total spend, not per provider.
type Tracker struct {
	mu sync.Mutex

	capUSD   float64
	spentUSD float64

	calls    int64
	unpriced int64

	// halted latches. Once stopped, it stays stopped for the life of the
	// process.
	halted     bool
	haltReason string
}

// New creates a tracker with the given hard cap in US dollars.
func New(capUSD float64) *Tracker {
	return &Tracker{capUSD: capUSD}
}

// Begin asks permission to make one outbound call. It returns
// ErrBudgetExceeded once the cap has been reached.
func (t *Tracker) Begin() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.halted {
		return fmt.Errorf("%w: %s", ErrBudgetExceeded, t.haltReason)
	}
	if t.spentUSD >= t.capUSD {
		t.haltLocked(fmt.Sprintf("spent $%.4f of the $%.2f cap", t.spentUSD, t.capUSD))
		return fmt.Errorf("%w: %s", ErrBudgetExceeded, t.haltReason)
	}
	return nil
}

// Record books the cost of one completed call and returns what it cost.
//
// A call that cannot be priced is not free, it is a call whose cost is
// unknown, so the tracker halts rather than undercounting. The response is
// still returned to the caller since the money is already spent.
func (t *Tracker) Record(u provider.Usage, pricer Pricer) (float64, error) {
	if !u.Reported() {
		return 0, t.fail("a call succeeded but reported no token usage, so it cannot be priced")
	}

	price, ok := pricer.Price(u.Model)
	if !ok {
		return 0, t.fail(fmt.Sprintf(
			"no price is configured for model %q, add it to the provider's pricing table",
			u.Model))
	}

	cost := price.Cost(u)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.spentUSD += cost
	t.calls++
	return cost, nil
}

// Snapshot is a photograph of the ledger at one instant, for display. Use
// Begin for decisions, never these fields.
type Snapshot struct {
	CapUSD        float64 `json:"cap_usd"`
	SpentUSD      float64 `json:"spent_usd"`
	RemainingUSD  float64 `json:"remaining_usd"`
	Calls         int64   `json:"calls_priced"`
	UnpricedCalls int64   `json:"calls_unpriced"`
	Halted        bool    `json:"halted"`
	HaltReason    string  `json:"halt_reason,omitempty"`
}

// Snapshot reads the whole ledger under one lock, so the numbers are
// consistent with each other even if stale by the time they are read.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	remaining := t.capUSD - t.spentUSD
	if remaining < 0 {
		remaining = 0
	}
	return Snapshot{
		CapUSD:        t.capUSD,
		SpentUSD:      t.spentUSD,
		RemainingUSD:  remaining,
		Calls:         t.calls,
		UnpricedCalls: t.unpriced,
		Halted:        t.halted,
		HaltReason:    t.haltReason,
	}
}

func (t *Tracker) fail(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.unpriced++
	t.haltLocked(reason)
	return fmt.Errorf("%w: %s", ErrUnpriced, reason)
}

// haltLocked latches the stop. Callers must hold t.mu.
func (t *Tracker) haltLocked(reason string) {
	if !t.halted {
		t.halted = true
		t.haltReason = reason
	}
}
