// Package gitlab converts GitLab CI/CD configurations to and from the IR.
package gitlab

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

// Provider is the GitLab CI/CD provider.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "gitlab" }

// Title implements ir.Provider.
func (Provider) Title() string { return "GitLab CI/CD" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return ".gitlab-ci.yml" }

// Detect implements ir.Provider.
func (Provider) Detect(path string, data []byte) int {
	base := strings.ToLower(filepath.Base(path))
	if base == ".gitlab-ci.yml" || base == ".gitlab-ci.yaml" || strings.HasSuffix(base, ".gitlab-ci.yml") {
		return 100
	}
	root, err := yamlx.Load(data)
	if err != nil || !yamlx.IsMap(root) {
		return 0
	}
	score := 0
	if yamlx.Has(root, "stages") {
		score += 40
	}
	for _, pair := range yamlx.Pairs(root) {
		if yamlx.Has(pair.Value, "script") {
			score += 40
			break
		}
	}
	if yamlx.Has(root, "jobs") || yamlx.Has(root, "pipelines") {
		score -= 30
	}
	return max(score, 0)
}

var _ ir.Provider = Provider{}

// shellVars are GitLab's predefined variables.
var shellVars = ir.NativeVars{
	{Canon: ir.VarSHA, Native: "CI_COMMIT_SHA"},
	{Canon: ir.VarRefName, Native: "CI_COMMIT_REF_NAME"},
	{Canon: ir.VarBranch, Native: "CI_COMMIT_BRANCH"},
	{Canon: ir.VarTag, Native: "CI_COMMIT_TAG"},
	{Canon: ir.VarRepository, Native: "CI_PROJECT_PATH"},
	{Canon: ir.VarRunID, Native: "CI_PIPELINE_ID"},
	{Canon: ir.VarRunNumber, Native: "CI_PIPELINE_IID"},
	{Canon: ir.VarWorkspace, Native: "CI_PROJECT_DIR"},
	{Canon: ir.VarJob, Native: "CI_JOB_NAME"},
	{Canon: ir.VarActor, Native: "GITLAB_USER_LOGIN"},
	{Canon: ir.VarPRNumber, Native: "CI_MERGE_REQUEST_IID"},
	{Canon: ir.VarEvent, Native: "CI_PIPELINE_SOURCE"},
	{Canon: ir.VarServerURL, Native: "CI_SERVER_URL"},
	{Canon: ir.VarDefaultBr, Native: "CI_DEFAULT_BRANCH"},
}

// builtinPrefixes are reserved variable prefixes on GitLab runners.
var builtinPrefixes = []string{"CI_", "GITLAB_", "FF_", "DOCKER_", "KUBERNETES_", "RUNNER_", "GIT_", "ARTIFACT_", "CACHE_", "TRANSFER_", "SECURE_", "SAST_", "DS_"}

// fromGL converts GitLab text to canonical form. Outside scripts, plain
// variable references become ${{ env.NAME }} so emitters can translate them.
func fromGL(s string, script bool) string {
	s = shellVars.FromShell(s)
	if script {
		return s
	}
	return ir.ReplaceShellVars(s, func(name string) (string, bool) {
		return ir.Ref(ir.RefEnv, name), true
	})
}

// toGL renders canonical references as GitLab variables.
func toGL(s string) string {
	return ir.RewriteRefs(s, func(kind, name string) string {
		if kind == ir.RefCI {
			if v, ok := shellVars.Lookup(name); ok {
				return "${" + v + "}"
			}
		}
		return "${" + name + "}"
	})
}

var gitlabRegexLit = regexp.MustCompile(`^/(.*)/[a-z]*$`)
