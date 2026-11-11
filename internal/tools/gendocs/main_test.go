package main

import (
	"reflect"
	"strings"
	"testing"
)

const sampleHelp = `ci tool does things.

Usage:
  tool <file> [flags]
  tool [command]

Aliases:
  tool, t

Examples:
  tool a.yml
  tool b.yml --to x

Available Commands:
  check       Check files
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  longername Short when the padding is a single space

Flags:
  -h, --help            help for tool
  -o, --output string   output file (default "out.yml")
      --strict          fail on warnings
  -v, --version         version for tool

Global Flags:
  -q, --quiet   say less

Use "tool [command] --help" for more information about a command.
`

func TestParseHelp(t *testing.T) {
	h := parseHelp(sampleHelp)
	if h.long != "ci tool does things." {
		t.Errorf("long = %q", h.long)
	}
	if want := []string{"tool <file> [flags]"}; !reflect.DeepEqual(h.usages, want) {
		t.Errorf("usages = %q, want %q", h.usages, want)
	}
	if want := []string{"t"}; !reflect.DeepEqual(h.aliases, want) {
		t.Errorf("aliases = %q, want %q", h.aliases, want)
	}
	if h.examples != "tool a.yml\ntool b.yml --to x" {
		t.Errorf("examples = %q", h.examples)
	}
	var names []string
	for _, c := range h.commands {
		names = append(names, c.name)
	}
	if want := []string{"check", "completion", "help", "longername"}; !reflect.DeepEqual(names, want) {
		t.Errorf("commands = %q, want %q", names, want)
	}
	if got := h.commands[3].short; got != "Short when the padding is a single space" {
		t.Errorf("short = %q", got)
	}
	if len(h.flags) != 2 || !strings.HasPrefix(h.flags[0], "-o, --output") {
		t.Errorf("flags = %q", h.flags)
	}
	if len(h.global) != 1 {
		t.Errorf("global = %q", h.global)
	}
}

func TestParseFlagLines(t *testing.T) {
	got := parseFlagLines([]string{
		`-o, --output string   output file (default "out.yml")`,
		`--strict          fail | on warnings`,
	})
	want := []flagInfo{
		{display: "`-o/--output OUTPUT`", compact: "[-o/--output OUTPUT]", defVal: "out.yml", desc: "output file"},
		{display: "`--strict`", compact: "[--strict]", desc: `fail \| on warnings`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestReplaceSection(t *testing.T) {
	doc := "intro `<!-- gendocs:x -->`\n" +
		"```\n<!-- gendocs:x -->\n<!-- /gendocs:x -->\n```\n" +
		"<!-- gendocs:x -->\nold\n<!-- /gendocs:x -->\ntail\n"
	got, err := replaceSection(doc, "<!-- gendocs:x -->", "<!-- /gendocs:x -->", "new")
	if err != nil {
		t.Fatal(err)
	}
	want := "intro `<!-- gendocs:x -->`\n" +
		"```\n<!-- gendocs:x -->\n<!-- /gendocs:x -->\n```\n" +
		"<!-- gendocs:x -->\nnew\n<!-- /gendocs:x -->\ntail\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, err := replaceSection("nothing here", "<!-- a -->", "<!-- /a -->", ""); err == nil {
		t.Error("want an error for missing markers")
	}
}

func TestSynopsis(t *testing.T) {
	cases := map[string]string{
		"Package store provides a database. More text.": "store provides a database",
		"Package main generates banners.":               "Generates banners",
		"Command tool converts files.":                  "tool converts files",
	}
	for in, want := range cases {
		pkg := "store"
		if strings.HasPrefix(in, "Package main") || strings.HasPrefix(in, "Command") {
			pkg = "main"
		}
		if got := synopsis(in, pkg); got != want {
			t.Errorf("synopsis(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenCommands(t *testing.T) {
	update := &cmdNode{path: []string{"schemas", "update"}, short: "Download | refresh"}
	tree := &cmdNode{
		help: helpData{
			usages: []string{"tool <file> [flags]"},
			flags:  []string{"-o, --output string   output file", "-q, --quiet   say less"},
		},
		subs: []*cmdNode{
			{path: []string{"check"}, short: "Check files", help: helpData{global: []string{"-q, --quiet   say less"}}},
			{path: []string{"schemas"}, short: "Show schemas", subs: []*cmdNode{update}},
		},
	}
	want := "```sh\ntool <file> [-o/--output OUTPUT]\n```\n\n" +
		"Flags: `-o/--output OUTPUT`\n\n" +
		"Global flags, accepted by every command: `-q/--quiet`\n\n" +
		"| Command | Description |\n|---------|-------------|\n" +
		"| [`tool check`](docs/CLI.md#tool-check) | Check files |\n" +
		"| [`tool schemas`](docs/CLI.md#tool-schemas) | Show schemas |\n" +
		"| [`tool schemas update`](docs/CLI.md#tool-schemas-update) | Download \\| refresh |\n\n" +
		"Every flag and example: [docs/CLI.md](docs/CLI.md)."
	if got := genCommands(tree, "tool", "docs/CLI.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := genCommands(tree, "tool", ""); strings.Contains(got, "](") {
		t.Errorf("no links without a reference file:\n%s", got)
	}
	if len(tree.help.flags) != 2 {
		t.Errorf("genCommands changed the tree's flags: %q", tree.help.flags)
	}
}

func TestParseMakefile(t *testing.T) {
	src := "BIN = tool\nGOBIN := $(shell go env GOPATH)/bin\nexport PATH := $(GOBIN):$(PATH)\n" +
		".PHONY: all build test\n\n" +
		"# The default target.\nall: build\n\n" +
		"# Build the binary,\n# stamped with the version.\nbuild:\n\tgo build\n\t# not a doc comment\n\n" +
		"# ── Section banner ──\n\ntest:\n\tgo test ./...\n" +
		"build:\n\techo again\n"
	got := parseMakefile(src)
	want := []makeTarget{
		{"all", "The default target."},
		{"build", "Build the binary, stamped with the version."},
		{"test", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	table := genMake(got)
	if !strings.Contains(table, "| `make build` | Build the binary, stamped with the version. |") ||
		!strings.Contains(table, "| `make test` |  |") {
		t.Errorf("table:\n%s", table)
	}
}

func TestRelLink(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"AGENTS.md", "docs/CLI.md", "docs/CLI.md"},
		{"docs/guide.md", "docs/CLI.md", "CLI.md"},
		{"e2e/README.md", "docs/CLI.md", "../docs/CLI.md"},
		{"AGENTS.md", "", ""},
	}
	for _, c := range cases {
		if got := relLink(c.from, c.to); got != c.want {
			t.Errorf("relLink(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

func TestAnchor(t *testing.T) {
	if got := anchor("tool", []string{"schemas", "update"}); got != "tool-schemas-update" {
		t.Errorf("anchor = %q", got)
	}
}
