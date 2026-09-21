package pricing

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/provider"
)

//go:embed data/prices.json
var embedded []byte

// DefaultURL is where a newer price file is published. It points at a
// branch, not a pinned commit, since this file is our own data and
// updating it is the point.
const DefaultURL = "https://raw.githubusercontent.com/fouad-lxix/seatbelt/main/pricing/v1/prices.json"

const (
	// DefaultInterval is how often the file is re-fetched.
	DefaultInterval = 24 * time.Hour

	// fetchTimeout bounds a refresh so a slow fetch never delays startup.
	fetchTimeout = 10 * time.Second

	// maxFileBytes stops a wrong URL from streaming something enormous
	// into memory.
	maxFileBytes = 4 << 20 // 4 MiB
)

// Options configures a Store. The zero value is a working production
// configuration.
type Options struct {
	// URL to fetch newer prices from. Empty means DefaultURL.
	URL string

	// Interval between refreshes. Zero means DefaultInterval.
	Interval time.Duration

	// Disabled turns off fetching entirely, leaving the embedded table
	// in force.
	Disabled bool

	// HTTPClient is the client used to fetch. Nil means a client with
	// fetchTimeout.
	HTTPClient *http.Client

	// Logf receives one line per refresh outcome. Nil means the
	// standard logger.
	Logf func(format string, args ...any)
}

// Store holds the price table currently in force and keeps it fresh.
//
// The table is held in an atomic.Pointer and swapped wholesale on
// refresh, so readers take no lock and never observe a half-updated
// table.
type Store struct {
	current atomic.Pointer[Table]

	url      string
	interval time.Duration
	disabled bool
	client   *http.Client
	logf     func(format string, args ...any)

	// started lets Stop be safe to call on a Store that was never
	// started, since the natural usage is defer store.Stop() right
	// after construction.
	started   atomic.Bool
	startOnce sync.Once

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// New builds a Store serving the embedded price table immediately.
func New(opts Options) (*Store, error) {
	base, err := parse(embedded, "built-in")
	if err != nil {
		return nil, fmt.Errorf("the built-in price table is unreadable: %w", err)
	}
	if err := validate(base, nil); err != nil {
		return nil, fmt.Errorf("the built-in price table is invalid: %w", err)
	}

	s := &Store{
		url:      opts.URL,
		interval: opts.Interval,
		disabled: opts.Disabled,
		client:   opts.HTTPClient,
		logf:     opts.Logf,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if s.url == "" {
		s.url = DefaultURL
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: fetchTimeout}
	}
	if s.logf == nil {
		s.logf = log.Printf
	}

	s.current.Store(base)
	return s, nil
}

// Table returns the price table currently in force. It never returns nil.
func (s *Store) Table() *Table { return s.current.Load() }

// Start performs an initial refresh and keeps refreshing on an interval
// until Stop is called. The initial refresh runs in the background since
// the embedded table is already serving.
func (s *Store) Start() {
	s.startOnce.Do(func() {
		s.started.Store(true)

		if s.disabled {
			t := s.Table()
			s.logf("pricing: refresh disabled, using the built-in table from %s (%d models)",
				t.AsOf(), t.Count())
			close(s.done)
			return
		}

		go s.loop()
	})
}

// Stop ends background refreshing and waits for it to finish. Safe to
// call more than once, or on a Store that was never started.
func (s *Store) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		<-s.done
	}
}

func (s *Store) loop() {
	defer close(s.done)

	s.refresh()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.refresh()
		}
	}
}

// refresh fetches, validates, and only if both succeed swaps in a new
// table. Every failure path here is non-fatal: the previous table stays
// in force and one line explains why.
func (s *Store) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	raw, err := s.fetch(ctx)
	if err != nil {
		s.keepCurrent("could not fetch", err)
		return
	}

	next, err := parse(raw, s.url)
	if err != nil {
		s.keepCurrent("could not read", err)
		return
	}

	previous := s.Table()
	if err := validate(next, previous); err != nil {
		s.keepCurrent("rejected", err)
		return
	}

	unchanged := next.AsOf() == previous.AsOf() && next.Count() == previous.Count()

	// Swap even when contents match, so Source reflects where the
	// numbers actually came from and a failing refresh is not confused
	// with one that simply had nothing new to say.
	s.current.Store(next)

	if unchanged {
		return
	}

	s.logf("pricing: updated from %s (%d models, as of %s, was %s from %s)",
		s.url, next.Count(), next.AsOf(), previous.AsOf(), previous.Source())
}

func (s *Store) keepCurrent(what string, err error) {
	t := s.Table()
	s.logf("pricing: %s the published price file (%v), continuing with the %s table from %s",
		what, err, sourceLabel(t), t.AsOf())
}

func sourceLabel(t *Table) string {
	if t.Source() == "built-in" {
		return "built-in"
	}
	return "last good"
}

func (s *Store) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("file exceeds the %d MiB limit", maxFileBytes>>20)
	}
	return raw, nil
}

// For returns a lookup bound to one provider, satisfying provider.Pricer.
// It reads the Store's current table on every call, so it always prices
// against whatever table is in force.
func (s *Store) For(providerName string) provider.Pricer {
	return pricerFunc(func(model string) (provider.Price, bool) {
		return s.Table().Lookup(providerName, model)
	})
}

type pricerFunc func(model string) (provider.Price, bool)

func (f pricerFunc) Price(model string) (provider.Price, bool) { return f(model) }
