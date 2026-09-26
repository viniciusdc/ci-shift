package bitbucket

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/viniciusdc/cistela/ir"
	"github.com/viniciusdc/cistela/providers/github"
	"github.com/viniciusdc/cistela/providers/gitlab"
	"github.com/viniciusdc/cistela/schema"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parse(t *testing.T, data []byte) (*ir.Pipeline, *ir.Diagnostics) {
	t.Helper()
	var d ir.Diagnostics
	p, err := Provider{}.Parse(data, &d)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p, &d
}

// emitValid emits p and requires the result to pass the Bitbucket schema.
func emitValid(t *testing.T, p *ir.Pipeline, d *ir.Diagnostics) []byte {
	t.Helper()
	out, err := Provider{}.Emit(p, d)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := schema.Validate("bitbucket", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) > 0 {
		t.Fatalf("schema issues: %v\n%s", issues, out)
	}
	return out
}

func job(t *testing.T, p *ir.Pipeline, id string) *ir.Job {
	t.Helper()
	j := p.FindJob(id)
	if j == nil {
		var ids []string
		for _, x := range p.Jobs {
			ids = append(ids, x.ID)
		}
		t.Fatalf("job %q not found in %v", id, ids)
	}
	return j
}

func stepsOf(j *ir.Job, kind ir.StepKind) []*ir.Step {
	var out []*ir.Step
	for _, st := range j.Steps {
		if st.Kind == kind {
			out = append(out, st)
		}
	}
	return out
}

func hasDiag(d *ir.Diagnostics, sev ir.Severity, substr string) bool {
	for _, x := range d.List() {
		if x.Severity == sev && strings.Contains(x.Message, substr) {
			return true
		}
	}
	return false
}

func TestDetect(t *testing.T) {
	p := Provider{}
	if got := p.Detect("repo/bitbucket-pipelines.yml", nil); got != 100 {
		t.Errorf("basename: got %d", got)
	}
	if got := p.Detect("ci.yml", []byte("pipelines:\n  default:\n    - step:\n        script: [ls]\n")); got != 80 {
		t.Errorf("content: got %d", got)
	}
	if got := p.Detect("ci.yml", []byte("jobs:\n  a:\n    runs-on: x\n")); got != 0 {
		t.Errorf("other: got %d", got)
	}
}

func TestParseFixture(t *testing.T) {
	p, d := parse(t, readFixture(t, "full.yml"))

	var ids []string
	for _, j := range p.Jobs {
		ids = append(ids, j.ID)
	}
	want := []string{
		"default-build", "default-unit-tests", "default-lint",
		"main-build", "main-publish", "tag-v-tag-build", "pr-pr-check",
		"deploy-staging-build", "deploy-staging-deploy",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("job ids = %v, want %v", ids, want)
	}

	// Triggers: default + tags keeps branch pushes, PRs and custom runs.
	tr := p.Triggers
	if tr.Push == nil || !reflect.DeepEqual(tr.Push.Branches, []string{"**"}) || !reflect.DeepEqual(tr.Push.Tags, []string{"v*"}) {
		t.Errorf("push trigger = %+v", tr.Push)
	}
	if tr.PullRequest == nil || !tr.Manual {
		t.Errorf("triggers = %+v", tr)
	}

	build := job(t, p, "default-build")
	if build.Image != "node:20" || build.TimeoutMinutes != 30 || build.Needs != nil {
		t.Errorf("build = %+v", build)
	}
	if build.If == nil || build.If.Event != ir.EventPush {
		t.Errorf("default job condition = %+v", build.If)
	}
	if co := build.Steps[0]; co.Kind != ir.StepCheckout || co.Checkout.Depth != 5 || !co.Checkout.LFS {
		t.Errorf("checkout = %+v", co.Checkout)
	}
	caches := stepsOf(build, ir.StepCache)
	if len(caches) != 2 ||
		!reflect.DeepEqual(caches[0].Cache, &ir.Cache{Prefix: "node", Paths: []string{"node_modules"}}) ||
		!reflect.DeepEqual(caches[1].Cache, &ir.Cache{Prefix: "npm-cache", Files: []string{"package-lock.json"}, Paths: []string{"~/.npm"}}) {
		t.Errorf("caches = %+v", caches)
	}
	runs := stepsOf(build, ir.StepRun)
	if len(runs) != 1 || runs[0].Run != "npm ci\nnpm run build -- --sha ${{ ci.sha }}" {
		t.Errorf("script = %+v", runs)
	}
	if up := stepsOf(build, ir.StepUpload); len(up) != 1 || !reflect.DeepEqual(up[0].Artifact.Paths, []string{"dist/**"}) {
		t.Errorf("uploads = %+v", up)
	}

	// Parallel group: both jobs need the previous item and download artifacts.
	unit := job(t, p, "default-unit-tests")
	lint := job(t, p, "default-lint")
	for _, j := range []*ir.Job{unit, lint} {
		if !reflect.DeepEqual(j.Needs, []string{"default-build"}) {
			t.Errorf("%s needs = %v", j.ID, j.Needs)
		}
		if len(stepsOf(j, ir.StepDownload)) != 1 {
			t.Errorf("%s has no artifact download", j.ID)
		}
	}
	if len(unit.Services) != 1 || unit.Services[0].Name != "postgres" || unit.Services[0].Image != "postgres:16" ||
		!reflect.DeepEqual(unit.Services[0].Env, []ir.KV{{Key: "POSTGRES_PASSWORD", Value: "example"}}) {
		t.Errorf("services = %+v", unit.Services)
	}
	unitRuns := stepsOf(unit, ir.StepRun)
	if len(unitRuns) != 2 || !strings.Contains(unitRuns[0].Run, "${{ secret.NPM_TOKEN }}") {
		t.Fatalf("unit runs = %+v", unitRuns)
	}
	if after := unitRuns[1]; after.If == nil || after.If.When != ir.WhenAlways || !after.AllowFailure {
		t.Errorf("after-script = %+v", after)
	}

	// Stage, deployment, runner, pipe.
	pub := job(t, p, "main-publish")
	if pub.Stage != "Release" || pub.Environment != "production" || pub.TimeoutMinutes != 60 {
		t.Errorf("publish = %+v", pub)
	}
	if !reflect.DeepEqual(pub.Needs, []string{"main-build"}) {
		t.Errorf("publish needs = %v", pub.Needs)
	}
	if !reflect.DeepEqual(pub.Runner.Labels, []string{"linux", "big"}) || !pub.Runner.IsLinux() {
		t.Errorf("runner = %+v", pub.Runner)
	}
	if pub.If == nil || !reflect.DeepEqual(pub.If.Branches, []string{"main"}) {
		t.Errorf("publish condition = %+v", pub.If)
	}
	pipes := stepsOf(pub, ir.StepAction)
	if len(pipes) != 1 || pipes[0].Uses != "atlassian/slack-notify:2.1.0" || pipes[0].UsesFrom != "bitbucket" ||
		!reflect.DeepEqual(pipes[0].With, []ir.KV{{Key: "WEBHOOK_URL", Value: "${{ secret.SLACK_WEBHOOK }}"}, {Key: "MESSAGE", Value: "published"}}) {
		t.Errorf("pipe = %+v", pipes)
	}

	tag := job(t, p, "tag-v-tag-build")
	if tag.Image != "node:22" || tag.If == nil || tag.If.Event != ir.EventTag {
		t.Errorf("tag job = %+v", tag)
	}
	if r := stepsOf(tag, ir.StepRun); len(r) != 1 || r[0].Run != `echo "${{ ci.tag }}"` {
		t.Errorf("tag script = %+v", r)
	}
	pr := job(t, p, "pr-pr-check")
	if pr.If == nil || pr.If.Event != ir.EventPullRequest || pr.Steps[0].Checkout.Depth != -1 {
		t.Errorf("pr job = %+v", pr)
	}

	deploy := job(t, p, "deploy-staging-deploy")
	if deploy.If == nil || deploy.If.Event != ir.EventManual || !deploy.If.Manual || deploy.Environment != "staging" {
		t.Errorf("deploy = %+v", deploy)
	}
	if len(stepsOf(deploy, ir.StepDownload)) != 0 {
		t.Error("artifacts.download: false should suppress the download")
	}

	if !hasDiag(d, ir.SeverityWarning, "custom pipeline variables") || !hasDiag(d, ir.SeverityWarning, "schedules") {
		t.Errorf("missing diagnostics: %v", d.List())
	}
}

func TestSingleSectionHasNoPrefixOrCondition(t *testing.T) {
	src := `
pipelines:
  branches:
    '{main,develop}':
      - step:
          script: [make]
      - step:
          name: Test
          trigger: manual
          script: [make test]
`
	p, _ := parse(t, []byte(src))
	if len(p.Jobs) != 2 || p.Jobs[0].ID != "step-1" || p.Jobs[1].ID != "test" {
		t.Fatalf("jobs = %+v %+v", p.Jobs[0], p.Jobs[1])
	}
	if p.Jobs[0].If != nil || p.Jobs[1].If == nil || !p.Jobs[1].If.Manual || len(p.Jobs[1].If.Branches) != 0 {
		t.Errorf("conditions = %+v %+v", p.Jobs[0].If, p.Jobs[1].If)
	}
	if !reflect.DeepEqual(p.Triggers.Push.Branches, []string{"main", "develop"}) {
		t.Errorf("branches = %v", p.Triggers.Push.Branches)
	}
	var d ir.Diagnostics
	out := string(emitValid(t, p, &d))
	if !strings.Contains(out, "'{main,develop}':") {
		t.Errorf("branches should be merged into a brace pattern:\n%s", out)
	}
}

func TestRoundTrip(t *testing.T) {
	p, d := parse(t, readFixture(t, "full.yml"))
	out := emitValid(t, p, d)
	again, err := Provider{}.Emit(p, &ir.Diagnostics{})
	if err != nil || !bytes.Equal(out, again) {
		t.Fatalf("emit is not deterministic")
	}
	q, _ := parse(t, out)

	var ids []string
	for _, j := range q.Jobs {
		ids = append(ids, j.ID)
	}
	want := []string{
		"default-build", "default-unit-tests", "default-lint",
		"main-build", "main-publish", "tag-v-tag-build", "pr-pr-check",
		"manual-build", "manual-deploy",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("round-trip ids = %v\n%s", ids, out)
	}
	needs := map[string][]string{
		"default-unit-tests": {"default-build"},
		"default-lint":       {"default-build"},
		"main-publish":       {"main-build"},
		"manual-deploy":      {"manual-build"},
	}
	for _, j := range q.Jobs {
		if !reflect.DeepEqual(j.Needs, needs[j.ID]) {
			t.Errorf("%s needs = %v, want %v", j.ID, j.Needs, needs[j.ID])
		}
		wantImg := "node:20"
		if j.ID == "tag-v-tag-build" {
			wantImg = "node:22"
		}
		if j.Image != wantImg || j.TimeoutMinutes == 0 {
			t.Errorf("%s image/timeout = %q/%d", j.ID, j.Image, j.TimeoutMinutes)
		}
	}
	orig, back := job(t, p, "default-build"), job(t, q, "default-build")
	if !reflect.DeepEqual(orig.Steps[1:], back.Steps[1:]) || !reflect.DeepEqual(orig.Steps[0].Checkout, back.Steps[0].Checkout) {
		t.Errorf("build steps changed:\n%+v\n%+v", orig.Steps, back.Steps)
	}
	if len(stepsOf(job(t, q, "default-lint"), ir.StepDownload)) != 1 || len(stepsOf(job(t, q, "manual-deploy"), ir.StepDownload)) != 0 {
		t.Error("artifact downloads changed")
	}
	pub := job(t, q, "main-publish")
	if !reflect.DeepEqual(pub.If.Branches, []string{"main"}) || pub.Environment != "production" ||
		!reflect.DeepEqual(pub.Runner.Labels, []string{"linux", "big"}) || pub.TimeoutMinutes != 60 {
		t.Errorf("publish = %+v", pub)
	}
	if a := stepsOf(pub, ir.StepAction); len(a) != 1 || !reflect.DeepEqual(a[0].With, job(t, p, "main-publish").Steps[3].With) {
		t.Errorf("pipe = %+v", a)
	}
	if j := job(t, q, "tag-v-tag-build"); j.If.Event != ir.EventTag {
		t.Errorf("tag condition = %+v", j.If)
	}
	if j := job(t, q, "pr-pr-check"); j.If.Event != ir.EventPullRequest || j.Steps[0].Checkout.Depth != -1 {
		t.Errorf("pr job = %+v", j)
	}
	if j := job(t, q, "manual-deploy"); j.If.Event != ir.EventManual || !j.If.Manual || j.Environment != "staging" {
		t.Errorf("manual deploy = %+v", j.If)
	}
	if u := job(t, q, "default-unit-tests"); len(u.Services) != 1 || u.Services[0].Image != "postgres:16" {
		t.Errorf("services = %+v", u.Services)
	}
}

func TestFromGitHub(t *testing.T) {
	var d ir.Diagnostics
	p, err := github.Provider{}.Parse(readFixture(t, "github.yml"), &d)
	if err != nil {
		t.Fatal(err)
	}
	out := string(emitValid(t, p, &d))
	for _, want := range []string{
		"image: atlassian/default-image:4",
		"name: test (3.11, pg)",
		`echo "py 3.12 on $DB_URL"`,
		"includePaths:",
		"'**':",
		"scheduled:",
		"deployment: production",
		"- deployer",
		"depth: full",
		"git submodule update --init --recursive",
		"export TOKEN=\"${DEPLOY_TOKEN}\"",
		"pytest || true",
		"after-script:",
		"download: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// deploy only runs on pushes to main.
	if strings.Count(out, "name: deploy") != 1 {
		t.Errorf("deploy should only be in branches.main:\n%s", out)
	}
	if !hasDiag(&d, ir.SeverityWarning, "Schedules with cron 0 3 * * 1") || !hasDiag(&d, ir.SeverityWarning, "codecov") {
		t.Errorf("missing diagnostics: %v", d.List())
	}
	q, _ := parse(t, []byte(out))
	if len(q.Jobs) == 0 {
		t.Fatal("output does not parse back")
	}
}

func TestFromGitLab(t *testing.T) {
	var d ir.Diagnostics
	p, err := gitlab.Provider{}.Parse(readFixture(t, "gitlab.yml"), &d)
	if err != nil {
		t.Fatal(err)
	}
	out := string(emitValid(t, p, &d))
	for _, want := range []string{
		"default:", "main:", "'*':", "'**':",
		"trigger: manual",
		"image: golang:1.21",
		"export GOOS=darwin",
		"go test ./... -run ${BITBUCKET_COMMIT}",
		"echo release ${BITBUCKET_TAG}",
		"depth: 10",
		"- go.sum",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !hasDiag(&d, ir.SeverityWarning, "cannot be allowed to fail") {
		t.Errorf("missing allow_failure warning: %v", d.List())
	}
}

func TestEmitFirstManualStepAndEmptyPipeline(t *testing.T) {
	p := &ir.Pipeline{Jobs: []*ir.Job{{
		ID: "deploy", If: &ir.Condition{Manual: true},
		Steps: []*ir.Step{{Kind: ir.StepRun, Run: "echo ${{ ci.event }}"}},
	}}}
	var d ir.Diagnostics
	out := string(emitValid(t, p, &d))
	if !strings.Contains(out, "name: Start") || !strings.Contains(out, "trigger: manual") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if !hasDiag(&d, ir.SeverityWarning, "ci.event") {
		t.Errorf("unknown CI variable not reported: %v", d.List())
	}
	emitValid(t, &ir.Pipeline{}, &ir.Diagnostics{})
}
