package rsghttp

import (
	"context"
	"strconv"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/evidence"
	"github.com/lichman0405/post/internal/rsg/provenance"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// The object detail page's two graph tabs (T0507): the PROVENANCE graph and
// the EVIDENCE graph, rendered on the same page, kept apart by construction.
//
// docs/10 §1 is the rule this file exists to hold: "Provenance 回答'从哪里来、
// 谁做了什么'。Evidence 回答'为什么这个证据与某个科学命题相关'。两者可以共享
// 节点但关系语义必须分开。" (CLAUDE.md §9 invariant 10). The two are
// different tables read through different services with different relation
// vocabularies — provenance edges come from the rebuildable provenance_edges
// projection and carry RSG relation types with a declared origin side
// (internal/rsg/provenance); evidence assertions come from evidence_assertions
// and carry one of the nine evidence relations plus the stance label the
// domain derives (internal/evidence). Nothing here re-derives either read
// model: the page renders what those two reads returned, and when a read
// fails it renders nothing at all.
//
// Three properties every renderer below holds, and the tests hold them to:
//
//  1. NO FABRICATED SEMANTICS. A relation's human label is the relation
//     catalog's own one-line Semantics (internal/rsg/relationcatalog, the Go
//     copy of docs/44 and docs/19). A relation the catalog does not carry is
//     rendered with NO semantics text — never with a sentence this file
//     invented, and never with the raw type repeated under a heading that
//     promises its meaning. That is the same fail-closed direction the
//     evidence stance rule takes (domain.RelationStance answers ok=false and
//     the row keeps no label).
//  2. NO ARITHMETIC. No count, ratio, total, weight or net position is
//     computed here or shown on the page: docs/10 §4 「V1 不自动赋数值权重」
//     and CLAUDE.md §9.13. The page does not even print "3 edges" — the list
//     shows the rows that exist and says so when there are none.
//  3. THE LIST IS THE CONTENT, THE DIAGRAM ILLUSTRATES IT. Every edge the SVG
//     draws carries the same identity (data-edge-id) as one row of the table
//     beside it (data-row-edge-id), and the SVG is aria-hidden: a keyboard or
//     screen-reader reader gets the whole graph from semantic HTML (docs/06 §:
//     "Graph 视图必须提供等价列表/outline"; docs/51: "Research Map 必须有
//     list/table fallback，不能只有 Canvas/graph").

// ProvenanceReader is the provenance-graph read the page renders (T0505).
// The production implementation is *provenancehttp.ProjectionStore: the same
// store the JSON graph/lineage routes read through, so the page and the API
// cannot disagree about a project's provenance.
type ProvenanceReader interface {
	// ListEdges returns the project's projected provenance edges.
	ListEdges(ctx context.Context, projectID string) ([]provenance.Edge, error)
	// ObjectStartNode resolves one version of one object as a graph node
	// (versionNo nil selects the latest version), enforcing the object's
	// membership in the path project before any version lookup.
	ObjectStartNode(ctx context.Context, objectID, projectID string, versionNo *int) (provenance.Node, error)
}

// EvidenceReader is the evidence-graph read the page renders (T0506). The
// production implementation is *evidencegraph.Service — the same read the
// evidence JSON routes serve, so the page's groups, stances and omissions are
// the projection's, not a second implementation of them.
//
// It takes the reader (ADR-024) for the same reason the JSON route does: the
// evidence rows the page may render depend on who is reading, and the
// T0106 gate the page already passed answered a different question — whether
// this reader may see the PROJECT at all. The page hands over the reader it
// resolved once for that gate, never a second one.
type EvidenceReader interface {
	// ObjectEvidence reads one object's evidence grouped by the target
	// version each assertion pins. A pinned version gets its group even when
	// it carries no assertions.
	ObjectEvidence(ctx context.Context, reader projects.Reader, projectID, objectID string, versionNo *int) (evidence.ObjectEvidence, error)
}

// The page's two graph identities. They are also the data-graph attribute
// values, the SVG marker suffixes and the section keys — one spelling per
// graph everywhere, so a test (or a stylesheet, or a reader) can never grab
// one graph's element by the other's name.
const (
	graphProvenance = "provenance"
	graphEvidence   = "evidence"
)

// Graph read outcomes as the page renders them. "ok" carries whatever the
// read returned (including nothing); "unavailable" carries no rows at all.
const (
	graphStateOK          = "ok"
	graphStateUnavailable = "unavailable"
)

// The sentences that tell the two graphs apart. They are the page's answer to
// the acceptance criterion 用户不把两图混淆, and each tab carries its own,
// naming the other: a reader who lands on one tab must be told what it is and
// what it is not without having to visit the other one first.
const (
	provenanceLead = "Provenance answers “where did this come from, and who did what”. " +
		"Each edge is an RSG relation between pinned object versions, labelled with the relation catalog's meaning."
	provenanceBoundary = "This is not the evidence graph. Evidence answers “why does this evidence bear on that " +
		"proposition”, from another table with other relation semantics (docs/10 §1)."
	evidenceLead = "Evidence answers “why does this evidence bear on that proposition”. Each row is an evidence " +
		"assertion pinned to this exact object version, labelled with the stance the domain rule derives from its relation."
	evidenceBoundary = "This is not the provenance graph. Provenance answers “where did this come from, and who did " +
		"what”, from another table with other relation semantics (docs/10 §1)."
)

// ---------------------------------------------------------------------------
// Drawing model
// ---------------------------------------------------------------------------

// graphNode is one drawn vertex: a pinned object version (provenance) or an
// evidence version (evidence). ID is the version id — the identity the read
// models use — and Key is the layout's own unique handle (two roles can name
// the same version; the picture still needs two boxes, and data-node-id still
// shows the version).
type graphNode struct {
	Key   string
	ID    string
	Label string
	Type  string
	Kind  string // "start", "origin", "dependent", "target", "evidence"
	X     int
	Y     int
	W     int
	H     int
}

// CX and CY are the box centre. The template has no arithmetic, so the centre
// is computed here rather than in the markup.
func (n graphNode) CX() int { return n.X + n.W/2 }
func (n graphNode) CY() int { return n.Y + n.H/2 }

// graphEdge is one drawn edge, carrying the identity of the row it
// illustrates: for provenance the relation version id, for evidence the
// assertion id. Stance is set for evidence edges only — a provenance edge has
// no stance, and the page never invents one for it.
type graphEdge struct {
	ID        string
	Relation  string
	Stance    string
	HasStance bool
	Tone      string // the stroke class
	X1, Y1    int
	X2, Y2    int
	LabelX    int
	LabelY    int
}

// graphDiagram is one rendered graph: the nodes, the edges and the viewBox
// they fit in. MarkerID namespaces the arrowhead so the two diagrams on the
// page cannot pick up each other's marker.
type graphDiagram struct {
	MarkerID string
	Width    int
	Height   int
	Nodes    []graphNode
	Edges    []graphEdge
}

// provenanceRow is one row of the provenance tab's list: one provenance edge
// as the projection returned it. SemanticsKnown is false for a relation type
// the catalog does not carry — the row then shows no meaning at all, which is
// why the template branches on it instead of printing an empty cell a reader
// would take for "no meaning recorded".
type provenanceRow struct {
	EdgeID         string
	Relation       string
	Semantics      string
	SemanticsKnown bool
	SourceLabel    string
	TargetLabel    string
}

// evidenceRow is one row of the evidence tab's list: one assertion exactly as
// internal/evidence projected it. StanceKnown is false for an assertion whose
// relation the domain rule does not map — rendered as no stance label, never
// as neutral (internal/evidence/projection.go's fail-closed rule).
type evidenceRow struct {
	AssertionID       string
	Relation          string
	Semantics         string
	SemanticsKnown    bool
	Stance            string
	StanceKnown       bool
	EvidenceType      string
	ReviewState       string
	EvidenceVersionID string
	CreatedAt         string
}

// provenancePanel is the provenance tab's model.
type provenancePanel struct {
	State           string
	Lead            string
	Boundary        string
	CounterHref     string
	Direction       string
	UpstreamHref    string
	DownHref        string
	UpstreamCurrent bool
	DownCurrent     bool
	DirectionNote   string
	Empty           string
	Unavailable     string
	Diagram         graphDiagram
	Rows            []provenanceRow
}

// evidencePanel is the evidence tab's model.
type evidencePanel struct {
	State       string
	Lead        string
	Boundary    string
	CounterHref string
	TargetLabel string
	Empty       string
	Unavailable string
	Diagram     graphDiagram
	Rows        []evidenceRow
}

// ---------------------------------------------------------------------------
// The provenance tab
// ---------------------------------------------------------------------------

// provenancePanelFor renders the provenance tab: the walk T0505 defines
// (lineage — upstream — by default, impact — downstream — on request) over the
// project's projected edges, starting from the exact version the page shows.
//
// A read that fails renders the unavailable state and NOTHING else. The page
// keeps its header, its tabs and the object's own facts — those came from a
// read that succeeded — but it says plainly that this graph could not be read
// rather than showing an empty one, which a reader would take for "this
// version has no provenance".
func provenancePanelFor(
	ctx context.Context,
	r ProvenanceReader,
	projectID, objectID string,
	versionNo *int,
	dir provenance.WalkDirection,
	tabHref func(tab, direction string) string,
) provenancePanel {
	panel := provenancePanel{
		State:           graphStateOK,
		Lead:            provenanceLead,
		Boundary:        provenanceBoundary,
		CounterHref:     tabHref(graphEvidence, ""),
		Direction:       dir.String(),
		UpstreamHref:    tabHref(graphProvenance, provenance.WalkUpstream.String()),
		DownHref:        tabHref(graphProvenance, provenance.WalkDownstream.String()),
		UpstreamCurrent: dir == provenance.WalkUpstream,
		DownCurrent:     dir == provenance.WalkDownstream,
		Empty: "No provenance edge recorded in this project reaches this version. " +
			"An empty answer is an answer: it says nothing is RECORDED, not that the version has no origin.",
		Unavailable: "The provenance graph could not be read. Nothing is shown here rather than an incomplete graph.",
	}
	if dir == provenance.WalkDownstream {
		panel.DirectionNote = "Dependents: the versions this one is an origin of — what a change here would affect."
	} else {
		panel.DirectionNote = "Origins: the versions this one came from."
	}
	if r == nil {
		// An unwired reader is a read that failed, not a licence to invent:
		// same state a store error produces.
		panel.State = graphStateUnavailable
		return panel
	}
	start, err := r.ObjectStartNode(ctx, objectID, projectID, versionNo)
	if err != nil {
		panel.State = graphStateUnavailable
		return panel
	}
	edges, err := r.ListEdges(ctx, projectID)
	if err != nil {
		panel.State = graphStateUnavailable
		return panel
	}
	walked := provenance.Walk(provenance.Build(edges), []provenance.Node{start}, dir, provenance.UnlimitedDepth)
	panel.Diagram = provenanceDiagram(walked, start, dir)
	for _, e := range walked.Edges {
		semantics, known := relationSemantics(e.RelationType)
		panel.Rows = append(panel.Rows, provenanceRow{
			EdgeID:         e.RelationVersionID,
			Relation:       e.RelationType,
			Semantics:      semantics,
			SemanticsKnown: known,
			SourceLabel:    nodeLabel(e.Source),
			TargetLabel:    nodeLabel(e.Target),
		})
	}
	return panel
}

// provenanceDiagram places the walked subgraph in hop layers: the start
// version in the first column, the versions one hop away in the second, and
// so on. Each edge is then drawn in its stored source → target direction and
// labelled with its relation type.
//
// The layers come from the canonical walk itself (max_depth 0, 1, 2, …)
// rather than from a second traversal written here, so the picture cannot
// disagree with the walk about which version is how far from the start.
func provenanceDiagram(g provenance.Graph, start provenance.Node, dir provenance.WalkDirection) graphDiagram {
	layers := [][]graphNode{{{
		Key: start.VersionID, ID: start.VersionID, Label: nodeLabel(start),
		Type: start.ObjectType, Kind: "start",
	}}}
	seen := map[string]bool{start.VersionID: true}
	for depth := 1; depth <= len(g.Nodes)+1; depth++ {
		reach := provenance.Walk(g, []provenance.Node{start}, dir, depth)
		var layer []graphNode
		for _, n := range reach.Nodes {
			if seen[n.VersionID] {
				continue
			}
			seen[n.VersionID] = true
			layer = append(layer, graphNode{
				Key: n.VersionID, ID: n.VersionID, Label: nodeLabel(n),
				Type: n.ObjectType, Kind: provenanceLayerKind(dir),
			})
		}
		if len(layer) == 0 {
			break
		}
		layers = append(layers, layer)
	}
	// The diagram draws the whole walked subgraph, so a node the layered scan
	// somehow missed would be a bug — it gets its own trailing layer rather
	// than being dropped (invariant 8: nothing disappears).
	var leftover []graphNode
	for _, n := range g.Nodes {
		if seen[n.VersionID] {
			continue
		}
		seen[n.VersionID] = true
		leftover = append(leftover, graphNode{
			Key: n.VersionID, ID: n.VersionID, Label: nodeLabel(n),
			Type: n.ObjectType, Kind: provenanceLayerKind(dir),
		})
	}
	if len(leftover) > 0 {
		layers = append(layers, leftover)
	}
	diagram := layoutLayers(graphProvenance, layers)
	diagram.Edges = connectEdges(diagram, g.Edges)
	return diagram
}

// provenanceLayerKind names what a non-start node is in this walk: an origin
// when the walk goes upstream, a dependent when it goes downstream.
func provenanceLayerKind(dir provenance.WalkDirection) string {
	if dir == provenance.WalkDownstream {
		return "dependent"
	}
	return "origin"
}

// connectEdges draws a raw provenance edge set over already-placed nodes. An
// edge whose endpoint is not among the drawn nodes is skipped: an edge cannot
// be drawn without both ends, and a half-edge would picture something that is
// not in the data. Edges keep the order the caller passed (the walk's
// deterministic order), so the picture is byte-stable.
func connectEdges(diagram graphDiagram, edges []provenance.Edge) []graphEdge {
	byKey := make(map[string]graphNode, len(diagram.Nodes))
	for _, n := range diagram.Nodes {
		byKey[n.Key] = n
	}
	rows := make([]graphEdge, 0, len(edges))
	for _, e := range edges {
		from, okFrom := byKey[e.Source.VersionID]
		to, okTo := byKey[e.Target.VersionID]
		if !okFrom || !okTo {
			continue
		}
		rows = append(rows, graphEdge{
			ID:       e.RelationVersionID,
			Relation: e.RelationType,
			Tone:     graphProvenance,
			X1:       from.X + from.W,
			Y1:       from.Y + from.H/2,
			X2:       to.X,
			Y2:       to.Y + to.H/2,
			LabelX:   (from.X + from.W + to.X) / 2,
			LabelY:   (from.Y+from.H/2+to.Y+to.H/2)/2 - 6,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// The evidence tab
// ---------------------------------------------------------------------------

// evidencePanelFor renders the evidence tab for the exact version the page
// shows: the read is pinned to that version, so the answer is about this
// version alone — "why believe or doubt THIS version".
//
// The reader is the page's own resolved caller (handlers.reader), handed to
// the SAME read the JSON route serves (ADR-024): which assertions a version
// carries depends on who is asking, and the tab must render the reader's set,
// not everyone's. It is not re-derived here and not consulted for anything
// else — this panel applies no rule of its own, so the page and the route
// cannot end up with two spellings of one audience.
//
// Both stances are rendered side by side, never merged and never netted: the
// projection hands back supporting and contesting as separate arrays
// (internal/evidence/projection.go), and the page walks all four buckets in
// the read model's own order, labelling every row with its own stance.
func evidencePanelFor(
	ctx context.Context,
	r EvidenceReader,
	reader projects.Reader,
	projectID, objectID string,
	versionNo *int,
	tabHref func(tab, direction string) string,
) evidencePanel {
	panel := evidencePanel{
		State:       graphStateOK,
		Lead:        evidenceLead,
		Boundary:    evidenceBoundary,
		CounterHref: tabHref(graphProvenance, ""),
		Empty: "No evidence assertion is recorded against this version. " +
			"An empty answer is an answer: it says nothing about whether evidence exists elsewhere.",
		Unavailable: "The evidence graph could not be read. Nothing is shown here rather than an incomplete graph.",
	}
	if r == nil {
		panel.State = graphStateUnavailable
		return panel
	}
	read, err := r.ObjectEvidence(ctx, reader, projectID, objectID, versionNo)
	if err != nil {
		panel.State = graphStateUnavailable
		return panel
	}
	for _, group := range read.Groups {
		if panel.TargetLabel == "" {
			panel.TargetLabel = groupTargetLabel(group.Target)
		}
		for _, assertion := range assertionsOf(group) {
			semantics, known := relationSemantics(assertion.Relation)
			panel.Rows = append(panel.Rows, evidenceRow{
				AssertionID:       assertion.ID,
				Relation:          assertion.Relation,
				Semantics:         semantics,
				SemanticsKnown:    known,
				Stance:            assertion.Stance,
				StanceKnown:       assertion.Stance != "",
				EvidenceType:      assertion.EvidenceType,
				ReviewState:       assertion.ReviewState,
				EvidenceVersionID: assertion.EvidenceObjectVersionID,
				CreatedAt:         assertion.CreatedAt,
			})
		}
	}
	panel.Diagram = evidenceDiagram(panel.Rows, panel.TargetLabel, read.Groups)
	return panel
}

// groupTargetLabel names the version a group is about: its title when the
// caller's version read resolved one, otherwise the pin itself. It never
// invents a title (internal/evidence leaves VersionNo unset for an unresolved
// pin for the same reason).
func groupTargetLabel(t evidence.Target) string {
	if strings.TrimSpace(t.Title) != "" {
		return t.Title
	}
	return t.ObjectVersionID
}

// assertionsOf walks one target group's four buckets in the read model's own
// order. The buckets are separate arrays by design — a supporting and a
// contesting assertion on the same pair are two rows, not one winner — so
// this is a concatenation of rows, never a merge of them: each row keeps its
// own bucket's stance, and a row with no stance keeps none.
func assertionsOf(g evidence.TargetGroup) []evidence.Assertion {
	rows := make([]evidence.Assertion, 0, len(g.Supporting)+len(g.Contesting)+len(g.Neutral)+len(g.Unlabeled))
	rows = append(rows, g.Supporting...)
	rows = append(rows, g.Contesting...)
	rows = append(rows, g.Neutral...)
	rows = append(rows, g.Unlabeled...)
	return rows
}

// evidenceDiagram draws the evidence graph as the star it is: every evidence
// version in the first column, the target version this page is about in the
// second, one edge per assertion pointing at the target it is about. Two
// assertions pinning the same pair — one supporting, one contesting — are two
// edges with different identities and different strokes, which is exactly
// what the schema refuses to collapse.
//
// Nodes keep first-seen order (the read's order is the stored order,
// created_at then id) and are deduplicated by (role, version id): two
// assertions citing one evidence version are two edges between one pair of
// boxes, not two boxes.
func evidenceDiagram(rows []evidenceRow, targetLabel string, groups []evidence.TargetGroup) graphDiagram {
	if len(groups) == 0 {
		return graphDiagram{MarkerID: graphEvidence}
	}
	targetKeys := make([]string, 0, len(groups))
	targetIDs := make([]string, 0, len(groups))
	for i, g := range groups {
		key := "target:" + g.Target.ObjectVersionID
		if g.Target.ObjectVersionID == "" {
			key = "target:#" + strconv.Itoa(i)
		}
		targetKeys = append(targetKeys, key)
		targetIDs = append(targetIDs, g.Target.ObjectVersionID)
	}
	// The first column: the evidence versions the rows cite, in first-seen
	// order. An assertion whose evidence version is empty (the store always
	// fills it, but a projection change must not silently drop a row) still
	// gets a node, because the row must be drawable and listed.
	evidenceKeys := map[string]string{}
	var left []graphNode
	for _, row := range rows {
		key := "evidence:" + row.EvidenceVersionID
		if _, seen := evidenceKeys[row.EvidenceVersionID]; !seen {
			evidenceKeys[row.EvidenceVersionID] = key
			left = append(left, graphNode{
				Key: key, ID: row.EvidenceVersionID, Label: row.EvidenceVersionID,
				Type: "evidence version", Kind: "evidence",
			})
		}
	}
	var right []graphNode
	for i, key := range targetKeys {
		label := targetLabel
		if i > 0 {
			label = targetIDs[i]
		}
		right = append(right, graphNode{
			Key: key, ID: targetIDs[i], Label: label, Type: "target version", Kind: "target",
		})
	}
	layers := make([][]graphNode, 0, 2)
	if len(left) > 0 {
		layers = append(layers, left)
	}
	layers = append(layers, right)
	diagram := layoutLayers(graphEvidence, layers)
	byKey := make(map[string]graphNode, len(diagram.Nodes))
	for _, n := range diagram.Nodes {
		byKey[n.Key] = n
	}
	// Each edge points at the box of the group its own assertion belongs to.
	// The page pins one version, so today that is the single box in the right
	// column — but the mapping is read from the read's own grouping rather
	// than assumed, so a read that answered several groups would draw each
	// assertion at the version it is about instead of stacking every edge on
	// the first one.
	targetOf := make(map[string]string, len(rows))
	for i, g := range groups {
		for _, assertion := range assertionsOf(g) {
			targetOf[assertion.ID] = targetKeys[i]
		}
	}
	edges := make([]graphEdge, 0, len(rows))
	for _, row := range rows {
		from, okFrom := byKey[evidenceKeys[row.EvidenceVersionID]]
		to, okTo := byKey[targetOf[row.AssertionID]]
		if !okFrom || !okTo {
			continue
		}
		edges = append(edges, graphEdge{
			ID:        row.AssertionID,
			Relation:  row.Relation,
			Stance:    row.Stance,
			HasStance: row.StanceKnown,
			Tone:      stanceTone(row.Stance),
			X1:        from.X + from.W,
			Y1:        from.Y + from.H/2,
			X2:        to.X,
			Y2:        to.Y + to.H/2,
			LabelX:    (from.X + from.W + to.X) / 2,
			LabelY:    (from.Y+from.H/2+to.Y+to.H/2)/2 - 6,
		})
	}
	diagram.Edges = edges
	return diagram
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// Layout constants: a rough monospace metric (labels are monospace in the
// page's stylesheet — every label here is a title, an id or a relation type),
// padding so a label never touches its box, and fixed gaps. The layout is
// deterministic — the same graph always renders the same bytes — so a test can
// pin it.
const (
	graphCharWidth = 7
	graphNodeH     = 42
	graphPadX      = 12
	graphMinNodeW  = 130
	graphColGap    = 96
	graphRowGap    = 22
	graphMargin    = 16
)

// layoutLayers places one layer per column: each column is as wide as its
// widest label and every column is centred on the tallest one, so an edge
// drawn between two columns is horizontal for nodes on the same row. Rows
// inside a column keep the order the caller passed — the read's order.
func layoutLayers(markerID string, layers [][]graphNode) graphDiagram {
	diagram := graphDiagram{MarkerID: markerID}
	colW := make([]int, len(layers))
	maxRows := 0
	for i, layer := range layers {
		w := graphMinNodeW
		for _, n := range layer {
			if got := runeLen(n.Label)*graphCharWidth + 2*graphPadX; got > w {
				w = got
			}
		}
		colW[i] = w
		if len(layer) > maxRows {
			maxRows = len(layer)
		}
	}
	height := 2*graphMargin + maxRows*graphNodeH + gapRows(maxRows)
	if maxRows == 0 {
		height = 2 * graphMargin
	}
	x := graphMargin
	for i, layer := range layers {
		total := len(layer)*graphNodeH + gapRows(len(layer))
		y := (height - total) / 2
		for _, n := range layer {
			n.X = x
			n.Y = y
			n.W = colW[i]
			n.H = graphNodeH
			diagram.Nodes = append(diagram.Nodes, n)
			y += graphNodeH + graphRowGap
		}
		x += colW[i] + graphColGap
	}
	width := x - graphColGap + graphMargin
	if len(layers) == 0 {
		width = 2 * graphMargin
	}
	diagram.Width = width
	diagram.Height = height
	return diagram
}

// gapRows is the vertical space between rows of a column: (n-1) gaps, never
// negative.
func gapRows(n int) int {
	if n < 2 {
		return 0
	}
	return (n - 1) * graphRowGap
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

// relationSemantics is the ONE place this surface turns a relation type into
// human words: the relation catalog's own one-line Semantics (docs/44,
// docs/19). ok is false for a type the catalog does not carry, and callers
// render no meaning for it — inventing a sentence, or repeating the raw type
// under a heading that promises its meaning, would be this page asserting a
// semantics nobody declared.
func relationSemantics(typ string) (string, bool) {
	entry, ok := relationcatalog.Lookup(typ)
	if !ok || strings.TrimSpace(entry.Semantics) == "" {
		return "", false
	}
	return entry.Semantics, true
}

// nodeLabel is a provenance node's display label: the version's title, or the
// version id when the projection carries no title — never a placeholder word
// that reads like a title.
func nodeLabel(n provenance.Node) string {
	if strings.TrimSpace(n.Title) != "" {
		return n.Title
	}
	return n.VersionID
}

// stanceTone maps the three stance labels — and the absence of one — to the
// stroke class the diagram uses. It is presentation only: the stance a row
// carries is always the label internal/evidence put on it, and the diagram
// never changes it. An assertion with no stance draws in the unlabeled tone,
// which the legend names "no stance label" rather than leaving a reader to
// guess that a fourth colour means "neutral".
func stanceTone(stance string) string {
	switch stance {
	case "supporting", "contesting", "neutral":
		return stance
	default:
		return "unlabeled"
	}
}

// runeLen counts display characters, not bytes: a label with a non-ASCII
// character must not be given a box sized for its UTF-8 length.
func runeLen(s string) int { return len([]rune(s)) }
