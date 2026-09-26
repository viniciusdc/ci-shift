// Package convert wires the providers together: detection, parsing,
// emitting and whole-file conversion.
package convert

import (
	"fmt"
	"sort"
	"strings"

	"github.com/viniciusdc/cistela/ir"
	"github.com/viniciusdc/cistela/providers/azure"
	"github.com/viniciusdc/cistela/providers/bitbucket"
	"github.com/viniciusdc/cistela/providers/circleci"
	"github.com/viniciusdc/cistela/providers/github"
	"github.com/viniciusdc/cistela/providers/gitlab"
)

var providers = []ir.Provider{
	github.Provider{},
	gitlab.Provider{},
	circleci.Provider{},
	azure.Provider{},
	bitbucket.Provider{},
}

var aliases = map[string]string{
	"gh":                  "github",
	"github-actions":      "github",
	"actions":             "github",
	"gl":                  "gitlab",
	"gitlab-ci":           "gitlab",
	"circle":              "circleci",
	"az":                  "azure",
	"azure-pipelines":     "azure",
	"azure-devops":        "azure",
	"ado":                 "azure",
	"bb":                  "bitbucket",
	"bitbucket-pipelines": "bitbucket",
}

// Providers returns every registered provider.
func Providers() []ir.Provider { return append([]ir.Provider(nil), providers...) }

// Names returns the provider names.
func Names() []string {
	var out []string
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}

// Get returns a provider by name or alias.
func Get(name string) (ir.Provider, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if a, ok := aliases[n]; ok {
		n = a
	}
	for _, p := range providers {
		if p.Name() == n {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(Names(), ", "))
}

// Detect guesses the provider of a config file from its path and content.
func Detect(path string, data []byte) (ir.Provider, error) {
	type scored struct {
		p     ir.Provider
		score int
	}
	var all []scored
	for _, p := range providers {
		if s := p.Detect(path, data); s > 0 {
			all = append(all, scored{p, s})
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("could not detect the CI provider of %s; pass --from", path)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > 1 && all[0].score == all[1].score {
		return nil, fmt.Errorf("%s looks like both %s and %s; pass --from", path, all[0].p.Name(), all[1].p.Name())
	}
	return all[0].p, nil
}

// Result is the outcome of a conversion.
type Result struct {
	From        ir.Provider
	To          ir.Provider
	Pipeline    *ir.Pipeline
	Output      []byte
	Diagnostics []ir.Diagnostic
}

// Options tune a conversion.
type Options struct {
	// Header adds a comment block to the output naming the source and
	// listing conversion warnings.
	Header bool
	// SourceName is shown in the header.
	SourceName string
}

// Convert parses data with from and emits it with to.
func Convert(data []byte, from, to ir.Provider, opts Options) (*Result, error) {
	var diags ir.Diagnostics
	p, err := from.Parse(data, &diags)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", from.Name(), err)
	}
	out, err := to.Emit(p, &diags)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", to.Name(), err)
	}
	res := &Result{From: from, To: to, Pipeline: p, Diagnostics: diags.List()}
	if opts.Header {
		out = append([]byte(header(res, opts.SourceName)), out...)
	}
	res.Output = out
	return res, nil
}

func header(r *Result, source string) string {
	var b strings.Builder
	src := r.From.Title()
	if source != "" {
		src = fmt.Sprintf("%s (%s)", source, r.From.Title())
	}
	fmt.Fprintf(&b, "# Converted by cistela from %s to %s.\n", src, r.To.Title())
	warnings := 0
	for _, d := range r.Diagnostics {
		if d.Severity == ir.SeverityWarning {
			warnings++
		}
	}
	if warnings == 0 {
		b.WriteString("# Review before use.\n\n")
		return b.String()
	}
	fmt.Fprintf(&b, "# Review before use; %d item(s) need attention:\n", warnings)
	for _, d := range r.Diagnostics {
		if d.Severity != ir.SeverityWarning {
			continue
		}
		line := d.Message
		if d.Path != "" {
			line = d.Path + ": " + line
		}
		fmt.Fprintf(&b, "#   - %s\n", strings.ReplaceAll(line, "\n", " "))
	}
	b.WriteString("\n")
	return b.String()
}
