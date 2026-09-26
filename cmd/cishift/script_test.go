package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// binDir holds the cishift binary built once by TestMain.
var binDir string

// TestMain builds cishift once into a temporary directory so every script
// runs the same executable a user would install.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cishift-script-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	exe := filepath.Join(dir, "cishift")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build cishift: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	binDir = dir

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestScript runs every testdata/examples/*.txtar as a testscript. These are
// the examples rendered into README.md by internal/tools/genexamples, so
// every command shown there is exercised here. Each script runs in its own
// work directory with cishift first on PATH and a private schema cache.
func TestScript(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("..", "..", "testdata", "examples"),
		Setup: func(env *testscript.Env) error {
			env.Setenv("PATH", binDir+string(filepath.ListSeparator)+env.Getenv("PATH"))
			env.Setenv("XDG_CACHE_HOME", filepath.Join(env.WorkDir, ".cache"))
			return nil
		},
	})
}
