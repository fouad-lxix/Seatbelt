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
	setAuth := func(h http.Header) {
		h.Set("X-Api-Key", a.apiKey)
		h.Set("Anthropic-Version", anthropicVersion)
	}

	var resp *Response
	var err error
	if req.Stream {
		resp, err = a.transport.postStream(ctx, a.Name(), anthropicPath, req.Body, setAuth, &anthropicMeter{})
	} else {
		resp, err = a.transport.postJSON(ctx, a.Name(), anthropicPath, req.Body, setAuth)
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK && resp.Stream == nil {
		resp.Usage = parseAnthropicUsage(resp.Body)
	}
	return resp, nil
}

type anthropicTokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// Anthropic reports cache tokens separately from input_tokens.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`

	CacheCreation struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// anthropicUsage is a partial view of the response. encoding/json ignores
// fields not in the struct, so the original bytes returned to the caller
// stay untouched even though only two fields are read here.
type anthropicUsage struct {
	Model string          `json:"model"`
	Usage anthropicTokens `json:"usage"`
}

func parseAnthropicUsage(body []byte) Usage {
	var v anthropicUsage
	if err := json.Unmarshal(body, &v); err != nil {
		return Usage{}
	}
	return v.Usage.usage(v.Model)
}

func (t anthropicTokens) usage(model string) Usage {
	// If the per-lifetime breakdown is absent or does not add up to the
	// reported total, attribute the whole amount to the more expensive
	// bucket. Overestimating is safe for a spend cap, underestimating is
	// not.
	write5m := t.CacheCreation.Ephemeral5m
	write1h := t.CacheCreation.Ephemeral1h
	if write5m+write1h != t.CacheCreationInputTokens {
		write5m, write1h = 0, t.CacheCreationInputTokens
	}

	return Usage{
		Model:              model,
		InputTokens:        t.InputTokens,
		CacheWrite5mTokens: write5m,
		CacheWrite1hTokens: write1h,
		CacheReadTokens:    t.CacheReadInputTokens,
		OutputTokens:       t.OutputTokens,
	}
}

// merge keeps the larger of each count, since streamed counts only grow.
func (t *anthropicTokens) merge(o anthropicTokens) {
	t.InputTokens = max(t.InputTokens, o.InputTokens)
	t.OutputTokens = max(t.OutputTokens, o.OutputTokens)
	t.CacheCreationInputTokens = max(t.CacheCreationInputTokens, o.CacheCreationInputTokens)
	t.CacheReadInputTokens = max(t.CacheReadInputTokens, o.CacheReadInputTokens)
	t.CacheCreation.Ephemeral5m = max(t.CacheCreation.Ephemeral5m, o.CacheCreation.Ephemeral5m)
	t.CacheCreation.Ephemeral1h = max(t.CacheCreation.Ephemeral1h, o.CacheCreation.Ephemeral1h)
}

// anthropicMeter reads usage from message_start and message_delta.
type anthropicMeter struct {
	model  string
	tokens anthropicTokens
	final  bool
	text   int
}

func (m *anthropicMeter) observe(data []byte) bool {
	var v struct {
		Type    string `json:"type"`
		Message struct {
			Model string          `json:"model"`
			Usage anthropicTokens `json:"usage"`
		} `json:"message"`
		Usage *anthropicTokens `json:"usage"`
		Delta struct {
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return false
	}

	switch v.Type {
	case "message_start":
		if v.Message.Model != "" {
			m.model = v.Message.Model
		}
		m.tokens.merge(v.Message.Usage)
	case "message_delta":
		if v.Usage != nil {
			m.tokens.merge(*v.Usage)
			m.final = true
		}
	case "content_block_delta":
		m.text += len(v.Delta.Text) + len(v.Delta.PartialJSON) + len(v.Delta.Thinking)
	}
	return false
}

func (m *anthropicMeter) usage() (Usage, bool) { return m.tokens.usage(m.model), m.final }

func (m *anthropicMeter) textBytes() int { return m.text }

func (a *Anthropic) Price(model string) (Price, bool) {
	if a.pricer == nil {
		return Price{}, false
	}
	return a.pricer.Price(model)
}
