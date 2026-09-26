package circleci

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

// builtinSteps are CircleCI's native step types; anything else is a
// reusable command or an orb command.
var builtinSteps = map[string]bool{
	"run": true, "checkout": true, "setup_remote_docker": true, "save_cache": true,
	"restore_cache": true, "deploy": true, "store_artifacts": true, "store_test_results": true,
	"persist_to_workspace": true, "attach_workspace": true, "add_ssh_keys": true, "when": true,
	"unless": true, "install_signing_bundle": true, "with_tool_cache": true,
}

// workflowKeys are the options of a workflow job entry; other keys pass
// job parameters.
var workflowKeys = map[string]bool{
	"requires": true, "filters": true, "context": true, "type": true, "name": true, "matrix": true,
	"pre-steps": true, "post-steps": true, "serial-group": true, "override-with": true,
}

// pipelineValues maps CircleCI pipeline values to canonical references.
var pipelineValues = map[string]string{
	"pipeline.git.branch":     ir.VarBranch,
	"pipeline.git.tag":        ir.VarTag,
	"pipeline.git.revision":   ir.VarSHA,
	"pipeline.number":         ir.VarRunNumber,
	"pipeline.id":             ir.VarRunID,
	"pipeline.trigger_source": ir.VarEvent,
}

// Commands that the emitter adds after checkout; parsing folds them back.
const (
	submoduleCmd = "git submodule sync\ngit submodule update --init --recursive"
	lfsCmd       = "git lfs pull"
)

const maxCommandDepth = 16

var (
	paramRe    = regexp.MustCompile(`<<\s*([A-Za-z0-9_.\-]+)\s*>>`)
	wholeRe    = regexp.MustCompile(`^\s*<<\s*([A-Za-z0-9_.\-]+)\s*>>\s*$`)
	checksumRe = regexp.MustCompile(`\{\{\s*checksum\s+"([^"]+)"\s*\}\}`)
	templateRe = regexp.MustCompile(`\{\{[^}]*\}\}`)
)

type parser struct {
	root       *yaml.Node
	d          *ir.Diagnostics
	orbs       map[string]string
	pipeParams map[string]*yaml.Node
}

// Parse implements ir.Provider.
func (Provider) Parse(data []byte, d *ir.Diagnostics) (*ir.Pipeline, error) {
	root, err := yamlx.Load(data)
	if err != nil {
		return nil, fmt.Errorf("circleci: %w", err)
	}
	if !yamlx.IsMap(root) {
		return nil, fmt.Errorf("circleci: configuration must be a mapping")
	}
	if !yamlx.IsMap(yamlx.Get(root, "jobs")) && !yamlx.IsMap(yamlx.Get(root, "workflows")) {
		return nil, fmt.Errorf("circleci: configuration has no jobs")
	}
	switch v := yamlx.Str(yamlx.Get(root, "version")); v {
	case "2.1", "":
	default:
		d.Infof("version", "version %s configuration was read with 2.1 semantics", v)
	}
	ps := &parser{root: root, d: d, orbs: map[string]string{}, pipeParams: map[string]*yaml.Node{}}
	for _, pair := range yamlx.Pairs(yamlx.Get(root, "orbs")) {
		if yamlx.IsScalar(pair.Value) {
			ps.orbs[pair.Key] = pair.Value.Value
		} else {
			ps.orbs[pair.Key] = "inline"
			d.Warnf("orbs."+pair.Key, "inline orbs are not portable; commands using orb %q are kept as CircleCI steps", pair.Key)
		}
	}
	for _, pair := range yamlx.Pairs(yamlx.Get(root, "parameters")) {
		ps.pipeParams[pair.Key] = yamlx.Get(pair.Value, "default")
	}
	if v, ok := yamlx.Bool(yamlx.Get(root, "setup")); ok && v {
		d.Warnf("setup", "dynamic configuration (setup workflows) is not portable; only this file was converted")
	}
	p := &ir.Pipeline{}
	ps.parseWorkflows(p)
	ir.PromoteExternalVars(p, d, builtinPrefixes...)
	return p, nil
}

// scalar builds a plain string node.
func scalar(s string) *yaml.Node { return yamlx.Key(s) }

// scope resolves << ... >> placeholders.
type scope struct {
	ps     *parser
	params map[string]*yaml.Node
	path   string
}

func (sc *scope) lookup(ref string) (*yaml.Node, bool) {
	switch {
	case strings.HasPrefix(ref, "parameters."):
		name := strings.TrimPrefix(ref, "parameters.")
		v, ok := sc.params[name]
		if !ok {
			sc.ps.d.Warnf(sc.path, "parameter %q is not declared; the placeholder was kept", name)
			return nil, false
		}
		if v == nil {
			return scalar(""), true
		}
		return v, true
	case strings.HasPrefix(ref, "pipeline.parameters."):
		name := strings.TrimPrefix(ref, "pipeline.parameters.")
		v, ok := sc.ps.pipeParams[name]
		if !ok || v == nil {
			sc.ps.d.Warnf(sc.path, "pipeline parameter %q has no default; it was replaced by an empty value", name)
			return scalar(""), true
		}
		sc.ps.d.Infof(sc.path, "pipeline parameter %q was replaced by its default value %q", name, yamlx.Str(v))
		return v, true
	case strings.HasPrefix(ref, "matrix."):
		return scalar(ir.Ref(ir.RefMatrix, strings.TrimPrefix(ref, "matrix."))), true
	}
	if canon, ok := pipelineValues[ref]; ok {
		return scalar(ir.Ref(ir.RefCI, canon)), true
	}
	sc.ps.d.Warnf(sc.path, "pipeline value %q has no portable equivalent; the placeholder was kept", ref)
	return nil, false
}

// subst returns a deep copy of n with placeholders replaced. A scalar that
// is a single placeholder takes the parameter's node (steps, maps, ...);
// such a node inside a sequence is spliced in (steps parameters).
func (sc *scope) subst(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		c := *n
		if !strings.Contains(n.Value, "<<") {
			return &c
		}
		if m := wholeRe.FindStringSubmatch(n.Value); m != nil {
			if v, ok := sc.lookup(m[1]); ok {
				return (&scope{ps: sc.ps}).subst(v)
			}
			return &c
		}
		c.Value = paramRe.ReplaceAllStringFunc(n.Value, func(s string) string {
			if v, ok := sc.lookup(paramRe.FindStringSubmatch(s)[1]); ok && yamlx.IsScalar(v) {
				return yamlx.Str(v)
			}
			return s
		})
		if c.Value != n.Value {
			c.Tag, c.Style = "!!str", 0
		}
		return &c
	case yaml.SequenceNode:
		c := *n
		c.Content = nil
		for _, item := range n.Content {
			r := sc.subst(item)
			if yamlx.IsScalar(item) && (yamlx.IsSeq(r) || yamlx.IsNull(r) && wholeRe.MatchString(item.Value)) {
				if yamlx.IsSeq(r) {
					c.Content = append(c.Content, r.Content...)
				}
				continue
			}
			c.Content = append(c.Content, r)
		}
		return &c
	case yaml.MappingNode:
		c := *n
		c.Content = nil
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := *n.Content[i]
			c.Content = append(c.Content, &k, sc.subst(n.Content[i+1]))
		}
		return &c
	}
	return n
}

// bindParams resolves declared parameters from the given arguments and
// the declared defaults.
func (ps *parser) bindParams(decl *yaml.Node, given func(string) *yaml.Node, where string) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	for _, pair := range yamlx.Pairs(decl) {
		v := given(pair.Key)
		if v == nil {
			v = yamlx.Get(pair.Value, "default")
		}
		if v == nil {
			switch yamlx.Str(yamlx.Get(pair.Value, "type")) {
			case "boolean":
				v = scalar("false")
			case "steps":
				v = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			default:
				ps.d.Warnf(where+".parameters."+pair.Key, "parameter %q has no value and no default; it was left empty", pair.Key)
				v = scalar("")
			}
		}
		out[pair.Key] = v
	}
	return out
}

// evalLogic evaluates a compile-time logic statement. known is false when
// the value depends on runtime data.
func evalLogic(n *yaml.Node) (value, known bool) {
	switch {
	case n == nil:
		return false, true
	case yamlx.IsScalar(n):
		v := strings.TrimSpace(n.Value)
		if strings.Contains(v, "${{") || strings.Contains(v, "<<") {
			return false, false
		}
		switch strings.ToLower(v) {
		case "", "false", "0", "null", "~", "no", "off":
			return false, true
		}
		return true, true
	case yamlx.IsMap(n) && len(n.Content) == 2:
		op, arg := n.Content[0].Value, n.Content[1]
		switch op {
		case "and", "or":
			result := op == "and"
			for _, item := range yamlx.Items(arg) {
				v, ok := evalLogic(item)
				if !ok {
					return false, false
				}
				if op == "and" {
					result = result && v
				} else {
					result = result || v
				}
			}
			return result, true
		case "not":
			v, ok := evalLogic(arg)
			return !v, ok
		case "equal":
			items := yamlx.StrList(arg)
			for _, it := range items {
				if strings.Contains(it, "${{") || strings.Contains(it, "<<") {
					return false, false
				}
			}
			for _, it := range items {
				if it != items[0] {
					return false, true
				}
			}
			return true, true
		case "matches":
			pat, val := yamlx.Str(yamlx.Get(arg, "pattern")), yamlx.Str(yamlx.Get(arg, "value"))
			re, err := regexp.Compile(pat)
			if err != nil || strings.Contains(val, "${{") || strings.Contains(val, "<<") {
				return false, false
			}
			return re.MatchString(val), true
		}
	}
	return false, false
}

// isManualOnly recognizes the workflow condition the emitter writes for
// manually triggered pipelines.
func isManualOnly(when *yaml.Node) bool {
	items := yamlx.StrList(yamlx.Get(when, "equal"))
	if len(items) != 2 {
		return false
	}
	hasAPI, hasSource := false, false
	for _, it := range items {
		switch {
		case it == "api":
			hasAPI = true
		case wholeRe.MatchString(it) && wholeRe.FindStringSubmatch(it)[1] == "pipeline.trigger_source":
			hasSource = true
		}
	}
	return hasAPI && hasSource
}

type workflow struct {
	name string
	node *yaml.Node
}

func isScheduled(wf *yaml.Node) bool {
	for _, t := range yamlx.Items(yamlx.Get(wf, "triggers")) {
		if yamlx.Has(t, "schedule") {
			return true
		}
	}
	return false
}

// parseWorkflows builds jobs and triggers from the workflows section.
func (ps *parser) parseWorkflows(p *ir.Pipeline) {
	d := ps.d
	var main *workflow
	var sched []workflow
	var others []string
	for _, pair := range yamlx.Pairs(yamlx.Get(ps.root, "workflows")) {
		if pair.Key == "version" || !yamlx.IsMap(pair.Value) {
			continue
		}
		w := workflow{name: pair.Key, node: pair.Value}
		switch {
		case isScheduled(pair.Value):
			sched = append(sched, w)
		case main == nil:
			main = &w
		default:
			others = append(others, pair.Key)
		}
	}
	if len(others) > 0 {
		d.Warnf("workflows", "only workflow %q was converted; workflows %s were dropped", main.name, strings.Join(others, ", "))
	}
	for _, w := range sched {
		p.Triggers.Schedules = append(p.Triggers.Schedules, ps.parseSchedules(w)...)
	}

	if main == nil && len(sched) == 0 {
		// Without workflows every job is a standalone unit.
		d.Infof("workflows", "no workflows are defined; every job was converted as an independent job")
		for _, pair := range yamlx.Pairs(yamlx.Get(ps.root, "jobs")) {
			if j := ps.buildJob(pair.Key, pair.Key, nil, "jobs."+pair.Key); j != nil {
				p.Jobs = append(p.Jobs, j)
			}
		}
		p.Triggers.Push = &ir.PushTrigger{}
		return
	}

	scheduleOnly := false
	if main == nil {
		main = &sched[0]
		scheduleOnly = true
	} else {
		p.Triggers.Push = &ir.PushTrigger{}
	}
	if main.name != "main" {
		p.Name = main.name
	}
	wpath := "workflows." + main.name
	if when := yamlx.Get(main.node, "when"); when != nil {
		if isManualOnly(when) {
			p.Triggers.Push = nil
			p.Triggers.Manual = true
		} else {
			d.Warnf(wpath+".when", "workflow conditions are not portable and were dropped")
		}
	}
	if yamlx.Has(main.node, "unless") {
		d.Warnf(wpath+".unless", "workflow conditions are not portable and were dropped")
	}

	g := &graph{ps: ps, refs: map[string]string{}, taken: map[string]bool{}, approvals: map[string]*entry{}}
	mainEntries := g.entries(main.name, main.node)
	filters := map[string]jobFilter{}
	for _, e := range mainEntries {
		if e.approval {
			continue
		}
		j := ps.buildJob(e.id, e.key, e.opts, "jobs."+e.key)
		if j == nil {
			continue
		}
		g.wire(j, e)
		filters[j.ID] = ps.parseFilters(yamlx.Get(e.opts, "filters"), e.path+".filters")
		p.Jobs = append(p.Jobs, j)
	}
	g.dropUnknownNeeds(p)
	if !scheduleOnly {
		liftFilters(p, filters, d, wpath)
	}

	// Jobs that only run in a scheduled workflow are schedule-conditioned.
	for _, w := range sched {
		if scheduleOnly && w.name == main.name {
			continue
		}
		for _, e := range g.entries(w.name, w.node) {
			if e.approval || !e.fresh {
				continue
			}
			j := ps.buildJob(e.id, e.key, e.opts, "jobs."+e.key)
			if j == nil {
				continue
			}
			g.wire(j, e)
			j.If = &ir.Condition{Event: ir.EventSchedule}
			p.Jobs = append(p.Jobs, j)
		}
	}
	g.dropUnknownNeeds(p)
}

func (ps *parser) parseSchedules(w workflow) []ir.Schedule {
	var out []ir.Schedule
	for i, t := range yamlx.Items(yamlx.Get(w.node, "triggers")) {
		s := yamlx.Get(t, "schedule")
		cron := yamlx.Str(yamlx.Get(s, "cron"))
		if cron == "" {
			continue
		}
		sc := ir.Schedule{Cron: cron}
		where := fmt.Sprintf("workflows.%s.triggers[%d]", w.name, i)
		for _, b := range yamlx.StrList(yamlx.Path(s, "filters", "branches", "only")) {
			if g, ok := ps.glob(b, where); ok {
				sc.Branches = append(sc.Branches, g)
			}
		}
		if yamlx.Path(s, "filters", "branches", "ignore") != nil {
			ps.d.Warnf(where, "schedule branch ignore filters are not portable and were dropped")
		}
		out = append(out, sc)
	}
	return out
}

// glob converts a CircleCI branch/tag specifier (literal or /regex/).
func (ps *parser) glob(s, where string) (string, bool) {
	if len(s) >= 2 && strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
		if g, ok := ir.RegexToGlob(s); ok {
			return g, true
		}
		ps.d.Warnf(where, "filter regex %s could not be translated and was dropped", s)
		return "", false
	}
	return s, true
}

// entry is one job invocation in a workflow.
type entry struct {
	key      string // job name in the jobs section
	id       string // IR job ID
	opts     *yaml.Node
	approval bool
	fresh    bool // first time this entry's identity was seen
	path     string
}

// graph resolves workflow entries into IR job IDs and dependencies.
type graph struct {
	ps        *parser
	refs      map[string]string // workflow name/alias -> IR ID
	taken     map[string]bool
	approvals map[string]*entry
}

func (g *graph) entries(wfName string, wf *yaml.Node) []*entry {
	var out []*entry
	for i, item := range yamlx.Items(yamlx.Get(wf, "jobs")) {
		e := &entry{path: fmt.Sprintf("workflows.%s.jobs[%d]", wfName, i)}
		switch {
		case yamlx.IsScalar(item):
			e.key = item.Value
		case yamlx.IsMap(item) && len(item.Content) == 2:
			e.key, e.opts = item.Content[0].Value, item.Content[1]
		default:
			g.ps.d.Warnf(e.path, "unrecognized workflow job entry was dropped")
			continue
		}
		e.approval = yamlx.Str(yamlx.Get(e.opts, "type")) == "approval"
		name := yamlx.Str(yamlx.Get(e.opts, "name"))
		ident := e.key
		base := e.key
		if name != "" {
			ident = name
			base = strings.Trim(paramRe.ReplaceAllString(name, ""), "-_ ")
			if base == "" {
				base = e.key
			}
		}
		if id, ok := g.refs[ident]; ok {
			e.id = id
		} else {
			if strings.Contains(base, "/") {
				base = ir.Slug(base)
			}
			e.id = ir.UniqueID(base, g.taken)
			e.fresh = true
			g.refs[ident] = e.id
		}
		if _, ok := g.refs[e.key]; !ok {
			g.refs[e.key] = e.id
		}
		if alias := yamlx.Str(yamlx.Path(e.opts, "matrix", "alias")); alias != "" {
			g.refs[alias] = e.id
		}
		if e.approval {
			g.approvals[e.id] = e
		}
		if c := yamlx.Get(e.opts, "context"); c != nil && !e.approval {
			g.ps.d.Infof(e.path+".context", "contexts %v provide secrets; recreate them in the target CI", yamlx.StrList(c))
		}
		out = append(out, e)
	}
	return out
}

// requires resolves an entry's requires list, looking through approval
// jobs. manual reports whether an approval gates the entry.
func (g *graph) requires(e *entry, depth int) (needs []string, when string, manual bool) {
	seen := map[string]bool{}
	whens := map[string]bool{}
	for _, item := range yamlx.Items(yamlx.Get(e.opts, "requires")) {
		ref := yamlx.Str(item)
		status := ""
		if yamlx.IsMap(item) && len(item.Content) == 2 {
			ref = item.Content[0].Value
			status = requireStatus(item.Content[1])
		}
		whens[status] = true
		id, ok := g.refs[ref]
		if !ok {
			g.ps.d.Warnf(e.path+".requires", "required job %q is not part of the workflow and was dropped", ref)
			continue
		}
		if a := g.approvals[id]; a != nil {
			manual = true
			if depth < maxCommandDepth {
				sub, _, _ := g.requires(a, depth+1)
				for _, s := range sub {
					if !seen[s] {
						seen[s] = true
						needs = append(needs, s)
					}
				}
			}
			continue
		}
		if !seen[id] {
			seen[id] = true
			needs = append(needs, id)
		}
	}
	if len(whens) > 1 {
		g.ps.d.Warnf(e.path+".requires", "mixed requirement statuses were approximated by the first one")
	}
	for w := range whens {
		if w != "" {
			when = w
		}
	}
	return needs, when, manual
}

// requireStatus maps a requires status to a condition When value.
func requireStatus(n *yaml.Node) string {
	statuses := yamlx.StrList(n)
	has := map[string]bool{}
	for _, s := range statuses {
		has[s] = true
	}
	switch {
	case has["terminal"] || has["success"] && has["failed"]:
		return ir.WhenAlways
	case has["failed"]:
		return ir.WhenOnFailure
	}
	return ""
}

// wire sets needs and gating conditions on a job built from an entry.
func (g *graph) wire(j *ir.Job, e *entry) {
	needs, when, manual := g.requires(e, 0)
	j.Needs = needs
	if when != "" || manual {
		if j.If == nil {
			j.If = &ir.Condition{}
		}
		j.If.When = when
		j.If.Manual = manual
	}
}

func (g *graph) dropUnknownNeeds(p *ir.Pipeline) {
	ids := map[string]bool{}
	for _, j := range p.Jobs {
		ids[j.ID] = true
	}
	for _, j := range p.Jobs {
		var kept []string
		for _, n := range j.Needs {
			if ids[n] {
				kept = append(kept, n)
			} else {
				g.ps.d.Warnf("jobs."+j.ID, "dependency on %q was dropped because that job could not be converted", n)
			}
		}
		j.Needs = kept
	}
}

// jobFilter is a parsed workflow job filter.
type jobFilter struct {
	only, ignore, tags []string
	tagOnly            bool
	raw                string
}

func (ps *parser) parseFilters(n *yaml.Node, where string) jobFilter {
	var f jobFilter
	if n == nil {
		return f
	}
	if yamlx.IsScalar(n) {
		f.raw = n.Value
		return f
	}
	conv := func(list []string, w string) []string {
		var out []string
		for _, s := range list {
			if g, ok := ps.glob(s, w); ok {
				out = append(out, g)
			}
		}
		return out
	}
	f.only = conv(yamlx.StrList(yamlx.Path(n, "branches", "only")), where+".branches.only")
	f.ignore = conv(yamlx.StrList(yamlx.Path(n, "branches", "ignore")), where+".branches.ignore")
	f.tags = conv(yamlx.StrList(yamlx.Path(n, "tags", "only")), where+".tags.only")
	if yamlx.Path(n, "tags", "ignore") != nil {
		ps.d.Warnf(where+".tags.ignore", "tag ignore filters are not portable and were dropped")
	}
	if len(f.tags) > 0 && len(f.only) == 0 {
		for _, b := range f.ignore {
			if b == "*" {
				f.tagOnly = true
				f.ignore = nil
				break
			}
		}
	}
	return f
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// liftFilters turns per-job filters into job conditions, lifting filters
// that every job shares into the push trigger.
func liftFilters(p *ir.Pipeline, filters map[string]jobFilter, d *ir.Diagnostics, wpath string) {
	if len(p.Jobs) == 0 || p.Triggers.Push == nil {
		return
	}
	first := filters[p.Jobs[0].ID]
	sharedOnly, sharedIgnore, allTagOnly := len(first.only) > 0, len(first.ignore) > 0, true
	var tags []string
	for _, j := range p.Jobs {
		f := filters[j.ID]
		sharedOnly = sharedOnly && !f.tagOnly && sameList(f.only, first.only)
		sharedIgnore = sharedIgnore && sameList(f.ignore, first.ignore)
		allTagOnly = allTagOnly && f.tagOnly
		tags = append(tags, f.tags...)
	}
	push := p.Triggers.Push
	if sharedOnly {
		push.Branches = first.only
	}
	if sharedIgnore {
		push.BranchesIgnore = first.ignore
	}
	push.Tags = uniq(tags)
	if len(push.Tags) > 0 && !allTagOnly && len(push.Branches) == 0 {
		// Branch builds stay enabled alongside the tag builds.
		push.Branches = []string{"*"}
	}
	for _, j := range p.Jobs {
		f := filters[j.ID]
		c := &ir.Condition{}
		if j.If != nil {
			c = j.If
		}
		switch {
		case f.raw != "":
			c.Raw, c.RawFrom = f.raw, "circleci"
			d.Warnf(wpath+"."+j.ID+".filters", "expression filter %q could not be translated", f.raw)
		case f.tagOnly && !allTagOnly:
			c.Event = ir.EventTag
		case !f.tagOnly:
			if !sharedOnly && len(f.only) > 0 {
				c.Branches = f.only
			} else if len(push.Tags) > 0 && len(f.tags) == 0 {
				// The job is filtered out of tag builds.
				c.Branches = []string{"*"}
			}
		}
		if !sharedIgnore && len(f.ignore) > 0 {
			d.Warnf(wpath+"."+j.ID+".filters", "per-job branch ignore filters are not portable and were dropped")
		}
		if !c.IsZero() {
			j.If = c
		}
	}
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// buildJob converts one job invocation.
func (ps *parser) buildJob(id, key string, opts *yaml.Node, where string) *ir.Job {
	d := ps.d
	j := &ir.Job{ID: id}
	def := yamlx.Get(yamlx.Get(ps.root, "jobs"), key)
	if def == nil {
		if strings.Contains(key, "/") {
			d.Warnf(where, "orb job %q is not portable; a placeholder job was created", key)
			st := &ir.Step{Kind: ir.StepAction, Uses: key, UsesFrom: "circleci"}
			for _, pair := range yamlx.Pairs(opts) {
				if !workflowKeys[pair.Key] {
					st.With = append(st.With, ir.KV{Key: pair.Key, Value: fromCC(yamlx.Str(pair.Value))})
				}
			}
			j.Steps = []*ir.Step{st}
			return j
		}
		d.Warnf(where, "job %q is not defined and was dropped", key)
		return nil
	}

	matrixVars := map[string]bool{}
	if m := yamlx.Get(opts, "matrix"); m != nil {
		j.Matrix = &ir.Matrix{}
		for _, pair := range yamlx.Pairs(yamlx.Get(m, "parameters")) {
			j.Matrix.Axes = append(j.Matrix.Axes, ir.Axis{Name: pair.Key, Values: yamlx.StrList(pair.Value)})
			matrixVars[pair.Key] = true
		}
		for _, ex := range yamlx.Items(yamlx.Get(m, "exclude")) {
			var combo []ir.KV
			for _, pair := range yamlx.Pairs(ex) {
				combo = append(combo, ir.KV{Key: pair.Key, Value: yamlx.Str(pair.Value)})
			}
			j.Matrix.Exclude = append(j.Matrix.Exclude, combo)
		}
	}
	top := &scope{ps: ps, path: where}
	params := ps.bindParams(yamlx.Get(def, "parameters"), func(name string) *yaml.Node {
		if matrixVars[name] {
			return scalar(ir.Ref(ir.RefMatrix, name))
		}
		if workflowKeys[name] {
			return nil
		}
		return top.subst(yamlx.Get(opts, name))
	}, where)
	def = (&scope{ps: ps, params: params, path: where}).subst(def)

	var ex execInfo
	if exRef := yamlx.Get(def, "executor"); exRef != nil {
		name, args := yamlx.Str(exRef), exRef
		if yamlx.IsMap(exRef) {
			name = yamlx.Str(yamlx.Get(exRef, "name"))
		}
		exDef := yamlx.Get(yamlx.Get(ps.root, "executors"), name)
		switch {
		case exDef != nil:
			exParams := ps.bindParams(yamlx.Get(exDef, "parameters"), func(n string) *yaml.Node {
				if n == "name" || !yamlx.IsMap(args) {
					return nil
				}
				return yamlx.Get(args, n)
			}, where+".executor")
			exDef = (&scope{ps: ps, params: exParams, path: where + ".executor"}).subst(exDef)
			ps.applyExecutor(j, exDef, &ex, where+".executor")
		case strings.Contains(name, "/"):
			d.Warnf(where+".executor", "orb executor %q is not portable; the default image is used", name)
		default:
			d.Warnf(where+".executor", "executor %q is not defined", name)
		}
	}
	ps.applyExecutor(j, def, &ex, where)

	for _, k := range []string{"parallelism", "circleci_ip_ranges", "retention"} {
		if yamlx.Has(def, k) {
			d.Warnf(where+"."+k, "%q is not portable and was dropped", k)
		}
	}

	items := yamlx.Items(yamlx.Get(def, "steps"))
	if pre := yamlx.Items(top.subst(yamlx.Get(opts, "pre-steps"))); len(pre) > 0 {
		items = append(append([]*yaml.Node(nil), pre...), items...)
	}
	if post := yamlx.Items(top.subst(yamlx.Get(opts, "post-steps"))); len(post) > 0 {
		items = append(append([]*yaml.Node(nil), items...), post...)
	}
	j.Steps = ps.parseSteps(ps.expandSteps(items, where+".steps", 0), where)

	// Relative working directories are expressed per step.
	rel := relWorkdir(ex.workdir)
	if ex.workdir != "" && rel == "" && !isDefaultWorkdir(ex.workdir) {
		d.Infof(where+".working_directory", "working directory %q was dropped; paths are relative to the checkout", ex.workdir)
	}
	shell := normalizeShell(ex.shell)
	for _, st := range j.Steps {
		if st.Kind != ir.StepRun {
			continue
		}
		if st.WorkingDir != "" {
			if r := relWorkdir(st.WorkingDir); r != "" || isDefaultWorkdir(st.WorkingDir) {
				st.WorkingDir = r
			}
		}
		if rel != "" && !path.IsAbs(st.WorkingDir) && !strings.HasPrefix(st.WorkingDir, "~") {
			st.WorkingDir = path.Join(rel, st.WorkingDir)
		}
		if st.Shell == "" {
			st.Shell = shell
		}
	}

	// Emitters write matrix parameters into same-named variables; drop them.
	var env []ir.KV
	for _, kv := range j.Env {
		if matrixVars[kv.Key] && kv.Value == ir.Ref(ir.RefMatrix, kv.Key) {
			continue
		}
		env = append(env, kv)
	}
	j.Env = env
	return j
}

// execInfo collects executor settings that apply to steps.
type execInfo struct {
	workdir string
	shell   string
}

// applyExecutor reads docker/machine/macos settings from an executor or
// job definition; later calls override earlier ones.
func (ps *parser) applyExecutor(j *ir.Job, n *yaml.Node, ex *execInfo, where string) {
	d := ps.d
	if docker := yamlx.Items(yamlx.Get(n, "docker")); len(docker) > 0 {
		j.Services = nil
		j.Runner = ir.Runner{}
		for i, c := range docker {
			img := fromCC(yamlx.Str(yamlx.Get(c, "image")))
			if hasAny(c, "auth", "aws_auth", "gcp_auth") {
				d.Warnf(fmt.Sprintf("%s.docker[%d]", where, i), "registry credentials were dropped; configure them in the target CI")
			}
			if hasAny(c, "entrypoint", "command", "user") {
				d.Warnf(fmt.Sprintf("%s.docker[%d]", where, i), "container entrypoint/command/user overrides were dropped")
			}
			if i == 0 {
				j.Image = img
				for _, kv := range parseEnv(yamlx.Get(c, "environment")) {
					j.Env = ir.KVSet(j.Env, kv.Key, kv.Value)
				}
				continue
			}
			svc := ir.Service{Name: yamlx.Str(yamlx.Get(c, "name")), Image: img, Env: parseEnv(yamlx.Get(c, "environment"))}
			if svc.Name == "" {
				svc.Name = serviceAlias(img)
			}
			j.Services = append(j.Services, svc)
		}
	}
	if m := yamlx.Get(n, "machine"); m != nil {
		j.Image, j.Services = "", nil
		j.Runner = ir.Runner{OS: ir.OSLinux}
		img := yamlx.Str(yamlx.Get(m, "image"))
		if yamlx.IsScalar(m) {
			if _, isBool := yamlx.Bool(m); !isBool {
				img = m.Value
			}
		}
		j.Runner.VMImage = img
		if strings.HasPrefix(img, "windows-") {
			j.Runner.OS = ir.OSWindows
		}
		if yamlx.Has(m, "docker_layer_caching") {
			d.Infof(where+".machine", "docker layer caching was dropped")
		}
		if rc := yamlx.Str(yamlx.Get(m, "resource_class")); rc != "" {
			ps.resourceClass(j, rc, where+".machine")
		}
	}
	if m := yamlx.Get(n, "macos"); m != nil {
		j.Image, j.Services = "", nil
		j.Runner = ir.Runner{OS: ir.OSMacOS}
		d.Infof(where+".macos", "Xcode version %s was dropped; pick a matching macOS runner", yamlx.Str(yamlx.Get(m, "xcode")))
	}
	if rc := yamlx.Str(yamlx.Get(n, "resource_class")); rc != "" {
		ps.resourceClass(j, rc, where)
	}
	for _, kv := range parseEnv(yamlx.Get(n, "environment")) {
		j.Env = ir.KVSet(j.Env, kv.Key, kv.Value)
	}
	if wd := yamlx.Str(yamlx.Get(n, "working_directory")); wd != "" {
		ex.workdir = wd
	}
	if sh := yamlx.Str(yamlx.Get(n, "shell")); sh != "" {
		ex.shell = sh
	}
}

func (ps *parser) resourceClass(j *ir.Job, rc, where string) {
	switch {
	case strings.Contains(rc, "/"):
		j.Runner.Labels = []string{rc}
		ps.d.Infof(where+".resource_class", "self-hosted runner class %q was kept as a runner label", rc)
	case strings.HasPrefix(rc, "windows."):
		j.Runner.OS = ir.OSWindows
	case strings.HasPrefix(rc, "macos."):
		j.Runner.OS = ir.OSMacOS
	default:
		ps.d.Infof(where+".resource_class", "resource class %q was dropped", rc)
	}
}

func isDefaultWorkdir(wd string) bool {
	switch strings.TrimSuffix(wd, "/") {
	case "", ".", "~/project", "/home/circleci/project", "$CIRCLE_WORKING_DIRECTORY", "${CIRCLE_WORKING_DIRECTORY}":
		return true
	}
	return false
}

// relWorkdir returns wd relative to the default checkout directory, or ""
// when it is the default or lies elsewhere.
func relWorkdir(wd string) string {
	if isDefaultWorkdir(wd) {
		return ""
	}
	for _, base := range []string{"~/project/", "/home/circleci/project/"} {
		if strings.HasPrefix(wd, base) {
			return strings.TrimSuffix(strings.TrimPrefix(wd, base), "/")
		}
	}
	if !path.IsAbs(wd) && !strings.HasPrefix(wd, "~") && !strings.HasPrefix(wd, "$") {
		return strings.TrimSuffix(wd, "/")
	}
	return ""
}

func serviceAlias(image string) string {
	name, _, _ := strings.Cut(image, ":")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return ir.Slug(name)
}

// parseEnv reads an environment map or a list of KEY=VALUE strings.
func parseEnv(n *yaml.Node) []ir.KV {
	var out []ir.KV
	switch {
	case yamlx.IsMap(n):
		for _, pair := range yamlx.Pairs(n) {
			out = append(out, ir.KV{Key: pair.Key, Value: fromCC(yamlx.Str(pair.Value))})
		}
	case yamlx.IsSeq(n):
		for _, item := range n.Content {
			if yamlx.IsMap(item) {
				out = append(out, parseEnv(item)...)
				continue
			}
			if k, v, ok := strings.Cut(yamlx.Str(item), "="); ok {
				out = append(out, ir.KV{Key: k, Value: fromCC(v)})
			}
		}
	}
	return out
}

// normalizeShell maps a CircleCI shell command line to a portable name;
// the default bash shell becomes "".
func normalizeShell(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	switch {
	case l == "":
		return ""
	case strings.Contains(l, "bash"):
		return ""
	case strings.HasPrefix(l, "/bin/sh") || l == "sh":
		return "sh"
	case strings.Contains(l, "pwsh"):
		return "pwsh"
	case strings.Contains(l, "powershell"):
		return "powershell"
	case strings.HasPrefix(l, "cmd"):
		return "cmd"
	case strings.Contains(l, "python"):
		return "python"
	}
	return s
}

func stepKey(n *yaml.Node) (string, *yaml.Node) {
	switch {
	case yamlx.IsScalar(n):
		return n.Value, nil
	case yamlx.IsMap(n) && len(n.Content) == 2:
		return n.Content[0].Value, n.Content[1]
	}
	return "", nil
}

// expandSteps inlines reusable commands and compile-time when/unless steps.
func (ps *parser) expandSteps(items []*yaml.Node, where string, depth int) []*yaml.Node {
	commands := yamlx.Get(ps.root, "commands")
	var out []*yaml.Node
	for _, it := range items {
		name, args := stepKey(it)
		switch {
		case name == "when" || name == "unless":
			v, known := evalLogic(yamlx.Get(args, "condition"))
			if !known {
				ps.d.Warnf(where, "%s condition depends on runtime values; its steps always run", name)
				v = name == "when"
			}
			if name == "unless" {
				v = !v
			}
			if v {
				body := yamlx.Get(args, "steps")
				if !yamlx.IsSeq(body) {
					body = &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{body}}
				}
				out = append(out, ps.expandSteps(yamlx.Items(body), where, depth+1)...)
			}
		case !builtinSteps[name] && yamlx.Get(commands, name) != nil:
			if depth >= maxCommandDepth {
				ps.d.Warnf(where, "command %q nests too deeply and was dropped", name)
				continue
			}
			cmd := yamlx.Get(commands, name)
			params := ps.bindParams(yamlx.Get(cmd, "parameters"), func(n string) *yaml.Node {
				return yamlx.Get(args, n)
			}, "commands."+name)
			body := (&scope{ps: ps, params: params, path: "commands." + name}).subst(yamlx.Get(cmd, "steps"))
			out = append(out, ps.expandSteps(yamlx.Items(body), where, depth+1)...)
		default:
			out = append(out, it)
		}
	}
	return out
}

func stepWhen(args *yaml.Node) string {
	switch yamlx.Str(yamlx.Get(args, "when")) {
	case "always":
		return ir.WhenAlways
	case "on_fail":
		return ir.WhenOnFailure
	}
	return ""
}

// parseSteps converts expanded native steps.
func (ps *parser) parseSteps(items []*yaml.Node, job string) []*ir.Step {
	d := ps.d
	var out []*ir.Step
	fromStore := map[*ir.Step]bool{}
	last := func() *ir.Step {
		if len(out) == 0 {
			return nil
		}
		return out[len(out)-1]
	}
	for i, it := range items {
		where := fmt.Sprintf("%s.steps[%d]", job, i)
		name, args := stepKey(it)
		switch name {
		case "checkout":
			co := &ir.Checkout{}
			if v, ok := yamlx.Int(yamlx.Get(args, "depth")); ok {
				co.Depth = v
			}
			if yamlx.Str(yamlx.Get(args, "method")) == "full" {
				co.Depth = -1
			}
			if yamlx.Has(args, "path") {
				d.Infof(where, "checkout path %q was dropped", yamlx.Str(yamlx.Get(args, "path")))
			}
			out = append(out, &ir.Step{Kind: ir.StepCheckout, Checkout: co})
		case "run":
			st := ps.parseRun(args, where)
			if prev := last(); prev != nil && prev.Kind == ir.StepCheckout && st.If == nil {
				switch st.Run {
				case submoduleCmd:
					prev.Checkout.Submodules = true
					continue
				case lfsCmd:
					prev.Checkout.LFS = true
					continue
				}
			}
			out = append(out, st)
		case "restore_cache":
			keys := yamlx.StrList(yamlx.Get(args, "keys"))
			if len(keys) == 0 {
				keys = yamlx.StrList(yamlx.Get(args, "key"))
			}
			if len(keys) == 0 {
				continue
			}
			c := ps.parseCacheKey(keys[0], where)
			out = append(out, &ir.Step{Kind: ir.StepCache, Name: yamlx.Str(yamlx.Get(args, "name")), Cache: c})
		case "save_cache":
			c := ps.parseCacheKey(yamlx.Str(yamlx.Get(args, "key")), where)
			paths := yamlx.StrList(yamlx.Get(args, "paths"))
			matched := false
			for _, st := range out {
				if st.Kind == ir.StepCache && st.Cache.Prefix == c.Prefix && len(st.Cache.Paths) == 0 {
					st.Cache.Paths = paths
					if len(st.Cache.Files) == 0 {
						st.Cache.Files = c.Files
					}
					matched = true
					break
				}
			}
			if !matched {
				d.Infof(where, "save_cache without a matching restore_cache became a cache step")
				c.Paths = paths
				out = append(out, &ir.Step{Kind: ir.StepCache, Cache: c})
			}
		case "persist_to_workspace":
			root := yamlx.Str(yamlx.Get(args, "root"))
			paths := yamlx.StrList(yamlx.Get(args, "paths"))
			paths = append(paths, yamlx.StrList(yamlx.Get(args, "path"))...)
			if r := relWorkdir(root); r != "" {
				for k := range paths {
					paths[k] = path.Join(r, paths[k])
				}
			} else if !isDefaultWorkdir(root) {
				d.Infof(where, "workspace root %q was dropped; paths are taken relative to the checkout", root)
			}
			out = append(out, &ir.Step{Kind: ir.StepUpload, Artifact: &ir.Artifact{Paths: paths, When: stepWhen(args)}})
		case "attach_workspace":
			if at := yamlx.Str(yamlx.Get(args, "at")); !isDefaultWorkdir(at) {
				d.Infof(where, "workspace attach directory %q was dropped; artifacts are restored into the checkout", at)
			}
			out = append(out, &ir.Step{Kind: ir.StepDownload, Artifact: &ir.Artifact{}})
		case "store_artifacts":
			p := yamlx.Str(yamlx.Get(args, "path"))
			dest := yamlx.Str(yamlx.Get(args, "destination"))
			if n, _, ok := strings.Cut(dest, "/"); ok {
				dest = n
			}
			if merged := mergeArtifact(out, p, dest); merged {
				continue
			}
			if prev := last(); prev != nil && fromStore[prev] && prev.Artifact.Name == dest {
				prev.Artifact.Paths = append(prev.Artifact.Paths, p)
				continue
			}
			st := &ir.Step{Kind: ir.StepUpload, Artifact: &ir.Artifact{Name: dest, Paths: []string{p}, When: stepWhen(args)}}
			fromStore[st] = true
			out = append(out, st)
		case "store_test_results":
			d.Infof(where, "test results were kept as a plain artifact")
			out = append(out, &ir.Step{Kind: ir.StepUpload, Artifact: &ir.Artifact{
				Name: "test-results", Paths: []string{yamlx.Str(yamlx.Get(args, "path"))}, When: stepWhen(args),
			}})
		case "setup_remote_docker":
			d.Infof(where, "setup_remote_docker was dropped; docker is available on most runners")
		case "add_ssh_keys":
			d.Warnf(where, "add_ssh_keys is not portable; configure deploy keys in the target CI")
		case "":
			d.Warnf(where, "unrecognized step was dropped")
		default:
			if st := ps.orbStep(name, args, where); st != nil {
				out = append(out, st)
			}
		}
	}
	var kept []*ir.Step
	for i, st := range out {
		if st.Kind == ir.StepCache && len(st.Cache.Paths) == 0 {
			d.Warnf(fmt.Sprintf("%s.steps[%d]", job, i), "restore_cache without a matching save_cache was dropped")
			continue
		}
		kept = append(kept, st)
	}
	return kept
}

// mergeArtifact folds a store_artifacts path into an upload that already
// publishes it (typically a persist_to_workspace step).
func mergeArtifact(steps []*ir.Step, p, name string) bool {
	for _, st := range steps {
		if st.Kind != ir.StepUpload {
			continue
		}
		for _, q := range st.Artifact.Paths {
			if path.Clean(q) == path.Clean(p) {
				if st.Artifact.Name == "" {
					st.Artifact.Name = name
				}
				return true
			}
		}
	}
	return false
}

func (ps *parser) parseRun(args *yaml.Node, where string) *ir.Step {
	st := &ir.Step{Kind: ir.StepRun}
	if yamlx.IsScalar(args) {
		st.Run = fromCC(strings.TrimRight(args.Value, "\n"))
	} else {
		st.Run = fromCC(strings.TrimRight(yamlx.Str(yamlx.Get(args, "command")), "\n"))
		st.Name = fromCC(yamlx.Str(yamlx.Get(args, "name")))
		st.WorkingDir = fromCC(yamlx.Str(yamlx.Get(args, "working_directory")))
		st.Env = parseEnv(yamlx.Get(args, "environment"))
		st.Shell = normalizeShell(yamlx.Str(yamlx.Get(args, "shell")))
		if w := stepWhen(args); w != "" {
			st.If = &ir.Condition{When: w}
		}
		if v, ok := yamlx.Bool(yamlx.Get(args, "background")); ok && v {
			ps.d.Warnf(where, "background steps are not portable; the step runs in the foreground")
		}
		if t := yamlx.Str(yamlx.Get(args, "no_output_timeout")); t != "" {
			if m, ok := ir.ParseMinutes(t); ok {
				st.TimeoutMinutes = m
				ps.d.Infof(where, "no_output_timeout %s became a step timeout", t)
			}
		}
		if hasAny(args, "max_auto_reruns", "teardown") {
			ps.d.Warnf(where, "automatic reruns and teardown steps are not portable and were dropped")
		}
	}
	// The emitter renders allowed failures as "|| true".
	switch {
	case strings.HasPrefix(st.Run, "(\n") && strings.HasSuffix(st.Run, "\n) || true"):
		st.Run = dedent(strings.TrimSuffix(strings.TrimPrefix(st.Run, "(\n"), "\n) || true"))
		st.AllowFailure = true
	case !strings.Contains(st.Run, "\n") && strings.HasSuffix(st.Run, " || true"):
		st.Run = strings.TrimSuffix(st.Run, " || true")
		st.AllowFailure = true
	}
	return st
}

func dedent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "  ")
	}
	return strings.Join(lines, "\n")
}

// parseCacheKey extracts the prefix and checksummed files of a key
// template such as "v1-deps-{{ checksum "package-lock.json" }}".
func (ps *parser) parseCacheKey(key, where string) *ir.Cache {
	c := &ir.Cache{}
	for _, m := range checksumRe.FindAllStringSubmatch(key, -1) {
		c.Files = append(c.Files, m[1])
	}
	for _, t := range templateRe.FindAllString(key, -1) {
		if checksumRe.MatchString(t) || strings.Contains(t, "epoch") {
			continue
		}
		ps.d.Infof(where, "cache key template %s was dropped", t)
	}
	lit := key
	if i := strings.Index(key, "{{"); i >= 0 {
		lit = key[:i]
	}
	lit = strings.Trim(lit, "-_. ")
	c.Prefix = ir.Slug(lit)
	if lit == "" {
		c.Prefix = "deps"
	}
	return c
}

// orbStep converts an orb command invocation.
func (ps *parser) orbStep(name string, args *yaml.Node, where string) *ir.Step {
	orb, _, isOrb := strings.Cut(name, "/")
	if !isOrb {
		ps.d.Warnf(where, "unknown step %q was dropped", name)
		return nil
	}
	ref, declared := ps.orbs[orb]
	if !declared {
		ps.d.Warnf(where, "orb %q is not declared under orbs", orb)
	}
	arg := func(k string) string { return fromCC(yamlx.Str(yamlx.Get(args, k))) }
	switch name {
	case "node/install":
		return &ir.Step{Kind: ir.StepTool, Tool: ir.ToolNode, Version: arg("node-version")}
	case "go/install":
		return &ir.Step{Kind: ir.StepTool, Tool: ir.ToolGo, Version: arg("version")}
	}
	st := &ir.Step{Kind: ir.StepAction, Uses: name, UsesFrom: "circleci"}
	for _, pair := range yamlx.Pairs(args) {
		if !yamlx.IsScalar(pair.Value) {
			ps.d.Warnf(where, "non-scalar orb parameter %q was dropped", pair.Key)
			continue
		}
		st.With = append(st.With, ir.KV{Key: pair.Key, Value: fromCC(pair.Value.Value)})
	}
	ps.d.Warnf(where, "orb command %q (%s) is not portable and was kept as a CircleCI step", name, ref)
	return st
}
