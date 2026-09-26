// Package main generates the CLI reference and project layout sections of the
// docs. It injects them into Markdown files between HTML comment markers:
//
//	<!-- gendocs:NAME --> ... content ... <!-- /gendocs:NAME -->
//
// Two sections are produced, both configured in docsmith.toml:
//
//   - cli: a command reference for the cobra binary built from
//     [project].main, written to [docs].cli_file. The command tree is
//     discovered by walking "<bin> --help" recursively, so there is nothing
//     to keep in sync by hand; hidden commands never show up and the
//     built-in help and completion commands are skipped.
//   - layout: a directory tree written to [docs].layout_file, annotated with
//     the first sentence of each Go package's doc comment, or with an entry
//     from [docs.layout.annotations].
//
// Run via:
//
//	go run ./internal/tools/gendocs
//	make docs
package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// config is the subset of docsmith.toml this tool reads.
type config struct {
	Project struct {
		Binary string `toml:"binary"`
		Main   string `toml:"main"`
	} `toml:"project"`
	Docs struct {
		CLIFile    string `toml:"cli_file"`
		LayoutFile string `toml:"layout_file"`
		Layout     struct {
			RootFiles   []string          `toml:"root_files"`
			Skip        []string          `toml:"skip"`
			MaxDepth    int               `toml:"max_depth"`
			Annotations map[string]string `toml:"annotations"`
		} `toml:"layout"`
	} `toml:"docs"`
}

func main() {
	root, err := findRoot()
	if err != nil {
		fatalf("%v", err)
	}
	var cfg config
	if _, err := toml.DecodeFile(filepath.Join(root, configFile), &cfg); err != nil {
		fatalf("%v", err)
	}

	if cfg.Docs.CLIFile != "" {
		if cfg.Project.Binary == "" || cfg.Project.Main == "" {
			fatalf("[docs].cli_file needs [project].binary and [project].main")
		}
		bin, cleanup := build(root, cfg.Project.Binary, cfg.Project.Main)
		cli := genCLI(bin, cfg.Project.Binary)
		cleanup()
		inject(root, cfg.Docs.CLIFile, "cli", cli)
	}
	if cfg.Docs.LayoutFile != "" {
		l := cfg.Docs.Layout
		inject(root, cfg.Docs.LayoutFile, "layout", genLayout(root, l.RootFiles, l.Skip, l.MaxDepth, l.Annotations))
	}
}

// ----- injection ------------------------------------------------------------

// inject replaces the content between the <!-- gendocs:name --> and
// <!-- /gendocs:name --> marker lines in root/relFile.
func inject(root, relFile, name, content string) {
	path := filepath.Join(root, filepath.FromSlash(relFile))
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", relFile, err)
	}
	open := fmt.Sprintf("<!-- gendocs:%s -->", name)
	end := fmt.Sprintf("<!-- /gendocs:%s -->", name)
	updated, err := replaceSection(string(data), open, end, content)
	if err != nil {
		fatalf("%s: %v", relFile, err)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		fatalf("write %s: %v", relFile, err)
	}
	fmt.Printf("  updated %-20s [%s]\n", relFile, name)
}

// replaceSection replaces the lines between the marker lines open and end in
// doc with content. Markers only count when they sit on a line of their own
// outside fenced code blocks, so docs can show the markers in examples.
func replaceSection(doc, open, end, content string) (string, error) {
	lines := strings.SplitAfter(doc, "\n")
	start, stop := -1, -1
	fence := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		switch strings.TrimRight(l, " \t\r\n") {
		case open:
			if start < 0 {
				start = i
			}
		case end:
			if start >= 0 && stop < 0 {
				stop = i
			}
		}
	}
	if start < 0 || stop < 0 {
		return "", fmt.Errorf("no %s / %s marker lines", open, end)
	}
	var b strings.Builder
	for _, l := range lines[:start+1] {
		b.WriteString(l)
	}
	b.WriteString(content + "\n")
	for _, l := range lines[stop:] {
		b.WriteString(l)
	}
	return b.String(), nil
}

// ----- CLI reference --------------------------------------------------------

// build compiles the CLI once into a temporary directory; the returned
// cleanup removes it.
func build(root, name, pkg string) (string, func()) {
	dir, err := os.MkdirTemp("", "gendocs-*")
	if err != nil {
		fatalf("mktemp: %v", err)
	}
	exe := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return exe, func() { _ = os.RemoveAll(dir) }
}

// cmdNode is one command in the discovered tree.
type cmdNode struct {
	path  []string // words after the binary name, e.g. ["schemas", "update"]
	short string   // one-line description from the parent's command list
	help  helpData
	subs  []*cmdNode
}

// helpData is what we keep from one --help page.
type helpData struct {
	long     string   // description paragraph(s) before "Usage:"
	usages   []string // usage lines, minus the "<bin> [command]" form
	aliases  []string // other names for the command
	examples string   // the Examples: block, dedented
	flags    []string // local flag lines (help and version excluded)
	global   []string // "Global Flags:" lines
	commands []subCmd // "Available Commands" (and cobra group sections)
}

type subCmd struct{ name, short string }

// helpSkip lists commands cobra adds on its own.
var helpSkip = map[string]bool{"help": true, "completion": true}

// discover walks the command tree by running "<bin> <path> --help".
func discover(bin string, path []string, short string) *cmdNode {
	args := append(append([]string{}, path...), "--help")
	out, err := exec.Command(bin, args...).Output() // #nosec G204 -- bin is the binary we just built
	if err != nil {
		fatalf("%s %s: %v", filepath.Base(bin), strings.Join(args, " "), err)
	}
	n := &cmdNode{path: path, short: short, help: parseHelp(string(out))}
	for _, c := range n.help.commands {
		if helpSkip[c.name] {
			continue
		}
		n.subs = append(n.subs, discover(bin, append(append([]string{}, path...), c.name), c.short))
	}
	return n
}

// parseHelp splits cobra's default help template into its sections. Section
// headers are unindented lines ending in ":"; any header ending in
// "Commands:" is a command list, which covers cobra command groups too.
func parseHelp(raw string) helpData {
	var h helpData
	var long, examples []string
	section := "long"
	for _, line := range strings.Split(raw, "\n") {
		trim := strings.TrimRight(line, " \t")
		if trim != "" && !strings.HasPrefix(trim, " ") && strings.HasSuffix(trim, ":") {
			switch {
			case trim == "Usage:":
				section = "usage"
			case trim == "Aliases:":
				section = "aliases"
			case trim == "Examples:":
				section = "examples"
			case trim == "Flags:":
				section = "flags"
			case trim == "Global Flags:":
				section = "global"
			case strings.HasSuffix(trim, "Commands:"):
				section = "commands"
			default:
				section = "other"
			}
			continue
		}
		if strings.HasPrefix(trim, "Use \"") && strings.HasSuffix(trim, "more information about a command.") {
			continue
		}
		field := strings.TrimSpace(trim)
		switch section {
		case "long":
			long = append(long, trim)
		case "usage":
			if field != "" && !strings.HasSuffix(field, "[command]") {
				h.usages = append(h.usages, field)
			}
		case "aliases":
			if field != "" {
				for _, a := range strings.Split(field, ",") {
					h.aliases = append(h.aliases, strings.TrimSpace(a))
				}
			}
		case "examples":
			examples = append(examples, trim)
		case "flags", "global":
			if field == "" || !strings.HasPrefix(field, "-") || isBuiltinFlag(field) {
				continue
			}
			if section == "flags" {
				h.flags = append(h.flags, field)
			} else {
				h.global = append(h.global, field)
			}
		case "commands":
			// "  name   short description" — the name is the first field;
			// the padding width depends on the longest sibling name.
			if name, desc, ok := strings.Cut(field, " "); ok {
				h.commands = append(h.commands, subCmd{name, strings.TrimSpace(desc)})
			} else if field != "" {
				h.commands = append(h.commands, subCmd{name: field})
			}
		}
	}
	h.long = strings.TrimSpace(strings.Join(long, "\n"))
	h.examples = dedent(strings.Trim(strings.Join(examples, "\n"), "\n"))
	// Aliases list the command's own name first.
	if len(h.aliases) > 0 {
		h.aliases = h.aliases[1:]
	}
	return h
}

// isBuiltinFlag reports the --help and --version flags cobra adds to every
// command, which would only repeat themselves in every table.
func isBuiltinFlag(field string) bool { return builtinFlagRE.MatchString(field) }

var builtinFlagRE = regexp.MustCompile(`^(-[hv], )?--(help|version)\b`)

func dedent(s string) string {
	lines := strings.Split(s, "\n")
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	for i, l := range lines {
		if len(l) >= min && min > 0 {
			lines[i] = l[min:]
		}
	}
	return strings.Join(lines, "\n")
}

// ── Flag parsing ─────────────────────────────────────────────────────────────

var (
	splitRE   = regexp.MustCompile(`\s{2,}`)
	defaultRE = regexp.MustCompile(`\s*\(default\s+"?([^)"]*)"?\)\s*$`)
)

type flagInfo struct {
	display string // "`-f/--file FILE`" or "`--dry-run`"
	compact string // "[-f/--file FILE]"
	defVal  string // extracted default value, or ""
	desc    string
}

func parseFlagLines(lines []string) []flagInfo {
	var out []flagInfo
	for _, line := range lines {
		parts := splitRE.Split(line, 2)
		flagPart := strings.TrimSpace(parts[0])
		descPart := ""
		if len(parts) == 2 {
			descPart = strings.TrimSpace(parts[1])
		}

		// Extract "(default ...)" from the end of the description.
		var defVal string
		if m := defaultRE.FindStringSubmatch(descPart); m != nil {
			defVal = m[1]
			descPart = strings.TrimSpace(defaultRE.ReplaceAllString(descPart, ""))
		}

		// Parse "-X, --long [type]" — remove commas, then classify tokens.
		var short, long, typ string
		for _, f := range strings.Fields(strings.ReplaceAll(flagPart, ",", "")) {
			switch {
			case strings.HasPrefix(f, "--"):
				long = f
			case strings.HasPrefix(f, "-"):
				short = f
			default:
				typ = f
			}
		}
		name := long
		if name == "" {
			name = short
		}
		if short != "" && long != "" {
			name = short + "/" + long
		}
		if typ != "" {
			// Use the flag's own name as the placeholder: --output FILE reads
			// better than --output string.
			placeholder := strings.ToUpper(strings.TrimLeft(long, "-"))
			if placeholder == "" {
				placeholder = strings.ToUpper(typ)
			}
			name += " " + placeholder
		}
		out = append(out, flagInfo{
			display: "`" + name + "`",
			compact: "[" + name + "]",
			defVal:  defVal,
			desc:    escapeCell(descPart),
		})
	}
	return out
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func writeFlagsTable(b *bytes.Buffer, flags []flagInfo) {
	if len(flags) == 0 {
		return
	}
	hasDef := false
	for _, f := range flags {
		if f.defVal != "" {
			hasDef = true
			break
		}
	}
	if hasDef {
		b.WriteString("| Flag | Default | Description |\n|------|---------|-------------|\n")
		for _, f := range flags {
			def := ""
			if f.defVal != "" {
				def = "`" + f.defVal + "`"
			}
			fmt.Fprintf(b, "| %s | %s | %s |\n", f.display, def, f.desc)
		}
	} else {
		b.WriteString("| Flag | Description |\n|------|-------------|\n")
		for _, f := range flags {
			fmt.Fprintf(b, "| %s | %s |\n", f.display, f.desc)
		}
	}
	b.WriteByte('\n')
}

// ── CLI reference generator ───────────────────────────────────────────────────

// genCLI renders the reference: the root command first, then one section per
// top-level command. A command with subcommands gets a summary table
// followed by a subsection for each leaf.
func genCLI(bin, name string) string {
	tree := discover(bin, nil, "")
	var b bytes.Buffer

	// Persistent flags show up as "Global Flags" on any subcommand; the root
	// lists them among its own flags, so they are split out there.
	var global []flagInfo
	if len(tree.subs) > 0 {
		global = parseFlagLines(tree.subs[0].help.global)
	}
	isGlobal := map[string]bool{}
	for _, g := range global {
		isGlobal[g.display] = true
	}
	var rootFlags []string
	for _, line := range tree.help.flags {
		if f := parseFlagLines([]string{line}); len(f) == 1 && !isGlobal[f[0].display] {
			rootFlags = append(rootFlags, line)
		}
	}
	tree.help.flags = rootFlags

	// Root command.
	if tree.help.long != "" {
		b.WriteString(tree.help.long + "\n\n")
	}
	writeUsage(&b, tree)
	writeFlagsTable(&b, parseFlagLines(rootFlags))
	if tree.help.examples != "" {
		fmt.Fprintf(&b, "```sh\n%s\n```\n\n", tree.help.examples)
	}
	if len(global) > 0 {
		b.WriteString("Global flags, accepted by every command:")
		for _, f := range global {
			b.WriteString(" " + f.display)
		}
		b.WriteString("\n\n")
	}

	if len(tree.subs) > 0 {
		b.WriteString("| Command | Description |\n|---------|-------------|\n")
		for _, s := range tree.subs {
			fmt.Fprintf(&b, "| [`%s`](#%s) | %s |\n", s.path[0], anchor(name, s.path), escapeCell(s.short))
		}
		b.WriteString("\n")
	}

	for _, top := range tree.subs {
		b.WriteString("---\n\n")
		fmt.Fprintf(&b, "### `%s %s`\n\n", name, top.path[0])
		writeCommand(&b, top, name)
		for _, leaf := range leaves(top) {
			if leaf == top {
				continue
			}
			fmt.Fprintf(&b, "#### `%s %s`\n\n", name, strings.Join(leaf.path, " "))
			writeCommand(&b, leaf, name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeCommand renders one command's description, usage, flags, aliases and
// examples; for a command group it adds a table of its leaf commands.
func writeCommand(b *bytes.Buffer, n *cmdNode, name string) {
	desc := n.help.long
	if desc == "" {
		desc = n.short
	}
	if desc != "" {
		b.WriteString(desc + "\n\n")
	}
	writeUsage(b, n)
	if len(n.help.aliases) > 0 {
		b.WriteString("Aliases:")
		for _, a := range n.help.aliases {
			fmt.Fprintf(b, " `%s`", a)
		}
		b.WriteString("\n\n")
	}
	writeFlagsTable(b, parseFlagLines(n.help.flags))
	if n.help.examples != "" {
		fmt.Fprintf(b, "```sh\n%s\n```\n\n", n.help.examples)
	}
	if len(n.subs) > 0 {
		b.WriteString("| Subcommand | Description |\n|------------|-------------|\n")
		for _, l := range leaves(n) {
			rel := strings.Join(l.path[len(n.path):], " ")
			fmt.Fprintf(b, "| [`%s`](#%s) | %s |\n", rel, anchor(name, l.path), escapeCell(l.short))
		}
		b.WriteString("\n")
	}
}

// writeUsage prints the usage line(s), replacing cobra's "[flags]" with the
// flags themselves when there are few of them, or dropping it when there are
// none.
func writeUsage(b *bytes.Buffer, n *cmdNode) {
	if len(n.help.usages) == 0 {
		return
	}
	var compact []string
	for _, f := range parseFlagLines(n.help.flags) {
		compact = append(compact, f.compact)
	}
	b.WriteString("```sh\n")
	for _, u := range n.help.usages {
		if rest, ok := strings.CutSuffix(u, " [flags]"); ok {
			switch {
			case len(compact) == 0:
				u = rest // only --help, which every command has
			case len(compact) <= 4:
				u = rest + " " + strings.Join(compact, " ")
			}
		}
		b.WriteString(u + "\n")
	}
	b.WriteString("```\n\n")
}

// leaves returns the commands under n that have no subcommands of their own,
// depth first. Intermediate groups are flattened into their children.
func leaves(n *cmdNode) []*cmdNode {
	if len(n.subs) == 0 {
		return []*cmdNode{n}
	}
	var out []*cmdNode
	for _, s := range n.subs {
		out = append(out, leaves(s)...)
	}
	return out
}

// anchor reproduces GitHub's heading anchor for "### `bin a b`".
func anchor(name string, path []string) string {
	s := strings.ToLower(name + " " + strings.Join(path, " "))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ----- Project layout -------------------------------------------------------

// defaultSkip applies when [docs.layout].skip is not set.
var defaultSkip = []string{"testdata", "vendor", "dist", "bin", "node_modules"}

func genLayout(root string, rootFiles, skip []string, maxDepth int, notes map[string]string) string {
	if skip == nil {
		skip = defaultSkip
	}
	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}
	keepFile := map[string]bool{}
	for _, f := range rootFiles {
		keepFile[f] = true
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}

	type entry struct {
		rel   string
		isDir bool
		desc  string
	}
	var entries []entry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if skipSet[name] || skipSet[rel] || strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			// Only the configured root files; everything else is summarised
			// by its directory.
			if !strings.Contains(rel, "/") && keepFile[name] {
				entries = append(entries, entry{rel: rel, desc: notes[rel]})
			}
			return nil
		}
		if strings.Count(rel, "/") >= maxDepth {
			return filepath.SkipDir
		}
		desc := notes[rel]
		if desc == "" {
			desc = pkgDoc(path)
		}
		entries = append(entries, entry{rel: rel, isDir: true, desc: desc})
		return nil
	})
	if err != nil {
		fatalf("walk: %v", err)
	}

	width := 0
	for _, e := range entries {
		if w := len(label(e.rel, e.isDir)); w > width {
			width = w
		}
	}
	width += 2

	var b bytes.Buffer
	b.WriteString("```\n")
	for _, e := range entries {
		l := label(e.rel, e.isDir)
		if e.desc != "" {
			fmt.Fprintf(&b, "%-*s%s\n", width, l, e.desc)
		} else {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	b.WriteString("```")
	return b.String()
}

// label renders a tree entry: indented base name, "/" for directories.
func label(rel string, isDir bool) string {
	s := strings.Repeat("  ", strings.Count(rel, "/")) + filepath.Base(rel)
	if isDir {
		s += "/"
	}
	return s
}

// pkgDoc returns the first sentence of the package doc comment of the Go
// package in dir, or "". It parses the files directly instead of running
// "go doc", so it needs no build and handles main packages.
func pkgDoc(dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	sort.Strings(files)
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil || af.Doc == nil {
			continue
		}
		return synopsis(af.Doc.Text(), af.Name.Name)
	}
	return ""
}

// synopsis trims a package comment to its first sentence, drops the
// conventional "Package" / "Command" prefix, and caps the length, so
// "Package store provides a database." becomes "store provides a database".
func synopsis(text, pkg string) string {
	joined := strings.Join(strings.Fields(text), " ")
	if i := strings.Index(joined, ". "); i >= 0 {
		joined = joined[:i+1]
	}
	joined = strings.TrimSuffix(joined, ".")
	switch {
	case strings.HasPrefix(joined, "Package main "):
		// "Package main generates x" reads best as "Generates x".
		joined = strings.TrimPrefix(joined, "Package main ")
		if r := []rune(joined); len(r) > 0 {
			joined = strings.ToUpper(string(r[0])) + string(r[1:])
		}
	case strings.HasPrefix(joined, "Package "), strings.HasPrefix(joined, "Command "):
		// Keep the identifier as written: "store provides a database".
		_, joined, _ = strings.Cut(joined, " ")
	}
	const max = 72
	if r := []rune(joined); len(r) > max {
		cut := string(r[:max])
		if i := strings.LastIndex(cut, " "); i > max/2 {
			cut = cut[:i]
		}
		joined = cut + "…"
	}
	return joined
}

// ----- utilities ------------------------------------------------------------

const configFile = "docsmith.toml"

// findRoot walks up from the working directory to the first directory that
// holds docsmith.toml.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, configFile)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(configFile + " not found in this directory or any parent")
		}
		dir = parent
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gendocs: "+format+"\n", args...)
	os.Exit(1)
}
