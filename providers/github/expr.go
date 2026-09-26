package github

import (
	"regexp"
	"strings"

	"github.com/viniciusdc/cistela/ir"
)

// shellVars are the default environment variables GitHub exports to steps.
var shellVars = ir.NativeVars{
	{Canon: ir.VarSHA, Native: "GITHUB_SHA"},
	{Canon: ir.VarRefName, Native: "GITHUB_REF_NAME"},
	{Canon: ir.VarRepository, Native: "GITHUB_REPOSITORY"},
	{Canon: ir.VarRunID, Native: "GITHUB_RUN_ID"},
	{Canon: ir.VarRunNumber, Native: "GITHUB_RUN_NUMBER"},
	{Canon: ir.VarWorkspace, Native: "GITHUB_WORKSPACE"},
	{Canon: ir.VarJob, Native: "GITHUB_JOB"},
	{Canon: ir.VarActor, Native: "GITHUB_ACTOR"},
	{Canon: ir.VarEvent, Native: "GITHUB_EVENT_NAME"},
	{Canon: ir.VarServerURL, Native: "GITHUB_SERVER_URL"},
}

// contextVars maps canonical names to GitHub expression contexts.
var contextVars = map[string]string{
	ir.VarSHA:        "github.sha",
	ir.VarRefName:    "github.ref_name",
	ir.VarBranch:     "github.head_ref || github.ref_name",
	ir.VarTag:        "github.ref_name",
	ir.VarRepository: "github.repository",
	ir.VarRunID:      "github.run_id",
	ir.VarRunNumber:  "github.run_number",
	ir.VarWorkspace:  "github.workspace",
	ir.VarJob:        "github.job",
	ir.VarActor:      "github.actor",
	ir.VarPRNumber:   "github.event.pull_request.number",
	ir.VarEvent:      "github.event_name",
	ir.VarServerURL:  "github.server_url",
	ir.VarDefaultBr:  "github.event.repository.default_branch",
}

// contextToCanon is the reverse lookup used when parsing.
var contextToCanon = map[string]string{
	"github.sha":                             ir.VarSHA,
	"github.ref_name":                        ir.VarRefName,
	"github.head_ref":                        ir.VarBranch,
	"github.repository":                      ir.VarRepository,
	"github.run_id":                          ir.VarRunID,
	"github.run_number":                      ir.VarRunNumber,
	"github.workspace":                       ir.VarWorkspace,
	"github.job":                             ir.VarJob,
	"github.actor":                           ir.VarActor,
	"github.triggering_actor":                ir.VarActor,
	"github.event.pull_request.number":       ir.VarPRNumber,
	"github.event.number":                    ir.VarPRNumber,
	"github.event_name":                      ir.VarEvent,
	"github.server_url":                      ir.VarServerURL,
	"github.event.repository.default_branch": ir.VarDefaultBr,
}

// branchIdiom recognizes the shell spelling of "branch name, also for
// pull requests" that toGH emits for ${{ ci.branch }}.
var branchIdiom = strings.NewReplacer(
	"${GITHUB_HEAD_REF:-${GITHUB_REF_NAME}}", ir.Ref(ir.RefCI, ir.VarBranch),
	"${GITHUB_HEAD_REF:-$GITHUB_REF_NAME}", ir.Ref(ir.RefCI, ir.VarBranch),
)

var ghExprRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)
var identRe = regexp.MustCompile(`^(secrets|env|matrix|vars)\.([A-Za-z_][A-Za-z0-9_\-]*)$`)

// fromGH converts GitHub expressions and default env vars in s into
// canonical references. Expressions that have no canonical form are kept.
func fromGH(s string) string {
	if s == "" {
		return s
	}
	s = ghExprRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(ghExprRe.FindStringSubmatch(m)[1])
		if c, ok := contextToCanon[inner]; ok {
			return ir.Ref(ir.RefCI, c)
		}
		if sub := identRe.FindStringSubmatch(inner); sub != nil {
			switch sub[1] {
			case "secrets":
				return ir.Ref(ir.RefSecret, sub[2])
			case "env", "vars":
				return ir.Ref(ir.RefEnv, sub[2])
			case "matrix":
				return ir.Ref(ir.RefMatrix, sub[2])
			}
		}
		return m
	})
	s = branchIdiom.Replace(s)
	return shellVars.FromShell(s)
}

// toGH converts canonical references into GitHub syntax. In scripts,
// env and secret refs become shell variables (secrets are then mapped
// through the step's env by the caller); elsewhere they become expressions.
func toGH(s string, script bool) string {
	return ir.RewriteRefs(s, func(kind, name string) string {
		switch kind {
		case ir.RefCI:
			if script {
				switch name {
				case ir.VarBranch:
					return "${GITHUB_HEAD_REF:-${GITHUB_REF_NAME}}"
				case ir.VarTag:
					return "${GITHUB_REF_NAME}"
				}
				if v, ok := shellVars.Lookup(name); ok {
					return "${" + v + "}"
				}
			}
			if ctx, ok := contextVars[name]; ok {
				return "${{ " + ctx + " }}"
			}
		case ir.RefSecret:
			if script {
				return "${" + name + "}"
			}
			return "${{ secrets." + name + " }}"
		case ir.RefEnv:
			if script {
				return "${" + name + "}"
			}
			return "${{ env." + name + " }}"
		case ir.RefMatrix:
			return "${{ matrix." + name + " }}"
		}
		return ir.Ref(kind, name)
	})
}
