// Package runner pushes the conformance pipeline to real CI providers. It
// waits for each run and checks every job's outcome.
//
// Each provider implements Provider on top of its REST API. A provider
// whose configuration (environment variables) is incomplete is skipped,
// not failed, so the harness can run before any account exists.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/viniciusdc/ci-shift/e2e/conformance"
)

// Status is a job or run state, normalized across providers.
type Status string

// Statuses.
const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusSuccess  Status = "success"
	StatusFailed   Status = "failed"
	StatusSkipped  Status = "skipped"
	StatusCanceled Status = "canceled"
)

// Final reports whether s cannot change any more.
func (s Status) Final() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusSkipped, StatusCanceled:
		return true
	}
	return false
}

// Run is one pipeline run on a provider.
type Run struct {
	ID  string // shown to people (run or build number)
	Ref string // the provider's handle for API calls, when it differs from ID
	URL string
	// Status is the overall state; Raw is the provider's own wording.
	Status Status
	Raw    string
}

// JobResult is one job (or matrix leg) of a run.
type JobResult struct {
	Name   string
	Status Status
	Raw    string
}

// Remote is a git repository the test branch is pushed to. Password is
// sent in an HTTP header through the environment, never written to disk.
type Remote struct {
	URL      string // https URL, without credentials
	Username string
	Password string
	Bearer   bool // send Password as a bearer token, not Basic credentials
}

// Provider is one live CI provider.
type Provider interface {
	// Name is the ci-shift provider name ("gitlab", ...).
	Name() string
	// Remote is where the test branch is pushed.
	Remote() Remote
	// Setup prepares the provider before the push (idempotent): it sets
	// the E2E_SECRET variable and, where needed, finds the pipeline.
	Setup(ctx context.Context) error
	// Find returns the run started for commit on branch, or nil when the
	// provider has not created it yet.
	Find(ctx context.Context, branch, commit string) (*Run, error)
	// Refresh returns the current state of run.
	Refresh(ctx context.Context, run *Run) (*Run, error)
	// Jobs lists the jobs of a finished run.
	Jobs(ctx context.Context, run *Run) ([]JobResult, error)
}

// Trigger is implemented by providers whose push trigger may not fire (a
// pipeline definition that did not exist yet): Start queues the run
// explicitly when none appeared after Options.TriggerAfter.
type Trigger interface {
	Start(ctx context.Context, branch string) (*Run, error)
}

// Adapter is implemented by providers that adjust the converted files for
// the test account before the push, for settings that are not part of the
// conversion (a self-hosted agent pool, for example).
type Adapter interface {
	Adapt(files map[string][]byte) error
}

// Canceler is implemented by providers that can stop a run. A run that is
// still going when the harness gives up is canceled before its branch is
// deleted, so jobs still queued do not fail on the missing branch.
type Canceler interface {
	Cancel(ctx context.Context, run *Run) error
}

// Pusher publishes files to a branch of a remote and deletes branches.
type Pusher interface {
	Push(ctx context.Context, r Remote, branch string, files map[string][]byte) (commit string, err error)
	Delete(ctx context.Context, r Remote, branch string) error
}

// Outcome of one provider.
type Outcome string

// Outcomes.
const (
	OutcomePassed  Outcome = "passed"
	OutcomeFailed  Outcome = "failed"
	OutcomeSkipped Outcome = "skipped"
	OutcomeDryRun  Outcome = "dry-run"
)

// Check is the verdict on one expected job of the conformance pipeline.
type Check struct {
	Job  conformance.Job
	Got  []JobResult
	OK   bool
	Note string
}

// Result is what happened on one provider.
type Result struct {
	Provider string
	Outcome  Outcome
	Reason   string // why it was skipped or failed
	Branch   string
	Run      *Run
	Checks   []Check
	Extra    []JobResult // jobs the pipeline does not define
	Files    []string    // dry run: the files that would be pushed
	Elapsed  time.Duration
}

// Options tune a run.
type Options struct {
	RunID        string        // makes branch names unique: e2e/<RunID>-<provider>
	Timeout      time.Duration // per provider, from push to the finished run
	TriggerAfter time.Duration // see Trigger
	PollMin      time.Duration // first polling interval
	PollMax      time.Duration // polling backoff cap
	DryRun       bool
	Log          io.Writer
}

func (o *Options) defaults() {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.TriggerAfter <= 0 {
		o.TriggerAfter = 2 * time.Minute
	}
	if o.PollMin <= 0 {
		o.PollMin = 5 * time.Second
	}
	if o.PollMax <= 0 {
		o.PollMax = 30 * time.Second
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
}

// Candidate is a provider selected for a run: either configured (P set) or
// skipped for the listed missing settings.
type Candidate struct {
	Name    string
	P       Provider
	Missing []string
}

// Branch returns the test branch name for a provider.
func Branch(runID, provider string) string {
	return "e2e/" + runID + "-" + provider
}

// Files returns what is pushed for a converted pipeline: the pipeline at
// the provider's path plus the conformance source, which the checkout job
// looks for.
func Files(c *conformance.Converted) map[string][]byte {
	return map[string][]byte{
		c.Path:                 c.Output,
		conformance.SourcePath: conformance.Source,
	}
}

// Prepare converts the pipeline for a candidate and returns the files to
// push, adjusted by the provider when it implements Adapter.
func Prepare(c Candidate) (map[string][]byte, error) {
	conv, err := conformance.Convert(c.Name)
	if err != nil {
		return nil, err
	}
	files := Files(conv)
	if a, ok := c.P.(Adapter); ok {
		if err := a.Adapt(files); err != nil {
			return nil, fmt.Errorf("adapt %s: %w", conv.Path, err)
		}
	}
	return files, nil
}

// Execute runs every candidate in turn and returns their results.
func Execute(ctx context.Context, cands []Candidate, push Pusher, opts Options) []Result {
	opts.defaults()
	var out []Result
	for _, c := range cands {
		start := time.Now()
		r := runOne(ctx, c, push, opts)
		r.Elapsed = time.Since(start).Round(time.Second)
		fmt.Fprintf(opts.Log, "%s: %s", r.Provider, r.Outcome)
		if r.Reason != "" {
			fmt.Fprintf(opts.Log, " (%s)", r.Reason)
		}
		fmt.Fprintln(opts.Log)
		out = append(out, r)
	}
	return out
}

func runOne(ctx context.Context, c Candidate, push Pusher, opts Options) Result {
	res := Result{Provider: c.Name, Branch: Branch(opts.RunID, c.Name)}
	logf := func(format string, args ...any) {
		fmt.Fprintf(opts.Log, "%s: "+format+"\n", append([]any{c.Name}, args...)...)
	}
	files, err := Prepare(c)
	if err != nil {
		return fail(res, err)
	}
	if opts.DryRun {
		res.Outcome = OutcomeDryRun
		for path, data := range files {
			res.Files = append(res.Files, fmt.Sprintf("%s (%d bytes)", path, len(data)))
		}
		sort.Strings(res.Files)
		if len(c.Missing) > 0 {
			res.Reason = "not configured; missing " + strings.Join(c.Missing, ", ")
		} else {
			res.Reason = "configured; would push " + res.Branch + " to " + c.P.Remote().URL
		}
		return res
	}
	if len(c.Missing) > 0 || c.P == nil {
		res.Outcome = OutcomeSkipped
		res.Reason = "missing " + strings.Join(c.Missing, ", ")
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	p := c.P
	logf("setting up")
	if err := p.Setup(ctx); err != nil {
		return fail(res, fmt.Errorf("setup: %w", err))
	}
	logf("pushing %s", res.Branch)
	commit, err := push.Push(ctx, p.Remote(), res.Branch, files)
	// Delete the branch whatever happens next, even after a failed push
	// that may have created it, with a context that survives the timeout.
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer ccancel()
		if err := push.Delete(cctx, p.Remote(), res.Branch); err != nil {
			logf("could not delete %s: %v", res.Branch, err)
		} else {
			logf("deleted %s", res.Branch)
		}
	}()
	if err != nil {
		return fail(res, fmt.Errorf("push: %w", err))
	}
	run, err := wait(ctx, p, res.Branch, commit, opts, logf)
	res.Run = run
	if err != nil {
		cancelRun(ctx, p, run, logf)
		return fail(res, err)
	}
	jobs, err := p.Jobs(ctx, run)
	if err != nil {
		return fail(res, fmt.Errorf("jobs: %w", err))
	}
	res.Checks, res.Extra = Evaluate(jobs)
	res.Outcome = OutcomePassed
	for _, ch := range res.Checks {
		if !ch.OK {
			res.Outcome = OutcomeFailed
			res.Reason = "conformance checks failed"
		}
	}
	if res.Outcome == OutcomePassed && run.Status != StatusSuccess {
		res.Outcome = OutcomeFailed
		res.Reason = "the run finished as " + run.Raw
	}
	return res
}

// cancelRun stops a run that has not finished, with a context that
// survives the timeout. Providers that cannot cancel leave the run going.
func cancelRun(ctx context.Context, p Provider, run *Run, logf func(string, ...any)) {
	c, ok := p.(Canceler)
	if !ok || run == nil || run.Status.Final() {
		return
	}
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer ccancel()
	if err := c.Cancel(cctx, run); err != nil {
		logf("could not cancel run %s: %v", run.ID, err)
	} else {
		logf("canceled run %s", run.ID)
	}
}

func fail(res Result, err error) Result {
	res.Outcome = OutcomeFailed
	res.Reason = err.Error()
	return res
}

// wait polls until the run for commit exists and has finished.
func wait(ctx context.Context, p Provider, branch, commit string, opts Options, logf func(string, ...any)) (*Run, error) {
	start := time.Now()
	delay := opts.PollMin
	var run *Run
	triggered := false
	for {
		var err error
		if run == nil {
			run, err = p.Find(ctx, branch, commit)
			if err == nil && run == nil {
				if t, ok := p.(Trigger); ok && !triggered && time.Since(start) >= opts.TriggerAfter {
					logf("no run after %s; starting one", opts.TriggerAfter)
					triggered = true
					run, err = t.Start(ctx, branch)
				}
			}
			if run != nil {
				logf("run %s: %s", run.ID, run.URL)
			}
		} else {
			run, err = p.Refresh(ctx, run)
		}
		if err != nil && ctx.Err() == nil {
			return run, err
		}
		if err == nil && run != nil && run.Status.Final() {
			return run, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return run, errors.New("interrupted")
			}
			if run == nil {
				return nil, fmt.Errorf("no run started for %s within %s", branch, opts.Timeout)
			}
			return run, fmt.Errorf("run %s did not finish within %s (last state %s)", run.ID, opts.Timeout, run.Raw)
		case <-time.After(delay):
		}
		delay = delay * 3 / 2
		if delay > opts.PollMax {
			delay = opts.PollMax
		}
	}
}

// Evaluate compares the jobs a provider reported with conformance.Jobs.
// Jobs the pipeline does not define are returned as extra.
func Evaluate(jobs []JobResult) (checks []Check, extra []JobResult) {
	used := make([]bool, len(jobs))
	for _, want := range conformance.Jobs {
		ch := Check{Job: want}
		for i, j := range jobs {
			if want.Match(j.Name) {
				ch.Got = append(ch.Got, j)
				used[i] = true
			}
		}
		ch.OK, ch.Note = judge(want, ch.Got)
		checks = append(checks, ch)
	}
	for i, j := range jobs {
		if !used[i] {
			extra = append(extra, j)
		}
	}
	return checks, extra
}

func judge(want conformance.Job, got []JobResult) (bool, string) {
	if want.Skipped {
		for _, g := range got {
			if g.Status != StatusSkipped {
				return false, fmt.Sprintf("must be skipped, %s was %s", g.Name, g.Raw)
			}
		}
		if len(got) == 0 {
			return true, "not scheduled"
		}
		return true, "skipped"
	}
	legs := want.Legs
	if legs == 0 {
		legs = 1
	}
	if len(got) != legs {
		return false, fmt.Sprintf("expected %d job(s), got %d", legs, len(got))
	}
	for _, g := range got {
		if g.Status != StatusSuccess {
			return false, fmt.Sprintf("%s was %s", g.Name, g.Raw)
		}
	}
	if legs > 1 {
		return true, fmt.Sprintf("%d legs passed", legs)
	}
	return true, "passed"
}

// Failed reports whether any result failed.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}

// errNotFound is returned by API calls answered with 404.
var errNotFound = errors.New("not found")
