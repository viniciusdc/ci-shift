// Package ir defines ci-shift's provider-neutral pipeline model.
//
// Every provider parses its native configuration into a *Pipeline and emits
// a *Pipeline back into its native configuration, so N providers need N
// parsers and N emitters instead of N×(N-1) pairwise converters.
//
// # Expressions
//
// Text fields (scripts, env values, images, ...) may contain canonical
// references that emitters rewrite into each provider's native syntax:
//
//	${{ ci.<name> }}      a well-known CI value (see the Var* constants)
//	${{ secret.<NAME> }}  a secret configured in the CI provider
//	${{ env.<NAME> }}     a pipeline/job variable, used outside of scripts
//	${{ matrix.<NAME> }}  the current value of a matrix axis
//
// Plain shell references such as $NAME inside scripts are left untouched,
// because every provider runs scripts with variables exported to the shell.
package ir

// Pipeline is a whole CI configuration.
type Pipeline struct {
	Name     string
	Triggers Triggers
	Env      []KV
	Jobs     []*Job
}

// KV is an ordered key/value pair.
type KV struct {
	Key   string
	Value string
}

// Triggers describes when a pipeline runs. A nil Push/PullRequest means the
// pipeline is not triggered by that event.
type Triggers struct {
	Push        *PushTrigger
	PullRequest *PRTrigger
	Schedules   []Schedule
	Manual      bool
}

// Empty reports whether no trigger is configured at all.
func (t Triggers) Empty() bool {
	return t.Push == nil && t.PullRequest == nil && len(t.Schedules) == 0 && !t.Manual
}

// PushTrigger runs the pipeline on pushes. With neither Branches nor Tags
// set, every push triggers. When only one of them is set, only that kind
// of ref triggers: Tags alone means "tags only". Use Branches ["*"] with
// Tags to mean "every branch plus these tags".
type PushTrigger struct {
	Branches       []string
	BranchesIgnore []string
	Tags           []string
	Paths          []string
	PathsIgnore    []string
}

// PRTrigger runs the pipeline for pull/merge requests targeting Branches.
type PRTrigger struct {
	Branches []string
	Paths    []string
}

// Schedule is a cron trigger (standard 5-field cron, UTC). An empty Cron
// means the source defines the schedule outside the file (GitLab and
// Bitbucket keep schedules in project settings); see CronSchedules.
type Schedule struct {
	Cron     string
	Branches []string
}

// Job is a unit scheduled on a runner/executor.
type Job struct {
	ID       string
	Name     string
	Stage    string
	Needs    []string
	Runner   Runner
	Image    string
	Services []Service
	Env      []KV
	Steps    []*Step
	If       *Condition
	Matrix   *Matrix

	TimeoutMinutes int
	AllowFailure   bool
	Environment    string
}

// DisplayName returns Name, falling back to ID.
func (j *Job) DisplayName() string {
	if j.Name != "" {
		return j.Name
	}
	return j.ID
}

// OS values for Runner.OS.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
	OSMacOS   = "macos"
)

// Runner describes the machine a job runs on. An empty OS means Linux.
// Labels select self-hosted/tagged runners; VMImage keeps a specific hosted
// image name when the source named one (e.g. "ubuntu-22.04").
type Runner struct {
	OS      string
	Labels  []string
	VMImage string
}

// IsLinux reports whether the runner is Linux (the default).
func (r Runner) IsLinux() bool { return r.OS == "" || r.OS == OSLinux }

// Service is a sidecar container (database, cache, ...).
type Service struct {
	Name  string
	Image string
	Env   []KV
	Ports []string
}

// Matrix fans a job out over variable combinations. Axes form a cartesian
// product; Exclude removes combinations from the product; Include extends
// product combinations or adds new ones, with GitHub's semantics (without
// Axes, each entry is its own combination).
type Matrix struct {
	Axes    []Axis
	Include [][]KV
	Exclude [][]KV
}

// Axis is one matrix dimension.
type Axis struct {
	Name   string
	Values []string
}

// When values for Condition.When.
const (
	WhenOnSuccess = ""
	WhenAlways    = "always"
	WhenOnFailure = "on_failure"
)

// Event values for Condition.Event.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventSchedule    = "schedule"
	EventManual      = "manual"
	EventTag         = "tag"
)

// Condition restricts when a job or step runs. All set fields must hold.
type Condition struct {
	When     string
	Branches []string
	Event    string
	Manual   bool
	// Raw keeps a provider-specific expression ci-shift could not translate.
	Raw     string
	RawFrom string
}

// IsZero reports whether the condition is a no-op.
func (c *Condition) IsZero() bool {
	return c == nil || (c.When == "" && len(c.Branches) == 0 && c.Event == "" && !c.Manual && c.Raw == "")
}

// StepKind identifies what a step does.
type StepKind string

// Step kinds.
const (
	StepRun      StepKind = "run"
	StepCheckout StepKind = "checkout"
	StepTool     StepKind = "tool"
	StepCache    StepKind = "cache"
	StepUpload   StepKind = "upload"
	StepDownload StepKind = "download"
	// StepAction is a provider-specific reusable unit (GitHub action, Azure
	// task, CircleCI orb command) with no portable equivalent.
	StepAction StepKind = "action"
)

// Step is one entry of a job's step list.
type Step struct {
	Kind StepKind
	Name string

	// StepRun
	Run   string
	Shell string

	WorkingDir string
	// Env holds step variables. Values are literal apart from canonical
	// references, which emitters translate.
	Env            []KV
	If             *Condition
	AllowFailure   bool
	TimeoutMinutes int

	// StepAction
	Uses     string
	With     []KV
	UsesFrom string // provider the action belongs to

	// StepTool
	Tool    string
	Version string

	// StepCache
	Cache *Cache

	// StepUpload / StepDownload
	Artifact *Artifact

	// StepCheckout
	Checkout *Checkout
}

// Known tool names for StepTool.
const (
	ToolNode   = "node"
	ToolPython = "python"
	ToolGo     = "go"
	ToolJava   = "java"
	ToolRuby   = "ruby"
	ToolDotnet = "dotnet"
)

// Cache restores and saves Paths under a key derived from Prefix and the
// hash of Files (lockfiles).
type Cache struct {
	Prefix string
	Files  []string
	Paths  []string
}

// Artifact is a set of files published by one job and consumed by another.
type Artifact struct {
	Name     string
	Paths    []string
	ExpireIn string
	When     string // WhenOnSuccess, WhenAlways or WhenOnFailure
}

// Checkout clones the repository. Depth 0 means the provider default,
// -1 the full history and any positive value a shallow clone of that depth.
type Checkout struct {
	Depth      int
	Submodules bool
	LFS        bool
}

// FindJob returns the job with the given ID.
func (p *Pipeline) FindJob(id string) *Job {
	for _, j := range p.Jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// KVGet returns the value for key in kvs.
func KVGet(kvs []KV, key string) (string, bool) {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

// KVSet sets key in kvs, replacing an existing entry.
func KVSet(kvs []KV, key, value string) []KV {
	for i := range kvs {
		if kvs[i].Key == key {
			kvs[i].Value = value
			return kvs
		}
	}
	return append(kvs, KV{Key: key, Value: value})
}

// CronSchedules returns the schedules that carry a cron expression and
// warns about those that do not, for emitters that need one.
func (t Triggers) CronSchedules(d *Diagnostics, path string) []Schedule {
	var out []Schedule
	for _, s := range t.Schedules {
		if s.Cron == "" {
			d.Warnf(path, "the source schedules this pipeline outside the file; add the cron expression by hand")
			continue
		}
		out = append(out, s)
	}
	return out
}
