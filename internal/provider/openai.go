package provider

import (
	"context"
	"encoding/json"
	"net/http"
)

const (
	openAIBaseURL = "https://api.openai.com"
	openAIPath    = "/v1/chat/completions"
)

// OpenAI proxies the Chat Completions API.
type OpenAI struct {
	apiKey    string
	transport transport
	pricer    Pricer
}

var _ Provider = (*OpenAI)(nil)

// NewOpenAI builds a provider for the given API key. Pass the zero Options
// for production.
func NewOpenAI(apiKey string, opts Options) *OpenAI {
	return &OpenAI{
		apiKey:    apiKey,
		pricer:    opts.Pricer,
		transport: newTransport(openAIBaseURL, opts),
	}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Send(ctx context.Context, req Request) (*Response, error) {
	resp, err := o.transport.postJSON(ctx, o.Name(), openAIPath, req.Body, func(h http.Header) {
		h.Set("Authorization", "Bearer "+o.apiKey)
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		resp.Usage = parseOpenAIUsage(resp.Body)
	}
	return resp, nil
}

// openAIUsage mirrors anthropicUsage. OpenAI calls the fields
// prompt/completion rather than input/output.
type openAIUsage struct {
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`

		// Unlike Anthropic's, these cached tokens are already included
		// in prompt_tokens.
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func parseOpenAIUsage(body []byte) Usage {
	var v openAIUsage
	if err := json.Unmarshal(body, &v); err != nil {
		return Usage{}
	}

	// Subtract the cached portion out, since Usage's fields are disjoint.
	cached := v.Usage.PromptTokensDetails.CachedTokens
	uncached := v.Usage.PromptTokens - cached
	if uncached < 0 {
		cached = v.Usage.PromptTokens
		uncached = 0
	}

	return Usage{
		Model:           v.Model,
		InputTokens:     uncached,
		CacheReadTokens: cached,
		OutputTokens:    v.Usage.CompletionTokens,
	}
}

func (o *OpenAI) Price(model string) (Price, bool) {
	if o.pricer == nil {
		return Price{}, false
	}
	return o.pricer.Price(model)
}
