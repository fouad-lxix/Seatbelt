// Command gen refreshes Seatbelt's price file from a community-maintained
// dataset.
//
// Run it with:
//
//	go generate ./internal/pricing
//
// It merges upstream into our file rather than replacing it, since
// upstream coverage is uneven and lags new model launches. It refuses to
// write a file that would fail validation. Every change is printed as a
// summary, review it before committing.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/pricing"
)

// upstreamURL is LiteLLM's price dataset.
const upstreamURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Anthropic's published cache multipliers, used to fill in rates upstream
// does not carry.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// upstreamEntry is the subset of LiteLLM's per-model record we read.
// Their figures are per token, ours are per million.
type upstreamEntry struct {
	Provider   string  `json:"litellm_provider"`
	Mode       string  `json:"mode"`
	InputCost  float64 `json:"input_cost_per_token"`
	OutputCost float64 `json:"output_cost_per_token"`
	CacheWrite float64 `json:"cache_creation_input_token_cost"`
	CacheRead  float64 `json:"cache_read_input_token_cost"`

	// Read only to detect models whose cost cannot be expressed in our
	// schema, such as audio or per-search billing.
	InputAudioCost  float64         `json:"input_cost_per_audio_token"`
	OutputAudioCost float64         `json:"output_cost_per_audio_token"`
	SearchCost      json.RawMessage `json:"search_context_cost_per_query"`
}

func (u upstreamEntry) pricesWeCannotExpress() bool {
	if u.InputAudioCost > 0 || u.OutputAudioCost > 0 {
		return true
	}
	if len(u.SearchCost) > 0 && string(u.SearchCost) != "null" {
		return true
	}
	return false
}

// ourFile mirrors the published schema. Redeclared here since this is a
// build tool writing a file, not a consumer of the type.
type ourFile struct {
	SchemaVersion int                            `json:"schema_version"`
	AsOf          string                         `json:"as_of"`
	Note          string                         `json:"note,omitempty"`
	Providers     map[string]map[string]ourEntry `json:"providers"`
}

type ourEntry struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	Output       float64 `json:"output"`
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("genprices: ")

	var (
		url    = flag.String("url", upstreamURL, "upstream price dataset")
		out    = flag.String("out", filepath.Join("..", "..", "pricing", "v1", "prices.json"), "published price file")
		embed  = flag.String("embed", filepath.Join("data", "prices.json"), "embedded copy to keep in step")
		dryRun = flag.Bool("dry-run", false, "report what would change without writing")
		asOf   = flag.String("as-of", time.Now().UTC().Format("2006-01-02"), "date to stamp the file with")
	)
	flag.Parse()

	if err := run(*url, *out, *embed, *asOf, *dryRun); err != nil {
		log.Fatal(err)
	}
}

func run(url, out, embed, asOf string, dryRun bool) error {
	current, err := readOurs(out)
	if err != nil {
		return err
	}

	upstream, err := fetchUpstream(url)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	fmt.Printf("upstream: %d entries\n\n", len(upstream))

	merged, changes := merge(current, upstream, asOf)

	report(changes)
	if len(changes.added)+len(changes.updated) == 0 {
		fmt.Println("nothing to change.")
		return nil
	}

	raw, err := encode(merged)
	if err != nil {
		return err
	}

	if err := pricing.ValidateFile(raw); err != nil {
		return fmt.Errorf("the merged file would be rejected at runtime, it was not written: %w", err)
	}

	if dryRun {
		fmt.Println("\ndry run: nothing written.")
		return nil
	}

	for _, path := range []string{out, embed} {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Printf("wrote %s\n", path)
	}

	fmt.Println("\nReview the diff before committing, and check any DERIVED rate " +
		"against the provider's published page.")
	return nil
}

func readOurs(path string) (ourFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ourFile{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var f ourFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return ourFile{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if f.Providers == nil {
		f.Providers = map[string]map[string]ourEntry{}
	}
	return f, nil
}

func fetchUpstream(url string) (map[string]upstreamEntry, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}

	var all map[string]upstreamEntry
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	return all, nil
}

type changeSet struct {
	added    []string
	updated  []string
	derived  []string // rates computed rather than read
	oursOnly []string // models upstream does not know about
	skipped  []string // models whose cost our schema cannot express
}

// merge folds upstream prices into our file.
func merge(current ourFile, upstream map[string]upstreamEntry, asOf string) (ourFile, changeSet) {
	merged := ourFile{
		SchemaVersion: 1,
		AsOf:          asOf,
		Note: "All figures are US dollars per MILLION tokens. Seatbelt fetches this file " +
			"at runtime, a copy is embedded in the binary as an offline fallback. " +
			"Generated by internal/pricing/gen, review before committing.",
		Providers: map[string]map[string]ourEntry{},
	}
	for name, models := range current.Providers {
		merged.Providers[name] = map[string]ourEntry{}
		for model, e := range models {
			merged.Providers[name][model] = e
		}
	}

	var ch changeSet
	seen := map[string]bool{}

	for key, u := range upstream {
		if (u.Provider == "anthropic" || u.Provider == "openai") &&
			u.Mode == "chat" && u.pricesWeCannotExpress() {
			ch.skipped = append(ch.skipped, key)
		}

		providerName, model, ok := classify(key, u)
		if !ok {
			continue
		}
		seen[providerName+"/"+model] = true

		next := ourEntry{
			Input:     perMillion(u.InputCost),
			Output:    perMillion(u.OutputCost),
			CacheRead: perMillion(u.CacheRead),
		}
		if next.Input <= 0 || next.Output <= 0 {
			continue
		}

		if providerName == "anthropic" {
			// Upstream has no 1-hour cache rate, so it is computed from
			// Anthropic's published multiplier and reported as derived.
			next.CacheWrite5m = perMillion(u.CacheWrite)
			if next.CacheWrite5m == 0 {
				next.CacheWrite5m = round(next.Input * cacheWrite5mMultiplier)
			}
			next.CacheWrite1h = round(next.Input * cacheWrite1hMultiplier)
			ch.derived = append(ch.derived, fmt.Sprintf("%s/%s 1h cache write = $%g", providerName, model, next.CacheWrite1h))
		}

		existing, had := merged.Providers[providerName][model]
		switch {
		case !had:
			merged.Providers[providerName][model] = next
			ch.added = append(ch.added, fmt.Sprintf("%s/%s  in $%g  out $%g",
				providerName, model, next.Input, next.Output))
		case existing != next:
			merged.Providers[providerName][model] = next
			ch.updated = append(ch.updated, fmt.Sprintf(
				"%s/%s  in $%g -> $%g   out $%g -> $%g",
				providerName, model,
				existing.Input, next.Input, existing.Output, next.Output))
		}
	}

	// Models we price that upstream does not know about are kept, not
	// dropped.
	for providerName, models := range merged.Providers {
		for model := range models {
			if !seen[providerName+"/"+model] {
				ch.oursOnly = append(ch.oursOnly, providerName+"/"+model)
			}
		}
	}

	sort.Strings(ch.skipped)
	sort.Strings(ch.added)
	sort.Strings(ch.updated)
	sort.Strings(ch.derived)
	sort.Strings(ch.oursOnly)
	return merged, ch
}

// classify decides whether an upstream key is a first-party chat model we
// care about. Upstream keys include cloud variants such as
// "anthropic.claude-...-v1:0" and "vertex_ai/claude-...", only bare
// first-party IDs are kept.
func classify(key string, u upstreamEntry) (providerName, model string, ok bool) {
	if u.Mode != "chat" {
		return "", "", false
	}
	if containsAny(key, "/", ":", " ") {
		return "", "", false
	}
	if u.pricesWeCannotExpress() {
		return "", "", false
	}

	switch u.Provider {
	case "anthropic":
		if !hasPrefix(key, "claude-") {
			return "", "", false
		}
		return "anthropic", key, true
	case "openai":
		if !hasPrefix(key, "gpt-") && !isReasoningModel(key) {
			return "", "", false
		}
		return "openai", key, true
	default:
		return "", "", false
	}
}

func isReasoningModel(key string) bool {
	// o1, o3, o4-mini and similar.
	return len(key) >= 2 && key[0] == 'o' && key[1] >= '1' && key[1] <= '9'
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

func perMillion(perToken float64) float64 { return round(perToken * 1_000_000) }

// round trims floating-point noise so the file diffs cleanly.
func round(v float64) float64 {
	const places = 1e6
	return float64(int64(v*places+0.5)) / places
}

func report(ch changeSet) {
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Printf("%s (%d)\n", title, len(lines))
		for _, l := range lines {
			fmt.Printf("  %s\n", l)
		}
		fmt.Println()
	}

	section("ADDED", ch.added)
	section("UPDATED", ch.updated)
	section("DERIVED, computed from a published multiplier not read, verify these", ch.derived)
	section("OURS ONLY, upstream does not price these, they were kept", ch.oursOnly)
	section("SKIPPED, audio or per-search pricing our schema cannot express, "+
		"calls to these fail closed rather than being undercounted", ch.skipped)
}

// encode uses MarshalIndent, which sorts map keys so output is
// deterministic and diffs reflect real changes.
func encode(f ourFile) ([]byte, error) {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
