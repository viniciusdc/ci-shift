package ir

import (
	"fmt"
	"sort"
	"strings"
)

// Provider converts between a CI system's native configuration and the IR.
type Provider interface {
	// Name is the short identifier used on the command line (e.g. "github").
	Name() string
	// Title is the human readable product name.
	Title() string
	// DefaultPath is the conventional location of the config file.
	DefaultPath() string
	// Detect scores how likely data (read from path) is this provider's
	// format: 0 means no, higher is more confident.
	Detect(path string, data []byte) int
	// Parse reads a native configuration into the IR.
	Parse(data []byte, diags *Diagnostics) (*Pipeline, error)
	// Emit renders the IR as a native configuration.
	Emit(p *Pipeline, diags *Diagnostics) ([]byte, error)
}

// Severity of a diagnostic.
type Severity int

// Severities.
const (
	SeverityInfo Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "info"
}

// Diagnostic reports something lost or approximated during conversion.
type Diagnostic struct {
	Severity Severity
	Path     string // location, e.g. "jobs.build.steps[2]"
	Message  string
}

func (d Diagnostic) String() string {
	if d.Path == "" {
		return fmt.Sprintf("%s: %s", d.Severity, d.Message)
	}
	return fmt.Sprintf("%s: %s: %s", d.Severity, d.Path, d.Message)
}

// Diagnostics collects conversion notes. The zero value is ready to use.
type Diagnostics struct {
	list []Diagnostic
	seen map[string]bool
}

func (d *Diagnostics) add(sev Severity, path, format string, args ...any) {
	if d == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	key := path + "\x00" + msg
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	d.list = append(d.list, Diagnostic{Severity: sev, Path: path, Message: msg})
}

// Warnf records a warning: behavior was dropped or approximated.
func (d *Diagnostics) Warnf(path, format string, args ...any) {
	d.add(SeverityWarning, path, format, args...)
}

// Infof records an informational note.
func (d *Diagnostics) Infof(path, format string, args ...any) {
	d.add(SeverityInfo, path, format, args...)
}

// List returns all diagnostics in the order they were recorded.
func (d *Diagnostics) List() []Diagnostic {
	if d == nil {
		return nil
	}
	return append([]Diagnostic(nil), d.list...)
}

// Warnings returns only warnings.
func (d *Diagnostics) Warnings() []Diagnostic {
	var out []Diagnostic
	for _, x := range d.List() {
		if x.Severity == SeverityWarning {
			out = append(out, x)
		}
	}
	return out
}

// Validate checks structural invariants: unique job IDs, known needs and
// no dependency cycles.
func (p *Pipeline) Validate() error {
	ids := map[string]bool{}
	for _, j := range p.Jobs {
		if j.ID == "" {
			return fmt.Errorf("job without an id")
		}
		if ids[j.ID] {
			return fmt.Errorf("duplicate job id %q", j.ID)
		}
		ids[j.ID] = true
	}
	for _, j := range p.Jobs {
		for _, n := range j.Needs {
			if !ids[n] {
				return fmt.Errorf("job %q needs unknown job %q", j.ID, n)
			}
		}
	}
	if _, err := p.Levels(); err != nil {
		return err
	}
	return nil
}

// Levels groups jobs into dependency levels: level 0 has no needs, level n
// only depends on earlier levels. Job order inside a level follows the
// pipeline order.
func (p *Pipeline) Levels() ([][]*Job, error) {
	level := map[string]int{}
	var visit func(j *Job, stack map[string]bool) (int, error)
	visit = func(j *Job, stack map[string]bool) (int, error) {
		if l, ok := level[j.ID]; ok {
			return l, nil
		}
		if stack[j.ID] {
			return 0, fmt.Errorf("dependency cycle involving job %q", j.ID)
		}
		stack[j.ID] = true
		l := 0
		for _, n := range j.Needs {
			dep := p.FindJob(n)
			if dep == nil {
				continue
			}
			dl, err := visit(dep, stack)
			if err != nil {
				return 0, err
			}
			if dl+1 > l {
				l = dl + 1
			}
		}
		delete(stack, j.ID)
		level[j.ID] = l
		return l, nil
	}
	max := -1
	for _, j := range p.Jobs {
		l, err := visit(j, map[string]bool{})
		if err != nil {
			return nil, err
		}
		if l > max {
			max = l
		}
	}
	out := make([][]*Job, max+1)
	for _, j := range p.Jobs {
		out[level[j.ID]] = append(out[level[j.ID]], j)
	}
	return out, nil
}

// StageNames returns stage names for emitters that need stages: the jobs'
// own stages when every job has one, otherwise one stage per dependency
// level ("stage-1", ...). The returned map assigns each job ID its stage.
func (p *Pipeline) StageNames() ([]string, map[string]string, error) {
	assign := map[string]string{}
	all := len(p.Jobs) > 0
	for _, j := range p.Jobs {
		if j.Stage == "" {
			all = false
			break
		}
	}
	if all {
		var stages []string
		seen := map[string]bool{}
		for _, j := range p.Jobs {
			if !seen[j.Stage] {
				seen[j.Stage] = true
				stages = append(stages, j.Stage)
			}
			assign[j.ID] = j.Stage
		}
		// Stages must not run a job before one it needs; fall back to levels
		// when the declared stage order contradicts the needs graph.
		pos := map[string]int{}
		for i, s := range stages {
			pos[s] = i
		}
		ok := true
		for _, j := range p.Jobs {
			for _, n := range j.Needs {
				if dep := p.FindJob(n); dep != nil && pos[dep.Stage] > pos[j.Stage] {
					ok = false
				}
			}
		}
		if ok {
			return stages, assign, nil
		}
		assign = map[string]string{}
	}
	levels, err := p.Levels()
	if err != nil {
		return nil, nil, err
	}
	names := levelStageNames(levels)
	for i, lv := range levels {
		for _, j := range lv {
			assign[j.ID] = names[i]
		}
	}
	return names, assign, nil
}

// levelStageNames names levels after common conventions when possible.
func levelStageNames(levels [][]*Job) []string {
	names := make([]string, len(levels))
	used := map[string]bool{}
	for i, lv := range levels {
		name := fmt.Sprintf("stage-%d", i+1)
		if len(lv) == 1 {
			cand := Slug(lv[0].ID)
			if !used[cand] {
				name = cand
			}
		}
		used[name] = true
		names[i] = name
	}
	return names
}

// Slug converts s into an identifier made of [a-z0-9_-].
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		case r == '-' || r == ' ' || r == '.' || r == '/' || r == ':':
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "job"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "job-" + out
	}
	return out
}

// IdentSlug is like Slug but uses underscores, for providers whose IDs
// must be valid identifiers (Azure Pipelines).
func IdentSlug(s string) string {
	return strings.ReplaceAll(Slug(s), "-", "_")
}

// UniqueID returns base, or base-2, base-3... if taken.
func UniqueID(base string, taken map[string]bool) string {
	id := base
	for i := 2; taken[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	taken[id] = true
	return id
}

// Combos expands the matrix into explicit variable combinations.
func (m *Matrix) Combos() [][]KV {
	if m == nil {
		return nil
	}
	var out [][]KV
	if len(m.Axes) > 0 {
		out = [][]KV{{}}
		for _, ax := range m.Axes {
			var next [][]KV
			for _, combo := range out {
				for _, v := range ax.Values {
					c := append(append([]KV(nil), combo...), KV{ax.Name, v})
					next = append(next, c)
				}
			}
			out = next
		}
	}
	if len(m.Exclude) > 0 {
		var kept [][]KV
		for _, c := range out {
			if !matchesAny(c, m.Exclude) {
				kept = append(kept, c)
			}
		}
		out = kept
	}
	// Includes follow GitHub: an entry "will be added to each of the matrix
	// combinations if none of the key:value pairs overwrite any of the
	// original matrix values", otherwise it is a new combination. Only
	// product combinations take entries; a later entry overwrites values an
	// earlier one added.
	axes := map[string]bool{}
	for _, ax := range m.Axes {
		axes[ax.Name] = true
	}
	original := len(out)
	for _, inc := range m.Include {
		merged := false
		for i := range out[:original] {
			if conflicts(out[i], inc, axes) {
				continue
			}
			for _, kv := range inc {
				if !axes[kv.Key] {
					out[i] = KVSet(out[i], kv.Key, kv.Value)
				}
			}
			merged = true
		}
		if !merged {
			out = append(out, append([]KV(nil), inc...))
		}
	}
	return out
}

// Vars returns every variable name the matrix defines, in order.
func (m *Matrix) Vars() []string {
	if m == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, ax := range m.Axes {
		add(ax.Name)
	}
	for _, inc := range m.Include {
		for _, kv := range inc {
			add(kv.Key)
		}
	}
	return out
}

// conflicts reports whether inc changes an axis value of c.
func conflicts(c, inc []KV, axes map[string]bool) bool {
	for _, kv := range inc {
		if !axes[kv.Key] {
			continue
		}
		if v, _ := KVGet(c, kv.Key); v != kv.Value {
			return true
		}
	}
	return false
}

func matchesAny(c []KV, patterns [][]KV) bool {
	for _, p := range patterns {
		ok := true
		for _, kv := range p {
			if v, found := KVGet(c, kv.Key); !found || v != kv.Value {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ToolImage returns a public container image providing tool at version.
func ToolImage(tool, version string) string {
	v := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(version), ".x"), ".x")
	if strings.Contains(v, "$") && !strings.Contains(v, "${{") {
		v = "" // native shell syntax cannot be carried over
	}
	tag := func(def string) string {
		if v == "" {
			return def
		}
		return v
	}
	switch tool {
	case ToolNode:
		return "node:" + tag("lts")
	case ToolPython:
		return "python:" + tag("3")
	case ToolGo:
		return "golang:" + tag("1")
	case ToolJava:
		return "eclipse-temurin:" + tag("21")
	case ToolRuby:
		return "ruby:" + tag("3")
	case ToolDotnet:
		return "mcr.microsoft.com/dotnet/sdk:" + tag("8.0")
	}
	return ""
}

// ToolFromImage guesses the tool/version provided by a well-known image.
func ToolFromImage(image string) (tool, version string) {
	name, tag, _ := strings.Cut(image, ":")
	name = strings.TrimPrefix(name, "docker.io/")
	name = strings.TrimPrefix(name, "library/")
	switch name {
	case "node", "cimg/node", "circleci/node":
		return ToolNode, tag
	case "python", "cimg/python", "circleci/python":
		return ToolPython, tag
	case "golang", "cimg/go", "circleci/golang":
		return ToolGo, tag
	case "eclipse-temurin", "openjdk", "cimg/openjdk", "maven", "gradle":
		return ToolJava, tag
	case "ruby", "cimg/ruby":
		return ToolRuby, tag
	case "mcr.microsoft.com/dotnet/sdk":
		return ToolDotnet, tag
	}
	return "", ""
}

// SortedKeys returns map keys sorted.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
