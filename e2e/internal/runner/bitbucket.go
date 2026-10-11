package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Bitbucket runs the pipeline in a Bitbucket Cloud repository. It accepts
// a repository access token (the default) or, when Email is set, an
// Atlassian API token of that account.
type Bitbucket struct {
	APIBase   string // https://api.bitbucket.org/2.0
	GitBase   string // https://bitbucket.org
	Workspace string
	Repo      string
	Email     string
	Token     string
	Secret    string // value given to the E2E_SECRET repository variable

	api *api
}

func newBitbucket(env func(string) string) (*Bitbucket, []string) {
	b := &Bitbucket{
		APIBase:   "https://api.bitbucket.org/2.0",
		GitBase:   "https://bitbucket.org",
		Workspace: env("E2E_BITBUCKET_WORKSPACE"),
		Repo:      env("E2E_BITBUCKET_REPO"),
		Email:     env("E2E_BITBUCKET_EMAIL"),
		Token:     env("E2E_BITBUCKET_TOKEN"),
	}
	return b, missing(map[string]string{
		"E2E_BITBUCKET_WORKSPACE": b.Workspace,
		"E2E_BITBUCKET_REPO":      b.Repo,
		"E2E_BITBUCKET_TOKEN":     b.Token,
	})
}

// Name implements Provider.
func (b *Bitbucket) Name() string { return "bitbucket" }

func (b *Bitbucket) client() *api {
	if b.api == nil {
		auth := bearerAuth(b.Token)
		if b.Email != "" {
			auth = basicAuth(b.Email, b.Token)
		}
		b.api = newAPI(b.APIBase, auth)
	}
	return b.api
}

func (b *Bitbucket) repoPath() string {
	return "/repositories/" + url.PathEscape(b.Workspace) + "/" + url.PathEscape(b.Repo)
}

// Remote implements Provider. Access tokens use the fixed user name
// x-token-auth; API tokens use x-bitbucket-api-token-auth.
func (b *Bitbucket) Remote() Remote {
	user := "x-token-auth"
	if b.Email != "" {
		user = "x-bitbucket-api-token-auth"
	}
	return Remote{URL: fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(b.GitBase, "/"), b.Workspace, b.Repo), Username: user, Password: b.Token}
}

type bbVariable struct {
	UUID    string `json:"uuid,omitempty"`
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
	Secured bool   `json:"secured"`
}

// Setup implements Provider: it creates or updates E2E_SECRET as a secured
// repository variable.
func (b *Bitbucket) Setup(ctx context.Context) error {
	want := bbVariable{Key: "E2E_SECRET", Value: b.Secret, Secured: true}
	path := b.repoPath() + "/pipelines_config/variables/"
	next := path + "?pagelen=100"
	for next != "" {
		var page struct {
			Values []bbVariable `json:"values"`
			Next   string       `json:"next"`
		}
		if err := b.client().do(ctx, http.MethodGet, next, nil, &page); err != nil {
			return err
		}
		for _, v := range page.Values {
			if v.Key == want.Key {
				return b.client().do(ctx, http.MethodPut, path+url.PathEscape(v.UUID), want, nil)
			}
		}
		next = b.relative(page.Next)
	}
	return b.client().do(ctx, http.MethodPost, path, want, nil)
}

// relative turns an absolute "next" link into a path for the client.
func (b *Bitbucket) relative(link string) string {
	return strings.TrimPrefix(link, strings.TrimRight(b.APIBase, "/"))
}

type bbState struct {
	Name   string `json:"name"`
	Result *struct {
		Name string `json:"name"`
	} `json:"result"`
	Stage *struct {
		Name string `json:"name"`
	} `json:"stage"`
}

// raw renders a state as "COMPLETED/SUCCESSFUL".
func (s bbState) raw() string {
	switch {
	case s.Result != nil:
		return s.Name + "/" + s.Result.Name
	case s.Stage != nil:
		return s.Name + "/" + s.Stage.Name
	}
	return s.Name
}

type bbPipeline struct {
	UUID        string  `json:"uuid"`
	BuildNumber int     `json:"build_number"`
	State       bbState `json:"state"`
	Target      struct {
		RefName string `json:"ref_name"`
		Commit  struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"target"`
}

func (b *Bitbucket) run(p bbPipeline) *Run {
	return &Run{
		ID:     fmt.Sprint(p.BuildNumber),
		Ref:    p.UUID,
		URL:    fmt.Sprintf("%s/%s/%s/pipelines/results/%d", strings.TrimRight(b.GitBase, "/"), b.Workspace, b.Repo, p.BuildNumber),
		Status: bitbucketStatus(p.State),
		Raw:    p.State.raw(),
	}
}

// Find implements Provider.
func (b *Bitbucket) Find(ctx context.Context, branch, commit string) (*Run, error) {
	q := url.Values{"target.branch": {branch}, "sort": {"-created_on"}, "pagelen": {"20"}}
	var page struct {
		Values []bbPipeline `json:"values"`
	}
	if err := b.client().do(ctx, http.MethodGet, b.repoPath()+"/pipelines/?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	for _, p := range page.Values {
		if p.Target.RefName == branch && strings.HasPrefix(p.Target.Commit.Hash, commit[:min(12, len(commit))]) {
			return b.run(p), nil
		}
	}
	return nil, nil
}

// Refresh implements Provider.
func (b *Bitbucket) Refresh(ctx context.Context, run *Run) (*Run, error) {
	var p bbPipeline
	if err := b.client().do(ctx, http.MethodGet, b.repoPath()+"/pipelines/"+url.PathEscape(run.Ref), nil, &p); err != nil {
		return run, err
	}
	return b.run(p), nil
}

// Jobs implements Provider: Bitbucket's jobs are the pipeline's steps.
func (b *Bitbucket) Jobs(ctx context.Context, run *Run) ([]JobResult, error) {
	var out []JobResult
	next := b.repoPath() + "/pipelines/" + url.PathEscape(run.Ref) + "/steps/?pagelen=100"
	for next != "" {
		var page struct {
			Values []struct {
				Name  string  `json:"name"`
				State bbState `json:"state"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if err := b.client().do(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Values {
			out = append(out, JobResult{Name: s.Name, Status: bitbucketStatus(s.State), Raw: s.State.raw()})
		}
		next = b.relative(page.Next)
	}
	return out, nil
}

// bitbucketStatus maps pipeline and step states. A paused pipeline waits
// for a manual step, which the conformance pipeline does not have, so it
// counts as failed rather than being waited on.
func bitbucketStatus(s bbState) Status {
	switch s.Name {
	case "COMPLETED":
		if s.Result == nil {
			return StatusFailed
		}
		switch s.Result.Name {
		case "SUCCESSFUL":
			return StatusSuccess
		case "NOT_RUN":
			return StatusSkipped
		case "STOPPED":
			return StatusCanceled
		}
		return StatusFailed // FAILED, ERROR, EXPIRED
	case "IN_PROGRESS":
		if s.Stage != nil && s.Stage.Name == "PAUSED" {
			return StatusFailed
		}
		return StatusRunning
	}
	return StatusPending // PENDING, READY
}
