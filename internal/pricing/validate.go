package pricing

import (
	"fmt"
	"math"

	"github.com/fouad-lxix/seatbelt/internal/provider"
)

// canaries are models whose rates were checked by hand against the
// providers' own pricing pages. A fetched table must agree with every one
// of them exactly, or it is rejected. Keep this list small and verified.
var canaries = map[string]map[string]provider.Price{
	"anthropic": {
		"claude-haiku-4-5": {
			InputPerMTok: 1, CacheWrite5mPerMTok: 1.25, CacheWrite1hPerMTok: 2,
			CacheReadPerMTok: 0.10, OutputPerMTok: 5,
		},
		"claude-sonnet-5": {
			InputPerMTok: 2, CacheWrite5mPerMTok: 2.50, CacheWrite1hPerMTok: 4,
			CacheReadPerMTok: 0.20, OutputPerMTok: 10,
		},
		"claude-opus-5": {
			InputPerMTok: 5, CacheWrite5mPerMTok: 6.25, CacheWrite1hPerMTok: 10,
			CacheReadPerMTok: 0.50, OutputPerMTok: 25,
		},
	},
	"openai": {
		"gpt-4o-mini": {
			InputPerMTok: 0.15, CacheReadPerMTok: 0.075, OutputPerMTok: 0.60,
		},
		"gpt-5-mini": {
			InputPerMTok: 0.25, CacheReadPerMTok: 0.025, OutputPerMTok: 2,
		},
	},
}

// maxSanePricePerMTok is an upper bound no real model approaches. A figure
// above it is a misplaced decimal point.
const maxSanePricePerMTok = 1000.0

// minModelsFraction is how much of the previous table's coverage a new one
// must retain, to catch a truncated or corrupted file.
const minModelsFraction = 0.5

// validate decides whether a table is fit to bill against. previous may be
// nil when there is nothing to compare against yet.
func validate(t *Table, previous *Table) error {
	if t.Count() == 0 {
		return fmt.Errorf("price table from %s contains no models", t.source)
	}

	if previous != nil {
		floor := int(float64(previous.Count()) * minModelsFraction)
		if t.Count() < floor {
			return fmt.Errorf(
				"price table from %s prices only %d models, the table in use prices %d, "+
					"this looks truncated rather than updated",
				t.source, t.Count(), previous.Count())
		}
	}

	for providerName, models := range t.models {
		for model, p := range models {
			if err := sane(providerName, model, p); err != nil {
				return fmt.Errorf("price table from %s: %w", t.source, err)
			}
		}
	}

	for providerName, expected := range canaries {
		for model, want := range expected {
			got, ok := t.Lookup(providerName, model)
			if !ok {
				return fmt.Errorf(
					"price table from %s does not price %s/%s, which is a known model",
					t.source, providerName, model)
			}
			if got != want {
				return fmt.Errorf(
					"price table from %s disagrees with the verified price for %s/%s "+
						"(got input $%g/output $%g, expected input $%g/output $%g), "+
						"refusing to bill against it",
					t.source, providerName, model,
					got.InputPerMTok, got.OutputPerMTok,
					want.InputPerMTok, want.OutputPerMTok)
			}
		}
	}

	return nil
}

// sane applies the rules that hold for every model at every provider, to
// catch a typo a behavioral test would not notice.
func sane(providerName, model string, p provider.Price) error {
	where := providerName + "/" + model

	switch {
	case p.InputPerMTok <= 0:
		return fmt.Errorf("%s has a non-positive input price ($%g)", where, p.InputPerMTok)
	case p.OutputPerMTok <= 0:
		return fmt.Errorf("%s has a non-positive output price ($%g)", where, p.OutputPerMTok)
	case math.IsNaN(p.InputPerMTok) || math.IsInf(p.InputPerMTok, 0):
		return fmt.Errorf("%s has a non-finite input price", where)
	case math.IsNaN(p.OutputPerMTok) || math.IsInf(p.OutputPerMTok, 0):
		return fmt.Errorf("%s has a non-finite output price", where)
	}

	if p.OutputPerMTok <= p.InputPerMTok {
		return fmt.Errorf("%s: output $%g is not above input $%g",
			where, p.OutputPerMTok, p.InputPerMTok)
	}

	if p.CacheReadPerMTok > p.InputPerMTok {
		return fmt.Errorf("%s: cache read $%g exceeds base input $%g",
			where, p.CacheReadPerMTok, p.InputPerMTok)
	}

	if p.CacheWrite5mPerMTok != 0 && p.CacheWrite5mPerMTok < p.InputPerMTok {
		return fmt.Errorf("%s: 5m cache write $%g is below base input $%g",
			where, p.CacheWrite5mPerMTok, p.InputPerMTok)
	}
	if p.CacheWrite1hPerMTok != 0 && p.CacheWrite1hPerMTok < p.CacheWrite5mPerMTok {
		return fmt.Errorf("%s: 1h cache write $%g is below the 5m write $%g",
			where, p.CacheWrite1hPerMTok, p.CacheWrite5mPerMTok)
	}

	if p.OutputPerMTok > maxSanePricePerMTok {
		return fmt.Errorf("%s: output price $%g looks like a misplaced decimal point",
			where, p.OutputPerMTok)
	}

	// Anthropic bills for cache writes, OpenAI does not. A zero rate is
	// correct for OpenAI and a silent undercount for Anthropic.
	if providerName == "anthropic" {
		if p.CacheWrite5mPerMTok == 0 || p.CacheWrite1hPerMTok == 0 {
			return fmt.Errorf(
				"%s: no cache write price (5m=$%g, 1h=$%g), Anthropic bills for cache "+
					"writes, leaving these at zero would bill cached calls as free",
				where, p.CacheWrite5mPerMTok, p.CacheWrite1hPerMTok)
		}
	}

	return nil
}
