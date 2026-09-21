// Package server exposes the gateway over HTTP. The proxy endpoints are
// full-fidelity passthroughs, Seatbelt's own errors are the only
// responses it invents and they are clearly marked as its own.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/budget"
	"github.com/fouad-lxix/seatbelt/internal/config"
	"github.com/fouad-lxix/seatbelt/internal/pool"
	"github.com/fouad-lxix/seatbelt/internal/pricing"
	"github.com/fouad-lxix/seatbelt/internal/provider"
	"github.com/fouad-lxix/seatbelt/internal/ratelimit"
)

// maxRequestBytes caps how large an inbound body may be, so an oversized
// request cannot exhaust the process.
const maxRequestBytes = 10 << 20 // 10 MiB

// Route ties one or more inbound paths to the provider pool that serves
// them.
type Route struct {
	Path     string // the documented path, e.g. /v1/anthropic/messages
	Aliases  []string
	Provider string // the pool key, e.g. "anthropic"
	Upstream string // named in errors, e.g. "Anthropic Messages API"

	// SDKBaseURL is the value to give an official SDK's base_url so the
	// path it builds lands on one of the paths above.
	SDKBaseURL string
}

// Aliases exist because the OpenAI and Anthropic SDKs build request paths
// differently from whatever base_url they are given, so the gateway
// answers on every shape a plausible base_url produces.
var routes = []Route{
	{
		Path: "/v1/anthropic/messages",
		Aliases: []string{
			"/v1/anthropic/v1/messages",
			"/anthropic/v1/messages",
		},
		Provider:   "anthropic",
		Upstream:   "Anthropic Messages API",
		SDKBaseURL: "/v1/anthropic",
	},
	{
		Path: "/v1/openai/chat/completions",
		Aliases: []string{
			"/v1/openai/v1/chat/completions",
			"/openai/v1/chat/completions",
		},
		Provider:   "openai",
		Upstream:   "OpenAI Chat Completions API",
		SDKBaseURL: "/v1/openai",
	},
}

// Routes reports the configured proxy routes.
func Routes() []Route { return routes }

// Deps is everything the handlers need.
type Deps struct {
	Config config.Config

	// Pools is keyed by provider name. A provider with no entry is
	// treated as not configured.
	Pools map[string]*pool.Pool

	// Limiters is keyed the same way, read only to report headroom on
	// /stats.
	Limiters map[string]*ratelimit.Bucket

	Budget *budget.Tracker

	// Pricing reports which price table is in force and how old it is.
	Pricing *pricing.Store
}

// Server holds everything the handlers need. It owns nothing, the pools,
// limiters and budget tracker are built in main.
type Server struct {
	deps    Deps
	started time.Time
}

func New(d Deps) *Server {
	return &Server{deps: d, started: time.Now()}
}

// ShutdownPools stops every provider's workers, letting queued and
// running jobs finish, and returns once they have stopped or ctx expires.
//
// This must run after http.Server.Shutdown, not before: handlers are
// blocked in pool.Submit waiting on the workers, so stopping workers
// first would strand every request mid-flight.
func (s *Server) ShutdownPools(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for name, p := range s.deps.Pools {
		wg.Add(1)
		go func(name string, p *pool.Pool) {
			defer wg.Done()
			if err := p.Shutdown(ctx); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			writeLogf("%s: workers stopped", name)
		}(name, p)
	}
	wg.Wait()

	return errors.Join(errs...)
}

// Handler returns the routed handler for the whole gateway.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	for _, rt := range routes {
		rt := rt
		handler := func(w http.ResponseWriter, r *http.Request) {
			s.proxy(w, r, rt)
		}
		for _, path := range append([]string{rt.Path}, rt.Aliases...) {
			mux.HandleFunc("POST "+path, handler)
		}
	}

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /stats", s.statsJSON)
	mux.HandleFunc("GET /stats/ui", s.statsUI)

	return logRequests(mux)
}

// proxy reads the caller's body, hands it to the right provider's queue,
// and writes back whatever came out.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, rt Route) {
	p, ok := s.deps.Pools[rt.Provider]
	if !ok || p == nil {
		writeError(w, http.StatusServiceUnavailable, "provider_not_configured", fmt.Sprintf(
			"%s is not configured, set %s_API_KEY and restart the gateway",
			rt.Provider, upper(rt.Provider)))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds the %d MiB limit", maxRequestBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "unreadable_request",
			"could not read the request body: "+err.Error())
		return
	}

	// r.Context() is cancelled when the client disconnects, letting a
	// worker skip a job whose caller has gone.
	resp, err := p.Submit(r.Context(), provider.Request{Body: body})
	if err != nil {
		s.writeSubmitError(w, r, rt, err)
		return
	}

	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(resp.Body); err != nil {
		log.Printf("%s: could not write response to client: %v", rt.Provider, err)
	}
}

// writeSubmitError turns one of the pool's sentinel errors into an HTTP
// status the caller can act on.
func (s *Server) writeSubmitError(w http.ResponseWriter, r *http.Request, rt Route, err error) {
	switch {
	case errors.Is(err, budget.ErrBudgetExceeded), errors.Is(err, budget.ErrUnpriced):
		w.Header().Set("X-Seatbelt-Budget", "exceeded")
		writeError(w, http.StatusPaymentRequired, "budget_exceeded", err.Error())

	case errors.Is(err, pool.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "queue_full", fmt.Sprintf(
			"%s has %d requests queued, which is the configured maximum, retry shortly",
			rt.Provider, s.deps.Config.MaxQueueDepth))

	case errors.Is(err, pool.ErrShuttingDown):
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusServiceUnavailable, "shutting_down",
			"the gateway is shutting down and is not accepting new requests")

	case errors.Is(err, context.Canceled):
		log.Printf("%s: client disconnected before the response was ready", rt.Provider)

	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", fmt.Sprintf(
			"the %s did not respond in time", rt.Upstream))

	default:
		writeError(w, http.StatusBadGateway, "upstream_unreachable", fmt.Sprintf(
			"could not reach the %s: %v", rt.Upstream, err))
	}
}

// health is the liveness check. It reports nothing about providers or
// spend, so a bad minute upstream does not restart a healthy process.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.started).Round(time.Second).String(),
	})
}

// hopByHop headers describe a single network hop, not the message, and
// must never be forwarded.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Length":      true,
}

// copyUpstreamHeaders forwards everything except hop-by-hop headers, so
// the caller sees the provider's own headers including ones invented
// after this code was written.
func copyUpstreamHeaders(dst, src http.Header) {
	for name, values := range src {
		if hopByHop[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// errorBody is Seatbelt's own error shape, deliberately unlike either
// provider's, so a gateway-side failure is obvious at a glance.
type errorBody struct {
	Error struct {
		Source  string `json:"source"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	var body errorBody
	body.Error.Source = "seatbelt"
	body.Error.Type = kind
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeLogf(format string, args ...any) { log.Printf(format, args...) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("could not encode response: %v", err)
	}
}

// statusRecorder remembers what status was written, since
// http.ResponseWriter offers no way to read it back.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		if r.URL.Path == "/healthz" {
			return
		}
		log.Printf("%s %s -> %d in %s",
			r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
