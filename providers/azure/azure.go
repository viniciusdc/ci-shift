// Package azure converts Azure Pipelines YAML definitions to and from the IR.
package azure

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// Provider is the Azure Pipelines provider.
type Provider struct{}

// Name implements ir.Provider.
func (Provider) Name() string { return "azure" }

// Title implements ir.Provider.
func (Provider) Title() string { return "Azure Pipelines" }

// DefaultPath implements ir.Provider.
func (Provider) DefaultPath() string { return "azure-pipelines.yml" }

// Detect implements ir.Provider.
func (Provider) Detect(path string, data []byte) int {
	slash := filepath.ToSlash(path)
	base := strings.ToLower(filepath.Base(slash))
	if base == "azure-pipelines.yml" || base == "azure-pipelines.yaml" ||
		strings.Contains(slash, ".azure-pipelines/") || strings.Contains(slash, ".azuredevops/") {
		return 100
	}
	root, err := yamlx.Load(data)
	if err != nil || !yamlx.IsMap(root) {
		return 0
	}
	// Structure: Azure uses lists of stage/job maps where the other
	// providers use maps keyed by job name.
	structure := 0
	for _, s := range yamlx.Items(yamlx.Get(root, "stages")) {
		if yamlx.Has(s, "stage") || yamlx.Has(s, "template") {
			structure = 50
			break
		}
	}
	for _, j := range yamlx.Items(yamlx.Get(root, "jobs")) {
		if yamlx.Has(j, "job") || yamlx.Has(j, "deployment") || yamlx.Has(j, "template") {
			structure = 50
			break
		}
	}
	for _, st := range yamlx.Items(yamlx.Get(root, "steps")) {
		if isStepNode(st) {
			structure = max(structure, 40)
			break
		}
	}
	if yamlx.Has(root, "extends") {
		structure = max(structure, 30)
	}
	extra := 0
	for _, k := range []string{"trigger", "pr", "pool", "schedules"} {
		if yamlx.Has(root, k) {
			extra += 10
		}
	}
	if structure == 0 {
		return min(extra, 20)
	}
	return min(structure+extra, 80)
}

// isStepNode reports whether n looks like an Azure step.
func isStepNode(n *yaml.Node) bool {
	for _, k := range []string{"script", "bash", "pwsh", "powershell", "task", "checkout", "publish", "download"} {
		if yamlx.Has(n, k) {
			return true
		}
	}
	return false
}

var _ ir.Provider = Provider{}

// macroVars maps canonical CI values to Azure predefined variables, used as
// $(Name) macros. When several canonical names share a variable, the first
// entry wins on parsing.
var macroVars = ir.NativeVars{
	{Canon: ir.VarSHA, Native: "Build.SourceVersion"},
	{Canon: ir.VarRefName, Native: "Build.SourceBranchName"},
	{Canon: ir.VarBranch, Native: "Build.SourceBranchName"},
	{Canon: ir.VarTag, Native: "Build.SourceBranchName"},
	{Canon: ir.VarRepository, Native: "Build.Repository.Name"},
	{Canon: ir.VarRunID, Native: "Build.BuildId"},
	{Canon: ir.VarRunNumber, Native: "Build.BuildNumber"},
	{Canon: ir.VarWorkspace, Native: "Build.SourcesDirectory"},
	{Canon: ir.VarJob, Native: "Agent.JobName"},
	{Canon: ir.VarActor, Native: "Build.RequestedFor"},
	{Canon: ir.VarPRNumber, Native: "System.PullRequest.PullRequestNumber"},
	{Canon: ir.VarEvent, Native: "Build.Reason"},
	{Canon: ir.VarServerURL, Native: "System.CollectionUri"},
	// Aliases only recognized when parsing.
	{Canon: ir.VarWorkspace, Native: "System.DefaultWorkingDirectory"},
	{Canon: ir.VarJob, Native: "System.JobName"},
}

// shellVars are the environment variable forms of macroVars that Azure
// exports to scripts (upper case, dots replaced by underscores).
var shellVars = func() ir.NativeVars {
	out := make(ir.NativeVars, 0, len(macroVars))
	for _, v := range macroVars {
		out = append(out, ir.NativeVar{Canon: v.Canon, Native: envName(v.Native)})
	}
	return out
}()

// envName is the environment variable Azure exports for a pipeline variable.
func envName(variable string) string {
	return strings.ToUpper(strings.ReplaceAll(variable, ".", "_"))
}

// macroToCanon looks up a predefined variable (case-insensitively, as Azure
// does) and returns its canonical name.
func macroToCanon(name string) (string, bool) {
	for _, v := range macroVars {
		if strings.EqualFold(v.Native, name) {
			return v.Canon, true
		}
	}
	return "", false
}

// predefinedPrefixes are the namespaces of Azure predefined variables.
var predefinedPrefixes = []string{"build.", "system.", "agent.", "pipeline.", "environment.", "resources.", "release.", "tf_"}

func isPredefined(name string) bool {
	l := strings.ToLower(name)
	for _, p := range predefinedPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// builtinEnvPrefixes are the environment variable prefixes of predefined
// variables, used when promoting unknown shell variables to secrets.
var builtinEnvPrefixes = []string{"BUILD_", "SYSTEM_", "AGENT_", "TF_", "PIPELINE_", "ENVIRONMENT_", "RESOURCES_", "RELEASE_"}
