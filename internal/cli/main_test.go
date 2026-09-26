package cli

import (
	"os"
	"testing"
)

// TestMain keeps a developer's downloaded schemas out of the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cishift-schemas")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CISHIFT_CACHE_DIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
