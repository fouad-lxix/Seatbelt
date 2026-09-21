// Package pricing owns what each model costs and keeps that current
// without Seatbelt itself needing an upgrade.
//
// An embedded copy always works offline. A newer copy is fetched at
// startup and once a day after that. Nothing fetched is trusted until it
// passes validation, and anything that fails leaves the previous table in
// force.
package pricing

//go:generate go run ./gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fouad-lxix/seatbelt/internal/provider"
)

// SchemaVersion is the price file format this build understands. The
// published file lives under a versioned path so a breaking format change
// can ship at v2 while deployed binaries keep reading v1.
const SchemaVersion = 1

// file is the on-disk and over-the-wire shape of a price file. Every
// figure is US dollars per million tokens.
type file struct {
	SchemaVersion int    `json:"schema_version"`
	AsOf          string `json:"as_of"`
	Note          string `json:"note,omitempty"`

	Providers map[string]map[string]entry `json:"providers"`
}

// entry is one model's rates. Absent fields are zero, correct for rates a
// provider does not charge.
type entry struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	Output       float64 `json:"output"`
}

// Table is an immutable set of prices. A refresh builds a whole new Table
// and swaps it in, which is what lets readers work without locking.
type Table struct {
	asOf   string
	source string
	models map[string]map[string]provider.Price
}

func (t *Table) AsOf() string { return t.asOf }

// Source describes where the table came from: "built-in" or a URL.
func (t *Table) Source() string { return t.source }

func (t *Table) Count() int {
	n := 0
	for _, models := range t.models {
		n += len(models)
	}
	return n
}

func (t *Table) Providers() []string {
	names := make([]string, 0, len(t.models))
	for name := range t.models {
		names = append(names, name)
	}
	return names
}

// Lookup finds the price for one provider's model, tolerating dated
// snapshots. Providers often return a dated snapshot of the requested
// alias, so lookup tries an exact match first, then the longest key that
// is a prefix of the model ID followed by a hyphen.
//
// Longest prefix wins rather than first match, since "gpt-4o" is a prefix
// of "gpt-4o-mini" and the two are priced very differently. A hyphen must
// follow the prefix so "gpt-5" cannot claim "gpt-5.1".
//
// An unknown family returns false and the budget tracker halts, rather
// than guessing.
func (t *Table) Lookup(providerName, model string) (provider.Price, bool) {
	models, ok := t.models[providerName]
	if !ok {
		return provider.Price{}, false
	}
	if p, ok := models[model]; ok {
		return p, true
	}

	best := ""
	for key := range models {
		if len(key) >= len(model) {
			continue
		}
		if !strings.HasPrefix(model, key) {
			continue
		}
		if model[len(key)] != '-' {
			continue
		}
		if len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return provider.Price{}, false
	}
	return models[best], true
}

// ValidateFile checks a price file's raw bytes against every rule a
// fetched file must satisfy. Exported so the generator and CI can reject a
// bad file before it reaches a running gateway.
func ValidateFile(raw []byte) error {
	t, err := parse(raw, "file")
	if err != nil {
		return err
	}
	return validate(t, nil)
}

func parse(raw []byte, source string) (*Table, error) {
	// Strip a UTF-8 BOM if present. encoding/json rejects one, and this
	// file is meant to be hand-edited, where a BOM is common.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("price file from %s is not valid JSON: %w", source, err)
	}
	if f.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf(
			"price file from %s uses schema version %d, this build of Seatbelt understands version %d",
			source, f.SchemaVersion, SchemaVersion)
	}
	if f.AsOf == "" {
		return nil, fmt.Errorf("price file from %s has no as_of date", source)
	}

	t := &Table{
		asOf:   f.AsOf,
		source: source,
		models: make(map[string]map[string]provider.Price, len(f.Providers)),
	}
	for providerName, models := range f.Providers {
		converted := make(map[string]provider.Price, len(models))
		for model, e := range models {
			converted[model] = provider.Price{
				InputPerMTok:        e.Input,
				CacheWrite5mPerMTok: e.CacheWrite5m,
				CacheWrite1hPerMTok: e.CacheWrite1h,
				CacheReadPerMTok:    e.CacheRead,
				OutputPerMTok:       e.Output,
			}
		}
		t.models[providerName] = converted
	}
	return t, nil
}
