package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync/atomic"
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

	// streamClient has no overall limit, firstByte and idle bound it instead.
	streamClient *http.Client
	firstByte    time.Duration
	idle         time.Duration
}

func newTransport(defaultBaseURL string, opts Options) transport {
	t := transport{
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: defaultRequestTimeout},
		idle:    defaultStreamIdleTimeout,
	}
	if opts.BaseURL != "" {
		t.baseURL = opts.BaseURL
	}
	if opts.HTTPClient != nil {
		t.client = opts.HTTPClient
	}
	if opts.StreamIdleTimeout > 0 {
		t.idle = opts.StreamIdleTimeout
	}

	sc := *t.client
	sc.Timeout = 0
	t.streamClient = &sc
	t.firstByte = t.client.Timeout
	if t.firstByte <= 0 {
		t.firstByte = defaultRequestTimeout
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

// postStream returns an open Stream for an event stream, otherwise the whole body.
func (t transport) postStream(
	ctx context.Context,
	name, path string,
	body []byte,
	setAuth func(h http.Header),
	m meter,
) (*Response, error) {
	ctx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s: building request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// A compressed stream is held back by the decompressor.
	req.Header.Set("Accept-Encoding", "identity")
	setAuth(req.Header)

	// Bounds the wait for headers, and for the whole body if it is not a stream.
	var timedOut atomic.Bool
	timer := time.AfterFunc(t.firstByte, func() {
		timedOut.Store(true)
		cancel()
	})

	resp, err := t.streamClient.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		if timedOut.Load() {
			return nil, fmt.Errorf("%s: no response within %s", name, t.firstByte)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	if resp.StatusCode != http.StatusOK || !isEventStream(resp.Header) {
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		timer.Stop()
		cancel()
		if err != nil {
			if timedOut.Load() {
				return nil, fmt.Errorf("%s: no response within %s", name, t.firstByte)
			}
			return nil, fmt.Errorf("%s: reading response body: %w", name, err)
		}
		return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: raw}, nil
	}

	if !timer.Stop() {
		// The limit ran out just as the headers arrived.
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%s: no response within %s", name, t.firstByte)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Stream:     newStream(resp.Body, cancel, m, readRequestInfo(body), t.idle),
	}, nil
}

func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}
