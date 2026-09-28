package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/viniciusdc/ci-shift/e2e/conformance"
)

// fakeProvider replays a scripted sequence of run states.
type fakeProvider struct {
	name     string
	setupErr error
	findNil  int // Find returns nil this many times first
	states   []Status
	jobs     []JobResult
	started  bool

	finds, refreshes int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Remote() Remote {
	return Remote{URL: "https://example.invalid/" + f.name + ".git"}
}
func (f *fakeProvider) Setup(context.Context) error { return f.setupErr }

func (f *fakeProvider) Find(context.Context, string, string) (*Run, error) {
	f.finds++
	if f.finds <= f.findNil {
		return nil, nil
	}
	return f.state(0), nil
}

func (f *fakeProvider) Refresh(context.Context, *Run) (*Run, error) {
	f.refreshes++
	return f.state(f.refreshes), nil
}

func (f *fakeProvider) state(i int) *Run {
	if i >= len(f.states) {
		i = len(f.states) - 1
	}
	return &Run{ID: "7", URL: "https://example.invalid/run/7", Status: f.states[i], Raw: string(f.states[i])}
}

func (f *fakeProvider) Jobs(context.Context, *Run) ([]JobResult, error) { return f.jobs, nil }

type triggerProvider struct{ fakeProvider }

func (t *triggerProvider) Start(context.Context, string) (*Run, error) {
	t.started = true
	return &Run{ID: "9", Status: StatusSuccess, Raw: "succeeded"}, nil
}

type fakePusher struct {
	pushErr error
	pushed  map[string][]byte
	deleted []string
}

func (p *fakePusher) Push(_ context.Context, _ Remote, _ string, files map[string][]byte) (string, error) {
	p.pushed = files
	return "abc123", p.pushErr
}

func (p *fakePusher) Delete(_ context.Context, _ Remote, branch string) error {
	p.deleted = append(p.deleted, branch)
	return nil
}

// passingJobs is what a provider reports when every check passes.
func passingJobs() []JobResult {
	var out []JobResult
	for _, j := range conformance.Jobs {
		switch {
		case j.Skipped:
			out = append(out, JobResult{Name: j.ID, Status: StatusSkipped, Raw: "skipped"})
		case j.Legs > 0:
			out = append(out, JobResult{Name: j.ID + " (red)", Status: StatusSuccess, Raw: "success"},
				JobResult{Name: j.ID + " (blue)", Status: StatusSuccess, Raw: "success"})
		default:
			out = append(out, JobResult{Name: j.ID, Status: StatusSuccess, Raw: "success"})
		}
	}
	return out
}

var fast = Options{RunID: "1-1", Timeout: time.Second, PollMin: time.Millisecond, PollMax: time.Millisecond, TriggerAfter: 5 * time.Millisecond}

func TestExecutePasses(t *testing.T) {
	p := &fakeProvider{name: "gitlab", findNil: 2, states: []Status{StatusPending, StatusRunning, StatusSuccess}, jobs: passingJobs()}
	push := &fakePusher{}
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: p}}, push, fast)
	if len(res) != 1 || res[0].Outcome != OutcomePassed {
		t.Fatalf("got %+v", res)
	}
	if _, ok := push.pushed[".gitlab-ci.yml"]; !ok {
		t.Errorf("pushed %v, want .gitlab-ci.yml", keys(push.pushed))
	}
	if _, ok := push.pushed[conformance.SourcePath]; !ok {
		t.Errorf("pushed %v, want %s", keys(push.pushed), conformance.SourcePath)
	}
	if len(push.deleted) != 1 || push.deleted[0] != "e2e/1-1-gitlab" {
		t.Errorf("deleted %v", push.deleted)
	}
	if Failed(res) {
		t.Error("Failed should be false")
	}
}

func TestExecuteSkipsUnconfigured(t *testing.T) {
	push := &fakePusher{}
	res := Execute(context.Background(), []Candidate{{Name: "azure", Missing: []string{"E2E_AZURE_ORG", "E2E_AZURE_TOKEN"}}}, push, fast)
	if res[0].Outcome != OutcomeSkipped || !strings.Contains(res[0].Reason, "E2E_AZURE_TOKEN") {
		t.Fatalf("got %+v", res[0])
	}
	if push.pushed != nil || push.deleted != nil {
		t.Error("a skipped provider must not touch git")
	}
	if Failed(res) {
		t.Error("skipped providers must not fail the run")
	}
}

func TestExecuteDryRun(t *testing.T) {
	push := &fakePusher{}
	cands := []Candidate{
		{Name: "bitbucket", Missing: []string{"E2E_BITBUCKET_TOKEN"}},
		{Name: "gitlab", P: &fakeProvider{name: "gitlab"}},
	}
	opts := fast
	opts.DryRun = true
	res := Execute(context.Background(), cands, push, opts)
	for _, r := range res {
		if r.Outcome != OutcomeDryRun || len(r.Files) != 2 {
			t.Errorf("got %+v", r)
		}
	}
	if push.pushed != nil || push.deleted != nil {
		t.Error("a dry run must not touch git")
	}
}

func TestExecuteFailedJobFailsRun(t *testing.T) {
	jobs := passingJobs()
	jobs[0].Status, jobs[0].Raw = StatusFailed, "failed"
	p := &fakeProvider{name: "gitlab", states: []Status{StatusFailed}, jobs: jobs}
	push := &fakePusher{}
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: p}}, push, fast)
	if res[0].Outcome != OutcomeFailed || res[0].Checks[0].OK {
		t.Fatalf("got %+v", res[0])
	}
	if len(push.deleted) != 1 {
		t.Error("the branch must be deleted after a failure")
	}
	if !Failed(res) {
		t.Error("Failed should be true")
	}
}

func TestExecuteDeletesBranchAfterPushError(t *testing.T) {
	push := &fakePusher{pushErr: errors.New("denied")}
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: &fakeProvider{name: "gitlab"}}}, push, fast)
	if res[0].Outcome != OutcomeFailed || !strings.Contains(res[0].Reason, "denied") {
		t.Fatalf("got %+v", res[0])
	}
	if len(push.deleted) != 1 {
		t.Error("the branch must be deleted even when the push failed")
	}
}

func TestExecuteSetupErrorDoesNotPush(t *testing.T) {
	push := &fakePusher{}
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: &fakeProvider{name: "gitlab", setupErr: errors.New("403")}}}, push, fast)
	if res[0].Outcome != OutcomeFailed || push.pushed != nil {
		t.Fatalf("got %+v, pushed %v", res[0], push.pushed)
	}
}

func TestExecuteTimesOut(t *testing.T) {
	p := &fakeProvider{name: "gitlab", states: []Status{StatusRunning}}
	opts := fast
	opts.Timeout = 20 * time.Millisecond
	push := &fakePusher{}
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: p}}, push, opts)
	if res[0].Outcome != OutcomeFailed || !strings.Contains(res[0].Reason, "did not finish") {
		t.Fatalf("got %+v", res[0])
	}
	if len(push.deleted) != 1 {
		t.Error("the branch must be deleted after a timeout")
	}
}

func TestExecuteNoRunTimesOut(t *testing.T) {
	p := &fakeProvider{name: "gitlab", findNil: 1 << 30, states: []Status{StatusRunning}}
	opts := fast
	opts.Timeout = 20 * time.Millisecond
	res := Execute(context.Background(), []Candidate{{Name: "gitlab", P: p}}, &fakePusher{}, opts)
	if res[0].Outcome != OutcomeFailed || !strings.Contains(res[0].Reason, "no run started") {
		t.Fatalf("got %+v", res[0])
	}
}

func TestExecuteTriggersWhenNoRunAppears(t *testing.T) {
	p := &triggerProvider{fakeProvider{name: "azure", findNil: 1 << 30, states: []Status{StatusSuccess}, jobs: passingJobs()}}
	res := Execute(context.Background(), []Candidate{{Name: "azure", P: p}}, &fakePusher{}, fast)
	if !p.started {
		t.Fatal("Start was not called")
	}
	if res[0].Outcome != OutcomePassed {
		t.Fatalf("got %+v", res[0])
	}
}

func TestEvaluate(t *testing.T) {
	t.Run("all pass, skipped job absent", func(t *testing.T) {
		var jobs []JobResult
		for _, j := range passingJobs() {
			if j.Name != "never-runs" {
				jobs = append(jobs, j)
			}
		}
		jobs = append(jobs, JobResult{Name: "Build setup", Status: StatusSuccess, Raw: "success"})
		checks, extra := Evaluate(jobs)
		for _, c := range checks {
			if !c.OK {
				t.Errorf("%s: %s", c.Job.ID, c.Note)
			}
		}
		if len(extra) != 1 || extra[0].Name != "Build setup" {
			t.Errorf("extra = %v", extra)
		}
	})
	t.Run("skipped job ran", func(t *testing.T) {
		jobs := passingJobs()
		for i := range jobs {
			if jobs[i].Name == "never-runs" {
				jobs[i].Status, jobs[i].Raw = StatusFailed, "failed"
			}
		}
		if ok := check(t, jobs, "never-runs"); ok.OK {
			t.Error("a job that must be skipped ran and passed the check")
		}
	})
	t.Run("missing matrix leg", func(t *testing.T) {
		var jobs []JobResult
		for _, j := range passingJobs() {
			if !strings.HasSuffix(j.Name, "(blue)") {
				jobs = append(jobs, j)
			}
		}
		if c := check(t, jobs, "matrix-legs"); c.OK || !strings.Contains(c.Note, "expected 2") {
			t.Errorf("got %+v", c)
		}
	})
	t.Run("missing job", func(t *testing.T) {
		var jobs []JobResult
		for _, j := range passingJobs() {
			if j.Name != "on-test-branch" {
				jobs = append(jobs, j)
			}
		}
		if c := check(t, jobs, "on-test-branch"); c.OK {
			t.Errorf("got %+v", c)
		}
	})
}

func check(t *testing.T, jobs []JobResult, id string) Check {
	t.Helper()
	checks, _ := Evaluate(jobs)
	for _, c := range checks {
		if c.Job.ID == id {
			return c
		}
	}
	t.Fatalf("no check for %s", id)
	return Check{}
}

func TestSelect(t *testing.T) {
	env := map[string]string{"E2E_GITLAB_PROJECT": "me/e2e", "E2E_GITLAB_TOKEN": "glpat"}
	cands, err := Select([]string{"gitlab", " Azure ", "gitlab", ""}, func(k string) string { return env[k] }, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates", len(cands))
	}
	if cands[0].Name != "gitlab" || len(cands[0].Missing) != 0 {
		t.Errorf("gitlab: %+v", cands[0])
	}
	if cands[1].Name != "azure" || len(cands[1].Missing) == 0 {
		t.Errorf("azure should be missing settings: %+v", cands[1])
	}
	if _, err := Select([]string{"jenkins"}, func(string) string { return "" }, ""); err == nil {
		t.Error("unknown providers must be rejected")
	}
}

func TestSummary(t *testing.T) {
	checks, _ := Evaluate(passingJobs())
	checks[1].OK, checks[1].Note = false, "artifact-produce was failed"
	results := []Result{
		{Provider: "gitlab", Outcome: OutcomeFailed, Reason: "conformance checks failed", Run: &Run{ID: "42", URL: "https://gitlab.example/p/42"}, Checks: checks, Elapsed: 3 * time.Minute},
		{Provider: "azure", Outcome: OutcomeSkipped, Reason: "missing E2E_AZURE_TOKEN"},
		{Provider: "circleci", Outcome: OutcomeDryRun, Reason: "not configured", Files: []string{".circleci/config.yml (10 bytes)"}},
	}
	var b strings.Builder
	Summary(&b, results)
	out := b.String()
	for _, want := range []string{
		"| gitlab | ❌ failed | [42](https://gitlab.example/p/42) | 3m0s | conformance checks failed |",
		"| azure | ⏭️ skipped | – | – | missing E2E_AZURE_TOKEN |",
		"| circleci | 📝 dry-run | – | – | not configured; files: .circleci/config.yml (10 bytes) |",
		"### gitlab jobs",
		"| `artifact-produce` |",
		"❌ artifact-produce was failed",
		"matrix-legs (red): success<br>matrix-legs (blue): success",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "### azure") {
		t.Error("skipped providers have no job table")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
