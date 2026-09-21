package pool

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/provider"
)

const (
	// defaultMaxAttempts counts the first try, so 3 means one attempt
	// plus two retries.
	defaultMaxAttempts = 3

	defaultBaseBackoff = 500 * time.Millisecond
	defaultMaxBackoff  = 8 * time.Second

	// maxHonoredRetryAfter caps a provider-supplied Retry-After so a
	// provider cannot park a worker for an hour.
	maxHonoredRetryAfter = 60 * time.Second
)

type retryPolicy struct {
	maxAttempts int
	base        time.Duration
	max         time.Duration
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		maxAttempts: defaultMaxAttempts,
		base:        defaultBaseBackoff,
		max:         defaultMaxBackoff,
	}
}

// retryable reports whether a failed attempt is worth repeating. err means
// no response arrived at all. resp means the provider answered and it was
// not good enough.
func retryable(resp *provider.Response, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true
	case resp.StatusCode >= 500:
		return true
	default:
		// 400, 401, 404, 422 and similar will fail identically every
		// time. Retrying wastes time and hides the real problem.
		return false
	}
}

// delay returns how long to wait before the next attempt.
func (rp retryPolicy) delay(attempt int, resp *provider.Response) time.Duration {
	if d, ok := retryAfter(resp); ok {
		if d > maxHonoredRetryAfter {
			d = maxHonoredRetryAfter
		}
		return d
	}

	d := rp.base << (attempt - 1)
	if d <= 0 || d > rp.max { // d <= 0 catches shift overflow
		d = rp.max
	}

	// Equal jitter: at least half the computed delay, plus a random
	// amount up to the other half. Full jitter (0 to d) is more common
	// but can retry almost immediately, the wrong response to a 429.
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryAfter reads the Retry-After header. Only the delta-seconds form is
// handled.
func retryAfter(resp *provider.Response) (time.Duration, bool) {
	if resp == nil || resp.Header == nil {
		return 0, false
	}
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}
