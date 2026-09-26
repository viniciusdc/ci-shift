package circleci

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/providers/github"
	"github.com/viniciusdc/ci-shift/providers/gitlab"
	"github.com/viniciusdc/ci-shift/schema"
)

func parseFixture(t *testing.T) *ir.Pipeline {
	t.Helper()
	data, err := os.ReadFile("testdata/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	var d ir.Diagnostics
	p, err := Provider{}.Parse(data, &d)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range d.List() {
		t.Log(x)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
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
		var ids []string
		for _, x := range p.Jobs {
			ids = append(ids, x.ID)
		}
		t.Fatalf("job %q not found in %v", id, ids)
	}
	return j
}

func findStep(j *ir.Job, kind ir.StepKind) *ir.Step {
	for _, st := range j.Steps {
		if st.Kind == kind {
			return st
		}
	}
	return nil
}

func TestParseFixture(t *testing.T) {
	p := parseFixture(t)

	if p.Name != "build-test-deploy" {
		t.Errorf("name = %q", p.Name)
	}
	push := p.Triggers.Push
	if push == nil || !reflect.DeepEqual(push.Branches, []string{"main", "release/*"}) {
		t.Errorf("push trigger = %+v", push)
	}
	if len(p.Triggers.Schedules) != 1 || p.Triggers.Schedules[0].Cron != "0 3 * * *" ||
		!reflect.DeepEqual(p.Triggers.Schedules[0].Branches, []string{"main"}) {
		t.Errorf("schedules = %+v", p.Triggers.Schedules)
	}

	build := mustJob(t, p, "build")
	if build.Image != "cimg/node:20.11" {
		t.Errorf("build image = %q", build.Image)
	}
	if len(build.Services) != 1 || build.Services[0].Name != "db" || build.Services[0].Image != "postgres:16" {
		t.Errorf("build services = %+v", build.Services)
	}
	if v, _ := ir.KVGet(build.Services[0].Env, "POSTGRES_PASSWORD"); v != "example" {
		t.Errorf("service env = %+v", build.Services[0].Env)
	}
	if v, _ := ir.KVGet(build.Env, "NODE_ENV"); v != "test" {
		t.Errorf("build env = %+v", build.Env)
	}
	want := []ir.StepKind{ir.StepCheckout, ir.StepCache, ir.StepRun, ir.StepRun, ir.StepUpload}
	if !reflect.DeepEqual(kinds(build), want) {
		t.Errorf("build steps = %v, want %v", kinds(build), want)
	}
	c := findStep(build, ir.StepCache).Cache
	if c.Prefix != "v1-deps" || !reflect.DeepEqual(c.Files, []string{"package-lock.json"}) || !reflect.DeepEqual(c.Paths, []string{"~/.npm"}) {
		t.Errorf("cache = %+v", c)
	}
	if run := build.Steps[3]; run.Name != "Build" || !strings.Contains(run.Run, "${{ ci.sha }}") {
		t.Errorf("build run = %+v", run)
	}
	up := findStep(build, ir.StepUpload).Artifact
	if up.Name != "bundle" || !reflect.DeepEqual(up.Paths, []string{"dist"}) {
		t.Errorf("upload = %+v", up)
	}

	test := mustJob(t, p, "test")
	if !reflect.DeepEqual(test.Needs, []string{"build"}) {
		t.Errorf("test needs = %v", test.Needs)
	}
	if test.Matrix == nil || len(test.Matrix.Axes) != 1 || test.Matrix.Axes[0].Name != "node" ||
		!reflect.DeepEqual(test.Matrix.Axes[0].Values, []string{"18", "20"}) {
		t.Errorf("test matrix = %+v", test.Matrix)
	}
	if test.Image != "cimg/node:${{ matrix.node }}" {
		t.Errorf("test image = %q", test.Image)
	}
	want = []ir.StepKind{ir.StepCheckout, ir.StepDownload, ir.StepRun, ir.StepUpload, ir.StepRun}
	if !reflect.DeepEqual(kinds(test), want) {
		t.Errorf("test steps = %v, want %v", kinds(test), want)
	}
	if v, _ := ir.KVGet(test.Steps[2].Env, "NODE_VERSION"); v != "${{ matrix.node }}" {
		t.Errorf("test step env = %+v", test.Steps[2].Env)
	}
	cov := test.Steps[4]
	if cov.If == nil || cov.If.When != ir.WhenAlways || !strings.Contains(cov.Run, "${{ secret.CODECOV_TOKEN }}") {
		t.Errorf("coverage step = %+v (if %+v)", cov, cov.If)
	}

	deploy := mustJob(t, p, "deploy")
	if !reflect.DeepEqual(deploy.Needs, []string{"test"}) || deploy.If == nil || !deploy.If.Manual {
		t.Errorf("deploy needs=%v if=%+v", deploy.Needs, deploy.If)
	}
	if deploy.Runner.VMImage != "ubuntu-2204:current" || deploy.Image != "" {
		t.Errorf("deploy runner = %+v image %q", deploy.Runner, deploy.Image)
	}
	act := findStep(deploy, ir.StepAction)
	if act == nil || act.Uses != "node/install-packages" || act.UsesFrom != "circleci" {
		t.Errorf("deploy action = %+v", act)
	}
	last := deploy.Steps[len(deploy.Steps)-1]
	if last.Run != "./deploy.sh staging ${{ ci.branch }}" {
		t.Errorf("deploy run = %q", last.Run)
	}

	audit := mustJob(t, p, "nightly-audit")
	if audit.If == nil || audit.If.Event != ir.EventSchedule {
		t.Errorf("audit condition = %+v", audit.If)
	}
	if p.FindJob("hold") != nil {
		t.Error("approval job must not become an IR job")
	}
}

func validate(t *testing.T, out []byte) {
	t.Helper()
	issues, err := schema.Validate("circleci", out, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		t.Errorf("schema: %s", i)
	}
	if t.Failed() {
		t.Logf("emitted:\n%s", out)
	}
}

func emit(t *testing.T, p *ir.Pipeline) []byte {
	t.Helper()
	var d ir.Diagnostics
	out, err := Provider{}.Emit(p, &d)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range d.List() {
		t.Log(x)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	p := parseFixture(t)
	out := emit(t, p)
	validate(t, out)
	t.Logf("emitted:\n%s", out)

	var d ir.Diagnostics
	q, err := Provider{}.Parse(out, &d)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Jobs) != len(p.Jobs) {
		t.Fatalf("jobs: %d != %d", len(q.Jobs), len(p.Jobs))
	}
	for i, a := range p.Jobs {
		b := q.Jobs[i]
		if a.ID != b.ID || !reflect.DeepEqual(a.Needs, b.Needs) || a.Image != b.Image {
			t.Errorf("job %s: id/needs/image %q %v %q != %q %v %q", a.ID, a.ID, a.Needs, a.Image, b.ID, b.Needs, b.Image)
		}
		if !reflect.DeepEqual(a.Services, b.Services) {
			t.Errorf("job %s services %+v != %+v", a.ID, a.Services, b.Services)
		}
		if !reflect.DeepEqual(a.Matrix, b.Matrix) {
			t.Errorf("job %s matrix %+v != %+v", a.ID, a.Matrix, b.Matrix)
		}
		if !reflect.DeepEqual(kinds(a), kinds(b)) {
			t.Errorf("job %s steps %v != %v", a.ID, kinds(a), kinds(b))
		}
		if !reflect.DeepEqual(a.If, b.If) {
			t.Errorf("job %s condition %+v != %+v", a.ID, a.If, b.If)
		}
		for k := range a.Steps {
			if k >= len(b.Steps) {
				break
			}
			sa, sb := a.Steps[k], b.Steps[k]
			if !reflect.DeepEqual(sa.Cache, sb.Cache) || sa.Run != sb.Run || !reflect.DeepEqual(sa.If, sb.If) {
				t.Errorf("job %s step %d: %+v != %+v", a.ID, k, sa, sb)
			}
		}
	}
	if !reflect.DeepEqual(p.Triggers, q.Triggers) {
		t.Errorf("triggers %+v != %+v", p.Triggers, q.Triggers)
	}
}

const githubWorkflow = `
name: CI
on:
  push:
    branches: [main, 'release/**']
    tags: ['v*']
  pull_request:
  schedule:
    - cron: '0 4 * * 1'
  workflow_dispatch:
env:
  GLOBAL: "1"
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          submodules: recursive
      - uses: actions/setup-node@v4
        with:
          node-version: 20
      - uses: actions/cache@v4
        with:
          path: ~/.npm
          key: ${{ runner.os }}-npm-${{ hashFiles('**/package-lock.json', '**/yarn.lock') }}
      - run: npm ci && npm run lint
        continue-on-error: true
  test:
    needs: lint
    runs-on: ubuntu-22.04
    timeout-minutes: 30
    strategy:
      matrix:
        python-version: ["3.10", "3.12"]
        os-name: [linux, mac]
        exclude:
          - python-version: "3.10"
            os-name: mac
        include:
          - python-version: "3.13"
            os-name: linux
    services:
      redis:
        image: redis:7
        ports: ["6379:6379"]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: ${{ matrix.python-version }}
      - run: |
          pip install -r requirements.txt
          pytest --junitxml=reports/junit.xml
        env:
          API_TOKEN: ${{ secrets.API_TOKEN }}
          REPO: ${{ github.repository }}
      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: reports
          path: |
            reports/
            coverage/*.xml
      - if: github.ref == 'refs/heads/main'
        run: echo main only
      - uses: codecov/codecov-action@v4
  windows:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v4
      - run: dir
        shell: pwsh
  mac:
    runs-on: macos-14
    steps:
      - run: xcodebuild -version
  selfhosted:
    runs-on: [self-hosted, linux, gpu]
    steps:
      - run: nvidia-smi
  release:
    needs: [test]
    if: startsWith(github.ref, 'refs/tags/')
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - uses: actions/download-artifact@v4
        with:
          name: reports
      - run: docker build -t app:${{ github.sha }} . && ./release.sh ${{ github.ref_name }}
  notify:
    needs: [release]
    if: failure()
    runs-on: ubuntu-latest
    steps:
      - run: echo "failed in ${{ github.run_id }}"
`

const gitlabConfig = `
stages: [build, test, deploy]
variables:
  APP: demo
build:
  stage: build
  image: golang:1.22
  cache:
    key:
      files: [go.sum]
      prefix: gomod
    paths: [.cache/go]
  script:
    - go build ./...
  artifacts:
    paths: [bin/]
    expire_in: 1 week
test:
  stage: test
  image: golang:1.22
  services:
    - name: postgres:16
      alias: db
  parallel:
    matrix:
      - GOOS: [linux, darwin]
        GOARCH: [amd64]
      - GOOS: [windows]
        GOARCH: [arm64]
  script:
    - go test ./... -tags $GOOS
deploy:
  stage: deploy
  image: alpine:3
  script:
    - ./deploy.sh $DEPLOY_TOKEN $CI_COMMIT_SHA
  rules:
    - if: $CI_COMMIT_BRANCH == "main"
      when: manual
  environment: production
  allow_failure: true
tag-release:
  stage: deploy
  script: [echo release]
  rules:
    - if: $CI_COMMIT_TAG
`

func TestEmitFromGitHub(t *testing.T) {
	var d ir.Diagnostics
	p, err := github.Provider{}.Parse([]byte(githubWorkflow), &d)
	if err != nil {
		t.Fatal(err)
	}
	out := emit(t, p)
	validate(t, out)
	t.Logf("emitted:\n%s", out)
	s := string(out)
	for _, want := range []string{
		"setup_remote_docker", "windows-server-2022-gui:current", "macos:", "resource_class: gpu",
		"<< parameters.python-version >>", "exclude:", "restore_cache:", "save_cache:", "persist_to_workspace:",
		"attach_workspace:", "store_artifacts:", "|| true", `[ "${CIRCLE_BRANCH:-}" = "main" ]`,
		"scheduled:", "cron: 0 4 * * 1", "notify:\n", "failed", "${CIRCLE_PROJECT_USERNAME}/${CIRCLE_PROJECT_REPONAME}",
		"codecov/codecov-action@v4", "exit 1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// The emitted file must read back into the same job graph.
	q, err := Provider{}.Parse(out, &d)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Jobs) != len(p.Jobs) {
		t.Errorf("round trip jobs %d != %d", len(q.Jobs), len(p.Jobs))
	}
}

func TestEmitFromGitLab(t *testing.T) {
	var d ir.Diagnostics
	p, err := gitlab.Provider{}.Parse([]byte(gitlabConfig), &d)
	if err != nil {
		t.Fatal(err)
	}
	out := emit(t, p)
	validate(t, out)
	t.Logf("emitted:\n%s", out)
	s := string(out)
	for _, want := range []string{"name: db", "exclude:", "GOOS: << parameters.GOOS >>", "hold-deploy", "tags:"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestDetect(t *testing.T) {
	data, err := os.ReadFile("testdata/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		data string
		min  int
		max  int
	}{
		{".circleci/config.yml", "", 100, 100},
		{"config.yml", string(data), 60, 90},
		{"ci.yml", githubWorkflow, 0, 0},
		{"ci.yml", gitlabConfig, 0, 0},
		{"azure.yml", "trigger: [main]\npool:\n  vmImage: ubuntu-latest\nsteps:\n  - script: echo hi\n", 0, 0},
		{"azure.yml", "stages:\n  - stage: A\n    jobs:\n      - job: B\n        steps:\n          - script: echo\n", 0, 0},
		{"bb.yml", "pipelines:\n  default:\n    - step:\n        script: [echo]\n", 0, 0},
	}
	for _, c := range cases {
		got := Provider{}.Detect(c.path, []byte(c.data))
		if got < c.min || got > c.max {
			t.Errorf("Detect(%s) = %d, want [%d,%d]", c.path, got, c.min, c.max)
		}
	}
}

func TestParseCommandsAndConditions(t *testing.T) {
	cfg := `
version: 2.1
commands:
  greet:
    parameters:
      who: {type: string, default: world}
      extra: {type: steps, default: []}
    steps:
      - run: echo hello << parameters.who >>
      - << parameters.extra >>
jobs:
  a:
    docker: [{image: cimg/go:1.22}]
    working_directory: ~/project/app
    steps:
      - checkout
      - greet:
          who: you
          extra:
            - run: echo extra
      - unless:
          condition: true
          steps: [{run: echo never}]
      - run:
          command: make || true
          working_directory: sub
          shell: /bin/sh -eo pipefail
  b:
    docker: [{image: cimg/base:stable}]
    steps: [{run: echo "$CIRCLE_PROJECT_USERNAME/$CIRCLE_PROJECT_REPONAME"}]
workflows:
  main:
    jobs:
      - a:
          filters:
            tags: {only: /^v.*/}
            branches: {ignore: /.*/}
      - b:
          requires: [{a: [success, failed]}]
          filters:
            tags: {only: /^v.*/}
            branches: {ignore: /.*/}
`
	var d ir.Diagnostics
	p, err := Provider{}.Parse([]byte(cfg), &d)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range d.List() {
		t.Log(x)
	}
	if p.Triggers.Push == nil || !reflect.DeepEqual(p.Triggers.Push.Tags, []string{"v*"}) || len(p.Triggers.Push.Branches) != 0 {
		t.Errorf("push = %+v", p.Triggers.Push)
	}
	a := mustJob(t, p, "a")
	var runs []string
	for _, st := range a.Steps {
		if st.Kind == ir.StepRun {
			runs = append(runs, st.Run)
		}
	}
	if !reflect.DeepEqual(runs, []string{"echo hello you", "echo extra", "make"}) {
		t.Errorf("runs = %q", runs)
	}
	last := a.Steps[len(a.Steps)-1]
	if !last.AllowFailure || last.WorkingDir != "app/sub" || last.Shell != "sh" {
		t.Errorf("last step = %+v", last)
	}
	b := mustJob(t, p, "b")
	if b.If == nil || b.If.When != ir.WhenAlways || !reflect.DeepEqual(b.Needs, []string{"a"}) {
		t.Errorf("b = needs %v if %+v", b.Needs, b.If)
	}
	if b.Steps[0].Run != `echo "${{ ci.repository }}"` {
		t.Errorf("b run = %q", b.Steps[0].Run)
	}
	out := emit(t, p)
	validate(t, out)
	if !strings.Contains(string(out), "ignore: /.*/") || !strings.Contains(string(out), "a: terminal") {
		t.Errorf("unexpected output:\n%s", out)
	}
}
