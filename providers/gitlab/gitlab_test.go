package gitlab

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/schema"
)

func parse(t *testing.T, data []byte) (*ir.Pipeline, *ir.Diagnostics) {
	t.Helper()
	d := &ir.Diagnostics{}
	p, err := Provider{}.Parse(data, d)
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/gitlab/python-lib.gitlab-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func stepKinds(j *ir.Job) []ir.StepKind {
	var out []ir.StepKind
	for _, s := range j.Steps {
		out = append(out, s.Kind)
	}
	return out
}

func TestParseFixture(t *testing.T) {
	p, _ := parse(t, fixture(t))
	tr := p.Triggers
	if tr.PullRequest == nil || tr.Push == nil || !reflect.DeepEqual(tr.Push.Branches, []string{"main"}) || !reflect.DeepEqual(tr.Push.Tags, []string{"*"}) {
		t.Errorf("triggers = %+v push=%+v", tr, tr.Push)
	}
	if v, _ := ir.KVGet(p.Env, "PIP_CACHE_DIR"); v != "${{ ci.workspace }}/.cache/pip" {
		t.Errorf("PIP_CACHE_DIR = %q", v)
	}
	var ids []string
	for _, j := range p.Jobs {
		ids = append(ids, j.ID)
	}
	if !reflect.DeepEqual(ids, []string{"ruff", "pytest", "build-wheel", "publish"}) {
		t.Fatalf("jobs = %v (hidden templates must be skipped)", ids)
	}
	pytest := p.FindJob("pytest")
	if pytest.Image != "python:${{ matrix.PY }}" {
		t.Errorf("image = %q", pytest.Image)
	}
	if !reflect.DeepEqual(pytest.Needs, []string{"ruff"}) {
		t.Errorf("needs from stage order = %v", pytest.Needs)
	}
	if len(pytest.Services) != 1 || pytest.Services[0].Name != "redis" {
		t.Errorf("services = %+v", pytest.Services)
	}
	want := []ir.StepKind{ir.StepCheckout, ir.StepCache, ir.StepRun, ir.StepRun, ir.StepUpload}
	if got := stepKinds(pytest); !reflect.DeepEqual(got, want) {
		t.Errorf("pytest steps = %v", got)
	}
	// build-wheel follows implicit stage order and receives pytest artifacts;
	// publish uses explicit needs and only receives build-wheel's.
	if got := stepKinds(p.FindJob("build-wheel")); got[1] != ir.StepDownload {
		t.Errorf("build-wheel steps = %v", got)
	}
	publish := p.FindJob("publish")
	var downloads []string
	for _, s := range publish.Steps {
		if s.Kind == ir.StepDownload {
			downloads = append(downloads, s.Artifact.Name)
		}
	}
	if !reflect.DeepEqual(downloads, []string{"build-wheel"}) {
		t.Errorf("publish downloads = %v", downloads)
	}
	if publish.If == nil || publish.If.Event != ir.EventTag || publish.Environment != "pypi" {
		t.Errorf("publish = %+v", publish.If)
	}
	if !strings.Contains(publish.Steps[len(publish.Steps)-1].Run, "${{ secret.PYPI_TOKEN }}") {
		t.Errorf("PYPI_TOKEN not promoted to a secret")
	}
}

func TestExtendsReferenceAndRules(t *testing.T) {
	p, d := parse(t, []byte(`
.setup:
  before_script:
    - echo setup
.base:
  image: alpine
  variables:
    A: "1"
  script: [echo base]
job:
  extends: .base
  variables:
    B: "2"
  script:
    - !reference [.setup, before_script]
    - echo job
  rules:
    - if: $CI_COMMIT_BRANCH =~ /^release\/.*$/
      when: manual
  tags: [docker, saas-linux-small-amd64]
  timeout: 1h 30m
  allow_failure: true
  after_script:
    - echo cleanup
`))
	j := p.Jobs[0]
	if j.Image != "alpine" || len(j.Env) != 2 {
		t.Errorf("extends not merged: image=%q env=%v", j.Image, j.Env)
	}
	if j.Steps[1].Run != "echo setup\necho job" {
		t.Errorf("!reference not flattened: %q", j.Steps[1].Run)
	}
	if j.If == nil || !j.If.Manual || !reflect.DeepEqual(j.If.Branches, []string{"release/*"}) {
		t.Errorf("rules = %+v", j.If)
	}
	if !reflect.DeepEqual(j.Runner.Labels, []string{"docker"}) || j.TimeoutMinutes != 90 || !j.AllowFailure {
		t.Errorf("job = %+v", j)
	}
	last := j.Steps[len(j.Steps)-1]
	if last.If == nil || last.If.When != ir.WhenAlways {
		t.Errorf("after_script should always run: %+v", last)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("unexpected warnings: %v", d.Warnings())
	}
}

func TestEmitRoundTripAndSchema(t *testing.T) {
	p, d := parse(t, fixture(t))
	out, err := Provider{}.Emit(p, d)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := schema.Validate("gitlab", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) > 0 {
		t.Fatalf("schema issues: %v\n%s", issues, out)
	}
	p2, _ := parse(t, out)
	out2, err := Provider{}.Emit(p2, d)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("emit is not stable:\n%s\n---\n%s", out, out2)
	}
}

func TestWrapScript(t *testing.T) {
	var d ir.Diagnostics
	st := &ir.Step{
		Kind: ir.StepRun, Run: "make test", Env: []ir.KV{{Key: "MODE", Value: "ci run"}},
		WorkingDir: "app", If: &ir.Condition{Branches: []string{"main"}}, AllowFailure: true,
	}
	got, _ := wrapScript(st, "x", &d)
	want := "if [ \"${CI_COMMIT_BRANCH:-}\" = \"main\" ]; then\n  (\n    export MODE='ci run'\n    cd app\n    make test\n  )\nfi || true"
	if !strings.Contains(got, "export MODE='ci run'") || !strings.HasPrefix(got, "(\n") && !strings.HasPrefix(got, "if ") {
		t.Fatalf("got:\n%s\nwant something like:\n%s", got, want)
	}
}
