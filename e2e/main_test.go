package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noEnv is an environment without any provider configured.
func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUnconfiguredProvidersAreSkipped(t *testing.T) {
	summary := filepath.Join(t.TempDir(), "summary.md")
	var stdout, stderr strings.Builder
	code := run([]string{"-summary", summary}, noEnv, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, p := range []string{"gitlab", "circleci", "azure", "bitbucket"} {
		if !strings.Contains(stdout.String(), "| "+p+" | ⏭️ skipped |") {
			t.Errorf("%s not reported as skipped:\n%s", p, stdout.String())
		}
	}
	data, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ci-shift live provider tests") {
		t.Errorf("step summary:\n%s", data)
	}
}

func TestDryRun(t *testing.T) {
	out := t.TempDir()
	var stdout, stderr strings.Builder
	env := envOf(map[string]string{"E2E_GITLAB_PROJECT": "me/e2e", "E2E_GITLAB_TOKEN": "glpat-x", "E2E_PROVIDERS": "gitlab,azure"})
	code := run([]string{"-dry-run", "-out", out, "-run-id", "42"}, env, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"| gitlab | 📝 dry-run |",
		"would push e2e/42-gitlab to https://gitlab.com/me/e2e.git",
		"| azure | 📝 dry-run |",
		"missing E2E_AZURE_ORG",
		"azure-pipelines.yml (",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "glpat-x") {
		t.Error("the summary leaks the token")
	}
	for _, f := range []string{"gitlab/.gitlab-ci.yml", "gitlab/e2e/conformance/ci.yml", "azure/azure-pipelines.yml"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
}

func TestMasksSecretsInActions(t *testing.T) {
	var stdout, stderr strings.Builder
	env := envOf(map[string]string{"GITHUB_ACTIONS": "true", "E2E_PROVIDERS": "circleci",
		"E2E_CIRCLECI_REPO": "me/e2e", "E2E_CIRCLECI_GH_TOKEN": "ghtok", "E2E_CIRCLECI_TOKEN": "cctok"})
	if code := run([]string{"-dry-run"}, env, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, s := range []string{"::add-mask::ghtok", "::add-mask::cctok"} {
		if !strings.Contains(stdout.String(), s) {
			t.Errorf("missing %s", s)
		}
	}
}

func TestBadArguments(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"-providers", "jenkins"}, noEnv, &stdout, &stderr); code != 2 {
		t.Errorf("unknown provider: exit %d", code)
	}
	if code := run([]string{"extra"}, noEnv, &stdout, &stderr); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
}
