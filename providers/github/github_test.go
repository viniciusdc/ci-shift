package github

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/viniciusdc/cistela/ir"
	"github.com/viniciusdc/cistela/schema"
)

func parseFixture(t *testing.T) *ir.Pipeline {
	t.Helper()
	data, err := os.ReadFile("../../testdata/github/node-app.yml")
	if err != nil {
		t.Fatal(err)
	}
	var d ir.Diagnostics
	p, err := Provider{}.Parse(data, &d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	p := parseFixture(t)
	if p.Name != "Node app" {
		t.Errorf("name = %q", p.Name)
	}
	tr := p.Triggers
	if tr.Push == nil || !reflect.DeepEqual(tr.Push.Branches, []string{"main", "release/**"}) || !reflect.DeepEqual(tr.Push.Tags, []string{"v*"}) {
		t.Errorf("push = %+v", tr.Push)
	}
	if tr.PullRequest == nil || len(tr.Schedules) != 1 || !tr.Manual {
		t.Errorf("triggers = %+v", tr)
	}
	if len(p.Jobs) != 4 {
		t.Fatalf("jobs = %d", len(p.Jobs))
	}
	lint := p.Jobs[0]
	kinds := []ir.StepKind{}
	for _, s := range lint.Steps {
		kinds = append(kinds, s.Kind)
	}
	if !reflect.DeepEqual(kinds, []ir.StepKind{ir.StepCheckout, ir.StepTool, ir.StepCache, ir.StepRun, ir.StepRun}) {
		t.Errorf("lint kinds = %v", kinds)
	}
	if c := lint.Steps[2].Cache; c.Prefix != "npm" || !reflect.DeepEqual(c.Files, []string{"package-lock.json"}) || !reflect.DeepEqual(c.Paths, []string{"~/.npm"}) {
		t.Errorf("cache = %+v", c)
	}
	test := p.FindJob("test")
	if test.Matrix == nil || len(test.Matrix.Combos()) != 2 || test.TimeoutMinutes != 20 {
		t.Errorf("test job = %+v", test)
	}
	if test.Steps[0].Checkout.Depth != -1 {
		t.Errorf("fetch-depth 0 should mean full history")
	}
	if up := test.Steps[4]; up.Kind != ir.StepUpload || up.Artifact.When != ir.WhenAlways || up.Artifact.ExpireIn != "7 days" {
		t.Errorf("upload = %+v", up.Artifact)
	}
	build := p.FindJob("build")
	if !strings.Contains(build.Steps[2].Run, "${{ ci.sha }}") || !strings.Contains(build.Steps[2].Run, "${{ ci.ref_name }}") {
		t.Errorf("expressions not canonical: %q", build.Steps[2].Run)
	}
	deploy := p.FindJob("deploy")
	if deploy.If == nil || !reflect.DeepEqual(deploy.If.Branches, []string{"main"}) || deploy.Environment != "production" {
		t.Errorf("deploy = %+v", deploy)
	}
	if !strings.Contains(deploy.Steps[1].Run, "${{ secret.DEPLOY_TOKEN }}") {
		t.Errorf("secret not canonical: %q", deploy.Steps[1].Run)
	}
	if a := deploy.Steps[2]; a.Kind != ir.StepAction || a.UsesFrom != "github" {
		t.Errorf("action = %+v", a)
	}
}

func TestParseIf(t *testing.T) {
	cases := map[string]ir.Condition{
		"always()":         {When: ir.WhenAlways},
		"${{ failure() }}": {When: ir.WhenOnFailure},
		"github.ref == 'refs/heads/main' && github.event_name == 'push'":    {Branches: []string{"main"}, Event: ir.EventPush},
		"github.ref == 'refs/heads/main' || github.ref == 'refs/heads/dev'": {Branches: []string{"main", "dev"}},
		"startsWith(github.ref, 'refs/tags/')":                              {Event: ir.EventTag},
		"success() && (github.ref_name == 'a' || github.ref_name == 'b')":   {Branches: []string{"a", "b"}},
		"contains(github.event.head_commit.message, '[skip]')":              {Raw: "contains(github.event.head_commit.message, '[skip]')", RawFrom: "github"},
	}
	for in, want := range cases {
		if got := parseIf(in); !reflect.DeepEqual(*got, want) {
			t.Errorf("parseIf(%q) = %+v, want %+v", in, *got, want)
		}
	}
}

func TestRoundTripStable(t *testing.T) {
	p := parseFixture(t)
	var d ir.Diagnostics
	out, err := Provider{}.Emit(p, &d)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := schema.Validate("github", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) > 0 {
		t.Fatalf("schema issues: %v\n%s", issues, out)
	}
	p2, err := Provider{}.Parse(out, &d)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := Provider{}.Emit(p2, &d)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("emit is not stable:\n%s\n---\n%s", out, out2)
	}
}

func TestCommonDir(t *testing.T) {
	cases := map[string][]string{
		"dist":    {"dist/"},
		".":       {"coverage.xml"},
		"build":   {"build/a.txt", "build/b.txt"},
		"out":     {"out/**/*.js"},
		"reports": {"reports/unit", "reports/e2e"},
	}
	for want, in := range cases {
		if got := commonDir(in); got != want {
			t.Errorf("commonDir(%v) = %q, want %q", in, got, want)
		}
	}
}
