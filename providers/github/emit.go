package github

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

var jobIDRe = regexp.MustCompile(`^[_a-zA-Z][a-zA-Z0-9_-]*$`)

// Emit implements ir.Provider.
func (Provider) Emit(p *ir.Pipeline, d *ir.Diagnostics) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	taken := map[string]bool{}
	for _, j := range p.Jobs {
		id := j.ID
		if !jobIDRe.MatchString(id) {
			id = ir.Slug(id)
		}
		ids[j.ID] = ir.UniqueID(id, taken)
	}

	root := yamlx.NewMap()
	name := p.Name
	if name == "" {
		name = "CI"
	}
	root.Set("name", name)
	root.Set("on", emitOn(p.Triggers, d))
	if env := emitEnv(p.Env, false); env.Len() > 0 {
		root.Set("env", env)
	}
	jobs := yamlx.NewMap()
	for _, j := range p.Jobs {
		jobs.Set(ids[j.ID], emitJob(p, j, ids, d))
	}
	root.Set("jobs", jobs)
	return yamlx.Marshal(root, "")
}

func emitEnv(kvs []ir.KV, script bool) *yamlx.Map {
	m := yamlx.NewMap()
	for _, kv := range kvs {
		m.Set(kv.Key, toGH(kv.Value, script))
	}
	return m
}

func emitOn(t ir.Triggers, d *ir.Diagnostics) any {
	if t.Empty() {
		t.Push = &ir.PushTrigger{}
		t.PullRequest = &ir.PRTrigger{}
	}
	on := yamlx.NewMap()
	if t.Push != nil {
		m := yamlx.NewMap()
		m.SetIf("branches", ghPatterns(t.Push.Branches))
		m.SetIf("branches-ignore", ghPatterns(t.Push.BranchesIgnore))
		m.SetIf("tags", ghPatterns(t.Push.Tags))
		m.SetIf("paths", t.Push.Paths)
		m.SetIf("paths-ignore", t.Push.PathsIgnore)
		if m.Len() > 0 && len(t.Push.Branches) > 0 && len(t.Push.BranchesIgnore) > 0 {
			m.Delete("branches-ignore")
			d.Warnf("on.push", "GitHub cannot combine branches and branches-ignore; ignore filters were dropped")
		}
		on.Set("push", nilIfEmpty(m))
	}
	if t.PullRequest != nil {
		m := yamlx.NewMap()
		m.SetIf("branches", ghPatterns(t.PullRequest.Branches))
		m.SetIf("paths", t.PullRequest.Paths)
		on.Set("pull_request", nilIfEmpty(m))
	}
	if len(t.Schedules) > 0 {
		var items []any
		for _, s := range t.Schedules {
			items = append(items, yamlx.NewMap().Set("cron", s.Cron))
			if len(s.Branches) > 0 {
				d.Warnf("on.schedule", "GitHub schedules always run on the default branch; branch filters %v were dropped", s.Branches)
			}
		}
		on.Set("schedule", items)
	}
	if t.Manual {
		on.Set("workflow_dispatch", nil)
	}
	return on
}

// ghPatterns converts glob patterns: GitHub treats "*" as not matching "/",
// so a lone "*" should become "**" to keep "match everything" semantics.
func ghPatterns(pats []string) []string {
	var out []string
	for _, p := range pats {
		if p == "*" {
			p = "**"
		}
		out = append(out, p)
	}
	return out
}

func nilIfEmpty(m *yamlx.Map) any {
	if m.Len() == 0 {
		return nil
	}
	return m
}

func runsOn(r ir.Runner, d *ir.Diagnostics, path string) any {
	if len(r.Labels) > 0 {
		labels := []string{"self-hosted"}
		switch r.OS {
		case ir.OSWindows:
			labels = append(labels, "windows")
		case ir.OSMacOS:
			labels = append(labels, "macOS")
		}
		return append(labels, r.Labels...)
	}
	if img := strings.ToLower(r.VMImage); img != "" {
		if strings.Contains(img, "${{") {
			return toGH(r.VMImage, false)
		}
		for _, prefix := range []string{"ubuntu-", "windows-", "macos-"} {
			// Images such as CircleCI's "ubuntu-2204:current" are not labels.
			if strings.HasPrefix(img, prefix) && !strings.ContainsAny(img, ":/ ") {
				return img
			}
		}
		d.Infof(path, "runner image %q is not a GitHub-hosted label; using the OS default", r.VMImage)
	}
	switch r.OS {
	case ir.OSWindows:
		return "windows-latest"
	case ir.OSMacOS:
		return "macos-latest"
	}
	return "ubuntu-latest"
}

func emitJob(p *ir.Pipeline, j *ir.Job, ids map[string]string, d *ir.Diagnostics) *yamlx.Map {
	path := "jobs." + ids[j.ID]
	m := yamlx.NewMap()
	if j.Name != "" && j.Name != ids[j.ID] {
		m.Set("name", toGH(j.Name, false))
	}
	var needs []string
	for _, n := range j.Needs {
		needs = append(needs, ids[n])
	}
	if len(needs) == 1 {
		m.Set("needs", needs[0])
	} else {
		m.SetIf("needs", needs)
	}
	if cond := emitIf(j.If, d, path); cond != "" {
		m.Set("if", cond)
	}
	if j.If != nil && j.If.Manual {
		d.Warnf(path, "manual approval gates map to GitHub environments with required reviewers; configure one for environment %q", envOrDefault(j))
		if j.Environment == "" {
			m.Set("environment", envOrDefault(j))
		}
	}
	m.Set("runs-on", runsOn(j.Runner, d, path))
	if j.Environment != "" {
		m.Set("environment", j.Environment)
	}
	if j.Image != "" {
		m.Set("container", toGH(jobField(p, j, j.Image, path+".container", d), false))
	}
	if len(j.Services) > 0 {
		svcs := yamlx.NewMap()
		for _, s := range j.Services {
			sm := yamlx.NewMap().Set("image", toGH(jobField(p, j, s.Image, path+".services", d), false))
			if env := emitEnv(s.Env, false); env.Len() > 0 {
				sm.Set("env", env)
			}
			sm.SetIf("ports", s.Ports)
			svcs.Set(ir.Slug(s.Name), sm)
		}
		m.Set("services", svcs)
		if j.Image == "" {
			d.Infof(path+".services", "services run on the runner host; reach them via localhost and mapped ports")
		}
	}
	if j.Matrix != nil {
		m.Set("strategy", yamlx.NewMap().Set("matrix", emitMatrix(j.Matrix)))
	}
	env := emitEnv(j.Env, false)
	// Scripts from other providers read matrix values as shell variables.
	for _, v := range j.Matrix.Vars() {
		if _, set := env.Get(v); !set && scriptsUseVar(j, v) {
			env.Set(v, "${{ matrix."+v+" }}")
		}
	}
	if env.Len() > 0 {
		m.Set("env", env)
	}
	if j.TimeoutMinutes > 0 {
		m.Set("timeout-minutes", j.TimeoutMinutes)
	}
	if j.AllowFailure {
		m.Set("continue-on-error", true)
	}
	var steps []any
	for i, st := range j.Steps {
		steps = append(steps, emitStep(p, j, st, fmt.Sprintf("%s.steps[%d]", path, i), d)...)
	}
	m.Set("steps", steps)
	return m
}

// jobField resolves env references in fields where GitHub does not allow
// the env context (container and service images).
func jobField(p *ir.Pipeline, j *ir.Job, s, path string, d *ir.Diagnostics) string {
	out, ok := ir.ResolveEnvRefs(s, j.Env, p.Env)
	if !ok {
		d.Warnf(path, "%q references variables GitHub cannot expand here; set the value explicitly", s)
	}
	return out
}

func envOrDefault(j *ir.Job) string {
	if j.Environment != "" {
		return j.Environment
	}
	return "approval"
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

func emitMatrix(m *ir.Matrix) *yamlx.Map {
	out := yamlx.NewMap()
	for _, ax := range m.Axes {
		out.Set(ax.Name, yamlx.Flow{Value: ax.Values})
	}
	combos := func(list [][]ir.KV) []any {
		var items []any
		for _, c := range list {
			cm := yamlx.NewMap()
			for _, kv := range c {
				cm.Set(kv.Key, kv.Value)
			}
			items = append(items, cm)
		}
		return items
	}
	if len(m.Include) > 0 {
		out.Set("include", combos(m.Include))
	}
	if len(m.Exclude) > 0 {
		out.Set("exclude", combos(m.Exclude))
	}
	return out
}

func emitIf(c *ir.Condition, d *ir.Diagnostics, path string) string {
	if c.IsZero() {
		return ""
	}
	if c.Raw != "" {
		if c.RawFrom == "github" {
			return c.Raw
		}
		d.Warnf(path, "condition %q (from %s) could not be translated and was dropped", c.Raw, c.RawFrom)
	}
	var parts []string
	switch c.When {
	case ir.WhenAlways:
		parts = append(parts, "always()")
	case ir.WhenOnFailure:
		parts = append(parts, "failure()")
	}
	switch c.Event {
	case ir.EventTag:
		parts = append(parts, "startsWith(github.ref, 'refs/tags/')")
	case ir.EventPush:
		parts = append(parts, "github.event_name == 'push'")
	case ir.EventPullRequest:
		parts = append(parts, "github.event_name == 'pull_request'")
	case ir.EventSchedule:
		parts = append(parts, "github.event_name == 'schedule'")
	case ir.EventManual:
		parts = append(parts, "github.event_name == 'workflow_dispatch'")
	}
	var br []string
	for _, b := range c.Branches {
		if strings.HasSuffix(b, "*") {
			br = append(br, fmt.Sprintf("startsWith(github.ref, 'refs/heads/%s')", strings.TrimRight(b, "*")))
		} else {
			br = append(br, fmt.Sprintf("github.ref == 'refs/heads/%s'", b))
		}
	}
	switch {
	case len(br) == 1:
		parts = append(parts, br[0])
	case len(br) > 1:
		if len(parts) == 0 {
			parts = append(parts, strings.Join(br, " || "))
		} else {
			parts = append(parts, "("+strings.Join(br, " || ")+")")
		}
	}
	return strings.Join(parts, " && ")
}

func emitStep(p *ir.Pipeline, j *ir.Job, st *ir.Step, path string, d *ir.Diagnostics) []any {
	m := yamlx.NewMap()
	m.SetIf("name", toGH(st.Name, false))
	if cond := emitIf(st.If, d, path); cond != "" {
		m.Set("if", cond)
	}
	if st.If != nil && st.If.Manual {
		d.Warnf(path, "manual steps are not supported by GitHub Actions; the step runs automatically")
	}
	with := yamlx.NewMap()
	switch st.Kind {
	case ir.StepCheckout:
		m.Set("uses", "actions/checkout@v4")
		if c := st.Checkout; c != nil {
			switch {
			case c.Depth < 0:
				with.Set("fetch-depth", 0)
			case c.Depth > 0:
				with.Set("fetch-depth", c.Depth)
			}
			if c.Submodules {
				with.Set("submodules", "recursive")
			}
			if c.LFS {
				with.Set("lfs", true)
			}
		}
	case ir.StepTool:
		action, input := toolAction(st.Tool)
		if action == "" {
			d.Warnf(path, "no setup action known for tool %q", st.Tool)
			return nil
		}
		m.Set("uses", action)
		if st.Tool == ir.ToolJava {
			with.Set("distribution", "temurin")
		}
		if st.Version != "" {
			with.Set(input, toGH(st.Version, false))
		}
	case ir.StepCache:
		c := st.Cache
		m.Set("uses", "actions/cache@v4")
		with.Set("path", strings.Join(c.Paths, "\n"))
		key := "${{ runner.os }}-" + c.Prefix
		if len(c.Files) > 0 {
			var q []string
			for _, f := range c.Files {
				q = append(q, "'"+globAll(f)+"'")
			}
			with.Set("key", key+"-${{ hashFiles("+strings.Join(q, ", ")+") }}")
			with.Set("restore-keys", key+"-")
		} else {
			with.Set("key", key)
		}
	case ir.StepUpload:
		a := st.Artifact
		m.Set("uses", "actions/upload-artifact@v4")
		with.Set("name", toGH(uploadName(a, j), false))
		with.Set("path", toGH(strings.Join(a.Paths, "\n"), false))
		if days, ok := ir.DurationDays(a.ExpireIn); ok {
			with.Set("retention-days", days)
		}
		switch a.When {
		case ir.WhenAlways:
			m.Set("if", "always()")
		case ir.WhenOnFailure:
			m.Set("if", "failure()")
		}
	case ir.StepDownload:
		return emitDownloads(p, j, st, m, d, path)
	case ir.StepAction:
		if st.UsesFrom != "github" {
			d.Warnf(path, "%s step %q has no GitHub equivalent; a failing placeholder step was emitted", st.UsesFrom, st.Uses)
			m.Set("run", fmt.Sprintf("echo \"::error::cistela: port %s step '%s' manually\"\nexit 1", st.UsesFrom, st.Uses))
			return []any{m}
		}
		m.Set("uses", st.Uses)
		for _, kv := range st.With {
			with.Set(kv.Key, toGH(kv.Value, false))
		}
	default:
		run := st.Run
		m.Set("run", toGH(run, true))
		m.SetIf("shell", st.Shell)
		m.SetIf("working-directory", toGH(st.WorkingDir, false))
	}
	if with.Len() > 0 {
		m.Set("with", with)
	}
	env := emitEnv(st.Env, false)
	for _, s := range ir.StepSecrets(st) {
		if st.Kind == ir.StepRun || st.Kind == "" {
			if _, set := env.Get(s); !set {
				env.Set(s, "${{ secrets."+s+" }}")
			}
		}
	}
	if env.Len() > 0 {
		m.Set("env", env)
	}
	if st.AllowFailure {
		m.Set("continue-on-error", true)
	}
	if st.TimeoutMinutes > 0 {
		m.Set("timeout-minutes", st.TimeoutMinutes)
	}
	return []any{m}
}

// globAll makes lockfile patterns match anywhere in the repo like the
// equivalent keys of other providers do for root-level files.
func globAll(f string) string {
	if strings.ContainsAny(f, "*/") {
		return f
	}
	return "**/" + f
}

func toolAction(tool string) (action, input string) {
	switch tool {
	case ir.ToolNode:
		return "actions/setup-node@v4", "node-version"
	case ir.ToolPython:
		return "actions/setup-python@v5", "python-version"
	case ir.ToolGo:
		return "actions/setup-go@v5", "go-version"
	case ir.ToolJava:
		return "actions/setup-java@v4", "java-version"
	case ir.ToolRuby:
		return "ruby/setup-ruby@v1", "ruby-version"
	case ir.ToolDotnet:
		return "actions/setup-dotnet@v4", "dotnet-version"
	}
	return "", ""
}

func artifactName(a *ir.Artifact, j *ir.Job) string {
	if a.Name != "" {
		return a.Name
	}
	return ir.Slug(j.ID)
}

// uploadName makes artifact names unique per matrix combination, since
// upload-artifact@v4 refuses to upload the same name twice in a run.
func uploadName(a *ir.Artifact, j *ir.Job) string {
	name := artifactName(a, j)
	if j.Matrix == nil || strings.Contains(name, "${{ matrix.") {
		return name
	}
	for _, v := range j.Matrix.Vars() {
		name += "-" + ir.Ref(ir.RefMatrix, v)
	}
	return name
}

// emitDownloads restores artifacts to the paths they were uploaded from.
// GitHub strips the common directory of uploaded paths, so the download
// path must point back at it.
func emitDownloads(p *ir.Pipeline, j *ir.Job, st *ir.Step, base *yamlx.Map, d *ir.Diagnostics, where string) []any {
	type src struct {
		name    string
		pattern bool // matrix uploads: one artifact per combination
		dir     string
	}
	var sources []src
	for _, dep := range upstream(p, j) {
		for _, s := range dep.Steps {
			if s.Kind != ir.StepUpload {
				continue
			}
			name := artifactName(s.Artifact, dep)
			if st.Artifact.Name != "" && st.Artifact.Name != name {
				continue
			}
			sources = append(sources, src{name: name, pattern: dep.Matrix != nil, dir: commonDir(s.Artifact.Paths)})
		}
	}
	if len(sources) == 0 {
		m := yamlx.NewMap()
		for _, k := range base.Keys() {
			v, _ := base.Get(k)
			m.Set(k, v)
		}
		m.Set("uses", "actions/download-artifact@v4")
		with := yamlx.NewMap()
		with.SetIf("name", st.Artifact.Name)
		if st.Artifact.Name == "" {
			with.Set("merge-multiple", true)
		}
		if with.Len() > 0 {
			m.Set("with", with)
		}
		d.Infof(where, "could not find the matching upload; check the download path")
		return []any{m}
	}
	var out []any
	for _, s := range sources {
		m := yamlx.NewMap()
		for _, k := range base.Keys() {
			v, _ := base.Get(k)
			m.Set(k, v)
		}
		if _, ok := m.Get("name"); !ok {
			m.Set("name", "Download "+s.name)
		}
		m.Set("uses", "actions/download-artifact@v4")
		with := yamlx.NewMap()
		if s.pattern {
			base := s.name
			if i := strings.Index(base, "${{"); i >= 0 {
				base = base[:i]
			} else {
				base += "-"
			}
			with.Set("pattern", toGH(base, false)+"*")
			with.Set("merge-multiple", true)
		} else {
			with.Set("name", toGH(s.name, false))
		}
		if s.dir != "." {
			with.Set("path", s.dir)
		}
		m.Set("with", with)
		out = append(out, m)
	}
	return out
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

// commonDir returns the deepest directory containing every path, the way
// upload-artifact computes its root.
func commonDir(paths []string) string {
	var dirs []string
	for _, p := range paths {
		p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		if strings.ContainsAny(p, "*?[") {
			p = path.Dir(p[:strings.IndexAny(p, "*?[")+1])
		}
		dirs = append(dirs, p)
	}
	if len(dirs) == 0 {
		return "."
	}
	common := strings.Split(dirs[0], "/")
	if len(dirs) > 1 || looksLikeFile(dirs[0]) {
		// For a single file the root is its directory.
		if len(dirs) == 1 {
			common = common[:len(common)-1]
		}
		for _, dd := range dirs[1:] {
			parts := strings.Split(dd, "/")
			if looksLikeFile(dd) {
				parts = parts[:len(parts)-1]
			}
			n := 0
			for n < len(common) && n < len(parts) && common[n] == parts[n] {
				n++
			}
			common = common[:n]
		}
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
