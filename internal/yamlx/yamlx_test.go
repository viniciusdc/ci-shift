package yamlx

import (
	"strings"
	"testing"
)

func TestLoadResolvesAnchorsAndMerges(t *testing.T) {
	root, err := Load([]byte(`
base: &base
  image: node:20
  script: [echo base]
job:
  <<: *base
  script: [echo job]
list: *base
`))
	if err != nil {
		t.Fatal(err)
	}
	job := Get(root, "job")
	if Str(Get(job, "image")) != "node:20" {
		t.Errorf("merged image missing: %v", Keys(job))
	}
	if got := StrList(Get(job, "script")); len(got) != 1 || got[0] != "echo job" {
		t.Errorf("explicit key should win, got %v", got)
	}
	if Keys(job)[0] != "script" {
		t.Errorf("explicit keys come first, got %v", Keys(job))
	}
	if Str(Path(root, "list", "image")) != "node:20" {
		t.Error("alias not resolved")
	}
}

func TestLoadEmpty(t *testing.T) {
	root, err := Load([]byte(""))
	if err != nil || root != nil {
		t.Fatalf("got %v, %v", root, err)
	}
}

func TestStrListFlattens(t *testing.T) {
	root, _ := Load([]byte("s: [a, [b, c], d]"))
	if got := strings.Join(StrList(Get(root, "s")), ","); got != "a,b,c,d" {
		t.Fatalf("got %s", got)
	}
}

func TestMarshal(t *testing.T) {
	m := NewMap().
		Set("on", NewMap().Set("push", nil).Set("workflow_dispatch", nil)).
		Set("version", "2.1").
		Set("flag", "yes").
		Set("list", Flow{Value: []string{"a", "b"}}).
		Set("script", "line1\nline2").
		SetIf("empty", "").
		Set("job", Comment{Text: "Build and test", Value: NewMap().Set("x", 1)})
	out, err := Marshal(m, "header")
	if err != nil {
		t.Fatal(err)
	}
	want := `# header

on:
  push:
  workflow_dispatch:
version: '2.1'
flag: 'yes'
list: [a, b]
script: |-
  line1
  line2
# Build and test
job:
  x: 1
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestMapOrderAndDelete(t *testing.T) {
	m := NewMap().Set("b", 1).Set("a", 2).Set("b", 3)
	if strings.Join(m.Keys(), ",") != "b,a" {
		t.Fatalf("keys %v", m.Keys())
	}
	m.Delete("b")
	if m.Len() != 1 {
		t.Fatal("delete failed")
	}
}
