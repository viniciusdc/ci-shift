// Package bitbucket converts Bitbucket Pipelines configurations to and from
// the IR.
//
// Bitbucket has no job graph: a pipeline is a sequence of steps and
// parallel groups, and a file holds several pipelines selected by branch,
// tag, pull request or manual run. The parser merges every pipeline into
// one IR pipeline (jobs of each section get a condition); the emitter
// splits the IR back into sections and serializes its dependency levels.
package bitbucket

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

// Provider is the Bitbucket Pipelines provider.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "bitbucket" }

// Title implements ir.Provider.
func (Provider) Title() string { return "Bitbucket Pipelines" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return "bitbucket-pipelines.yml" }

// Detect implements ir.Provider.
func (Provider) Detect(path string, data []byte) int {
	base := strings.ToLower(filepath.Base(path))
	if base == "bitbucket-pipelines.yml" || base == "bitbucket-pipelines.yaml" {
		return 100
	}
	root, err := yamlx.Load(data)
	if err != nil || !yamlx.IsMap(root) {
		return 0
	}
	if yamlx.IsMap(yamlx.Get(root, "pipelines")) {
		return 80
	}
	return 0
}

var _ ir.Provider = Provider{}

// shellVars are Bitbucket's default variables. The branch entry comes
// before ref_name so that parsing maps BITBUCKET_BRANCH to ci.branch.
var shellVars = ir.NativeVars{
	{Canon: ir.VarSHA, Native: "BITBUCKET_COMMIT"},
	{Canon: ir.VarBranch, Native: "BITBUCKET_BRANCH"},
	{Canon: ir.VarRefName, Native: "BITBUCKET_BRANCH"},
	{Canon: ir.VarTag, Native: "BITBUCKET_TAG"},
	{Canon: ir.VarRepository, Native: "BITBUCKET_REPO_FULL_NAME"},
	{Canon: ir.VarRunID, Native: "BITBUCKET_PIPELINE_UUID"},
	{Canon: ir.VarRunNumber, Native: "BITBUCKET_BUILD_NUMBER"},
	{Canon: ir.VarWorkspace, Native: "BITBUCKET_CLONE_DIR"},
	{Canon: ir.VarJob, Native: "BITBUCKET_STEP_UUID"},
	{Canon: ir.VarActor, Native: "BITBUCKET_STEP_TRIGGERER_UUID"},
	{Canon: ir.VarPRNumber, Native: "BITBUCKET_PR_ID"},
}

// builtinPrefixes are reserved variable prefixes on Bitbucket runners.
var builtinPrefixes = []string{"BITBUCKET_", "CI"}

// predefinedCaches are the caches Bitbucket provides without a definition.
// The docker cache (image layers) has no path and no portable equivalent.
var predefinedCaches = map[string]string{
	"composer":   "~/.composer/cache",
	"dotnetcore": "~/.nuget/packages",
	"gradle":     "~/.gradle/caches",
	"ivy2":       "~/.ivy2/cache",
	"maven":      "~/.m2/repository",
	"node":       "node_modules",
	"pip":        "~/.cache/pip",
	"sbt":        "~/.sbt",
}

// predefinedOrder lists predefined cache names deterministically.
var predefinedOrder = []string{"composer", "dotnetcore", "gradle", "ivy2", "maven", "node", "pip", "sbt"}

// fromBB converts Bitbucket text to canonical form. Outside scripts, plain
// variable references become ${{ env.NAME }} so emitters can translate them.
func fromBB(s string, script bool) string {
	s = shellVars.FromShell(s)
	if script {
		return s
	}
	return ir.ReplaceShellVars(s, func(name string) (string, bool) {
		return ir.Ref(ir.RefEnv, name), true
	})
}

// toBB renders canonical references as Bitbucket variables. CI values
// Bitbucket does not provide are reported and replaced.
func toBB(s string, d *ir.Diagnostics, path string) string {
	return ir.RewriteRefs(s, func(kind, name string) string {
		if kind != ir.RefCI {
			return "${" + name + "}"
		}
		switch name {
		case ir.VarJob:
			d.Infof(path, "%s was approximated with BITBUCKET_STEP_UUID", ir.Ref(kind, name))
		case ir.VarActor:
			d.Infof(path, "%s was approximated with BITBUCKET_STEP_TRIGGERER_UUID (a user UUID, not a login)", ir.Ref(kind, name))
		case ir.VarRefName:
			d.Infof(path, "%s was mapped to BITBUCKET_BRANCH, which is empty in tag builds (use BITBUCKET_TAG there)", ir.Ref(kind, name))
		case ir.VarServerURL:
			return "https://bitbucket.org"
		}
		if v, ok := shellVars.Lookup(name); ok {
			return "${" + v + "}"
		}
		d.Warnf(path, "%s has no Bitbucket equivalent and was replaced with an empty string", ir.Ref(kind, name))
		return ""
	})
}

// serviceAlias derives a service name from its image.
func serviceAlias(image string) string {
	name, _, _ := strings.Cut(image, ":")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return ir.Slug(name)
}

// expandBraces expands Bitbucket glob alternatives: "{main,dev}" becomes
// ["main", "dev"] and "release/{a,b}-*" becomes ["release/a-*", "release/b-*"].
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	end := strings.IndexByte(pattern[open:], '}')
	if end < 0 {
		return []string{pattern}
	}
	end += open
	var out []string
	for _, alt := range strings.Split(pattern[open+1:end], ",") {
		out = append(out, expandBraces(pattern[:open]+alt+pattern[end+1:])...)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(list []string, items ...string) []string {
	for _, s := range items {
		if !contains(list, s) {
			list = append(list, s)
		}
	}
	return list
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}

func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, "$`") {
		return `"` + strings.NewReplacer(`"`, `\"`).Replace(s) + `"`
	}
	if strings.ContainsAny(s, " \t'\"\\;&|<>(){}*?!#~") {
		return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
	}
	return s
}

var compoundRe = regexp.MustCompile(`(^|[\s;(])(if|then|else|elif|fi|for|while|until|do|done|case|esac|function)(\s|;|$)|[{}]\s*$|<<|\\$`)

// splitCommands splits a script into one command per line when every line
// is a self-contained command (no compound statements, heredocs, line
// continuations or unbalanced quotes). It matches GitLab's splitting so
// both providers render scripts the same way.
func splitCommands(script string) ([]string, bool) {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if line != trimmed && !strings.HasPrefix(line, trimmed) {
			// Indented lines belong to a surrounding construct.
			return nil, false
		}
		if compoundRe.MatchString(trimmed) || strings.Count(trimmed, "'")%2 != 0 || strings.Count(trimmed, `"`)%2 != 0 {
			return nil, false
		}
		out = append(out, trimmed)
	}
	return out, len(out) > 0
}
