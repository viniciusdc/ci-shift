package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CircleCI runs the pipeline for a GitHub repository followed in CircleCI:
// the branch is pushed to GitHub with a fine-grained token and the run is
// read from the CircleCI v2 API.
type CircleCI struct {
	APIBase     string // https://circleci.com/api/v2
	GitHubBase  string // https://github.com
	Repo        string // owner/name on GitHub
	Slug        string // CircleCI project slug, from Project Settings
	GitHubToken string
	Token       string // CircleCI personal API token
	Secret      string // value given to the E2E_SECRET project variable

	api *api
}

func newCircleCI(env func(string) string) (*CircleCI, []string) {
	c := &CircleCI{
		APIBase:     "https://circleci.com/api/v2",
		GitHubBase:  "https://github.com",
		Repo:        env("E2E_CIRCLECI_REPO"),
		Slug:        env("E2E_CIRCLECI_SLUG"),
		GitHubToken: env("E2E_CIRCLECI_GH_TOKEN"),
		Token:       env("E2E_CIRCLECI_TOKEN"),
	}
	// The slug is not derived from the repository: projects created through
	// the GitHub App get circleci/<org-id>/<project-id>, not gh/<owner>/<name>.
	return c, missing(map[string]string{
		"E2E_CIRCLECI_REPO":     c.Repo,
		"E2E_CIRCLECI_SLUG":     c.Slug,
		"E2E_CIRCLECI_GH_TOKEN": c.GitHubToken,
		"E2E_CIRCLECI_TOKEN":    c.Token,
	})
}

// Name implements Provider.
func (c *CircleCI) Name() string { return "circleci" }

// Secrets returns the credentials besides the git password, for masking.
func (c *CircleCI) Secrets() []string { return []string{c.Token} }

func (c *CircleCI) client() *api {
	if c.api == nil {
		c.api = newAPI(c.APIBase, headerAuth("Circle-Token", c.Token))
	}
	return c.api
}

// slugPath escapes each segment of the project slug but keeps its slashes.
func (c *CircleCI) slugPath() string {
	parts := strings.Split(c.Slug, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/project/" + strings.Join(parts, "/")
}

// Remote implements Provider: the GitHub repository CircleCI builds.
func (c *CircleCI) Remote() Remote {
	return Remote{URL: strings.TrimRight(c.GitHubBase, "/") + "/" + c.Repo + ".git", Username: "x-access-token", Password: c.GitHubToken}
}

// Setup implements Provider: it replaces the E2E_SECRET project variable.
func (c *CircleCI) Setup(ctx context.Context) error {
	err := c.client().do(ctx, http.MethodDelete, c.slugPath()+"/envvar/E2E_SECRET", nil, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return c.client().do(ctx, http.MethodPost, c.slugPath()+"/envvar", map[string]string{"name": "E2E_SECRET", "value": c.Secret}, nil)
}

type ccPipeline struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	State  string `json:"state"`
	VCS    struct {
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	} `json:"vcs"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// webSlug is the slug as the web app spells it (gh → github).
func (c *CircleCI) webSlug() string {
	if rest, ok := strings.CutPrefix(c.Slug, "gh/"); ok {
		return "github/" + rest
	}
	if rest, ok := strings.CutPrefix(c.Slug, "bb/"); ok {
		return "bitbucket/" + rest
	}
	return c.Slug
}

// Find implements Provider.
func (c *CircleCI) Find(ctx context.Context, branch, commit string) (*Run, error) {
	var page struct {
		Items []ccPipeline `json:"items"`
	}
	path := c.slugPath() + "/pipeline?" + url.Values{"branch": {branch}}.Encode()
	if err := c.client().do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	for _, p := range page.Items {
		if p.VCS.Revision == commit || commit == "" {
			return c.status(ctx, p)
		}
	}
	return nil, nil
}

// Refresh implements Provider.
func (c *CircleCI) Refresh(ctx context.Context, run *Run) (*Run, error) {
	var p ccPipeline
	if err := c.client().do(ctx, http.MethodGet, "/pipeline/"+url.PathEscape(run.Ref), nil, &p); err != nil {
		return run, err
	}
	return c.status(ctx, p)
}

type ccWorkflow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// status combines the pipeline and its workflows into one run state.
func (c *CircleCI) status(ctx context.Context, p ccPipeline) (*Run, error) {
	run := &Run{
		ID:  fmt.Sprint(p.Number),
		Ref: p.ID,
		URL: fmt.Sprintf("https://app.circleci.com/pipelines/%s/%d", c.webSlug(), p.Number),
		Raw: p.State,
	}
	if p.State == "errored" {
		run.Status = StatusFailed
		for _, e := range p.Errors {
			run.Raw += "; " + e.Message
		}
		return run, nil
	}
	if p.State != "created" {
		run.Status = StatusPending
		return run, nil
	}
	wfs, err := c.workflows(ctx, p.ID)
	if err != nil {
		return run, err
	}
	if len(wfs) == 0 {
		run.Status = StatusPending
		return run, nil
	}
	run.Status = StatusSuccess
	var raw []string
	for _, w := range wfs {
		raw = append(raw, w.Name+": "+w.Status)
		switch s := circleWorkflowStatus(w.Status); {
		case !s.Final():
			run.Status = StatusRunning
		case s != StatusSuccess && run.Status == StatusSuccess:
			run.Status = s
		}
	}
	run.Raw = strings.Join(raw, ", ")
	return run, nil
}

func (c *CircleCI) workflows(ctx context.Context, pipelineID string) ([]ccWorkflow, error) {
	var out []ccWorkflow
	token := ""
	for {
		var page struct {
			Items         []ccWorkflow `json:"items"`
			NextPageToken string       `json:"next_page_token"`
		}
		path := "/pipeline/" + url.PathEscape(pipelineID) + "/workflow"
		if token != "" {
			path += "?page-token=" + url.QueryEscape(token)
		}
		if err := c.client().do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if token = page.NextPageToken; token == "" {
			return out, nil
		}
	}
}

// Jobs implements Provider: the jobs of every workflow of the pipeline.
func (c *CircleCI) Jobs(ctx context.Context, run *Run) ([]JobResult, error) {
	wfs, err := c.workflows(ctx, run.Ref)
	if err != nil {
		return nil, err
	}
	var out []JobResult
	for _, w := range wfs {
		token := ""
		for {
			var page struct {
				Items []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"items"`
				NextPageToken string `json:"next_page_token"`
			}
			path := "/workflow/" + url.PathEscape(w.ID) + "/job"
			if token != "" {
				path += "?page-token=" + url.QueryEscape(token)
			}
			if err := c.client().do(ctx, http.MethodGet, path, nil, &page); err != nil {
				return nil, err
			}
			for _, j := range page.Items {
				out = append(out, JobResult{Name: j.Name, Status: circleJobStatus(j.Status), Raw: j.Status})
			}
			if token = page.NextPageToken; token == "" {
				break
			}
		}
	}
	return out, nil
}

// circleWorkflowStatus maps workflow statuses. on_hold waits for an
// approval, which the conformance pipeline does not have: it counts as
// failed rather than being waited on.
func circleWorkflowStatus(s string) Status {
	switch s {
	case "success":
		return StatusSuccess
	case "failed", "error", "unauthorized", "on_hold":
		return StatusFailed
	case "canceled":
		return StatusCanceled
	case "not_run":
		return StatusSkipped
	}
	return StatusRunning // running, failing
}

func circleJobStatus(s string) Status {
	switch s {
	case "success":
		return StatusSuccess
	case "failed", "infrastructure_fail", "timedout", "terminated-unknown", "unauthorized":
		return StatusFailed
	case "canceled":
		return StatusCanceled
	case "not_run", "blocked":
		// blocked jobs of a finished workflow never started because a job
		// they require failed.
		return StatusSkipped
	case "queued", "not_running":
		return StatusPending
	}
	return StatusRunning // running, retried, on_hold
}
