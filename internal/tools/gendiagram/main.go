// Package main generates architecture diagram SVGs from TOML definitions.
// It reads every *.toml file in [diagrams].source_dir of docsmith.toml
// (default docs/diagrams/) and writes dark + light SVG variants to
// [diagrams].output_dir (default docs/assets/diagrams/).
//
// A diagram is a list of nodes (styled source, stage, sink or planned),
// edges and optional groups. Positions are computed from the edges unless
// the file sets width/height, in which case every node supplies its own x/y.
//
// Run via:
//
//	go run ./internal/tools/gendiagram
//	make diagrams
package main

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// ── Typography ───────────────────────────────────────────────────────────────

const (
	fontMono = "ui-monospace,'Cascadia Code','Source Code Pro',Menlo,Consolas,monospace"
	fontSans = "-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif"
)

// ── Themes ───────────────────────────────────────────────────────────────────

// Theme holds all colour tokens for one render pass.
type Theme struct {
	Name string

	// Canvas
	CanvasBG string

	// Outline nodes (source / sink)
	CardBG   string
	Border   string
	NodeText string

	// Stage (accent) nodes
	StageFill   string
	StageBorder string
	StageText   string
	StageDot    string

	// Planned / future nodes
	PlannedFill   string
	PlannedBorder string
	PlannedText   string

	// Groups
	GroupBG     string
	GroupBorder string
	GroupText   string

	// Edges
	EdgeColor   string
	PlannedEdge string

	// Misc
	TitleText string
	MutedText string
}

var dark = Theme{
	Name:          "dark",
	CanvasBG:      "#0d1117",
	CardBG:        "#161b22",
	Border:        "#30363d",
	NodeText:      "#c9d1d9",
	StageFill:     "#2d1a08",
	StageBorder:   "#f0883e",
	StageText:     "#f0883e",
	StageDot:      "#f0883e",
	PlannedFill:   "#0d1117",
	PlannedBorder: "#30363d",
	PlannedText:   "#8b949e",
	GroupBG:       "#161b22",
	GroupBorder:   "#21262d",
	GroupText:     "#8b949e",
	EdgeColor:     "#484f58",
	PlannedEdge:   "#30363d",
	TitleText:     "#e6edf3",
	MutedText:     "#8b949e",
}

var light = Theme{
	Name:          "light",
	CanvasBG:      "#ffffff",
	CardBG:        "#f6f8fa",
	Border:        "#d0d7de",
	NodeText:      "#1f2328",
	StageFill:     "#fff0e6",
	StageBorder:   "#bc4c00",
	StageText:     "#bc4c00",
	StageDot:      "#bc4c00",
	PlannedFill:   "#ffffff",
	PlannedBorder: "#d0d7de",
	PlannedText:   "#57606a",
	GroupBG:       "#f6f8fa",
	GroupBorder:   "#d0d7de",
	GroupText:     "#57606a",
	EdgeColor:     "#c6cdd5",
	PlannedEdge:   "#d0d7de",
	TitleText:     "#1f2328",
	MutedText:     "#57606a",
}

// ── TOML definition ──────────────────────────────────────────────────────────

// Def is the top-level TOML structure for a diagram definition.
type Def struct {
	Title     string     `toml:"title"`
	Direction string     `toml:"direction"` // "LR" (default) | "TB"
	Width     float64    `toml:"width"`     // explicit canvas width; when set, nodes must supply x/y
	Height    float64    `toml:"height"`    // explicit canvas height
	Scale     float64    `toml:"scale"`     // uniform output scale multiplier (default 1.0)
	ShowGrid  bool       `toml:"show_grid"` // render debug grid lines (column/row anchors); never set in production
	Nodes     []NodeDef  `toml:"nodes"`
	Edges     []EdgeDef  `toml:"edges"`
	Groups    []GroupDef `toml:"groups"`
}

// NodeDef describes a single node.
type NodeDef struct {
	ID    string  `toml:"id"`
	Label string  `toml:"label"` // use \n for multi-line (TOML literal: "A\nB")
	Style string  `toml:"style"` // source | stage | sink | planned
	X     float64 `toml:"x"`     // explicit position (used when Def.Width > 0)
	Y     float64 `toml:"y"`
	W     float64 `toml:"w"` // explicit width; 0 → autoW(label)
}

// EdgeDef describes a directed connection.
type EdgeDef struct {
	From  string `toml:"from"`
	To    string `toml:"to"`
	Label string `toml:"label"`
	Style string `toml:"style"` // "" (solid) | "dashed"
}

// GroupDef describes a visual containment box.
type GroupDef struct {
	ID       string   `toml:"id"`
	Label    string   `toml:"label"`
	Contains []string `toml:"contains"`
}

// ── Layout constants ─────────────────────────────────────────────────────────

const (
	hGap      = 22.0 // horizontal gap between layers
	vGap      = 10.0 // vertical gap between nodes in the same layer
	groupPadX = 14.0 // horizontal padding inside a group box
	groupPadY = 12.0 // vertical padding inside a group box
	groupLblH = 18.0 // extra height at top of group for the label
	marginX   = 18.0 // outer horizontal canvas margin
	titleH    = 34.0 // height reserved for the title at the top
	marginY   = 14.0 // outer vertical canvas margin (top of work area, bottom)
	nodeRx    = 14.0 // border-radius for pill / stadium nodes
	charW     = 5.6  // approximate monospace char width at 10 px
	nodePadX  = 26.0 // total horizontal padding inside a node (both sides)
	nodeMinW  = 56.0 // minimum node width
	nodeH     = 30.0 // fixed node height (single-line)
)

// ── Layout data structures ───────────────────────────────────────────────────

type lNode struct {
	def     *NodeDef
	layer   int
	x, y    float64
	w, h    float64
	groupID string
	origIdx int // index in def.Nodes (for stable sort)
}

type lEdge struct {
	def      *EdgeDef
	from, to *lNode
	// bypass: route below the main flow (long cross-layer edges)
	bypass bool
}

type lGroup struct {
	def        *GroupDef
	nodes      []*lNode
	x, y, w, h float64
}

type layout struct {
	def    *Def
	nodes  []*lNode
	edges  []*lEdge
	groups []*lGroup
	width  float64
	height float64
}

// ── Node sizing ──────────────────────────────────────────────────────────────

func autoW(label string) float64 {
	maxLen := 0
	for _, line := range strings.Split(label, "\n") {
		if len(line) > maxLen {
			maxLen = len(line)
		}
	}
	w := math.Ceil(float64(maxLen)*charW + nodePadX)
	if w < nodeMinW {
		w = nodeMinW
	}
	return math.Ceil(w/4) * 4
}

// ── Topological sort (Kahn) ──────────────────────────────────────────────────

func topoSort(def *Def) []string {
	adj := map[string][]string{}
	indeg := map[string]int{}
	for _, n := range def.Nodes {
		adj[n.ID] = nil
		indeg[n.ID] = 0
	}
	for _, e := range def.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}
	var queue []string
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	var result []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		result = append(result, cur)
		sort.Strings(adj[cur])
		for _, next := range adj[cur] {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
				sort.Strings(queue)
			}
		}
	}
	return result
}

// ── Layout computation ───────────────────────────────────────────────────────

func compute(def *Def) *layout {
	if def.Width > 0 {
		return computeExplicit(def)
	}
	return computeAuto(def)
}

// computeExplicit uses the x/y/w values supplied directly in the TOML.
// Groups are still derived from member bounding boxes (no need to specify them).
func computeExplicit(def *Def) *layout {
	byID := map[string]*lNode{}
	for i := range def.Nodes {
		nd := &def.Nodes[i]
		w := nd.W
		if w == 0 {
			w = autoW(nd.Label)
		}
		byID[nd.ID] = &lNode{def: nd, x: nd.X, y: nd.Y, w: w, h: nodeH, origIdx: i}
	}

	// Build group membership
	groupMembers := map[string][]*lNode{}
	for i := range def.Groups {
		g := &def.Groups[i]
		for _, id := range g.Contains {
			if ln, ok := byID[id]; ok {
				ln.groupID = g.ID
				groupMembers[g.ID] = append(groupMembers[g.ID], ln)
			}
		}
	}

	// Uniform width within each group (use the widest member)
	for _, members := range groupMembers {
		maxW := 0.0
		for _, n := range members {
			if n.w > maxW {
				maxW = n.w
			}
		}
		for _, n := range members {
			n.w = maxW
		}
	}

	groups := buildGroups(def, byID, groupMembers)

	// Edges — bypass when the horizontal span is large (> 2× a typical node gap)
	var edges []*lEdge
	for i := range def.Edges {
		ed := &def.Edges[i]
		from, ok1 := byID[ed.From]
		to, ok2 := byID[ed.To]
		if !ok1 || !ok2 {
			continue
		}
		// Bypass only when source is far left AND below the target — routing
		// below the canvas avoids the edge crossing the stage area. When the
		// source is above the target the bezier curves down naturally.
		bypass := (to.x-from.x-from.w) > 200 && (from.y+from.h/2) > (to.y+to.h/2)
		edges = append(edges, &lEdge{def: ed, from: from, to: to, bypass: bypass})
	}

	// Stable node order: by TOML definition index
	allNodes := make([]*lNode, len(def.Nodes))
	for i := range def.Nodes {
		allNodes[i] = byID[def.Nodes[i].ID]
	}

	return &layout{
		def:    def,
		nodes:  allNodes,
		edges:  edges,
		groups: groups,
		width:  def.Width,
		height: def.Height,
	}
}

// computeAuto assigns layers topologically and positions nodes automatically.
func computeAuto(def *Def) *layout {
	// Build node map
	byID := map[string]*lNode{}
	for i := range def.Nodes {
		nd := &def.Nodes[i]
		byID[nd.ID] = &lNode{def: nd, layer: 0, w: autoW(nd.Label), h: nodeH, origIdx: i}
	}

	// Build group membership
	nodeGroup := map[string]string{}
	groupMembers := map[string][]*lNode{}
	for i := range def.Groups {
		g := &def.Groups[i]
		for _, id := range g.Contains {
			if ln, ok := byID[id]; ok {
				nodeGroup[id] = g.ID
				ln.groupID = g.ID
				groupMembers[g.ID] = append(groupMembers[g.ID], ln)
			}
		}
	}

	// Uniform width within each group
	for _, members := range groupMembers {
		maxW := nodeMinW
		for _, n := range members {
			if n.w > maxW {
				maxW = n.w
			}
		}
		for _, n := range members {
			n.w = maxW
		}
	}

	// Assign layers via longest-path (topological order)
	layers := map[string]int{}
	outgoing := map[string][]string{}
	for _, e := range def.Edges {
		outgoing[e.From] = append(outgoing[e.From], e.To)
	}
	for _, id := range topoSort(def) {
		for _, to := range outgoing[id] {
			if layers[to] < layers[id]+1 {
				layers[to] = layers[id] + 1
			}
		}
	}
	for id, ln := range byID {
		ln.layer = layers[id]
	}

	// Force same layer for nodes in the same group (use the minimum)
	for _, members := range groupMembers {
		minL := members[0].layer
		for _, n := range members[1:] {
			if n.layer < minL {
				minL = n.layer
			}
		}
		for _, n := range members {
			n.layer = minL
		}
	}

	// Bucket nodes by layer
	byLayer := map[int][]*lNode{}
	maxLayer := 0
	for _, ln := range byID {
		byLayer[ln.layer] = append(byLayer[ln.layer], ln)
		if ln.layer > maxLayer {
			maxLayer = ln.layer
		}
	}

	// Sort within each layer: non-planned before planned, TOML definition order within each
	for l, ns := range byLayer {
		sort.SliceStable(ns, func(i, j int) bool {
			gi, gj := nodeGroup[ns[i].def.ID], nodeGroup[ns[j].def.ID]
			if gi != gj {
				return gi > gj
			}
			pi, pj := ns[i].def.Style == "planned", ns[j].def.Style == "planned"
			if pi != pj {
				return !pi
			}
			return ns[i].origIdx < ns[j].origIdx
		})
		byLayer[l] = ns
	}

	// Compute layer widths (max node width per layer)
	layerW := make([]float64, maxLayer+1)
	for l := 0; l <= maxLayer; l++ {
		for _, n := range byLayer[l] {
			if n.w > layerW[l] {
				layerW[l] = n.w
			}
		}
	}

	// Compute layer X positions
	layerX := make([]float64, maxLayer+1)
	x := marginX
	for l := 0; l <= maxLayer; l++ {
		layerX[l] = x
		x += layerW[l] + hGap
	}

	// Compute per-layer total heights
	layerTH := make([]float64, maxLayer+1)
	for l := 0; l <= maxLayer; l++ {
		ns := byLayer[l]
		h := float64(len(ns))*nodeH + float64(len(ns)-1)*vGap
		layerTH[l] = h
	}
	maxTH := 0.0
	for _, h := range layerTH {
		if h > maxTH {
			maxTH = h
		}
	}

	canvasH := titleH + marginY + maxTH + marginY
	canvasW := x - hGap + marginX

	// Position nodes
	for l := 0; l <= maxLayer; l++ {
		ns := byLayer[l]
		startY := titleH + marginY + (maxTH-layerTH[l])/2
		for _, n := range ns {
			n.x = layerX[l]
			n.y = startY
			startY += nodeH + vGap
		}
	}

	groups := buildGroups(def, byID, groupMembers)
	for _, g := range groups {
		if g.x+g.w+marginX > canvasW {
			canvasW = g.x + g.w + marginX
		}
		if g.y < titleH {
			shift := titleH - g.y + marginY/2
			for _, n := range byID {
				n.y += shift
			}
			for _, gg := range groups {
				gg.y += shift
			}
			canvasH += shift
		}
		if g.y+g.h+marginY > canvasH {
			canvasH = g.y + g.h + marginY
		}
	}

	// Build edges
	var edges []*lEdge
	for i := range def.Edges {
		ed := &def.Edges[i]
		from, ok1 := byID[ed.From]
		to, ok2 := byID[ed.To]
		if !ok1 || !ok2 {
			continue
		}
		bypass := (to.layer - from.layer) > 2
		edges = append(edges, &lEdge{def: ed, from: from, to: to, bypass: bypass})
	}

	// Collect all nodes in stable order (layer, then original index)
	var allNodes []*lNode
	for l := 0; l <= maxLayer; l++ {
		allNodes = append(allNodes, byLayer[l]...)
	}

	return &layout{
		def:    def,
		nodes:  allNodes,
		edges:  edges,
		groups: groups,
		width:  math.Ceil(canvasW),
		height: math.Ceil(canvasH),
	}
}

// buildGroups computes group bounding boxes from member node positions.
func buildGroups(def *Def, byID map[string]*lNode, groupMembers map[string][]*lNode) []*lGroup {
	var groups []*lGroup
	for i := range def.Groups {
		gd := &def.Groups[i]
		members := groupMembers[gd.ID]
		if len(members) == 0 {
			continue
		}
		minX, minY := members[0].x, members[0].y
		maxX, maxY2 := members[0].x+members[0].w, members[0].y+members[0].h
		for _, n := range members[1:] {
			if n.x < minX {
				minX = n.x
			}
			if n.y < minY {
				minY = n.y
			}
			if n.x+n.w > maxX {
				maxX = n.x + n.w
			}
			if n.y+n.h > maxY2 {
				maxY2 = n.y + n.h
			}
		}
		groups = append(groups, &lGroup{
			def:   gd,
			nodes: members,
			x:     minX - groupPadX,
			y:     minY - groupPadY - groupLblH,
			w:     maxX - minX + 2*groupPadX,
			h:     maxY2 - minY + 2*groupPadY + groupLblH,
		})
	}
	return groups
}

// ── SVG rendering ────────────────────────────────────────────────────────────

func renderSVG(lay *layout, t Theme) string {
	var b bytes.Buffer
	ln := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	W, H := lay.width, lay.height
	bypassY := H - marginY/2

	scale := lay.def.Scale
	if scale <= 0 {
		scale = 1.0
	}
	SW, SH := math.Ceil(W*scale), math.Ceil(H*scale)

	ln(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`, SW, SH, SW, SH)

	// ── Defs ──
	ln(`  <defs>`)
	// Arrowhead for solid edges.
	// refX="7" aligns the triangle tip (at x=7) to the path endpoint so the
	// arrowhead never overlaps the target node.
	ln(`    <marker id="arr" markerWidth="7" markerHeight="7" refX="7" refY="2.5" orient="auto">`)
	ln(`      <path d="M0,0 L0,5 L7,2.5 z" fill="%s"/>`, t.EdgeColor)
	ln(`    </marker>`)
	// Arrowhead for dashed/planned edges
	ln(`    <marker id="arr-d" markerWidth="7" markerHeight="7" refX="7" refY="2.5" orient="auto">`)
	ln(`      <path d="M0,0 L0,5 L7,2.5 z" fill="%s"/>`, t.PlannedEdge)
	ln(`    </marker>`)
	// Subtle dot texture for stage nodes
	ln(`    <pattern id="dots-d" patternUnits="userSpaceOnUse" width="9" height="9">`)
	ln(`      <circle cx="4.5" cy="4.5" r="1" fill="%s" opacity="0.18"/>`, t.StageDot)
	ln(`    </pattern>`)
	ln(`    <pattern id="dots-l" patternUnits="userSpaceOnUse" width="9" height="9">`)
	ln(`      <circle cx="4.5" cy="4.5" r="1" fill="%s" opacity="0.12"/>`, t.StageDot)
	ln(`    </pattern>`)
	dotPat := "dots-d"
	if t.Name == "light" {
		dotPat = "dots-l"
	}
	ln(`  </defs>`)

	// ── Content group (scaled) ──
	if scale != 1.0 {
		ln(`  <g transform="scale(%.4g)">`, scale)
	}

	// ── Background ──
	ln(`  <rect width="%.0f" height="%.0f" fill="%s" rx="10"/>`, W, H, t.CanvasBG)

	// ── Title ──
	ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="12" font-weight="600" fill="%s">%s</text>`,
		marginX+4, titleH-10, fontSans, t.TitleText, html.EscapeString(lay.def.Title))

	// ── Debug grid (optional, never committed with show_grid=true) ──
	// Vertical lines at each unique node left-edge x; horizontal lines at each
	// unique node center-y.  Useful during layout work to verify alignment.
	if lay.def.ShowGrid {
		colSet := map[float64]bool{}
		rowSet := map[float64]bool{}
		for _, n := range lay.nodes {
			colSet[n.x] = true
			rowSet[n.y+n.h/2] = true
		}
		var cols, rows []float64
		for x := range colSet {
			cols = append(cols, x)
		}
		for y := range rowSet {
			rows = append(rows, y)
		}
		sort.Float64s(cols)
		sort.Float64s(rows)
		gridColor := t.MutedText
		for _, x := range cols {
			ln(`  <line x1="%.1f" y1="0" x2="%.1f" y2="%.0f" stroke="%s" stroke-width="0.5" opacity="0.18"/>`,
				x, x, H, gridColor)
		}
		for _, y := range rows {
			ln(`  <line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" stroke="%s" stroke-width="0.5" opacity="0.18"/>`,
				y, W, y, gridColor)
		}
	}

	// ── Groups (behind edges and nodes) ──
	for _, g := range lay.groups {
		ln(`  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" stroke="%s" stroke-width="1" rx="8" opacity="0.85"/>`,
			g.x, g.y, g.w, g.h, t.GroupBG, t.GroupBorder)
		ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="10" fill="%s">%s</text>`,
			g.x+groupPadX, g.y+groupLblH-4, fontSans, t.GroupText, html.EscapeString(g.def.Label))
	}

	// ── Edges (behind nodes) ──
	for _, e := range lay.edges {
		isDashed := e.def.Style == "dashed"
		strokeColor := t.EdgeColor
		markerID := "arr"
		if isDashed {
			strokeColor = t.PlannedEdge
			markerID = "arr-d"
		}
		dashAttr := ""
		if isDashed {
			dashAttr = ` stroke-dasharray="5,3"`
		}

		fx := e.from.x + e.from.w
		fy := e.from.y + e.from.h/2
		// 3 px gap so the arrowhead tip (refX=7) lands just before the node border.
		tx := e.to.x - 3
		ty := e.to.y + e.to.h/2

		var pathD string
		if e.bypass {
			// Orthogonal bypass path routed below the main flow
			corner := 6.0
			exitX := fx + 10
			entryX := tx - 10
			p0y := bypassY
			pathD = fmt.Sprintf(
				"M%.1f,%.1f L%.1f,%.1f Q%.1f,%.1f %.1f,%.1f L%.1f,%.1f Q%.1f,%.1f %.1f,%.1f L%.1f,%.1f Q%.1f,%.1f %.1f,%.1f L%.1f,%.1f Q%.1f,%.1f %.1f,%.1f",
				fx, fy, exitX-corner, fy,
				exitX, fy, exitX, fy+corner,
				exitX, p0y-corner,
				exitX, p0y, exitX+corner, p0y,
				entryX-corner, p0y,
				entryX, p0y, entryX, p0y-corner,
				entryX, ty+corner,
				entryX, ty, tx, ty,
			)
		} else {
			// Cubic bezier S-curve using cubic-bezier(1,-0.01,0,1.01) mapping.
			// CP1 = (tx, fy) and CP2 = (fx, ty) — the "crossing corners" of the
			// bounding box.  Both tangents are horizontal; dx/dt ≥ 0 everywhere.
			ddy := ty - fy
			c1y := fy - 0.01*ddy
			c2y := ty + 0.01*ddy
			pathD = fmt.Sprintf("M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f",
				fx, fy, tx, c1y, fx, c2y, tx, ty)
		}

		ln(`  <path d="%s" fill="none" stroke="%s" stroke-width="1.5"%s marker-end="url(#%s)"/>`,
			pathD, strokeColor, dashAttr, markerID)

		// Edge label
		if e.def.Label != "" {
			lblX := (fx + tx) / 2
			lblY := (fy+ty)/2 - 5
			if e.bypass {
				lblY = bypassY - 5
			}
			ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="9" fill="%s" text-anchor="middle">%s</text>`,
				lblX, lblY, fontSans, t.MutedText, html.EscapeString(e.def.Label))
		}
	}

	// ── Nodes ──
	for _, n := range lay.nodes {
		rx := math.Min(nodeRx, n.h/2)
		cx := n.x + n.w/2
		cy := n.y + n.h/2
		lblY := cy + 3.5 // approximate text baseline for vertically centered 10px text

		switch n.def.Style {
		case "stage":
			// Accent fill + dot texture overlay
			ln(`  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" stroke="%s" stroke-width="1.5" rx="%.0f"/>`,
				n.x, n.y, n.w, n.h, t.StageFill, t.StageBorder, rx)
			ln(`  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="url(#%s)" rx="%.0f"/>`,
				n.x, n.y, n.w, n.h, dotPat, rx)
			ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="10" font-weight="700" fill="%s" text-anchor="middle" letter-spacing="0.6">%s</text>`,
				cx, lblY, fontMono, t.StageText, html.EscapeString(n.def.Label))

		case "planned":
			ln(`  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" stroke="%s" stroke-width="1" stroke-dasharray="4,2.5" rx="%.0f"/>`,
				n.x, n.y, n.w, n.h, t.PlannedFill, t.PlannedBorder, rx)
			ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="10" font-weight="600" fill="%s" text-anchor="middle" letter-spacing="0.4">%s</text>`,
				cx, lblY, fontMono, t.PlannedText, html.EscapeString(n.def.Label))

		default: // source, sink
			ln(`  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" stroke="%s" stroke-width="1" rx="%.0f"/>`,
				n.x, n.y, n.w, n.h, t.CardBG, t.Border, rx)
			ln(`  <text x="%.1f" y="%.1f" font-family="%s" font-size="10" font-weight="600" fill="%s" text-anchor="middle" letter-spacing="0.4">%s</text>`,
				cx, lblY, fontMono, t.NodeText, html.EscapeString(n.def.Label))
		}
	}

	if scale != 1.0 {
		ln(`  </g>`)
	}
	ln(`</svg>`)
	return b.String()
}

// ── Main ─────────────────────────────────────────────────────────────────────

// config is the subset of docsmith.toml this tool reads.
type config struct {
	Diagrams struct {
		SourceDir string `toml:"source_dir"`
		OutputDir string `toml:"output_dir"`
	} `toml:"diagrams"`
}

func main() {
	root, err := findRoot()
	must(err, "find repo root")

	var cfg config
	_, err = toml.DecodeFile(filepath.Join(root, configFile), &cfg)
	must(err, "read "+configFile)
	srcDir, outRel := cfg.Diagrams.SourceDir, cfg.Diagrams.OutputDir
	if srcDir == "" {
		srcDir = "docs/diagrams"
	}
	if outRel == "" {
		outRel = "docs/assets/diagrams"
	}

	inDir := filepath.Join(root, filepath.FromSlash(srcDir))
	outDir := filepath.Join(root, filepath.FromSlash(outRel))

	entries, err := filepath.Glob(filepath.Join(inDir, "*.toml"))
	must(err, "glob diagrams")
	if len(entries) == 0 {
		fmt.Printf("  no *.toml files found in %s/\n", srcDir)
		return
	}
	must(os.MkdirAll(outDir, 0o755), "create output dir")

	for _, tomlPath := range entries {
		base := strings.TrimSuffix(filepath.Base(tomlPath), ".toml")

		var def Def
		if _, err := toml.DecodeFile(tomlPath, &def); err != nil {
			fmt.Fprintf(os.Stderr, "gendiagram: parse %s: %v\n", relPath(root, tomlPath), err)
			os.Exit(1)
		}
		must(check(&def), relPath(root, tomlPath))

		lay := compute(&def)

		for _, theme := range []Theme{dark, light} {
			svg := renderSVG(lay, theme)
			out := filepath.Join(outDir, fmt.Sprintf("%s-%s.svg", base, theme.Name))
			must(os.WriteFile(out, []byte(svg), 0o644), "write "+out)
			fmt.Printf("  wrote %s\n", relPath(root, out))
		}
	}
}

// check rejects edges and groups that name unknown nodes, which would
// otherwise be dropped from the output without a word.
func check(def *Def) error {
	ids := map[string]bool{}
	for _, n := range def.Nodes {
		if ids[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		ids[n.ID] = true
	}
	for _, e := range def.Edges {
		for _, id := range []string{e.From, e.To} {
			if !ids[id] {
				return fmt.Errorf("edge %s -> %s: unknown node %q", e.From, e.To, id)
			}
		}
	}
	for _, g := range def.Groups {
		for _, id := range g.Contains {
			if !ids[id] {
				return fmt.Errorf("group %q: unknown node %q", g.ID, id)
			}
		}
	}
	return nil
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

func relPath(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendiagram: %s: %v\n", ctx, err)
		os.Exit(1)
	}
}
