// Package github converts GitHub Actions workflows to and from the IR.
package github

import (
	"path/filepath"
	"strings"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// Provider is the GitHub Actions provider.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "github" }

// Title implements ir.Provider.
func (Provider) Title() string { return "GitHub Actions" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return ".github/workflows/ci.yml" }

// Detect implements ir.Provider.
func (Provider) Detect(path string, data []byte) int {
	if strings.Contains(filepath.ToSlash(path), ".github/workflows/") {
		return 100
	}
	root, err := yamlx.Load(data)
	if err != nil || !yamlx.IsMap(root) {
		return 0
	}
	score := 0
	if yamlx.Has(root, "on") || yamlx.Has(root, "true") {
		score += 30
	}
	for _, j := range yamlx.Pairs(yamlx.Get(root, "jobs")) {
		if yamlx.Has(j.Value, "runs-on") {
			score += 50
			break
		}
	}
	return score
}

var _ ir.Provider = Provider{}
