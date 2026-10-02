package azure

import (
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/ir"
)

func TestWithErrexit(t *testing.T) {
	cases := []struct {
		name, script string
		pipefail     bool
		want         string
	}{
		{"multi-line", "make\nmake test", false, "set -e\nmake\nmake test"},
		{"multi-line bash", "make\nmake test", true, "set -eo pipefail\nmake\nmake test"},
		{"one-liner", "make test", false, "make test"},
		{"one-liner bash", "make test", true, "make test"},
		{"one line, two commands", "make; make test", false, "set -e\nmake; make test"},
		{"one-line pipe without pipefail", "make | tee log", false, "make | tee log"},
		{"one-line pipe with pipefail", "make | tee log", true, "set -eo pipefail\nmake | tee log"},
		{"comments do not count", "# build\nmake test\n", false, "# build\nmake test\n"},
		{"empty", "", false, ""},
		{"already set -e", "set -e\nmake\nmake test", false, "set -e\nmake\nmake test"},
		{"already set -eo pipefail", "set -eo pipefail\nmake\nmake test", true, "set -eo pipefail\nmake\nmake test"},
		{"already set -euxo pipefail", "set -euxo pipefail\nmake\nmake test", true, "set -euxo pipefail\nmake\nmake test"},
		{"already set -o errexit", "set -o errexit\nmake\nmake test", false, "set -o errexit\nmake\nmake test"},
		{"set after a comment", "#!/bin/bash\n# build\nset -e\nmake\nmake test", false, "#!/bin/bash\n# build\nset -e\nmake\nmake test"},
		{"set -e but pipefail wanted", "set -e\nmake\nmake test", true, "set -o pipefail\nset -e\nmake\nmake test"},
		{"set -o pipefail but no errexit", "set -o pipefail\nmake\nmake test", true, "set -e\nset -o pipefail\nmake\nmake test"},
		{"set +e is not errexit", "set +e\nmake\nmake test", false, "set -e\nset +e\nmake\nmake test"},
		{"set -x is not errexit", "set -x\nmake\nmake test", false, "set -e\nset -x\nmake\nmake test"},
		{"set later in the script", "make\nset -e\nmake test", false, "set -e\nmake\nset -e\nmake test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withErrexit(c.script, c.pipefail); got != c.want {
				t.Errorf("withErrexit(%q, %v) = %q, want %q", c.script, c.pipefail, got, c.want)
			}
		})
	}
}

// TestEmitErrexit checks which emitted script steps get the set line.
func TestEmitErrexit(t *testing.T) {
	run := func(shell, script string) *ir.Step {
		return &ir.Step{Kind: ir.StepRun, Shell: shell, Run: script}
	}
	job := func(id string, os string, steps ...*ir.Step) *ir.Job {
		return &ir.Job{ID: id, Runner: ir.Runner{OS: os}, Steps: steps}
	}
	p := &ir.Pipeline{Jobs: []*ir.Job{
		job("linux", "",
			run("", "make\nmake test"),
			run("sh", "make\nmake test"),
			run("bash", "make\nmake test"),
			run("", "set -e\nmake lint\nmake vet"),
			run("", "make one"),
			run("pwsh", "Write-Host a\nWrite-Host b"),
			run("python", "print(1)\nprint(2)"),
		),
		job("windows", ir.OSWindows,
			run("", "echo a\necho b"),
			run("bash", "echo c\necho d"),
		),
	}}
	var d ir.Diagnostics
	out, err := Provider{}.Emit(p, &d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"- script: |-\n          set -e\n          make\n          make test\n",
		"- bash: |-\n          set -eo pipefail\n          make\n          make test\n",
		"- script: |-\n          set -e\n          make lint\n",
		"- script: make one\n",
		"- pwsh: |-\n          Write-Host a\n",
		// Azure has no python step: bash runs it, as the warning says.
		"- script: |-\n          set -e\n          print(1)\n",
		"- script: |-\n          echo a\n",
		"- bash: |-\n          set -eo pipefail\n          echo c\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if n := strings.Count(s, "set -e\n"); n != 4 {
		t.Errorf("want 4 set -e lines (default, sh, already set, python), got %d:\n%s", n, s)
	}
	assertFixedPoint(t, out)
}

// TestParseErrexitNote checks the note on Azure scripts that relied on
// running without errexit.
func TestParseErrexitNote(t *testing.T) {
	const note = "without errexit"
	cases := []struct {
		name, src string
		want      bool
	}{
		{"multi-line script", "steps:\n  - script: |\n      make\n      make test\n", true},
		{"multi-line bash", "steps:\n  - bash: |\n      make\n      make test\n", true},
		{"one-liner", "steps:\n  - script: make test\n", false},
		{"already set -e", "steps:\n  - script: |\n      set -e\n      make\n      make test\n", false},
		{"pwsh", "steps:\n  - pwsh: |\n      Write-Host a\n      Write-Host b\n", false},
		{"windows cmd", "pool:\n  vmImage: windows-latest\nsteps:\n  - script: |\n      echo a\n      echo b\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d ir.Diagnostics
			if _, err := (Provider{}).Parse([]byte(c.src), &d); err != nil {
				t.Fatal(err)
			}
			if got := hasDiag(&d, note); got != c.want {
				t.Errorf("note present = %v, want %v: %v", got, c.want, d.List())
			}
		})
	}
}
