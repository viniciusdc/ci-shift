package azure

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

var (
	// branchFilterRe is the pattern Azure accepts for branch/tag filters.
	branchFilterRe = regexp.MustCompile(`^[^/~^: \[\]\\]+(/[^/~^: \[\]\\]+)*$`)
	versionRe      = regexp.MustCompile(`^\d+(\.\d+)?$`)
)

// Emit implements ir.Provider.
func (Provider) Emit(p *ir.Pipeline, d *ir.Diagnostics) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p = promoteForEmit(p, d)
	e := &emitter{p: p, d: d}
	ids := map[string]string{}
	taken := map[string]bool{}
	for _, j := range p.Jobs {
		base := j.ID
		if !identRe.MatchString(base) {
			base = ir.IdentSlug(base)
		}
		if isDeployment(j) && strings.EqualFold(base, "deploy") {
			base += "_job" // "deploy" is a reserved deployment name
		}
		ids[j.ID] = uniqueIdent(base, taken)
	}
	e.ids = ids
	e.collectServices()

	root := yamlx.NewMap()
	e.triggers(root)
	vars := yamlx.NewMap()
	for _, kv := range p.Env {
		if len(ir.Refs(kv.Value, ir.RefSecret)) > 0 {
			e.secretEnv = append(e.secretEnv, kv)
			continue
		}
		vars.Set(kv.Key, e.toAZ(kv.Value, scriptNone, "variables."+kv.Key))
	}
	if len(e.secretEnv) > 0 {
		d.Infof("variables", "variables holding secrets were moved to the env of each script step")
	}
	if vars.Len() > 0 {
		root.Set("variables", vars)
	}
	if len(e.resources) > 0 {
		root.Set("resources", yamlx.NewMap().Set("containers", e.resources))
	}

	useStages := len(p.Jobs) > 0
	for _, j := range p.Jobs {
		if j.Stage == "" {
			useStages = false
		}
	}
	if useStages {
		stages, err := e.stages()
		if err != nil {
			return nil, err
		}
		root.Set("stages", stages)
	} else {
		var jobs []any
		for _, j := range p.Jobs {
			jobs = append(jobs, e.job(j, j.Needs, "jobs."+ids[j.ID]))
		}
		root.Set("jobs", jobs)
	}
	if p.Name != "" {
		// Azure's name: key is the run number format, not the pipeline name.
		d.Infof("name", "Azure Pipelines names pipelines in the UI; the name %q was dropped", p.Name)
	}
	return yamlx.Marshal(root, "")
}

// promoteForEmit returns a copy of p in which shell variables defined
// nowhere in the pipeline are secret references, exactly as the parser
// reads them back. Azure does not export secrets to scripts, so such
// variables must be mapped through each step's env to be readable, and
// doing it here keeps emit(parse(output)) == output.
func promoteForEmit(p *ir.Pipeline, d *ir.Diagnostics) *ir.Pipeline {
	cp := *p
	cp.Env = append([]ir.KV(nil), p.Env...)
	cp.Jobs = make([]*ir.Job, len(p.Jobs))
	for i, j := range p.Jobs {
		jc := *j // Services stay shared: only text fields are rewritten
		jc.Env = append([]ir.KV(nil), j.Env...)
		jc.Steps = make([]*ir.Step, len(j.Steps))
		for k, st := range j.Steps {
			sc := *st
			sc.Env = append([]ir.KV(nil), st.Env...)
			jc.Steps[k] = &sc
		}
		cp.Jobs[i] = &jc
	}
	before := map[string]bool{}
	for _, s := range p.Secrets() {
		before[s] = true
	}
	var scratch ir.Diagnostics
	ir.PromoteExternalVars(&cp, &scratch, builtinEnvPrefixes...)
	var added []string
	for _, s := range cp.Secrets() {
		if !before[s] {
			added = append(added, s)
		}
	}
	if len(added) > 0 {
		d.Infof("", "variables defined outside the file are mapped into script steps as secrets: %s", strings.Join(added, ", "))
	}
	return &cp
}

// uniqueIdent returns base, or base_2, base_3... if taken.
func uniqueIdent(base string, taken map[string]bool) string {
	id := base
	for i := 2; taken[strings.ToLower(id)]; i++ {
		id = fmt.Sprintf("%s_%d", base, i)
	}
	taken[strings.ToLower(id)] = true
	return id
}

// isDeployment reports whether a job is emitted as a deployment job, the
// only kind of job that can target an environment.
func isDeployment(j *ir.Job) bool { return j.Environment != "" && j.Matrix == nil }

// emitter holds state shared while rendering one pipeline.
type emitter struct {
	p         *ir.Pipeline
	d         *ir.Diagnostics
	ids       map[string]string
	secretEnv []ir.KV // pipeline variables holding secrets
	resources []any   // resources.containers entries
	svcAlias  map[*ir.Service]string
}

// collectServices assigns every service container a resource alias,
// sharing one resource between identical services.
func (e *emitter) collectServices() {
	e.svcAlias = map[*ir.Service]string{}
	bySig := map[string]string{}
	taken := map[string]bool{}
	for _, j := range e.p.Jobs {
		taken[strings.ToLower(j.Image)] = true // keep aliases distinct from images
	}
	for _, j := range e.p.Jobs {
		path := "jobs." + e.ids[j.ID] + ".services"
		if len(j.Services) > 0 && !j.Runner.IsLinux() {
			e.d.Warnf(path, "service containers need a Linux agent")
		}
		for i := range j.Services {
			s := &j.Services[i]
			sig := s.Image
			for _, kv := range s.Env {
				sig += "\x00" + kv.Key + "=" + kv.Value
			}
			sig += "\x00" + strings.Join(s.Ports, ",")
			if alias, ok := bySig[sig]; ok {
				e.svcAlias[s] = alias
				continue
			}
			alias := ir.Slug(s.Name)
			for n := 2; taken[strings.ToLower(alias)]; n++ {
				alias = fmt.Sprintf("%s-%d", ir.Slug(s.Name), n)
			}
			taken[strings.ToLower(alias)] = true
			bySig[sig] = alias
			e.svcAlias[s] = alias
			if len(ir.Refs(s.Image, ir.RefMatrix)) > 0 {
				e.d.Warnf(path, "service image %q uses matrix values, which container resources cannot read", s.Image)
			}
			res := yamlx.NewMap().Set("container", alias).Set("image", e.toAZ(s.Image, scriptNone, path))
			if len(s.Env) > 0 {
				env := yamlx.NewMap()
				for _, kv := range s.Env {
					env.Set(kv.Key, e.toAZ(kv.Value, scriptNone, path))
				}
				res.Set("env", env)
			}
			res.SetIf("ports", s.Ports)
			e.resources = append(e.resources, res)
		}
	}
}

// ---------------------------------------------------------------------------
// Triggers

// branchFilters converts glob patterns into Azure filters, dropping the
// ones Azure cannot express.
func (e *emitter) branchFilters(pats []string, path string) []string {
	var out []string
	for _, p := range pats {
		p = strings.ReplaceAll(p, "**", "*")
		if !branchFilterRe.MatchString(p) {
			e.d.Warnf(path, "filter %q is not a valid Azure branch filter and was dropped", p)
			continue
		}
		out = append(out, p)
	}
	return out
}

func includeExclude(include, exclude []string) *yamlx.Map {
	return yamlx.NewMap().SetIf("include", include).SetIf("exclude", exclude)
}

func (e *emitter) triggers(root *yamlx.Map) {
	t := e.p.Triggers
	t.Schedules = t.CronSchedules(e.d, "schedules")
	if t.Empty() {
		return // Azure's defaults: CI on every branch and pull requests
	}
	if push := t.Push; push == nil {
		root.Set("trigger", "none")
	} else {
		branches := e.branchFilters(push.Branches, "trigger.branches")
		ignore := e.branchFilters(push.BranchesIgnore, "trigger.branches")
		tags := e.branchFilters(push.Tags, "trigger.tags")
		if len(ignore) == 0 && len(tags) == 0 && len(push.Paths) == 0 && len(push.PathsIgnore) == 0 {
			if len(branches) > 0 {
				root.Set("trigger", branches)
			}
		} else {
			if len(tags) > 0 && len(branches) == 0 && len(ignore) == 0 {
				ignore = []string{"*"} // tag-only CI
			}
			tm := yamlx.NewMap()
			if b := includeExclude(branches, ignore); b.Len() > 0 {
				tm.Set("branches", b)
			}
			if len(tags) > 0 {
				tm.Set("tags", includeExclude(tags, nil))
			}
			if pm := includeExclude(push.Paths, push.PathsIgnore); pm.Len() > 0 {
				tm.Set("paths", pm)
			}
			root.Set("trigger", tm)
		}
	}
	if pr := t.PullRequest; pr == nil {
		root.Set("pr", "none")
	} else {
		branches := e.branchFilters(pr.Branches, "pr.branches")
		switch {
		case len(pr.Paths) > 0:
			pm := yamlx.NewMap()
			if len(branches) > 0 {
				pm.Set("branches", includeExclude(branches, nil))
			}
			pm.Set("paths", includeExclude(pr.Paths, nil))
			root.Set("pr", pm)
		case len(branches) > 0:
			root.Set("pr", branches)
		}
		e.d.Infof("pr", "pr triggers apply to GitHub and Bitbucket repositories; Azure Repos validate pull requests with branch policies")
	}
	if len(t.Schedules) > 0 {
		var items []any
		for i, s := range t.Schedules {
			where := fmt.Sprintf("schedules[%d]", i)
			branches := e.branchFilters(s.Branches, where)
			if len(branches) == 0 {
				branches = []string{"main"}
				e.d.Warnf(where, "Azure schedules need a branch filter; assuming the default branch is \"main\"")
			}
			items = append(items, yamlx.NewMap().
				Set("cron", s.Cron).
				Set("branches", includeExclude(branches, nil)).
				Set("always", "true")) // other CIs run schedules unconditionally
		}
		root.Set("schedules", items)
	}
	if t.Manual {
		e.d.Infof("trigger", "manual runs are always available in Azure Pipelines")
	}
}

// ---------------------------------------------------------------------------
// Structure

func (e *emitter) stages() ([]any, error) {
	names, assign, err := e.p.StageNames()
	if err != nil {
		return nil, err
	}
	stageIDs := map[string]string{}
	taken := map[string]bool{}
	for _, s := range names {
		base := s
		if !identRe.MatchString(base) {
			base = ir.IdentSlug(base)
		}
		stageIDs[s] = uniqueIdent(base, taken)
	}
	var out []any
	for _, s := range names {
		sm := yamlx.NewMap().Set("stage", stageIDs[s])
		if stageIDs[s] != s {
			sm.Set("displayName", s)
		}
		deps := []string{}
		seen := map[string]bool{}
		var jobs []any
		for _, j := range e.p.Jobs {
			if assign[j.ID] != s {
				continue
			}
			var local []string
			for _, n := range j.Needs {
				if other := assign[n]; other != s {
					if !seen[other] {
						seen[other] = true
						deps = append(deps, stageIDs[other])
					}
					continue
				}
				local = append(local, n)
			}
			jobs = append(jobs, e.job(j, local, "stages."+stageIDs[s]+".jobs."+e.ids[j.ID]))
		}
		// An absent dependsOn means "the previous stage": always be explicit.
		sm.Set("dependsOn", deps)
		sm.Set("jobs", jobs)
		out = append(out, sm)
	}
	return out, nil
}

func (e *emitter) job(j *ir.Job, needs []string, path string) *yamlx.Map {
	d := e.d
	m := yamlx.NewMap()
	deployment := isDeployment(j)
	if deployment {
		m.Set("deployment", e.ids[j.ID])
	} else {
		m.Set("job", e.ids[j.ID])
		if j.Environment != "" {
			d.Warnf(path, "matrix jobs cannot be deployment jobs; environment %q was dropped", j.Environment)
		}
	}
	if j.Name != "" && j.Name != e.ids[j.ID] {
		m.Set("displayName", e.toAZ(j.Name, scriptNone, path))
	}
	var deps []string
	for _, n := range needs {
		deps = append(deps, e.ids[n])
	}
	if len(deps) == 1 {
		m.Set("dependsOn", deps[0])
	} else {
		m.SetIf("dependsOn", deps)
	}
	if cond := e.condition(j.If, path); cond != "" {
		m.Set("condition", cond)
	}
	if j.If != nil && j.If.Manual {
		if deployment {
			d.Warnf(path, "manual approval gates map to approval checks; add one to environment %q", j.Environment)
		} else {
			d.Warnf(path, "manual approval gates map to Azure environments with approval checks; give the job an environment and configure one")
		}
	}
	if j.AllowFailure {
		m.Set("continueOnError", "true")
	}
	if j.TimeoutMinutes > 0 {
		m.Set("timeoutInMinutes", strconv.Itoa(j.TimeoutMinutes))
	}
	m.Set("pool", e.pool(j.Runner, path+".pool"))
	if j.Image != "" {
		if j.Runner.IsLinux() {
			img := e.toAZ(j.Image, scriptNone, path+".container")
			if strings.Contains(j.Image, "${{") {
				img = runtimeFormat(j.Image) // macros are not expanded here
			}
			m.Set("container", img)
		} else {
			d.Warnf(path+".container", "container jobs need a Linux agent; image %q was dropped", j.Image)
		}
	}
	if len(j.Services) > 0 {
		svcs := yamlx.NewMap()
		for i := range j.Services {
			s := &j.Services[i]
			svcs.Set(ir.Slug(s.Name), e.svcAlias[s])
		}
		m.Set("services", svcs)
		if j.Image == "" {
			d.Infof(path+".services", "services run next to the agent; reach them via localhost and mapped ports")
		}
	}
	vars := yamlx.NewMap()
	secretEnv := append([]ir.KV(nil), e.secretEnv...)
	for _, kv := range j.Env {
		if len(ir.Refs(kv.Value, ir.RefSecret)) > 0 {
			secretEnv = ir.KVSet(secretEnv, kv.Key, kv.Value)
			d.Infof(path+".variables", "variable %s holds a secret and was moved to the env of each script step", kv.Key)
			continue
		}
		vars.Set(kv.Key, e.toAZ(kv.Value, scriptNone, path+".variables."+kv.Key))
	}
	if vars.Len() > 0 {
		m.Set("variables", vars)
	}
	if j.Matrix != nil {
		if legs := e.matrix(j, path+".strategy.matrix"); legs.Len() > 0 {
			m.Set("strategy", yamlx.NewMap().Set("matrix", legs))
		}
	}

	var steps []any
	hasCheckout := false
	for _, st := range j.Steps {
		if st.Kind == ir.StepCheckout {
			hasCheckout = true
		}
	}
	switch {
	case deployment:
		// Deployment jobs download every artifact unless told otherwise.
		steps = append(steps, yamlx.NewMap().Set("download", "none"))
	case !hasCheckout:
		steps = append(steps, yamlx.NewMap().Set("checkout", "none"))
	}
	uploads := false
	for i, st := range j.Steps {
		if st.Kind == ir.StepUpload && len(ir.Refs(artifactName(st.Artifact, j), ir.RefMatrix)) == 0 {
			uploads = true
		}
		steps = append(steps, e.step(j, st, secretEnv, fmt.Sprintf("%s.steps[%d]", path, i))...)
	}
	if uploads && j.Matrix != nil {
		d.Warnf(path, "every matrix leg publishes the same artifact name; Azure requires unique names per run")
	}
	if len(steps) == 1 && len(j.Steps) == 0 {
		// Only the checkout/download marker: Azure needs a real step.
		steps = append(steps, yamlx.NewMap().Set("script", "echo 'nothing to do'"))
	}
	if deployment {
		m.Set("environment", j.Environment)
		m.Set("strategy", yamlx.NewMap().Set("runOnce",
			yamlx.NewMap().Set("deploy", yamlx.NewMap().Set("steps", steps))))
	} else {
		m.Set("steps", steps)
	}
	return m
}

func (e *emitter) pool(r ir.Runner, path string) *yamlx.Map {
	m := yamlx.NewMap()
	if len(r.Labels) > 0 {
		m.Set("name", r.Labels[0])
		demands := append([]string(nil), r.Labels[1:]...)
		switch r.OS {
		case ir.OSWindows:
			demands = append(demands, "Agent.OS -equals Windows_NT")
		case ir.OSMacOS:
			demands = append(demands, "Agent.OS -equals Darwin")
		}
		m.SetIf("demands", demands)
		e.d.Infof(path, "runner labels were mapped to agent pool %q; other labels became demands", r.Labels[0])
		return m
	}
	if img := r.VMImage; img != "" {
		if strings.Contains(img, "${{") {
			return m.Set("vmImage", e.toAZ(img, scriptNone, path))
		}
		l := strings.ToLower(img)
		if strings.HasPrefix(l, "macos-") || strings.HasPrefix(l, "ubuntu-") || strings.HasPrefix(l, "windows-") {
			return m.Set("vmImage", hostedImage(img))
		}
		e.d.Infof(path, "runner image %q is not a Microsoft-hosted image; using the OS default", img)
	}
	switch r.OS {
	case ir.OSWindows:
		return m.Set("vmImage", "windows-latest")
	case ir.OSMacOS:
		return m.Set("vmImage", "macOS-latest")
	}
	return m.Set("vmImage", "ubuntu-latest")
}

// hostedImage spells macOS images the way Azure documents them.
func hostedImage(img string) string {
	if strings.HasPrefix(strings.ToLower(img), "macos-") {
		return "macOS-" + img[len("macos-"):]
	}
	return img
}

// matrix renders explicit legs named after their values.
func (e *emitter) matrix(j *ir.Job, path string) *yamlx.Map {
	mx := j.Matrix
	// Matrix values that select the VM image are normalized like images.
	imageVars := map[string]bool{}
	for _, v := range ir.Refs(j.Runner.VMImage, ir.RefMatrix) {
		imageVars[v] = true
	}
	legs := yamlx.NewMap()
	taken := map[string]bool{}
	for _, combo := range mx.Combos() {
		var vals []string
		leg := yamlx.NewMap()
		for _, kv := range combo {
			vals = append(vals, kv.Value)
			val := e.toAZ(kv.Value, scriptNone, path)
			if imageVars[kv.Key] {
				val = hostedImage(val)
			}
			leg.Set(kv.Key, val)
		}
		name := strings.Join(vals, "_")
		if len(combo) > 0 && (name == "" || name[0] >= '0' && name[0] <= '9') {
			name = combo[0].Key + "_" + name // node_18 rather than job_18
		}
		legs.Set(uniqueIdent(ir.IdentSlug(name), taken), leg)
	}
	if legs.Len() == 0 {
		e.d.Warnf(path, "matrix has no combinations and was dropped")
	}
	return legs
}

// ---------------------------------------------------------------------------
// Steps

// tail appends the settings shared by every step. Tasks take typed values;
// shortcut steps (script, checkout, publish...) take strings.
func (e *emitter) tail(m *yamlx.Map, st *ir.Step, cond string, task bool, env *yamlx.Map) {
	if cond != "" {
		m.Set("condition", cond)
	}
	if st.AllowFailure {
		if task {
			m.Set("continueOnError", true)
		} else {
			m.Set("continueOnError", "true")
		}
	}
	if st.TimeoutMinutes > 0 {
		if task {
			m.Set("timeoutInMinutes", st.TimeoutMinutes)
		} else {
			m.Set("timeoutInMinutes", strconv.Itoa(st.TimeoutMinutes))
		}
	}
	if env != nil && env.Len() > 0 {
		m.Set("env", env)
	}
}

func (e *emitter) stepEnv(st *ir.Step, path string) *yamlx.Map {
	env := yamlx.NewMap()
	for _, kv := range st.Env {
		env.Set(kv.Key, e.toAZ(kv.Value, scriptNone, path+".env."+kv.Key))
	}
	return env
}

func (e *emitter) step(j *ir.Job, st *ir.Step, secretEnv []ir.KV, path string) []any {
	d := e.d
	cond := e.condition(st.If, path)
	if st.If != nil && st.If.Manual {
		d.Warnf(path, "manual steps are not supported by Azure Pipelines; the step runs automatically")
	}
	name := e.toAZ(st.Name, scriptNone, path)
	env := e.stepEnv(st, path)
	switch st.Kind {
	case ir.StepCheckout:
		m := yamlx.NewMap().Set("checkout", "self").SetIf("displayName", name)
		if c := st.Checkout; c != nil {
			switch {
			case c.Depth < 0:
				m.Set("fetchDepth", "0")
			case c.Depth > 0:
				m.Set("fetchDepth", strconv.Itoa(c.Depth))
			}
			if c.Submodules {
				m.Set("submodules", "recursive")
			}
			if c.LFS {
				m.Set("lfs", "true")
			}
		}
		e.tail(m, st, cond, false, env)
		return []any{m}
	case ir.StepTool:
		task, inputs := toolTask(st.Tool, e.toAZ(st.Version, scriptNone, path))
		if task == "" {
			d.Warnf(path, "no Azure task known for tool %q; the step was dropped", st.Tool)
			return nil
		}
		m := yamlx.NewMap().Set("task", task).SetIf("displayName", name)
		if inputs.Len() > 0 {
			m.Set("inputs", inputs)
		}
		e.tail(m, st, cond, true, env)
		return []any{m}
	case ir.StepCache:
		return e.cache(st, name, cond, env, path)
	case ir.StepUpload:
		return e.upload(j, st, name, cond, env, path)
	case ir.StepDownload:
		return e.download(j, st, name, cond, env, path)
	case ir.StepAction:
		if st.UsesFrom == "azure" {
			m := yamlx.NewMap().Set("task", st.Uses).SetIf("displayName", name)
			if len(st.With) > 0 {
				inputs := yamlx.NewMap()
				for _, kv := range st.With {
					inputs.Set(kv.Key, taskInput(e.toAZ(kv.Value, scriptNone, path)))
				}
				m.Set("inputs", inputs)
			}
			e.tail(m, st, cond, true, env)
			return []any{m}
		}
		d.Warnf(path, "%s step %q has no Azure Pipelines equivalent; a failing placeholder step was emitted", st.UsesFrom, st.Uses)
		m := yamlx.NewMap().
			Set("script", fmt.Sprintf("echo \"##vso[task.logissue type=error]ci-shift: port %s step '%s' manually\" && exit 1", st.UsesFrom, st.Uses)).
			SetIf("displayName", name)
		e.tail(m, st, cond, false, nil)
		return []any{m}
	}
	return []any{e.run(j, st, name, cond, env, secretEnv, path)}
}

// taskInput keeps boolean inputs typed, as task schemas expect.
func taskInput(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}

func (e *emitter) run(j *ir.Job, st *ir.Step, name, cond string, env *yamlx.Map, secretEnv []ir.KV, path string) *yamlx.Map {
	key, kind := "script", scriptPOSIX
	if !j.Runner.IsLinux() && j.Runner.OS == ir.OSWindows {
		kind = scriptCmd // script runs cmd.exe on Windows
	}
	switch st.Shell {
	case "", "sh":
	case "bash":
		key, kind = "bash", scriptPOSIX
	case "pwsh":
		key, kind = "pwsh", scriptPowerShell
	case "powershell":
		key, kind = "powershell", scriptPowerShell
	case "cmd":
		kind = scriptCmd
		if j.Runner.OS != ir.OSWindows {
			e.d.Warnf(path, "cmd scripts need a Windows agent")
		}
	default:
		e.d.Warnf(path, "shell %q is not supported by Azure Pipelines; the script runs in the default shell", st.Shell)
	}
	body := st.Run
	if kind == scriptPOSIX {
		body = e.exportedRefs(j, body)
	}
	m := yamlx.NewMap().Set(key, e.toAZ(body, kind, path)).SetIf("displayName", name)
	m.SetIf("workingDirectory", e.toAZ(st.WorkingDir, scriptNone, path))
	// Secrets are not exported to scripts: map the ones the step reads.
	for _, kv := range secretEnv {
		if _, set := env.Get(kv.Key); !set {
			env.Set(kv.Key, e.toAZ(kv.Value, scriptNone, path))
		}
	}
	for _, s := range ir.Refs(st.Run, ir.RefSecret) {
		if _, set := env.Get(s); !set {
			env.Set(s, "$("+s+")")
		}
	}
	e.tail(m, st, cond, false, env)
	return m
}

// exportedRefs rewrites $name shell references to pipeline and matrix
// variables whose exported environment name differs (Azure upper-cases
// names and replaces dots) into $(name) macros.
func (e *emitter) exportedRefs(j *ir.Job, s string) string {
	vars := map[string]bool{}
	for _, kv := range e.p.Env {
		vars[kv.Key] = true
	}
	for _, kv := range j.Env {
		vars[kv.Key] = true
	}
	for _, v := range j.Matrix.Vars() {
		vars[v] = true
	}
	return ir.ReplaceShellVars(s, func(name string) (string, bool) {
		if vars[name] && envName(name) != name {
			return "$(" + name + ")", true
		}
		return "", false
	})
}

func toolTask(tool, version string) (string, *yamlx.Map) {
	in := yamlx.NewMap()
	switch tool {
	case ir.ToolNode:
		return "NodeTool@0", in.SetIf("versionSpec", version)
	case ir.ToolPython:
		return "UsePythonVersion@0", in.SetIf("versionSpec", version)
	case ir.ToolGo:
		return "GoTool@0", in.SetIf("version", version)
	case ir.ToolJava:
		return "JavaToolInstaller@0", in.SetIf("versionSpec", version).
			Set("jdkArchitectureOption", "x64").Set("jdkSourceOption", "PreInstalled")
	case ir.ToolRuby:
		return "UseRubyVersion@0", in.SetIf("versionSpec", version)
	case ir.ToolDotnet:
		if versionRe.MatchString(version) {
			version += ".x"
		}
		return "UseDotNet@2", in.Set("packageType", "sdk").SetIf("version", version)
	}
	return "", nil
}

func (e *emitter) cache(st *ir.Step, name, cond string, env *yamlx.Map, path string) []any {
	c := st.Cache
	if len(c.Paths) == 0 {
		e.d.Warnf(path, "cache without paths was dropped")
		return nil
	}
	prefix := c.Prefix
	if prefix == "" {
		prefix = "deps"
	}
	if len(c.Paths) > 1 {
		e.d.Infof(path, "Cache@2 caches one path; one task per path was emitted")
	}
	var out []any
	for _, p := range c.Paths {
		// Each task needs its own key: fold the path into the prefix.
		segs := []string{prefix, `"$(Agent.OS)"`}
		if len(c.Paths) > 1 {
			segs[0] = ir.Slug(prefix + "-" + p)
		}
		if strings.HasPrefix(p, "~") {
			e.d.Warnf(path, "Cache@2 does not expand ~ in %q; use an absolute path such as $(Pipeline.Workspace)/...", p)
		}
		in := yamlx.NewMap().Set("key", strings.Join(append(append([]string(nil), segs...), c.Files...), " | "))
		if len(c.Files) > 0 {
			in.Set("restoreKeys", strings.Join(segs, " | "))
		}
		in.Set("path", e.toAZ(p, scriptNone, path))
		m := yamlx.NewMap().Set("task", "Cache@2").SetIf("displayName", name).Set("inputs", in)
		e.tail(m, st, cond, true, env)
		out = append(out, m)
	}
	return out
}

func artifactName(a *ir.Artifact, j *ir.Job) string {
	if a.Name != "" {
		return a.Name
	}
	return ir.Slug(j.ID)
}

// artifactRoot returns the single path to publish for an artifact and the
// directory the published files are restored into. ok is false when the
// paths had to be widened to their common directory.
func artifactRoot(paths []string) (publish, restore string, ok bool) {
	if len(paths) == 1 && !strings.ContainsAny(paths[0], "*?[") {
		p := strings.TrimSuffix(strings.TrimPrefix(paths[0], "./"), "/")
		if looksLikeFile(p) {
			return p, path.Dir(p), true
		}
		return p, p, true
	}
	dir := commonDir(paths)
	return dir, dir, false
}

func (e *emitter) upload(j *ir.Job, st *ir.Step, name, cond string, env *yamlx.Map, where string) []any {
	a := st.Artifact
	if len(a.Paths) == 0 {
		e.d.Warnf(where, "artifact without paths was dropped")
		return nil
	}
	publish, _, ok := artifactRoot(a.Paths)
	if !ok {
		e.d.Warnf(where, "Azure publishes a single path; publishing %q, which contains %s", publish, strings.Join(a.Paths, ", "))
	}
	if publish == "." {
		publish = ir.Ref(ir.RefCI, ir.VarWorkspace)
	}
	if a.ExpireIn != "" {
		e.d.Infof(where, "artifact retention (%s) is configured in the project settings", a.ExpireIn)
	}
	if cond == "" {
		switch a.When {
		case ir.WhenAlways:
			cond = "always()"
		case ir.WhenOnFailure:
			cond = "failed()"
		}
	}
	m := yamlx.NewMap().
		Set("publish", e.toAZ(publish, scriptNone, where)).
		Set("artifact", e.toAZ(artifactName(a, j), scriptNone, where)).
		SetIf("displayName", name)
	e.tail(m, st, cond, false, env)
	return []any{m}
}

// download restores artifacts to the paths they were published from.
// The download shortcut always extracts into $(Pipeline.Workspace), so
// artifacts of known upstream jobs use the task with an explicit target.
func (e *emitter) download(j *ir.Job, st *ir.Step, name, cond string, env *yamlx.Map, where string) []any {
	// Names are compared as rendered: ${{ env.X }} and ${{ matrix.X }}
	// are both $(X) in Azure.
	plain := func(s string) string {
		return ir.RewriteRefs(s, func(_, n string) string { return "$(" + n + ")" })
	}
	var out []any
	seen := map[string]bool{}
	for _, dep := range upstream(e.p, j) {
		for _, s := range dep.Steps {
			if s.Kind != ir.StepUpload || len(s.Artifact.Paths) == 0 {
				continue
			}
			art := artifactName(s.Artifact, dep)
			if st.Artifact.Name != "" && plain(st.Artifact.Name) != plain(art) || seen[plain(art)] {
				continue
			}
			seen[plain(art)] = true
			_, dir, _ := artifactRoot(s.Artifact.Paths)
			target := ir.Ref(ir.RefCI, ir.VarWorkspace)
			switch {
			case strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "$"):
				target = dir // already absolute
			case dir != ".":
				target += "/" + dir
			}
			display := name
			if display == "" {
				display = "Download " + e.toAZ(art, scriptNone, where)
			}
			m := yamlx.NewMap().Set("task", "DownloadPipelineArtifact@2").Set("displayName", display).
				Set("inputs", yamlx.NewMap().
					Set("buildType", "current").
					Set("artifactName", e.toAZ(art, scriptNone, where)).
					Set("targetPath", e.toAZ(target, scriptNone, where)))
			e.tail(m, st, cond, true, env)
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out
	}
	m := yamlx.NewMap().Set("download", "current").SetIf("artifact", e.toAZ(st.Artifact.Name, scriptNone, where)).SetIf("displayName", name)
	e.tail(m, st, cond, false, env)
	e.d.Infof(where, "artifacts are downloaded into $(Pipeline.Workspace); adjust paths that expect them elsewhere")
	return []any{m}
}

// upstream returns every job j transitively needs.
func upstream(p *ir.Pipeline, j *ir.Job) []*ir.Job {
	seen := map[string]bool{}
	var out []*ir.Job
	var walk func(*ir.Job)
	walk = func(x *ir.Job) {
		for _, n := range x.Needs {
			if seen[n] {
				continue
			}
			seen[n] = true
			if dep := p.FindJob(n); dep != nil {
				out = append(out, dep)
				walk(dep)
			}
		}
	}
	walk(j)
	return out
}

// commonDir returns the deepest directory containing every path.
func commonDir(paths []string) string {
	var common []string
	for i, p := range paths {
		p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		if k := strings.IndexAny(p, "*?["); k >= 0 {
			p = path.Dir(p[:k+1])
		} else if looksLikeFile(p) {
			p = path.Dir(p)
		}
		parts := strings.Split(p, "/")
		if p == "." || p == "" {
			parts = nil
		}
		if i == 0 {
			common = parts
			continue
		}
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	if len(common) == 0 {
		return "."
	}
	return strings.Join(common, "/")
}

func looksLikeFile(p string) bool {
	base := path.Base(p)
	return strings.Contains(base, ".") && !strings.HasPrefix(base, ".")
}
