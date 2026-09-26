// Package main generates executable documentation snippets.
//
// It reads every .txtar file in [examples].dir of docsmith.toml (default
// testdata/examples/), takes its display.sh section and its "# doc:KEY value"
// headers, and injects the result into the documentation file named by
// doc:file between
//
//	<!-- gendocs:example:ID --> … <!-- /gendocs:example:ID -->
//
// markers. When freeze (github.com/charmbracelet/freeze) is on PATH the
// snippet is rendered as dark and light terminal SVGs with a title bar and a
// "✓ CI tested" footer, shown through a <picture> element that links back to
// the .txtar; without freeze it falls back to a fenced code block.
//
// Recognised headers:
//
//	# doc:id      example-id        (required) marker and SVG name
//	# doc:file    README.md         (required) file that holds the markers
//	# doc:tested  true              the script also runs under testscript
//	# doc:lang    bash              language of the code-block fallback
//
// Rendered SVGs are committed. Each one records a hash of the snippet it
// shows, so an edited display.sh is re-rendered, and a stale SVG is never
// reused: without freeze the example drops back to a code block, which makes
// the drift check fail until someone re-renders it.
//
// Run with:
//
//	go run ./internal/tools/genexamples          # render missing or stale SVGs
//	go run ./internal/tools/genexamples -force   # re-render every SVG
//	make examples
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rogpeppe/go-internal/txtar"
)

// config is the subset of docsmith.toml this tool reads.
type config struct {
	Project struct {
		Binary string `toml:"binary"`
	} `toml:"project"`
	Examples struct {
		Dir    string  `toml:"dir"`
		SVGDir string  `toml:"svg_dir"`
		Width  float64 `toml:"width"`
	} `toml:"examples"`
}

// settings is the resolved configuration for one run.
type settings struct {
	root   string
	binary string
	dir    string // absolute examples dir
	svgDir string // absolute SVG output dir
	width  float64
	force  bool
}

func main() {
	force := flag.Bool("force", false, "re-render every SVG, even when it is up to date")
	flag.Parse()

	s, err := load()
	if err == nil {
		s.force = *force
		err = run(s)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genexamples:", err)
		os.Exit(1)
	}
}

func load() (settings, error) {
	root, err := findRoot()
	if err != nil {
		return settings{}, err
	}
	var cfg config
	if _, err := toml.DecodeFile(filepath.Join(root, configFile), &cfg); err != nil {
		return settings{}, err
	}
	s := settings{
		root:   root,
		binary: cfg.Project.Binary,
		dir:    orDefault(cfg.Examples.Dir, "testdata/examples"),
		svgDir: orDefault(cfg.Examples.SVGDir, "docs/assets/examples"),
		width:  cfg.Examples.Width,
	}
	s.dir = filepath.Join(root, filepath.FromSlash(s.dir))
	s.svgDir = filepath.Join(root, filepath.FromSlash(s.svgDir))
	if s.width <= 0 {
		s.width = 760
	}
	return s, nil
}

// ── txtar parsing ─────────────────────────────────────────────────────────────

// parseExample returns the doc:* headers from the txtar comment (the script
// area before the first file) and the contents of its display.sh file.
func parseExample(data []byte) (meta map[string]string, display string) {
	ar := txtar.Parse(data)
	meta = map[string]string{}
	for _, line := range strings.Split(string(ar.Comment), "\n") {
		rest, ok := strings.CutPrefix(line, "# doc:")
		if !ok {
			continue
		}
		key, val, _ := strings.Cut(rest, " ")
		meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	for _, f := range ar.Files {
		if f.Name == "display.sh" {
			display = strings.TrimRight(string(f.Data), "\n")
		}
	}
	return meta, display
}

// ── freeze rendering ──────────────────────────────────────────────────────────

// freezePath is the binary name looked up in PATH.
var freezePath = "freeze"

// stampPrefix starts the comment that records which snippet an SVG shows.
const stampPrefix = "<!-- docsmith:sha256:"

// renderOrUse returns the paths, relative to docDir, of the dark and light
// SVGs for one example, rendering them when needed. It returns "", "" when
// no usable SVGs exist and freeze is unavailable, so the caller falls back to
// a code block.
//
// The stamp covers everything that shows up in the image, so changing the
// snippet, the title or the footer re-renders it.
func renderOrUse(s settings, id, content, docDir, txtarPath string, tested bool) (darkRel, lightRel string) {
	darkAbs := filepath.Join(s.svgDir, id+"-dark.svg")
	lightAbs := filepath.Join(s.svgDir, id+"-light.svg")
	stamp := stampFor(id, content, filepath.Base(txtarPath), tested, s.width)

	rel := func() (string, string) {
		d, _ := filepath.Rel(docDir, darkAbs)
		l, _ := filepath.Rel(docDir, lightAbs)
		return filepath.ToSlash(d), filepath.ToSlash(l)
	}

	if !s.force && hasStamp(darkAbs, stamp) && hasStamp(lightAbs, stamp) {
		return rel()
	}

	freezeBin, err := exec.LookPath(freezePath)
	if err != nil {
		if fileExists(darkAbs) {
			fmt.Fprintf(os.Stderr, "  warn: %s is out of date and freeze is not installed; using a code block\n", filepath.Base(darkAbs))
		}
		return "", ""
	}
	if err := os.MkdirAll(s.svgDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: mkdir %s: %v\n", s.svgDir, err)
		return "", ""
	}

	for _, v := range []struct {
		theme  string
		dst    string
		isDark bool
	}{
		{"github-dark", darkAbs, true},
		{"github", lightAbs, false},
	} {
		if err := runFreeze(freezeBin, v.theme, id, content, v.dst, v.isDark, txtarPath, tested, s.width, stamp); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: freeze %s: %v\n", v.dst, err)
			return "", ""
		}
		r, _ := filepath.Rel(s.root, v.dst)
		fmt.Fprintf(os.Stderr, "  rendered %s\n", filepath.ToSlash(r))
	}
	return rel()
}

func stampFor(id, content, txtarName string, tested bool, width float64) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%t\x00%.0f\x00%s", id, txtarName, tested, width, content)
	return stampPrefix + hex.EncodeToString(h.Sum(nil))[:16] + " -->"
}

func hasStamp(path, stamp string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), stamp)
}

// runFreeze pipes content into freeze, writes the SVG to dst, then adds the
// title bar, the CI footer and the stamp.
func runFreeze(bin, theme, id, content, dst string, isDark bool, txtarPath string, tested bool, width float64, stamp string) error {
	cmd := exec.Command(bin, // #nosec G204 -- bin comes from exec.LookPath on a fixed name
		"--language", "bash",
		"--window",
		"--border.radius", "8",
		"--width", strconv.FormatFloat(width, 'f', 0, 64),
		"--theme", theme,
		"--output", dst,
	)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		return fmt.Errorf("read svg %s: %w", dst, err)
	}
	svg, err := addWindowTitle(string(raw), id, isDark)
	if err != nil {
		return err
	}
	svg = addCIFooter(svg, txtarPath, isDark, tested)
	// The stamp goes right after the root element so hasStamp finds it
	// without parsing the (large, font-embedding) document.
	svg = rootOpen.ReplaceAllString(svg, "$0\n"+stamp)
	return os.WriteFile(dst, []byte(svg), 0o644)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var (
	// rootOpen matches the root <svg …> start tag (the first one).
	rootOpen = regexp.MustCompile(`(?s)\A.*?<svg\b[^>]*>`)
	// rootWidth and rootHeight read the root element's size.
	rootWidth  = regexp.MustCompile(`<svg\b[^>]*?\bwidth="([0-9.]+)`)
	rootHeight = regexp.MustCompile(`(<svg\b[^>]*?\bheight=")([0-9.]+)(")`)
	// windowRect is the rounded window background freeze draws first.
	windowRect = regexp.MustCompile(`<rect\b[^>]*\brx="[0-9.]+"[^>]*/>`)
	// lights matches the traffic-light circles of the window controls.
	lights = regexp.MustCompile(`<circle\b[^>]*/>`)
	// fillAttr is the fill attribute of the window background.
	fillAttr = regexp.MustCompile(`fill="#[0-9a-fA-F]{3,8}"`)
)

const (
	svgFooterHeight = 36.0
	titlebarH       = 24.0
)

// addWindowTitle remaps the freeze background colour to the GitHub code-block
// palette, injects a distinct titlebar band with a separator, and centres the
// example-id label inside the titlebar. The SVG must come from freeze
// --window (traffic-light circles centred at y=12).
func addWindowTitle(svg, id string, isDark bool) (string, error) {
	bgColor := "#161b22"       // GitHub dark  — code-block bg
	titlebarColor := "#21262d" // GitHub dark  — elevated surface
	titleColor := "#8b949e"    // GitHub dark  — muted fg
	sepColor := "#30363d"      // GitHub dark  — border
	if !isDark {
		bgColor = "#f6f8fa"       // GitHub light — code-block bg
		titlebarColor = "#eaeef2" // GitHub light — elevated surface
		titleColor = "#57606a"    // GitHub light — muted fg
		sepColor = "#d0d7de"      // GitHub light — border
	}

	loc := windowRect.FindStringIndex(svg)
	if loc == nil {
		return "", errors.New("unexpected freeze output: no window background rect")
	}
	bg := fillAttr.ReplaceAllString(svg[loc[0]:loc[1]], `fill="`+bgColor+`"`)

	svgW := sizeOf(rootWidth, svg, 760)
	chrome := fmt.Sprintf(
		// Full rounded rect then a square-cornered cover strip = rounded-top-only.
		`<rect x="0.00" y="0.00" width="%.2f" height="%.2f" rx="8.00" ry="8.00" fill="%s"/>`+
			`<rect x="0.00" y="%.2f" width="%.2f" height="8.00" fill="%s"/>`+
			`<line x1="0.00" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="0.50"/>`,
		svgW, titlebarH, titlebarColor,
		titlebarH-8, svgW, titlebarColor,
		titlebarH, svgW, titlebarH, sepColor)
	svg = svg[:loc[0]] + bg + chrome + svg[loc[1]:]

	// Centre the title after the last traffic light so it draws on top.
	label := fmt.Sprintf(
		`<text x="%.2f" y="12.00" font-family="JetBrains Mono" font-size="11" `+
			`fill="%s" text-anchor="middle" dominant-baseline="central">%s</text>`,
		svgW/2, titleColor, strings.ReplaceAll(id, "-", " "))
	all := lights.FindAllStringIndex(svg, -1)
	if len(all) == 0 {
		return "", errors.New("unexpected freeze output: no window controls")
	}
	last := all[len(all)-1]
	return svg[:last[1]] + label + svg[last[1]:], nil
}

// addCIFooter extends the SVG canvas downward and appends a footer bar that
// shows the .txtar source filename and a ✓ CI tested badge when tested=true.
// The window background is stretched to cover the footer so the rounded
// bottom corners stay consistent.
func addCIFooter(svg, txtarPath string, isDark, tested bool) string {
	svgW := sizeOf(rootWidth, svg, 760)
	svgH := sizeOf(rootHeight, svg, 0)
	if svgH == 0 {
		return svg
	}
	newH := svgH + svgFooterHeight

	// Extend the root height, then the window background (the first element
	// that spans the full canvas).
	svg = rootHeight.ReplaceAllString(svg, fmt.Sprintf("${1}%.2f${3}", newH))
	bgOld := fmt.Sprintf(`width="%.2f" height="%.2f"`, svgW, svgH)
	bgNew := fmt.Sprintf(`width="%.2f" height="%.2f"`, svgW, newH)
	svg = strings.Replace(svg, bgOld, bgNew, 1)

	dividerColor := "#30363d"
	textColor := "#8b949e"
	if !isDark {
		dividerColor = "#d0d7de"
		textColor = "#57606a"
	}

	footerText := filepath.Base(txtarPath)
	if tested {
		footerText = "✓ CI tested · " + footerText
	}
	elements := fmt.Sprintf(
		"\n<line x1=\"12.00\" y1=\"%.2f\" x2=\"%.2f\" y2=\"%.2f\" stroke=\"%s\" stroke-width=\"0.50\"/>"+
			"\n<text x=\"%.2f\" y=\"%.2f\" font-family=\"JetBrains Mono\" font-size=\"11\" fill=\"%s\" text-anchor=\"middle\" dominant-baseline=\"central\">%s</text>",
		svgH, svgW-12, svgH, dividerColor,
		svgW/2, svgH+svgFooterHeight/2, textColor, footerText)

	i := strings.LastIndex(svg, "</svg>")
	if i < 0 {
		return svg
	}
	return svg[:i] + elements + "\n" + svg[i:]
}

// sizeOf reads a numeric attribute captured by re (group 1 for width, group 2
// for the three-group height pattern), or returns def.
func sizeOf(re *regexp.Regexp, svg string, def float64) float64 {
	m := re.FindStringSubmatch(svg)
	if m == nil {
		return def
	}
	v, err := strconv.ParseFloat(m[len(m)/2], 64)
	if err != nil {
		return def
	}
	return v
}

// pictureBlock builds a centred <picture> element for dark/light SVG
// switching, wrapped in a link to the txtar source file.
func pictureBlock(binary, id, darkRel, lightRel, txtarRel string) string {
	alt := strings.TrimSpace(binary + " " + strings.ReplaceAll(id, "-", " "))
	return fmt.Sprintf(`<div align="center">
<a href="%s"><picture>
  <source media="(prefers-color-scheme: dark)"  srcset="%s">
  <source media="(prefers-color-scheme: light)" srcset="%s">
  <img src="%s" alt="%s example" />
</picture></a>
</div>`, txtarRel, darkRel, lightRel, darkRel, alt)
}

// ── doc injection ─────────────────────────────────────────────────────────────

// inject replaces the content between the <!-- gendocs:example:ID --> and
// <!-- /gendocs:example:ID --> marker lines in the file at path.
func inject(path, id, content string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	open := "<!-- gendocs:example:" + id + " -->"
	end := "<!-- /gendocs:example:" + id + " -->"
	updated, err := replaceSection(string(raw), open, end, content)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return os.WriteFile(path, []byte(updated), 0o644)
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

// ── main pipeline ─────────────────────────────────────────────────────────────

type injection struct {
	id      string
	lang    string
	display string
	txtar   string // absolute path of the source .txtar file
	tested  bool   // doc:tested true → CI attribution
}

func run(s settings) error {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.txtar"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "  no .txtar files in %s\n", s.dir)
		return nil
	}
	sort.Strings(files)

	// Group injections by target file so we can report per file.
	byFile := map[string][]injection{}
	seen := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		meta, display := parseExample(data)
		id, docFile := meta["id"], meta["file"]
		if id == "" || docFile == "" {
			continue // not a documentation example
		}
		if prev, ok := seen[id]; ok {
			return fmt.Errorf("doc:id %q is used by both %s and %s", id, filepath.Base(prev), filepath.Base(f))
		}
		seen[id] = f
		if display == "" {
			fmt.Fprintf(os.Stderr, "  skip %-30s no display.sh section\n", filepath.Base(f))
			continue
		}
		// freeze clips rather than wraps: 14px JetBrains Mono advances 8.4px
		// per character, inside 20px of padding on each side.
		if limit := int((s.width - 40) / 8.4); limit > 0 {
			for i, line := range strings.Split(display, "\n") {
				if n := len([]rune(line)); n > limit {
					fmt.Fprintf(os.Stderr, "  warn: %s display.sh:%d is %d characters; lines over %d are cut off\n",
						filepath.Base(f), i+1, n, limit)
				}
			}
		}
		abs := filepath.Join(s.root, filepath.FromSlash(docFile))
		byFile[abs] = append(byFile[abs], injection{
			id:      id,
			lang:    orDefault(meta["lang"], "bash"),
			display: display,
			txtar:   f,
			tested:  strings.EqualFold(meta["tested"], "true"),
		})
	}

	docs := make([]string, 0, len(byFile))
	for f := range byFile {
		docs = append(docs, f)
	}
	sort.Strings(docs)

	for _, absFile := range docs {
		docDir := filepath.Dir(absFile)
		for _, inj := range byFile[absFile] {
			// Relative to the doc so the link resolves on GitHub.
			relTxtar, _ := filepath.Rel(docDir, inj.txtar)
			relTxtar = filepath.ToSlash(relTxtar)

			var block string
			darkRel, lightRel := renderOrUse(s, inj.id, inj.display, docDir, inj.txtar, inj.tested)
			if darkRel != "" {
				// The link and CI badge are baked into the image.
				block = pictureBlock(s.binary, inj.id, darkRel, lightRel, relTxtar)
			} else {
				label := "source"
				if inj.tested {
					label = "✓ CI-tested"
				}
				block = fmt.Sprintf("```%s\n%s\n```\n<p align=\"right\"><sub>%s · <a href=\"%s\"><code>%s</code></a></sub></p>",
					inj.lang, inj.display, label, relTxtar, filepath.Base(inj.txtar))
			}
			if err := inject(absFile, inj.id, block); err != nil {
				return fmt.Errorf("inject %s: %w", inj.id, err)
			}
		}
		rel, _ := filepath.Rel(s.root, absFile)
		fmt.Fprintf(os.Stderr, "  updated %-30s [%d example(s)]\n", filepath.ToSlash(rel), len(byFile[absFile]))
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

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

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
