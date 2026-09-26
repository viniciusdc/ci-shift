package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidate(t *testing.T) {
	code, out, _ := run(t, "", "validate", "../../testdata/validate-valid-github.yml", "--provider", "github")
	if code != 0 || !strings.Contains(out, "valid GitHub Actions configuration") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	code, out, _ = run(t, "", "validate", "-p", "github", "../../testdata/validate-invalid-github.yml")
	if code != 1 || !strings.Contains(out, "additional properties 'runn' not allowed") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestConvertStdinToFile(t *testing.T) {
	src, err := os.ReadFile("../../testdata/gitlab/python-lib.gitlab-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "wf", "ci.yml")
	code, _, stderr := run(t, string(src), "convert", "-", "--from", "gitlab", "--to", "gh", "-o", dst, "--check")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# Converted by cistela from stdin (GitLab CI/CD) to GitHub Actions.") {
		t.Fatalf("missing header:\n%s", out)
	}
	if !strings.Contains(stderr, "wrote "+dst) {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestConvertStrict(t *testing.T) {
	code, out, stderr := run(t, "", "convert", "../../testdata/github/node-app.yml", "--to", "gitlab", "--strict", "--no-header")
	if code != 1 {
		t.Fatalf("expected strict failure, code=%d", code)
	}
	if !strings.HasPrefix(out, "workflow:") {
		t.Fatalf("output should still be written:\n%s", out)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestDetectAndProviders(t *testing.T) {
	code, out, _ := run(t, "", "detect", "../../testdata/github/node-app.yml", "../../testdata/gitlab/python-lib.gitlab-ci.yml")
	if code != 0 || !strings.Contains(out, "node-app.yml: github") || !strings.Contains(out, "python-lib.gitlab-ci.yml: gitlab") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	code, out, _ = run(t, "", "providers")
	if code != 0 || strings.Count(out, "\n") != 5 {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestErrors(t *testing.T) {
	code, _, stderr := run(t, "", "convert", "../../testdata/github/node-app.yml", "--to", "jenkins")
	if code != 2 || !strings.Contains(stderr, `unknown provider "jenkins"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	code, _, stderr = run(t, "", "convert", "missing.yml", "--to", "github")
	if code != 2 || !strings.Contains(stderr, "no such file") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}
