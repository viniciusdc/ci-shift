package github

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viniciusdc/cistela/internal/yamlx"
	"github.com/viniciusdc/cistela/ir"
)

// Parse implements ir.Provider.
func (Provider) Parse(data []byte, d *ir.Diagnostics) (*ir.Pipeline, error) {
	root, err := yamlx.Load(data)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	if !yamlx.IsMap(root) {
		return nil, fmt.Errorf("github: workflow must be a mapping")
	}
	p := &ir.Pipeline{Name: yamlx.Str(yamlx.Get(root, "name"))}
	p.Triggers = parseOn(yamlx.Get(root, "on"), d)
	p.Env = parseEnv(yamlx.Get(root, "env"))
	for _, key := range []string{"permissions", "concurrency", "defaults", "run-name"} {
		if yamlx.Has(root, key) {
			d.Infof(key, "%q has no portable equivalent and was dropped", key)
		}
	}

	jobs := yamlx.Get(root, "jobs")
	if !yamlx.IsMap(jobs) {
		return nil, fmt.Errorf("github: workflow has no jobs")
	}
	for _, pair := range yamlx.Pairs(jobs) {
		j, err := parseJob(pair.Key, pair.Value, d)
		if err != nil {
			return nil, err
		}
		p.Jobs = append(p.Jobs, j)
	}
	return p, nil
}

func parseEnv(n *yaml.Node) []ir.KV {
	var out []ir.KV
	for _, pair := range yamlx.Pairs(n) {
		out = append(out, ir.KV{Key: pair.Key, Value: fromGH(yamlx.Str(pair.Value))})
	}
	return out
}

func parseOn(n *yaml.Node, d *ir.Diagnostics) ir.Triggers {
	var t ir.Triggers
	events := map[string]*yaml.Node{}
	var order []string
	switch {
	case yamlx.IsScalar(n):
		order = []string{n.Value}
	case yamlx.IsSeq(n):
		order = yamlx.StrList(n)
	case yamlx.IsMap(n):
		for _, pair := range yamlx.Pairs(n) {
			order = append(order, pair.Key)
			events[pair.Key] = pair.Value
		}
	}
	for _, ev := range order {
		cfg := events[ev]
		switch ev {
		case "push":
			t.Push = &ir.PushTrigger{
				Branches:       yamlx.StrList(yamlx.Get(cfg, "branches")),
				BranchesIgnore: yamlx.StrList(yamlx.Get(cfg, "branches-ignore")),
				Tags:           yamlx.StrList(yamlx.Get(cfg, "tags")),
				Paths:          yamlx.StrList(yamlx.Get(cfg, "paths")),
				PathsIgnore:    yamlx.StrList(yamlx.Get(cfg, "paths-ignore")),
			}
			if yamlx.Has(cfg, "tags-ignore") {
				d.Warnf("on.push.tags-ignore", "tag ignore filters are not portable and were dropped")
			}
		case "pull_request", "pull_request_target":
			t.PullRequest = &ir.PRTrigger{
				Branches: yamlx.StrList(yamlx.Get(cfg, "branches")),
				Paths:    yamlx.StrList(yamlx.Get(cfg, "paths")),
			}
			if ev == "pull_request_target" {
				d.Warnf("on.pull_request_target", "pull_request_target was converted to a regular pull request trigger")
			}
		case "schedule":
			for _, item := range yamlx.Items(cfg) {
				if cron := yamlx.Str(yamlx.Get(item, "cron")); cron != "" {
					t.Schedules = append(t.Schedules, ir.Schedule{Cron: cron})
				}
			}
		case "workflow_dispatch":
			t.Manual = true
			if yamlx.Has(cfg, "inputs") {
				d.Warnf("on.workflow_dispatch.inputs", "manual run inputs are not portable and were dropped")
			}
		default:
			d.Warnf("on."+ev, "trigger %q has no portable equivalent and was dropped", ev)
		}
	}
	return t
}

func parseJob(id string, n *yaml.Node, d *ir.Diagnostics) (*ir.Job, error) {
	path := "jobs." + id
	if yamlx.Has(n, "uses") {
		d.Warnf(path, "reusable workflow call %q cannot be converted; a placeholder job was created", yamlx.Str(yamlx.Get(n, "uses")))
		return &ir.Job{ID: id, Name: yamlx.Str(yamlx.Get(n, "name")), Steps: []*ir.Step{{
			Kind: ir.StepAction, Name: "Reusable workflow", Uses: yamlx.Str(yamlx.Get(n, "uses")), UsesFrom: "github",
		}}, Needs: yamlx.StrList(yamlx.Get(n, "needs"))}, nil
	}
	j := &ir.Job{
		ID:    id,
		Name:  fromGH(yamlx.Str(yamlx.Get(n, "name"))),
		Needs: yamlx.StrList(yamlx.Get(n, "needs")),
		Env:   parseEnv(yamlx.Get(n, "env")),
	}
	j.Runner = parseRunsOn(yamlx.Get(n, "runs-on"))
	if c := yamlx.Get(n, "container"); c != nil {
		if yamlx.IsScalar(c) {
			j.Image = fromGH(c.Value)
		} else {
			j.Image = fromGH(yamlx.Str(yamlx.Get(c, "image")))
			if env := parseEnv(yamlx.Get(c, "env")); len(env) > 0 {
				j.Env = append(j.Env, env...)
			}
			if yamlx.Has(c, "credentials") || yamlx.Has(c, "options") {
				d.Warnf(path+".container", "container credentials/options were dropped")
			}
		}
	}
	for _, pair := range yamlx.Pairs(yamlx.Get(n, "services")) {
		svc := ir.Service{Name: pair.Key}
		if yamlx.IsScalar(pair.Value) {
			svc.Image = fromGH(pair.Value.Value)
		} else {
			svc.Image = fromGH(yamlx.Str(yamlx.Get(pair.Value, "image")))
			svc.Env = parseEnv(yamlx.Get(pair.Value, "env"))
			svc.Ports = yamlx.StrList(yamlx.Get(pair.Value, "ports"))
		}
		j.Services = append(j.Services, svc)
	}
	if cond := yamlx.Str(yamlx.Get(n, "if")); cond != "" {
		j.If = parseIf(cond)
	}
	if v, ok := yamlx.Int(yamlx.Get(n, "timeout-minutes")); ok {
		j.TimeoutMinutes = v
	}
	if v, ok := yamlx.Bool(yamlx.Get(n, "continue-on-error")); ok {
		j.AllowFailure = v
	}
	if env := yamlx.Get(n, "environment"); env != nil {
		if yamlx.IsScalar(env) {
			j.Environment = env.Value
		} else {
			j.Environment = yamlx.Str(yamlx.Get(env, "name"))
		}
	}
	if m := yamlx.Path(n, "strategy", "matrix"); m != nil {
		j.Matrix = parseMatrix(m, path+".strategy.matrix", d)
	}
	if yamlx.Has(n, "outputs") {
		d.Warnf(path+".outputs", "job outputs are not portable and were dropped")
	}
	for i, st := range yamlx.Items(yamlx.Get(n, "steps")) {
		step, err := parseStep(st, fmt.Sprintf("%s.steps[%d]", path, i), d)
		if err != nil {
			return nil, err
		}
		j.Steps = append(j.Steps, step)
	}
	return j, nil
}

func parseRunsOn(n *yaml.Node) ir.Runner {
	var labels []string
	switch {
	case yamlx.IsMap(n):
		labels = yamlx.StrList(yamlx.Get(n, "labels"))
		if g := yamlx.Str(yamlx.Get(n, "group")); g != "" {
			labels = append([]string{g}, labels...)
		}
	default:
		labels = yamlx.StrList(n)
	}
	var r ir.Runner
	if len(labels) == 1 && !strings.EqualFold(labels[0], "self-hosted") {
		r.VMImage = fromGH(labels[0])
		r.OS = osFromLabel(labels[0])
		return r
	}
	for _, l := range labels {
		switch strings.ToLower(l) {
		case "self-hosted":
		case "linux":
			r.OS = ir.OSLinux
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

func osFromLabel(l string) string {
	l = strings.ToLower(l)
	switch {
	case strings.HasPrefix(l, "windows"):
		return ir.OSWindows
	case strings.HasPrefix(l, "macos"):
		return ir.OSMacOS
	}
	return ir.OSLinux
}

func parseMatrix(n *yaml.Node, path string, d *ir.Diagnostics) *ir.Matrix {
	m := &ir.Matrix{}
	if yamlx.IsScalar(n) {
		d.Warnf(path, "dynamic matrix %q cannot be converted", n.Value)
		return nil
	}
	for _, pair := range yamlx.Pairs(n) {
		switch pair.Key {
		case "include":
			m.Include = parseCombos(pair.Value)
		case "exclude":
			m.Exclude = parseCombos(pair.Value)
		default:
			if !yamlx.IsSeq(pair.Value) {
				d.Warnf(path+"."+pair.Key, "non-list matrix axis was dropped")
				continue
			}
			ax := ir.Axis{Name: pair.Key}
			for _, v := range pair.Value.Content {
				if !yamlx.IsScalar(v) {
					d.Warnf(path+"."+pair.Key, "only scalar matrix values are portable; an object value was dropped")
					continue
				}
				ax.Values = append(ax.Values, v.Value)
			}
			m.Axes = append(m.Axes, ax)
		}
	}
	return m
}

func parseCombos(n *yaml.Node) [][]ir.KV {
	var out [][]ir.KV
	for _, item := range yamlx.Items(n) {
		var combo []ir.KV
		for _, pair := range yamlx.Pairs(item) {
			combo = append(combo, ir.KV{Key: pair.Key, Value: yamlx.Str(pair.Value)})
		}
		out = append(out, combo)
	}
	return out
}

func parseStep(n *yaml.Node, path string, d *ir.Diagnostics) (*ir.Step, error) {
	st := &ir.Step{
		Name:       fromGH(yamlx.Str(yamlx.Get(n, "name"))),
		Env:        parseEnv(yamlx.Get(n, "env")),
		WorkingDir: fromGH(yamlx.Str(yamlx.Get(n, "working-directory"))),
	}
	if cond := yamlx.Str(yamlx.Get(n, "if")); cond != "" {
		st.If = parseIf(cond)
	}
	if v, ok := yamlx.Bool(yamlx.Get(n, "continue-on-error")); ok {
		st.AllowFailure = v
	}
	if v, ok := yamlx.Int(yamlx.Get(n, "timeout-minutes")); ok {
		st.TimeoutMinutes = v
	}
	if yamlx.Has(n, "id") {
		d.Infof(path, "step id %q was dropped; step outputs are not portable", yamlx.Str(yamlx.Get(n, "id")))
	}
	if run := yamlx.Get(n, "run"); run != nil {
		st.Kind = ir.StepRun
		st.Run = fromGH(strings.TrimRight(yamlx.Str(run), "\n"))
		st.Shell = yamlx.Str(yamlx.Get(n, "shell"))
		return st, nil
	}
	uses := yamlx.Str(yamlx.Get(n, "uses"))
	if uses == "" {
		return nil, fmt.Errorf("github: %s: step has neither run nor uses", path)
	}
	with := yamlx.Get(n, "with")
	w := func(k string) string { return fromGH(yamlx.Str(yamlx.Get(with, k))) }
	action, _, _ := strings.Cut(uses, "@")
	switch strings.ToLower(action) {
	case "actions/checkout":
		st.Kind = ir.StepCheckout
		st.Checkout = &ir.Checkout{}
		if v, ok := yamlx.Int(yamlx.Get(with, "fetch-depth")); ok {
			st.Checkout.Depth = v
			if v == 0 {
				st.Checkout.Depth = -1 // full history
			}
		}
		if v := w("submodules"); v == "true" || v == "recursive" {
			st.Checkout.Submodules = true
		}
		st.Checkout.LFS = w("lfs") == "true"
		return st, nil
	case "actions/setup-node":
		return toolStep(st, ir.ToolNode, w("node-version")), nil
	case "actions/setup-python":
		return toolStep(st, ir.ToolPython, w("python-version")), nil
	case "actions/setup-go":
		return toolStep(st, ir.ToolGo, w("go-version")), nil
	case "actions/setup-java":
		return toolStep(st, ir.ToolJava, w("java-version")), nil
	case "ruby/setup-ruby":
		return toolStep(st, ir.ToolRuby, w("ruby-version")), nil
	case "actions/setup-dotnet":
		return toolStep(st, ir.ToolDotnet, w("dotnet-version")), nil
	case "actions/cache", "actions/cache/restore":
		st.Kind = ir.StepCache
		st.Cache = parseCacheKey(w("key"))
		st.Cache.Paths = lines(w("path"))
		return st, nil
	case "actions/upload-artifact":
		st.Kind = ir.StepUpload
		st.Artifact = &ir.Artifact{Name: w("name"), Paths: lines(w("path"))}
		if days := w("retention-days"); days != "" {
			st.Artifact.ExpireIn = days + " days"
		}
		if st.If != nil && st.If.When != "" && len(st.If.Branches) == 0 && st.If.Event == "" && st.If.Raw == "" {
			st.Artifact.When = st.If.When
			st.If = nil
		}
		return st, nil
	case "actions/download-artifact":
		st.Kind = ir.StepDownload
		st.Artifact = &ir.Artifact{Name: w("name")}
		if st.Artifact.Name == "" {
			// Matrix uploads are fetched with pattern: <name>-*.
			st.Artifact.Name = strings.TrimSuffix(strings.TrimSuffix(w("pattern"), "*"), "-")
		}
		return st, nil
	}
	st.Kind = ir.StepAction
	st.Uses = uses
	st.UsesFrom = "github"
	for _, pair := range yamlx.Pairs(with) {
		st.With = append(st.With, ir.KV{Key: pair.Key, Value: fromGH(yamlx.Str(pair.Value))})
	}
	return st, nil
}

func toolStep(st *ir.Step, tool, version string) *ir.Step {
	st.Kind = ir.StepTool
	st.Tool = tool
	st.Version = version
	return st
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var hashFilesRe = regexp.MustCompile(`hashFiles\(([^)]*)\)`)
var quotedRe = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// parseCacheKey extracts the prefix and lockfiles from a cache key such as
// "${{ runner.os }}-node-${{ hashFiles('**/package-lock.json') }}".
func parseCacheKey(key string) *ir.Cache {
	c := &ir.Cache{}
	for _, m := range hashFilesRe.FindAllStringSubmatch(key, -1) {
		for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
			f := q[1] + q[2]
			c.Files = append(c.Files, strings.TrimPrefix(f, "**/"))
		}
	}
	prefix := ghExprRe.ReplaceAllString(key, "")
	prefix = ir.RewriteRefs(prefix, func(string, string) string { return "" })
	prefix = strings.Trim(prefix, "-_ ")
	c.Prefix = ir.Slug(prefix)
	if prefix == "" {
		c.Prefix = "deps"
	}
	return c
}

var (
	refEqRe     = regexp.MustCompile(`^github\.ref\s*==\s*'refs/heads/([^']+)'$`)
	refNameEqRe = regexp.MustCompile(`^github\.ref_name\s*==\s*'([^']+)'$`)
	tagPrefixRe = regexp.MustCompile(`^startsWith\(\s*github\.ref\s*,\s*'refs/tags/?'?\s*\)$`)
	branchPfxRe = regexp.MustCompile(`^startsWith\(\s*github\.ref\s*,\s*'refs/heads/([^']*)'\s*\)$`)
	eventEqRe   = regexp.MustCompile(`^github\.event_name\s*==\s*'([^']+)'$`)
)

// parseIf translates common GitHub conditions; anything else is kept raw.
func parseIf(expr string) *ir.Condition {
	raw := strings.TrimSpace(expr)
	e := raw
	if strings.HasPrefix(e, "${{") && strings.HasSuffix(e, "}}") {
		e = strings.TrimSpace(e[3 : len(e)-2])
	}
	c := &ir.Condition{}
	fail := func() *ir.Condition { return &ir.Condition{Raw: e, RawFrom: "github"} }

	if strings.Contains(e, "||") && !strings.Contains(e, "&&") {
		for _, part := range strings.Split(trimParens(e), "||") {
			b, ok := branchOf(strings.TrimSpace(part))
			if !ok {
				return fail()
			}
			c.Branches = append(c.Branches, b)
		}
		return c
	}
	for _, part := range strings.Split(e, "&&") {
		part = trimParens(strings.TrimSpace(part))
		if strings.Contains(part, "||") {
			sub := parseIf(part)
			if sub.Raw != "" || len(sub.Branches) == 0 {
				return fail()
			}
			c.Branches = append(c.Branches, sub.Branches...)
			continue
		}
		switch {
		case part == "always()":
			c.When = ir.WhenAlways
		case part == "failure()":
			c.When = ir.WhenOnFailure
		case part == "success()":
		case tagPrefixRe.MatchString(part):
			c.Event = ir.EventTag
		case eventEqRe.MatchString(part):
			switch eventEqRe.FindStringSubmatch(part)[1] {
			case "push":
				c.Event = ir.EventPush
			case "pull_request", "pull_request_target":
				c.Event = ir.EventPullRequest
			case "schedule":
				c.Event = ir.EventSchedule
			case "workflow_dispatch":
				c.Event = ir.EventManual
			default:
				return fail()
			}
		default:
			b, ok := branchOf(part)
			if !ok {
				return fail()
			}
			c.Branches = append(c.Branches, b)
		}
	}
	return c
}

func branchOf(part string) (string, bool) {
	if m := refEqRe.FindStringSubmatch(part); m != nil {
		return m[1], true
	}
	if m := refNameEqRe.FindStringSubmatch(part); m != nil {
		return m[1], true
	}
	if m := branchPfxRe.FindStringSubmatch(part); m != nil {
		return m[1] + "*", true
	}
	return "", false
}

func trimParens(s string) string {
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balanced(s[1:len(s)-1]) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}
