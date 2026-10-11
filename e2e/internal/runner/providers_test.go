package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/viniciusdc/ci-shift/e2e/conformance"
	"github.com/viniciusdc/ci-shift/schema"
)

// fakeAPI serves canned JSON by "METHOD path" (path without the query)
// and records every request.
type fakeAPI struct {
	t      *testing.T
	auth   func(*http.Request) bool
	routes map[string]func(w http.ResponseWriter, r *http.Request, body string)

	mu   sync.Mutex
	seen []string
}

func newFakeAPI(t *testing.T, auth func(*http.Request) bool) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, auth: auth, routes: map[string]func(http.ResponseWriter, *http.Request, string){}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.EscapedPath()
	f.mu.Lock()
	f.seen = append(f.seen, key)
	f.mu.Unlock()
	if !f.auth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h, ok := f.routes[key]
	if !ok {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
		return
	}
	h(w, r, string(body))
}

func (f *fakeAPI) json(key string, v any) {
	f.routes[key] = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *fakeAPI) saw(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.seen {
		if s == key {
			return true
		}
	}
	return false
}

func names(jobs []JobResult) string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Name+"="+string(j.Status))
	}
	return strings.Join(out, ",")
}

func TestGitLab(t *testing.T) {
	f, srv := newFakeAPI(t, func(r *http.Request) bool { return r.Header.Get("PRIVATE-TOKEN") == "glpat" })
	const proj = "/api/v4/projects/me%2Fe2e"
	var created string
	f.routes["POST "+proj+"/variables"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		created = body
		w.WriteHeader(http.StatusCreated)
	}
	f.routes["GET "+proj+"/pipelines"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Query().Get("ref") != "e2e/1-gitlab" || r.URL.Query().Get("sha") != "abc" {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_, _ = io.WriteString(w, `[{"id":5,"status":"running","sha":"abc","web_url":"https://gl/p/5"}]`)
	}
	f.json("GET "+proj+"/pipelines/5", map[string]any{"id": 5, "status": "failed", "web_url": "https://gl/p/5"})
	f.json("GET "+proj+"/pipelines/5/jobs", []map[string]any{
		{"name": "checkout-repo", "status": "success"},
		{"name": "matrix-legs: [red]", "status": "failed"},
		{"name": "never-runs", "status": "skipped"},
		{"name": "cache-roundtrip", "status": "manual"},
	})

	g, miss := newGitLab(func(k string) string {
		return map[string]string{"E2E_GITLAB_PROJECT": "me/e2e", "E2E_GITLAB_TOKEN": "glpat", "E2E_GITLAB_URL": srv.URL}[k]
	})
	if len(miss) != 0 {
		t.Fatalf("missing %v", miss)
	}
	g.Secret = "0123456789abcdef"
	ctx := context.Background()
	if err := g.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.saw("PUT "+proj+"/variables/E2E_SECRET") || !strings.Contains(created, `"key":"E2E_SECRET"`) || !strings.Contains(created, `"protected":false`) {
		t.Errorf("variable not upserted: %v %s", f.seen, created)
	}
	if r := g.Remote(); r.URL != srv.URL+"/me/e2e.git" || r.Password != "glpat" {
		t.Errorf("remote %+v", r)
	}
	run, err := g.Find(ctx, "e2e/1-gitlab", "abc")
	if err != nil || run == nil || run.Status != StatusRunning || run.URL != "https://gl/p/5" {
		t.Fatalf("find: %+v %v", run, err)
	}
	if other, _ := g.Find(ctx, "e2e/1-gitlab", "zzz"); other != nil {
		t.Errorf("a pipeline for another commit was returned: %+v", other)
	}
	run, err = g.Refresh(ctx, run)
	if err != nil || run.Status != StatusFailed {
		t.Fatalf("refresh: %+v %v", run, err)
	}
	jobs, err := g.Jobs(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(jobs), "checkout-repo=success,matrix-legs: [red]=failed,never-runs=skipped,cache-roundtrip=failed"; got != want {
		t.Errorf("jobs\n got %s\nwant %s", got, want)
	}
}

func TestGitLabStatus(t *testing.T) {
	for in, want := range map[string]Status{
		"created": StatusPending, "waiting_for_resource": StatusPending, "preparing": StatusPending,
		"pending": StatusPending, "running": StatusRunning, "canceling": StatusRunning,
		"success": StatusSuccess, "failed": StatusFailed, "canceled": StatusCanceled,
		"skipped": StatusSkipped, "manual": StatusFailed, "scheduled": StatusPending,
	} {
		if got := gitlabStatus(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestBitbucket(t *testing.T) {
	f, srv := newFakeAPI(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer bbtok" })
	const repo = "/repositories/ws/e2e"
	var put string
	f.routes["GET "+repo+"/pipelines_config/variables/"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(w, `{"values":[{"uuid":"{v2}","key":"E2E_SECRET","secured":true}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"values":[{"uuid":"{v1}","key":"OTHER"}],"next":"`+srvURL(r)+repo+`/pipelines_config/variables/?page=2"}`)
	}
	f.routes["PUT "+repo+"/pipelines_config/variables/%7Bv2%7D"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		put = body
	}
	f.routes["GET "+repo+"/pipelines/"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Query().Get("target.branch") != "e2e/1-bitbucket" {
			_, _ = io.WriteString(w, `{"values":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"values":[
			{"uuid":"{old}","build_number":3,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"ref_name":"e2e/1-bitbucket","commit":{"hash":"0000"}}},
			{"uuid":"{p1}","build_number":4,"state":{"name":"PENDING"},"target":{"ref_name":"e2e/1-bitbucket","commit":{"hash":"abc123"}}}]}`)
	}
	f.json("GET "+repo+"/pipelines/%7Bp1%7D", map[string]any{"uuid": "{p1}", "build_number": 4, "state": map[string]any{"name": "COMPLETED", "result": map[string]any{"name": "SUCCESSFUL"}}})
	f.json("GET "+repo+"/pipelines/%7Bp1%7D/steps/", map[string]any{"values": []map[string]any{
		{"name": "checkout-repo", "state": map[string]any{"name": "COMPLETED", "result": map[string]any{"name": "SUCCESSFUL"}}},
		{"name": "matrix-legs (red)", "state": map[string]any{"name": "COMPLETED", "result": map[string]any{"name": "NOT_RUN"}}},
		{"name": "artifact-consume", "state": map[string]any{"name": "COMPLETED", "result": map[string]any{"name": "ERROR"}}},
		{"name": "env-vars", "state": map[string]any{"name": "IN_PROGRESS", "stage": map[string]any{"name": "RUNNING"}}},
	}})

	b, miss := newBitbucket(func(k string) string {
		return map[string]string{"E2E_BITBUCKET_WORKSPACE": "ws", "E2E_BITBUCKET_REPO": "e2e", "E2E_BITBUCKET_TOKEN": "bbtok"}[k]
	})
	if len(miss) != 0 {
		t.Fatalf("missing %v", miss)
	}
	b.APIBase, b.GitBase, b.Secret = srv.URL, "https://bitbucket.example", "s3cret"
	ctx := context.Background()
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(put, `"secured":true`) || !strings.Contains(put, `"value":"s3cret"`) {
		t.Errorf("variable not updated: %q", put)
	}
	if r := b.Remote(); r.Username != "x-token-auth" || r.URL != "https://bitbucket.example/ws/e2e.git" {
		t.Errorf("remote %+v", r)
	}
	run, err := b.Find(ctx, "e2e/1-bitbucket", "abc123")
	if err != nil || run == nil || run.ID != "4" || run.Status != StatusPending {
		t.Fatalf("find: %+v %v", run, err)
	}
	if run.URL != "https://bitbucket.example/ws/e2e/pipelines/results/4" {
		t.Errorf("url %s", run.URL)
	}
	run, err = b.Refresh(ctx, run)
	if err != nil || run.Status != StatusSuccess || run.Raw != "COMPLETED/SUCCESSFUL" {
		t.Fatalf("refresh: %+v %v", run, err)
	}
	jobs, err := b.Jobs(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(jobs), "checkout-repo=success,matrix-legs (red)=skipped,artifact-consume=failed,env-vars=running"; got != want {
		t.Errorf("jobs\n got %s\nwant %s", got, want)
	}
}

func TestBitbucketAPIToken(t *testing.T) {
	b, _ := newBitbucket(func(k string) string {
		return map[string]string{"E2E_BITBUCKET_EMAIL": "me@example.com", "E2E_BITBUCKET_TOKEN": "t"}[k]
	})
	if r := b.Remote(); r.Username != "x-bitbucket-api-token-auth" {
		t.Errorf("remote user %s", r.Username)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	b.client().auth(req)
	if u, p, ok := req.BasicAuth(); !ok || u != "me@example.com" || p != "t" {
		t.Errorf("API tokens use Basic email:token, got %q", req.Header.Get("Authorization"))
	}
}

func TestBitbucketStatus(t *testing.T) {
	res := func(n string) *struct {
		Name string `json:"name"`
	} {
		return &struct {
			Name string `json:"name"`
		}{n}
	}
	stage := func(n string) *struct {
		Name string `json:"name"`
	} {
		return &struct {
			Name string `json:"name"`
		}{n}
	}
	for _, tc := range []struct {
		s    bbState
		want Status
	}{
		{bbState{Name: "PENDING"}, StatusPending},
		{bbState{Name: "READY"}, StatusPending},
		{bbState{Name: "IN_PROGRESS", Stage: stage("RUNNING")}, StatusRunning},
		{bbState{Name: "IN_PROGRESS", Stage: stage("PAUSED")}, StatusFailed},
		{bbState{Name: "COMPLETED", Result: res("SUCCESSFUL")}, StatusSuccess},
		{bbState{Name: "COMPLETED", Result: res("FAILED")}, StatusFailed},
		{bbState{Name: "COMPLETED", Result: res("ERROR")}, StatusFailed},
		{bbState{Name: "COMPLETED", Result: res("EXPIRED")}, StatusFailed},
		{bbState{Name: "COMPLETED", Result: res("STOPPED")}, StatusCanceled},
		{bbState{Name: "COMPLETED", Result: res("NOT_RUN")}, StatusSkipped},
	} {
		if got := bitbucketStatus(tc.s); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.s.raw(), got, tc.want)
		}
	}
}

func TestAzure(t *testing.T) {
	f, srv := newFakeAPI(t, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == "" && p == "pat" && r.URL.Query().Get("api-version") == azureAPIVersion
	})
	const proj = "/org/My%20Project/_apis"
	f.json("GET "+proj+"/pipelines", map[string]any{"value": []map[string]any{{"id": 1, "name": "other"}}})
	f.json("GET "+proj+"/git/repositories/e2e", map[string]any{"id": "repo-guid", "name": "e2e"})
	var createBody, defBody string
	f.routes["POST "+proj+"/pipelines"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		createBody = body
		_, _ = io.WriteString(w, `{"id":12,"name":"ci-shift-e2e"}`)
	}
	f.json("GET "+proj+"/build/definitions/12", map[string]any{"id": 12, "revision": 3, "variables": map[string]any{"KEEP": map[string]any{"value": "x"}}})
	f.routes["PUT "+proj+"/build/definitions/12"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		defBody = body
		_, _ = io.WriteString(w, body)
	}
	f.routes["GET "+proj+"/build/builds"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		q := r.URL.Query()
		if q.Get("definitions") != "12" || q.Get("branchName") != "refs/heads/e2e/1-azure" {
			_, _ = io.WriteString(w, `{"value":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"value":[{"id":77,"status":"inProgress","result":"none","sourceVersion":"abc"}]}`)
	}
	f.json("GET "+proj+"/build/builds/77", map[string]any{"id": 77, "status": "completed", "result": "partiallySucceeded"})
	f.json("GET "+proj+"/build/builds/77/timeline", map[string]any{"records": []map[string]any{
		{"type": "Stage", "name": "__default", "state": "completed", "result": "failed"},
		{"type": "Job", "name": "checkout_repo", "state": "completed", "result": "succeeded"},
		{"type": "Job", "name": "matrix_legs red", "state": "completed", "result": "succeededWithIssues"},
		{"type": "Job", "name": "never_runs", "state": "completed", "result": "skipped"},
		{"type": "Job", "name": "env_vars", "state": "completed", "result": "failed"},
		{"type": "Task", "name": "Checkout", "state": "completed", "result": "succeeded"},
	}})
	var started string
	f.routes["POST "+proj+"/pipelines/12/runs"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		started = body
		_, _ = io.WriteString(w, `{"id":77}`)
	}

	a, miss := newAzure(func(k string) string {
		return map[string]string{"E2E_AZURE_ORG": "org", "E2E_AZURE_PROJECT": "My Project", "E2E_AZURE_REPO": "e2e", "E2E_AZURE_TOKEN": "pat"}[k]
	})
	if len(miss) != 0 {
		t.Fatalf("missing %v", miss)
	}
	a.BaseURL, a.Secret = srv.URL, "s3cret"
	ctx := context.Background()
	if err := a.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createBody, `"id":"repo-guid"`) || !strings.Contains(createBody, `"path":"/azure-pipelines.yml"`) {
		t.Errorf("create body %s", createBody)
	}
	if !strings.Contains(defBody, `"E2E_SECRET":{"allowOverride":false,"isSecret":true,"value":"s3cret"}`) || !strings.Contains(defBody, `"KEEP"`) || !strings.Contains(defBody, `"revision":3`) {
		t.Errorf("definition update %s", defBody)
	}
	if r := a.Remote(); r.URL != srv.URL+"/org/My%20Project/_git/e2e" {
		t.Errorf("remote %+v", r)
	}
	run, err := a.Find(ctx, "e2e/1-azure", "abc")
	if err != nil || run == nil || run.ID != "77" || run.Status != StatusRunning {
		t.Fatalf("find: %+v %v", run, err)
	}
	if run.URL != srv.URL+"/org/My%20Project/_build/results?buildId=77" {
		t.Errorf("url %s", run.URL)
	}
	run, err = a.Refresh(ctx, run)
	if err != nil || run.Status != StatusFailed || run.Raw != "partiallySucceeded" {
		t.Fatalf("refresh: %+v %v", run, err)
	}
	jobs, err := a.Jobs(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(jobs), "checkout_repo=success,matrix_legs red=success,never_runs=skipped,env_vars=failed"; got != want {
		t.Errorf("jobs\n got %s\nwant %s", got, want)
	}
	if run, err := a.Start(ctx, "e2e/1-azure"); err != nil || run.ID != "77" || !strings.Contains(started, `"refName":"refs/heads/e2e/1-azure"`) {
		t.Errorf("start: %+v %v %s", run, err, started)
	}
}

func TestAzureExistingPipeline(t *testing.T) {
	f, srv := newFakeAPI(t, func(*http.Request) bool { return true })
	f.json("GET /o/p/_apis/pipelines", map[string]any{"value": []map[string]any{{"id": 4, "name": "ci-shift-e2e"}}})
	f.json("GET /o/p/_apis/build/definitions/4", map[string]any{"id": 4})
	f.json("PUT /o/p/_apis/build/definitions/4", map[string]any{"id": 4})
	a := &Azure{BaseURL: srv.URL, Org: "o", Project: "p", Repo: "r", Pipeline: "ci-shift-e2e", Token: "t"}
	if err := a.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.saw("POST /o/p/_apis/pipelines") {
		t.Error("an existing pipeline must not be recreated")
	}
}

func TestAzureEntraToken(t *testing.T) {
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"
	f, srv := newFakeAPI(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+jwt })
	f.json("GET /o/p/_apis/build/builds/7", map[string]any{"id": 7, "status": "completed", "result": "succeeded"})
	a := &Azure{BaseURL: srv.URL, Org: "o", Project: "p", Repo: "r", Token: jwt}
	if _, err := a.Refresh(context.Background(), &Run{ID: "7"}); err != nil {
		t.Fatalf("an Entra token must be sent as a bearer token: %v", err)
	}
	if r := a.Remote(); !r.Bearer || authHeader(r) != "Authorization: Bearer "+jwt {
		t.Errorf("git must use the bearer token too, got %+v", r)
	}
	if r := (&Azure{Token: "abc123pat"}).Remote(); r.Bearer || !strings.HasPrefix(authHeader(r), "Authorization: Basic ") {
		t.Errorf("a PAT must stay Basic, got %+v", r)
	}
}

func TestAzureCancel(t *testing.T) {
	f, srv := newFakeAPI(t, func(*http.Request) bool { return true })
	var body string
	f.routes["PATCH /o/p/_apis/build/builds/7"] = func(w http.ResponseWriter, _ *http.Request, b string) {
		body = b
		_, _ = w.Write([]byte(`{}`))
	}
	a := &Azure{BaseURL: srv.URL, Org: "o", Project: "p", Token: "t"}
	if err := a.Cancel(context.Background(), &Run{ID: "7"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"status":"cancelling"`) {
		t.Errorf("cancel body %s", body)
	}
}

func TestAzurePool(t *testing.T) {
	conv, err := conformance.Convert("azure")
	if err != nil {
		t.Fatal(err)
	}
	files := Files(conv)
	hosted := strings.Count(string(conv.Output), "vmImage:")
	if hosted == 0 {
		t.Fatal("the converted pipeline sets no vmImage; the test needs updating")
	}

	if err := (&Azure{}).Adapt(files); err != nil || string(files[conv.Path]) != string(conv.Output) {
		t.Fatalf("without a pool the pipeline must be unchanged (err %v)", err)
	}

	if err := (&Azure{Pool: "e2e-pool"}).Adapt(files); err != nil {
		t.Fatal(err)
	}
	out := string(files[conv.Path])
	if strings.Contains(out, "vmImage:") || strings.Count(out, "name: e2e-pool") != hosted {
		t.Errorf("want all %d pools on e2e-pool, got:\n%s", hosted, out)
	}
	if !strings.HasPrefix(out, "# Converted by ci-shift") {
		t.Error("the header comment was lost")
	}
	issues, err := schema.Validate("azure", files[conv.Path], schema.Options{Embedded: true})
	if err != nil || len(issues) > 0 {
		t.Errorf("adapted pipeline fails the schema: %v %v", err, issues)
	}

	if _, err := setPool([]byte("steps:\n  - script: echo\n"), "x"); err == nil {
		t.Error("a pipeline without a pool must be an error")
	}
}

func TestCircleCI(t *testing.T) {
	f, srv := newFakeAPI(t, func(r *http.Request) bool { return r.Header.Get("Circle-Token") == "cctok" })
	const slug = "/project/gh/me/e2e"
	f.routes["DELETE "+slug+"/envvar/E2E_SECRET"] = func(w http.ResponseWriter, _ *http.Request, _ string) {
		http.Error(w, `{"message":"Environment variable not found."}`, http.StatusNotFound)
	}
	var envvar string
	f.routes["POST "+slug+"/envvar"] = func(w http.ResponseWriter, _ *http.Request, body string) {
		envvar = body
		w.WriteHeader(http.StatusCreated)
	}
	f.routes["GET "+slug+"/pipeline"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Query().Get("branch") != "e2e/1-circleci" {
			_, _ = io.WriteString(w, `{"items":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"p-1","number":8,"state":"created","vcs":{"revision":"abc","branch":"e2e/1-circleci"}}]}`)
	}
	wfCalls := 0
	f.routes["GET /pipeline/p-1/workflow"] = func(w http.ResponseWriter, r *http.Request, _ string) {
		wfCalls++
		if r.URL.Query().Get("page-token") == "" {
			status := "running"
			if wfCalls > 1 {
				status = "success"
			}
			_, _ = io.WriteString(w, `{"items":[{"id":"w-1","name":"ci","status":"`+status+`"}],"next_page_token":"t2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"w-2","name":"extra","status":"failed"}],"next_page_token":null}`)
	}
	f.json("GET /pipeline/p-1", map[string]any{"id": "p-1", "number": 8, "state": "created"})
	f.json("GET /workflow/w-1/job", map[string]any{"items": []map[string]any{
		{"name": "checkout-repo", "status": "success"},
		{"name": "matrix-legs-red", "status": "infrastructure_fail"},
		{"name": "artifact-consume", "status": "blocked"},
	}})
	f.json("GET /workflow/w-2/job", map[string]any{"items": []map[string]any{{"name": "on-test-branch", "status": "canceled"}}})

	c, miss := newCircleCI(func(k string) string {
		return map[string]string{"E2E_CIRCLECI_REPO": "me/e2e", "E2E_CIRCLECI_SLUG": "gh/me/e2e", "E2E_CIRCLECI_GH_TOKEN": "ghp", "E2E_CIRCLECI_TOKEN": "cctok"}[k]
	})
	if len(miss) != 0 {
		t.Fatalf("missing %v", miss)
	}
	c.APIBase, c.Secret = srv.URL, "s3cret"
	ctx := context.Background()
	if err := c.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if envvar != `{"name":"E2E_SECRET","value":"s3cret"}` {
		t.Errorf("envvar %s", envvar)
	}
	if r := c.Remote(); r.URL != "https://github.com/me/e2e.git" || r.Password != "ghp" {
		t.Errorf("remote %+v", r)
	}
	run, err := c.Find(ctx, "e2e/1-circleci", "abc")
	if err != nil || run == nil || run.Status != StatusRunning || run.URL != "https://app.circleci.com/pipelines/github/me/e2e/8" {
		t.Fatalf("find: %+v %v", run, err)
	}
	run, err = c.Refresh(ctx, run)
	if err != nil || run.Status != StatusFailed || run.Raw != "ci: success, extra: failed" {
		t.Fatalf("refresh: %+v %v", run, err)
	}
	jobs, err := c.Jobs(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(jobs), "checkout-repo=success,matrix-legs-red=failed,artifact-consume=skipped,on-test-branch=canceled"; got != want {
		t.Errorf("jobs\n got %s\nwant %s", got, want)
	}
}

func TestCircleCIErroredPipeline(t *testing.T) {
	f, srv := newFakeAPI(t, func(*http.Request) bool { return true })
	f.json("GET /project/circleci/org-id/proj-id/pipeline", map[string]any{"items": []map[string]any{
		{"id": "p-2", "number": 2, "state": "errored", "vcs": map[string]any{"revision": "abc"}, "errors": []map[string]any{{"type": "config", "message": "bad config"}}},
	}})
	c := &CircleCI{APIBase: srv.URL, Slug: "circleci/org-id/proj-id", Token: "t"}
	run, err := c.Find(context.Background(), "b", "abc")
	if err != nil || run.Status != StatusFailed || !strings.Contains(run.Raw, "bad config") {
		t.Fatalf("got %+v %v", run, err)
	}
	if run.URL != "https://app.circleci.com/pipelines/circleci/org-id/proj-id/2" {
		t.Errorf("url %s", run.URL)
	}
}

func TestHTTPErrorsDoNotLeakCredentials(t *testing.T) {
	_, srv := newFakeAPI(t, func(*http.Request) bool { return false })
	g := &GitLab{BaseURL: srv.URL, Project: "me/e2e", Token: "glpat-supersecret"}
	_, err := g.Find(context.Background(), "b", "c")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("error leaks the token: %v", err)
	}
}

// srvURL is the scheme and host the request was sent to.
func srvURL(r *http.Request) string { return "http://" + r.Host }
