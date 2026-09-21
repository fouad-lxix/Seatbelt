package provider

import (
	"context"
	"encoding/json"
	"net/http"
)

const (
	anthropicBaseURL = "https://api.anthropic.com"
	anthropicPath    = "/v1/messages"
	anthropicVersion = "2023-06-01"
)

// Anthropic proxies the Messages API.
type Anthropic struct {
	apiKey    string
	transport transport
	pricer    Pricer
}

var _ Provider = (*Anthropic)(nil)

// NewAnthropic builds a provider for the given API key. Pass the zero
// Options for production.
func NewAnthropic(apiKey string, opts Options) *Anthropic {
	return &Anthropic{
		apiKey:    apiKey,
		pricer:    opts.Pricer,
		transport: newTransport(anthropicBaseURL, opts),
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Send(ctx context.Context, req Request) (*Response, error) {
	resp, err := a.transport.postJSON(ctx, a.Name(), anthropicPath, req.Body, func(h http.Header) {
		h.Set("X-Api-Key", a.apiKey)
		h.Set("Anthropic-Version", anthropicVersion)
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		resp.Usage = parseAnthropicUsage(resp.Body)
	}
	return resp, nil
}

// anthropicUsage is a partial view of the response. encoding/json ignores
// fields not in the struct, so the original bytes returned to the caller
// stay untouched even though only two fields are read here.
type anthropicUsage struct {
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`

		// Anthropic reports cache tokens separately from input_tokens.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`

		CacheCreation struct {
			Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

func parseAnthropicUsage(body []byte) Usage {
	var v anthropicUsage
	if err := json.Unmarshal(body, &v); err != nil {
		return Usage{}
	}

	// If the per-lifetime breakdown is absent or does not add up to the
	// reported total, attribute the whole amount to the more expensive
	// bucket. Overestimating is safe for a spend cap, underestimating is
	// not.
	write5m := v.Usage.CacheCreation.Ephemeral5m
	write1h := v.Usage.CacheCreation.Ephemeral1h
	if write5m+write1h != v.Usage.CacheCreationInputTokens {
		write5m, write1h = 0, v.Usage.CacheCreationInputTokens
	}

	return Usage{
		Model:              v.Model,
		InputTokens:        v.Usage.InputTokens,
		CacheWrite5mTokens: write5m,
		CacheWrite1hTokens: write1h,
		CacheReadTokens:    v.Usage.CacheReadInputTokens,
		OutputTokens:       v.Usage.OutputTokens,
	}
}

func (a *Anthropic) Price(model string) (Price, bool) {
	if a.pricer == nil {
		return Price{}, false
	}
	return a.pricer.Price(model)
}
