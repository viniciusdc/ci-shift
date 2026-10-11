package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// azureAPIVersion is the Azure DevOps REST API version used throughout.
const azureAPIVersion = "7.1"

// Azure runs the pipeline in an Azure Repos repository, authenticated with
// a personal access token. The YAML pipeline definition is looked up by
// name and created when missing. With Pool set, every job runs on that
// self-hosted agent pool instead of the Microsoft-hosted one the
// conversion picked.
type Azure struct {
	BaseURL  string // https://dev.azure.com
	Org      string
	Project  string
	Repo     string
	Pipeline string // definition name
	Pool     string // optional self-hosted agent pool
	Token    string
	Secret   string // value given to the E2E_SECRET pipeline variable

	api        *api
	pipelineID int
}

func newAzure(env func(string) string) (*Azure, []string) {
	a := &Azure{
		BaseURL:  "https://dev.azure.com",
		Org:      env("E2E_AZURE_ORG"),
		Project:  env("E2E_AZURE_PROJECT"),
		Repo:     env("E2E_AZURE_REPO"),
		Pipeline: or(env("E2E_AZURE_PIPELINE"), "ci-shift-e2e"),
		Pool:     strings.TrimSpace(env("E2E_AZURE_POOL")),
		Token:    env("E2E_AZURE_TOKEN"),
	}
	return a, missing(map[string]string{
		"E2E_AZURE_ORG":     a.Org,
		"E2E_AZURE_PROJECT": a.Project,
		"E2E_AZURE_REPO":    a.Repo,
		"E2E_AZURE_TOKEN":   a.Token,
	})
}

// Name implements Provider.
func (a *Azure) Name() string { return "azure" }

func (a *Azure) projectURL() string {
	return strings.TrimRight(a.BaseURL, "/") + "/" + url.PathEscape(a.Org) + "/" + url.PathEscape(a.Project)
}

// entraToken reports whether Token is a Microsoft Entra access token (a
// JWT, as the workflow gets through OIDC) rather than a personal access
// token. Azure DevOps takes the first as a bearer token and the second as
// Basic credentials.
func (a *Azure) entraToken() bool {
	return strings.HasPrefix(a.Token, "eyJ") && strings.Count(a.Token, ".") == 2
}

func (a *Azure) client() *api {
	if a.api == nil {
		auth := basicAuth("", a.Token)
		if a.entraToken() {
			auth = bearerAuth(a.Token)
		}
		a.api = newAPI(a.projectURL()+"/_apis", auth)
	}
	return a.api
}

// path appends the api-version to an _apis path.
func (a *Azure) path(p string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", azureAPIVersion)
	return p + "?" + q.Encode()
}

// Remote implements Provider. Azure Repos accepts any user name with a
// PAT, and an Entra token as a bearer token.
func (a *Azure) Remote() Remote {
	return Remote{URL: a.projectURL() + "/_git/" + url.PathEscape(a.Repo), Username: "pat", Password: a.Token, Bearer: a.entraToken()}
}

// Adapt implements Adapter: with Pool set, it points every pool of the
// converted pipeline at that self-hosted pool.
func (a *Azure) Adapt(files map[string][]byte) error {
	if a.Pool == "" {
		return nil
	}
	const path = "azure-pipelines.yml"
	data, ok := files[path]
	if !ok {
		return fmt.Errorf("no %s to adapt", path)
	}
	out, err := setPool(data, a.Pool)
	if err != nil {
		return err
	}
	files[path] = out
	return nil
}

// setPool replaces every pool of an Azure pipeline (top level, stages,
// jobs) with the named pool, keeping the rest of the document.
func setPool(src []byte, pool string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	replaced := 0
	var walk func(m *yaml.Node)
	walk = func(m *yaml.Node) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			key, val := m.Content[i], m.Content[i+1]
			switch key.Value {
			case "pool":
				m.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
					{Kind: yaml.ScalarNode, Tag: "!!str", Value: pool},
				}}
				replaced++
			case "stages", "jobs":
				if val.Kind != yaml.SequenceNode {
					continue
				}
				for _, item := range val.Content {
					if item.Kind == yaml.MappingNode {
						walk(item)
					}
				}
			}
		}
	}
	walk(doc.Content[0])
	if replaced == 0 {
		return nil, errors.New("the pipeline sets no pool to replace")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Setup implements Provider: it finds (or creates) the pipeline definition
// and stores E2E_SECRET in it as a secret variable.
func (a *Azure) Setup(ctx context.Context) error {
	id, err := a.findPipeline(ctx)
	if err != nil {
		return err
	}
	if id == 0 {
		if id, err = a.createPipeline(ctx); err != nil {
			return fmt.Errorf("create pipeline %q: %w (create it once by hand; see e2e/README.md)", a.Pipeline, err)
		}
	}
	a.pipelineID = id
	return a.setSecret(ctx)
}

func (a *Azure) findPipeline(ctx context.Context) (int, error) {
	q := url.Values{"$top": {"1000"}}
	var list struct {
		Value []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := a.client().do(ctx, http.MethodGet, a.path("/pipelines", q), nil, &list); err != nil {
		return 0, err
	}
	for _, p := range list.Value {
		if p.Name == a.Pipeline {
			return p.ID, nil
		}
	}
	return 0, nil
}

// createPipeline registers azure-pipelines.yml of the repository as a YAML
// pipeline. Azure reads the file from the default branch at this point,
// so it must exist there.
func (a *Azure) createPipeline(ctx context.Context) (int, error) {
	var repo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := a.client().do(ctx, http.MethodGet, a.path("/git/repositories/"+url.PathEscape(a.Repo), nil), nil, &repo); err != nil {
		return 0, err
	}
	body := map[string]any{
		"name": a.Pipeline,
		"configuration": map[string]any{
			"type": "yaml",
			"path": "/azure-pipelines.yml",
			"repository": map[string]any{
				"id":   repo.ID,
				"name": repo.Name,
				"type": "azureReposGit",
			},
		},
	}
	var created struct {
		ID int `json:"id"`
	}
	if err := a.client().do(ctx, http.MethodPost, a.path("/pipelines", nil), body, &created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

// setSecret updates the build definition behind the pipeline, keeping
// every field it does not know about.
func (a *Azure) setSecret(ctx context.Context) error {
	defPath := fmt.Sprintf("/build/definitions/%d", a.pipelineID)
	var def map[string]any
	if err := a.client().do(ctx, http.MethodGet, a.path(defPath, nil), nil, &def); err != nil {
		return err
	}
	vars, _ := def["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	vars["E2E_SECRET"] = map[string]any{"value": a.Secret, "isSecret": true, "allowOverride": false}
	def["variables"] = vars
	return a.client().do(ctx, http.MethodPut, a.path(defPath, nil), def, nil)
}

type azBuild struct {
	ID            int    `json:"id"`
	BuildNumber   string `json:"buildNumber"`
	Status        string `json:"status"`
	Result        string `json:"result"`
	SourceBranch  string `json:"sourceBranch"`
	SourceVersion string `json:"sourceVersion"`
}

func (a *Azure) run(b azBuild) *Run {
	raw := b.Status
	if b.Status == "completed" {
		raw = b.Result
	}
	return &Run{
		ID:     fmt.Sprint(b.ID),
		URL:    fmt.Sprintf("%s/_build/results?buildId=%d", a.projectURL(), b.ID),
		Status: azureBuildStatus(b.Status, b.Result),
		Raw:    raw,
	}
}

// Find implements Provider: the build the push triggered for commit.
func (a *Azure) Find(ctx context.Context, branch, commit string) (*Run, error) {
	q := url.Values{
		"definitions": {fmt.Sprint(a.pipelineID)},
		"branchName":  {"refs/heads/" + branch},
		"queryOrder":  {"queueTimeDescending"},
		"$top":        {"20"},
	}
	var list struct {
		Value []azBuild `json:"value"`
	}
	if err := a.client().do(ctx, http.MethodGet, a.path("/build/builds", q), nil, &list); err != nil {
		return nil, err
	}
	for _, b := range list.Value {
		if b.SourceVersion == commit || commit == "" {
			return a.run(b), nil
		}
	}
	return nil, nil
}

// Start implements Trigger: it queues a run when the push trigger did not
// fire (for example when the organization disabled implied YAML triggers).
func (a *Azure) Start(ctx context.Context, branch string) (*Run, error) {
	body := map[string]any{"resources": map[string]any{"repositories": map[string]any{
		"self": map[string]any{"refName": "refs/heads/" + branch},
	}}}
	var run struct {
		ID int `json:"id"`
	}
	if err := a.client().do(ctx, http.MethodPost, a.path(fmt.Sprintf("/pipelines/%d/runs", a.pipelineID), nil), body, &run); err != nil {
		return nil, err
	}
	// A pipeline run is a build with the same id.
	return a.Refresh(ctx, &Run{ID: fmt.Sprint(run.ID)})
}

// Refresh implements Provider.
func (a *Azure) Refresh(ctx context.Context, run *Run) (*Run, error) {
	var b azBuild
	if err := a.client().do(ctx, http.MethodGet, a.path("/build/builds/"+run.ID, nil), nil, &b); err != nil {
		return run, err
	}
	return a.run(b), nil
}

// Cancel implements Canceler.
func (a *Azure) Cancel(ctx context.Context, run *Run) error {
	body := map[string]any{"status": "cancelling"}
	return a.client().do(ctx, http.MethodPatch, a.path("/build/builds/"+run.ID, nil), body, nil)
}

// Jobs implements Provider: the Job records of the build timeline. Matrix
// legs are separate records named "<job> <leg>".
func (a *Azure) Jobs(ctx context.Context, run *Run) ([]JobResult, error) {
	var tl struct {
		Records []struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			State  string `json:"state"`
			Result string `json:"result"`
		} `json:"records"`
	}
	if err := a.client().do(ctx, http.MethodGet, a.path("/build/builds/"+run.ID+"/timeline", nil), nil, &tl); err != nil {
		return nil, err
	}
	var out []JobResult
	for _, r := range tl.Records {
		if r.Type != "Job" {
			continue
		}
		raw := r.State
		if r.State == "completed" {
			raw = r.Result
		}
		out = append(out, JobResult{Name: r.Name, Status: azureRecordStatus(r.State, r.Result), Raw: raw})
	}
	return out, nil
}

func azureBuildStatus(status, result string) Status {
	switch status {
	case "completed":
		switch result {
		case "succeeded":
			return StatusSuccess
		case "canceled":
			return StatusCanceled
		}
		return StatusFailed // partiallySucceeded, failed, none
	case "inProgress", "cancelling":
		return StatusRunning
	}
	return StatusPending // notStarted, postponed, none
}

func azureRecordStatus(state, result string) Status {
	switch state {
	case "completed":
		switch result {
		case "succeeded", "succeededWithIssues":
			return StatusSuccess
		case "skipped":
			return StatusSkipped
		case "canceled":
			return StatusCanceled
		}
		return StatusFailed // failed, abandoned
	case "inProgress":
		return StatusRunning
	}
	return StatusPending
}
