package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultRequestTimeout is a backstop per upstream attempt. It is generous
// because LLM calls legitimately take a long time. The caller's context
// provides the real deadline.
const defaultRequestTimeout = 120 * time.Second

// transport holds the plumbing every provider shares.
type transport struct {
	baseURL string
	client  *http.Client
}

func newTransport(defaultBaseURL string, opts Options) transport {
	t := transport{
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: defaultRequestTimeout},
	}
	if opts.BaseURL != "" {
		t.baseURL = opts.BaseURL
	}
	if opts.HTTPClient != nil {
		t.client = opts.HTTPClient
	}
	return t
}

// postJSON sends body to baseURL+path and returns the raw result.
func (t transport) postJSON(
	ctx context.Context,
	name, path string,
	body []byte,
	setAuth func(h http.Header),
) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setAuth(req.Header)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: reading response body: %w", name, err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       raw,
	}, nil
}
