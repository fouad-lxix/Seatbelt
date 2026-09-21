// Command validate-prices checks a Seatbelt price file against the same
// rules a running gateway applies before adopting one: canary prices,
// sanity checks, schema version.
//
// Usage:
//
//	go run ./cmd/validate-prices pricing/v1/prices.json
package main

import (
	"fmt"
	"os"

	"github.com/fouad-lxix/seatbelt/internal/pricing"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: validate-prices <path-to-prices.json>")
		os.Exit(2)
	}

	path := os.Args[1]
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", path, err)
		os.Exit(1)
	}

	if err := pricing.ValidateFile(raw); err != nil {
		fmt.Fprintf(os.Stderr, "%s failed validation: %v\n", path, err)
		os.Exit(1)
	}

	fmt.Printf("%s is valid\n", path)
}
