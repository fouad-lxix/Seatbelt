// Package config loads Seatbelt's settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultMaxSpendUSD        = 5.0
	DefaultWorkersPerProvider = 4
	DefaultRateLimitPerMin    = 60
	DefaultMaxQueueDepth      = 64
	DefaultPort               = 8080
)

// Config is the fully resolved, validated configuration for one run. It is
// built once at startup and treated as read-only afterward.
type Config struct {
	// AnthropicAPIKey and OpenAIAPIKey are optional. A provider is enabled
	// only if its key is present.
	AnthropicAPIKey string
	OpenAIAPIKey    string

	// MaxSpendUSD is the hard cumulative spend cap.
	MaxSpendUSD float64

	// WorkersPerProvider is the max requests in flight to that provider
	// at once.
	WorkersPerProvider int

	// RateLimitPerMin is each provider's token bucket refill rate,
	// requests per minute.
	RateLimitPerMin int

	// RateLimitBurst is how much idle allowance may be banked and spent
	// at once. Zero means match WorkersPerProvider.
	RateLimitBurst int

	// MaxQueueDepth is how many jobs may wait before new requests are
	// rejected.
	MaxQueueDepth int

	Port int

	// PricingRefresh controls whether Seatbelt fetches an updated price
	// file at startup and daily thereafter.
	PricingRefresh bool

	// PricingURL overrides where that file is fetched from. Empty means
	// the published default.
	PricingURL string
}

// Load reads configuration from the environment, applies defaults, and
// validates the result. It reports every problem found rather than
// stopping at the first.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		AnthropicAPIKey: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		OpenAIAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
	}

	cfg.MaxSpendUSD = envFloat("MAX_SPEND_USD", DefaultMaxSpendUSD, &problems)
	cfg.WorkersPerProvider = envInt("WORKERS_PER_PROVIDER", DefaultWorkersPerProvider, &problems)
	cfg.RateLimitPerMin = envInt("RATE_LIMIT_PER_PROVIDER", DefaultRateLimitPerMin, &problems)
	cfg.RateLimitBurst = envInt("RATE_LIMIT_BURST", 0, &problems)
	cfg.MaxQueueDepth = envInt("MAX_QUEUE_DEPTH", DefaultMaxQueueDepth, &problems)
	cfg.Port = envInt("PORT", DefaultPort, &problems)
	cfg.PricingRefresh = envBool("PRICING_REFRESH", true, &problems)
	cfg.PricingURL = strings.TrimSpace(os.Getenv("PRICING_URL"))

	if cfg.AnthropicAPIKey == "" && cfg.OpenAIAPIKey == "" {
		problems = append(problems, "no provider API keys set: set ANTHROPIC_API_KEY, OPENAI_API_KEY, or both")
	}
	if cfg.MaxSpendUSD <= 0 {
		problems = append(problems, "MAX_SPEND_USD must be greater than 0")
	}
	if cfg.WorkersPerProvider < 1 {
		problems = append(problems, "WORKERS_PER_PROVIDER must be at least 1")
	}
	if cfg.RateLimitPerMin < 1 {
		problems = append(problems, "RATE_LIMIT_PER_PROVIDER must be at least 1 (requests per minute)")
	}
	if cfg.RateLimitBurst < 0 {
		problems = append(problems, "RATE_LIMIT_BURST must be 0 (match WORKERS_PER_PROVIDER) or greater")
	}
	if cfg.MaxQueueDepth < 1 {
		problems = append(problems, "MAX_QUEUE_DEPTH must be at least 1")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		problems = append(problems, "PORT must be between 1 and 65535")
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}

	if cfg.RateLimitBurst == 0 {
		cfg.RateLimitBurst = cfg.WorkersPerProvider
	}
	return cfg, nil
}

func (c Config) AnthropicEnabled() bool { return c.AnthropicAPIKey != "" }

func (c Config) OpenAIEnabled() bool { return c.OpenAIAPIKey != "" }

// Summary renders the settings for the startup log. API keys are shown
// only as a short fingerprint.
func (c Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "port=%d\n", c.Port)
	fmt.Fprintf(&b, "anthropic=%s\n", keyStatus(c.AnthropicAPIKey))
	fmt.Fprintf(&b, "openai=%s\n", keyStatus(c.OpenAIAPIKey))
	fmt.Fprintf(&b, "max spend=$%.2f\n", c.MaxSpendUSD)
	fmt.Fprintf(&b, "workers per provider=%d\n", c.WorkersPerProvider)
	fmt.Fprintf(&b, "rate limit=%d requests/minute per provider\n", c.RateLimitPerMin)
	fmt.Fprintf(&b, "max queue depth=%d per provider\n", c.MaxQueueDepth)
	if c.PricingRefresh {
		fmt.Fprintf(&b, "pricing=refreshed daily from the published table")
	} else {
		fmt.Fprintf(&b, "pricing=built-in table only (refresh disabled)")
	}
	return b.String()
}

func keyStatus(key string) string {
	if key == "" {
		return "disabled (no API key)"
	}
	if len(key) <= 8 {
		return "enabled (key ...redacted)"
	}
	return fmt.Sprintf("enabled (key ...%s)", key[len(key)-4:])
}

// envInt reads an integer environment variable. An unparseable value is
// recorded as a problem rather than silently replaced by the default.
func envInt(name string, def int, problems *[]string) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s=%q is not a whole number", name, raw))
		return def
	}
	return v
}

func envBool(name string, def bool, problems *[]string) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch raw {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		*problems = append(*problems, fmt.Sprintf(
			"%s=%q is not a yes/no value (try true or false)", name, raw))
		return def
	}
}

func envFloat(name string, def float64, problems *[]string) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	raw = strings.TrimPrefix(raw, "$")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s=%q is not a number", name, raw))
		return def
	}
	return v
}
