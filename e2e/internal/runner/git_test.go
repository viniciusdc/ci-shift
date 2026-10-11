package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitPusher(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// A local path ignores the auth header, but the password still flows
	// through the same code, so masking can be checked.
	var masked []string
	g := GitPusher{Mask: func(s string) { masked = append(masked, s) }}
	remote := Remote{URL: bare, Username: "user", Password: "hunter2"}
	ctx := context.Background()

	commit, err := g.Push(ctx, remote, "e2e/1-test", map[string][]byte{
		".gitlab-ci.yml":         []byte("stages: [a]\n"),
		"e2e/conformance/ci.yml": []byte("name: x\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commit) != 40 {
		t.Errorf("commit %q", commit)
	}
	out, err := exec.Command("git", "--git-dir", bare, "ls-tree", "-r", "--name-only", "e2e/1-test").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.Fields(string(out)); strings.Join(got, ",") != ".gitlab-ci.yml,e2e/conformance/ci.yml" {
		t.Errorf("pushed tree %v", got)
	}
	if len(masked) == 0 || masked[0] != "hunter2" {
		t.Errorf("masked %v", masked)
	}

	if err := g.Delete(ctx, remote, "e2e/1-test"); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "--git-dir", bare, "branch", "--list", "e2e/*").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch still exists: %s", out)
	}
}

func TestGitErrorsAreScrubbed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Pushing to a missing local path fails; the URL (which here contains
	// the password on purpose) must be scrubbed from the error.
	missing := filepath.Join(t.TempDir(), "hunter2-missing.git")
	err := GitPusher{}.Delete(context.Background(), Remote{URL: missing, Username: "u", Password: "hunter2"}, "b")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestGitEnv(t *testing.T) {
	env := gitEnv([]string{"PATH=/bin", "GIT_TRACE=1", "GIT_CURL_VERBOSE=1", "GIT_CONFIG_GLOBAL=/home/me/.gitconfig"})
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"GIT_TRACE=1", "GIT_CURL_VERBOSE=1", "/home/me/.gitconfig"} {
		if strings.Contains(joined, bad) {
			t.Errorf("env keeps %s", bad)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("env %v", env)
	}
}
