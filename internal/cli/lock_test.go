package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/internal/lockfile"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLockfileLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".gitlab-ci.yml")
	out := filepath.Join(dir, ".github", "workflows", "ci.yml")
	lock := filepath.Join(dir, lockfile.Name)
	copyFile(t, "../../testdata/gitlab/python-lib.gitlab-ci.yml", src)

	// Writing with -o records provenance with lockfile-relative paths.
	code, _, stderr := run(t, "", src, "--to", "github", "-o", out, "--lock", lock)
	if code != 0 || !strings.Contains(stderr, "recorded in") {
		t.Fatalf("convert: code=%d stderr=%s", code, stderr)
	}
	lf, err := lockfile.Load(lock)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lf.Get(".github/workflows/ci.yml")
	if !ok || e.Source != ".gitlab-ci.yml" || e.From != "gitlab" || e.To != "github" || e.Tool == "" ||
		!strings.HasPrefix(e.Schema, "github@sha256:") || !strings.HasPrefix(e.OutputSHA256, "sha256:") {
		t.Fatalf("entry = %+v", e)
	}
	generated, _ := os.ReadFile(out)
	if !strings.Contains(string(generated), "from .gitlab-ci.yml (GitLab CI/CD)") {
		t.Fatalf("header should name the lockfile-relative source:\n%s", generated[:200])
	}

	code, stdout, _ := run(t, "", "verify", "--lock", lock)
	if code != 0 || !strings.Contains(stdout, "✓ .github/workflows/ci.yml (from .gitlab-ci.yml)") {
		t.Fatalf("verify clean: code=%d out=%s", code, stdout)
	}

	// Hand edits are detected, protected from overwrites, and --force wins.
	if err := os.WriteFile(out, append(generated, []byte("# tweak\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = run(t, "", "verify", "--lock", lock)
	if code != 1 || !strings.Contains(stdout, "edited by hand") {
		t.Fatalf("verify edited: code=%d out=%s", code, stdout)
	}
	code, _, stderr = run(t, "", src, "--to", "github", "-o", out, "--lock", lock)
	if code != 2 || !strings.Contains(stderr, "use --force") {
		t.Fatalf("overwrite should be refused: code=%d stderr=%s", code, stderr)
	}
	code, stdout, _ = run(t, "", "sync", "--lock", lock)
	if code != 1 || !strings.Contains(stdout, "skipped .github/workflows/ci.yml") {
		t.Fatalf("sync should skip edited file: code=%d out=%s", code, stdout)
	}
	code, stdout, _ = run(t, "", "sync", "--lock", lock, "--force")
	if code != 0 || !strings.Contains(stdout, "regenerated .github/workflows/ci.yml") {
		t.Fatalf("sync --force: code=%d out=%s", code, stdout)
	}

	// Source changes make the output stale until synced.
	f, _ := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("\nlint-docs:\n  stage: lint\n  script: [make docs]\n")
	_ = f.Close()
	code, stdout, _ = run(t, "", "verify", "--lock", lock)
	if code != 1 || !strings.Contains(stdout, ".gitlab-ci.yml changed since it was generated") {
		t.Fatalf("verify stale: code=%d out=%s", code, stdout)
	}
	code, stdout, _ = run(t, "", "sync", "--lock", lock)
	if code != 0 || !strings.Contains(stdout, "regenerated") {
		t.Fatalf("sync: code=%d out=%s", code, stdout)
	}
	regenerated, _ := os.ReadFile(out)
	if !strings.Contains(string(regenerated), "make docs") {
		t.Fatal("sync did not pick up the source change")
	}
	code, stdout, _ = run(t, "", "verify", "--lock", lock)
	if code != 0 {
		t.Fatalf("verify after sync: code=%d out=%s", code, stdout)
	}
	code, stdout, _ = run(t, "", "sync", "--lock", lock)
	if code != 0 || !strings.Contains(stdout, "up to date") {
		t.Fatalf("second sync: code=%d out=%s", code, stdout)
	}

	// --no-lock leaves the lockfile alone.
	other := filepath.Join(dir, "azure-pipelines.yml")
	code, _, _ = run(t, "", src, "--to", "azure", "-o", other, "--lock", lock, "--no-lock", "-q")
	if code != 0 {
		t.Fatalf("no-lock convert failed: %d", code)
	}
	lf, _ = lockfile.Load(lock)
	if len(lf.Generated) != 1 {
		t.Fatalf("--no-lock recorded an entry: %+v", lf.Generated)
	}
}

func TestLockfileFind(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.Find(sub)
	if err != nil || got != filepath.Join(root, lockfile.Name) {
		t.Fatalf("Find in git repo = %s, %v", got, err)
	}
	nested := filepath.Join(root, "a", lockfile.Name)
	if err := os.WriteFile(nested, []byte("version: 1\ngenerated: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := lockfile.Find(sub); got != nested {
		t.Fatalf("nearest existing lockfile should win, got %s", got)
	}
}

func TestSchemasAndVersionCommands(t *testing.T) {
	code, out, _ := run(t, "", "schemas")
	if code != 0 || !strings.Contains(out, "github") || !strings.Contains(out, "embedded") || !strings.Contains(out, "@sha256:") {
		t.Fatalf("schemas: code=%d out=%s", code, out)
	}
	code, out, _ = run(t, "", "version")
	if code != 0 || !strings.HasPrefix(out, "cishift ") {
		t.Fatalf("version: code=%d out=%s", code, out)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v1.2.0", "v1.1.9", true}, {"v1.2.0", "v1.2.0", false}, {"v2.0.0-rc1", "v1.9.9", true}, {"1.0.0", "v1.0.1", false}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
