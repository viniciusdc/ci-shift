// Package circleci converts CircleCI 2.1 configurations to and from the IR.
package circleci

import (
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// Provider is the CircleCI provider.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "circleci" }

// Title implements ir.Provider.
func (Provider) Title() string { return "CircleCI" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return ".circleci/config.yml" }

// Detect implements ir.Provider.
func (Provider) Detect(path string, data []byte) int {
	if strings.Contains(filepath.ToSlash(path), ".circleci/") {
		return 100
	}
	root, err := yamlx.Load(data)
	if err != nil || !yamlx.IsMap(root) {
		return 0
	}
	// Keys that belong to the other providers' formats.
	for _, k := range []string{"on", "pipelines", "trigger", "pr", "pool", "stages", "image", "services"} {
		if yamlx.Has(root, k) {
			return 0
		}
	}
	jobs := yamlx.Get(root, "jobs")
	workflows := yamlx.Get(root, "workflows")
	if !yamlx.IsMap(jobs) && !yamlx.IsMap(workflows) {
		return 0
	}
	score := 0
	if yamlx.Has(root, "version") {
		score += 20
	}
	for _, pair := range yamlx.Pairs(jobs) {
		if yamlx.Has(pair.Value, "runs-on") {
			return 0
		}
		if hasAny(pair.Value, "docker", "machine", "macos", "executor") {
			score += 40
			break
		}
		if yamlx.Has(pair.Value, "steps") {
			score += 20
			break
		}
	}
	if yamlx.IsMap(workflows) {
		score += 30
	}
	if hasAny(root, "orbs", "executors", "commands") {
		score += 10
	}
	return min(score, 90)
}

func hasAny(n *yaml.Node, keys ...string) bool {
	for _, k := range keys {
		if yamlx.Has(n, k) {
			return true
		}
	}
	return false
}

var _ ir.Provider = Provider{}

// shellVars are CircleCI's built-in environment variables. CIRCLE_BRANCH
// is listed for branch first so parsing prefers the more precise name.
var shellVars = ir.NativeVars{
	{Canon: ir.VarSHA, Native: "CIRCLE_SHA1"},
	{Canon: ir.VarBranch, Native: "CIRCLE_BRANCH"},
	{Canon: ir.VarRefName, Native: "CIRCLE_BRANCH"},
	{Canon: ir.VarTag, Native: "CIRCLE_TAG"},
	{Canon: ir.VarRunID, Native: "CIRCLE_WORKFLOW_ID"},
	{Canon: ir.VarRunNumber, Native: "CIRCLE_BUILD_NUM"},
	{Canon: ir.VarWorkspace, Native: "CIRCLE_WORKING_DIRECTORY"},
	{Canon: ir.VarJob, Native: "CIRCLE_JOB"},
	{Canon: ir.VarActor, Native: "CIRCLE_USERNAME"},
	{Canon: ir.VarPRNumber, Native: "CIRCLE_PR_NUMBER"},
}

// builtinPrefixes are reserved variable prefixes on CircleCI.
// Other providers' reserved prefixes are included: their parsers leave
// untranslatable built-ins in scripts, and those are not secrets either.
var builtinPrefixes = []string{"CIRCLE", "CIRCLECI", "GITHUB_", "RUNNER_", "CI_", "GITLAB_", "BITBUCKET_"}

// Composite native expressions for canonical values that have no single
// CircleCI variable.
const (
	nativeRepository = "${CIRCLE_PROJECT_USERNAME}/${CIRCLE_PROJECT_REPONAME}"
	nativeRefName    = "${CIRCLE_BRANCH:-${CIRCLE_TAG}}"
)

var repoRe = regexp.MustCompile(`\$(?:\{CIRCLE_PROJECT_USERNAME\}|CIRCLE_PROJECT_USERNAME\b)/\$(?:\{CIRCLE_PROJECT_REPONAME\}|CIRCLE_PROJECT_REPONAME\b)`)

// fromCC converts CircleCI text into canonical form. CircleCI runs every
// command in a shell and never interpolates other fields, so only the
// well-known variables are rewritten.
func fromCC(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	s = strings.ReplaceAll(s, nativeRefName, ir.Ref(ir.RefCI, ir.VarRefName))
	s = repoRe.ReplaceAllString(s, ir.Ref(ir.RefCI, ir.VarRepository))
	return shellVars.FromShell(s)
}
