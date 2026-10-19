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
	"log"
	"sync"
	"time"

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

// Reset controls when the spend total starts over.
type Reset int

const (
	ResetMonthly Reset = iota // calendar month, UTC
	ResetNever
)

func (r Reset) String() string {
	if r == ResetNever {
		return "never"
	}
	return "monthly"
}

// haltKind records why the tracker stopped, since each kind lifts
// differently.
type haltKind int

const (
	haltNone     haltKind = iota
	haltCap               // cap reached, lifts when the period changes
	haltUnpriced          // a call could not be priced, lifts only on restart
	haltRecord            // saved record unreadable, lifts on recovery or a new period
)

// Pricer looks up what a model costs.
type Pricer interface {
	Price(model string) (provider.Price, bool)
}

// Options configures a Tracker.
type Options struct {
	CapUSD float64
	Reset  Reset

	// Dir is where the spend record is saved. Empty means memory only.
	Dir string

	// Recover sets this period's spend when there is no readable saved
	// record. It is ignored when a readable record exists.
	Recover *float64

	// Now and Logf are injectable for tests. Nil means time.Now and
	// log.Printf.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Tracker is the gateway's single spend ledger, shared by every provider.
// The cap is on total spend, not per provider.
type Tracker struct {
	mu sync.Mutex

	capUSD   float64
	spentUSD float64
	calls    int64
	unpriced int64

	reset  Reset
	period string
	now    func() time.Time
	logf   func(format string, args ...any)

	ledger  *ledger
	saveErr string

	halt       haltKind
	haltReason string
}

// New creates a memory-only tracker that never resets.
func New(capUSD float64) *Tracker {
	return newTracker(Options{CapUSD: capUSD, Reset: ResetNever})
}

func newTracker(opts Options) *Tracker {
	t := &Tracker{
		capUSD: opts.CapUSD,
		reset:  opts.Reset,
		now:    opts.Now,
		logf:   opts.Logf,
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.logf == nil {
		t.logf = log.Printf
	}
	t.period = t.periodFor(t.now())
	return t
}

// Open creates a tracker that saves its total to opts.Dir and restores it
// on startup. It fails only when the directory cannot be used at all.
func Open(opts Options) (*Tracker, error) {
	t := newTracker(opts)
	if opts.Dir == "" {
		return t, nil
	}

	l, err := newLedger(opts.Dir)
	if err != nil {
		return nil, err
	}
	t.ledger = l

	rec, status := l.load()
	switch status {
	case loadMissing:
		if opts.Recover != nil {
			t.spentUSD = *opts.Recover
			t.logf("spend: no saved record, starting at $%.4f from RECOVER_SPEND_USD", t.spentUSD)
			t.saveLocked()
		}

	case loadOK, loadFromBackup:
		if status == loadFromBackup {
			t.logf("spend: the main spend record was damaged, restored from backup")
		}
		if opts.Recover != nil {
			t.logf("spend: RECOVER_SPEND_USD ignored, a readable spend record exists")
		}
		if rec.Period == t.period {
			t.spentUSD = rec.SpentUSD
			t.calls = rec.Calls
		}

	case loadUnreadable:
		if opts.Recover != nil {
			t.spentUSD = *opts.Recover
			t.logf("spend: saved record was unreadable, recovered to $%.4f from RECOVER_SPEND_USD", t.spentUSD)
			t.saveLocked()
			break
		}
		t.halt = haltRecord
		t.haltReason = fmt.Sprintf("the saved spend record in %s is unreadable. "+
			"Set RECOVER_SPEND_USD to the amount spent so far (check your provider's "+
			"billing page, or use 0 to start fresh) and restart", opts.Dir)
		t.logf("spend: %s", t.haltReason)
	}
	return t, nil
}

// Begin asks permission to make one outbound call. It returns
// ErrBudgetExceeded once the cap has been reached.
func (t *Tracker) Begin() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.rolloverLocked()
	if t.halt != haltNone {
		return fmt.Errorf("%w: %s", ErrBudgetExceeded, t.haltReason)
	}
	if t.spentUSD >= t.capUSD {
		t.haltLocked(haltCap, fmt.Sprintf("spent $%.4f of the $%.2f cap", t.spentUSD, t.capUSD))
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
	t.rolloverLocked()
	t.spentUSD += cost
	t.calls++
	t.saveLocked()
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

	Period    string `json:"period"`
	Reset     string `json:"reset"`
	ResetsAt  string `json:"resets_at,omitempty"`
	Persisted bool   `json:"persisted"`
	SaveError string `json:"save_error,omitempty"`
}

// Snapshot reads the whole ledger under one lock, so the numbers are
// consistent with each other even if stale by the time they are read.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.rolloverLocked()
	remaining := t.capUSD - t.spentUSD
	if remaining < 0 {
		remaining = 0
	}

	s := Snapshot{
		CapUSD:        t.capUSD,
		SpentUSD:      t.spentUSD,
		RemainingUSD:  remaining,
		Calls:         t.calls,
		UnpricedCalls: t.unpriced,
		Halted:        t.halt != haltNone,
		HaltReason:    t.haltReason,
		Period:        t.period,
		Reset:         t.reset.String(),
		Persisted:     t.ledger != nil,
		SaveError:     t.saveErr,
	}
	if t.reset == ResetMonthly {
		s.ResetsAt = nextMonth(t.now()).Format("2006-01-02")
	}
	return s
}

func (t *Tracker) fail(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.unpriced++
	t.haltLocked(haltUnpriced, reason)
	return fmt.Errorf("%w: %s", ErrUnpriced, reason)
}

// haltLocked latches the first stop. Callers must hold t.mu.
func (t *Tracker) haltLocked(kind haltKind, reason string) {
	if t.halt == haltNone {
		t.halt = kind
		t.haltReason = reason
	}
}

// rolloverLocked starts a new period when the calendar has moved on. A cap
// stop and an unreadable-record stop both lift, since a new period's spend
// is genuinely zero. An unpriced stop does not. Callers must hold t.mu.
func (t *Tracker) rolloverLocked() {
	p := t.periodFor(t.now())
	if p == t.period {
		return
	}

	t.period = p
	t.spentUSD = 0
	t.calls = 0
	if t.halt == haltCap || t.halt == haltRecord {
		t.halt = haltNone
		t.haltReason = ""
	}
	t.logf("spend: new period %s, total reset to $0", p)
	t.saveLocked()
}

// saveLocked writes the current total to disk if persistence is on. A
// failed save does not stop calls, the cap is still enforced in memory.
// Callers must hold t.mu.
func (t *Tracker) saveLocked() {
	if t.ledger == nil {
		return
	}

	err := t.ledger.save(record{
		Period:    t.period,
		SpentUSD:  t.spentUSD,
		Calls:     t.calls,
		UpdatedAt: t.now().UTC(),
	})
	switch {
	case err != nil && t.saveErr == "":
		t.saveErr = err.Error()
		t.logf("spend: %v, the cap is still enforced but the total will not survive a restart", err)
	case err != nil:
		t.saveErr = err.Error()
	case t.saveErr != "":
		t.saveErr = ""
		t.logf("spend: saving works again")
	}
}

func (t *Tracker) periodFor(now time.Time) string {
	if t.reset == ResetNever {
		return "all"
	}
	return now.UTC().Format("2006-01")
}

// nextMonth is the first instant of the month after now, UTC.
func nextMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
