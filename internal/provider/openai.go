package provider

import (
	"bytes"
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
	setAuth := func(h http.Header) {
		h.Set("Authorization", "Bearer "+o.apiKey)
	}

	var resp *Response
	var err error
	if req.Stream {
		body, hide := withStreamUsage(req.Body)
		resp, err = o.transport.postStream(ctx, o.Name(), openAIPath, body, setAuth, &openAIMeter{hideUsage: hide})
	} else {
		resp, err = o.transport.postJSON(ctx, o.Name(), openAIPath, req.Body, setAuth)
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK && resp.Stream == nil {
		resp.Usage = parseOpenAIUsage(resp.Body)
	}
	return resp, nil
}

// OpenAI calls the fields prompt/completion rather than input/output.
type openAITokens struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`

	// Unlike Anthropic's, these cached tokens are already in prompt_tokens.
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// openAIUsage mirrors anthropicUsage.
type openAIUsage struct {
	Model string       `json:"model"`
	Usage openAITokens `json:"usage"`
}

func parseOpenAIUsage(body []byte) Usage {
	var v openAIUsage
	if err := json.Unmarshal(body, &v); err != nil {
		return Usage{}
	}
	return v.Usage.usage(v.Model)
}

func (t openAITokens) usage(model string) Usage {
	// Subtract the cached portion out, since Usage's fields are disjoint.
	cached := t.PromptTokensDetails.CachedTokens
	uncached := t.PromptTokens - cached
	if uncached < 0 {
		cached = t.PromptTokens
		uncached = 0
	}

	return Usage{
		Model:           model,
		InputTokens:     uncached,
		CacheReadTokens: cached,
		OutputTokens:    t.CompletionTokens,
	}
}

// withStreamUsage turns on include_usage. hide is true if the caller did not ask for it.
func withStreamUsage(body []byte) (out []byte, hide bool) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}

	opts := map[string]json.RawMessage{}
	if raw, ok := req["stream_options"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &opts); err != nil {
			return body, false
		}
	}
	var asked bool
	if raw, ok := opts["include_usage"]; ok {
		_ = json.Unmarshal(raw, &asked)
	}
	if asked {
		return body, false
	}

	opts["include_usage"] = json.RawMessage("true")
	rawOpts, err := marshalRaw(opts)
	if err != nil {
		return body, false
	}
	req["stream_options"] = rawOpts
	out, err = marshalRaw(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// marshalRaw encodes without escaping <, > and &.
func marshalRaw(v map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// openAIMeter reads usage from the final event, which has empty choices.
type openAIMeter struct {
	hideUsage bool
	model     string
	tokens    openAITokens
	final     bool
	text      int
}

func (m *openAIMeter) observe(data []byte) bool {
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		return false
	}

	var v struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				FunctionCall struct {
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *openAITokens `json:"usage"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return false
	}

	if v.Model != "" {
		m.model = v.Model
	}
	for _, c := range v.Choices {
		m.text += len(c.Delta.Content) + len(c.Delta.Refusal) + len(c.Delta.FunctionCall.Arguments)
		for _, tc := range c.Delta.ToolCalls {
			m.text += len(tc.Function.Arguments)
		}
	}
	if v.Usage != nil {
		m.tokens = *v.Usage
		m.final = true
		return m.hideUsage && len(v.Choices) == 0
	}
	return false
}

func (m *openAIMeter) usage() (Usage, bool) { return m.tokens.usage(m.model), m.final }

func (m *openAIMeter) textBytes() int { return m.text }

func (o *OpenAI) Price(model string) (Price, bool) {
	if o.pricer == nil {
		return Price{}, false
	}
	return o.pricer.Price(model)
}
