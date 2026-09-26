package gitlab

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/viniciusdc/ci-shift/internal/yamlx"
	"github.com/viniciusdc/ci-shift/ir"
)

// Emit implements ir.Provider.
func (Provider) Emit(p *ir.Pipeline, d *ir.Diagnostics) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	stages, stageOf, err := p.StageNames()
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	taken := map[string]bool{}
	for k := range reserved {
		taken[k] = true
	}
	for _, j := range p.Jobs {
		base := j.ID
		if reserved[base] || strings.HasPrefix(base, ".") || base == "pages" {
			base += "-job"
		}
		names[j.ID] = ir.UniqueID(base, taken)
	}

	root := yamlx.NewMap()
	if wf := emitWorkflow(p, d); wf.Len() > 0 {
		root.Set("workflow", wf)
	}
	root.Set("stages", stages)
	if len(p.Env) > 0 {
		vars := yamlx.NewMap()
		for _, kv := range p.Env {
			vars.Set(kv.Key, toGL(kv.Value))
		}
		root.Set("variables", vars)
	}
	for _, j := range p.Jobs {
		// GitLab jobs have no display name; the key is the name.
		root.Set(names[j.ID], emitJob(p, j, names, stageOf[j.ID], d))
	}
	return yamlx.Marshal(root, "")
}

func branchCompare(b string) string {
	if ir.IsGlob(b) {
		return fmt.Sprintf("$CI_COMMIT_BRANCH =~ /%s/", ir.GlobToRegex(b))
	}
	return fmt.Sprintf("$CI_COMMIT_BRANCH == %q", b)
}

func emitWorkflow(p *ir.Pipeline, d *ir.Diagnostics) *yamlx.Map {
	wf := yamlx.NewMap()
	if p.Name != "" {
		wf.Set("name", toGL(p.Name))
	}
	t := p.Triggers
	if t.Empty() {
		return wf
	}
	// A plain "any push" trigger is GitLab's default behavior.
	if t.PullRequest == nil && len(t.Schedules) == 0 && !t.Manual && t.Push != nil &&
		len(t.Push.Branches) == 0 && len(t.Push.BranchesIgnore) == 0 && len(t.Push.Tags) == 0 &&
		len(t.Push.Paths) == 0 {
		return wf
	}
	var rules []any
	rule := func(expr string) *yamlx.Map { return yamlx.NewMap().Set("if", expr) }
	if push := t.Push; push != nil {
		for _, b := range push.BranchesIgnore {
			rules = append(rules, rule(branchCompare(b)).Set("when", "never"))
		}
		withChanges := func(m *yamlx.Map) *yamlx.Map {
			if len(push.Paths) > 0 {
				m.Set("changes", push.Paths)
			}
			return m
		}
		if len(push.PathsIgnore) > 0 {
			d.Warnf("workflow", "paths-ignore filters are not supported by GitLab rules and were dropped")
		}
		switch {
		case len(push.Branches) > 0:
			for _, b := range push.Branches {
				rules = append(rules, withChanges(rule(branchCompare(b))))
			}
		case len(push.Tags) == 0:
			rules = append(rules, withChanges(rule("$CI_COMMIT_BRANCH")))
		}
		for _, tag := range push.Tags {
			if tag == "*" || tag == "**" {
				rules = append(rules, rule("$CI_COMMIT_TAG"))
			} else {
				rules = append(rules, rule(fmt.Sprintf("$CI_COMMIT_TAG =~ /%s/", ir.GlobToRegex(tag))))
			}
		}
	}
	if pr := t.PullRequest; pr != nil {
		const mr = `$CI_PIPELINE_SOURCE == "merge_request_event"`
		if len(pr.Branches) == 0 {
			r := rule(mr)
			if len(pr.Paths) > 0 {
				r.Set("changes", pr.Paths)
			}
			rules = append(rules, r)
		}
		for _, b := range pr.Branches {
			cmp := fmt.Sprintf("$CI_MERGE_REQUEST_TARGET_BRANCH_NAME == %q", b)
			if ir.IsGlob(b) {
				cmp = fmt.Sprintf("$CI_MERGE_REQUEST_TARGET_BRANCH_NAME =~ /%s/", ir.GlobToRegex(b))
			}
			r := rule(mr + " && " + cmp)
			if len(pr.Paths) > 0 {
				r.Set("changes", pr.Paths)
			}
			rules = append(rules, r)
		}
	}
	if len(t.Schedules) > 0 {
		var crons []string
		for _, s := range t.Schedules {
			if s.Cron != "" {
				crons = append(crons, s.Cron)
			}
		}
		if len(crons) > 0 {
			d.Warnf("workflow", "create a pipeline schedule in GitLab (Build > Pipeline schedules) for cron %s", strings.Join(crons, ", "))
		}
		rules = append(rules, rule(`$CI_PIPELINE_SOURCE == "schedule"`))
	}
	if t.Manual {
		rules = append(rules, rule(`$CI_PIPELINE_SOURCE == "web"`))
	}
	wf.Set("rules", rules)
	return wf
}

func conditionRule(c *ir.Condition, d *ir.Diagnostics, path string) (*yamlx.Map, bool) {
	if c.IsZero() {
		return nil, false
	}
	r := yamlx.NewMap()
	var parts []string
	if c.Raw != "" {
		if c.RawFrom == "gitlab" {
			parts = append(parts, c.Raw)
		} else {
			d.Warnf(path, "condition %q (from %s) could not be translated and was dropped", c.Raw, c.RawFrom)
		}
	}
	switch c.Event {
	case ir.EventTag:
		parts = append(parts, "$CI_COMMIT_TAG")
	case ir.EventPullRequest:
		parts = append(parts, `$CI_PIPELINE_SOURCE == "merge_request_event"`)
	case ir.EventSchedule:
		parts = append(parts, `$CI_PIPELINE_SOURCE == "schedule"`)
	case ir.EventManual:
		parts = append(parts, `$CI_PIPELINE_SOURCE == "web"`)
	case ir.EventPush:
		parts = append(parts, `$CI_PIPELINE_SOURCE == "push"`)
	}
	branchRules := len(c.Branches)
	switch c.When {
	case ir.WhenAlways:
		r.Set("when", "always")
	case ir.WhenOnFailure:
		r.Set("when", "on_failure")
	}
	if c.Manual {
		r.Set("when", "manual")
	}
	if branchRules == 1 {
		parts = append(parts, branchCompare(c.Branches[0]))
	} else if branchRules > 1 {
		var alts []string
		for _, b := range c.Branches {
			alts = append(alts, branchCompare(b))
		}
		if len(parts) == 0 {
			parts = append(parts, strings.Join(alts, " || "))
		} else {
			parts = append(parts, "("+strings.Join(alts, " || ")+")")
		}
	}
	if len(parts) > 0 {
		m := yamlx.NewMap().Set("if", strings.Join(parts, " && "))
		for _, k := range r.Keys() {
			v, _ := r.Get(k)
			m.Set(k, v)
		}
		r = m
	}
	return r, r.Len() > 0
}

func emitJob(p *ir.Pipeline, j *ir.Job, names map[string]string, stage string, d *ir.Diagnostics) *yamlx.Map {
	path := names[j.ID]
	m := yamlx.NewMap()
	m.Set("stage", stage)
	image, steps := ir.FoldTools(j, d, path)
	if image != "" {
		m.Set("image", toGL(image))
	} else if !j.Runner.IsLinux() {
		d.Infof(path, "no image set; the job uses the runner's shell")
	}
	if len(j.Services) > 0 {
		var svcs []any
		for _, s := range j.Services {
			sm := yamlx.NewMap().Set("name", toGL(s.Image))
			if s.Name != "" && s.Name != serviceAlias(s.Image) {
				sm.Set("alias", s.Name)
			}
			if len(s.Env) > 0 {
				vars := yamlx.NewMap()
				for _, kv := range s.Env {
					vars.Set(kv.Key, toGL(kv.Value))
				}
				sm.Set("variables", vars)
			}
			if sm.Len() == 1 {
				svcs = append(svcs, toGL(s.Image))
			} else {
				svcs = append(svcs, sm)
			}
			if len(s.Ports) > 0 {
				d.Infof(path+".services", "service %s is reachable by its alias; port mappings are not needed", s.Name)
			}
		}
		m.Set("services", svcs)
	}
	// Needs: always explicit so independence is preserved across stages.
	needs := []string{}
	for _, n := range j.Needs {
		needs = append(needs, names[n])
	}
	m.Set("needs", needs)

	vars := yamlx.NewMap()
	for _, kv := range j.Env {
		vars.Set(kv.Key, toGL(kv.Value))
	}
	var gitVars []ir.KV
	for _, st := range steps {
		if st.Kind == ir.StepCheckout && st.Checkout != nil {
			switch {
			case st.Checkout.Depth < 0:
				gitVars = append(gitVars, ir.KV{Key: "GIT_DEPTH", Value: "0"})
			case st.Checkout.Depth > 0:
				gitVars = append(gitVars, ir.KV{Key: "GIT_DEPTH", Value: fmt.Sprint(st.Checkout.Depth)})
			}
			if st.Checkout.Submodules {
				gitVars = append(gitVars, ir.KV{Key: "GIT_SUBMODULE_STRATEGY", Value: "recursive"})
			}
		}
	}
	hasCheckout := false
	for _, st := range steps {
		if st.Kind == ir.StepCheckout {
			hasCheckout = true
		}
	}
	if !hasCheckout {
		gitVars = append(gitVars, ir.KV{Key: "GIT_STRATEGY", Value: "none"})
	}
	for _, kv := range gitVars {
		vars.Set(kv.Key, kv.Value)
	}
	if vars.Len() > 0 {
		m.Set("variables", vars)
	}

	var script, after []string
	var caches []any
	var uploads []*ir.Artifact
	for i, st := range steps {
		where := fmt.Sprintf("%s.steps[%d]", path, i)
		switch st.Kind {
		case ir.StepCheckout, ir.StepDownload:
			// GitLab clones and downloads artifacts from needed jobs automatically.
		case ir.StepCache:
			cm := yamlx.NewMap()
			files := st.Cache.Files
			if len(files) > 2 {
				d.Warnf(where, "GitLab cache keys hash at most 2 files; using %v", files[:2])
				files = files[:2]
			}
			if len(files) > 0 {
				key := yamlx.NewMap().Set("files", files)
				key.SetIf("prefix", st.Cache.Prefix)
				cm.Set("key", key)
			} else {
				cm.Set("key", st.Cache.Prefix)
			}
			cm.Set("paths", st.Cache.Paths)
			caches = append(caches, cm)
		case ir.StepUpload:
			uploads = append(uploads, st.Artifact)
		case ir.StepAction:
			d.Warnf(where, "%s step %q has no GitLab equivalent; a failing placeholder command was emitted", st.UsesFrom, st.Uses)
			script = append(script, fmt.Sprintf("echo \"ci-shift: port %s step '%s' manually\" && exit 1", st.UsesFrom, st.Uses))
		case ir.StepTool:
			// Already folded into the image or reported.
		default:
			cmds := scriptItems(st, where, d)
			if st.If != nil && (st.If.When == ir.WhenAlways || st.If.When == ir.WhenOnFailure) {
				after = append(after, cmds...)
			} else {
				script = append(script, cmds...)
			}
		}
	}
	if len(caches) == 1 {
		m.Set("cache", caches[0])
	} else if len(caches) > 1 {
		m.Set("cache", caches)
	}
	if len(script) == 0 {
		script = []string{"echo 'nothing to do'"}
	}
	m.Set("script", script)
	m.SetIf("after_script", after)
	if len(uploads) > 0 {
		a := yamlx.NewMap()
		var paths []string
		for _, u := range uploads {
			paths = append(paths, u.Paths...)
		}
		if len(uploads) == 1 {
			a.SetIf("name", toGL(uploads[0].Name))
		}
		a.Set("paths", ir.SortedUnique(paths))
		if uploads[0].ExpireIn != "" {
			a.Set("expire_in", uploads[0].ExpireIn)
		}
		switch uploads[0].When {
		case ir.WhenAlways:
			a.Set("when", "always")
		case ir.WhenOnFailure:
			a.Set("when", "on_failure")
		}
		m.Set("artifacts", a)
	}
	if r, ok := conditionRule(j.If, d, path); ok {
		m.Set("rules", []any{r})
	}
	if j.Matrix != nil {
		m.Set("parallel", yamlx.NewMap().Set("matrix", emitMatrix(j.Matrix, d, path)))
	}
	if len(j.Runner.Labels) > 0 {
		m.Set("tags", j.Runner.Labels)
	} else {
		switch j.Runner.OS {
		case ir.OSWindows:
			m.Set("tags", []string{"saas-windows-medium-amd64"})
		case ir.OSMacOS:
			m.Set("tags", []string{"saas-macos-medium-m1"})
		}
	}
	if j.TimeoutMinutes > 0 {
		m.Set("timeout", ir.FormatMinutes(j.TimeoutMinutes))
	}
	if j.AllowFailure {
		m.Set("allow_failure", true)
	}
	if j.Environment != "" {
		m.Set("environment", j.Environment)
	}
	return m
}

// scriptItems renders a run step as script entries: plain steps become one
// entry per command (idiomatic GitLab), anything else a single entry.
func scriptItems(st *ir.Step, where string, d *ir.Diagnostics) []string {
	plain := len(st.Env) == 0 && st.WorkingDir == "" && st.If.IsZero() && !st.AllowFailure
	if plain {
		if lines, ok := SplitCommands(toGL(st.Run)); ok {
			return lines
		}
	}
	cmd, _ := wrapScript(st, where, d)
	return []string{cmd}
}

var compoundRe = regexp.MustCompile(`(^|[\s;(])(if|then|else|elif|fi|for|while|until|do|done|case|esac|function)(\s|;|$)|(^|[\s;])[{}]\s*$|^[{}]|<<|\\$`)

// SplitCommands splits a script into one command per line when every line
// is a self-contained command (no compound statements, heredocs, line
// continuations or unbalanced quotes).
func SplitCommands(script string) ([]string, bool) {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if line != trimmed && !strings.HasPrefix(line, trimmed) {
			// Indented lines belong to a surrounding construct.
			return nil, false
		}
		if compoundRe.MatchString(trimmed) || strings.Count(trimmed, "'")%2 != 0 || strings.Count(trimmed, `"`)%2 != 0 {
			return nil, false
		}
		out = append(out, trimmed)
	}
	return out, len(out) > 0
}

// wrapScript renders a run step as one script entry, scoping env vars and
// working directory in a subshell and guarding step conditions.
func wrapScript(st *ir.Step, where string, d *ir.Diagnostics) (string, bool) {
	body := toGL(st.Run)
	if st.Shell != "" && st.Shell != "bash" && st.Shell != "sh" {
		d.Warnf(where, "shell %q is not configurable per step on GitLab; the script runs in the runner's shell", st.Shell)
	}
	if len(st.Env) > 0 || st.WorkingDir != "" {
		var b strings.Builder
		b.WriteString("(\n")
		for _, kv := range st.Env {
			fmt.Fprintf(&b, "  export %s=%s\n", kv.Key, shellQuote(toGL(kv.Value)))
		}
		if st.WorkingDir != "" {
			fmt.Fprintf(&b, "  cd %s\n", shellQuote(toGL(st.WorkingDir)))
		}
		for _, line := range strings.Split(body, "\n") {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString(")")
		body = b.String()
	}
	if st.If != nil {
		c := *st.If
		c.When = ""
		cond, ok := ir.ShellCondition(&c, "CI_COMMIT_BRANCH", "CI_COMMIT_TAG")
		if !ok {
			d.Warnf(where, "step condition could not be translated; the step always runs")
		} else if cond != "" {
			body = fmt.Sprintf("if %s; then\n%s\nfi", cond, indent(body))
		}
		if st.If.When == ir.WhenOnFailure {
			body = fmt.Sprintf("if [ \"$CI_JOB_STATUS\" = \"failed\" ]; then\n%s\nfi", indent(body))
		}
	}
	if st.AllowFailure && (st.If == nil || st.If.When == "") {
		if strings.Contains(body, "\n") {
			body = "(\n" + indent(body) + "\n) || true"
		} else {
			body += " || true"
		}
	}
	if st.Name != "" && strings.Contains(body, "\n") {
		body = "# " + st.Name + "\n" + body
	}
	return body, true
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

func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, "$`") {
		return `"` + strings.NewReplacer(`"`, `\"`).Replace(s) + `"`
	}
	if strings.ContainsAny(s, " \t'\"\\;&|<>(){}*?!#~") {
		return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
	}
	return s
}

func emitMatrix(m *ir.Matrix, d *ir.Diagnostics, path string) []any {
	combos := m.Combos()
	if len(combos) > 200 {
		d.Warnf(path+".parallel", "GitLab allows at most 200 matrix jobs; got %d", len(combos))
	}
	// Pure cartesian products stay compact.
	if len(m.Include) == 0 && len(m.Exclude) == 0 {
		entry := yamlx.NewMap()
		for _, ax := range m.Axes {
			entry.Set(ax.Name, yamlx.Flow{Value: ax.Values})
		}
		return []any{entry}
	}
	var out []any
	for _, c := range combos {
		entry := yamlx.NewMap()
		for _, kv := range c {
			entry.Set(kv.Key, kv.Value)
		}
		out = append(out, entry)
	}
	return out
}
