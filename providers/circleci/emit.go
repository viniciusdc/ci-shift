package circleci

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

var (
	jobIDRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	paramNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	dockerCmdRe = regexp.MustCompile(`(?m)(?:^|[\s;&|(])docker(?:-compose)?\s+(?:build|buildx|run|push|pull|compose|login|tag|exec|images|ps)\b`)
)

// Executor defaults used when the IR does not name a CircleCI image.
const (
	defaultImage     = "cimg/base:stable"
	windowsImage     = "windows-server-2022-gui:current"
	windowsClass     = "windows.medium"
	macosXcode       = "15.4.0"
	macosClass       = "macos.m1.medium.gen1"
	scheduledWF      = "scheduled"
	allPattern       = "/.*/"
	defaultSchedules = "main"
)

type emitter struct {
	p       *ir.Pipeline
	d       *ir.Diagnostics
	ids     map[string]string // IR job ID -> CircleCI job name
	holds   map[string]string // IR job ID -> approval job name
	persist map[string]bool   // jobs whose uploads feed a workspace attach
	attach  map[string]bool   // jobs that attach an upstream workspace
	orbs    map[string]bool
	params  map[string]string // current job: matrix var -> parameter name
}

// Emit implements ir.Provider.
func (Provider) Emit(p *ir.Pipeline, d *ir.Diagnostics) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	e := &emitter{
		p: p, d: d, ids: map[string]string{}, holds: map[string]string{},
		persist: map[string]bool{}, attach: map[string]bool{}, orbs: map[string]bool{},
	}
	taken := map[string]bool{}
	for _, j := range p.Jobs {
		id := j.ID
		if !jobIDRe.MatchString(id) {
			id = ir.Slug(id)
		}
		e.ids[j.ID] = ir.UniqueID(id, taken)
	}
	for _, j := range p.Jobs {
		if j.If != nil && j.If.Manual {
			e.holds[j.ID] = ir.UniqueID("hold-"+e.ids[j.ID], taken)
		}
	}
	e.planWorkspaces()
	if len(p.Env) > 0 {
		d.Infof("", "CircleCI has no pipeline-level environment; variables were copied into every job")
	}

	jobs := yamlx.NewMap()
	for _, j := range p.Jobs {
		jm := e.emitJob(j)
		if j.Name != "" && j.Name != e.ids[j.ID] {
			jobs.Set(e.ids[j.ID], yamlx.Comment{Text: j.Name, Value: jm})
		} else {
			jobs.Set(e.ids[j.ID], jm)
		}
	}

	root := yamlx.NewMap()
	root.Set("version", 2.1)
	if len(e.orbs) > 0 {
		orbs := yamlx.NewMap()
		for _, name := range ir.SortedKeys(e.orbs) {
			orbs.Set(name, "circleci/"+name+"@volatile")
		}
		root.Set("orbs", orbs)
		d.Warnf("orbs", "orb references were guessed as circleci/<name>@volatile; pin the real orbs and versions")
	}
	root.Set("jobs", jobs)
	root.Set("workflows", e.workflows())
	if secrets := p.Secrets(); len(secrets) > 0 {
		d.Infof("", "define these secrets as project environment variables or in a context: %s", strings.Join(secrets, ", "))
	}
	return yamlx.Marshal(root, "")
}

// text renders canonical references as CircleCI shell variables. Matrix
// values become job parameters.
func (e *emitter) text(s string) string {
	return ir.RewriteRefs(s, func(kind, name string) string {
		switch kind {
		case ir.RefCI:
			switch name {
			case ir.VarRepository:
				return nativeRepository
			case ir.VarRefName:
				return nativeRefName
			}
			if v, ok := shellVars.Lookup(name); ok {
				return "${" + v + "}"
			}
			v := "CISTELA_" + strings.ToUpper(name)
			e.d.Warnf("", "CircleCI has no built-in variable for ci.%s; define %s in the project settings", name, v)
			return "${" + v + ":-}"
		case ir.RefMatrix:
			if p, ok := e.params[name]; ok {
				return "<< parameters." + p + " >>"
			}
		}
		return "${" + name + "}"
	})
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

func hasKind(j *ir.Job, kind ir.StepKind) bool {
	for _, st := range j.Steps {
		if st.Kind == kind {
			return true
		}
	}
	return false
}

// planWorkspaces decides which uploads must also persist to the workspace:
// only those some downstream job attaches.
func (e *emitter) planWorkspaces() {
	for _, j := range e.p.Jobs {
		if !hasKind(j, ir.StepDownload) {
			continue
		}
		for _, u := range upstream(e.p, j) {
			if hasKind(u, ir.StepUpload) {
				e.persist[u.ID] = true
				e.attach[j.ID] = true
			}
		}
	}
}

func paramName(v string) string {
	if paramNameRe.MatchString(v) {
		return v
	}
	var b strings.Builder
	for _, r := range v {
		if r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if !paramNameRe.MatchString(out) {
		out = "p" + out
	}
	return out
}

func scriptsUseVar(j *ir.Job, name string) bool {
	for _, st := range j.Steps {
		found := false
		ir.ReplaceShellVars(st.Run, func(n string) (string, bool) {
			if n == name {
				found = true
			}
			return "", false
		})
		if found {
			return true
		}
	}
	return false
}

func (e *emitter) emitJob(j *ir.Job) *yamlx.Map {
	id := e.ids[j.ID]
	path := "jobs." + id
	m := yamlx.NewMap()
	e.params = map[string]string{}
	if j.Matrix != nil {
		decl := yamlx.NewMap()
		for _, v := range j.Matrix.Vars() {
			pn := paramName(v)
			e.params[v] = pn
			decl.Set(pn, yamlx.NewMap().Set("type", "string"))
		}
		m.SetIf("parameters", decl)
	}
	steps, docker := e.executor(j, m, path)

	env := yamlx.NewMap()
	for _, kv := range append(append([]ir.KV(nil), e.p.Env...), j.Env...) {
		if v, ok := e.envValue(kv, path+".environment"); ok {
			env.Set(kv.Key, v)
		}
	}
	// Scripts from other providers read matrix values as shell variables.
	for _, v := range j.Matrix.Vars() {
		if _, set := env.Get(v); !set && envNameRe.MatchString(v) && scriptsUseVar(j, v) {
			env.Set(v, "<< parameters."+e.params[v]+" >>")
		}
	}
	m.SetIf("environment", env)
	m.Set("steps", e.emitSteps(j, steps, docker, path))

	if j.TimeoutMinutes > 0 {
		e.d.Warnf(path, "CircleCI has no job timeout; the %d minute limit was dropped (steps fail after 10 minutes without output)", j.TimeoutMinutes)
	}
	if j.AllowFailure {
		e.d.Warnf(path, "CircleCI cannot allow a job to fail; the job's failure fails the workflow")
	}
	if j.Environment != "" {
		e.d.Infof(path, "deployment environment %q has no CircleCI equivalent; use a context to scope its secrets", j.Environment)
	}
	return m
}

// envValue renders an environment value. CircleCI does not expand
// variables in environment values, and secrets are already exported
// under their own name, so NAME: ${{ secret.NAME }} is redundant.
func (e *emitter) envValue(kv ir.KV, where string) (string, bool) {
	if kv.Value == ir.Ref(ir.RefSecret, kv.Key) || kv.Value == ir.Ref(ir.RefEnv, kv.Key) {
		return "", false
	}
	if strings.Contains(kv.Value, "${{") && !strings.Contains(kv.Value, "${{ matrix.") {
		e.d.Infof(where, "CircleCI does not expand variables in environment values; export %s in a run step if it must be computed", kv.Key)
	}
	return e.text(kv.Value), true
}

// isMachineImage reports whether img names a CircleCI machine image.
func isMachineImage(img string) bool {
	if !strings.Contains(img, ":") || strings.Contains(img, "/") {
		return false
	}
	for _, pre := range []string{"ubuntu-", "android", "default"} {
		if strings.HasPrefix(img, pre) {
			return true
		}
	}
	return false
}

// executor writes the job's execution environment and returns the steps
// left to emit and whether the job runs in docker.
func (e *emitter) executor(j *ir.Job, m *yamlx.Map, path string) ([]*ir.Step, bool) {
	r := j.Runner
	switch {
	case len(r.Labels) > 0:
		m.Set("machine", true)
		m.Set("resource_class", r.Labels[0])
		e.d.Warnf(path, "self-hosted runner labels %v became resource class %q; register a CircleCI self-hosted runner class (<namespace>/<name>)", r.Labels, r.Labels[0])
		return e.noContainer(j, path), false
	case r.OS == ir.OSWindows:
		m.Set("machine", yamlx.NewMap().Set("image", windowsImage))
		m.Set("resource_class", windowsClass)
		e.d.Warnf(path, "Windows jobs run on a Windows machine executor (or the circleci/windows orb); review the image, resource class and shell")
		return e.noContainer(j, path), false
	case r.OS == ir.OSMacOS:
		m.Set("macos", yamlx.NewMap().Set("xcode", macosXcode))
		m.Set("resource_class", macosClass)
		e.d.Warnf(path, "choose the Xcode version for the macOS executor (%s was assumed)", macosXcode)
		return e.noContainer(j, path), false
	case isMachineImage(r.VMImage) && j.Image == "" && len(j.Services) == 0:
		m.Set("machine", yamlx.NewMap().Set("image", r.VMImage))
		return e.noContainer(j, path), false
	}
	image, steps := ir.FoldTools(j, e.d, path)
	if image == "" {
		image = defaultImage
	}
	docker := []any{yamlx.NewMap().Set("image", e.text(image))}
	for _, s := range j.Services {
		sm := yamlx.NewMap().Set("image", e.text(s.Image))
		sm.SetIf("name", ir.Slug(s.Name))
		env := yamlx.NewMap()
		for _, kv := range s.Env {
			env.Set(kv.Key, e.text(kv.Value))
		}
		sm.SetIf("environment", env)
		docker = append(docker, sm)
		if len(s.Ports) > 0 {
			e.d.Infof(path+".docker", "service %s shares the job's network; reach it on localhost without port mappings", s.Name)
		}
	}
	m.Set("docker", docker)
	return steps, true
}

// noContainer handles jobs on machine/macOS executors, where there is no
// job container, no sidecar services and tools must be installed by hand.
func (e *emitter) noContainer(j *ir.Job, path string) []*ir.Step {
	if j.Image != "" {
		e.d.Warnf(path, "container image %s cannot be combined with this executor and was dropped", j.Image)
	}
	if len(j.Services) > 0 {
		e.d.Warnf(path, "services need a docker executor; start them with docker run in a step")
	}
	var steps []*ir.Step
	for i, st := range j.Steps {
		if st.Kind == ir.StepTool {
			e.d.Warnf(fmt.Sprintf("%s.steps[%d]", path, i), "install %s %s in a run step (or with its orb)", st.Tool, st.Version)
			continue
		}
		steps = append(steps, st)
	}
	return steps
}

func usesDocker(steps []*ir.Step) bool {
	for _, st := range steps {
		if (st.Kind == ir.StepRun || st.Kind == "") && dockerCmdRe.MatchString(st.Run) {
			return true
		}
	}
	return false
}

func (e *emitter) emitSteps(j *ir.Job, steps []*ir.Step, docker bool, path string) []any {
	var out, saves []any
	remote := docker && usesDocker(steps)
	if remote {
		e.d.Infof(path, "docker commands need setup_remote_docker on the docker executor; it was added")
	}
	for i, st := range steps {
		where := fmt.Sprintf("%s.steps[%d]", path, i)
		switch st.Kind {
		case ir.StepCheckout:
			out = append(out, e.checkout(st, where)...)
			if remote {
				out = append(out, "setup_remote_docker")
				remote = false
			}
		case ir.StepCache:
			restore, save := e.cache(st, where)
			out = append(out, restore)
			saves = append(saves, save)
		case ir.StepUpload:
			out = append(out, e.upload(j, st, where)...)
		case ir.StepDownload:
			if !e.attach[j.ID] {
				e.d.Infof(where, "no upstream job uploads artifacts; the download step was dropped")
				continue
			}
			am := yamlx.NewMap().Set("at", ".")
			am.SetIf("when", e.nonRunWhen(st, where))
			out = append(out, yamlx.NewMap().Set("attach_workspace", am))
		case ir.StepAction:
			out = append(out, e.action(st, where))
		case ir.StepTool:
			e.d.Warnf(where, "install %s %s in a run step (or with its orb)", st.Tool, st.Version)
		default:
			out = append(out, e.run(st, where))
		}
	}
	if remote {
		out = append([]any{"setup_remote_docker"}, out...)
	}
	out = append(out, saves...)
	if len(out) == 0 {
		out = []any{yamlx.NewMap().Set("run", "echo 'nothing to do'")}
	}
	return out
}

func (e *emitter) checkout(st *ir.Step, where string) []any {
	out := []any{"checkout"}
	c := st.Checkout
	if c == nil {
		return out
	}
	if c.Depth != 0 {
		e.d.Infof(where, "checkout depth is not set; CircleCI's default clone is used")
	}
	if c.Submodules {
		out = append(out, yamlx.NewMap().Set("run", yamlx.NewMap().Set("name", "Checkout submodules").Set("command", submoduleCmd)))
	}
	if c.LFS {
		out = append(out, yamlx.NewMap().Set("run", yamlx.NewMap().Set("name", "Fetch LFS objects").Set("command", lfsCmd)))
	}
	if st.If != nil {
		e.d.Warnf(where, "checkout conditions are not supported; the checkout always runs")
	}
	return out
}

func whenOf(w string) string {
	switch w {
	case ir.WhenAlways:
		return "always"
	case ir.WhenOnFailure:
		return "on_fail"
	}
	return ""
}

// nonRunWhen renders a condition on a built-in step, which only supports
// when: always/on_fail.
func (e *emitter) nonRunWhen(st *ir.Step, where string) string {
	if st.If.IsZero() {
		return ""
	}
	c := *st.If
	if len(c.Branches) > 0 || c.Event != "" || c.Manual || c.Raw != "" {
		e.d.Warnf(where, "conditions on %s steps cannot be expressed on CircleCI; the step always runs", st.Kind)
	}
	return whenOf(c.When)
}

func (e *emitter) cache(st *ir.Step, where string) (restore, save any) {
	c := st.Cache
	prefix := c.Prefix
	if prefix == "" {
		prefix = "deps"
	}
	var sums []string
	for _, f := range c.Files {
		if ir.IsGlob(f) {
			e.d.Warnf(where, "CircleCI checksums a single file; replace %q with a concrete path", f)
		}
		sums = append(sums, fmt.Sprintf(`{{ checksum "%s" }}`, e.text(f)))
	}
	key := prefix + "-{{ epoch }}"
	keys := []string{prefix + "-"}
	if len(sums) > 0 {
		key = prefix + "-" + strings.Join(sums, "-")
		keys = []string{key, prefix + "-"}
	}
	when := e.nonRunWhen(st, where)
	rm := yamlx.NewMap()
	rm.SetIf("name", e.text(st.Name))
	rm.Set("keys", keys)
	rm.SetIf("when", when)
	var paths []string
	for _, p := range c.Paths {
		paths = append(paths, e.text(p))
	}
	sm := yamlx.NewMap().Set("key", key).Set("paths", paths)
	sm.SetIf("when", when)
	return yamlx.NewMap().Set("restore_cache", rm), yamlx.NewMap().Set("save_cache", sm)
}

func (e *emitter) upload(j *ir.Job, st *ir.Step, where string) []any {
	a := st.Artifact
	when := whenOf(a.When)
	if w := e.nonRunWhen(st, where); w != "" {
		when = w
	}
	var out []any
	if e.persist[j.ID] {
		var rel []string
		for _, p := range a.Paths {
			p = strings.TrimPrefix(p, "./")
			if path.IsAbs(p) || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "..") {
				e.d.Warnf(where, "workspace paths must be inside the checkout; %q was not persisted", p)
				continue
			}
			rel = append(rel, e.text(p))
		}
		if len(rel) > 0 {
			if j.Matrix != nil {
				e.d.Warnf(where, "every matrix instance persists the same workspace paths, which CircleCI rejects as a conflict; persist per-instance directories")
			}
			pm := yamlx.NewMap().Set("root", ".").Set("paths", rel)
			pm.SetIf("when", when)
			out = append(out, yamlx.NewMap().Set("persist_to_workspace", pm))
		}
	}
	for _, p := range a.Paths {
		sp := p
		if i := strings.IndexAny(p, "*?["); i >= 0 {
			sp = path.Dir(p[:i] + "x")
			e.d.Warnf(where, "store_artifacts takes a file or directory; pattern %q was widened to %q", p, sp)
		}
		am := yamlx.NewMap().Set("path", e.text(sp))
		if a.Name != "" {
			dest := a.Name
			if len(a.Paths) > 1 {
				dest = a.Name + "/" + ir.Slug(p)
			}
			am.Set("destination", dest)
		}
		am.SetIf("when", when)
		out = append(out, yamlx.NewMap().Set("store_artifacts", am))
	}
	if a.ExpireIn != "" {
		e.d.Infof(where, "artifact retention (%s) is configured in the CircleCI plan settings", a.ExpireIn)
	}
	return out
}

func (e *emitter) action(st *ir.Step, where string) any {
	if st.UsesFrom == "circleci" {
		name := st.Uses
		if orb, _, ok := strings.Cut(name, "/"); ok {
			e.orbs[orb] = true
		} else {
			e.d.Warnf(where, "command %q is not defined in the emitted configuration", name)
		}
		if len(st.With) == 0 {
			return name
		}
		w := yamlx.NewMap()
		for _, kv := range st.With {
			w.Set(kv.Key, e.text(kv.Value))
		}
		return yamlx.NewMap().Set(name, w)
	}
	e.d.Warnf(where, "%s step %q has no CircleCI equivalent; a failing placeholder command was emitted", st.UsesFrom, st.Uses)
	cmd := fmt.Sprintf("echo \"ci-shift: port %s step '%s' manually\" && exit 1", st.UsesFrom, st.Uses)
	if st.Name == "" {
		return yamlx.NewMap().Set("run", cmd)
	}
	return yamlx.NewMap().Set("run", yamlx.NewMap().Set("name", e.text(st.Name)).Set("command", cmd))
}

func (e *emitter) shell(s, where string) string {
	switch s {
	case "", "bash":
		return ""
	case "sh":
		return "/bin/sh -e"
	case "pwsh":
		return "pwsh"
	case "powershell":
		return "powershell.exe"
	case "cmd":
		return "cmd.exe"
	case "python":
		return "python3"
	}
	e.d.Infof(where, "shell %q was passed through; make sure it exists on the executor", s)
	return s
}

func (e *emitter) run(st *ir.Step, where string) any {
	cmd := e.text(st.Run)
	when := ""
	if st.If != nil {
		c := *st.If
		when = whenOf(c.When)
		c.When = ""
		if c.Manual {
			e.d.Warnf(where, "manual steps are not supported by CircleCI; the step runs automatically")
			c.Manual = false
		}
		var guards []string
		if c.Event == ir.EventPullRequest {
			guards = append(guards, `[ -n "${CIRCLE_PULL_REQUEST:-}" ]`)
			c.Event = ""
		}
		cond, ok := ir.ShellCondition(&c, "CIRCLE_BRANCH", "CIRCLE_TAG")
		if !ok {
			e.d.Warnf(where, "step condition could not be translated; the step always runs")
		} else if cond != "" {
			guards = append(guards, cond)
		}
		if len(guards) > 0 {
			cmd = fmt.Sprintf("if %s; then\n%s\nfi", strings.Join(guards, " && "), indent(cmd))
		}
	}
	if st.AllowFailure {
		if strings.Contains(cmd, "\n") {
			cmd = "(\n" + indent(cmd) + "\n) || true"
		} else {
			cmd += " || true"
		}
	}
	opts := yamlx.NewMap()
	opts.SetIf("name", e.text(st.Name))
	opts.Set("command", cmd)
	opts.SetIf("shell", e.shell(st.Shell, where))
	opts.SetIf("working_directory", e.text(st.WorkingDir))
	env := yamlx.NewMap()
	for _, kv := range st.Env {
		if v, ok := e.envValue(kv, where); ok {
			env.Set(kv.Key, v)
		}
	}
	opts.SetIf("environment", env)
	opts.SetIf("when", when)
	if st.TimeoutMinutes > 0 {
		opts.Set("no_output_timeout", fmt.Sprintf("%dm", st.TimeoutMinutes))
		e.d.Infof(where, "the step timeout became no_output_timeout, which only limits time without output")
	}
	if opts.Len() == 1 {
		return yamlx.NewMap().Set("run", cmd)
	}
	return yamlx.NewMap().Set("run", opts)
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

// filter is a workflow job filter.
type filter struct {
	only, ignore, tags []string
	raw                string
}

func (f *filter) clone() *filter {
	c := *f
	c.only = append([]string(nil), f.only...)
	c.ignore = append([]string(nil), f.ignore...)
	c.tags = append([]string(nil), f.tags...)
	return &c
}

func oneOrList(s []string) any {
	if len(s) == 1 {
		return s[0]
	}
	return s
}

func (f *filter) node() any {
	if f.raw != "" {
		return f.raw
	}
	m := yamlx.NewMap()
	br := yamlx.NewMap()
	if len(f.only) > 0 {
		br.Set("only", oneOrList(f.only))
	}
	if len(f.ignore) > 0 {
		br.Set("ignore", oneOrList(f.ignore))
	}
	m.SetIf("branches", br)
	if len(f.tags) > 0 {
		m.Set("tags", yamlx.NewMap().Set("only", oneOrList(f.tags)))
	}
	if m.Len() == 0 {
		return nil
	}
	return m
}

func isAny(p string) bool { return p == "*" || p == "**" }

func branchPattern(b string) string {
	if ir.IsGlob(b) {
		return "/" + ir.GlobToRegex(b) + "/"
	}
	return b
}

func tagPattern(t string) string {
	if isAny(t) {
		return allPattern
	}
	return branchPattern(t)
}

// pushFilter translates the push trigger into filters shared by every job.
func (e *emitter) pushFilter() *filter {
	f := &filter{}
	t := e.p.Triggers
	t.Schedules = t.CronSchedules(e.d, "schedules")
	push := t.Push
	if push == nil {
		return f
	}
	anyBranch := false
	for _, b := range push.Branches {
		if isAny(b) {
			anyBranch = true
			continue
		}
		f.only = append(f.only, branchPattern(b))
	}
	if anyBranch {
		f.only = nil
	}
	for _, b := range push.BranchesIgnore {
		f.ignore = append(f.ignore, branchPattern(b))
	}
	if len(f.only) > 0 && len(f.ignore) > 0 {
		e.d.Warnf("workflows", "CircleCI ignores branches.ignore when branches.only is set; ignore filters were dropped")
		f.ignore = nil
	}
	for _, tg := range push.Tags {
		f.tags = append(f.tags, tagPattern(tg))
	}
	if len(f.tags) > 0 && len(push.Branches) == 0 && len(push.BranchesIgnore) == 0 && t.PullRequest == nil {
		// Only tag pushes trigger the pipeline.
		f.ignore = []string{allPattern}
	}
	if len(push.Paths) > 0 || len(push.PathsIgnore) > 0 {
		e.d.Warnf("workflows", "path filters need the circleci/path-filtering orb (dynamic configuration); they were dropped")
	}
	return f
}

// jobFilter combines the shared filters with the job's own condition.
func (e *emitter) jobFilter(j *ir.Job, base *filter) *filter {
	f := base.clone()
	c := j.If
	if c.IsZero() {
		return f
	}
	path := "workflows." + e.ids[j.ID]
	if c.Raw != "" {
		if c.RawFrom == "circleci" && len(c.Branches) == 0 && c.Event == "" {
			return &filter{raw: c.Raw}
		}
		e.d.Warnf(path, "condition %q (from %s) could not be translated and was dropped", c.Raw, c.RawFrom)
	}
	if len(c.Branches) > 0 {
		var only []string
		anyBranch := false
		for _, b := range c.Branches {
			if isAny(b) {
				anyBranch = true
				continue
			}
			only = append(only, branchPattern(b))
		}
		if !anyBranch {
			f.only, f.ignore = only, nil
		}
		// A branch condition keeps the job out of tag builds.
		f.tags = nil
	}
	switch c.Event {
	case ir.EventTag:
		if len(base.tags) > 0 {
			f.tags = base.tags
		} else {
			f.tags = []string{allPattern}
		}
		f.only, f.ignore = nil, []string{allPattern}
	case ir.EventPullRequest:
		e.d.Infof(path, "CircleCI filters cannot select pull request builds; the job runs on every matching build")
	case ir.EventManual:
		e.d.Warnf(path, "CircleCI filters cannot select manually triggered pipelines; the job runs on every build")
	}
	return f
}

// excluded returns jobs matching skip, plus everything that depends on them.
func (e *emitter) excluded(skip func(*ir.Job) bool) map[string]bool {
	out := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, j := range e.p.Jobs {
			if out[j.ID] {
				continue
			}
			hit := skip(j)
			for _, n := range j.Needs {
				hit = hit || out[n]
			}
			if hit {
				out[j.ID] = true
				changed = true
			}
		}
	}
	return out
}

func (e *emitter) workflows() *yamlx.Map {
	t := e.p.Triggers
	t.Schedules = t.CronSchedules(e.d, "schedules")
	wfs := yamlx.NewMap()
	name := "main"
	if e.p.Name != "" {
		name = ir.Slug(e.p.Name)
	}
	if name == scheduledWF || name == "version" {
		name += "-workflow"
	}
	hasSchedules := len(t.Schedules) > 0
	isEvent := func(ev string) func(*ir.Job) bool {
		return func(j *ir.Job) bool { return j.If != nil && j.If.Event == ev }
	}
	scheduleOnly := map[string]bool{}
	if hasSchedules {
		scheduleOnly = e.excluded(isEvent(ir.EventSchedule))
	} else {
		for _, j := range e.p.Jobs {
			if isEvent(ir.EventSchedule)(j) {
				e.d.Warnf("jobs."+e.ids[j.ID], "the job is limited to scheduled runs but the pipeline has no schedule; it runs on every build")
			}
		}
	}

	if t.Push == nil && t.PullRequest != nil {
		e.d.Infof("workflows", "CircleCI builds pushed branches; enable \"Only build pull requests\" in the project settings")
	}
	if pr := t.PullRequest; pr != nil && (len(pr.Branches) > 0 || len(pr.Paths) > 0) {
		e.d.Warnf("workflows", "pull request target branch and path filters are not supported by CircleCI and were dropped")
	}
	if t.PullRequest != nil && t.Push != nil && len(t.Push.Branches) > 0 {
		e.d.Warnf("workflows", "CircleCI builds pull requests as branch builds; the push branch filters also skip pull requests from other branches")
	}
	manualOnly := t.Manual && t.Push == nil && t.PullRequest == nil && !hasSchedules
	if t.Empty() || t.Push != nil || t.PullRequest != nil || manualOnly {
		w := yamlx.NewMap()
		if manualOnly {
			w.Set("when", yamlx.NewMap().Set("equal", []any{"api", "<< pipeline.trigger_source >>"}))
			e.d.Infof("workflows."+name, "the workflow only runs for pipelines triggered from the UI or API")
		} else if t.Manual {
			e.d.Infof("workflows."+name, "trigger manual runs with \"Trigger Pipeline\" in the CircleCI UI")
		}
		if jobs := e.workflowJobs(e.pushFilter(), scheduleOnly); len(jobs) > 0 {
			w.Set("jobs", jobs)
			wfs.Set(name, w)
		}
	}
	if hasSchedules {
		e.d.Warnf("workflows."+scheduledWF, "schedules were emitted as a legacy scheduled workflow; CircleCI recommends scheduled pipelines configured in the project settings")
		var triggers []any
		for _, s := range t.Schedules {
			branches := make([]string, 0, len(s.Branches))
			for _, b := range s.Branches {
				branches = append(branches, branchPattern(b))
			}
			if len(branches) == 0 {
				branches = []string{defaultSchedules}
				e.d.Infof("workflows."+scheduledWF, "schedule %q runs on branch %q; adjust it to your default branch", s.Cron, defaultSchedules)
			}
			sm := yamlx.NewMap().Set("cron", s.Cron).
				Set("filters", yamlx.NewMap().Set("branches", yamlx.NewMap().Set("only", oneOrList(branches))))
			triggers = append(triggers, yamlx.NewMap().Set("schedule", sm))
		}
		skip := e.excluded(isEvent(ir.EventTag))
		if jobs := e.workflowJobs(&filter{}, skip); len(jobs) > 0 {
			wfs.Set(scheduledWF, yamlx.NewMap().Set("triggers", triggers).Set("jobs", jobs))
		}
	}
	return wfs
}

// workflowJobs renders the job entries of one workflow.
func (e *emitter) workflowJobs(base *filter, skip map[string]bool) []any {
	filters := map[string]*filter{}
	for _, j := range e.p.Jobs {
		if !skip[j.ID] {
			filters[j.ID] = e.jobFilter(j, base)
		}
	}
	// Jobs required by a job that runs on tags must run on tags too.
	for _, j := range e.p.Jobs {
		f := filters[j.ID]
		if f == nil || len(f.tags) == 0 {
			continue
		}
		for _, u := range upstream(e.p, j) {
			if uf := filters[u.ID]; uf != nil && uf.raw == "" && len(uf.tags) == 0 {
				uf.tags = f.tags
				e.d.Infof("workflows."+e.ids[u.ID], "tag filters were added because job %s runs on tags and requires it", e.ids[j.ID])
			}
		}
	}
	var out []any
	for _, j := range e.p.Jobs {
		f := filters[j.ID]
		if f == nil {
			continue
		}
		id := e.ids[j.ID]
		path := "workflows." + id
		var needs []any
		for _, n := range j.Needs {
			needs = append(needs, e.ids[n])
		}
		opts := yamlx.NewMap()
		if hold, ok := e.holds[j.ID]; ok {
			ho := yamlx.NewMap().Set("type", "approval")
			ho.SetIf("requires", needs)
			ho.SetIf("filters", f.node())
			out = append(out, yamlx.NewMap().Set(hold, ho))
			opts.Set("requires", []any{hold})
		} else {
			opts.SetIf("requires", e.requires(j, needs, path))
		}
		opts.SetIf("filters", f.node())
		if j.Matrix != nil {
			e.params = map[string]string{}
			for _, v := range j.Matrix.Vars() {
				e.params[v] = paramName(v)
			}
			opts.SetIf("matrix", e.matrix(j.Matrix, path))
		}
		if j.If != nil && j.If.Manual && j.If.When != "" {
			e.d.Warnf(path, "a run condition on an approval-gated job was dropped")
		}
		if opts.Len() == 0 {
			out = append(out, id)
		} else {
			out = append(out, yamlx.NewMap().Set(id, opts))
		}
	}
	return out
}

// requires renders needs, using requirement statuses for jobs that run
// after failures.
func (e *emitter) requires(j *ir.Job, needs []any, path string) []any {
	when := ""
	if j.If != nil {
		when = j.If.When
	}
	if when == "" {
		return needs
	}
	if len(needs) == 0 {
		e.d.Warnf(path, "a job-level %s condition has no effect on CircleCI without upstream jobs and was dropped", when)
		return needs
	}
	status := "terminal"
	if when == ir.WhenOnFailure {
		status = "failed"
		if len(needs) > 1 {
			e.d.Infof(path, "the job runs only when every required job failed (CircleCI requires all statuses to match)")
		}
	}
	out := make([]any, 0, len(needs))
	for _, n := range needs {
		out = append(out, yamlx.NewMap().Set(n.(string), status))
	}
	return out
}

// matrix renders a job matrix as workflow matrix parameters. Explicit
// combinations become the product of their values minus exclusions.
func (e *emitter) matrix(m *ir.Matrix, path string) *yamlx.Map {
	params := yamlx.NewMap()
	out := yamlx.NewMap()
	if len(m.Include) == 0 && len(m.Exclude) == 0 {
		for _, ax := range m.Axes {
			params.Set(e.params[ax.Name], yamlx.Flow{Value: ax.Values})
		}
		return out.Set("parameters", params)
	}
	vars := m.Vars()
	var combos [][]ir.KV
	for _, c := range m.Combos() {
		complete := true
		for _, v := range vars {
			if _, ok := ir.KVGet(c, v); !ok {
				complete = false
			}
		}
		if complete {
			combos = append(combos, c)
		} else {
			e.d.Warnf(path+".matrix", "matrix combination %v does not set every variable and was dropped", c)
		}
	}
	values := map[string][]string{}
	for _, v := range vars {
		seen := map[string]bool{}
		for _, c := range combos {
			val, _ := ir.KVGet(c, v)
			if !seen[val] {
				seen[val] = true
				values[v] = append(values[v], val)
			}
		}
		params.Set(e.params[v], yamlx.Flow{Value: values[v]})
	}
	out.Set("parameters", params)
	// Enumerate the product and exclude what the combinations don't cover.
	product := [][]ir.KV{{}}
	for _, v := range vars {
		var next [][]ir.KV
		for _, c := range product {
			for _, val := range values[v] {
				next = append(next, append(append([]ir.KV(nil), c...), ir.KV{Key: v, Value: val}))
			}
		}
		product = next
	}
	var exclude []any
	for _, c := range product {
		if containsCombo(combos, c) {
			continue
		}
		em := yamlx.NewMap()
		for _, kv := range c {
			em.Set(e.params[kv.Key], kv.Value)
		}
		exclude = append(exclude, em)
	}
	out.SetIf("exclude", exclude)
	return out
}

func containsCombo(list [][]ir.KV, c []ir.KV) bool {
	for _, l := range list {
		match := true
		for _, kv := range c {
			if v, ok := ir.KVGet(l, kv.Key); !ok || v != kv.Value {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
