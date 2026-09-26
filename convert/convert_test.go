package convert

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/schema"
)

var update = flag.Bool("update", false, "rewrite golden files")

type fixture struct {
	name     string // e.g. "gitlab/python-lib"
	provider ir.Provider
	data     []byte
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	var out []fixture
	for _, p := range Providers() {
		files, _ := filepath.Glob(filepath.Join("..", "testdata", p.Name(), "*.yml"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSuffix(filepath.Base(f), ".yml")
			base = strings.TrimSuffix(base, ".gitlab-ci")
			out = append(out, fixture{name: p.Name() + "/" + base, provider: p, data: data})
		}
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found")
	}
	return out
}

// TestGolden converts every fixture to every provider and checks that the
// output (1) validates against the target schema, (2) matches the golden
// file, (3) is detected as the target provider and (4) is a fixed point:
// parsing and re-emitting it yields the same document.
func TestGolden(t *testing.T) {
	for _, fx := range fixtures(t) {
		for _, to := range Providers() {
			fx, to := fx, to
			t.Run(fx.name+"->"+to.Name(), func(t *testing.T) {
				res, err := Convert(fx.data, fx.provider, to, Options{Header: true, SourceName: fx.name})
				if err != nil {
					t.Fatal(err)
				}
				issues, err := schema.Validate(to.Name(), res.Output, schema.Options{})
				if err != nil {
					t.Fatal(err)
				}
				for _, i := range issues {
					t.Errorf("schema: %s", i)
				}
				if t.Failed() {
					t.Logf("output:\n%s", res.Output)
				}

				golden := filepath.Join("..", "testdata", "golden", strings.ReplaceAll(fx.name, "/", "_")+"."+to.Name()+".yml")
				if *update {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, res.Output, 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					want, err := os.ReadFile(golden)
					if err != nil {
						t.Fatalf("%v (run `make golden` to create it)", err)
					}
					if string(want) != string(res.Output) {
						t.Errorf("output differs from %s (run `make golden` if intended)\n%s", golden, diff(string(want), string(res.Output)))
					}
				}

				if got, err := Detect(to.DefaultPath(), res.Output); err != nil || got.Name() != to.Name() {
					t.Errorf("detect(output) = %v, %v", got, err)
				}
				body := stripHeader(res.Output)
				if got, err := Detect("pipeline.yml", body); err != nil || got.Name() != to.Name() {
					t.Errorf("content-only detect(output) = %v, %v", got, err)
				}

				again, err := Convert(body, to, to, Options{})
				if err != nil {
					t.Fatalf("re-parse: %v", err)
				}
				first, err := Convert(fx.data, fx.provider, to, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if string(again.Output) != string(first.Output) {
					t.Errorf("emit(parse(output)) is not a fixed point\n%s", diff(string(first.Output), string(again.Output)))
				}
			})
		}
	}
}

func stripHeader(b []byte) []byte {
	lines := strings.SplitAfter(string(b), "\n")
	i := 0
	for i < len(lines) && (strings.HasPrefix(lines[i], "#") || strings.TrimSpace(lines[i]) == "") {
		i++
	}
	return []byte(strings.Join(lines[i:], ""))
}

// diff returns a small line diff good enough for test output.
func diff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "- "+x, "+ "+y)
			if len(out) > 40 {
				out = append(out, "...")
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

func TestDetectAmbiguity(t *testing.T) {
	if _, err := Detect("x.yml", []byte("foo: bar\n")); err == nil {
		t.Fatal("expected detection failure")
	}
}

func TestGetAliases(t *testing.T) {
	for alias, want := range map[string]string{"gh": "github", "GitLab-CI": "gitlab", "ado": "azure", "bb": "bitbucket", "circle": "circleci"} {
		p, err := Get(alias)
		if err != nil || p.Name() != want {
			t.Errorf("Get(%q) = %v, %v", alias, p, err)
		}
	}
}
