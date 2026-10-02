// Package conformance holds the pipeline that the live end-to-end tests run
// on every provider, and converts it for them.
//
// ci.yml is a GitHub Actions workflow whose jobs each exercise one
// translated feature and fail when it was mistranslated. It must convert to
// every target without warnings and pass each target's schema; the tests in
// this package check that on every `go test`, without provider accounts.
package conformance

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/viniciusdc/ci-shift/convert"
	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/schema"
)

// Source is the conformance pipeline, a GitHub Actions workflow.
//
//go:embed ci.yml
var Source []byte

// SourcePath is where Source lives in this repository. The pushed test
// branches carry a copy at the same path, which the checkout job looks for.
const SourcePath = "e2e/conformance/ci.yml"

// Targets are the providers the pipeline is converted to.
var Targets = []string{"gitlab", "circleci", "azure", "bitbucket"}

// Job is a job of the conformance pipeline and what its outcome must be.
type Job struct {
	// ID is the job's key in ci.yml. Providers rename jobs (Azure uses
	// underscores, matrix legs get suffixes), so results are matched on
	// the normalized form; see Match.
	ID string
	// Legs is the number of matrix legs the job fans out into, 0 for none.
	Legs int
	// Skipped means the job must not run on the test branch: it must be
	// reported as skipped or not appear at all.
	Skipped bool
	// Checks is a short description of what the job verifies.
	Checks string
}

// Jobs lists every job of ci.yml with its expected outcome.
var Jobs = []Job{
	{ID: "checkout-repo", Checks: "the repository is checked out (a known file is present)"},
	{ID: "artifact-produce", Checks: "a job publishes an artifact"},
	{ID: "artifact-consume", Checks: "a dependent job runs after it and reads the artifact's content"},
	{ID: "matrix-legs", Legs: 2, Checks: "the matrix fans out into one leg per value, each seeing its own value"},
	{ID: "env-vars", Checks: "scripts stop at the first failing command; pipeline, job and step variables have their values; the E2E_SECRET secret is set"},
	{ID: "postgres-service", Checks: "a postgres:16 service container accepts TCP connections on 5432"},
	{ID: "cache-roundtrip", Checks: "the cache restore/save steps run"},
	{ID: "on-test-branch", Checks: "a job conditioned on the e2e/ branch prefix runs"},
	{ID: "never-runs", Skipped: true, Checks: "a job conditioned on another branch is skipped (it exits 1 if it runs)"},
}

// Normalize lowercases name and drops everything but letters and digits,
// so "matrix-legs", "matrix_legs" and "Matrix Legs" compare equal.
func Normalize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Match reports whether a job name reported by a provider belongs to j:
// its normalized form equals the normalized ID or starts with it (matrix
// legs such as "matrix-legs (red)", "matrix-legs: [red]", "matrix_legs red").
// No job ID is a prefix of another, so the match is unambiguous.
func (j Job) Match(name string) bool {
	return strings.HasPrefix(Normalize(name), Normalize(j.ID))
}

// Converted is the pipeline rendered for one provider.
type Converted struct {
	Provider string
	// Path is where the provider expects the file (e.g. ".gitlab-ci.yml").
	Path   string
	Output []byte
	// Notes are the informational diagnostics of the conversion.
	Notes []ir.Diagnostic
}

// Convert renders the pipeline for target. It fails when the conversion
// produces any warning (as `cishift --strict` does) or when the output does
// not pass the target's embedded schema.
func Convert(target string) (*Converted, error) {
	from, err := convert.Get("github")
	if err != nil {
		return nil, err
	}
	to, err := convert.Get(target)
	if err != nil {
		return nil, err
	}
	res, err := convert.Convert(Source, from, to, convert.Options{Header: true, SourceName: SourcePath})
	if err != nil {
		return nil, fmt.Errorf("convert to %s: %w", target, err)
	}
	var warnings []string
	var notes []ir.Diagnostic
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityWarning {
			warnings = append(warnings, d.String())
		} else {
			notes = append(notes, d)
		}
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("convert to %s: %d warning(s):\n  %s", target, len(warnings), strings.Join(warnings, "\n  "))
	}
	issues, err := schema.Validate(to.Name(), res.Output, schema.Options{Embedded: true})
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", target, err)
	}
	if len(issues) > 0 {
		var msgs []string
		for _, i := range issues {
			msgs = append(msgs, i.String())
		}
		return nil, fmt.Errorf("%s schema: %d issue(s):\n  %s", target, len(issues), strings.Join(msgs, "\n  "))
	}
	return &Converted{Provider: to.Name(), Path: to.DefaultPath(), Output: res.Output, Notes: notes}, nil
}
