package ir

import (
	"regexp"
	"sort"
	"strings"
)

// Reference kinds used in canonical expressions.
const (
	RefCI     = "ci"
	RefSecret = "secret"
	RefEnv    = "env"
	RefMatrix = "matrix"
)

// Well-known CI values, used as ${{ ci.<name> }}.
const (
	VarSHA        = "sha"        // full commit SHA
	VarRefName    = "ref_name"   // branch or tag name
	VarBranch     = "branch"     // branch name
	VarTag        = "tag"        // tag name
	VarRepository = "repository" // owner/name (or project path)
	VarRunID      = "run_id"     // unique id of the pipeline run
	VarRunNumber  = "run_number" // sequential run number
	VarWorkspace  = "workspace"  // checkout directory
	VarJob        = "job"        // current job name
	VarActor      = "actor"      // user who triggered the run
	VarPRNumber   = "pr_number"  // pull/merge request number
	VarEvent      = "event"      // triggering event name
	VarServerURL  = "server_url" // base URL of the forge
	VarDefaultBr  = "default_branch"
)

// CIVars lists every well-known CI value.
var CIVars = []string{
	VarSHA, VarRefName, VarBranch, VarTag, VarRepository, VarRunID, VarRunNumber,
	VarWorkspace, VarJob, VarActor, VarPRNumber, VarEvent, VarServerURL, VarDefaultBr,
}

var refRe = regexp.MustCompile(`\$\{\{\s*(ci|secret|env|matrix)\.([A-Za-z0-9_\-]+)\s*\}\}`)

// Ref builds a canonical reference such as ${{ ci.sha }}.
func Ref(kind, name string) string { return "${{ " + kind + "." + name + " }}" }

// RewriteRefs replaces every canonical reference in s with fn(kind, name).
func RewriteRefs(s string, fn func(kind, name string) string) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	return refRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := refRe.FindStringSubmatch(m)
		return fn(sub[1], sub[2])
	})
}

// Refs returns the names referenced with the given kind in s.
func Refs(s, kind string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		if m[1] == kind {
			out = append(out, m[2])
		}
	}
	return out
}

var anyExprRe = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// ForeignExpressions returns ${{ ... }} expressions in s that are not
// canonical references (typically untranslated GitHub expressions).
func ForeignExpressions(s string) []string {
	var out []string
	for _, m := range anyExprRe.FindAllString(s, -1) {
		if !refRe.MatchString(m) {
			out = append(out, m)
		}
	}
	return out
}

// ShellVarRe matches $NAME and ${NAME} shell references.
var ShellVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// ReplaceShellVars rewrites $NAME / ${NAME} references using fn. When fn
// returns ok=false the reference is kept as is.
func ReplaceShellVars(s string, fn func(name string) (string, bool)) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return ShellVarRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := ShellVarRe.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		if r, ok := fn(name); ok {
			return r
		}
		return m
	})
}

// NativeVar pairs a canonical CI var with a provider's native value.
type NativeVar struct {
	Canon  string
	Native string
}

// NativeVars is a provider's table of well-known variables. When several
// canonical names share a native variable, the first entry wins on parsing.
type NativeVars []NativeVar

// Lookup returns the native value for a canonical name.
func (nv NativeVars) Lookup(canon string) (string, bool) {
	for _, v := range nv {
		if v.Canon == canon {
			return v.Native, true
		}
	}
	return "", false
}

// FromShell rewrites native shell variables ($NAME or ${NAME}) into
// canonical ${{ ci.* }} references.
func (nv NativeVars) FromShell(s string) string {
	return ReplaceShellVars(s, func(name string) (string, bool) {
		for _, v := range nv {
			if v.Native == name {
				return Ref(RefCI, v.Canon), true
			}
		}
		return "", false
	})
}

// Secrets returns the sorted, de-duplicated secret names referenced anywhere
// in the pipeline.
func (p *Pipeline) Secrets() []string {
	seen := map[string]bool{}
	p.WalkText(func(s string) {
		for _, name := range Refs(s, RefSecret) {
			seen[name] = true
		}
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WalkText calls fn for every free-text field of the pipeline.
func (p *Pipeline) WalkText(fn func(string)) {
	for _, kv := range p.Env {
		fn(kv.Value)
	}
	for _, j := range p.Jobs {
		fn(j.Image)
		for _, kv := range j.Env {
			fn(kv.Value)
		}
		for _, s := range j.Services {
			fn(s.Image)
			for _, kv := range s.Env {
				fn(kv.Value)
			}
		}
		for _, st := range j.Steps {
			fn(st.Run)
			fn(st.WorkingDir)
			for _, kv := range st.Env {
				fn(kv.Value)
			}
			for _, kv := range st.With {
				fn(kv.Value)
			}
		}
	}
}

// StepSecrets returns secrets referenced by a step's script and env.
func StepSecrets(st *Step) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, n := range Refs(s, RefSecret) {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(st.Run)
	for _, kv := range st.Env {
		add(kv.Value)
	}
	return out
}
