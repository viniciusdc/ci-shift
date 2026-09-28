package conformance

import (
	"os"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/convert"
	"github.com/viniciusdc/ci-shift/ir"
)

// TestMain keeps a developer's downloaded schemas out of the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cishift-schemas")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CISHIFT_CACHE_DIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestConvertsCleanly is the offline half of the live tests: the pipeline
// converts to every target with no warnings and passes its schema, and the
// jobs the live runner expects survive the conversion.
func TestConvertsCleanly(t *testing.T) {
	for _, target := range Targets {
		t.Run(target, func(t *testing.T) {
			c, err := Convert(target)
			if err != nil {
				t.Fatal(err)
			}
			to, _ := convert.Get(target)
			var diags ir.Diagnostics
			p, err := to.Parse(c.Output, &diags)
			if err != nil {
				t.Fatalf("parse the %s output back: %v", target, err)
			}
			for _, j := range Jobs {
				found := false
				for _, pj := range p.Jobs {
					if j.Match(pj.ID) || j.Match(pj.DisplayName()) {
						found = true
					}
				}
				// Bitbucket keeps conditional jobs in per-branch sections,
				// and a skipped job may legitimately be dropped.
				if !found && !j.Skipped {
					t.Errorf("job %s is missing from the %s output", j.ID, target)
				}
			}
			if t.Failed() {
				t.Logf("output:\n%s", c.Output)
			}
		})
	}
}

// TestJobsMatchSource keeps Jobs in step with ci.yml.
func TestJobsMatchSource(t *testing.T) {
	gh, _ := convert.Get("github")
	p, err := gh.Parse(Source, &ir.Diagnostics{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != len(Jobs) {
		t.Errorf("ci.yml has %d jobs, Jobs lists %d", len(p.Jobs), len(Jobs))
	}
	for _, j := range Jobs {
		src := p.FindJob(j.ID)
		if src == nil {
			t.Errorf("Jobs lists %s, which is not in ci.yml", j.ID)
			continue
		}
		if legs := len(src.Matrix.Combos()); legs != j.Legs {
			t.Errorf("%s: ci.yml has %d matrix legs, Jobs says %d", j.ID, legs, j.Legs)
		}
		if j.Skipped && src.If.IsZero() {
			t.Errorf("%s must be skipped but has no condition in ci.yml", j.ID)
		}
	}
	// Match relies on no normalized ID being a prefix of another.
	for _, a := range Jobs {
		for _, b := range Jobs {
			if a.ID != b.ID && strings.HasPrefix(Normalize(a.ID), Normalize(b.ID)) {
				t.Errorf("job %s starts with job %s; results would be ambiguous", a.ID, b.ID)
			}
		}
	}
}

func TestMatch(t *testing.T) {
	j := Job{ID: "matrix-legs"}
	for _, name := range []string{"matrix-legs", "matrix_legs", "matrix-legs (red)", "matrix-legs: [blue]", "matrix_legs red", "Matrix Legs"} {
		if !j.Match(name) {
			t.Errorf("%q should match %s", name, j.ID)
		}
	}
	for _, name := range []string{"matrix", "legs", "env-vars"} {
		if j.Match(name) {
			t.Errorf("%q should not match %s", name, j.ID)
		}
	}
}
