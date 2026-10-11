package ir

import (
	"reflect"
	"testing"
)

func TestLevels(t *testing.T) {
	p := &Pipeline{Jobs: []*Job{
		{ID: "a"}, {ID: "b", Needs: []string{"a"}}, {ID: "c", Needs: []string{"a"}}, {ID: "d", Needs: []string{"b", "c"}},
	}}
	levels, err := p.Levels()
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, lv := range levels {
		var ids []string
		for _, j := range lv {
			ids = append(ids, j.ID)
		}
		got = append(got, ids)
	}
	want := [][]string{{"a"}, {"b", "c"}, {"d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("levels = %v, want %v", got, want)
	}
}

func TestValidateCycle(t *testing.T) {
	p := &Pipeline{Jobs: []*Job{{ID: "a", Needs: []string{"b"}}, {ID: "b", Needs: []string{"a"}}}}
	if err := p.Validate(); err == nil {
		t.Fatal("expected cycle error")
	}
	p = &Pipeline{Jobs: []*Job{{ID: "a", Needs: []string{"missing"}}}}
	if err := p.Validate(); err == nil {
		t.Fatal("expected unknown job error")
	}
}

func TestStageNames(t *testing.T) {
	p := &Pipeline{Jobs: []*Job{{ID: "build"}, {ID: "unit", Needs: []string{"build"}}, {ID: "e2e", Needs: []string{"build"}}}}
	stages, assign, err := p.StageNames()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stages, []string{"build", "stage-2"}) {
		t.Fatalf("stages = %v", stages)
	}
	if assign["e2e"] != "stage-2" {
		t.Fatalf("assign = %v", assign)
	}
	// Declared stages are kept when consistent with needs.
	p.Jobs[0].Stage, p.Jobs[1].Stage, p.Jobs[2].Stage = "compile", "test", "test"
	stages, _, _ = p.StageNames()
	if !reflect.DeepEqual(stages, []string{"compile", "test"}) {
		t.Fatalf("stages = %v", stages)
	}
}

func TestMatrixCombos(t *testing.T) {
	m := &Matrix{
		Axes:    []Axis{{Name: "os", Values: []string{"linux", "windows"}}, {Name: "node", Values: []string{"18", "20"}}},
		Exclude: [][]KV{{{"os", "windows"}, {"node", "18"}}},
		Include: [][]KV{{{"os", "linux"}, {"experimental", "true"}}, {{"os", "macos"}, {"node", "20"}}},
	}
	got := m.Combos()
	want := [][]KV{
		{{"os", "linux"}, {"node", "18"}, {"experimental", "true"}},
		{{"os", "linux"}, {"node", "20"}, {"experimental", "true"}},
		{{"os", "windows"}, {"node", "20"}},
		{{"os", "macos"}, {"node", "20"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("combos = %v\nwant %v", got, want)
	}
	if vars := m.Vars(); !reflect.DeepEqual(vars, []string{"os", "node", "experimental"}) {
		t.Fatalf("vars = %v", vars)
	}
}

func TestMatrixInclude(t *testing.T) {
	osNode := []Axis{{Name: "os", Values: []string{"linux", "windows"}}, {Name: "node", Values: []string{"18", "20"}}}
	tests := []struct {
		name string
		m    Matrix
		want [][]KV
	}{
		{
			name: "only new keys extend every combination",
			m:    Matrix{Axes: osNode, Include: [][]KV{{{"experimental", "true"}}}},
			want: [][]KV{
				{{"os", "linux"}, {"node", "18"}, {"experimental", "true"}},
				{{"os", "linux"}, {"node", "20"}, {"experimental", "true"}},
				{{"os", "windows"}, {"node", "18"}, {"experimental", "true"}},
				{{"os", "windows"}, {"node", "20"}, {"experimental", "true"}},
			},
		},
		{
			name: "existing combination is not duplicated",
			m:    Matrix{Axes: osNode, Include: [][]KV{{{"os", "linux"}, {"node", "18"}}}},
			want: [][]KV{
				{{"os", "linux"}, {"node", "18"}},
				{{"os", "linux"}, {"node", "20"}},
				{{"os", "windows"}, {"node", "18"}},
				{{"os", "windows"}, {"node", "20"}},
			},
		},
		{
			name: "added value is overwritten by a later include",
			m: Matrix{Axes: osNode, Include: [][]KV{
				{{"os", "linux"}, {"npm", "6"}},
				{{"node", "20"}, {"npm", "7"}},
			}},
			want: [][]KV{
				{{"os", "linux"}, {"node", "18"}, {"npm", "6"}},
				{{"os", "linux"}, {"node", "20"}, {"npm", "7"}},
				{{"os", "windows"}, {"node", "18"}},
				{{"os", "windows"}, {"node", "20"}, {"npm", "7"}},
			},
		},
		{
			name: "excluded combination is added back",
			m: Matrix{
				Axes:    osNode,
				Exclude: [][]KV{{{"os", "windows"}}},
				Include: [][]KV{{{"os", "windows"}, {"node", "20"}}},
			},
			want: [][]KV{
				{{"os", "linux"}, {"node", "18"}},
				{{"os", "linux"}, {"node", "20"}},
				{{"os", "windows"}, {"node", "20"}},
			},
		},
		{
			name: "without axes every include is its own combination",
			m:    Matrix{Include: [][]KV{{{"os", "linux"}}, {{"os", "linux"}, {"arch", "arm64"}}}},
			want: [][]KV{
				{{"os", "linux"}},
				{{"os", "linux"}, {"arch", "arm64"}},
			},
		},
		{
			// docs.github.com: "Expanding or adding matrix configurations".
			name: "github docs example",
			m: Matrix{
				Axes: []Axis{{Name: "fruit", Values: []string{"apple", "pear"}}, {Name: "animal", Values: []string{"cat", "dog"}}},
				Include: [][]KV{
					{{"color", "green"}},
					{{"color", "pink"}, {"animal", "cat"}},
					{{"fruit", "apple"}, {"shape", "circle"}},
					{{"fruit", "banana"}},
					{{"fruit", "banana"}, {"animal", "cat"}},
				},
			},
			want: [][]KV{
				{{"fruit", "apple"}, {"animal", "cat"}, {"color", "pink"}, {"shape", "circle"}},
				{{"fruit", "apple"}, {"animal", "dog"}, {"color", "green"}, {"shape", "circle"}},
				{{"fruit", "pear"}, {"animal", "cat"}, {"color", "pink"}},
				{{"fruit", "pear"}, {"animal", "dog"}, {"color", "green"}},
				{{"fruit", "banana"}},
				{{"fruit", "banana"}, {"animal", "cat"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Combos(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("combos = %v\nwant     %v", got, tt.want)
			}
		})
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]int{"1h 30m": 90, "3 hours": 180, "45": 45, "90 minutes": 90, "30s": 1, "1 day": 1440} {
		if got, ok := ParseMinutes(in); !ok || got != want {
			t.Errorf("ParseMinutes(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for in, want := range map[string]int{"1 week": 7, "30 days": 30, "2 months": 60, "3 hours": 1, "7": 7} {
		if got, ok := DurationDays(in); !ok || got != want {
			t.Errorf("DurationDays(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := DurationDays("never"); ok {
		t.Error("never should not parse")
	}
	if got := FormatMinutes(90); got != "1h 30m" {
		t.Errorf("FormatMinutes(90) = %q", got)
	}
}

func TestGlobRegex(t *testing.T) {
	if got := GlobToRegex("release/*"); got != `^release\/.*$` {
		t.Errorf("GlobToRegex = %q", got)
	}
	for re, want := range map[string]string{`/^release\/.*$/`: "release/*", `/^main$/`: "main", `^feature-`: "feature-*"} {
		if got, ok := RegexToGlob(re); !ok || got != want {
			t.Errorf("RegexToGlob(%q) = %q, %v; want %q", re, got, ok, want)
		}
	}
	if _, ok := RegexToGlob(`/^(a|b)$/`); ok {
		t.Error("alternation should not convert")
	}
}

func TestRefs(t *testing.T) {
	s := "echo ${{ ci.sha }} ${{ secret.TOKEN }} ${{ github.foo }}"
	got := RewriteRefs(s, func(kind, name string) string { return "<" + kind + ":" + name + ">" })
	if got != "echo <ci:sha> <secret:TOKEN> ${{ github.foo }}" {
		t.Fatalf("RewriteRefs = %q", got)
	}
	if f := ForeignExpressions(s); !reflect.DeepEqual(f, []string{"${{ github.foo }}"}) {
		t.Fatalf("ForeignExpressions = %v", f)
	}
	nv := NativeVars{{Canon: VarSHA, Native: "CI_COMMIT_SHA"}}
	if got := nv.FromShell("git checkout $CI_COMMIT_SHA ${CI_COMMIT_SHA}x $CI_COMMIT_SHAX"); got != "git checkout ${{ ci.sha }} ${{ ci.sha }}x $CI_COMMIT_SHAX" {
		t.Fatalf("FromShell = %q", got)
	}
}

func TestPromoteExternalVars(t *testing.T) {
	p := &Pipeline{
		Env: []KV{{"GLOBAL", "1"}},
		Jobs: []*Job{{ID: "a", Env: []KV{{"LOCAL", "x"}}, Steps: []*Step{{
			Kind: StepRun,
			Run:  "export BUILT=1\nfor f in $FILES; do echo $f; done\ncurl -H \"$API_TOKEN\" $GLOBAL $LOCAL $BUILT $HOME $CI_JOB_ID",
		}}}},
	}
	var d Diagnostics
	PromoteExternalVars(p, &d, "CI_")
	want := "export BUILT=1\nfor f in ${{ secret.FILES }}; do echo $f; done\ncurl -H \"${{ secret.API_TOKEN }}\" $GLOBAL $LOCAL $BUILT $HOME $CI_JOB_ID"
	if got := p.Jobs[0].Steps[0].Run; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if secrets := p.Secrets(); !reflect.DeepEqual(secrets, []string{"API_TOKEN", "FILES"}) {
		t.Fatalf("secrets = %v", secrets)
	}
}

func TestShellCondition(t *testing.T) {
	got, ok := ShellCondition(&Condition{Branches: []string{"main", "release/*"}}, "B", "T")
	want := `{ [ "${B:-}" = "main" ] || case "${B:-}" in release/*) true;; *) false;; esac; }`
	if !ok || got != want {
		t.Fatalf("got %q", got)
	}
	if _, ok := ShellCondition(&Condition{Manual: true}, "B", "T"); ok {
		t.Fatal("manual conditions are not expressible in shell")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Build & Test": "build-test", "1st": "job-1st", "a/b:c": "a-b-c", "": "job"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if IdentSlug("unit-tests") != "unit_tests" {
		t.Error("IdentSlug")
	}
}

func TestToolImage(t *testing.T) {
	cases := map[[2]string]string{
		{ToolNode, "20.x"}:               "node:20",
		{ToolPython, ""}:                 "python:3",
		{ToolNode, "${{ matrix.node }}"}: "node:${{ matrix.node }}",
		{ToolGo, "$GO_VERSION"}:          "golang:1",
		{ToolDotnet, "8.0.x"}:            "mcr.microsoft.com/dotnet/sdk:8.0",
	}
	for in, want := range cases {
		if got := ToolImage(in[0], in[1]); got != want {
			t.Errorf("ToolImage(%v) = %q, want %q", in, got, want)
		}
	}
	if tool, v := ToolFromImage("cimg/node:20.11"); tool != ToolNode || v != "20.11" {
		t.Errorf("ToolFromImage = %s %s", tool, v)
	}
}
