package schema

import (
	"io"
	"net/http"
	"os"
	"strings"
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

func TestSnapshotsCompile(t *testing.T) {
	for _, s := range Sources {
		if _, err := Load(s.Provider, Options{}); err != nil {
			t.Errorf("%s: %v", s.Provider, err)
		}
	}
}

func TestValidateGitHub(t *testing.T) {
	valid, err := os.ReadFile("../testdata/validate-valid-github.yml")
	if err != nil {
		t.Fatal(err)
	}
	issues, err := Validate("github", valid, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues, got %v", issues)
	}
	invalid, err := os.ReadFile("../testdata/validate-invalid-github.yml")
	if err != nil {
		t.Fatal(err)
	}
	issues, err = Validate("github", invalid, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("expected issues for invalid workflow")
	}
	for _, i := range issues {
		t.Log(i)
	}
}

func TestValidateCircleCIVersionNumber(t *testing.T) {
	doc := []byte("version: 2.1\njobs:\n  build:\n    docker:\n      - image: cimg/base:stable\n    steps:\n      - checkout\nworkflows:\n  main:\n    jobs: [build]\n")
	issues, err := Validate("circleci", doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
}

// fakeTransport serves the embedded snapshot for every schema URL, or the
// given body when set.
type fakeTransport struct{ body string }

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := f.body
	if body == "" {
		for _, s := range Sources {
			if s.URL == r.URL.String() {
				data, _ := snapshots.ReadFile("snapshots/" + s.file())
				body = string(data)
			}
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestHybridSources(t *testing.T) {
	opts := Options{CacheDir: t.TempDir(), HTTPClient: &http.Client{Transport: fakeTransport{}}}
	status, err := Status(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range status {
		if d.Origin != OriginEmbedded {
			t.Fatalf("%s: origin %s before update", d.Provider, d.Origin)
		}
	}
	docs, errs := Update(opts)
	if len(errs) > 0 || len(docs) != len(Sources) {
		t.Fatalf("update: %d docs, errors %v", len(docs), errs)
	}
	gh, err := Resolve("github", opts)
	if err != nil || gh.Origin != OriginCache || gh.UpdatedAt.IsZero() {
		t.Fatalf("after update: %+v, %v", gh, err)
	}
	if !strings.HasPrefix(gh.Fingerprint(), "github@sha256:") || len(gh.Fingerprint()) != len("github@sha256:")+12 {
		t.Fatalf("fingerprint %q", gh.Fingerprint())
	}
	forced, _ := Resolve("github", Options{CacheDir: opts.CacheDir, Embedded: true})
	if forced.Origin != OriginEmbedded {
		t.Fatalf("Embedded should ignore the cache: %s", forced.Origin)
	}
	if err := Reset(opts); err != nil {
		t.Fatal(err)
	}
	if d, _ := Resolve("github", opts); d.Origin != OriginEmbedded {
		t.Fatalf("after reset: %s", d.Origin)
	}
}

func TestUpdateRejectsBrokenSchemas(t *testing.T) {
	opts := Options{CacheDir: t.TempDir(), HTTPClient: &http.Client{Transport: fakeTransport{body: "<html>rate limited</html>"}}}
	_, errs := Update(opts)
	if len(errs) != len(Sources) {
		t.Fatalf("expected every provider to fail, got %v", errs)
	}
	if d, _ := Resolve("gitlab", opts); d.Origin != OriginEmbedded {
		t.Fatalf("a failed update must not replace the schema: %s", d.Origin)
	}
}
