package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GitLab runs the pipeline in a GitLab project, authenticated with an
// access token that has the api scope.
type GitLab struct {
	BaseURL string // https://gitlab.com
	Project string // namespace/project
	Token   string
	Secret  string // value given to the E2E_SECRET project variable

	api *api
}

func newGitLab(env func(string) string) (*GitLab, []string) {
	g := &GitLab{
		BaseURL: or(env("E2E_GITLAB_URL"), "https://gitlab.com"),
		Project: env("E2E_GITLAB_PROJECT"),
		Token:   env("E2E_GITLAB_TOKEN"),
	}
	return g, missing(map[string]string{"E2E_GITLAB_PROJECT": g.Project, "E2E_GITLAB_TOKEN": g.Token})
}

// Name implements Provider.
func (g *GitLab) Name() string { return "gitlab" }

func (g *GitLab) client() *api {
	if g.api == nil {
		g.api = newAPI(strings.TrimRight(g.BaseURL, "/")+"/api/v4", headerAuth("PRIVATE-TOKEN", g.Token))
	}
	return g.api
}

// projectPath is the URL-encoded project path used as :id.
func (g *GitLab) projectPath() string { return "/projects/" + url.PathEscape(g.Project) }

// Remote implements Provider. GitLab access tokens accept any non-blank
// user name over HTTPS.
func (g *GitLab) Remote() Remote {
	return Remote{URL: strings.TrimRight(g.BaseURL, "/") + "/" + g.Project + ".git", Username: "oauth2", Password: g.Token}
}

// Setup implements Provider: it creates or updates the E2E_SECRET project
// variable, masked and unprotected so that test branches can read it.
func (g *GitLab) Setup(ctx context.Context) error {
	body := map[string]any{"value": g.Secret, "masked": true, "protected": false}
	err := g.client().do(ctx, http.MethodPut, g.projectPath()+"/variables/E2E_SECRET", body, nil)
	if errors.Is(err, errNotFound) {
		body["key"] = "E2E_SECRET"
		err = g.client().do(ctx, http.MethodPost, g.projectPath()+"/variables", body, nil)
	}
	return err
}

type glPipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	SHA    string `json:"sha"`
	WebURL string `json:"web_url"`
}

func (p glPipeline) run() *Run {
	return &Run{ID: fmt.Sprint(p.ID), URL: p.WebURL, Status: gitlabStatus(p.Status), Raw: p.Status}
}

// Find implements Provider.
func (g *GitLab) Find(ctx context.Context, branch, commit string) (*Run, error) {
	q := url.Values{"ref": {branch}, "sha": {commit}, "order_by": {"id"}, "sort": {"desc"}}
	var list []glPipeline
	if err := g.client().do(ctx, http.MethodGet, g.projectPath()+"/pipelines?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[0].run(), nil
}

// Refresh implements Provider.
func (g *GitLab) Refresh(ctx context.Context, run *Run) (*Run, error) {
	var p glPipeline
	if err := g.client().do(ctx, http.MethodGet, g.projectPath()+"/pipelines/"+run.ID, nil, &p); err != nil {
		return run, err
	}
	return p.run(), nil
}

// Jobs implements Provider.
func (g *GitLab) Jobs(ctx context.Context, run *Run) ([]JobResult, error) {
	var out []JobResult
	for page := 1; ; page++ {
		var jobs []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		path := fmt.Sprintf("%s/pipelines/%s/jobs?per_page=100&page=%d", g.projectPath(), run.ID, page)
		if err := g.client().do(ctx, http.MethodGet, path, nil, &jobs); err != nil {
			return nil, err
		}
		for _, j := range jobs {
			out = append(out, JobResult{Name: j.Name, Status: gitlabStatus(j.Status), Raw: j.Status})
		}
		if len(jobs) < 100 {
			return out, nil
		}
	}
}

// gitlabStatus maps pipeline and job statuses. A pipeline waiting on a
// manual job counts as finished: nothing in the conformance pipeline is
// manual, so it would wait forever.
func gitlabStatus(s string) Status {
	switch s {
	case "success":
		return StatusSuccess
	case "failed", "manual":
		return StatusFailed
	case "canceled":
		return StatusCanceled
	case "skipped":
		return StatusSkipped
	case "running", "canceling":
		return StatusRunning
	}
	return StatusPending // created, waiting_for_resource, preparing, pending, scheduled
}
