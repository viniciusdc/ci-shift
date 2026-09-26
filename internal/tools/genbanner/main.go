// Package main generates the README banner SVGs from docsmith.toml. It
// writes banner-dark.svg and banner-light.svg from the [banner] table.
//
// The left half is the project title with an accent underline and a subtitle;
// the right half is a decorative motif, either "blocks" (stacked rows of
// rounded blocks) or "lanes" (parallel lanes with connectors that shift
// between them). Layout is fixed; text, colours and motif geometry come from
// the config, so the output is reproducible byte for byte.
//
// Run via:
//
//	go run ./internal/tools/genbanner
//	make banner
package main

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ── Canvas ───────────────────────────────────────────────────────────────────

const (
	canvasW = 860
	canvasH = 160
)

// ── Typography (layout only — colours come from Theme) ───────────────────────

const (
	titleX       = 52
	titleY       = 96
	maxTitleSize = 68.0

	accentLineX1 = 52
	accentLineY  = 114
	accentLineW  = 2

	subtitleX       = 54
	subtitleY       = 136
	maxSubtitleSize = 14.0

	// Approximate advance widths, as a fraction of the font size, used to
	// size the title and underline without measuring glyphs.
	monoAdvance = 0.60
	sansAdvance = 0.52

	fontMono = "ui-monospace,'Cascadia Code','Source Code Pro',Menlo,Consolas,monospace"
	fontSans = "-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif"
)

// ── Motif panel ──────────────────────────────────────────────────────────────

const (
	panelX    = 458
	dividerX  = panelX - 18
	textRoom  = dividerX - 24 - titleX // horizontal room for the title/subtitle
	rowH      = 14
	rowGap    = 8
	rowR      = 4
	fadeX     = 700
	fadeW     = canvasW - fadeX
	stackTopY = (canvasH - (5*rowH + 4*rowGap)) / 2 // = 29
)

// ── Config ───────────────────────────────────────────────────────────────────

// config is the subset of docsmith.toml this tool reads.
type config struct {
	Project struct {
		Name    string `toml:"name"`
		Tagline string `toml:"tagline"`
	} `toml:"project"`
	Banner bannerConfig `toml:"banner"`
}

type bannerConfig struct {
	Title     string      `toml:"title"`
	Subtitle  string      `toml:"subtitle"`
	OutputDir string      `toml:"output_dir"`
	Motif     string      `toml:"motif"`
	Dark      themeConfig `toml:"dark"`
	Light     themeConfig `toml:"light"`
	Rows      []rowConfig `toml:"rows"`
	Lanes     int         `toml:"lanes"`
	Shifts    []shift     `toml:"shifts"`
}

type themeConfig struct {
	Accent  string   `toml:"accent"`
	Palette []string `toml:"palette"`
}

// rowConfig is one row of the "blocks" motif. Color indexes the theme
// palette; when omitted the row index is used.
type rowConfig struct {
	Color  *int        `toml:"color"`
	Blocks [][]float64 `toml:"blocks"` // [x, width, opacity]
}

// shift is one connector of the "lanes" motif.
type shift struct {
	From int     `toml:"from"`
	To   int     `toml:"to"`
	X    float64 `toml:"x"`
}

// ── Theme ─────────────────────────────────────────────────────────────────────

// Theme holds every colour used in the banner so dark and light variants share
// identical layout code.
type Theme struct {
	name string

	bg      string // canvas background (right)
	bgCard  string // canvas background (left, slightly lighter/darker)
	title   string // title text
	sub     string // subtitle text
	divider string // vertical separator + horizontal rule
	accent  string // underline, dot, glow

	palette []string // motif colours, cycled
}

var dark = Theme{
	name:    "dark",
	bg:      "#0d1117",
	bgCard:  "#161b22",
	title:   "#e6edf3",
	sub:     "#8b949e",
	divider: "#e6edf3",
	accent:  "#58a6ff",
	palette: []string{"#58a6ff", "#3fb950", "#f0883e", "#bc8cff", "#2ea6a6"},
}

var light = Theme{
	name:    "light",
	bg:      "#ffffff",
	bgCard:  "#f6f8fa",
	title:   "#1f2328",
	sub:     "#57606a",
	divider: "#1f2328",
	accent:  "#0969da",
	palette: []string{"#0969da", "#1a7f37", "#bc4c00", "#8250df", "#1b7c83"},
}

// withConfig returns t with the accent and palette overridden by c.
func (t Theme) withConfig(c themeConfig) Theme {
	if c.Accent != "" {
		t.accent = c.Accent
	}
	if len(c.Palette) > 0 {
		t.palette = c.Palette
	}
	return t
}

func (t Theme) color(i int) string {
	return t.palette[((i%len(t.palette))+len(t.palette))%len(t.palette)]
}

// ── Default motif geometry ───────────────────────────────────────────────────

// defaultRows is the stacked-blocks motif: five rows of [x, width, opacity].
var defaultRows = [][][]float64{
	{{0, 130, 0.90}, {145, 72, 0.60}, {232, 105, 0.75}},
	{{0, 52, 0.85}, {68, 138, 0.65}, {222, 90, 0.50}},
	{{0, 88, 0.80}, {104, 48, 0.60}, {168, 118, 0.72}},
	{{0, 28, 0.90}, {44, 28, 0.65}, {172, 38, 0.75}, {226, 28, 0.55}},
	{{0, 105, 0.50}, {120, 62, 0.75}, {198, 84, 0.60}},
}

const defaultLanes = 5

// defaultShifts bend a few lanes into others, left to right.
var defaultShifts = []shift{
	{From: 0, To: 2, X: 52},
	{From: 4, To: 1, X: 136},
	{From: 2, To: 4, X: 222},
}

// ── SVG builder ───────────────────────────────────────────────────────────────

type writer struct{ bytes.Buffer }

func (w *writer) line(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

func generate(b bannerConfig, t Theme) string {
	var w writer

	w.line(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`,
		canvasW, canvasH, canvasW, canvasH)

	// ── defs ─────────────────────────────────────────────────────────────────
	w.line(`  <defs>`)
	w.line(`    <linearGradient id="bgGrad" x1="0" y1="0" x2="1" y2="0">`)
	w.line(`      <stop offset="0%%"   stop-color="%s"/>`, t.bgCard)
	w.line(`      <stop offset="100%%" stop-color="%s"/>`, t.bg)
	w.line(`    </linearGradient>`)
	w.line(`    <linearGradient id="fadeGrad" x1="0" y1="0" x2="1" y2="0">`)
	w.line(`      <stop offset="0%%"   stop-color="%s" stop-opacity="0"/>`, t.bg)
	w.line(`      <stop offset="100%%" stop-color="%s" stop-opacity="1"/>`, t.bg)
	w.line(`    </linearGradient>`)
	w.line(`    <filter id="glow" x="-5%%" y="-20%%" width="110%%" height="140%%">`)
	w.line(`      <feGaussianBlur in="SourceAlpha" stdDeviation="4" result="blur"/>`)
	w.line(`      <feFlood flood-color="%s" flood-opacity="0.25" result="clr"/>`, t.accent)
	w.line(`      <feComposite in="clr" in2="blur" operator="in" result="glow"/>`)
	w.line(`      <feMerge><feMergeNode in="glow"/><feMergeNode in="SourceGraphic"/></feMerge>`)
	w.line(`    </filter>`)
	w.line(`  </defs>`)

	// ── Background ───────────────────────────────────────────────────────────
	w.line(`  <rect width="%d" height="%d" fill="url(#bgGrad)"/>`, canvasW, canvasH)
	w.line(`  <line x1="0" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="1" opacity="0.08"/>`,
		accentLineY, canvasW, accentLineY, t.divider)

	// ── Motif ────────────────────────────────────────────────────────────────
	switch b.Motif {
	case "lanes":
		drawLanes(&w, b, t)
	default:
		drawBlocks(&w, b, t)
	}
	w.line(`  <rect x="%d" y="0" width="%d" height="%d" fill="url(#fadeGrad)"/>`, fadeX, fadeW, canvasH)
	w.line(`  <line x1="%d" y1="20" x2="%d" y2="%d" stroke="%s" stroke-width="1" opacity="0.12"/>`,
		dividerX, dividerX, canvasH-20, t.divider)

	// ── Title ────────────────────────────────────────────────────────────────
	// Shrink long titles and subtitles so they never cross the divider.
	titleSize := math.Min(maxTitleSize, textRoom/(monoAdvance*float64(runeLen(b.Title))))
	subSize := math.Min(maxSubtitleSize, textRoom/(sansAdvance*float64(runeLen(b.Subtitle))))
	titleEnd := titleX + monoAdvance*titleSize*float64(runeLen(b.Title))
	subEnd := subtitleX + sansAdvance*subSize*float64(runeLen(b.Subtitle))
	lineEnd := math.Min(math.Max(titleEnd, subEnd)+16, dividerX-24)

	w.line(`  <text x="%d" y="%d" font-family="%s" font-size="%.1f" font-weight="700" letter-spacing="-1" fill="%s" filter="url(#glow)">%s</text>`,
		titleX, titleY, fontMono, titleSize, t.title, html.EscapeString(b.Title))
	w.line(`  <line x1="%d" y1="%d" x2="%.0f" y2="%d" stroke="%s" stroke-width="%d" stroke-linecap="round"/>`,
		accentLineX1, accentLineY, lineEnd, accentLineY, t.accent, accentLineW)
	w.line(`  <circle cx="%.0f" cy="%d" r="3" fill="%s"/>`, lineEnd+5, accentLineY, t.accent)

	// ── Subtitle ─────────────────────────────────────────────────────────────
	w.line(`  <text x="%d" y="%d" font-family="%s" font-size="%.1f" fill="%s" letter-spacing="0.5">%s</text>`,
		subtitleX, subtitleY, fontSans, subSize, t.sub, html.EscapeString(b.Subtitle))

	w.line(`</svg>`)
	return w.String()
}

// drawBlocks renders the stacked-blocks motif: rows of rounded blocks, one
// palette colour per row.
func drawBlocks(w *writer, b bannerConfig, t Theme) {
	rows := b.Rows
	if len(rows) == 0 {
		for _, blocks := range defaultRows {
			rows = append(rows, rowConfig{Blocks: blocks})
		}
	}
	// Centre the stack vertically whatever the row count.
	top := (canvasH - (len(rows)*rowH + (len(rows)-1)*rowGap)) / 2
	for i, row := range rows {
		color := i
		if row.Color != nil {
			color = *row.Color
		}
		y := top + i*(rowH+rowGap)
		for _, blk := range row.Blocks {
			if len(blk) < 2 {
				continue
			}
			alpha := 0.75
			if len(blk) >= 3 {
				alpha = blk[2]
			}
			w.line(`  <rect x="%.0f" y="%d" width="%.0f" height="%d" rx="%d" fill="%s" opacity="%.2f"/>`,
				panelX+blk[0], y, blk[1], rowH, rowR, t.color(color), alpha)
		}
	}
}

// drawLanes renders the lanes motif: parallel lanes running right, each ending
// in an arrowhead, with curved connectors that shift from one lane to another.
func drawLanes(w *writer, b bannerConfig, t Theme) {
	n := b.Lanes
	if n <= 0 {
		n = defaultLanes
	}
	shifts := b.Shifts
	if len(shifts) == 0 && b.Lanes <= 0 {
		shifts = defaultShifts
	}

	const (
		laneStartX = panelX + 4
		arrowX     = canvasW - 72 // arrowheads fade out with the right edge
		span       = 5*rowH + 4*rowGap
		pillW      = 26
		shiftW     = 56 // horizontal run of one shift connector
	)
	gap := 0.0
	if n > 1 {
		gap = float64(span-rowH) / float64(n-1)
	}
	laneY := func(i int) float64 { return float64(stackTopY+rowH/2) + float64(i)*gap }

	// Shift endpoints per lane, so stage markers can keep clear of them.
	var valid []shift
	ends := map[int][]float64{}
	for _, s := range shifts {
		if s.From < 0 || s.From >= n || s.To < 0 || s.To >= n || s.From == s.To {
			continue
		}
		valid = append(valid, s)
		x1 := float64(laneStartX) + s.X
		ends[s.From] = append(ends[s.From], x1)
		ends[s.To] = append(ends[s.To], x1+shiftW)
	}
	isClear := func(lane int, x float64) bool {
		for _, e := range ends[lane] {
			if x < e+8 && x+pillW > e-8 {
				return false
			}
		}
		return true
	}

	// Lanes: a faint track, stage markers, and an arrowhead.
	for i := 0; i < n; i++ {
		y := laneY(i)
		c := t.color(i)
		w.line(`  <line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-width="2" stroke-linecap="round" opacity="0.30"/>`,
			laneStartX, y, arrowX, y, c)
		// Stage markers, staggered per lane so the rows read as independent
		// pipelines rather than a grid.
		for k := 0; k < 3; k++ {
			x := float64(laneStartX) + 12 + float64(k)*86 + float64((i*29)%37)
			if !isClear(i, x) {
				continue
			}
			w.line(`  <rect x="%.1f" y="%.1f" width="%d" height="10" rx="5" fill="%s" opacity="%.2f"/>`,
				x, y-5, pillW, c, 0.55+0.12*float64((i+k)%3))
		}
		w.line(`  <path d="M%d,%.1f l-9,-5.5 v11 z" fill="%s" opacity="0.9"/>`, arrowX+9, y, c)
	}

	// Shifts: an S-curve from lane From at X to lane To, drawn over the lanes.
	for _, s := range valid {
		x1 := float64(laneStartX) + s.X
		x2 := x1 + shiftW
		y1, y2 := laneY(s.From), laneY(s.To)
		c := t.color(s.From)
		w.line(`  <path d="M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="none" stroke="%s" stroke-width="2.5" stroke-linecap="round" opacity="0.95"/>`,
			x1, y1, x1+32, y1, x2-32, y2, x2, y2, c)
		w.line(`  <circle cx="%.1f" cy="%.1f" r="3.5" fill="%s"/>`, x1, y1, c)
		w.line(`  <circle cx="%.1f" cy="%.1f" r="3.5" fill="%s" stroke="%s" stroke-width="1.5"/>`, x2, y2, t.bg, c)
	}
}

func runeLen(s string) int {
	n := len([]rune(s))
	if n == 0 {
		return 1
	}
	return n
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	root, err := findRoot()
	if err != nil {
		fatal(err)
	}
	var cfg config
	if _, err := toml.DecodeFile(filepath.Join(root, configFile), &cfg); err != nil {
		fatal(err)
	}
	b := cfg.Banner
	if b.Title == "" {
		b.Title = cfg.Project.Name
	}
	if b.Subtitle == "" {
		b.Subtitle = cfg.Project.Tagline
	}
	if b.OutputDir == "" {
		b.OutputDir = "docs/assets"
	}
	if b.Motif != "" && b.Motif != "blocks" && b.Motif != "lanes" {
		fatal(fmt.Errorf("banner.motif %q: want \"blocks\" or \"lanes\"", b.Motif))
	}

	outDir := filepath.Join(root, filepath.FromSlash(b.OutputDir))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}
	for _, t := range []Theme{dark.withConfig(b.Dark), light.withConfig(b.Light)} {
		out := filepath.Join(outDir, "banner-"+t.name+".svg")
		if err := os.WriteFile(out, []byte(generate(b, t)), 0o644); err != nil {
			fatal(err)
		}
		rel, _ := filepath.Rel(root, out)
		fmt.Printf("  wrote %s\n", filepath.ToSlash(rel))
	}
}

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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genbanner:", err)
	os.Exit(1)
}
