package azure

import (
	"strings"

	"github.com/viniciusdc/ci-shift/ir"
)

// withErrexit makes a script stop at its first failing command, as an IR run
// step does on every other provider. Azure runs script and bash steps as
// `bash --noprofile --norc <file>` without errexit, so the emitter prepends
// "set -e" to script steps and "set -eo pipefail" to bash steps (which come
// from GitHub's `shell: bash`). Scripts that already start with such a set
// line, and single one-line commands, are left as they are; PowerShell and
// cmd scripts are not touched.
func withErrexit(script string, pipefail bool) string {
	if line := errexitLine(script, pipefail); line != "" {
		return line + "\n" + script
	}
	return script
}

// errexitLine returns the set line script needs to stop at its first
// failing command (and, with pipefail, at a failing pipeline stage), or ""
// when it needs none. A script whose first command sets only some of the
// options gets a set line for the rest.
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
// which errexit (and, without "|", pipefail) changes nothing. A ";" (or a
// "|" when pipefail is wanted) counts as a second command even when quoted,
// which errs on the side of adding the set line.
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

// noteErrexit adds an info note when a multi-command Azure script will stop
// at its first failing command once converted. Windows script steps
// (cmd.exe) are skipped.
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
