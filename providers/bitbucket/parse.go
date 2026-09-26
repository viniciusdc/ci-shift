package bitbucket

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// Pipeline section kinds under the top-level "pipelines" key.
const (
	kindDefault  = "default"
	kindBranches = "branches"
	kindTags     = "tags"
	kindPR       = "pull-requests"
	kindCustom   = "custom"
)

// sectionSpec is one pipeline of the file (e.g. pipelines.branches.main).
type sectionSpec struct {
	kind    string
	pattern string
	items   *yaml.Node
	path    string
	prefix  string // job ID prefix when the file has several sections
}

type parser struct {
	d          *ir.Diagnostics
	services   *yaml.Node // definitions.services
	caches     *yaml.Node // definitions.caches
	image      string     // global image
	timeout    int        // options.max-time
	runner     ir.Runner  // options.runs-on
	clone      ir.Checkout
	cloneOff   bool // clone.enabled: false
	taken      map[string]bool
	noDownload map[string]bool // steps with artifacts.download: false
	jobs       []*ir.Job
	changes    []changeset // per job, in job order
}

// changeset is a step's condition.changesets, kept to be folded into the
// trigger path filters when every step of the matching sections agrees.
type changeset struct {
	kind    string // section kind of the step
	path    string
	include []string
	exclude []string
	set     bool
}

// Parse implements ir.Provider.
func (Provider) Parse(data []byte, d *ir.Diagnostics) (*ir.Pipeline, error) {
	root, err := yamlx.Load(data)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: %w", err)
	}
	if !yamlx.IsMap(root) {
		return nil, fmt.Errorf("bitbucket: configuration must be a mapping")
	}
	pipelines := yamlx.Get(root, "pipelines")
	if !yamlx.IsMap(pipelines) {
		return nil, fmt.Errorf("bitbucket: configuration has no pipelines")
	}
	ps := &parser{
		d:          d,
		services:   yamlx.Path(root, "definitions", "services"),
		caches:     yamlx.Path(root, "definitions", "caches"),
		taken:      map[string]bool{},
		noDownload: map[string]bool{},
	}
	if img := yamlx.Get(root, "image"); img != nil {
		ps.image = ps.parseImage(img, "image")
	}
	opts := yamlx.Get(root, "options")
	if v, ok := yamlx.Int(yamlx.Get(opts, "max-time")); ok {
		ps.timeout = v
	}
	if ro := yamlx.Get(opts, "runs-on"); ro != nil {
		ps.runner = parseRunsOn(ro)
	}
	if yamlx.Has(opts, "docker") {
		d.Infof("options.docker", "the Docker daemon option was dropped; other providers need a docker-in-docker service or a machine executor")
	}
	if yamlx.Has(opts, "size") {
		d.Infof("options.size", "step size %s was dropped; pick a runner size on the target provider", yamlx.Str(yamlx.Get(opts, "size")))
	}
	if yamlx.Has(opts, "runtime") {
		d.Infof("options.runtime", "runtime options were dropped")
	}
	ps.applyClone(yamlx.Get(root, "clone"), &ps.clone, &ps.cloneOff, "clone")
	for _, key := range []string{"export", "labels", "triggers"} {
		if yamlx.Has(root, key) {
			d.Warnf(key, "%q has no portable equivalent and was dropped", key)
		}
	}
	defs := yamlx.Get(root, "definitions")
	for _, key := range []string{"pipelines", "imports", "agents"} {
		if yamlx.Has(defs, key) {
			d.Warnf("definitions."+key, "shared %s are not portable and were dropped", key)
		}
	}

	var secs []*sectionSpec
	for _, pair := range yamlx.Pairs(pipelines) {
		switch pair.Key {
		case kindDefault:
			secs = append(secs, &sectionSpec{kind: kindDefault, items: pair.Value, path: "pipelines.default", prefix: "default"})
		case kindBranches, kindTags, kindPR, kindCustom:
			for _, sub := range yamlx.Pairs(pair.Value) {
				s := &sectionSpec{kind: pair.Key, pattern: sub.Key, items: sub.Value, path: "pipelines." + pair.Key + "." + sub.Key}
				s.prefix = sectionPrefix(s)
				secs = append(secs, s)
			}
		default:
			d.Warnf("pipelines."+pair.Key, "unknown pipeline section %q was dropped", pair.Key)
		}
	}
	if len(secs) == 0 {
		return nil, fmt.Errorf("bitbucket: configuration has no pipelines")
	}

	p := &ir.Pipeline{}
	multi := len(secs) > 1
	p.Triggers = ps.triggers(secs, multi)
	for _, s := range secs {
		ps.parseSection(s, multi)
	}
	p.Jobs = ps.jobs
	ps.foldChangesets(&p.Triggers)
	addArtifactDownloads(p, ps.noDownload)
	ir.PromoteExternalVars(p, d, builtinPrefixes...)
	return p, nil
}

// sectionPrefix names a section for job IDs, e.g. "main", "tag-v", "pr".
func sectionPrefix(s *sectionSpec) string {
	var parts []string
	for _, alt := range expandBraces(s.pattern) {
		if alt = strings.Trim(ir.Slug(strings.NewReplacer("*", "", "?", "").Replace(alt)), "-"); alt != "job" && alt != "" {
			parts = append(parts, alt)
		}
	}
	pat := strings.Join(parts, "-")
	switch s.kind {
	case kindTags:
		return strings.TrimSuffix("tag-"+pat, "-")
	case kindPR:
		return strings.TrimSuffix("pr-"+pat, "-")
	}
	if pat == "" {
		return s.kind
	}
	return pat
}

// triggers derives the pipeline triggers from the sections present.
func (ps *parser) triggers(secs []*sectionSpec, multi bool) ir.Triggers {
	var t ir.Triggers
	d := ps.d
	push := func() *ir.PushTrigger {
		if t.Push == nil {
			t.Push = &ir.PushTrigger{}
		}
		return t.Push
	}
	hasDefault := false
	var customs []string
	for _, s := range secs {
		switch s.kind {
		case kindDefault:
			hasDefault = true
			push()
			if multi {
				d.Infof(s.path, "the default pipeline only runs for pushes without a matching branch pipeline; its jobs were given a push condition")
			}
		case kindBranches:
			push().Branches = appendUnique(push().Branches, expandBraces(s.pattern)...)
		case kindTags:
			push().Tags = appendUnique(push().Tags, expandBraces(s.pattern)...)
		case kindPR:
			if t.PullRequest == nil {
				t.PullRequest = &ir.PRTrigger{}
			}
			if s.pattern != "**" {
				d.Infof(s.path, "pull request pipelines filter on the source branch (%q); the filter was dropped", s.pattern)
			}
		case kindCustom:
			customs = append(customs, s.pattern)
			if s.pattern == "scheduled" {
				// Bitbucket keeps the cron in Repository settings > Pipelines >
				// Schedules, so the schedule has no cron in the IR.
				t.Schedules = append(t.Schedules, ir.Schedule{})
			} else {
				t.Manual = true
			}
		}
	}
	if len(customs) > 0 {
		d.Warnf("pipelines.custom", "custom pipelines run manually or from schedules configured in Repository settings > Pipelines > Schedules; schedules are not stored in the file and were not converted")
	}
	if len(customs) > 1 {
		d.Infof("pipelines.custom", "custom pipelines %s all map to the manual trigger", strings.Join(customs, ", "))
	}
	if hasDefault {
		// "default" covers every branch; keep branch pushes enabled when tag
		// filters would otherwise restrict the trigger to tags.
		t.Push.Branches = nil
		if len(t.Push.Tags) > 0 {
			t.Push.Branches = []string{"**"}
		}
	}
	return t
}

// foldChangesets turns step changeset conditions into trigger path
// filters when every step of the push (default/branches) or pull request
// pipelines carries the same condition; others are reported and dropped.
func (ps *parser) foldChangesets(t *ir.Triggers) {
	fold := func(kinds []string, apply func(cs changeset) bool) {
		var group []changeset
		for _, cs := range ps.changes {
			if contains(kinds, cs.kind) {
				group = append(group, cs)
			}
		}
		if len(group) == 0 {
			return
		}
		same := group[0].set
		for _, cs := range group[1:] {
			if !reflect.DeepEqual(cs.include, group[0].include) || !reflect.DeepEqual(cs.exclude, group[0].exclude) {
				same = false
			}
		}
		if same && apply(group[0]) {
			return
		}
		for _, cs := range group {
			if cs.set {
				ps.d.Warnf(cs.path, "changeset conditions differ between steps and were dropped; the step always runs")
			}
		}
	}
	fold([]string{kindDefault, kindBranches}, func(cs changeset) bool {
		if t.Push == nil {
			return false
		}
		t.Push.Paths, t.Push.PathsIgnore = cs.include, cs.exclude
		return true
	})
	fold([]string{kindPR}, func(cs changeset) bool {
		if t.PullRequest == nil || len(cs.exclude) > 0 {
			return false
		}
		t.PullRequest.Paths = cs.include
		return true
	})
	fold([]string{kindTags, kindCustom}, func(changeset) bool { return false })
}

// condition returns the job condition implied by the section.
func (s *sectionSpec) condition() *ir.Condition {
	switch s.kind {
	case kindDefault:
		return &ir.Condition{Event: ir.EventPush}
	case kindBranches:
		return &ir.Condition{Branches: expandBraces(s.pattern)}
	case kindTags:
		return &ir.Condition{Event: ir.EventTag}
	case kindPR:
		return &ir.Condition{Event: ir.EventPullRequest}
	case kindCustom:
		if s.pattern == "scheduled" {
			return &ir.Condition{Event: ir.EventSchedule}
		}
		return &ir.Condition{Event: ir.EventManual}
	}
	return nil
}

// parseSection turns a pipeline's item list into jobs. Items run in
// sequence: every job of an item needs every job of the previous item.
func (ps *parser) parseSection(s *sectionSpec, multi bool) {
	d := ps.d
	if !yamlx.IsSeq(s.items) {
		if yamlx.Has(s.items, "import") {
			d.Warnf(s.path, "imported pipelines are not resolved; convert the source repository separately")
		} else {
			d.Warnf(s.path, "pipeline must be a list of steps; it was dropped")
		}
		return
	}
	var prev []string
	num := 0
	add := func(n *yaml.Node, where string, needs []string) *ir.Job {
		num++
		j := ps.parseStep(n, s, multi, num, where)
		if len(needs) > 0 {
			j.Needs = append([]string(nil), needs...)
		}
		ps.jobs = append(ps.jobs, j)
		return j
	}
	for i, item := range yamlx.Items(s.items) {
		where := fmt.Sprintf("%s[%d]", s.path, i)
		switch {
		case yamlx.Has(item, "step"):
			j := add(yamlx.Get(item, "step"), where+".step", prev)
			prev = []string{j.ID}
		case yamlx.Has(item, "parallel"):
			pn := yamlx.Get(item, "parallel")
			steps := pn
			if yamlx.IsMap(pn) {
				steps = yamlx.Get(pn, "steps")
				if yamlx.Has(pn, "fail-fast") {
					d.Infof(where+".parallel.fail-fast", "fail-fast was dropped")
				}
			}
			var ids []string
			for k, si := range yamlx.Items(steps) {
				st := yamlx.Get(si, "step")
				if st == nil {
					continue
				}
				ids = append(ids, add(st, fmt.Sprintf("%s.parallel[%d].step", where, k), prev).ID)
			}
			if len(ids) > 0 {
				prev = ids
			}
		case yamlx.Has(item, "stage"):
			sn := yamlx.Get(item, "stage")
			name := yamlx.Str(yamlx.Get(sn, "name"))
			deployment := yamlx.Str(yamlx.Get(sn, "deployment"))
			if yamlx.Has(sn, "condition") {
				d.Warnf(where+".stage.condition", "stage conditions are not portable and were dropped")
			}
			for k, si := range yamlx.Items(yamlx.Get(sn, "steps")) {
				st := yamlx.Get(si, "step")
				if st == nil {
					continue
				}
				j := add(st, fmt.Sprintf("%s.stage.steps[%d].step", where, k), prev)
				j.Stage = name
				if j.Environment == "" {
					j.Environment = deployment
				}
				if k == 0 && yamlx.Str(yamlx.Get(sn, "trigger")) == "manual" {
					if j.If == nil {
						j.If = &ir.Condition{}
					}
					j.If.Manual = true
				}
				prev = []string{j.ID}
			}
		case yamlx.Has(item, "variables"):
			d.Warnf(where+".variables", "custom pipeline variables are prompted at run time and were dropped")
		case yamlx.Has(item, "final"):
			j := add(yamlx.Get(item, "final"), where+".final", prev)
			if j.If == nil {
				j.If = &ir.Condition{}
			}
			j.If.When = ir.WhenAlways
			prev = []string{j.ID}
		default:
			d.Warnf(where, "unknown pipeline item was dropped")
		}
	}
}

func (ps *parser) parseImage(n *yaml.Node, path string) string {
	if !yamlx.IsMap(n) {
		return fromBB(yamlx.Str(n), false)
	}
	if yamlx.Has(n, "username") || yamlx.Has(n, "password") || yamlx.Has(n, "aws") {
		ps.d.Warnf(path, "private registry credentials were dropped; configure registry access on the target provider")
	}
	if yamlx.Has(n, "run-as-user") {
		ps.d.Infof(path+".run-as-user", "run-as-user was dropped")
	}
	return fromBB(yamlx.Str(yamlx.Get(n, "name")), false)
}

// parseRunsOn maps runner labels: windows/macos select the OS, the
// implicit self.hosted label is dropped and everything else is kept.
func parseRunsOn(n *yaml.Node) ir.Runner {
	var r ir.Runner
	for _, l := range yamlx.StrList(n) {
		switch strings.ToLower(l) {
		case "self.hosted":
		case "windows":
			r.OS = ir.OSWindows
		case "macos":
			r.OS = ir.OSMacOS
		default:
			r.Labels = append(r.Labels, l)
		}
	}
	return r
}

// applyClone reads clone options into co.
func (ps *parser) applyClone(n *yaml.Node, co *ir.Checkout, off *bool, path string) {
	if n == nil {
		return
	}
	if v, ok := yamlx.Bool(yamlx.Get(n, "enabled")); ok {
		*off = !v
	}
	if dn := yamlx.Get(n, "depth"); dn != nil {
		if yamlx.Str(dn) == "full" {
			co.Depth = -1
		} else if v, ok := yamlx.Int(dn); ok {
			co.Depth = v
		}
	}
	if v, ok := yamlx.Bool(yamlx.Get(n, "lfs")); ok {
		co.LFS = v
	}
	if yamlx.Has(n, "skip-ssl-verify") {
		ps.d.Infof(path+".skip-ssl-verify", "skip-ssl-verify was dropped")
	}
}

func (ps *parser) parseStep(n *yaml.Node, s *sectionSpec, multi bool, num int, path string) *ir.Job {
	d := ps.d
	name := yamlx.Str(yamlx.Get(n, "name"))
	base := ir.Slug(name)
	if name == "" {
		base = fmt.Sprintf("step-%d", num)
	}
	if multi {
		base = s.prefix + "-" + base
	}
	j := &ir.Job{ID: ir.UniqueID(base, ps.taken), Name: name, Image: ps.image, Runner: ps.runner, TimeoutMinutes: ps.timeout}
	if img := yamlx.Get(n, "image"); img != nil {
		j.Image = ps.parseImage(img, path+".image")
	}
	if ro := yamlx.Get(n, "runs-on"); ro != nil {
		j.Runner = parseRunsOn(ro)
	}
	if v, ok := yamlx.Int(yamlx.Get(n, "max-time")); ok {
		j.TimeoutMinutes = v
	}
	j.Environment = yamlx.Str(yamlx.Get(n, "deployment"))

	var cond *ir.Condition
	if multi {
		cond = s.condition()
	}
	if yamlx.Str(yamlx.Get(n, "trigger")) == "manual" {
		if cond == nil {
			cond = &ir.Condition{}
		}
		cond.Manual = true
	}
	if !cond.IsZero() {
		j.If = cond
	}
	cs := changeset{kind: s.kind, path: path + ".condition"}
	if c := yamlx.Get(n, "condition"); c != nil {
		ch := yamlx.Get(c, "changesets")
		cs.include = yamlx.StrList(yamlx.Get(ch, "includePaths"))
		cs.exclude = yamlx.StrList(yamlx.Get(ch, "excludePaths"))
		cs.set = len(cs.include)+len(cs.exclude) > 0
		if yamlx.Has(c, "state") {
			d.Warnf(path+".condition.state", "state conditions are not portable and were dropped; the step always runs")
		}
	}
	ps.changes = append(ps.changes, cs)
	for _, k := range []string{"size", "oidc", "fail-fast", "runtime", "concurrency-group", "input-variables", "auth", "environment", "type", "custom"} {
		if yamlx.Has(n, k) {
			d.Infof(path+"."+k, "%q has no portable equivalent and was dropped", k)
		}
	}

	for _, sn := range yamlx.StrList(yamlx.Get(n, "services")) {
		def := yamlx.Get(ps.services, sn)
		if sn == "docker" || yamlx.Str(yamlx.Get(def, "type")) == "docker" {
			d.Warnf(path+".services", "the Docker service %q (docker-in-docker) was dropped; enable Docker on the target provider", sn)
			continue
		}
		if def == nil {
			d.Warnf(path+".services", "service %q is not defined in definitions.services and was dropped", sn)
			continue
		}
		svc := ir.Service{Name: sn, Image: ps.parseImage(yamlx.Get(def, "image"), "definitions.services."+sn+".image")}
		for _, pair := range yamlx.Pairs(yamlx.Get(def, "variables")) {
			svc.Env = append(svc.Env, ir.KV{Key: pair.Key, Value: fromBB(yamlx.Str(pair.Value), false)})
		}
		if yamlx.Has(def, "memory") {
			d.Infof("definitions.services."+sn+".memory", "service memory limits were dropped")
		}
		j.Services = append(j.Services, svc)
	}

	// Steps: implicit clone, caches, script, after-script, artifacts.
	co, off := ps.clone, ps.cloneOff
	ps.applyClone(yamlx.Get(n, "clone"), &co, &off, path+".clone")
	if !off {
		j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepCheckout, Checkout: &co})
	}
	for _, cn := range yamlx.StrList(yamlx.Get(n, "caches")) {
		if c := ps.cache(cn, path+".caches"); c != nil {
			j.Steps = append(j.Steps, &ir.Step{Kind: ir.StepCache, Cache: c})
		}
	}
	if !yamlx.Has(n, "script") {
		d.Warnf(path, "step has no script")
	}
	j.Steps = append(j.Steps, ps.parseScript(yamlx.Get(n, "script"), path+".script")...)
	for _, st := range ps.parseScript(yamlx.Get(n, "after-script"), path+".after-script") {
		st.If = &ir.Condition{When: ir.WhenAlways}
		st.AllowFailure = true
		j.Steps = append(j.Steps, st)
	}
	j.Steps = append(j.Steps, ps.parseArtifacts(j, yamlx.Get(n, "artifacts"), path+".artifacts")...)
	return j
}

// cache resolves a cache name against definitions.caches and the
// predefined caches.
func (ps *parser) cache(name, path string) *ir.Cache {
	if def := yamlx.Get(ps.caches, name); def != nil {
		c := &ir.Cache{Prefix: name}
		if yamlx.IsScalar(def) {
			c.Paths = []string{yamlx.Str(def)}
		} else {
			c.Paths = []string{yamlx.Str(yamlx.Get(def, "path"))}
			c.Files = yamlx.StrList(yamlx.Path(def, "key", "files"))
		}
		return c
	}
	if name == "docker" {
		ps.d.Warnf(path, "the docker layer cache has no portable equivalent and was dropped")
		return nil
	}
	if p, ok := predefinedCaches[name]; ok {
		return &ir.Cache{Prefix: name, Paths: []string{p}}
	}
	ps.d.Warnf(path, "cache %q is not defined in definitions.caches and was dropped", name)
	return nil
}

// parseScript turns script items into steps: consecutive one-line
// commands are grouped into one run step, any other item (multi-line
// blocks, compound one-liners) is a step of its own, and pipes become
// action steps in between. This mirrors how the emitter splits steps, so
// converting a file twice gives the same result.
func (ps *parser) parseScript(n *yaml.Node, path string) []*ir.Step {
	var steps []*ir.Step
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			steps = append(steps, &ir.Step{Kind: ir.StepRun, Run: strings.Join(buf, "\n")})
			buf = nil
		}
	}
	for i, item := range yamlx.Items(n) {
		where := fmt.Sprintf("%s[%d]", path, i)
		if yamlx.IsScalar(item) {
			raw := strings.TrimRight(yamlx.Str(item), "\n")
			cmd := fromBB(raw, true)
			if _, ok := splitCommands(raw); ok && !strings.Contains(raw, "\n") {
				buf = append(buf, cmd)
				continue
			}
			flush()
			steps = append(steps, &ir.Step{Kind: ir.StepRun, Run: cmd})
			continue
		}
		pipe := yamlx.Str(yamlx.Get(item, "pipe"))
		if pipe == "" {
			ps.d.Warnf(where, "script item is neither a command nor a pipe and was dropped")
			continue
		}
		flush()
		st := &ir.Step{Kind: ir.StepAction, Uses: pipe, UsesFrom: "bitbucket"}
		for _, pair := range yamlx.Pairs(yamlx.Get(item, "variables")) {
			v := yamlx.Str(pair.Value)
			if yamlx.IsSeq(pair.Value) {
				v = strings.Join(yamlx.StrList(pair.Value), ",")
				ps.d.Infof(where+".variables."+pair.Key, "list value was joined with commas")
			}
			st.With = append(st.With, ir.KV{Key: pair.Key, Value: pipeValue(v)})
		}
		ps.d.Warnf(where, "Bitbucket pipe %q only exists on Bitbucket Pipelines", pipe)
		steps = append(steps, st)
	}
	flush()
	return steps
}

// pipeValue converts a pipe variable. Pipe variables almost always refer to
// repository variables, so non-builtin references become secrets.
func pipeValue(v string) string {
	v = shellVars.FromShell(v)
	return ir.ReplaceShellVars(v, func(name string) (string, bool) {
		for _, pre := range builtinPrefixes {
			if strings.HasPrefix(name, pre) {
				return "", false
			}
		}
		return ir.Ref(ir.RefSecret, name), true
	})
}

func (ps *parser) parseArtifacts(j *ir.Job, n *yaml.Node, path string) []*ir.Step {
	if n == nil {
		return nil
	}
	var arts []*ir.Artifact
	paths := yamlx.StrList(n)
	if yamlx.IsMap(n) {
		paths = yamlx.StrList(yamlx.Get(n, "paths"))
		dl := yamlx.Get(n, "download")
		if v, ok := yamlx.Bool(dl); ok && !v {
			ps.noDownload[j.ID] = true
		} else if yamlx.IsSeq(dl) {
			ps.d.Infof(path+".download", "selective artifact downloads were widened to every upstream artifact")
		}
		for _, u := range yamlx.Items(yamlx.Get(n, "upload")) {
			if yamlx.IsScalar(u) {
				paths = append(paths, yamlx.Str(u))
				continue
			}
			a := &ir.Artifact{Name: yamlx.Str(yamlx.Get(u, "name")), Paths: yamlx.StrList(yamlx.Get(u, "paths"))}
			switch yamlx.Str(yamlx.Get(u, "capture-on")) {
			case "always":
				a.When = ir.WhenAlways
			case "failed":
				a.When = ir.WhenOnFailure
			}
			if yamlx.Str(yamlx.Get(u, "type")) == "test-reports" {
				ps.d.Infof(path+".upload", "test report artifacts were kept as plain artifacts")
			}
			if len(a.Paths) > 0 {
				arts = append(arts, a)
			}
		}
	}
	if len(paths) > 0 {
		arts = append([]*ir.Artifact{{Paths: paths}}, arts...)
	}
	var steps []*ir.Step
	for _, a := range arts {
		steps = append(steps, &ir.Step{Kind: ir.StepUpload, Artifact: a})
	}
	return steps
}

// addArtifactDownloads makes Bitbucket's implicit artifact passing explicit:
// a step downloads the artifacts of every step it (transitively) follows,
// unless it opted out with artifacts.download: false.
func addArtifactDownloads(p *ir.Pipeline, skip map[string]bool) {
	uploads := map[string]bool{}
	for _, j := range p.Jobs {
		for _, st := range j.Steps {
			if st.Kind == ir.StepUpload {
				uploads[j.ID] = true
			}
		}
	}
	for _, j := range p.Jobs {
		if skip[j.ID] {
			continue
		}
		has := false
		for _, up := range upstream(p, j) {
			if uploads[up.ID] {
				has = true
			}
		}
		if !has {
			continue
		}
		// Insert after checkout/cache steps.
		idx := 0
		for idx < len(j.Steps) && (j.Steps[idx].Kind == ir.StepCheckout || j.Steps[idx].Kind == ir.StepCache) {
			idx++
		}
		dl := &ir.Step{Kind: ir.StepDownload, Artifact: &ir.Artifact{}}
		j.Steps = append(j.Steps[:idx], append([]*ir.Step{dl}, j.Steps[idx:]...)...)
	}
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
