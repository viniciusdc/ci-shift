package azure

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/providers/github"
	"github.com/viniciusdc/ci-shift/providers/gitlab"
	"github.com/viniciusdc/ci-shift/schema"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseFixture(t *testing.T, name string) (*ir.Pipeline, *ir.Diagnostics) {
	t.Helper()
	var d ir.Diagnostics
	p, err := Provider{}.Parse(readFixture(t, name), &d)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p, &d
}

func jobIDs(p *ir.Pipeline) []string {
	var out []string
	for _, j := range p.Jobs {
		out = append(out, j.ID)
	}
	return out
}

func kinds(j *ir.Job) []ir.StepKind {
	var out []ir.StepKind
	for _, st := range j.Steps {
		out = append(out, st.Kind)
	}
	return out
}

func mustJob(t *testing.T, p *ir.Pipeline, id string) *ir.Job {
	t.Helper()
	j := p.FindJob(id)
	if j == nil {
		t.Fatalf("job %q not found in %v", id, jobIDs(p))
	}
	return j
}

func eq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func hasDiag(d *ir.Diagnostics, substr string) bool {
	for _, x := range d.List() {
		if strings.Contains(x.String(), substr) {
			return true
		}
	}
	return false
}

func TestParseMultiStage(t *testing.T) {
	p, d := parseFixture(t, "multistage.yml")

	eq(t, "name", p.Name, "")
	eq(t, "push", *p.Triggers.Push, ir.PushTrigger{
		Branches:       []string{"main", "release/*"},
		BranchesIgnore: []string{"experimental/*"},
		Tags:           []string{"v*"},
		PathsIgnore:    []string{"docs/*"},
	})
	eq(t, "pr", *p.Triggers.PullRequest, ir.PRTrigger{Branches: []string{"main"}, Paths: []string{"src/*"}})
	eq(t, "schedules", p.Triggers.Schedules, []ir.Schedule{{Cron: "0 3 * * 1", Branches: []string{"main"}}})
	eq(t, "pipeline env", p.Env, []ir.KV{
		{Key: "buildConfiguration", Value: "Release"},
		{Key: "NODE_ENV", Value: "production"},
		{Key: "python_version", Value: "3.12"},
	})
	eq(t, "job ids", jobIDs(p), []string{"Build_lint", "test", "package", "Package_lint", "release", "notify"})

	lint := mustJob(t, p, "Build_lint")
	eq(t, "lint stage", lint.Stage, "Build")
	eq(t, "lint name", lint.Name, "Lint")
	eq(t, "lint image", lint.Image, "node:20")
	eq(t, "lint env", lint.Env, []ir.KV{{Key: "STAGE_VAR", Value: "build"}})
	eq(t, "lint steps", kinds(lint), []ir.StepKind{ir.StepCheckout, ir.StepTool, ir.StepCache, ir.StepRun})
	eq(t, "lint checkout", *lint.Steps[0].Checkout, ir.Checkout{Depth: 1, Submodules: true, LFS: true})
	eq(t, "lint tool", [2]string{lint.Steps[1].Tool, lint.Steps[1].Version}, [2]string{ir.ToolNode, "20.x"})
	eq(t, "lint cache", *lint.Steps[2].Cache, ir.Cache{Prefix: "npm", Files: []string{"package-lock.json"}, Paths: []string{"$(Pipeline.Workspace)/.npm"}})
	eq(t, "lint run", lint.Steps[3].Run, "npm ci\nnpm run lint -- --config ${{ env.buildConfiguration }}")
	eq(t, "lint run env", lint.Steps[3].Env, []ir.KV{{Key: "NPM_TOKEN", Value: "${{ secret.NPM_TOKEN }}"}})

	test := mustJob(t, p, "test")
	eq(t, "test needs", test.Needs, []string{"Build_lint"})
	eq(t, "test timeout", test.TimeoutMinutes, 30)
	eq(t, "test allow failure", test.AllowFailure, true)
	eq(t, "test runner", test.Runner, ir.Runner{OS: ir.OSLinux, VMImage: "${{ matrix.imageName }}"})
	eq(t, "test matrix", test.Matrix.Combos(), [][]ir.KV{
		{{Key: "imageName", Value: "ubuntu-latest"}, {Key: "python_version", Value: "3.11"}},
		{{Key: "imageName", Value: "macOS-latest"}, {Key: "python_version", Value: "3.12"}},
	})
	eq(t, "test services", test.Services, []ir.Service{{
		Name: "postgres", Image: "postgres:16",
		Env:   []ir.KV{{Key: "POSTGRES_PASSWORD", Value: "${{ secret.DB_PASSWORD }}"}},
		Ports: []string{"5432:5432"},
	}})
	eq(t, "test steps", kinds(test), []ir.StepKind{ir.StepCheckout, ir.StepTool, ir.StepRun, ir.StepUpload, ir.StepAction})
	eq(t, "test python", test.Steps[1].Version, "${{ matrix.python_version }}")
	run := test.Steps[2]
	eq(t, "test run", run.Run, "echo \"sha ${{ ci.sha }} on ${{ ci.ref_name }}\"\necho \"python ${{ matrix.python_version }}\"\npytest --junitxml=out/results.xml")
	eq(t, "test shell", run.Shell, "bash")
	eq(t, "test workdir", run.WorkingDir, "${{ ci.workspace }}/tests")
	eq(t, "test run if", *run.If, ir.Condition{When: ir.WhenAlways})
	eq(t, "test upload", *test.Steps[3].Artifact, ir.Artifact{Name: "test-results", Paths: []string{"out"}, When: ir.WhenAlways})
	eq(t, "test upload if", test.Steps[3].If, (*ir.Condition)(nil))
	action := test.Steps[4]
	eq(t, "action", [2]string{action.Uses, action.UsesFrom}, [2]string{"SomeVendorTask@3", "azure"})
	eq(t, "action with", action.With, []ir.KV{{Key: "flag", Value: "true"}, {Key: "target", Value: "${{ ci.run_id }}"}})

	pkg := mustJob(t, p, "package")
	eq(t, "package needs", pkg.Needs, []string{"Build_lint", "test"})
	eq(t, "package runner", pkg.Runner, ir.Runner{Labels: []string{"SelfHosted"}})
	eq(t, "package steps", kinds(pkg), []ir.StepKind{ir.StepDownload, ir.StepUpload})
	eq(t, "package download", pkg.Steps[0].Artifact.Name, "test-results")
	eq(t, "package upload", *pkg.Steps[1].Artifact, ir.Artifact{Name: "bundle", Paths: []string{"dist"}})
	eq(t, "package if", *pkg.If, ir.Condition{Branches: []string{"main"}})
	eq(t, "package lint", mustJob(t, p, "Package_lint").Needs, []string{"Build_lint", "test"})

	rel := mustJob(t, p, "release")
	eq(t, "release env", rel.Environment, "production")
	eq(t, "release needs", rel.Needs, []string{"package", "Package_lint"})
	eq(t, "release steps", kinds(rel), []ir.StepKind{ir.StepDownload, ir.StepRun})
	eq(t, "release run", rel.Steps[1].Run, "./deploy.sh ${{ ci.run_number }}")
	eq(t, "release if", *rel.If, ir.Condition{Event: ir.EventTag})

	notify := mustJob(t, p, "notify")
	eq(t, "notify needs", notify.Needs, []string{"release"})
	eq(t, "notify if", *notify.If, ir.Condition{Branches: []string{"main", "hotfix/*"}})
	eq(t, "notify command substitution", notify.Steps[1].Run, `echo "$(pwd)" notified`)
	eq(t, "notify raw", *notify.Steps[2].If, ir.Condition{Raw: "and(succeeded(), ne(variables['Build.Reason'], 'PullRequest'))", RawFrom: "azure"})

	for _, want := range []string{
		"variables defined outside the file were treated as secrets: DB_PASSWORD, DEPLOY_TOKEN, NPM_TOKEN",
		`run number format "$(Date:yyyyMMdd)$(Rev:.r)" was dropped`,
		`deployment lifecycle hook "preDeploy"`,
		`step template "steps/notify.yml" is not resolved`,
		"disabled step was dropped",
		"task SomeVendorTask@3 has no portable equivalent",
	} {
		if !hasDiag(d, want) {
			t.Errorf("missing diagnostic %q in %v", want, d.List())
		}
	}
}

func TestParseImplicitJob(t *testing.T) {
	src := `
trigger: none
pr: [main]
pool:
  vmImage: windows-latest
variables:
  - name: CONFIG
    value: debug
  - group: shared
steps:
  - pwsh: Write-Host $(CONFIG) $(SECRET_KEY) $env:BUILD_BUILDID
  - task: Bash@3
    inputs:
      targetType: inline
      script: echo $BUILD_SOURCEVERSION $(git rev-parse HEAD)
`
	var d ir.Diagnostics
	p, err := Provider{}.Parse([]byte(src), &d)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "push", p.Triggers.Push, (*ir.PushTrigger)(nil))
	eq(t, "pr", *p.Triggers.PullRequest, ir.PRTrigger{Branches: []string{"main"}})
	eq(t, "ids", jobIDs(p), []string{"job"})
	j := p.Jobs[0]
	eq(t, "runner", j.Runner, ir.Runner{OS: ir.OSWindows, VMImage: "windows-latest"})
	eq(t, "steps", kinds(j), []ir.StepKind{ir.StepCheckout, ir.StepRun, ir.StepRun})
	eq(t, "pwsh", j.Steps[1].Run, "Write-Host ${{ env.CONFIG }} ${{ secret.SECRET_KEY }} $env:BUILD_BUILDID")
	eq(t, "bash task", [2]string{j.Steps[2].Shell, j.Steps[2].Run}, [2]string{"bash", "echo ${{ ci.sha }} $(git rev-parse HEAD)"})
	if !hasDiag(&d, `variable group "shared"`) {
		t.Errorf("missing variable group warning: %v", d.List())
	}
}

func TestConditions(t *testing.T) {
	cases := []struct {
		in   string
		want ir.Condition
	}{
		{"always()", ir.Condition{When: ir.WhenAlways}},
		{"failed()", ir.Condition{When: ir.WhenOnFailure}},
		{"and(succeeded(), eq(variables['Build.SourceBranch'], 'refs/heads/main'))", ir.Condition{Branches: []string{"main"}}},
		{"eq(variables['build.sourcebranchname'], 'dev')", ir.Condition{Branches: []string{"dev"}}},
		{"startsWith(variables['Build.SourceBranch'], 'refs/tags/')", ir.Condition{Event: ir.EventTag}},
		{"and(always(), eq(variables['Build.Reason'], 'PullRequest'))", ir.Condition{When: ir.WhenAlways, Event: ir.EventPullRequest}},
		{"eq(variables['Build.Reason'], 'Schedule')", ir.Condition{Event: ir.EventSchedule}},
		{"eq('Manual', variables['Build.Reason'])", ir.Condition{Event: ir.EventManual}},
		{"in(variables['Build.Reason'], 'IndividualCI', 'BatchedCI')", ir.Condition{Event: ir.EventPush}},
		{"in(variables['Build.SourceBranch'], 'refs/heads/a', 'refs/heads/b')", ir.Condition{Branches: []string{"a", "b"}}},
		{"or(eq(variables['Build.SourceBranch'], 'refs/heads/a'), startsWith(variables['Build.SourceBranch'], 'refs/heads/rel/'))", ir.Condition{Branches: []string{"a", "rel/*"}}},
		{"canceled()", ir.Condition{Raw: "canceled()", RawFrom: "azure"}},
		{"eq(dependencies.a.result, 'Succeeded')", ir.Condition{Raw: "eq(dependencies.a.result, 'Succeeded')", RawFrom: "azure"}},
		{"and(succeeded(", ir.Condition{Raw: "and(succeeded(", RawFrom: "azure"}},
	}
	ps := &parser{d: &ir.Diagnostics{}, promoted: map[string]bool{}}
	e := &emitter{d: &ir.Diagnostics{}}
	for _, c := range cases {
		got := ps.parseCondition(c.in, "x")
		if got == nil {
			t.Errorf("%s: got nil", c.in)
			continue
		}
		eq(t, c.in, *got, c.want)
		// Every translated condition must survive an emit/parse cycle.
		back := ps.parseCondition(e.condition(got, "x"), "x")
		eq(t, c.in+" (round trip)", *back, c.want)
	}
	if ps.parseCondition("succeeded()", "x") != nil {
		t.Errorf("succeeded() should be the default (nil) condition")
	}
}

// collapse merges consecutive downloads: an unnamed download of every
// upstream artifact is emitted as one task per artifact.
func collapse(ks []ir.StepKind) []ir.StepKind {
	var out []ir.StepKind
	for _, k := range ks {
		if k == ir.StepDownload && len(out) > 0 && out[len(out)-1] == ir.StepDownload {
			continue
		}
		out = append(out, k)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	p1, _ := parseFixture(t, "multistage.yml")
	var d ir.Diagnostics
	out, err := Provider{}.Emit(p1, &d)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Provider{}.Emit(p1, &ir.Diagnostics{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(again) {
		t.Errorf("emit is not deterministic")
	}
	assertFixedPoint(t, out)
	p2, err := Provider{}.Parse(out, &ir.Diagnostics{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	eq(t, "triggers", p2.Triggers, p1.Triggers)
	eq(t, "env", p2.Env, p1.Env)
	eq(t, "job ids", jobIDs(p2), jobIDs(p1))
	for _, j1 := range p1.Jobs {
		j2 := mustJob(t, p2, j1.ID)
		eq(t, j1.ID+" stage", j2.Stage, j1.Stage)
		eq(t, j1.ID+" name", j2.Name, j1.Name)
		eq(t, j1.ID+" needs", j2.Needs, j1.Needs)
		eq(t, j1.ID+" if", j2.If, j1.If)
		eq(t, j1.ID+" runner", j2.Runner, j1.Runner)
		eq(t, j1.ID+" image", j2.Image, j1.Image)
		eq(t, j1.ID+" services", j2.Services, j1.Services)
		eq(t, j1.ID+" env", j2.Env, j1.Env)
		eq(t, j1.ID+" matrix", j2.Matrix.Combos(), j1.Matrix.Combos())
		eq(t, j1.ID+" environment", j2.Environment, j1.Environment)
		eq(t, j1.ID+" timeout", j2.TimeoutMinutes, j1.TimeoutMinutes)
		eq(t, j1.ID+" allow failure", j2.AllowFailure, j1.AllowFailure)
		eq(t, j1.ID+" steps", collapse(kinds(j2)), collapse(kinds(j1)))
		if len(j1.Steps) != len(j2.Steps) {
			continue
		}
		for i, s1 := range j1.Steps {
			s2 := j2.Steps[i]
			// Emitted scripts gain the set line that keeps them
			// stopping at the first failing command.
			want := s1.Run
			if s1.Kind == ir.StepRun && posixScript(j1, s1) {
				want = withErrexit(want, s1.Shell == "bash")
			}
			eq(t, j1.ID+" step run", s2.Run, want)
			eq(t, j1.ID+" step env", s2.Env, s1.Env)
			eq(t, j1.ID+" step if", s2.If, s1.If)
			eq(t, j1.ID+" step cache", s2.Cache, s1.Cache)
			eq(t, j1.ID+" step checkout", s2.Checkout, s1.Checkout)
			eq(t, j1.ID+" step artifact", s2.Artifact, s1.Artifact)
		}
	}
	if t.Failed() {
		t.Logf("emitted:\n%s", out)
	}
}

// assertFixedPoint checks that parsing emitted output and emitting it
// again yields the same document.
func assertFixedPoint(t *testing.T, out []byte) {
	t.Helper()
	p, err := Provider{}.Parse(out, &ir.Diagnostics{})
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	again, err := Provider{}.Emit(p, &ir.Diagnostics{})
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Errorf("emit(parse(output)) is not a fixed point:\n--- first\n%s\n--- again\n%s", out, again)
	}
}

// TestEmitFromOtherProviders converts GitHub and GitLab pipelines and
// checks the result against the official Azure Pipelines schema.
func TestEmitFromOtherProviders(t *testing.T) {
	cases := []struct {
		fixture string
		parse   func([]byte, *ir.Diagnostics) (*ir.Pipeline, error)
		want    []string
	}{
		{"github.yml", github.Provider{}.Parse, []string{
			"vmImage: $(os)",
			"os: macOS-latest",
			"artifact: dist-$(os)",
			"deployment: deploy_job",
			"fetchDepth: '0'",
			"DEPLOY_KEY: $(DEPLOY_KEY)",
			`curl -H "Authorization: ${DEPLOY_KEY}"`,
			"container: golang:1.22",
			"redis: redis",
		}},
		{"gitlab.yml", gitlab.Provider{}.Parse, []string{
			"stages:\n  - stage: build\n    dependsOn: []",
			"key: npm | \"$(Agent.OS)\" | package-lock.json",
			"targetPath: $(Build.SourcesDirectory)/dist",
			"REGISTRY_PASSWORD: $(REGISTRY_PASSWORD)",
			"db: db",
			"pr: none",
		}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			var d ir.Diagnostics
			p, err := c.parse(readFixture(t, c.fixture), &d)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Provider{}.Emit(p, &d)
			if err != nil {
				t.Fatal(err)
			}
			issues, err := schema.Validate("azure", out, schema.Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range issues {
				t.Errorf("schema: %s", i)
			}
			for _, w := range c.want {
				if !strings.Contains(string(out), w) {
					t.Errorf("output lacks %q", w)
				}
			}
			if got := (Provider{}).Detect("pipeline.yml", out); got < 60 {
				t.Errorf("content detection scored %d", got)
			}
			assertFixedPoint(t, out)
			if t.Failed() {
				t.Logf("emitted:\n%s", out)
			}
		})
	}
}

// TestEmitAzureFixtureValid validates the round-tripped fixture against the
// schema; the vendor task is removed because the schema only lists
// Microsoft's tasks.
func TestEmitAzureFixtureValid(t *testing.T) {
	p, _ := parseFixture(t, "multistage.yml")
	for _, j := range p.Jobs {
		var steps []*ir.Step
		for _, st := range j.Steps {
			if st.Kind != ir.StepAction {
				steps = append(steps, st)
			}
		}
		j.Steps = steps
	}
	out, err := Provider{}.Emit(p, &ir.Diagnostics{})
	if err != nil {
		t.Fatal(err)
	}
	issues, err := schema.Validate("azure", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		t.Errorf("schema: %s", i)
	}
}

func TestEmitDetails(t *testing.T) {
	p := &ir.Pipeline{
		Triggers: ir.Triggers{Push: &ir.PushTrigger{Tags: []string{"v*"}}},
		Env:      []ir.KV{{Key: "TOKEN", Value: "${{ secret.TOKEN }}"}},
		Jobs: []*ir.Job{{
			ID:     "build-app",
			Image:  "node:${{ matrix.node }}",
			Matrix: &ir.Matrix{Axes: []ir.Axis{{Name: "node", Values: []string{"18", "20"}}}},
			Steps: []*ir.Step{
				{Kind: ir.StepRun, Run: "echo $node ${{ secret.OTHER }}"},
				{Kind: ir.StepAction, Uses: "orb/foo", UsesFrom: "circleci"},
			},
		}},
	}
	var d ir.Diagnostics
	out, err := Provider{}.Emit(p, &d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"trigger:\n  branches:\n    exclude:\n      - '*'\n  tags:\n    include:\n      - v*",
		"pr: none",
		"- job: build_app",
		"container: $[ format('node:{0}', variables['node']) ]",
		"- checkout: none",
		"script: echo $(node) ${OTHER}",
		"TOKEN: $(TOKEN)",
		"OTHER: $(OTHER)",
		"##vso[task.logissue type=error]ci-shift: port circleci step ''orb/foo'' manually",
		"node_18:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "variables:") {
		t.Errorf("secret-bearing variables must not be emitted as variables:\n%s", s)
	}
	issues, err := schema.Validate("azure", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		t.Errorf("schema: %s", i)
	}
	back, err := Provider{}.Parse(out, &ir.Diagnostics{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "tag-only trigger", *back.Triggers.Push, ir.PushTrigger{Tags: []string{"v*"}})
}

func TestDetect(t *testing.T) {
	az := readFixture(t, "multistage.yml")
	pr := Provider{}
	if got := pr.Detect("azure-pipelines.yml", nil); got != 100 {
		t.Errorf("by name: %d", got)
	}
	if got := pr.Detect(".azuredevops/ci.yml", nil); got != 100 {
		t.Errorf("by dir: %d", got)
	}
	if got := pr.Detect("ci.yml", az); got < 60 || got > 80 {
		t.Errorf("azure content: %d", got)
	}
	others := map[string]string{
		"github":    string(readFixture(t, "github.yml")),
		"gitlab":    string(readFixture(t, "gitlab.yml")),
		"circleci":  "version: 2.1\njobs:\n  build:\n    docker: [{image: cimg/base:current}]\n    steps: [checkout, {run: make}]\nworkflows:\n  main:\n    jobs: [build]\n",
		"bitbucket": "pipelines:\n  default:\n    - step:\n        script: [make]\n",
	}
	for name, src := range others {
		if got := pr.Detect("ci.yml", []byte(src)); got > 10 {
			t.Errorf("%s content scored %d", name, got)
		}
	}
}
