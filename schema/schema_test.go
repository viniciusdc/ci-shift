package schema

import (
	"os"
	"testing"
)

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
