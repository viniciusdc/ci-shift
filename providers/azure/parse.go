package azure

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

type parser struct {
	d          *ir.Diagnostics
	containers map[string]*yaml.Node // resources.containers by alias
	promoted   map[string]bool       // $(NAME) macros turned into secrets
}

// rawStage and rawJob are the first parsing pass: names and IDs are
// collected before jobs are parsed so dependencies can be resolved.
type rawStage struct {
	name     string // stage ID, used by dependsOn
	display  string // IR stage name (displayName when set)
	path     string
	node     *yaml.Node // nil for pipelines without stages
	jobs     []*rawJob
	deps     []string
	explicit bool // dependsOn was set (possibly to [])
}

type rawJob struct {
	name string
	id   string
	path string
	node *yaml.Node
}

// jobCtx carries stage-level defaults into parseJob.
type jobCtx struct {
	scope  *scope
	pool   *yaml.Node
	env    []ir.KV
	cond   *ir.Condition
	manual bool
}

// Parse implements ir.Provider.
func (Provider) Parse(data []byte, d *ir.Diagnostics) (*ir.Pipeline, error) {
	root, err := yamlx.Load(data)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	if !yamlx.IsMap(root) {
		return nil, fmt.Errorf("azure: pipeline must be a mapping")
	}
	if yamlx.Has(root, "extends") {
		return nil, fmt.Errorf("azure: pipelines that extend a template cannot be converted; convert the template instead")
	}
	ps := &parser{d: d, containers: map[string]*yaml.Node{}, promoted: map[string]bool{}}
	p := &ir.Pipeline{}
	if name := yamlx.Str(yamlx.Get(root, "name")); name != "" {
		if strings.Contains(name, "$(") {
			d.Infof("name", "run number format %q was dropped", name)
		} else {
			p.Name = name
		}
	}
	if yamlx.Has(root, "parameters") {
		d.Warnf("parameters", "runtime parameters are not portable; ${{ parameters.* }} expressions are kept verbatim")
	}
	for _, key := range []string{"lockBehavior", "appendCommitMessageToRunName"} {
		if yamlx.Has(root, key) {
			d.Infof(key, "%q has no portable equivalent and was dropped", key)
		}
	}
	p.Triggers = ps.parseTriggers(root)
	ps.parseResources(yamlx.Get(root, "resources"))
	rootScope := newScope(nil)
	p.Env = ps.parseVariables(yamlx.Get(root, "variables"), rootScope, "variables")

	stages, err := ps.collect(root)
	if err != nil {
		return nil, err
	}
	byStage := map[string][]string{} // stage name -> IR job IDs
	for i, st := range stages {
		ctx := &jobCtx{scope: newScope(rootScope), pool: yamlx.Get(root, "pool")}
		if st.node != nil {
			ps.stageDefaults(st, ctx)
		}
		local := map[string]string{} // job name -> IR ID within the stage
		for _, rj := range st.jobs {
			local[rj.name] = rj.id
		}
		// Absent dependsOn on a stage means "the previous stage".
		deps := st.deps
		if st.node != nil && !st.explicit && i > 0 {
			deps = []string{stages[i-1].name}
		}
		var stageNeeds []string
		for _, dep := range deps {
			ids, ok := byStage[dep]
			if !ok {
				d.Warnf(st.path+".dependsOn", "stage %q is unknown or not declared earlier; the dependency was dropped", dep)
				continue
			}
			stageNeeds = append(stageNeeds, ids...)
		}
		for _, rj := range st.jobs {
			j, deps := ps.parseJob(rj, ctx)
			j.Stage = st.display
			for _, dep := range deps {
				id, ok := local[dep]
				if !ok {
					d.Warnf(rj.path+".dependsOn", "job %q is unknown; the dependency was dropped", dep)
					continue
				}
				j.Needs = append(j.Needs, id)
			}
			if len(j.Needs) == 0 {
				// First-level jobs of a stage wait for the stages it depends on.
				j.Needs = append(j.Needs, stageNeeds...)
				if ctx.manual {
					if j.If == nil {
						j.If = &ir.Condition{}
					}
					j.If.Manual = true
				}
			}
			p.Jobs = append(p.Jobs, j)
			byStage[st.name] = append(byStage[st.name], j.ID)
		}
	}
	if len(p.Jobs) == 0 {
		return nil, fmt.Errorf("azure: pipeline has no jobs")
	}
	ps.promoteShellVars(p)
	return p, nil
}

// collect runs the first pass over stages/jobs/steps and assigns unique
// job IDs (prefixed with the stage name when a job name repeats).
func (ps *parser) collect(root *yaml.Node) ([]*rawStage, error) {
	var stages []*rawStage
	switch {
	case yamlx.Has(root, "stages"):
		for i, sn := range yamlx.Items(yamlx.Get(root, "stages")) {
			path := fmt.Sprintf("stages[%d]", i)
			if yamlx.Has(sn, "template") {
				ps.d.Warnf(path, "stage template %q is not resolved; convert it separately", yamlx.Str(yamlx.Get(sn, "template")))
				continue
			}
			name := yamlx.Str(yamlx.Get(sn, "stage"))
			if name == "" {
				name = fmt.Sprintf("stage%d", i+1)
			}
			st := &rawStage{name: name, display: name, path: "stages." + name, node: sn}
			if dn := yamlx.Str(yamlx.Get(sn, "displayName")); dn != "" {
				st.display = dn
			}
			if dep := yamlx.Get(sn, "dependsOn"); dep != nil {
				st.explicit = true
				st.deps = yamlx.StrList(dep)
			}
			st.jobs = ps.collectJobs(yamlx.Get(sn, "jobs"), st.path+".jobs")
			stages = append(stages, st)
		}
	case yamlx.Has(root, "jobs"):
		stages = []*rawStage{{jobs: ps.collectJobs(yamlx.Get(root, "jobs"), "jobs")}}
	case yamlx.Has(root, "steps"):
		// A bare steps list is a single implicit job; root keys that
		// belong to that job are copied into a synthetic job node.
		job := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, key := range []string{"container", "services", "strategy", "workspace", "continueOnError", "steps"} {
			if v := yamlx.Get(root, key); v != nil {
				job.Content = append(job.Content, yamlx.Key(key), v)
			}
		}
		stages = []*rawStage{{jobs: []*rawJob{{name: "job", id: "job", path: "steps", node: job}}}}
		return stages, nil
	default:
		return nil, fmt.Errorf("azure: pipeline has no stages, jobs or steps")
	}
	count := map[string]int{}
	for _, st := range stages {
		for _, rj := range st.jobs {
			count[rj.name]++
		}
	}
	taken := map[string]bool{}
	for _, st := range stages {
		for _, rj := range st.jobs {
			base := rj.name
			if count[rj.name] > 1 && st.name != "" {
				base = st.name + "_" + rj.name
			}
			rj.id = base
			for i := 2; taken[rj.id]; i++ {
				rj.id = fmt.Sprintf("%s_%d", base, i)
			}
			taken[rj.id] = true
		}
	}
	return stages, nil
}

func (ps *parser) collectJobs(n *yaml.Node, path string) []*rawJob {
	var out []*rawJob
	for i, jn := range yamlx.Items(n) {
		where := fmt.Sprintf("%s[%d]", path, i)
		if yamlx.Has(jn, "template") {
			ps.d.Warnf(where, "job template %q is not resolved; convert it separately", yamlx.Str(yamlx.Get(jn, "template")))
			continue
		}
		name := yamlx.Str(yamlx.Get(jn, "job"))
		if yamlx.Has(jn, "deployment") {
			name = yamlx.Str(yamlx.Get(jn, "deployment"))
		} else if !yamlx.Has(jn, "job") {
			ps.d.Warnf(where, "entry is neither a job nor a deployment and was dropped")
			continue
		}
		if name == "" {
			name = "job"
		}
		out = append(out, &rawJob{name: name, path: path + "." + name, node: jn})
	}
	return out
}

// stageDefaults applies stage-level variables, pool and condition.
func (ps *parser) stageDefaults(st *rawStage, ctx *jobCtx) {
	n := st.node
	ctx.env = ps.parseVariables(yamlx.Get(n, "variables"), ctx.scope, st.path+".variables")
	if pool := yamlx.Get(n, "pool"); pool != nil {
		ctx.pool = pool
	}
	if cond := yamlx.Str(yamlx.Get(n, "condition")); cond != "" {
		ctx.cond = ps.parseCondition(cond, st.path+".condition")
	}
	if yamlx.Str(yamlx.Get(n, "trigger")) == "manual" {
		ctx.manual = true
		ps.d.Infof(st.path+".trigger", "manually triggered stage was converted to manual jobs")
	}
	for _, key := range []string{"lockBehavior", "isSkippable", "templateContext", "checks", "group"} {
		if yamlx.Has(n, key) {
			ps.d.Infof(st.path+"."+key, "%q has no portable equivalent and was dropped", key)
		}
	}
}

// sanitizeVar makes a variable name portable ("python.version" becomes
// "python_version").
func (ps *parser) sanitizeVar(name, path string) string {
	if identRe.MatchString(name) {
		return name
	}
	out := strings.NewReplacer(".", "_", "-", "_", " ", "_").Replace(name)
	ps.d.Infof(path, "variable %q was renamed to %q", name, out)
	return out
}

// parseVariables reads a variables section (mapping or list form) and
// defines the names in sc so that values and later text can refer to them.
func (ps *parser) parseVariables(n *yaml.Node, sc *scope, path string) []ir.KV {
	type raw struct{ name, value string }
	var vars []raw
	switch {
	case yamlx.IsMap(n):
		for _, pair := range yamlx.Pairs(n) {
			v := pair.Value
			if yamlx.IsMap(v) {
				v = yamlx.Get(v, "value")
			}
			vars = append(vars, raw{pair.Key, yamlx.Str(v)})
		}
	case yamlx.IsSeq(n):
		for _, item := range yamlx.Items(n) {
			switch {
			case yamlx.Has(item, "group"):
				ps.d.Warnf(path, "variable group %q is not portable; variables it provides are treated as secrets", yamlx.Str(yamlx.Get(item, "group")))
			case yamlx.Has(item, "template"):
				ps.d.Warnf(path, "variable template %q is not resolved", yamlx.Str(yamlx.Get(item, "template")))
			default:
				vars = append(vars, raw{yamlx.Str(yamlx.Get(item, "name")), yamlx.Str(yamlx.Get(item, "value"))})
			}
		}
	}
	var out []ir.KV
	for _, v := range vars {
		if v.name == "" {
			continue
		}
		if isPredefined(v.name) {
			ps.d.Infof(path+"."+v.name, "setting predefined variable %q is not portable and was dropped", v.name)
			continue
		}
		sc.vars[strings.ToLower(v.name)] = ps.sanitizeVar(v.name, path+"."+v.name)
	}
	for _, v := range vars {
		if v.name == "" || isPredefined(v.name) {
			continue
		}
		name := sc.vars[strings.ToLower(v.name)]
		out = ir.KVSet(out, name, ps.fromAZ(v.value, sc, false, path+"."+v.name))
	}
	return out
}

func (ps *parser) parseResources(n *yaml.Node) {
	for _, c := range yamlx.Items(yamlx.Get(n, "containers")) {
		if alias := yamlx.Str(yamlx.Get(c, "container")); alias != "" {
			ps.containers[alias] = c
		}
	}
	for _, r := range yamlx.Items(yamlx.Get(n, "repositories")) {
		if alias := yamlx.Str(yamlx.Get(r, "repository")); alias != "self" {
			ps.d.Warnf("resources.repositories", "repository resource %q is not portable and was dropped", alias)
		}
	}
	for _, key := range []string{"pipelines", "builds", "packages", "webhooks"} {
		if yamlx.Has(n, key) {
			ps.d.Warnf("resources."+key, "%s resources are not portable and were dropped", key)
		}
	}
}

// branchName strips the refs/heads/ prefix from a branch filter.
func branchName(s string) string { return strings.TrimPrefix(s, "refs/heads/") }

func (ps *parser) parseTriggers(root *yaml.Node) ir.Triggers {
	var t ir.Triggers
	// Absent trigger means CI on every branch.
	switch tr := yamlx.Get(root, "trigger"); {
	case yamlx.IsNull(tr):
		t.Push = &ir.PushTrigger{}
	case yamlx.IsScalar(tr):
		switch strings.ToLower(tr.Value) {
		case "none", "false":
		case "true":
			t.Push = &ir.PushTrigger{}
		default:
			t.Push = &ir.PushTrigger{Branches: []string{branchName(tr.Value)}}
		}
	case yamlx.IsSeq(tr):
		t.Push = &ir.PushTrigger{}
		ps.addBranches(t.Push, yamlx.StrList(tr))
	case yamlx.IsMap(tr):
		push := &ir.PushTrigger{}
		ps.addBranches(push, yamlx.StrList(yamlx.Path(tr, "branches", "include")))
		for _, b := range yamlx.StrList(yamlx.Path(tr, "branches", "exclude")) {
			push.BranchesIgnore = append(push.BranchesIgnore, branchName(b))
		}
		for _, tag := range yamlx.StrList(yamlx.Path(tr, "tags", "include")) {
			push.Tags = append(push.Tags, strings.TrimPrefix(tag, "refs/tags/"))
		}
		if yamlx.Has(yamlx.Get(tr, "tags"), "exclude") {
			ps.d.Warnf("trigger.tags.exclude", "tag exclude filters are not portable and were dropped")
		}
		push.Paths = yamlx.StrList(yamlx.Path(tr, "paths", "include"))
		push.PathsIgnore = yamlx.StrList(yamlx.Path(tr, "paths", "exclude"))
		// "exclude every branch, include tags" is how tag-only CI is written.
		if len(push.Branches) == 0 && len(push.Tags) > 0 && len(push.BranchesIgnore) == 1 && push.BranchesIgnore[0] == "*" {
			push.BranchesIgnore = nil
		}
		if yamlx.Has(tr, "batch") {
			ps.d.Infof("trigger.batch", "batched CI runs are not portable and were dropped")
		}
		t.Push = push
	}

	// Absent pr means pull requests to any branch (GitHub/Bitbucket repos).
	switch pr := yamlx.Get(root, "pr"); {
	case yamlx.IsNull(pr):
		t.PullRequest = &ir.PRTrigger{}
	case yamlx.IsScalar(pr):
		switch strings.ToLower(pr.Value) {
		case "none", "false":
		case "true":
			t.PullRequest = &ir.PRTrigger{}
		default:
			t.PullRequest = &ir.PRTrigger{Branches: []string{branchName(pr.Value)}}
		}
	case yamlx.IsSeq(pr):
		t.PullRequest = &ir.PRTrigger{}
		for _, b := range yamlx.StrList(pr) {
			t.PullRequest.Branches = append(t.PullRequest.Branches, branchName(b))
		}
	case yamlx.IsMap(pr):
		t.PullRequest = &ir.PRTrigger{Paths: yamlx.StrList(yamlx.Path(pr, "paths", "include"))}
		for _, b := range yamlx.StrList(yamlx.Path(pr, "branches", "include")) {
			t.PullRequest.Branches = append(t.PullRequest.Branches, branchName(b))
		}
		if yamlx.Has(yamlx.Get(pr, "branches"), "exclude") || yamlx.Has(yamlx.Get(pr, "paths"), "exclude") {
			ps.d.Warnf("pr", "pull request exclude filters are not portable and were dropped")
		}
		for _, key := range []string{"autoCancel", "drafts"} {
			if yamlx.Has(pr, key) {
				ps.d.Infof("pr."+key, "%q has no portable equivalent and was dropped", key)
			}
		}
	}

	for i, s := range yamlx.Items(yamlx.Get(root, "schedules")) {
		cron := yamlx.Str(yamlx.Get(s, "cron"))
		if cron == "" {
			continue
		}
		sched := ir.Schedule{Cron: cron}
		for _, b := range yamlx.StrList(yamlx.Path(s, "branches", "include")) {
			sched.Branches = append(sched.Branches, branchName(b))
		}
		if yamlx.Has(yamlx.Get(s, "branches"), "exclude") {
			ps.d.Warnf(fmt.Sprintf("schedules[%d].branches.exclude", i), "schedule branch exclude filters are not portable and were dropped")
		}
		t.Schedules = append(t.Schedules, sched)
	}
	return t
}

// addBranches adds branch include filters; refs/tags/ entries are tags.
func (ps *parser) addBranches(push *ir.PushTrigger, list []string) {
	for _, b := range list {
		if tag, ok := strings.CutPrefix(b, "refs/tags/"); ok {
			push.Tags = append(push.Tags, tag)
			continue
		}
		push.Branches = append(push.Branches, branchName(b))
	}
}

// boolish reads booleans written either as YAML booleans or strings.
func boolish(n *yaml.Node) bool {
	v, _ := yamlx.Bool(n)
	return v
}

func (ps *parser) parseJob(rj *rawJob, ctx *jobCtx) (*ir.Job, []string) {
	n, path := rj.node, rj.path
	d := ps.d
	sc := newScope(ctx.scope)
	j := &ir.Job{ID: rj.id, Name: yamlx.Str(yamlx.Get(n, "displayName"))}
	if j.Name == "" && rj.id != rj.name {
		j.Name = rj.name
	}
	deps := yamlx.StrList(yamlx.Get(n, "dependsOn"))
	deployment := yamlx.Has(n, "deployment")

	// Matrix variables must be known before any text is converted.
	strategy := yamlx.Get(n, "strategy")
	matrix := yamlx.Get(strategy, "matrix")
	if !deployment && yamlx.IsMap(matrix) {
		for _, leg := range yamlx.Pairs(matrix) {
			for _, kv := range yamlx.Pairs(leg.Value) {
				sc.matrix[strings.ToLower(kv.Key)] = ps.sanitizeVar(kv.Key, path+".strategy.matrix."+leg.Key)
			}
		}
	}
	j.Env = append(j.Env, ctx.env...)
	for _, kv := range ps.parseVariables(yamlx.Get(n, "variables"), sc, path+".variables") {
		j.Env = ir.KVSet(j.Env, kv.Key, kv.Value)
	}

	j.If = ps.parseCondition(yamlx.Str(yamlx.Get(n, "condition")), path+".condition")
	if !ctx.cond.IsZero() {
		switch {
		case j.If == nil:
			c := *ctx.cond
			j.If = &c
		case !mergeCond(j.If, ctx.cond):
			d.Warnf(path+".condition", "the stage condition could not be combined with the job condition and was dropped")
		}
	}
	j.AllowFailure = boolish(yamlx.Get(n, "continueOnError"))
	if v, ok := yamlx.Int(yamlx.Get(n, "timeoutInMinutes")); ok {
		j.TimeoutMinutes = v
	}
	pool := yamlx.Get(n, "pool")
	if pool == nil {
		pool = ctx.pool
	}
	j.Runner = ps.parsePool(pool, sc, path+".pool")
	if c := yamlx.Get(n, "container"); c != nil {
		var env []ir.KV
		j.Image, env, _ = ps.parseContainer(c, sc, path+".container")
		j.Env = append(j.Env, env...)
	}
	for _, pair := range yamlx.Pairs(yamlx.Get(n, "services")) {
		svc := ir.Service{Name: pair.Key}
		svc.Image, svc.Env, svc.Ports = ps.parseContainer(pair.Value, sc, path+".services."+pair.Key)
		j.Services = append(j.Services, svc)
	}
	if yamlx.Has(n, "workspace") {
		d.Infof(path+".workspace", "workspace cleaning options were dropped")
	}
	for _, key := range []string{"cancelTimeoutInMinutes", "uses", "templateContext"} {
		if yamlx.Has(n, key) {
			d.Infof(path+"."+key, "%q has no portable equivalent and was dropped", key)
		}
	}

	var steps *yaml.Node
	if deployment {
		j.Environment = ps.parseEnvironment(yamlx.Get(n, "environment"), path)
		steps = ps.deploymentSteps(strategy, path+".strategy")
	} else {
		steps = yamlx.Get(n, "steps")
		switch {
		case yamlx.IsScalar(matrix):
			d.Warnf(path+".strategy.matrix", "runtime matrix %q cannot be converted and was dropped", matrix.Value)
		case yamlx.IsMap(matrix):
			m := &ir.Matrix{}
			for _, leg := range yamlx.Pairs(matrix) {
				var combo []ir.KV
				for _, kv := range yamlx.Pairs(leg.Value) {
					combo = append(combo, ir.KV{Key: sc.matrix[strings.ToLower(kv.Key)], Value: ps.fromAZ(yamlx.Str(kv.Value), sc, false, path+".strategy.matrix")})
				}
				m.Include = append(m.Include, combo)
			}
			if len(m.Include) > 0 {
				j.Matrix = m
				d.Infof(path+".strategy.matrix", "matrix leg names were dropped; legs are identified by their values")
			}
		}
		if yamlx.Has(strategy, "maxParallel") {
			d.Infof(path+".strategy.maxParallel", "maxParallel was dropped")
		}
		if yamlx.Has(strategy, "parallel") {
			d.Warnf(path+".strategy.parallel", "parallel job slicing is not portable and was dropped")
		}
	}

	var sawCheckout, noCheckout, sawDownload bool
	for i, sn := range yamlx.Items(steps) {
		where := fmt.Sprintf("%s.steps[%d]", path, i)
		st, marker := ps.parseStep(sn, sc, where)
		switch marker {
		case "checkout":
			sawCheckout = true
		case "checkout-none":
			noCheckout = true
		case "download":
			sawDownload = true
		}
		if st != nil {
			j.Steps = append(j.Steps, st)
		}
	}
	if deployment {
		// Deployment jobs do not check out the repository but download
		// every artifact of the run unless a download step says otherwise.
		if !sawDownload {
			j.Steps = append([]*ir.Step{{Kind: ir.StepDownload, Artifact: &ir.Artifact{}}}, j.Steps...)
		}
	} else if !sawCheckout && !noCheckout {
		// Regular jobs check out the repository implicitly.
		j.Steps = append([]*ir.Step{{Kind: ir.StepCheckout, Checkout: &ir.Checkout{}}}, j.Steps...)
	}
	return j, deps
}

func (ps *parser) parseEnvironment(n *yaml.Node, path string) string {
	name := yamlx.Str(n)
	if yamlx.IsMap(n) {
		name = yamlx.Str(yamlx.Get(n, "name"))
		if yamlx.Has(n, "resourceName") || yamlx.Has(n, "tags") {
			ps.d.Infof(path+".environment", "environment resource targeting was dropped")
		}
	}
	if env, res, ok := strings.Cut(name, "."); ok {
		ps.d.Infof(path+".environment", "environment resource %q was dropped", res)
		name = env
	}
	return name
}

// deploymentSteps returns the deploy steps of a deployment strategy.
func (ps *parser) deploymentSteps(strategy *yaml.Node, path string) *yaml.Node {
	for _, kind := range []string{"runOnce", "rolling", "canary"} {
		s := yamlx.Get(strategy, kind)
		if s == nil {
			continue
		}
		if kind != "runOnce" {
			ps.d.Warnf(path+"."+kind, "%s deployments were converted to a single run of the deploy steps", kind)
		}
		for _, hook := range yamlx.Keys(s) {
			if hook != "deploy" && hook != "maxParallel" && hook != "increments" {
				ps.d.Warnf(path+"."+kind+"."+hook, "deployment lifecycle hook %q is not portable and was dropped", hook)
			}
		}
		return yamlx.Path(s, "deploy", "steps")
	}
	ps.d.Warnf(path, "deployment job has no supported strategy")
	return nil
}

func (ps *parser) parsePool(n *yaml.Node, sc *scope, path string) ir.Runner {
	var r ir.Runner
	name := yamlx.Str(n)
	if yamlx.IsMap(n) {
		name = yamlx.Str(yamlx.Get(n, "name"))
		if img := yamlx.Str(yamlx.Get(n, "vmImage")); img != "" {
			r.VMImage = ps.fromAZ(img, sc, false, path+".vmImage")
			r.OS = osFromImage(img)
		}
		for _, dem := range yamlx.StrList(yamlx.Get(n, "demands")) {
			l := strings.ToLower(dem)
			if strings.HasPrefix(l, "agent.os") && strings.Contains(l, "-equals") {
				switch {
				case strings.Contains(l, "windows"):
					r.OS = ir.OSWindows
				case strings.Contains(l, "darwin"):
					r.OS = ir.OSMacOS
				default:
					r.OS = ir.OSLinux
				}
				continue
			}
			r.Labels = append(r.Labels, dem)
		}
	}
	if name != "" && !strings.EqualFold(name, "Azure Pipelines") {
		r.Labels = append([]string{name}, r.Labels...)
	}
	return r
}

func osFromImage(img string) string {
	l := strings.ToLower(img)
	switch {
	case strings.HasPrefix(l, "windows") || strings.HasPrefix(l, "vs20") || strings.HasPrefix(l, "win"):
		return ir.OSWindows
	case strings.HasPrefix(l, "macos"):
		return ir.OSMacOS
	}
	return ir.OSLinux
}

// parseContainer reads a container reference (resource alias or image) or
// an inline container spec.
func (ps *parser) parseContainer(n *yaml.Node, sc *scope, path string) (image string, env []ir.KV, ports []string) {
	spec := n
	if yamlx.IsScalar(n) {
		res, ok := ps.containers[n.Value]
		if !ok {
			return ps.fromAZ(n.Value, sc, false, path), nil, nil
		}
		spec = res
	} else if alias := yamlx.Str(yamlx.Get(n, "alias")); alias != "" {
		spec = ps.containers[alias]
	}
	image = ps.fromAZ(yamlx.Str(yamlx.Get(spec, "image")), sc, false, path+".image")
	for _, pair := range yamlx.Pairs(yamlx.Get(spec, "env")) {
		env = append(env, ir.KV{Key: pair.Key, Value: ps.fromAZ(yamlx.Str(pair.Value), sc, false, path+".env")})
	}
	ports = yamlx.StrList(yamlx.Get(spec, "ports"))
	if yamlx.Has(spec, "endpoint") {
		ps.d.Warnf(path, "container registry endpoint %q was dropped; configure registry credentials in the target CI", yamlx.Str(yamlx.Get(spec, "endpoint")))
	}
	for _, key := range []string{"options", "volumes", "mountReadOnly", "mapDockerSocket"} {
		if yamlx.Has(spec, key) {
			ps.d.Warnf(path, "container %q was dropped", key)
		}
	}
	return image, env, ports
}

// scriptKeys maps Azure script step keys to IR shells.
var scriptKeys = []struct{ key, shell string }{
	{"script", ""}, {"bash", "bash"}, {"pwsh", "pwsh"}, {"powershell", "powershell"},
}

// parseStep converts one step. marker reports checkout/download steps
// that affect implicit behavior ("checkout", "checkout-none", "download").
func (ps *parser) parseStep(n *yaml.Node, sc *scope, path string) (*ir.Step, string) {
	d := ps.d
	for _, k := range yamlx.Keys(n) {
		if strings.HasPrefix(k, "${{") {
			d.Warnf(path, "conditional template insertion %q is not portable and was dropped", k)
			return nil, ""
		}
	}
	if t := yamlx.Get(n, "template"); t != nil {
		d.Warnf(path, "step template %q is not resolved; convert it separately", yamlx.Str(t))
		return nil, ""
	}
	if v, ok := yamlx.Bool(yamlx.Get(n, "enabled")); ok && !v {
		d.Warnf(path, "disabled step was dropped")
		if yamlx.Has(n, "checkout") {
			return nil, "checkout"
		}
		return nil, ""
	}
	st := &ir.Step{Name: ps.fromAZ(yamlx.Str(yamlx.Get(n, "displayName")), sc, false, path)}
	for _, pair := range yamlx.Pairs(yamlx.Get(n, "env")) {
		st.Env = append(st.Env, ir.KV{Key: pair.Key, Value: ps.fromAZ(yamlx.Str(pair.Value), sc, false, path+".env."+pair.Key)})
	}
	st.If = ps.parseCondition(yamlx.Str(yamlx.Get(n, "condition")), path+".condition")
	st.AllowFailure = boolish(yamlx.Get(n, "continueOnError"))
	if v, ok := yamlx.Int(yamlx.Get(n, "timeoutInMinutes")); ok {
		st.TimeoutMinutes = v
	}
	if yamlx.Has(n, "name") {
		d.Infof(path, "step name %q was dropped; output variables are not portable", yamlx.Str(yamlx.Get(n, "name")))
	}
	for _, key := range []string{"target", "retryCountOnTaskFailure"} {
		if yamlx.Has(n, key) {
			d.Warnf(path+"."+key, "%q is not portable and was dropped", key)
		}
	}

	for _, sk := range scriptKeys {
		if v := yamlx.Get(n, sk.key); v != nil {
			st.Kind = ir.StepRun
			st.Shell = sk.shell
			st.Run = ps.fromAZ(strings.TrimRight(yamlx.Str(v), "\n"), sc, true, path)
			st.WorkingDir = ps.fromAZ(yamlx.Str(yamlx.Get(n, "workingDirectory")), sc, false, path)
			for _, key := range []string{"failOnStderr", "errorActionPreference", "ignoreLASTEXITCODE"} {
				if yamlx.Has(n, key) {
					d.Infof(path+"."+key, "%q was dropped", key)
				}
			}
			return st, ""
		}
	}
	if co := yamlx.Get(n, "checkout"); co != nil {
		switch yamlx.Str(co) {
		case "none":
			return nil, "checkout-none"
		case "self":
		default:
			d.Warnf(path, "checkout of repository %q is not portable and was dropped", yamlx.Str(co))
			return nil, ""
		}
		st.Kind = ir.StepCheckout
		st.Checkout = &ir.Checkout{}
		if v, ok := yamlx.Int(yamlx.Get(n, "fetchDepth")); ok {
			st.Checkout.Depth = v
			if v == 0 {
				st.Checkout.Depth = -1 // full history
			}
		}
		if v := strings.ToLower(yamlx.Str(yamlx.Get(n, "submodules"))); v == "true" || v == "recursive" {
			st.Checkout.Submodules = true
		}
		st.Checkout.LFS = boolish(yamlx.Get(n, "lfs"))
		for _, key := range []string{"clean", "path", "persistCredentials", "fetchTags", "fetchFilter", "sparseCheckoutDirectories", "sparseCheckoutPatterns", "workspaceRepo"} {
			if yamlx.Has(n, key) {
				d.Infof(path+"."+key, "checkout option %q was dropped", key)
			}
		}
		return st, "checkout"
	}
	for _, key := range []string{"publish", "upload"} {
		if v := yamlx.Get(n, key); v != nil {
			st.Kind = ir.StepUpload
			st.Artifact = &ir.Artifact{
				Name:  ps.artifactName(yamlx.Str(yamlx.Get(n, "artifact")), sc, path),
				Paths: []string{ps.fromAZ(yamlx.Str(v), sc, false, path)},
			}
			return ps.artifactWhen(st), ""
		}
	}
	if v := yamlx.Get(n, "download"); v != nil {
		switch yamlx.Str(v) {
		case "none":
			return nil, "download"
		case "current":
		default:
			d.Warnf(path, "downloading from pipeline resource %q is not portable and was dropped", yamlx.Str(v))
			return nil, "download"
		}
		if yamlx.Has(n, "patterns") {
			d.Infof(path+".patterns", "download patterns were dropped; the whole artifact is downloaded")
		}
		st.Kind = ir.StepDownload
		st.Artifact = &ir.Artifact{Name: ps.artifactName(yamlx.Str(yamlx.Get(n, "artifact")), sc, path)}
		return st, "download"
	}
	if task := yamlx.Str(yamlx.Get(n, "task")); task != "" {
		return ps.parseTask(st, task, yamlx.Get(n, "inputs"), sc, path)
	}
	for _, key := range []string{"downloadBuild", "getPackage", "reviewApp"} {
		if yamlx.Has(n, key) {
			d.Warnf(path, "%q steps are not portable and were dropped", key)
			return nil, ""
		}
	}
	d.Warnf(path, "unknown step type was dropped")
	return nil, ""
}

// artifactName converts an artifact name. Variables defined nowhere in
// the file are matrix values of the producing job (a download in another
// job cannot see them), never secrets.
func (ps *parser) artifactName(name string, sc *scope, path string) string {
	name = macroRe.ReplaceAllStringFunc(name, func(m string) string {
		v := macroRe.FindStringSubmatch(m)[1]
		_, ci := macroToCanon(v)
		_, known := sc.vars[strings.ToLower(v)]
		_, mx := sc.matrix[strings.ToLower(v)]
		if ci || known || mx || isPredefined(v) || !identRe.MatchString(v) {
			return m
		}
		return ir.Ref(ir.RefMatrix, v)
	})
	return ps.fromAZ(name, sc, false, path)
}

// artifactWhen moves an always()/failed() condition of an upload step into
// the artifact's When, the portable form.
func (ps *parser) artifactWhen(st *ir.Step) *ir.Step {
	if c := st.If; c != nil && c.When != "" && len(c.Branches) == 0 && c.Event == "" && c.Raw == "" && !c.Manual {
		st.Artifact.When = c.When
		st.If = nil
	}
	return st
}

// parseTask maps well-known tasks to portable steps; others are kept as
// Azure-specific actions.
func (ps *parser) parseTask(st *ir.Step, task string, inputs *yaml.Node, sc *scope, path string) (*ir.Step, string) {
	// Input names are case-insensitive; aliases are tried in order.
	raw := func(keys ...string) string {
		for _, k := range keys {
			for _, pair := range yamlx.Pairs(inputs) {
				if strings.EqualFold(pair.Key, k) {
					return yamlx.Str(pair.Value)
				}
			}
		}
		return ""
	}
	in := func(keys ...string) string {
		return ps.fromAZ(raw(keys...), sc, false, path+".inputs")
	}
	tool := func(name, version string) (*ir.Step, string) {
		st.Kind = ir.StepTool
		st.Tool = name
		st.Version = version
		return st, ""
	}
	script := func(shell, body string) (*ir.Step, string) {
		st.Kind = ir.StepRun
		st.Shell = shell
		st.Run = ps.fromAZ(strings.TrimRight(body, "\n"), sc, true, path+".inputs")
		st.WorkingDir = in("workingDirectory")
		return st, ""
	}
	// fileScript runs a script file for the task's filePath target type.
	fileScript := func(shell, runner string) (*ir.Step, string) {
		cmd := strings.TrimSpace(runner + " " + raw("filePath") + " " + raw("arguments"))
		return script(shell, cmd)
	}
	switch strings.ToLower(task) {
	case "nodetool@0":
		return tool(ir.ToolNode, in("versionSpec"))
	case "usenode@1":
		return tool(ir.ToolNode, in("version"))
	case "usepythonversion@0":
		return tool(ir.ToolPython, in("versionSpec"))
	case "gotool@0":
		return tool(ir.ToolGo, in("version"))
	case "javatoolinstaller@0":
		return tool(ir.ToolJava, in("versionSpec"))
	case "userubyversion@0":
		return tool(ir.ToolRuby, in("versionSpec"))
	case "usedotnet@2":
		return tool(ir.ToolDotnet, in("version"))
	case "cache@2", "cache@1", "cachebeta@1", "cachebeta@0":
		st.Kind = ir.StepCache
		st.Cache = parseCacheKey(in("key"))
		if p := in("path"); p != "" {
			st.Cache.Paths = []string{p}
		}
		return st, ""
	case "publishpipelineartifact@1", "publishpipelineartifact@0":
		st.Kind = ir.StepUpload
		st.Artifact = &ir.Artifact{Name: ps.artifactName(raw("artifact", "artifactName"), sc, path), Paths: []string{in("targetPath", "path")}}
		if strings.EqualFold(in("publishLocation", "artifactType"), "filepath") {
			ps.d.Warnf(path, "publishing to a file share is not portable; the artifact is published to the run instead")
		}
		return ps.artifactWhen(st), ""
	case "publishbuildartifacts@1":
		st.Kind = ir.StepUpload
		name := ps.artifactName(raw("ArtifactName"), sc, path)
		if name == "" {
			name = "drop"
		}
		p := in("PathtoPublish")
		if p == "" {
			p = ps.fromAZ("$(Build.ArtifactStagingDirectory)", sc, false, path)
		}
		st.Artifact = &ir.Artifact{Name: name, Paths: []string{p}}
		return ps.artifactWhen(st), ""
	case "downloadpipelineartifact@2", "downloadpipelineartifact@1", "downloadbuildartifacts@0", "downloadbuildartifacts@1":
		if strings.EqualFold(in("buildType", "source"), "specific") {
			ps.d.Warnf(path, "downloading artifacts of another pipeline is not portable and was dropped")
			return nil, "download"
		}
		st.Kind = ir.StepDownload
		st.Artifact = &ir.Artifact{Name: ps.artifactName(raw("artifact", "artifactName"), sc, path)}
		return st, "download"
	case "cmdline@2", "cmdline@1":
		return script("", raw("script"))
	case "bash@3":
		if strings.EqualFold(in("targetType"), "inline") {
			return script("bash", raw("script"))
		}
		return fileScript("bash", "bash")
	case "powershell@2":
		shell := "powershell"
		if v := strings.ToLower(in("pwsh")); v == "true" {
			shell = "pwsh"
		}
		if strings.EqualFold(in("targetType"), "inline") {
			return script(shell, raw("script"))
		}
		return fileScript(shell, "&")
	}
	st.Kind = ir.StepAction
	st.Uses = task
	st.UsesFrom = "azure"
	for _, pair := range yamlx.Pairs(inputs) {
		st.With = append(st.With, ir.KV{Key: pair.Key, Value: ps.fromAZ(yamlx.Str(pair.Value), sc, false, path+".inputs."+pair.Key)})
	}
	ps.d.Warnf(path, "task %s has no portable equivalent; it only converts back to Azure Pipelines", task)
	return st, ""
}

// parseCacheKey extracts the prefix and lockfiles from a Cache@2 key such
// as 'npm | "$(Agent.OS)" | package-lock.json'. Quoted segments and
// variables are dropped; segments that look like files are hashed files.
func parseCacheKey(key string) *ir.Cache {
	c := &ir.Cache{}
	prefix := ""
	for _, seg := range strings.Split(key, "|") {
		seg = strings.TrimSpace(seg)
		if seg == "" || strings.HasPrefix(seg, `"`) || strings.Contains(seg, "${{") || strings.Contains(seg, "$(") {
			continue
		}
		if strings.ContainsAny(seg, ".*/") {
			for _, f := range strings.Split(seg, ",") {
				f = strings.TrimSpace(f)
				if f != "" && !strings.HasPrefix(f, "!") {
					c.Files = append(c.Files, strings.TrimPrefix(f, "**/"))
				}
			}
			continue
		}
		if prefix == "" {
			prefix = seg
		}
	}
	c.Prefix = "deps"
	if prefix != "" {
		c.Prefix = ir.Slug(prefix)
	}
	return c
}

// promoteShellVars rewrites references to exported pipeline variables
// ($PYTHON_VERSION for python.version) as canonical references, promotes
// shell variables defined nowhere to secrets and reports every promoted
// name in a single note.
func (ps *parser) promoteShellVars(p *ir.Pipeline) {
	before := map[string]bool{}
	for _, s := range p.Secrets() {
		before[s] = true
	}
	var scratch ir.Diagnostics
	ir.PromoteExternalVars(p, &scratch, builtinEnvPrefixes...)
	for _, s := range p.Secrets() {
		if !before[s] {
			ps.promoted[s] = true
		}
	}
	if len(ps.promoted) > 0 {
		ps.d.Infof("", "variables defined outside the file were treated as secrets: %s", strings.Join(ir.SortedKeys(ps.promoted), ", "))
	}
}
