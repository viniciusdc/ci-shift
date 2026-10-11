package azure

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/viniciusdc/ci-shift/ir"
)

var (
	// macroRe matches $(Name) macros. In bash, $(cmd) is also command
	// substitution; callers decide which one a match is.
	macroRe = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_.\-]*)\)`)
	// templateRe matches ${{ ... }} compile-time template expressions.
	templateRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)
	// runtimeRe matches $[ ... ] runtime expressions.
	runtimeRe = regexp.MustCompile(`\$\[[^\]]*\]`)
	// templateVarRe matches variables.X and variables['X'] inside ${{ }}.
	templateVarRe = regexp.MustCompile(`^variables(?:\.([A-Za-z0-9_.\-]+)|\[\s*'([^']+)'\s*\])$`)
	identRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	lowerWordRe   = regexp.MustCompile(`^[a-z0-9_\-]+$`)
)

// scope holds the variable names visible to a job: pipeline, stage and job
// variables (keyed by lower-case name, since Azure names are
// case-insensitive) and matrix variables.
type scope struct {
	vars   map[string]string
	matrix map[string]string
}

func newScope(parent *scope) *scope {
	s := &scope{vars: map[string]string{}, matrix: map[string]string{}}
	if parent != nil {
		for k, v := range parent.vars {
			s.vars[k] = v
		}
		for k, v := range parent.matrix {
			s.matrix[k] = v
		}
	}
	return s
}

// fromAZ converts Azure text into canonical form. Macros naming well-known
// variables become ${{ ci.* }}, pipeline variables ${{ env.* }}, matrix
// variables ${{ matrix.* }} and variables defined nowhere in the file
// (UI variables, variable groups) ${{ secret.* }}. In scripts, lower-case
// $(word) references to undefined names are kept: they are most likely
// shell command substitutions.
func (ps *parser) fromAZ(s string, sc *scope, script bool, path string) string {
	if s == "" || !strings.Contains(s, "$") {
		return s
	}
	for _, m := range runtimeRe.FindAllString(s, -1) {
		ps.d.Warnf(path, "runtime expression %s cannot be converted and was kept verbatim", m)
	}
	s = templateRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := templateRe.FindStringSubmatch(m)[1]
		if sub := templateVarRe.FindStringSubmatch(inner); sub != nil {
			if r, ok := ps.resolveVar(sub[1]+sub[2], sc, false, path); ok {
				return r
			}
			return m
		}
		ps.d.Warnf(path, "template expression %s cannot be converted and was kept verbatim", m)
		return m
	})
	s = macroRe.ReplaceAllStringFunc(s, func(m string) string {
		if r, ok := ps.resolveVar(macroRe.FindStringSubmatch(m)[1], sc, script, path); ok {
			return r
		}
		return m
	})
	if script {
		s = shellVars.FromShell(s)
		if sc != nil {
			s = ir.ReplaceShellVars(s, sc.exported)
		}
	}
	return s
}

// exported resolves shell references to the environment variables Azure
// exports for pipeline and matrix variables whose names differ from the
// variable itself (python_version is exported as $PYTHON_VERSION).
func (sc *scope) exported(name string) (string, bool) {
	for _, n := range sc.matrix {
		if envName(n) == name && n != name {
			return ir.Ref(ir.RefMatrix, n), true
		}
	}
	for _, n := range sc.vars {
		if envName(n) == name && n != name {
			return ir.Ref(ir.RefEnv, n), true
		}
	}
	return "", false
}

// resolveVar maps an Azure variable name to a canonical reference.
func (ps *parser) resolveVar(name string, sc *scope, script bool, path string) (string, bool) {
	if c, ok := macroToCanon(name); ok {
		return ir.Ref(ir.RefCI, c), true
	}
	if sc != nil {
		if n, ok := sc.matrix[strings.ToLower(name)]; ok {
			return ir.Ref(ir.RefMatrix, n), true
		}
		if n, ok := sc.vars[strings.ToLower(name)]; ok {
			return ir.Ref(ir.RefEnv, n), true
		}
	}
	if isPredefined(name) {
		ps.d.Warnf(path, "predefined variable $(%s) has no portable equivalent and was kept verbatim", name)
		return "", false
	}
	if !identRe.MatchString(name) || (script && lowerWordRe.MatchString(name)) {
		return "", false
	}
	ps.promoted[name] = true
	return ir.Ref(ir.RefSecret, name), true
}

// Script kinds decide how secrets are read inside a script.
const (
	scriptNone = iota // not a script: use macros everywhere
	scriptPOSIX
	scriptPowerShell
	scriptCmd
)

// toAZ renders canonical references. Outside scripts every reference is a
// $(Name) macro. Inside scripts, secrets are read from the environment
// (the caller maps them through the step's env) so their values never
// appear in the script text.
func (e *emitter) toAZ(s string, kind int, path string) string {
	if s == "" {
		return s
	}
	for _, x := range ir.ForeignExpressions(s) {
		e.d.Warnf(path, "expression %s has no Azure Pipelines equivalent and was kept verbatim", x)
	}
	return ir.RewriteRefs(s, func(k, name string) string {
		switch k {
		case ir.RefCI:
			return e.ciMacro(name, path)
		case ir.RefSecret:
			switch kind {
			case scriptPOSIX:
				return "${" + name + "}"
			case scriptPowerShell:
				return "$env:" + name
			case scriptCmd:
				return "%" + name + "%"
			}
		}
		return "$(" + name + ")"
	})
}

func (e *emitter) ciMacro(name, path string) string {
	if v, ok := macroVars.Lookup(name); ok {
		return "$(" + v + ")"
	}
	if name == ir.VarDefaultBr {
		e.d.Warnf(path, "Azure Pipelines has no default-branch variable; assuming \"main\"")
		return "main"
	}
	e.d.Warnf(path, "CI value %q has no Azure Pipelines equivalent", name)
	return ir.Ref(ir.RefCI, name)
}

// variableExpr renders a canonical reference as a runtime expression
// operand (variables['Name']).
func variableExpr(kind, name string) string {
	if kind == ir.RefCI {
		if v, ok := macroVars.Lookup(name); ok {
			name = v
		}
	}
	return "variables['" + name + "']"
}

// runtimeFormat renders text containing references as a $[ ... ] runtime
// expression, for fields such as container images where macros are not
// expanded.
func runtimeFormat(s string) string {
	var args []string
	lit := ir.RewriteRefs(s, func(kind, name string) string {
		args = append(args, variableExpr(kind, name))
		return fmt.Sprintf("\x00%d\x01", len(args)-1)
	})
	if len(args) == 0 {
		return s
	}
	// Escape literal braces for format(), then turn markers into {N}.
	lit = strings.NewReplacer("{", "{{", "}", "}}").Replace(lit)
	lit = strings.NewReplacer("\x00", "{", "\x01", "}").Replace(lit)
	if lit == "{0}" && len(args) == 1 {
		return "$[ " + args[0] + " ]"
	}
	return fmt.Sprintf("$[ format('%s', %s) ]", strings.ReplaceAll(lit, "'", "''"), strings.Join(args, ", "))
}

// cexpr is a node of an Azure condition expression: a function call, a
// string/number literal or a property reference such as
// variables['Build.SourceBranch'].
type cexpr struct {
	fn    string // lower-cased function name for calls
	args  []*cexpr
	lit   string // literal value
	isLit bool
	ref   string // lower-cased reference, e.g. "variables.build.sourcebranch"
}

type condParser struct {
	s   string
	pos int
}

func parseCondExpr(s string) (*cexpr, error) {
	cp := &condParser{s: s}
	e, err := cp.expr()
	if err != nil {
		return nil, err
	}
	cp.skip()
	if cp.pos != len(cp.s) {
		return nil, fmt.Errorf("unexpected %q", cp.s[cp.pos:])
	}
	return e, nil
}

func (cp *condParser) skip() {
	for cp.pos < len(cp.s) && strings.ContainsRune(" \t\r\n", rune(cp.s[cp.pos])) {
		cp.pos++
	}
}

func (cp *condParser) peek() byte {
	cp.skip()
	if cp.pos < len(cp.s) {
		return cp.s[cp.pos]
	}
	return 0
}

func (cp *condParser) str() (string, error) {
	cp.pos++ // opening quote
	var b strings.Builder
	for cp.pos < len(cp.s) {
		c := cp.s[cp.pos]
		cp.pos++
		if c == '\'' {
			if cp.pos < len(cp.s) && cp.s[cp.pos] == '\'' {
				b.WriteByte('\'')
				cp.pos++
				continue
			}
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return "", fmt.Errorf("unterminated string")
}

func (cp *condParser) ident() string {
	start := cp.pos
	for cp.pos < len(cp.s) {
		c := cp.s[cp.pos]
		if c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			cp.pos++
			continue
		}
		break
	}
	return cp.s[start:cp.pos]
}

func (cp *condParser) expr() (*cexpr, error) {
	switch cp.peek() {
	case '\'':
		v, err := cp.str()
		return &cexpr{lit: v, isLit: true}, err
	case 0:
		return nil, fmt.Errorf("unexpected end of expression")
	}
	name := cp.ident()
	if name == "" {
		return nil, fmt.Errorf("unexpected %q", cp.s[cp.pos:])
	}
	if cp.peek() == '(' {
		cp.pos++
		call := &cexpr{fn: strings.ToLower(name)}
		if cp.peek() == ')' {
			cp.pos++
			return call, nil
		}
		for {
			arg, err := cp.expr()
			if err != nil {
				return nil, err
			}
			call.args = append(call.args, arg)
			switch cp.peek() {
			case ',':
				cp.pos++
			case ')':
				cp.pos++
				return call, nil
			default:
				return nil, fmt.Errorf("expected , or ) at %d", cp.pos)
			}
		}
	}
	// Property reference: name(.name | ['key'])*
	parts := []string{name}
	for {
		switch cp.peek() {
		case '.':
			cp.pos++
			parts = append(parts, cp.ident())
			continue
		case '[':
			cp.pos++
			if cp.peek() != '\'' {
				return nil, fmt.Errorf("expected string index")
			}
			key, err := cp.str()
			if err != nil {
				return nil, err
			}
			if cp.peek() != ']' {
				return nil, fmt.Errorf("expected ]")
			}
			cp.pos++
			parts = append(parts, key)
			continue
		}
		break
	}
	ref := strings.ToLower(strings.Join(parts, "."))
	if len(parts) == 1 {
		// Bare words are literals (true, false, numbers).
		return &cexpr{lit: name, isLit: true, ref: ref}, nil
	}
	return &cexpr{ref: ref}, nil
}

// parseCondition translates an Azure condition; anything it does not
// understand is kept raw.
func (ps *parser) parseCondition(expr, path string) *ir.Condition {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	fail := func() *ir.Condition {
		ps.d.Warnf(path, "condition %q could not be translated and was kept verbatim", expr)
		return &ir.Condition{Raw: expr, RawFrom: "azure"}
	}
	e, err := parseCondExpr(expr)
	if err != nil {
		return fail()
	}
	c, ok := ps.condOf(e, path)
	if !ok {
		return fail()
	}
	if c.IsZero() {
		return nil
	}
	return c
}

const (
	refSourceBranch     = "variables.build.sourcebranch"
	refSourceBranchName = "variables.build.sourcebranchname"
	refReason           = "variables.build.reason"
)

func (ps *parser) condOf(e *cexpr, path string) (*ir.Condition, bool) {
	c := &ir.Condition{}
	if e.isLit {
		return c, strings.EqualFold(e.lit, "true")
	}
	switch e.fn {
	case "succeeded":
		return c, len(e.args) == 0
	case "always":
		c.When = ir.WhenAlways
		return c, true
	case "failed":
		c.When = ir.WhenOnFailure
		return c, len(e.args) == 0
	case "succeededorfailed":
		ps.d.Infof(path, "succeededOrFailed() was widened to always() (it also runs when the run is canceled)")
		c.When = ir.WhenAlways
		return c, len(e.args) == 0
	case "and":
		for _, a := range e.args {
			sub, ok := ps.condOf(a, path)
			if !ok || !mergeCond(c, sub) {
				return nil, false
			}
		}
		return c, len(e.args) > 0
	case "or":
		// Only alternatives of branches are portable.
		for _, a := range e.args {
			sub, ok := ps.condOf(a, path)
			if !ok || sub.When != "" || sub.Event != "" || len(sub.Branches) == 0 {
				return nil, false
			}
			c.Branches = append(c.Branches, sub.Branches...)
		}
		return c, len(e.args) > 0
	case "eq", "in":
		if len(e.args) < 2 {
			return nil, false
		}
		ref, vals := e.args[0], e.args[1:]
		if e.fn == "eq" && ref.isLit && !vals[0].isLit {
			ref, vals = vals[0], []*cexpr{ref}
		}
		var lits []string
		for _, v := range vals {
			if !v.isLit {
				return nil, false
			}
			lits = append(lits, v.lit)
		}
		return c, applyCompare(c, ref.ref, lits)
	case "startswith":
		if len(e.args) != 2 || !e.args[1].isLit {
			return nil, false
		}
		v := e.args[1].lit
		switch e.args[0].ref {
		case refSourceBranch:
			switch {
			case v == "refs/tags/" || v == "refs/tags":
				c.Event = ir.EventTag
				return c, true
			case strings.HasPrefix(v, "refs/heads/"):
				c.Branches = []string{strings.TrimPrefix(v, "refs/heads/") + "*"}
				return c, true
			}
		case refSourceBranchName:
			c.Branches = []string{v + "*"}
			return c, true
		}
	}
	return nil, false
}

// applyCompare handles eq/in comparisons of a variable with literals.
func applyCompare(c *ir.Condition, ref string, lits []string) bool {
	switch ref {
	case refSourceBranch:
		for _, l := range lits {
			b, ok := strings.CutPrefix(l, "refs/heads/")
			if !ok {
				return false
			}
			c.Branches = append(c.Branches, b)
		}
		return true
	case refSourceBranchName:
		c.Branches = append(c.Branches, lits...)
		return true
	case refReason:
		events := map[string]bool{}
		for _, l := range lits {
			switch strings.ToLower(l) {
			case "pullrequest":
				events[ir.EventPullRequest] = true
			case "schedule":
				events[ir.EventSchedule] = true
			case "manual":
				events[ir.EventManual] = true
			case "individualci", "batchedci":
				events[ir.EventPush] = true
			default:
				return false
			}
		}
		if len(events) != 1 {
			return false
		}
		for ev := range events {
			c.Event = ev
		}
		return true
	}
	return false
}

// mergeCond folds sub into c (both must hold); false on conflicts.
func mergeCond(c, sub *ir.Condition) bool {
	if sub.When != "" {
		if c.When != "" && c.When != sub.When {
			return false
		}
		c.When = sub.When
	}
	if sub.Event != "" {
		if c.Event != "" && c.Event != sub.Event {
			return false
		}
		c.Event = sub.Event
	}
	if len(sub.Branches) > 0 {
		if len(c.Branches) > 0 {
			return false // an intersection of branch sets is not portable
		}
		c.Branches = sub.Branches
	}
	return true
}

// condition renders an IR condition as an Azure expression ("" for none).
func (e *emitter) condition(c *ir.Condition, path string) string {
	if c.IsZero() {
		return ""
	}
	if c.Raw != "" {
		if c.RawFrom == "azure" {
			return c.Raw
		}
		e.d.Warnf(path, "condition %q (from %s) could not be translated and was dropped", c.Raw, c.RawFrom)
	}
	var parts []string
	switch c.Event {
	case ir.EventTag:
		parts = append(parts, "startsWith(variables['Build.SourceBranch'], 'refs/tags/')")
	case ir.EventPush:
		parts = append(parts, "in(variables['Build.Reason'], 'IndividualCI', 'BatchedCI')")
	case ir.EventPullRequest:
		parts = append(parts, "eq(variables['Build.Reason'], 'PullRequest')")
	case ir.EventSchedule:
		parts = append(parts, "eq(variables['Build.Reason'], 'Schedule')")
	case ir.EventManual:
		parts = append(parts, "eq(variables['Build.Reason'], 'Manual')")
	}
	var br []string
	for _, b := range c.Branches {
		switch {
		case b == "*" || b == "**":
			br = append(br, "startsWith(variables['Build.SourceBranch'], 'refs/heads/')")
		case strings.HasSuffix(b, "*") && !ir.IsGlob(strings.TrimRight(b, "*")):
			br = append(br, fmt.Sprintf("startsWith(variables['Build.SourceBranch'], 'refs/heads/%s')", quoteLit(strings.TrimRight(b, "*"))))
		case ir.IsGlob(b):
			e.d.Warnf(path, "branch pattern %q cannot be expressed in an Azure condition and was dropped", b)
		default:
			br = append(br, fmt.Sprintf("eq(variables['Build.SourceBranch'], 'refs/heads/%s')", quoteLit(b)))
		}
	}
	switch {
	case len(br) == 1:
		parts = append(parts, br[0])
	case len(br) > 1:
		parts = append(parts, "or("+strings.Join(br, ", ")+")")
	}
	status := "succeeded()"
	switch c.When {
	case ir.WhenAlways:
		status = "always()"
	case ir.WhenOnFailure:
		status = "failed()"
	}
	if len(parts) == 0 {
		if c.When == "" {
			return ""
		}
		return status
	}
	return "and(" + status + ", " + strings.Join(parts, ", ") + ")"
}

func quoteLit(s string) string { return strings.ReplaceAll(s, "'", "''") }
