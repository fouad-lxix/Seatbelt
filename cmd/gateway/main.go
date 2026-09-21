// Command gateway is the Seatbelt entrypoint. It reads configuration,
// constructs the pieces, wires them together, and runs the HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/budget"
	"github.com/fouad-lxix/seatbelt/internal/config"
	"github.com/fouad-lxix/seatbelt/internal/pool"
	"github.com/fouad-lxix/seatbelt/internal/pricing"
	"github.com/fouad-lxix/seatbelt/internal/provider"
	"github.com/fouad-lxix/seatbelt/internal/ratelimit"
	"github.com/fouad-lxix/seatbelt/internal/server"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("seatbelt: ")

	if err := run(); err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

// run holds the real work so it can return an error instead of calling
// os.Exit deep inside the program.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	fmt.Println("Seatbelt configuration")
	for _, line := range strings.Split(cfg.Summary(), "\n") {
		fmt.Println("  " + line)
	}
	fmt.Println()

	prices, err := pricing.New(pricing.Options{
		URL:      cfg.PricingURL,
		Disabled: !cfg.PricingRefresh,
	})
	if err != nil {
		return err
	}
	prices.Start()
	defer prices.Stop()

	// One ledger for the whole gateway, the cap is on total spend, not
	// per provider.
	tracker := budget.New(cfg.MaxSpendUSD)

	pools, limiters := buildPools(cfg, tracker, prices)
	if len(pools) == 0 {
		return fmt.Errorf("no providers enabled: set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	gw := server.New(server.Deps{
		Config:   cfg,
		Pools:    pools,
		Limiters: limiters,
		Budget:   tracker,
		Pricing:  prices,
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: gw.Handler(),

		// No WriteTimeout: model calls legitimately take a long time.
		// The caller's own context bounds request duration instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	printBanner(cfg, pools)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server stopped: %w", err)
		}
		return nil

	case <-ctx.Done():
		// stop() restores the default signal behavior, so a second
		// Ctrl-C kills the process immediately.
		stop()
		log.Println("shutting down, finishing in-flight requests " +
			"(press Ctrl-C again to stop immediately)")
	}

	return shutdown(srv, gw)
}

// shutdownTimeout bounds the whole drain. Docker sends SIGKILL ten
// seconds after SIGTERM by default, use "docker stop -t 30" for this to
// be honored in full.
const shutdownTimeout = 20 * time.Second

// shutdown drains the gateway: the listener closes first, then the
// workers stop once every handler has returned.
func shutdown(srv *http.Server, gw *server.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("some requests did not finish within %s, closing anyway", shutdownTimeout)
		_ = srv.Close()
	}

	if err := gw.ShutdownPools(ctx); err != nil {
		log.Printf("%v", err)
	}

	log.Println("stopped cleanly")
	return nil
}

// buildPools creates one worker pool per enabled provider, each with its
// own queue, workers, and rate limiter.
func buildPools(cfg config.Config, tracker *budget.Tracker, prices *pricing.Store) (map[string]*pool.Pool, map[string]*ratelimit.Bucket) {
	pools := make(map[string]*pool.Pool)
	limiters := make(map[string]*ratelimit.Bucket)

	add := func(p provider.Provider) {
		bucket := ratelimit.New(cfg.RateLimitPerMin, cfg.RateLimitBurst)
		limiters[p.Name()] = bucket

		pl := pool.New(p, pool.Options{
			Workers:    cfg.WorkersPerProvider,
			QueueDepth: cfg.MaxQueueDepth,
			Limiter:    bucket,
			Budget:     tracker,
		})
		pl.Start()
		pools[p.Name()] = pl
		log.Printf("%s enabled: %d workers, %d req/min (burst %d), queue depth %d",
			p.Name(), cfg.WorkersPerProvider, cfg.RateLimitPerMin,
			cfg.RateLimitBurst, cfg.MaxQueueDepth)
	}

	if cfg.AnthropicEnabled() {
		add(provider.NewAnthropic(cfg.AnthropicAPIKey, provider.Options{
			Pricer: prices.For("anthropic"),
		}))
	}
	if cfg.OpenAIEnabled() {
		add(provider.NewOpenAI(cfg.OpenAIAPIKey, provider.Options{
			Pricer: prices.For("openai"),
		}))
	}
	return pools, limiters
}

// printBanner tells the operator exactly what to point their client at.
func printBanner(cfg config.Config, pools map[string]*pool.Pool) {
	origin := fmt.Sprintf("http://localhost:%d", cfg.Port)

	log.Printf("listening on %s", origin)
	for _, rt := range server.Routes() {
		if _, ok := pools[rt.Provider]; !ok {
			continue
		}
		log.Printf("  POST %s%s", origin, rt.Path)
		log.Printf("       SDK base URL: %s%s", origin, rt.SDKBaseURL)
	}
	log.Printf("  GET  %s/stats", origin)
	log.Printf("  GET  %s/stats/ui", origin)
	log.Printf("  GET  %s/healthz", origin)
}
