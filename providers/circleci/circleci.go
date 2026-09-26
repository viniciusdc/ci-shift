// Package circleci converts circleci pipelines (work in progress).
package circleci

import (
	"errors"

	"github.com/viniciusdc/cistela/ir"
)

// Provider is a placeholder until the implementation lands.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "circleci" }

// Title implements ir.Provider.
func (Provider) Title() string { return "circleci" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return "" }

// Detect implements ir.Provider.
func (Provider) Detect(string, []byte) int { return 0 }

// Parse implements ir.Provider.
func (Provider) Parse([]byte, *ir.Diagnostics) (*ir.Pipeline, error) {
	return nil, errors.New("circleci: not implemented yet")
}

// Emit implements ir.Provider.
func (Provider) Emit(*ir.Pipeline, *ir.Diagnostics) ([]byte, error) {
	return nil, errors.New("circleci: not implemented yet")
}
