package gitlab

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// reserved top-level keys that are not jobs.
var reserved = map[string]bool{
	"image": true, "services": true, "stages": true, "types": true, "before_script": true,
	"after_script": true, "variables": true, "cache": true, "default": true, "include": true,
	"workflow": true, "spec": true,
}

var defaultStages = []string{".pre", "build", "test", "deploy", ".post"}

type parser struct {
	root *yaml.Node
	d    *ir.Diagnostics
	defs *yaml.Node // "default:" section merged with legacy globals
	meta map[string]*jobMeta
}

// jobMeta keeps the GitLab-specific facts that decide artifact passing.
type jobMeta struct {
	explicitNeeds bool
	noArtifacts   map[string]bool // needs entries with artifacts: false
	deps          []string        // dependencies: list
	hasDeps       bool
}

// Parse implements ir.Provider.
func (Provider) Parse(data []byte, d *ir.Diagnostics) (*ir.Pipeline, error) {
	root, err := yamlx.Load(data)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %w", err)
	}
	if !yamlx.IsMap(root) {
		return nil, fmt.Errorf("gitlab: configuration must be a mapping")
	}
	if err := resolveReferences(root, root, 0); err != nil {
		return nil, fmt.Errorf("gitlab: %w", err)
	}
	ps := &parser{root: root, d: d, meta: map[string]*jobMeta{}}
	if yamlx.Has(root, "include") {
		d.Warnf("include", "included files are not resolved; convert them separately and merge the result")
	}

	// Legacy globals act like default: entries.
	ps.defs = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range []string{"image", "services", "before_script", "after_script", "cache"} {
		if v := yamlx.Get(root, key); v != nil {
			ps.defs.Content = append(ps.defs.Content, yamlx.Key(key), v)
		}
	}
	if def := yamlx.Get(root, "default"); yamlx.IsMap(def) {
		ps.defs = mergeNodes(ps.defs, def)
	}

	p := &ir.Pipeline{}
	for _, pair := range yamlx.Pairs(yamlx.Get(root, "variables")) {
		p.Env = append(p.Env, ir.KV{Key: pair.Key, Value: fromGL(varValue(pair.Value), false)})
	}
	if n := yamlx.Str(yamlx.Path(root, "workflow", "name")); n != "" {
		p.Name = fromGL(n, false)
	}
	p.Triggers = ps.parseWorkflow(yamlx.Path(root, "workflow", "rules"))

	stages := yamlx.StrList(yamlx.Get(root, "stages"))
	if len(stages) == 0 {
		stages = yamlx.StrList(yamlx.Get(root, "types"))
	}
	if len(stages) == 0 {
		stages = defaultStages
	} else {
		stages = append(append([]string{".pre"}, stages...), ".post")
	}

	for _, pair := range yamlx.Pairs(root) {
		if reserved[pair.Key] || strings.HasPrefix(pair.Key, ".") || !yamlx.IsMap(pair.Value) {
			continue
		}
		job, err := ps.expandExtends(pair.Key, pair.Value, 0)
		if err != nil {
			return nil, err
		}
		j, err := ps.parseJob(pair.Key, job)
		if err != nil {
			return nil, err
		}
		if j != nil {
			p.Jobs = append(p.Jobs, j)
		}
	}
	pos := applyStageOrder(p, stages)
	ps.addArtifactDownloads(p, pos)
	for _, j := range p.Jobs {
		ir.BindMatrixRefs(j)
	}
	ir.PromoteExternalVars(p, d, builtinPrefixes...)
	return p, nil
}

func varValue(n *yaml.Node) string {
	if yamlx.IsMap(n) {
		return yamlx.Str(yamlx.Get(n, "value"))
	}
	return yamlx.Str(n)
}

// resolveReferences replaces !reference [job, key, ...] tags in place.
func resolveReferences(root, n *yaml.Node, depth int) error {
	if depth > 64 {
		return fmt.Errorf("!reference nesting too deep")
	}
	for i, c := range n.Content {
		if c.Tag == "!reference" {
			keys := yamlx.StrList(c)
			target := yamlx.Path(root, keys...)
			if target == nil {
				return fmt.Errorf("line %d: !reference %v not found", c.Line, keys)
			}
			if err := resolveReferences(root, target, depth+1); err != nil {
				return err
			}
			cp := *target
			n.Content[i] = &cp
			continue
		}
		if err := resolveReferences(root, c, depth+1); err != nil {
			return err
		}
	}
	// Scripts may contain a !reference that expands to a list: flatten.
	if yamlx.IsSeq(n) {
		var flat []*yaml.Node
		for _, c := range n.Content {
			if yamlx.IsSeq(c) {
				flat = append(flat, c.Content...)
			} else {
				flat = append(flat, c)
			}
		}
		n.Content = flat
	}
	return nil
}

// mergeNodes deep-merges mappings (override wins; sequences are replaced).
func mergeNodes(base, override *yaml.Node) *yaml.Node {
	if !yamlx.IsMap(base) || !yamlx.IsMap(override) {
		return override
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, p := range yamlx.Pairs(base) {
		v := p.Value
		if ov := yamlx.Get(override, p.Key); ov != nil {
			v = mergeNodes(v, ov)
		}
		out.Content = append(out.Content, yamlx.Key(p.Key), v)
	}
	for _, p := range yamlx.Pairs(override) {
		if !yamlx.Has(base, p.Key) {
			out.Content = append(out.Content, yamlx.Key(p.Key), p.Value)
		}
	}
	return out
}

func (ps *parser) expandExtends(name string, job *yaml.Node, depth int) (*yaml.Node, error) {
	if depth > 16 {
		return nil, fmt.Errorf("gitlab: extends chain too deep at %q", name)
	}
	parents := yamlx.StrList(yamlx.Get(job, "extends"))
	if len(parents) == 0 {
		return job, nil
	}
	var merged *yaml.Node
	for _, parent := range parents {
		pn := yamlx.Get(ps.root, parent)
		if pn == nil {
			ps.d.Warnf(name+".extends", "template %q not found (defined in an include?)", parent)
			continue
		}
		pn, err := ps.expandExtends(parent, pn, depth+1)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = pn
		} else {
			merged = mergeNodes(merged, pn)
		}
	}
	if merged == nil {
		return job, nil
	}
	return mergeNodes(merged, job), nil
}

func (ps *parser) get(job *yaml.Node, key string) *yaml.Node {
	if v := yamlx.Get(job, key); v != nil {
		return v
	}
	inherit := yamlx.Path(job, "inherit", "default")
	if v, ok := yamlx.Bool(inherit); ok && !v {
		return nil
	}
	if yamlx.IsSeq(inherit) {
		allowed := false
		for _, k := range yamlx.StrList(inherit) {
			if k == key {
				allowed = true
			}
		}
		if !allowed {
			return nil
		}
	}
	return yamlx.Get(ps.defs, key)
}

func (ps *parser) parseJob(name string, n *yaml.Node) (*ir.Job, error) {
	path := name
	d := ps.d
	if yamlx.Has(n, "trigger") {
		d.Warnf(path, "trigger jobs (downstream/child pipelines) are not portable; the job was dropped")
		return nil, nil
	}
	j := &ir.Job{ID: name, Stage: yamlx.Str(yamlx.Get(n, "stage"))}
	if j.Stage == "" {
		j.Stage = "test"
	}
	if img := ps.get(n, "image"); img != nil {
		if yamlx.IsMap(img) {
			j.Image = fromGL(yamlx.Str(yamlx.Get(img, "name")), false)
			if yamlx.Has(img, "entrypoint") {
				d.Warnf(path+".image.entrypoint", "image entrypoint overrides were dropped")
			}
		} else {
			j.Image = fromGL(yamlx.Str(img), false)
		}
	}
	for _, s := range yamlx.Items(ps.get(n, "services")) {
		svc := ir.Service{}
		if yamlx.IsMap(s) {
			svc.Image = fromGL(yamlx.Str(yamlx.Get(s, "name")), false)
			svc.Name = yamlx.Str(yamlx.Get(s, "alias"))
			for _, pair := range yamlx.Pairs(yamlx.Get(s, "variables")) {
				svc.Env = append(svc.Env, ir.KV{Key: pair.Key, Value: fromGL(varValue(pair.Value), false)})
			}
		} else {
			svc.Image = fromGL(yamlx.Str(s), false)
		}
		if svc.Name == "" {
			svc.Name = serviceAlias(svc.Image)
		}
		j.Services = append(j.Services, svc)
	}
	var gitDepth, submodules string
	for _, pair := range yamlx.Pairs(yamlx.Get(n, "variables")) {
		v := varValue(pair.Value)
		switch pair.Key {
		case "GIT_DEPTH":
			gitDepth = v
			continue
		case "GIT_SUBMODULE_STRATEGY":
			submodules = v
			continue
		}
		j.Env = append(j.Env, ir.KV{Key: pair.Key, Value: fromGL(v, false)})
	}
	// needs: absent means "stage order"; [] means "no dependencies".
	meta := &jobMeta{noArtifacts: map[string]bool{}}
	ps.meta[name] = meta
	if needs := yamlx.Get(n, "needs"); needs != nil {
		meta.explicitNeeds = true
		j.Needs = []string{}
		for _, item := range yamlx.Items(needs) {
			if yamlx.IsMap(item) {
				if yamlx.Has(item, "pipeline") || yamlx.Has(item, "project") {
					d.Warnf(path+".needs", "cross-pipeline needs are not portable")
					continue
				}
				dep := yamlx.Str(yamlx.Get(item, "job"))
				if v, ok := yamlx.Bool(yamlx.Get(item, "artifacts")); ok && !v {
					meta.noArtifacts[dep] = true
				}
				j.Needs = append(j.Needs, dep)
			} else {
				j.Needs = append(j.Needs, yamlx.Str(item))
			}
		}
	}
	if deps := yamlx.Get(n, "dependencies"); deps != nil {
		meta.hasDeps = true
		meta.deps = yamlx.StrList(deps)
	}

	for _, t := range yamlx.StrList(yamlx.Get(n, "tags")) {
		lt := strings.ToLower(t)
		switch {
		case strings.Contains(lt, "windows"):
			j.Runner.OS = ir.OSWindows
		case strings.Contains(lt, "macos"):
			j.Runner.OS = ir.OSMacOS
		}
		if !strings.HasPrefix(lt, "saas-") {
			j.Runner.Labels = append(j.Runner.Labels, t)
		}
	}
	if t := yamlx.Str(ps.get(n, "timeout")); t != "" {
		if m, ok := ir.ParseMinutes(t); ok {
			j.TimeoutMinutes = m
		}
	}
	if af := yamlx.Get(n, "allow_failure"); af != nil {
		if v, ok := yamlx.Bool(af); ok {
			j.AllowFailure = v
		} else if yamlx.IsMap(af) {
			j.AllowFailure = true
			d.Warnf(path+".allow_failure", "exit-code specific allow_failure was widened to any failure")
		}
	}
	if env := yamlx.Get(n, "environment"); env != nil {
		if yamlx.IsMap(env) {
			j.Environment = yamlx.Str(yamlx.Get(env, "name"))
		} else {
			j.Environment = yamlx.Str(env)
		}
	}
	for _, k := range []string{"retry", "interruptible", "resource_group", "coverage", "release", "secrets", "id_tokens", "hooks", "pages"} {
		if yamlx.Has(n, k) {
			d.Warnf(path+"."+k, "%q is not portable and was dropped", k)
		}
	}
	j.If = ps.parseJobCondition(n, path)
	j.Matrix = parseParallel(yamlx.Get(n, "parallel"), path, d)

	// Steps: implicit checkout, cache, script sections, artifacts.
	co := &ir.Checkout{}
	if gitDepth != "" {
		var depth int
		if _, err := fmt.Sscanf(gitDepth, "%d", &depth); err == nil {
			co.Depth = depth
			if depth == 0 {
				co.Depth = -1
			}
		}
	}
	co.Submodules = submodules == "recursive" || submodules == "normal"
	if yamlx.Str(yamlx.Get(yamlx.Get(n, "variables"), "GIT_STRATEGY")) != "none" {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepCheckout, Checkout: co})
	}
	for _, c := range cacheList(ps.get(n, "cache")) {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepCache, Cache: parseCache(c)})
	}
	for _, s := range scriptSteps(ps.get(n, "before_script")) {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepRun, Run: s})
	}
	main := scriptSteps(yamlx.Get(n, "script"))
	for _, s := range main {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepRun, Run: s})
	}
	if len(main) == 0 && !yamlx.Has(n, "run") {
		d.Warnf(path, "job has no script")
	}
	for _, s := range scriptSteps(ps.get(n, "after_script")) {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepRun, Run: s, If: &ir.Condition{When: ir.WhenAlways}, AllowFailure: true})
	}
	if a := yamlx.Get(n, "artifacts"); a != nil {
		paths := yamlx.StrList(yamlx.Get(a, "paths"))
		if yamlx.Has(a, "reports") {
			for _, pair := range yamlx.Pairs(yamlx.Get(a, "reports")) {
				paths = append(paths, yamlx.StrList(pair.Value)...)
			}
			d.Infof(path+".artifacts.reports", "report artifacts were kept as plain artifacts")
		}
		if len(paths) > 0 {
			art := &ir.Artifact{
				Name:     yamlx.Str(yamlx.Get(a, "name")),
				Paths:    paths,
				ExpireIn: yamlx.Str(yamlx.Get(a, "expire_in")),
			}
			art.Name = fromGL(art.Name, false)
			switch yamlx.Str(yamlx.Get(a, "when")) {
			case "always":
				art.When = ir.WhenAlways
			case "on_failure":
				art.When = ir.WhenOnFailure
			}
			j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepUpload, Artifact: art})
		}
	}
	return j, nil
}

func serviceAlias(image string) string {
	name, _, _ := strings.Cut(image, ":")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return ir.Slug(name)
}

// scriptSteps groups script items into run steps: consecutive one-line
// commands form one step, and every multi-line item is a step of its own.
// This mirrors how the emitter lays steps out, so conversions are stable.
func scriptSteps(n *yaml.Node) []string {
	var steps, group []string
	flush := func() {
		if len(group) > 0 {
			steps = append(steps, strings.Join(group, "\n"))
			group = nil
		}
	}
	for _, item := range yamlx.StrList(n) {
		item = fromGL(strings.TrimRight(item, "\n"), true)
		if strings.Contains(item, "\n") {
			flush()
			steps = append(steps, item)
			continue
		}
		group = append(group, item)
	}
	flush()
	return steps
}

func cacheList(n *yaml.Node) []*yaml.Node {
	if yamlx.IsSeq(n) {
		return n.Content
	}
	if yamlx.IsMap(n) {
		return []*yaml.Node{n}
	}
	return nil
}

func parseCache(n *yaml.Node) *ir.Cache {
	c := &ir.Cache{Paths: yamlx.StrList(yamlx.Get(n, "paths"))}
	key := yamlx.Get(n, "key")
	if yamlx.IsMap(key) {
		c.Files = yamlx.StrList(yamlx.Get(key, "files"))
		c.Prefix = yamlx.Str(yamlx.Get(key, "prefix"))
	} else {
		c.Prefix = yamlx.Str(key)
	}
	c.Prefix = ir.Slug(ir.ReplaceShellVars(c.Prefix, func(string) (string, bool) { return "", true }))
	if c.Prefix == "job" || c.Prefix == "" {
		c.Prefix = "deps"
	}
	return c
}

func parseParallel(n *yaml.Node, path string, d *ir.Diagnostics) *ir.Matrix {
	if n == nil {
		return nil
	}
	if yamlx.IsScalar(n) {
		d.Warnf(path+".parallel", "parallel: %s (job splitting) is not portable and was dropped", n.Value)
		return nil
	}
	entries := yamlx.Items(yamlx.Get(n, "matrix"))
	if len(entries) == 0 {
		return nil
	}
	m := &ir.Matrix{}
	if len(entries) == 1 {
		for _, pair := range yamlx.Pairs(entries[0]) {
			m.Axes = append(m.Axes, ir.Axis{Name: pair.Key, Values: yamlx.StrList(pair.Value)})
		}
		return m
	}
	// Several entries: each is its own cartesian product; expand them all.
	for _, e := range entries {
		sub := &ir.Matrix{}
		for _, pair := range yamlx.Pairs(e) {
			sub.Axes = append(sub.Axes, ir.Axis{Name: pair.Key, Values: yamlx.StrList(pair.Value)})
		}
		m.Include = append(m.Include, sub.Combos()...)
	}
	return m
}

// applyStageOrder turns implicit stage ordering into explicit needs and
// returns each job's stage position.
func applyStageOrder(p *ir.Pipeline, stages []string) map[string]int {
	pos := map[string]int{}
	for i, s := range stages {
		pos[s] = i
	}
	byStage := map[int][]string{}
	for _, j := range p.Jobs {
		if _, ok := pos[j.Stage]; !ok {
			pos[j.Stage] = len(stages)
			stages = append(stages, j.Stage)
		}
		byStage[pos[j.Stage]] = append(byStage[pos[j.Stage]], j.ID)
	}
	for _, j := range p.Jobs {
		if j.Needs != nil {
			continue
		}
		for s := pos[j.Stage] - 1; s >= 0; s-- {
			if len(byStage[s]) > 0 {
				j.Needs = append([]string(nil), byStage[s]...)
				break
			}
		}
	}
	jobPos := map[string]int{}
	for _, j := range p.Jobs {
		jobPos[j.ID] = pos[j.Stage]
		if len(j.Needs) == 0 {
			j.Needs = nil
		}
		switch j.Stage {
		case ".pre", ".post":
			j.Stage = strings.TrimPrefix(j.Stage, ".")
		}
	}
	return jobPos
}

// addArtifactDownloads makes GitLab's implicit artifact passing explicit.
// A job receives artifacts from its dependencies: list when present, else
// from its needs when they are explicit, else from every earlier stage.
func (ps *parser) addArtifactDownloads(p *ir.Pipeline, stagePos map[string]int) {
	uploads := map[string][]string{}
	for _, j := range p.Jobs {
		for _, st := range j.Steps {
			if st.Kind == ir.StepUpload {
				// Unnamed artifacts are known by their job, as emitters name them.
				name := st.Artifact.Name
				if name == "" {
					name = ir.Slug(j.ID)
				}
				uploads[j.ID] = append(uploads[j.ID], name)
			}
		}
	}
	for _, j := range p.Jobs {
		meta := ps.meta[j.ID]
		var sources []string
		switch {
		case meta.hasDeps:
			sources = meta.deps
		case meta.explicitNeeds:
			for _, n := range j.Needs {
				if !meta.noArtifacts[n] {
					sources = append(sources, n)
				}
			}
		default:
			for _, other := range p.Jobs {
				if stagePos[other.ID] < stagePos[j.ID] {
					sources = append(sources, other.ID)
				}
			}
		}
		var downloads []*ir.Step
		for _, src := range sources {
			for _, name := range uploads[src] {
				downloads = append(downloads, &ir.Step{Kind: ir.StepDownload, Artifact: &ir.Artifact{Name: name}})
			}
			if len(uploads[src]) > 0 && !contains(upstreamOf(p, j), src) {
				ps.d.Warnf(j.ID+".dependencies", "artifacts of %q are used but the job does not wait for it", src)
			}
		}
		if len(downloads) == 0 {
			continue
		}
		// Insert after checkout/cache steps.
		idx := 0
		for idx < len(j.Steps) && (j.Steps[idx].Kind == ir.StepCheckout || j.Steps[idx].Kind == ir.StepCache) {
			idx++
		}
		j.Steps = append(j.Steps[:idx], append(downloads, j.Steps[idx:]...)...)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// upstreamOf returns the IDs of every job j transitively needs.
func upstreamOf(p *ir.Pipeline, j *ir.Job) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(*ir.Job)
	walk = func(x *ir.Job) {
		for _, n := range x.Needs {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
				if dep := p.FindJob(n); dep != nil {
					walk(dep)
				}
			}
		}
	}
	walk(j)
	return out
}

var (
	cmpRe      = regexp.MustCompile(`^\$(\w+)\s*(==|!=|=~|!~)\s*(.+)$`)
	presenceRe = regexp.MustCompile(`^\$(\w+)$`)
)

// clause is one comparison in a rules:if expression.
type clause struct {
	varName string
	op      string
	value   string // unquoted literal, regex body (/.../) or $VAR
}

func parseClauses(expr string) ([]clause, bool) {
	expr = strings.TrimSpace(expr)
	if strings.Contains(expr, "||") || strings.Contains(expr, "(") {
		return nil, false
	}
	var out []clause
	for _, part := range strings.Split(expr, "&&") {
		part = strings.TrimSpace(part)
		if m := presenceRe.FindStringSubmatch(part); m != nil {
			out = append(out, clause{varName: m[1], op: "set"})
			continue
		}
		m := cmpRe.FindStringSubmatch(part)
		if m == nil {
			return nil, false
		}
		v := strings.TrimSpace(m[3])
		v = strings.Trim(v, `"'`)
		out = append(out, clause{varName: m[1], op: m[2], value: v})
	}
	return out, true
}

// branchValue interprets the right side of a branch comparison as a glob.
func branchValue(c clause) (string, bool) {
	switch c.op {
	case "==":
		if c.value == "$CI_DEFAULT_BRANCH" {
			return "main", true
		}
		if strings.HasPrefix(c.value, "$") {
			return "", false
		}
		return c.value, true
	case "=~":
		if m := gitlabRegexLit.FindStringSubmatch(c.value); m != nil {
			return ir.RegexToGlob(m[1])
		}
	}
	return "", false
}

// parseWorkflow maps workflow:rules to pipeline triggers.
func (ps *parser) parseWorkflow(rules *yaml.Node) ir.Triggers {
	var t ir.Triggers
	if rules == nil {
		t.Push = &ir.PushTrigger{}
		return t
	}
	push := func() *ir.PushTrigger {
		if t.Push == nil {
			t.Push = &ir.PushTrigger{}
		}
		return t.Push
	}
	for i, r := range yamlx.Items(rules) {
		path := fmt.Sprintf("workflow.rules[%d]", i)
		when := yamlx.Str(yamlx.Get(r, "when"))
		expr := yamlx.Str(yamlx.Get(r, "if"))
		clauses, ok := parseClauses(expr)
		if expr == "" {
			if when != "never" {
				push()
			}
			continue
		}
		if !ok {
			ps.d.Warnf(path, "workflow rule %q could not be translated", expr)
			continue
		}
		if when == "never" {
			for _, c := range clauses {
				if c.varName == "CI_COMMIT_BRANCH" && (c.op == "==" || c.op == "=~") {
					if b, ok := branchValue(c); ok {
						push().BranchesIgnore = append(push().BranchesIgnore, b)
					}
				}
			}
			continue
		}
		matched := false
		isPR := false
		for _, c := range clauses {
			switch {
			case c.varName == "CI_PIPELINE_SOURCE" && c.op == "==":
				matched = true
				switch c.value {
				case "merge_request_event", "external_pull_request_event":
					if t.PullRequest == nil {
						t.PullRequest = &ir.PRTrigger{}
					}
					isPR = true
				case "push":
					push()
				case "schedule":
					// The cron lives in the project settings, not in the file.
					t.Schedules = append(t.Schedules, ir.Schedule{})
					ps.d.Warnf(path, "GitLab schedules live in the project settings; set the cron expression in the converted pipeline")
				case "web", "api", "trigger":
					t.Manual = true
				default:
					matched = false
				}
			case c.varName == "CI_COMMIT_BRANCH" && c.op == "set":
				push()
				matched = true
			case c.varName == "CI_COMMIT_BRANCH":
				if b, ok := branchValue(c); ok && c.op != "!=" && c.op != "!~" {
					push().Branches = append(push().Branches, b)
					matched = true
				}
			case c.varName == "CI_COMMIT_TAG" && c.op == "set":
				push().Tags = append(push().Tags, "*")
				matched = true
			case c.varName == "CI_COMMIT_TAG" && c.op == "=~":
				if m := gitlabRegexLit.FindStringSubmatch(c.value); m != nil {
					if g, ok := ir.RegexToGlob(m[1]); ok {
						push().Tags = append(push().Tags, g)
						matched = true
					}
				}
			case c.varName == "CI_MERGE_REQUEST_TARGET_BRANCH_NAME":
				if b, ok := branchValue(c); ok {
					if t.PullRequest == nil {
						t.PullRequest = &ir.PRTrigger{}
					}
					t.PullRequest.Branches = append(t.PullRequest.Branches, b)
					matched = true
				}
			}
		}
		if !matched {
			ps.d.Warnf(path, "workflow rule %q could not be translated", expr)
		}
		if ch := yamlx.Get(r, "changes"); ch != nil && matched {
			paths := yamlx.StrList(ch)
			if yamlx.IsMap(ch) {
				paths = yamlx.StrList(yamlx.Get(ch, "paths"))
			}
			if isPR {
				t.PullRequest.Paths = ir.SortedUnique(append(t.PullRequest.Paths, paths...))
			} else if t.Push != nil {
				t.Push.Paths = ir.SortedUnique(append(t.Push.Paths, paths...))
			}
		}
	}
	if t.Empty() {
		t.Push = &ir.PushTrigger{}
	}
	return t
}

// parseJobCondition merges rules/only/except/when into one condition.
func (ps *parser) parseJobCondition(n *yaml.Node, path string) *ir.Condition {
	c := &ir.Condition{}
	switch yamlx.Str(yamlx.Get(n, "when")) {
	case "manual":
		c.Manual = true
	case "always":
		c.When = ir.WhenAlways
	case "on_failure":
		c.When = ir.WhenOnFailure
	case "delayed":
		ps.d.Warnf(path+".when", "delayed jobs are not portable; the delay was dropped")
	case "never":
		ps.d.Warnf(path+".when", "job is disabled (when: never)")
	}
	if rules := yamlx.Get(n, "rules"); rules != nil {
		ps.applyRules(c, rules, path)
	}
	if only := yamlx.Get(n, "only"); only != nil {
		refs := yamlx.StrList(only)
		if yamlx.IsMap(only) {
			refs = yamlx.StrList(yamlx.Get(only, "refs"))
			if yamlx.Has(only, "changes") || yamlx.Has(only, "variables") {
				ps.d.Warnf(path+".only", "only:changes/variables are not portable and were dropped")
			}
		}
		for _, r := range refs {
			switch r {
			case "branches", "pushes":
			case "tags":
				c.Event = ir.EventTag
			case "merge_requests", "external_pull_requests":
				c.Event = ir.EventPullRequest
			case "schedules":
				c.Event = ir.EventSchedule
			case "web", "api", "triggers":
				c.Event = ir.EventManual
			default:
				if m := gitlabRegexLit.FindStringSubmatch(r); m != nil {
					if g, ok := ir.RegexToGlob(m[1]); ok {
						c.Branches = append(c.Branches, g)
						continue
					}
					ps.d.Warnf(path+".only", "branch regex %s could not be translated", r)
					continue
				}
				c.Branches = append(c.Branches, r)
			}
		}
	}
	if yamlx.Has(n, "except") {
		ps.d.Warnf(path+".except", "except: filters are not portable and were dropped")
	}
	if c.IsZero() {
		return nil
	}
	return c
}

func (ps *parser) applyRules(c *ir.Condition, rules *yaml.Node, path string) {
	items := yamlx.Items(rules)
	var positive []*yaml.Node
	for _, r := range items {
		if yamlx.Str(yamlx.Get(r, "when")) == "never" {
			ps.d.Warnf(path+".rules", "exclusion rules (when: never) are not portable and were dropped")
			continue
		}
		positive = append(positive, r)
	}
	for i, r := range positive {
		where := fmt.Sprintf("%s.rules[%d]", path, i)
		if yamlx.Has(r, "changes") || yamlx.Has(r, "exists") {
			ps.d.Warnf(where, "rules:changes/exists are not portable and were dropped")
		}
		switch yamlx.Str(yamlx.Get(r, "when")) {
		case "manual":
			c.Manual = true
		case "always":
			c.When = ir.WhenAlways
		case "on_failure":
			c.When = ir.WhenOnFailure
		}
		if v, ok := yamlx.Bool(yamlx.Get(r, "allow_failure")); ok && v {
			ps.d.Infof(where, "rule-level allow_failure applies to the whole job")
		}
		expr := yamlx.Str(yamlx.Get(r, "if"))
		if expr == "" {
			continue
		}
		clauses, ok := parseClauses(expr)
		if !ok {
			c.Raw, c.RawFrom = expr, "gitlab"
			ps.d.Warnf(where, "rule %q could not be translated", expr)
			continue
		}
		for _, cl := range clauses {
			switch {
			case cl.varName == "CI_COMMIT_BRANCH" || cl.varName == "CI_COMMIT_REF_NAME":
				if cl.op == "set" {
					continue
				}
				if b, ok := branchValue(cl); ok && cl.op != "!=" && cl.op != "!~" {
					c.Branches = append(c.Branches, b)
					continue
				}
				c.Raw, c.RawFrom = expr, "gitlab"
			case cl.varName == "CI_COMMIT_TAG" && cl.op == "set":
				c.Event = ir.EventTag
			case cl.varName == "CI_PIPELINE_SOURCE" && cl.op == "==":
				switch cl.value {
				case "merge_request_event", "external_pull_request_event":
					c.Event = ir.EventPullRequest
				case "schedule":
					c.Event = ir.EventSchedule
				case "push":
					c.Event = ir.EventPush
				case "web", "api", "trigger":
					c.Event = ir.EventManual
				default:
					c.Raw, c.RawFrom = expr, "gitlab"
				}
			default:
				c.Raw, c.RawFrom = expr, "gitlab"
			}
		}
		if c.Raw != "" {
			ps.d.Warnf(where, "rule %q could not be translated", expr)
		}
	}
	if len(positive) > 1 && c.Event != "" && len(c.Branches) > 0 {
		ps.d.Warnf(path+".rules", "several rules were combined; review the converted condition")
	}
}
