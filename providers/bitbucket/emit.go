package bitbucket

import (
	"fmt"
	"strings"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

// defaultImage is Bitbucket's own default build image, set explicitly when
// some jobs name no image so the result does not depend on account defaults.
const defaultImage = "atlassian/default-image:4"

// jobInfo holds per-job values computed once before emitting.
type jobInfo struct {
	image  string
	steps  []*ir.Step // after ir.FoldTools
	clone  bool       // the job checks out the repository
	co     ir.Checkout
	level  int
	upload bool
}

// section is one emitted pipeline (e.g. pipelines.branches.main).
type section struct {
	kind    string
	pattern string
	jobs    map[string]bool
	include []string // condition.changesets.includePaths
	exclude []string // condition.changesets.excludePaths
}

type emitter struct {
	p      *ir.Pipeline
	d      *ir.Diagnostics
	levels [][]*ir.Job
	info   map[string]*jobInfo

	topImage   string
	topTimeout int
	topDepth   int
	topLFS     bool

	services   *yamlx.Map
	serviceIDs map[string]string // definition key -> service name
	caches     *yamlx.Map
	cacheIDs   map[string]string // definition key -> cache name
	taken      map[string]bool   // used service/cache names
}

// Emit implements ir.Provider.
func (Provider) Emit(p *ir.Pipeline, d *ir.Diagnostics) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	levels, err := p.Levels()
	if err != nil {
		return nil, err
	}
	e := &emitter{
		p: p, d: d, levels: levels, info: map[string]*jobInfo{},
		services: yamlx.NewMap(), serviceIDs: map[string]string{},
		caches: yamlx.NewMap(), cacheIDs: map[string]string{},
		taken: map[string]bool{"docker": true},
	}
	for _, n := range predefinedOrder {
		e.taken[n] = true
	}
	e.prepare()
	pipelines := e.emitPipelines()

	root := yamlx.NewMap()
	root.SetIf("image", toBB(e.topImage, d, "image"))
	clone := yamlx.NewMap()
	switch {
	case e.topDepth < 0:
		clone.Set("depth", "full")
	case e.topDepth > 0:
		clone.Set("depth", e.topDepth)
	}
	clone.SetIf("lfs", e.topLFS)
	root.SetIf("clone", clone)
	if e.topTimeout > 0 {
		root.Set("options", yamlx.NewMap().Set("max-time", e.topTimeout))
	}
	defs := yamlx.NewMap()
	defs.SetIf("caches", e.caches)
	defs.SetIf("services", e.services)
	root.SetIf("definitions", defs)
	root.Set("pipelines", pipelines)

	if p.Name != "" {
		d.Infof("", "Bitbucket pipelines have no name; %q was dropped", p.Name)
	}
	if secrets := p.Secrets(); len(secrets) > 0 {
		d.Infof("", "define these as secured repository variables (Repository settings > Repository variables): %s", strings.Join(secrets, ", "))
	}
	return yamlx.Marshal(root, "")
}

// prepare folds tool steps and picks the values shared by every job, which
// are emitted once at the top level.
func (e *emitter) prepare() {
	images := map[string]bool{}
	timeouts := map[int]bool{}
	depths := map[int]bool{}
	lfs := map[bool]bool{}
	for lvl, lv := range e.levels {
		for _, j := range lv {
			img, steps := ir.FoldTools(j, e.d, j.ID)
			in := &jobInfo{image: img, steps: steps, level: lvl}
			for _, st := range steps {
				switch st.Kind {
				case ir.StepCheckout:
					if !in.clone && st.Checkout != nil {
						in.co = *st.Checkout
					}
					in.clone = true
				case ir.StepUpload:
					in.upload = true
				}
			}
			e.info[j.ID] = in
			// Images with matrix values differ per expanded step.
			if len(ir.Refs(img, ir.RefMatrix)) > 0 {
				img = "\x00matrix"
			}
			images[img] = true
			timeouts[j.TimeoutMinutes] = true
			if in.clone {
				depths[in.co.Depth] = true
				lfs[in.co.LFS] = true
			}
		}
	}
	switch {
	case images[""]:
		e.topImage = defaultImage
	case len(images) == 1 && !images["\x00matrix"]:
		for img := range images {
			e.topImage = img
		}
	}
	if len(timeouts) == 1 {
		for t := range timeouts {
			e.topTimeout = t
		}
	}
	if len(depths) == 1 {
		for dp := range depths {
			e.topDepth = dp
		}
	}
	e.topLFS = len(lfs) == 1 && lfs[true]
}

// checkLevels reports when serializing the needs graph makes a job wait
// for jobs of the previous item it does not depend on.
func (e *emitter) checkLevels(path string, prev []string, cur []*ir.Job) {
	for _, j := range cur {
		up := map[string]bool{}
		for _, u := range upstream(e.p, j) {
			up[u.ID] = true
		}
		for _, id := range prev {
			if !up[id] {
				e.d.Infof("pipelines", "Bitbucket runs steps in sequence, so jobs were grouped by dependency level; some now wait for jobs they do not need (e.g. %s waits for %s)", j.ID, id)
				return
			}
		}
	}
}

// sections derives the pipelines to emit from the triggers and from the
// jobs' branch/event conditions, and places every job.
func (e *emitter) sections() []*section {
	t := e.p.Triggers
	d := e.d
	hasDefault := t.Empty()
	var branchPats, tagPats []string
	pr := t.PullRequest != nil
	manual, scheduled := t.Manual, len(t.Schedules) > 0
	if push := t.Push; push != nil {
		for _, b := range push.Branches {
			if b == "**" {
				hasDefault = true
				continue
			}
			branchPats = appendUnique(branchPats, b)
		}
		if len(push.Branches) == 0 && len(push.Tags) == 0 {
			hasDefault = true
		}
		tagPats = appendUnique(tagPats, push.Tags...)
		if len(push.BranchesIgnore) > 0 {
			d.Warnf("pipelines", "branch ignore filters %v have no Bitbucket equivalent and were dropped", push.BranchesIgnore)
		}
	}
	pushJobs := false
	for _, j := range e.p.Jobs {
		c := j.If
		if c == nil {
			continue
		}
		switch c.Event {
		case ir.EventTag:
			if len(tagPats) == 0 {
				tagPats = []string{"*"}
			}
		case ir.EventPullRequest:
			pr = true
		case ir.EventManual:
			manual = true
		case ir.EventSchedule:
			scheduled = true
		case ir.EventPush, "":
			for _, b := range c.Branches {
				if b == "**" {
					hasDefault = true
				} else {
					branchPats = appendUnique(branchPats, b)
				}
			}
			if c.Event == ir.EventPush && len(c.Branches) == 0 {
				pushJobs = true
			}
		}
	}
	if pushJobs && !hasDefault && len(branchPats) == 0 {
		hasDefault = true
	}

	var secs []*section
	newSec := func(kind, pattern string) *section {
		s := &section{kind: kind, pattern: pattern, jobs: map[string]bool{}}
		for _, j := range e.p.Jobs {
			if placed(j, kind, pattern, hasDefault) {
				s.jobs[j.ID] = true
			}
		}
		return s
	}
	var pushPaths, pushIgnore []string
	if t.Push != nil {
		pushPaths, pushIgnore = t.Push.Paths, t.Push.PathsIgnore
	}
	if hasDefault {
		s := newSec(kindDefault, "")
		s.include, s.exclude = pushPaths, pushIgnore
		secs = append(secs, s)
	}
	for _, s := range e.mergeBranches(branchPats, newSec) {
		s.include, s.exclude = pushPaths, pushIgnore
		secs = append(secs, s)
	}
	for _, tag := range tagPats {
		secs = append(secs, newSec(kindTags, tag))
	}
	if pr {
		s := newSec(kindPR, "**")
		if t.PullRequest != nil {
			s.include = t.PullRequest.Paths
			if len(t.PullRequest.Branches) > 0 {
				d.Infof("pipelines.pull-requests", "Bitbucket cannot filter pull requests by target branch; %v was dropped and the pipeline runs for every pull request", t.PullRequest.Branches)
			}
		}
		secs = append(secs, s)
	}
	if manual {
		secs = append(secs, newSec(kindCustom, "manual"))
	}
	if scheduled {
		var crons []string
		for _, s := range t.Schedules {
			crons = append(crons, s.Cron)
		}
		if len(crons) > 0 {
			d.Warnf("pipelines.custom.scheduled", "create a schedule for the custom pipeline \"scheduled\" in Repository settings > Pipelines > Schedules with cron %s (UTC)", strings.Join(crons, ", "))
		}
		secs = append(secs, newSec(kindCustom, "scheduled"))
	}
	if t.Push != nil && len(pushPaths) > 0 && len(pushIgnore) > 0 {
		d.Warnf("pipelines", "Bitbucket changeset conditions take either include or exclude paths; the exclude paths were dropped")
	}
	return secs
}

// placed reports whether job j belongs to a section.
func placed(j *ir.Job, kind, pattern string, hasDefault bool) bool {
	c := j.If
	if c == nil || (c.Event == "" && len(c.Branches) == 0) {
		return true
	}
	switch c.Event {
	case ir.EventTag:
		return kind == kindTags
	case ir.EventPullRequest:
		return kind == kindPR
	case ir.EventManual:
		return kind == kindCustom && pattern == "manual"
	case ir.EventSchedule:
		return kind == kindCustom && pattern == "scheduled"
	case ir.EventPush, "":
	default:
		return true
	}
	if len(c.Branches) > 0 {
		if kind == kindDefault {
			return contains(c.Branches, "**")
		}
		return kind == kindBranches && contains(c.Branches, pattern)
	}
	// A push-only job runs in the default pipeline, or in every branch
	// pipeline when there is no default one.
	return kind == kindDefault || (kind == kindBranches && !hasDefault)
}

// mergeBranches builds one section per branch pattern and merges patterns
// that hold exactly the same jobs into one "{a,b}" pattern.
func (e *emitter) mergeBranches(pats []string, newSec func(kind, pattern string) *section) []*section {
	var out []*section
	var groups [][]string
	keyOf := map[string]int{}
	for _, b := range pats {
		s := newSec(kindBranches, b)
		var ids []string
		for _, j := range e.p.Jobs {
			if s.jobs[j.ID] {
				ids = append(ids, j.ID)
			}
		}
		key := strings.Join(ids, "\x00")
		if i, ok := keyOf[key]; ok && !strings.ContainsAny(b, "{},") && !strings.ContainsAny(strings.Join(groups[i], ""), "{},") {
			groups[i] = append(groups[i], b)
			continue
		}
		keyOf[key] = len(groups)
		groups = append(groups, []string{b})
		out = append(out, s)
	}
	for i, g := range groups {
		if len(g) > 1 {
			out[i].pattern = "{" + strings.Join(g, ",") + "}"
		}
	}
	return out
}

func (e *emitter) emitPipelines() *yamlx.Map {
	out := yamlx.NewMap()
	groups := map[string]*yamlx.Map{}
	for _, s := range e.sections() {
		path := "pipelines." + s.kind
		if s.kind != kindDefault {
			path += "." + s.pattern
		}
		items := e.sectionItems(s, path)
		if len(items) == 0 {
			e.d.Infof(path, "no job applies to this pipeline; it was omitted")
			continue
		}
		if s.kind == kindDefault {
			out.Set(kindDefault, items)
			continue
		}
		g, ok := groups[s.kind]
		if !ok {
			g = yamlx.NewMap()
			groups[s.kind] = g
		}
		g.Set(s.pattern, items)
	}
	for _, k := range []string{kindBranches, kindTags, kindPR, kindCustom} {
		if g, ok := groups[k]; ok {
			out.Set(k, g)
		}
	}
	if out.Len() == 0 {
		out.Set(kindDefault, []any{yamlx.NewMap().Set("step", yamlx.NewMap().
			Set("name", "Empty").Set("script", []string{"echo 'nothing to do'"}))})
	}
	return out
}

// sectionItems serializes a section's jobs level by level: a level with a
// single step becomes "- step", several become a parallel group.
func (e *emitter) sectionItems(s *section, path string) []any {
	var items []any
	deployments := map[string]bool{}
	uploaded := false
	var prev []string
	for _, lv := range e.levels {
		var steps []*yamlx.Map
		var manual, ids []string
		var jobs []*ir.Job
		levelUploads := false
		for _, j := range lv {
			if !s.jobs[j.ID] {
				continue
			}
			jobs = append(jobs, j)
			ids = append(ids, j.ID)
			steps = append(steps, e.jobSteps(j, s, uploaded, deployments)...)
			if j.If != nil && j.If.Manual {
				manual = append(manual, j.ID)
			}
			levelUploads = levelUploads || e.info[j.ID].upload
		}
		if len(steps) == 0 {
			continue
		}
		e.checkLevels(path, prev, jobs)
		prev = ids
		if len(items) == 0 && len(manual) > 0 {
			// The first step of a Bitbucket pipeline cannot be manual.
			e.d.Infof(path, "the first step of a pipeline cannot be manual; a start step was added before %s", strings.Join(manual, ", "))
			items = append(items, yamlx.NewMap().Set("step", yamlx.NewMap().
				Set("name", "Start").Set("script", []string{"echo 'waiting for manual steps'"})))
		}
		if len(steps) == 1 {
			items = append(items, yamlx.NewMap().Set("step", steps[0]))
		} else {
			var par []any
			for _, st := range steps {
				par = append(par, yamlx.NewMap().Set("step", st))
			}
			items = append(items, yamlx.NewMap().Set("parallel", par))
		}
		uploaded = uploaded || levelUploads
	}
	return items
}

// jobSteps renders a job as one step, or one step per matrix combination.
func (e *emitter) jobSteps(j *ir.Job, s *section, uploaded bool, deployments map[string]bool) []*yamlx.Map {
	d := e.d
	path := j.ID
	in := e.info[j.ID]
	if c := j.If; c != nil {
		switch c.When {
		case ir.WhenAlways, ir.WhenOnFailure:
			d.Warnf(path, "Bitbucket steps only run after the previous steps succeed; when: %s was dropped", c.When)
		}
		if c.Raw != "" {
			d.Warnf(path, "condition %q (from %s) could not be translated and was dropped", c.Raw, c.RawFrom)
		}
	}
	if j.AllowFailure {
		d.Warnf(path, "Bitbucket steps cannot be allowed to fail; a failure of this step stops the pipeline")
	}
	if len(j.Services) > 5 {
		d.Warnf(path+".services", "Bitbucket allows at most 5 services per step")
	}
	combos := j.Matrix.Combos()
	if len(combos) == 0 {
		combos = [][]ir.KV{nil}
	} else if len(combos) > 100 {
		d.Warnf(path, "the matrix expands to %d parallel steps; Bitbucket allows at most 100", len(combos))
	}
	var out []*yamlx.Map
	for _, combo := range combos {
		out = append(out, e.step(j, in, s, combo, uploaded, deployments))
	}
	return out
}

func (e *emitter) step(j *ir.Job, in *jobInfo, s *section, combo []ir.KV, uploaded bool, deployments map[string]bool) *yamlx.Map {
	d := e.d
	path := j.ID
	tx := func(str string) string { return toBB(substMatrix(str, combo), d, path) }
	m := yamlx.NewMap()
	name := j.DisplayName()
	if len(combo) > 0 {
		var vals []string
		for _, kv := range combo {
			vals = append(vals, kv.Value)
		}
		name += " (" + strings.Join(vals, ", ") + ")"
	}
	m.Set("name", tx(name))
	if img := substMatrix(in.image, combo); img != "" && img != e.topImage {
		m.Set("image", tx(img))
	}
	if ro := e.runsOn(j); len(ro) > 0 {
		m.Set("runs-on", ro)
	}
	if j.TimeoutMinutes > 0 && j.TimeoutMinutes != e.topTimeout {
		m.Set("max-time", j.TimeoutMinutes)
	}
	if clone := e.stepClone(in); clone.Len() > 0 {
		m.Set("clone", clone)
	}
	if env := j.Environment; env != "" {
		d.Infof(path, "deployment environment %q must exist in Repository settings > Deployments", env)
		if deployments[env] {
			d.Warnf(path, "Bitbucket allows each deployment environment once per pipeline; deployment %q was dropped from this step", env)
		} else {
			deployments[env] = true
			m.Set("deployment", env)
		}
	}
	if j.If != nil && j.If.Manual {
		m.Set("trigger", "manual")
	}

	var services []string
	for _, svc := range j.Services {
		if n := e.service(svc, tx, path); !contains(services, n) {
			services = append(services, n)
		}
	}

	var script, after []any
	var caches, uploads []string
	hasDownload := false
	var exports []string
	for _, kv := range e.p.Env {
		exports = append(exports, fmt.Sprintf("export %s=%s", kv.Key, shellQuote(tx(kv.Value))))
	}
	if len(e.p.Env) > 0 {
		d.Infof("", "pipeline variables were exported at the start of every step (Bitbucket has no variables key)")
	}
	for _, kv := range combo {
		exports = append(exports, fmt.Sprintf("export %s=%s", kv.Key, shellQuote(kv.Value)))
	}
	for _, kv := range j.Env {
		exports = append(exports, fmt.Sprintf("export %s=%s", kv.Key, shellQuote(tx(kv.Value))))
	}
	if len(j.Env) > 0 {
		d.Infof(path, "job variables were exported at the start of the step script")
	}
	if len(exports) > 0 {
		script = append(script, strings.Join(exports, "\n"))
	}
	if in.clone && in.co.Submodules {
		script = append(script, "git submodule update --init --recursive")
		d.Infof(path, "Bitbucket does not clone submodules; a git submodule command was added")
	}
	for i, st := range in.steps {
		where := fmt.Sprintf("%s.steps[%d]", path, i)
		deferred := st.If != nil && (st.If.When == ir.WhenAlways || st.If.When == ir.WhenOnFailure)
		switch st.Kind {
		case ir.StepCheckout, ir.StepTool:
			// Cloning is implicit; tools were folded into the image.
		case ir.StepDownload:
			hasDownload = true
			if st.Artifact != nil && st.Artifact.Name != "" {
				d.Infof(where, "Bitbucket downloads every upstream artifact; the name filter %q was dropped", st.Artifact.Name)
			}
		case ir.StepCache:
			if n := e.cache(st.Cache, where); len(n) > 0 {
				caches = appendUnique(caches, n...)
			}
		case ir.StepUpload:
			for _, p := range st.Artifact.Paths {
				uploads = appendUnique(uploads, tx(p))
			}
			if st.Artifact.When != ir.WhenOnSuccess {
				d.Warnf(where, "Bitbucket uploads artifacts only when the step succeeds; when: %s was dropped", st.Artifact.When)
			}
			if st.Artifact.ExpireIn != "" {
				d.Infof(where, "Bitbucket keeps artifacts for 14 days; expire_in %s was dropped", st.Artifact.ExpireIn)
			}
		case ir.StepAction:
			item := e.action(st, tx, where)
			if deferred {
				after = append(after, item)
			} else {
				script = append(script, item)
			}
		default:
			cmd := wrapScript(st, tx, where, d)
			if deferred {
				after = append(after, cmd)
			} else {
				script = append(script, cmd)
			}
		}
	}
	if len(script) == 0 {
		script = []any{"echo 'nothing to do'"}
	}
	m.SetIf("caches", caches)
	m.SetIf("services", services)
	m.Set("script", script)
	m.SetIf("after-script", after)
	switch {
	case uploaded && !hasDownload:
		a := yamlx.NewMap().Set("download", false)
		a.SetIf("paths", uploads)
		m.Set("artifacts", a)
	case len(uploads) > 0:
		m.Set("artifacts", uploads)
	}
	if len(s.include) > 0 {
		m.Set("condition", yamlx.NewMap().Set("changesets", yamlx.NewMap().Set("includePaths", s.include)))
	} else if len(s.exclude) > 0 {
		m.Set("condition", yamlx.NewMap().Set("changesets", yamlx.NewMap().Set("excludePaths", s.exclude)))
	}
	return m
}

// runsOn renders runner labels; hosted Windows/macOS runners do not exist
// on Bitbucket Cloud, so those need self-hosted runners.
func (e *emitter) runsOn(j *ir.Job) []string {
	r := j.Runner
	path := j.ID
	if img := strings.ToLower(r.VMImage); img != "" && !strings.HasPrefix(img, "ubuntu-") && !strings.HasPrefix(img, "windows-") && !strings.HasPrefix(img, "macos-") {
		e.d.Infof(path, "runner image %q was dropped; Bitbucket runs Linux steps in the step image", r.VMImage)
	}
	if len(r.Labels) == 0 && r.IsLinux() {
		return nil
	}
	labels := []string{"self.hosted"}
	switch r.OS {
	case ir.OSWindows:
		labels = append(labels, "windows")
		e.d.Warnf(path, "Windows steps need a self-hosted Windows runner on Bitbucket")
	case ir.OSMacOS:
		labels = append(labels, "macos")
		e.d.Warnf(path, "macOS steps need a self-hosted macOS runner on Bitbucket")
	default:
		labels = append(labels, "linux")
	}
	labels = appendUnique(labels, r.Labels...)
	if len(labels) > 10 {
		e.d.Warnf(path, "Bitbucket allows at most 10 runner labels; extra labels were dropped")
		labels = labels[:10]
	}
	return labels
}

// stepClone renders clone options that differ from the top-level ones.
func (e *emitter) stepClone(in *jobInfo) *yamlx.Map {
	m := yamlx.NewMap()
	if !in.clone {
		return m.Set("enabled", false)
	}
	if in.co.Depth != e.topDepth {
		switch {
		case in.co.Depth < 0:
			m.Set("depth", "full")
		case in.co.Depth > 0:
			m.Set("depth", in.co.Depth)
		}
	}
	if in.co.LFS != e.topLFS {
		m.Set("lfs", in.co.LFS)
	}
	return m
}

// service registers a service definition and returns its name.
func (e *emitter) service(s ir.Service, tx func(string) string, path string) string {
	img := tx(s.Image)
	if strings.HasPrefix(img, "docker:") && strings.Contains(img, "dind") {
		e.d.Infof(path+".services", "docker-in-docker was replaced by Bitbucket's built-in docker service")
		return "docker"
	}
	if len(s.Ports) > 0 || s.Name != "" {
		e.d.Infof(path+".services", "Bitbucket services share the step's network; connect to localhost instead of the service name")
	}
	var env []string
	for _, kv := range s.Env {
		env = append(env, kv.Key+"="+tx(kv.Value))
	}
	key := s.Name + "\x00" + img + "\x00" + strings.Join(env, "\x00")
	if n, ok := e.serviceIDs[key]; ok {
		return n
	}
	base := ir.Slug(s.Name)
	if s.Name == "" {
		base = serviceAlias(img)
	}
	name := ir.UniqueID(base, e.taken)
	def := yamlx.NewMap().Set("image", img)
	if len(s.Env) > 0 {
		vars := yamlx.NewMap()
		for _, kv := range s.Env {
			vars.Set(kv.Key, tx(kv.Value))
		}
		def.Set("variables", vars)
	}
	e.services.Set(name, def)
	e.serviceIDs[key] = name
	return name
}

// cache returns the cache names for c, reusing predefined caches and
// registering custom definitions (Bitbucket caches hold one path each).
func (e *emitter) cache(c *ir.Cache, where string) []string {
	if len(c.Paths) == 0 {
		e.d.Warnf(where, "cache without paths was dropped")
		return nil
	}
	var names []string
	for i, p := range c.Paths {
		if len(c.Files) == 0 {
			if pre, ok := predefinedCaches[c.Prefix]; ok && pre == p {
				names = append(names, c.Prefix)
				continue
			}
		}
		key := strings.Join(c.Files, "\x00") + "\x01" + p
		if n, ok := e.cacheIDs[key]; ok {
			names = append(names, n)
			continue
		}
		base := cacheSlug(c.Prefix)
		if i > 0 {
			base = fmt.Sprintf("%s-%d", base, i+1)
		}
		name := ir.UniqueID(base, e.taken)
		if len(c.Files) > 0 {
			e.caches.Set(name, yamlx.NewMap().
				Set("key", yamlx.NewMap().Set("files", c.Files)).
				Set("path", p))
		} else {
			e.caches.Set(name, p)
		}
		e.cacheIDs[key] = name
		names = append(names, name)
	}
	return names
}

// cacheSlug makes a cache name: lowercase letters, digits and dashes.
func cacheSlug(s string) string {
	n := strings.ReplaceAll(ir.Slug(s), "_", "-")
	if n == "job" {
		n = "deps"
	}
	if len(n) > 40 {
		n = strings.TrimRight(n[:40], "-")
	}
	return n
}

// action renders a pipe, or a failing placeholder for foreign actions.
func (e *emitter) action(st *ir.Step, tx func(string) string, where string) any {
	if st.If != nil && (len(st.If.Branches) > 0 || st.If.Event != "" || st.If.Raw != "") {
		e.d.Warnf(where, "pipe conditions cannot be expressed on Bitbucket; the step always runs")
	}
	if st.UsesFrom != "bitbucket" {
		e.d.Warnf(where, "%s step %q has no Bitbucket equivalent; a failing placeholder command was emitted", st.UsesFrom, st.Uses)
		return fmt.Sprintf("echo \"cistela: port %s step '%s' manually\" && exit 1", st.UsesFrom, st.Uses)
	}
	pm := yamlx.NewMap().Set("pipe", st.Uses)
	vars := yamlx.NewMap()
	for _, kv := range st.With {
		vars.Set(kv.Key, tx(kv.Value))
	}
	pm.SetIf("variables", vars)
	return pm
}

// wrapScript renders a run step as one script entry, scoping env vars and
// working directory in a subshell and guarding step conditions.
func wrapScript(st *ir.Step, tx func(string) string, where string, d *ir.Diagnostics) string {
	body := tx(st.Run)
	if st.Shell != "" && st.Shell != "bash" && st.Shell != "sh" {
		d.Warnf(where, "shell %q is not configurable on Bitbucket; the script runs in the image's shell", st.Shell)
	}
	if exprs := ir.ForeignExpressions(body); len(exprs) > 0 {
		d.Warnf(where, "expressions %s could not be translated", strings.Join(exprs, ", "))
	}
	if st.TimeoutMinutes > 0 {
		d.Infof(where, "step timeouts are not supported by Bitbucket and were dropped")
	}
	if len(st.Env) > 0 || st.WorkingDir != "" {
		var b strings.Builder
		b.WriteString("(\n")
		for _, kv := range st.Env {
			fmt.Fprintf(&b, "  export %s=%s\n", kv.Key, shellQuote(tx(kv.Value)))
		}
		if st.WorkingDir != "" {
			fmt.Fprintf(&b, "  cd %s\n", shellQuote(tx(st.WorkingDir)))
		}
		for _, line := range strings.Split(body, "\n") {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString(")")
		body = b.String()
	}
	if st.If != nil {
		cond, ok := shellCondition(st.If)
		if !ok {
			d.Warnf(where, "step condition could not be translated; the step always runs")
		} else if cond != "" {
			body = fmt.Sprintf("if %s; then\n%s\nfi", cond, indent(body))
		}
		if st.If.When == ir.WhenOnFailure {
			body = fmt.Sprintf("if [ \"${BITBUCKET_EXIT_CODE:-0}\" != \"0\" ]; then\n%s\nfi", indent(body))
		}
	}
	if st.AllowFailure && (st.If == nil || st.If.When == ir.WhenOnSuccess) {
		if strings.Contains(body, "\n") {
			body = "(\n" + indent(body) + "\n) || true"
		} else {
			body += " || true"
		}
	}
	if st.Name != "" && strings.Contains(body, "\n") {
		body = "# " + tx(st.Name) + "\n" + body
	}
	return body
}

// shellCondition extends ir.ShellCondition with pull request/push events,
// which Bitbucket exposes through BITBUCKET_PR_ID.
func shellCondition(c *ir.Condition) (string, bool) {
	cc := *c
	cc.When = ""
	var extra string
	switch cc.Event {
	case ir.EventPullRequest:
		extra = `[ -n "${BITBUCKET_PR_ID:-}" ]`
		cc.Event = ""
	case ir.EventPush:
		extra = `[ -z "${BITBUCKET_PR_ID:-}" ]`
		cc.Event = ""
	}
	cond, ok := ir.ShellCondition(&cc, "BITBUCKET_BRANCH", "BITBUCKET_TAG")
	if !ok {
		return "", false
	}
	switch {
	case extra == "":
		return cond, true
	case cond == "":
		return extra, true
	}
	return extra + " && " + cond, true
}

// substMatrix replaces matrix references with a combination's values.
func substMatrix(s string, combo []ir.KV) string {
	if len(combo) == 0 {
		return s
	}
	return ir.RewriteRefs(s, func(kind, name string) string {
		if kind == ir.RefMatrix {
			if v, ok := ir.KVGet(combo, name); ok {
				return v
			}
		}
		return ir.Ref(kind, name)
	})
}
