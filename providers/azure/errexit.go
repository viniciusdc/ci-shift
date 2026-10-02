package azure

import (
	"strings"

	"github.com/viniciusdc/ci-shift/ir"
)

// Azure Pipelines runs script and bash steps as
// `bash --noprofile --norc <file>`, without errexit, so a failing command
// in the middle of a script is ignored and only the last command's exit
// status decides the step. GitHub Actions (bash -e, or bash -eo pipefail
// for `shell: bash`), GitLab, CircleCI (bash -eo pipefail) and Bitbucket
// all stop at the first failing command, and that is what an IR run step
// means. The emitter therefore prepends the options a script Azure runs
// with bash lacks:
//
//   - "set -e" for a script step. Every source runs its default shell
//     with errexit, but not all with pipefail (GitHub's default is
//     bash -e), so errexit is the common ground;
//   - "set -eo pipefail" for a bash step, which the IR's bash shell comes
//     from: GitHub's `shell: bash` (bash -eo pipefail) or Azure's own bash
//     steps.
//
// A shell Azure has no step for (python, ...) also lands in a script
// step, as the emitter warns, so bash runs it and it is treated the same.
// PowerShell steps stop on errors through errorActionPreference, which
// Azure sets to stop by default, and cmd scripts (a script step on
// Windows) report only their last exit code on GitHub too; both are left
// alone.
//
// The rule is:
//
//   - a script whose first command already is a set enabling the options
//     is left as it is, and one enabling only some of them gets a set
//     line for the rest;
//   - a single command on a single line is left as it is: its exit status
//     is the step's either way, so the set line would only add noise. A
//     line holding ";" (or "|" when pipefail is wanted) counts as several
//     commands, even when the character is quoted, which errs on the side
//     of adding the line;
//   - every other script gets the set line as its first line.

// errexitLine returns the set line script needs to stop at its first
// failing command (and, with pipefail, at a failing pipeline stage), or ""
// when it needs none.
func errexitLine(script string, pipefail bool) string {
	if singleCommand(script, pipefail) {
		return ""
	}
	hasE, hasPipefail := leadingSetOptions(script)
	switch {
	case !hasE && pipefail && !hasPipefail:
		return "set -eo pipefail"
	case !hasE:
		return "set -e"
	case pipefail && !hasPipefail:
		return "set -o pipefail"
	}
	return ""
}

// withErrexit returns script with the set line it needs prepended.
func withErrexit(script string, pipefail bool) string {
	if line := errexitLine(script, pipefail); line != "" {
		return line + "\n" + script
	}
	return script
}

// commandLines returns the lines of script that are neither blank nor
// comments.
func commandLines(script string) []string {
	var out []string
	for _, l := range strings.Split(script, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// singleCommand reports whether script is one command on one line, for
// which errexit (and, without "|", pipefail) changes nothing.
func singleCommand(script string, pipefail bool) bool {
	lines := commandLines(script)
	if len(lines) != 1 {
		return len(lines) == 0
	}
	if strings.Contains(lines[0], ";") {
		return false
	}
	return !pipefail || !strings.Contains(lines[0], "|")
}

// leadingSetOptions reports whether the first command of script is a set
// builtin enabling errexit and pipefail ("set -e", "set -eo pipefail",
// "set -euxo pipefail", "set -o errexit", ...).
func leadingSetOptions(script string) (errexit, pipefail bool) {
	lines := commandLines(script)
	if len(lines) == 0 {
		return false, false
	}
	cmd := lines[0]
	if i := strings.IndexAny(cmd, ";&|#"); i >= 0 {
		cmd = cmd[:i]
	}
	f := strings.Fields(cmd)
	if len(f) == 0 || f[0] != "set" {
		return false, false
	}
	for i := 1; i < len(f); i++ {
		a := f[i]
		if len(a) < 2 || a[0] != '-' || a == "--" {
			break
		}
		flags := a[1:]
		if strings.ContainsRune(flags, 'e') {
			errexit = true
		}
		if strings.HasSuffix(flags, "o") && i+1 < len(f) {
			i++
			switch f[i] {
			case "errexit":
				errexit = true
			case "pipefail":
				pipefail = true
			}
		}
	}
	return errexit, pipefail
}

// noteErrexit tells, for an Azure source, that a script which relied on
// Azure running it without errexit now stops at its first failing
// command. On Windows a plain script step runs cmd.exe, which other
// targets do not change.
func (ps *parser) noteErrexit(j *ir.Job, st *ir.Step, path string) {
	if st.Kind != ir.StepRun || !posixScript(j, st) {
		return
	}
	if line := errexitLine(st.Run, st.Shell == "bash"); line != "" && line != "set -o pipefail" {
		ps.d.Infof(path, "Azure Pipelines runs this script without errexit; converted, it stops at its first failing command")
	}
}

// posixScript reports whether Azure runs the parsed script step with bash:
// a bash step anywhere, or a script step outside Windows.
func posixScript(j *ir.Job, st *ir.Step) bool {
	return st.Shell == "bash" || (st.Shell == "" && j.Runner.OS != ir.OSWindows)
}
