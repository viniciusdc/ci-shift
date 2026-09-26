package ir

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FoldTools removes tool-setup steps for providers that run jobs inside a
// container image: the first tool picks the image when the job has none,
// and tools already provided by the image are dropped. It returns the image
// to use (possibly "") and the remaining steps.
func FoldTools(j *Job, d *Diagnostics, path string) (string, []*Step) {
	image := j.Image
	var steps []*Step
	for i, st := range j.Steps {
		if st.Kind != StepTool {
			steps = append(steps, st)
			continue
		}
		where := fmt.Sprintf("%s.steps[%d]", path, i)
		if image == "" {
			image = ToolImage(st.Tool, st.Version)
			d.Infof(where, "tool %s %s is provided by image %s", st.Tool, st.Version, image)
			continue
		}
		if tool, _ := ToolFromImage(image); tool == st.Tool {
			continue
		}
		d.Warnf(where, "image %s does not provide %s %s; install it in a script step", image, st.Tool, st.Version)
	}
	return image, steps
}

// Shell environment names that are never CI secrets.
var commonEnv = map[string]bool{
	"HOME": true, "PATH": true, "PWD": true, "OLDPWD": true, "USER": true, "SHELL": true,
	"HOSTNAME": true, "TMPDIR": true, "TEMP": true, "TMP": true, "LANG": true, "LC_ALL": true,
	"TERM": true, "RANDOM": true, "UID": true, "EUID": true, "CI": true, "GOPATH": true,
	"GOROOT": true, "JAVA_HOME": true, "NODE_ENV": true, "PYTHONPATH": true, "VIRTUAL_ENV": true,
	"DEBIAN_FRONTEND": true, "DOCKER_HOST": true, "DOCKER_TLS_CERTDIR": true, "DOCKER_DRIVER": true,
	"IFS": true, "BASH_SOURCE": true, "LINENO": true, "SECONDS": true, "PPID": true, "HOSTTYPE": true,
	"OSTYPE": true, "MACHTYPE": true, "PS1": true, "PS4": true, "XDG_CACHE_HOME": true,
	"XDG_CONFIG_HOME": true, "GOCACHE": true, "GOFLAGS": true, "npm_config_cache": true,
}

// builtinPrefixes are variable prefixes reserved by the supported CI
// providers; such variables are never secrets, whatever the source.
var builtinPrefixes = []string{
	"CI_", "GITLAB_", "GITHUB_", "RUNNER_", "CIRCLE_", "CIRCLECI", "BITBUCKET_",
	"BUILD_", "SYSTEM_", "AGENT_", "PIPELINE_", "TF_BUILD", "ACTIONS_",
}

var upperRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// PromoteExternalVars turns $NAME references to variables that are defined
// nowhere in the pipeline (typically project-level CI/CD variables holding
// credentials) into ${{ secret.NAME }} references, so emitters for
// providers that require explicit secret mapping can wire them up.
// extraPrefixes adds reserved prefixes beyond those of the supported
// providers.
func PromoteExternalVars(p *Pipeline, d *Diagnostics, extraPrefixes ...string) {
	defined := map[string]bool{}
	for _, kv := range p.Env {
		defined[kv.Key] = true
	}
	promoted := map[string]bool{}
	isExternal := func(name string, local map[string]bool) bool {
		if defined[name] || local[name] || commonEnv[name] || !upperRe.MatchString(name) {
			return false
		}
		for _, list := range [][]string{builtinPrefixes, extraPrefixes} {
			for _, pre := range list {
				if strings.HasPrefix(name, pre) {
					return false
				}
			}
		}
		return true
	}
	for _, j := range p.Jobs {
		local := map[string]bool{}
		for _, kv := range j.Env {
			local[kv.Key] = true
		}
		for _, v := range j.Matrix.Vars() {
			local[v] = true
		}
		for _, s := range j.Services {
			for _, kv := range s.Env {
				local[kv.Key] = true
			}
		}
		for _, st := range j.Steps {
			for _, kv := range st.Env {
				local[kv.Key] = true
			}
			for _, name := range assignedVars(st.Run) {
				local[name] = true
			}
		}
		rewrite := func(s string) string {
			return ReplaceShellVars(s, func(name string) (string, bool) {
				if isExternal(name, local) {
					promoted[name] = true
					return Ref(RefSecret, name), true
				}
				return "", false
			})
		}
		for _, st := range j.Steps {
			st.Run = rewrite(st.Run)
			for i := range st.Env {
				st.Env[i].Value = rewrite(st.Env[i].Value)
			}
			// Action/pipe inputs usually carry repository secrets too.
			for i := range st.With {
				st.With[i].Value = rewrite(st.With[i].Value)
			}
		}
		for i := range j.Env {
			j.Env[i].Value = rewrite(j.Env[i].Value)
		}
	}
	if len(promoted) > 0 {
		names := SortedKeys(promoted)
		d.Infof("", "variables defined outside the file were treated as secrets: %s", strings.Join(names, ", "))
	}
}

var assignRe = regexp.MustCompile(`(?m)(?:^|[;&|(\s])(?:export\s+|local\s+|readonly\s+|declare\s+(?:-\w+\s+)?)?([A-Za-z_][A-Za-z0-9_]*)=|\bfor\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\b|\bread\s+(?:-\w+\s+)*([A-Za-z_][A-Za-z0-9_ ]*)`)

// assignedVars returns shell variables assigned inside a script.
func assignedVars(script string) []string {
	var out []string
	for _, m := range assignRe.FindAllStringSubmatch(script, -1) {
		switch {
		case m[1] != "":
			out = append(out, m[1])
		case m[2] != "":
			out = append(out, m[2])
		case m[3] != "":
			out = append(out, strings.Fields(m[3])...)
		}
	}
	return out
}

// GlobToRegex converts a branch glob ("release/*") to an anchored regex body.
func GlobToRegex(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
			}
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '{', '}', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '/':
			b.WriteString(`\/`)
		default:
			b.WriteByte(c)
		}
	}
	return "^" + b.String() + "$"
}

var simpleRegexRe = regexp.MustCompile(`^\^?((?:[A-Za-z0-9_\-]|\\[/.\-])*)(\.\*|\.\+)?\$?$`)

// RegexToGlob converts simple anchored regexes such as /^release\/.*$/ back
// into globs; ok is false for anything more complex.
func RegexToGlob(re string) (string, bool) {
	re = strings.TrimSuffix(strings.TrimPrefix(re, "/"), "/")
	m := simpleRegexRe.FindStringSubmatch(re)
	if m == nil {
		return "", false
	}
	lit := strings.NewReplacer(`\/`, "/", `\.`, ".", `\-`, "-").Replace(m[1])
	if m[2] != "" || !strings.HasSuffix(re, "$") {
		return lit + "*", true
	}
	return lit, true
}

// IsGlob reports whether a branch pattern contains wildcards.
func IsGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// SortedUnique sorts and de-duplicates.
func SortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ShellCondition renders a condition as a POSIX shell test using the
// provider's native branch/event variables, for providers without step
// conditions. ok is false when the condition cannot be expressed.
func ShellCondition(c *Condition, branchVar, tagVar string) (string, bool) {
	if c.IsZero() {
		return "", true
	}
	if c.Raw != "" || c.Manual || (c.Event != "" && c.Event != EventTag) {
		return "", false
	}
	var parts []string
	if c.Event == EventTag {
		parts = append(parts, fmt.Sprintf(`[ -n "${%s:-}" ]`, tagVar))
	}
	var br []string
	for _, b := range c.Branches {
		if IsGlob(b) {
			br = append(br, fmt.Sprintf(`case "${%s:-}" in %s) true;; *) false;; esac`, branchVar, b))
		} else {
			br = append(br, fmt.Sprintf(`[ "${%s:-}" = %q ]`, branchVar, b))
		}
	}
	if len(br) == 1 {
		parts = append(parts, br[0])
	} else if len(br) > 1 {
		parts = append(parts, "{ "+strings.Join(br, " || ")+"; }")
	}
	return strings.Join(parts, " && "), true
}

// BindMatrixRefs rewrites ${{ env.X }} references to ${{ matrix.X }} for
// every matrix variable X of the job. Parsers of providers where matrix
// values are plain variables call it once the matrix is known.
func BindMatrixRefs(j *Job) {
	vars := map[string]bool{}
	for _, v := range j.Matrix.Vars() {
		vars[v] = true
	}
	if len(vars) == 0 {
		return
	}
	bind := func(s string) string {
		return RewriteRefs(s, func(kind, name string) string {
			if kind == RefEnv && vars[name] {
				kind = RefMatrix
			}
			return Ref(kind, name)
		})
	}
	j.Name = bind(j.Name)
	j.Image = bind(j.Image)
	j.Runner.VMImage = bind(j.Runner.VMImage)
	for i := range j.Env {
		j.Env[i].Value = bind(j.Env[i].Value)
	}
	for si := range j.Services {
		j.Services[si].Image = bind(j.Services[si].Image)
		for i := range j.Services[si].Env {
			j.Services[si].Env[i].Value = bind(j.Services[si].Env[i].Value)
		}
	}
	for _, st := range j.Steps {
		st.Name = bind(st.Name)
		st.WorkingDir = bind(st.WorkingDir)
		st.Version = bind(st.Version)
		for i := range st.Env {
			st.Env[i].Value = bind(st.Env[i].Value)
		}
		for i := range st.With {
			st.With[i].Value = bind(st.With[i].Value)
		}
		if st.Artifact != nil {
			st.Artifact.Name = bind(st.Artifact.Name)
		}
	}
}

// ResolveEnvRefs replaces ${{ env.X }} with the literal value of X from
// the given variable lists (first match wins). ok is false when a
// reference could not be resolved.
func ResolveEnvRefs(s string, scopes ...[]KV) (string, bool) {
	ok := true
	out := RewriteRefs(s, func(kind, name string) string {
		if kind != RefEnv {
			return Ref(kind, name)
		}
		for _, scope := range scopes {
			if v, found := KVGet(scope, name); found {
				return v
			}
		}
		ok = false
		return Ref(kind, name)
	})
	return out, ok
}
