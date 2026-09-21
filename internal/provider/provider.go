// Package provider defines the Provider interface: something you send an
// opaque request body to and get an opaque response body back from, plus
// enough token accounting to price the call.
//
// Seatbelt is a passthrough, not a translator. A Provider implementation
// only handles where to send the request, how to authenticate it, and how
// to read token usage out of the response. Everything else is bytes it
// must not touch.
package provider

import (
	"context"
	"net/http"
)

// Provider is the interface the worker pool, the budget tracker and the
// HTTP handlers all depend on. Nothing outside this package mentions
// Anthropic or OpenAI by name.
type Provider interface {
	Name() string

	// Send forwards one request upstream. A 400 or 429 from the provider
	// is a valid Response returned with err == nil and the real status
	// code intact. A non-nil error means no response arrived at all.
	Send(ctx context.Context, req Request) (*Response, error)

	// Price returns the per-token pricing for one model. ok is false for
	// a model with no known price.
	Price(model string) (Price, bool)
}

// Request is one inbound call to forward upstream.
type Request struct {
	// Body is the caller's JSON, forwarded byte for byte.
	Body []byte
}

// Response is what the upstream provider returned.
type Response struct {
	StatusCode int

	// Header is the full upstream header set. The HTTP server layer
	// decides which are safe to copy back (hop-by-hop headers must not
	// be forwarded).
	Header http.Header

	Body []byte

	// Usage is the token accounting extracted from Body, if present. See
	// Usage.Reported.
	Usage Usage
}

// Usage is the token accounting for one completed call, normalized across
// providers. The four token fields are disjoint, each token is counted in
// exactly one of them. Anthropic reports cache tokens separately from
// input_tokens, OpenAI reports cached tokens as a subset of prompt_tokens
// and the adapter subtracts them out.
type Usage struct {
	// Model is the model the provider reported actually serving the
	// request, which is not always the one the caller asked for.
	Model string

	InputTokens int

	// CacheWrite5mTokens and CacheWrite1hTokens are prompt tokens written
	// into a provider cache, billed at a premium that differs by cache
	// lifetime. OpenAI does not charge for cache writes and leaves both
	// at zero.
	CacheWrite5mTokens int
	CacheWrite1hTokens int

	// CacheReadTokens is prompt input served from cache, billed at a
	// discount.
	CacheReadTokens int

	OutputTokens int
}

// TotalInputTokens is every prompt-side token, however it was billed.
func (u Usage) TotalInputTokens() int {
	return u.InputTokens + u.CacheWrite5mTokens + u.CacheWrite1hTokens + u.CacheReadTokens
}

// Reported says whether the provider told us what the call cost. A
// successful call reporting no usage is not a free call, callers must
// check this before treating cost as zero.
func (u Usage) Reported() bool {
	return u.Model != "" && (u.TotalInputTokens() > 0 || u.OutputTokens > 0)
}

// Price is what one model costs, per million tokens, matching how both
// providers publish their rates.
type Price struct {
	InputPerMTok        float64
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64
	CacheReadPerMTok    float64
	OutputPerMTok       float64
}

// Cost prices one call in US dollars.
func (p Price) Cost(u Usage) float64 {
	return (float64(u.InputTokens)*p.InputPerMTok +
		float64(u.CacheWrite5mTokens)*p.CacheWrite5mPerMTok +
		float64(u.CacheWrite1hTokens)*p.CacheWrite1hPerMTok +
		float64(u.CacheReadTokens)*p.CacheReadPerMTok +
		float64(u.OutputTokens)*p.OutputPerMTok) / 1_000_000
}

// Pricer supplies per-model rates. It is injected because prices change on
// their own schedule and internal/pricing can replace the whole table at
// runtime. A provider with no Pricer prices nothing, and the budget
// tracker halts rather than proceeding silently.
type Pricer interface {
	Price(model string) (Price, bool)
}

// Options tunes a provider implementation. The zero value is the
// production configuration.
type Options struct {
	// Pricer supplies per-model rates. Nil means nothing can be priced.
	Pricer Pricer

	// BaseURL replaces the provider's real API origin. Empty means the
	// real API.
	BaseURL string

	// HTTPClient replaces the default client.
	HTTPClient *http.Client
}
