package rsghttp

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/lichman0405/post/internal/application/rsg"
)

// The Research Map (T1102): the middle column of the Research page — a coarse
// picture of the project's research state that a reader drills into one layer
// at a time, beside the outline list that carries every row and a panel
// naming the selected node (docs/42 §Research Page: 左侧 outline/question
// tree，中间 Research Map，右侧 selected node detail).
//
// docs/06 §5 is the rule this file exists to hold, verbatim: 「默认粗粒度，以
// Research Question → Hypothesis/Research Path → Finding 组织。节点数量控制；
// 默认聚合。点击逐层 drill-down 到 Claim/Evidence/Object。完整 RSG 不默认渲染
// 为蜘蛛网。支持视图：By Question（默认）、By Finding」. Three properties follow
// from it, and every renderer below holds them:
//
//  1. THE DEFAULT IS COARSE. What a reader first sees is ONE column of root
//     questions (By Question) or of findings (By Finding) — never one node
//     per object, never one node per relation, and nothing below a node until
//     that node is selected. A column draws at most researchMapColumnBudget
//     peers and gives the rest ONE remainder node carrying their count, so
//     the picture of a 1000-object state and the picture of a seed project
//     differ in their labels, not in their size: the drawn node count is
//     bounded by researchMapNodeBudget whatever the project holds, and the
//     tests hold a 1000-object outline to it. The map is a way INTO the
//     state; the outline list beside it is the state.
//
//  2. AGGREGATES ARE ROW COUNTS, NEVER WEIGHTS. A bucket node prints how many
//     rows it stands for ("40 rows"), because a box hiding 40 findings must
//     not look like one finding. That number is a count of rows the read
//     returned: nothing here computes a ratio, a percentage, a rank, a weight
//     or any score, and nothing is ordered by size (CLAUDE.md §9.13, docs/10
//     §4). Bucket order is fixed by docs/06 §5's own line, not by counts.
//
//  3. THE PICTURE ILLUSTRATES THE LIST. The SVG is aria-hidden and carries no
//     content of its own: every drawn node has the same identity
//     (data-map-node == data-map-row) as one row of the table beside it, and
//     every drawn edge is that row's relation column (data-map-edge-to names a
//     drawn node, data-map-edge-role repeats its row's role). Keyboard and
//     screen-reader readers therefore get the whole map from semantic HTML,
//     and every drill-down is a plain <a href> with a ?path= coordinate — no
//     canvas, no script, no client state (docs/06 §10, docs/51).
//
// The map adds no read. It is built from the same rsg.ResearchOutline the
// outline list is built from, so the map's "initial aggregated query" is the
// outline read docs/27 already budgets at p95 < 1s — one project-wide read —
// and the map's own work is bounded by the constants below rather than by the
// project's size (the default view looks nothing up at all).

// The map's granularity constants. They are the acceptance criteria written
// as numbers: the default view of any project, at any size, draws at most
// researchMapNodeBudget boxes.
const (
	// researchMapColumnBudget is how many peers one column draws before the
	// remainder node takes over.
	researchMapColumnBudget = 8
	// researchMapBucketBudget is how many buckets one focused node rolls up
	// into. It is the largest bucket list this file can produce: a question's
	// three object roles plus its sub-questions, or a finding's claims plus
	// the questions it addresses.
	researchMapBucketBudget = 4
	// researchMapDepth is how many drill-down steps the map itself answers.
	// The next step is the object detail page, which is where a claim's
	// provenance and evidence live (docs/06 §5: drill-down 到 Claim/Evidence/
	// Object): the third step lands on an object, and the page it links to
	// holds the rest.
	//
	// The cap is a contract in BOTH directions: researchMapPath never reads
	// more than this many elements, and no renderer may emit a coordinate with
	// more (panelFocus refuses to). A step past the cap is not a deeper page —
	// it is the page the reader is already on.
	researchMapDepth = 3
	// researchMapNodeBudget is the hard ceiling on drawn nodes: two budgeted
	// columns (each of which may carry one remainder node), one bucket column,
	// and the two slots the budgeted columns' remainders occupy. It is
	// asserted, not hoped for — a map that draws more than this is a spider
	// web, whatever it draws.
	researchMapNodeBudget = 2*researchMapColumnBudget + researchMapBucketBudget + 2
	// researchMapEdgeBudget is the ceiling on arrows: one node's buckets, plus
	// one bucket's rows, drawn ones and — because the remainder node hangs off
	// the same parent — the remainder that stands for the rest. It is larger
	// than the bucket column alone, and stating the smaller number would be a
	// claim about this file that this file does not keep.
	researchMapEdgeBudget = researchMapBucketBudget + researchMapColumnBudget + 1
	// researchMapLabelRunes is how much of a title a drawn box shows. The
	// table beside the diagram carries every full title: truncation is a
	// property of the illustration, never of the content.
	researchMapLabelRunes = 44
	// researchMapDetailBudget is how many rows the selected-node panel lists
	// before it names the count of the rest.
	researchMapDetailBudget = 8
)

// researchMapListAnchor is the in-page anchor of the outline list — where a
// remainder node sends a reader, because the list is where the rows a bounded
// picture cannot draw actually are.
const researchMapListAnchor = "#research-outline"

// The map's bucket roles. Each is the outline read's own grouping (the words
// the outline list prints in its role column), never a role this file
// invented: "hypothesis", "finding" and "addresses" are how
// internal/application/rsg groups what addresses a question, and "claim" and
// "addresses" are what a finding carries (its claim_version_refs and the
// questions it addresses). "sub-question" names the question tree's own
// parent_question_id edge.
const (
	mapRoleHypothesis  = "hypothesis"
	mapRoleFinding     = "finding"
	mapRoleAddresses   = "addresses"
	mapRoleSubQuestion = "sub-question"
	mapRoleClaim       = "claim"
)

// The map's node kinds. They are the CSS classes and the table's Kind column;
// a member node's kind is its object type.
const (
	mapKindQuestion  = "question"
	mapKindFinding   = "finding"
	mapKindBucket    = "bucket"
	mapKindRemainder = "remainder"
)

// ---------------------------------------------------------------------------
// Coordinate
// ---------------------------------------------------------------------------

// researchMapLinks builds the map's links: the drill-down coordinates on this
// page (?view, ?path) and the object detail page a node's rows link to.
type researchMapLinks struct {
	basePath  string
	projectID string
}

// focus is the drill-down URL for one coordinate: this page, the view it was
// read in, and the path of element keys selecting one node. An empty path is
// the coarse default, which is what the breadcrumb's first crumb links to.
func (l researchMapLinks) focus(view string, path []string) string {
	query := url.Values{}
	query.Set("view", view)
	if len(path) > 0 {
		query.Set("path", strings.Join(path, ","))
	}
	return l.basePath + "?" + query.Encode()
}

// object is an object's detail page (the same coordinate the outline list
// links to). A row whose version carries no branch has no link — the map never
// fabricates a branch coordinate.
func (l researchMapLinks) object(branchID, objectID string) string {
	return objectHref(l.projectID, branchID, objectID)
}

// panelFocus is the coordinate a panel link carries, or "" when the map cannot
// answer it. A candidate is answerable when resolve keeps every element — the
// map's own rule for selecting a node — so a link built here can never promise
// a node the map would not draw, and the producer of a coordinate is held to
// the same rule the consumer applies.
//
// The guard exists because an unanswerable coordinate fails silently rather
// than loudly: resolve keeps the longest valid prefix, so a click would land on
// a shallower node (a bucket of a node at the deepest step, whose first element
// is not a root, collapses to the coarse default) or, being past
// researchMapDepth, on the page the reader was already on. Suppressing the link
// is what keeps the page's promise that every drill-down is a link: what the
// panel states without a coordinate is not a drill-down, and the object detail
// page remains linked beside it.
func (b researchMapBuilder) panelFocus(path []string, extra ...string) string {
	candidate := make([]string, 0, len(path)+len(extra))
	candidate = append(candidate, path...)
	candidate = append(candidate, extra...)
	if len(b.resolve(candidate)) != len(candidate) {
		return ""
	}
	return b.links.focus(b.view, candidate)
}

// researchMapPath parses the ?path= coordinate: the comma-separated element
// keys naming the selection, outermost first. It caps the length at the map's
// own depth; whether the elements name anything is decided by the builder
// against the outline it actually read — a navigation is never an error (the
// same rule the ?view and ?tab parameters follow).
func researchMapPath(r *http.Request) []string {
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	path := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part == "" {
			break
		}
		path = append(path, part)
		if len(path) == researchMapDepth {
			break
		}
	}
	return path
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// researchMapNode is one drawn, focusable vertex. Key is the node's identity
// in the picture AND in the table beside it; Elem is its key inside its own
// column, which is the value a ?path= coordinate carries — element keys are
// scoped per column, because a bucket named "finding" under one question and a
// bucket named "finding" under another are different nodes, while an object id
// is unique on its own.
type researchMapNode struct {
	Key  string
	Elem string
	Col  int
	// ID is the domain object id ("" for a bucket or a remainder).
	ID string
	// Kind is the node's kind (a question, a finding, a bucket, a remainder,
	// or a member's object type).
	Kind string
	// Label is the node's own name (a title, or a bucket's role label). The
	// table prints it in full; the diagram truncates it.
	Label string
	// Type is the diagram's second line: the object type for a node that IS an
	// object, the row count for a node that stands for rows.
	Type string
	// Role is the relation this node hangs off its parent by ("" in the first
	// column, which has no parent).
	Role string
	// Count is how many rows this node stands for — 1 for a node that is
	// itself one object, more for an aggregate. It is a count of rows the read
	// returned, never a weight (property 2).
	Count int
	// Href is the drill-down link; DetailHref is the object detail page ("" when
	// the node is not an object, or its version carries no branch).
	Href       string
	DetailHref string
	// State is the node's own state label (a question state, a finding
	// assessment), rendered as a badge with its text, never as colour alone.
	State string
	// Statement is the node's statement text ("" when the read carried none).
	Statement string
	// Parent is the label of the node this one hangs off ("" in column 0), so
	// one table row states the edge it illustrates.
	Parent string
	// Selected marks the node the coordinate resolves to.
	Selected bool
	// Remaining is the count of rows the column could not draw (remainder
	// nodes only).
	Remaining int
	// Members are the rows an aggregate stands for — kept for the drill-down
	// column and the panel, never drawn on the node itself.
	Members []researchMapMember

	X, Y, W, H int
}

// CX and CY are the box centre. The template has no arithmetic, so the centre
// is computed here rather than in the markup (the same rule the object detail
// page's graphDiagram follows).
func (n researchMapNode) CX() int { return n.X + n.W/2 }
func (n researchMapNode) CY() int { return n.Y + n.H/2 }

// ShortLabel is the label as the diagram draws it: truncated, because a box is
// sized for a box. The table row for the same node prints Label in full.
func (n researchMapNode) ShortLabel() string { return researchMapShortLabel(n.Label) }

// researchMapMember is one row an aggregate stands for. Questions and object
// references are normalized into it so a bucket's members have ONE shape and
// the panel renders them one way.
type researchMapMember struct {
	ObjectID   string
	ObjectType string
	Title      string
	BranchID   string
	Statement  string
	State      string
	// VersionID is set for a claim reference pinned to a version this project
	// cannot resolve (the findings view's pinned-only claim): the member then
	// renders its pinned version id and no object link, exactly as the outline
	// list does.
	VersionID string
}

// Label is a member's display label: its title, or the identity the read
// pinned when there is no title — never a placeholder word that reads like a
// title (the rule nodeLabel follows on the detail page).
func (m researchMapMember) Label() string {
	if strings.TrimSpace(m.Title) != "" {
		return m.Title
	}
	if m.ObjectID != "" {
		return m.ObjectID
	}
	return m.VersionID
}

// Elem is a member's key inside the member column.
func (m researchMapMember) Elem() string {
	if m.ObjectID != "" {
		return m.ObjectID
	}
	return m.VersionID
}

// researchMapEdge is one drawn edge. Role is the relation it illustrates, and
// To names the node of the table row it belongs to: an edge cannot exist
// without that row, because a drawn edge is only ever the path from a selected
// node to a node in the next column.
type researchMapEdge struct {
	From   string
	To     string
	Role   string
	X1, Y1 int
	X2, Y2 int
	LabelX int
	LabelY int
}

// researchMapRow is one row of the map's table — the accessible form of one
// drawn node.
type researchMapRow struct {
	Key        string
	Label      string
	Role       string
	Kind       string
	State      string
	Count      int
	Href       string
	DetailHref string
	Selected   bool
	// Parent is the label of the node this row hangs off ("" for column 0).
	Parent string
	// RemainingNote explains a remainder row: how many rows it stands for and
	// where they are.
	RemainingNote string
}

// researchMapDetail is the right column: the selected node's own facts. It is
// built from the same outline as everything else — the panel is a view of the
// node, not a second read.
type researchMapDetail struct {
	Empty      bool
	EmptyNote  string
	Kind       string
	Label      string
	Type       string
	State      string
	Statement  string
	Role       string
	Parent     string
	Count      int
	CountNote  string
	Href       string
	DetailHref string
	Rows       []researchMapDetailRow
	RowsNote   string
	Buckets    []researchMapDetailBucket
	// DepthNote explains a panel that lists rows it cannot drill into: the map
	// has no step left for them, so their links are on the object detail page.
	DepthNote string
	Note      string
}

// researchMapDetailRow is one row of the panel's list.
type researchMapDetailRow struct {
	Label      string
	Type       string
	State      string
	Href       string
	DetailHref string
}

// researchMapDetailBucket is one bucket of the selected node's rollup, with
// the link that drills into it. Href is "" when the map cannot answer the
// bucket's coordinate; the panel then states the bucket and CountLabel as text,
// never a link that would drop the reader somewhere else.
type researchMapDetailBucket struct {
	Role       string
	Label      string
	Count      int
	CountLabel string
	Href       string
}

// researchMapCrumb is one step of the drill path.
type researchMapCrumb struct {
	Label   string
	Href    string
	Current bool
}

// researchMap is the whole model the template renders.
type researchMap struct {
	View  string
	Path  []string
	Lead  string
	Empty string
	// Columns are the drawn columns (the diagram's layers), outermost first.
	Columns [][]researchMapNode
	// Nodes is every drawn node, in draw order — the SVG's boxes.
	Nodes []researchMapNode
	// Rows is the table's rows: one per drawn node, from the same node list, so
	// a picture without a row and a row without a picture are both impossible.
	Rows []researchMapRow
	// Edges relate each node outside the first column to the selected node it
	// hangs off.
	Edges   []researchMapEdge
	Diagram researchMapDiagram
	Detail  researchMapDetail
	Crumbs  []researchMapCrumb
	// Note is the map's own annotation (what is drawn, what is aggregated),
	// rendered under the diagram.
	Note string
}

// researchMapDiagram is the geometry the SVG renders.
type researchMapDiagram struct {
	MarkerID string
	Width    int
	Height   int
	Nodes    []researchMapNode
	Edges    []researchMapEdge
}

// ---------------------------------------------------------------------------
// Builder
// ---------------------------------------------------------------------------

// researchMapBuilder holds what every step of the build needs: the outline the
// page already read, the view it was read in, and the link builder.
type researchMapBuilder struct {
	out   rsg.ResearchOutline
	view  string
	links researchMapLinks
}

// buildResearchMap builds the map for one (view, path) coordinate. It is pure:
// the same outline and coordinate always produce the same bytes, and it reads
// nothing the page's single outline read did not return.
func buildResearchMap(out rsg.ResearchOutline, view string, path []string, links researchMapLinks) researchMap {
	b := researchMapBuilder{out: out, view: view, links: links}
	return b.build(path)
}

// build resolves the coordinate against the outline, then draws the columns
// the resolved path opens: the roots, then one column per drill-down step.
func (b researchMapBuilder) build(path []string) researchMap {
	m := researchMap{
		View: b.view,
		Lead: "Coarse by default: the map draws a bounded number of boxes and aggregates the rest — " +
			"nothing below a node is drawn until that node is selected. Every box is also a row of the table below it.",
		Empty: b.emptyNote(),
	}
	resolved := b.resolve(path)
	m.Path = resolved
	for depth := 0; depth <= len(resolved) && depth <= researchMapDepth; depth++ {
		column := b.column(resolved[:depth])
		if len(column) == 0 {
			break
		}
		m.Columns = append(m.Columns, column)
	}
	selected := ""
	if len(resolved) > 0 {
		selected = resolved[len(resolved)-1]
	}
	for _, column := range m.Columns {
		for _, node := range column {
			node.Selected = node.Col == len(resolved)-1 && node.Elem == selected
			m.Nodes = append(m.Nodes, node)
		}
	}
	m.Diagram = layoutResearchMap(m.Columns, m.Nodes, resolved)
	m.Edges = m.Diagram.Edges
	m.Rows = researchMapRows(m.Nodes)
	m.Detail = b.detail(resolved, m)
	m.Crumbs = b.crumbs(resolved)
	m.Note = b.note(m)
	return m
}

// resolve walks the coordinate against the outline and keeps the longest valid
// prefix. An element that names nothing focusable at its depth ends the path
// there: the map falls back to the deepest level it can prove, never to an
// empty frame and never to an error page.
func (b researchMapBuilder) resolve(path []string) []string {
	resolved := make([]string, 0, len(path))
	for _, elem := range path {
		if len(resolved) > researchMapDepth {
			break
		}
		focusable := false
		for _, node := range b.column(resolved) {
			if node.Elem == elem && node.focusable() {
				focusable = true
				break
			}
		}
		if !focusable {
			break
		}
		resolved = append(resolved, elem)
	}
	return resolved
}

// focusable reports whether a node can be selected by a coordinate. A
// remainder node cannot: there is no second page of the map to drill into, and
// a coordinate that promised one would be a dead end. Its row sends the reader
// to the outline list instead, which draws every row.
func (n researchMapNode) focusable() bool {
	return n.Kind != mapKindRemainder && n.Href != ""
}

// column returns the column that follows one resolved prefix: the roots for the
// empty prefix, the buckets of the focused node for a prefix of one, the rows
// of a focused bucket for a prefix of two, and nothing at the map's deepest
// step (a row's own details live on its object page).
func (b researchMapBuilder) column(prefix []string) []researchMapNode {
	switch len(prefix) {
	case 0:
		return b.column0()
	case 1:
		return b.buckets(prefix[0])
	case 2:
		return b.members(prefix[0], prefix[1])
	default:
		return nil
	}
}

// column0 draws the view's roots: the question tree's roots (By Question) or
// the project's findings (By Finding), in the outline's own order — the order
// the list beside the map renders them in, so the map draws the first rows of
// the list it illustrates.
func (b researchMapBuilder) column0() []researchMapNode {
	var nodes []researchMapNode
	if b.view == researchViewFindings {
		nodes = make([]researchMapNode, 0, len(b.out.Findings))
		for _, f := range b.out.Findings {
			nodes = append(nodes, researchMapNode{
				Elem:       f.ObjectID,
				ID:         f.ObjectID,
				Kind:       mapKindFinding,
				Label:      researchMapTitle(f.Title, f.ObjectID),
				Type:       "finding",
				Count:      1,
				Href:       b.links.focus(b.view, []string{f.ObjectID}),
				DetailHref: b.links.object(f.BranchID, f.ObjectID),
				State:      f.Assessment,
				Statement:  f.Statement,
			})
		}
	} else {
		nodes = make([]researchMapNode, 0, len(b.out.Questions))
		for _, q := range b.out.Questions {
			nodes = append(nodes, researchMapNode{
				Elem:       q.ObjectID,
				ID:         q.ObjectID,
				Kind:       mapKindQuestion,
				Label:      researchMapTitle(q.Title, q.ObjectID),
				Type:       "research_question",
				Count:      1,
				Href:       b.links.focus(b.view, []string{q.ObjectID}),
				DetailHref: b.links.object(q.BranchID, q.ObjectID),
				State:      q.QuestionState,
				Statement:  q.Statement,
			})
		}
	}
	return budgetResearchMapColumn(nodes, 0, b.rootNoun())
}

// rootNoun names what column 0 holds, for the remainder node's label.
func (b researchMapBuilder) rootNoun() string {
	if b.view == researchViewFindings {
		return "finding"
	}
	return "question"
}

// budgetResearchMapColumn applies the column budget: the first
// researchMapColumnBudget nodes stay and everything after them becomes ONE
// remainder node — never dropped (invariant 8: nothing disappears; the list
// beside the diagram still has every row).
func budgetResearchMapColumn(nodes []researchMapNode, col int, noun string) []researchMapNode {
	for i := range nodes {
		nodes[i].Col = col
		nodes[i].Key = researchMapKey(col, nodes[i].Elem)
	}
	if len(nodes) <= researchMapColumnBudget {
		return nodes
	}
	hidden := len(nodes) - researchMapColumnBudget
	kept := nodes[:researchMapColumnBudget]
	return append(kept, researchMapNode{
		Key:       researchMapKey(col, "more"),
		Elem:      "more",
		Col:       col,
		Kind:      mapKindRemainder,
		Label:     pluralCount(hidden, noun),
		Type:      "in the outline list",
		Count:     hidden,
		Href:      researchMapListAnchor,
		Remaining: hidden,
	})
}

// buckets draws one node's rollup: what hangs off the focused question or
// finding, aggregated. Each bucket is ONE node carrying its row count, so a
// question with 300 findings costs the same one box as a question with three.
//
// The order is fixed — hypotheses, findings, then everything else that
// addresses the question, with the sub-questions last — and it is the docs'
// own line (docs/06 §5). It is never an order by size: ranking the project's
// objects by their counts is exactly the arithmetic the domain refuses
// (CLAUDE.md §9.13).
func (b researchMapBuilder) buckets(elem string) []researchMapNode {
	var (
		parent string
		groups []researchMapGroup
	)
	if b.view == researchViewFindings {
		f, ok := b.finding(elem)
		if !ok {
			return nil
		}
		parent = researchMapTitle(f.Title, f.ObjectID)
		groups = []researchMapGroup{
			{role: mapRoleClaim, label: "Claims", members: claimMembers(f)},
			{role: mapRoleAddresses, label: "Questions addressed", members: refMembers(f.Questions)},
		}
	} else {
		q, ok := b.question(elem)
		if !ok {
			return nil
		}
		parent = researchMapTitle(q.Title, q.ObjectID)
		groups = []researchMapGroup{
			{role: mapRoleHypothesis, label: "Hypotheses", members: refMembers(q.Hypotheses)},
			{role: mapRoleFinding, label: "Findings", members: refMembers(q.Findings)},
			{role: mapRoleAddresses, label: "Other objects", members: refMembers(q.OtherObjects)},
			{role: mapRoleSubQuestion, label: "Sub-questions", members: questionMembers(q.Children)},
		}
	}
	nodes := make([]researchMapNode, 0, len(groups))
	for _, g := range groups {
		if len(g.members) == 0 {
			continue
		}
		// A bucket's element key inside its column is its role: the roles are a
		// closed set per parent, so "hypothesis" names exactly one node under
		// one question, and the resolve step checks it against this column.
		nodes = append(nodes, researchMapNode{
			Elem:    g.role,
			Kind:    mapKindBucket,
			Label:   g.label,
			Type:    pluralCount(len(g.members), "row"),
			Count:   len(g.members),
			Role:    g.role,
			Parent:  parent,
			Href:    b.links.focus(b.view, []string{elem, g.role}),
			Members: g.members,
		})
	}
	return budgetResearchMapColumn(nodes, 1, "bucket")
}

// members draws one bucket's rows: the objects themselves, bounded by the same
// budget, with the remainder counting what the column could not draw. These are
// the map's leaf nodes — the next step from one of them is its object page,
// where its provenance and evidence are (docs/06 §5).
func (b researchMapBuilder) members(elem, role string) []researchMapNode {
	var bucket researchMapNode
	found := false
	for _, node := range b.buckets(elem) {
		if node.Elem == role {
			bucket, found = node, true
			break
		}
	}
	if !found {
		return nil
	}
	nodes := make([]researchMapNode, 0, len(bucket.Members))
	for _, member := range bucket.Members {
		memberType := member.ObjectType
		if memberType == "" {
			memberType = mapRoleClaim
		}
		detail := ""
		if member.ObjectID != "" {
			detail = b.links.object(member.BranchID, member.ObjectID)
		}
		nodes = append(nodes, researchMapNode{
			Elem:       member.Elem(),
			ID:         member.ObjectID,
			Kind:       memberType,
			Label:      member.Label(),
			Type:       memberType,
			Count:      1,
			Role:       bucket.Role,
			Parent:     bucket.Label,
			Href:       b.links.focus(b.view, []string{elem, role, member.Elem()}),
			DetailHref: detail,
			State:      member.State,
			Statement:  member.Statement,
		})
	}
	// A member that is itself a question has its own buckets, which the panel
	// lists when it is selected — the recursion docs/06 §5 asks for, rendered
	// where it costs no boxes. members() is the only place a member's own
	// rollup is attached, so the map's node list stays the picture's list.
	for i := range nodes {
		if nodes[i].Kind != "research_question" {
			continue
		}
		nodes[i].Members = b.memberRollup(nodes[i].Elem)
	}
	return budgetResearchMapColumn(nodes, 2, "row")
}

// memberRollup is a member question's own rows, normalized like a bucket's
// members so the panel can list them.
func (b researchMapBuilder) memberRollup(objectID string) []researchMapMember {
	q, ok := b.question(objectID)
	if !ok {
		return nil
	}
	members := make([]researchMapMember, 0, len(q.Hypotheses)+len(q.Findings)+len(q.OtherObjects)+len(q.Children))
	members = append(members, refMembers(q.Hypotheses)...)
	members = append(members, refMembers(q.Findings)...)
	members = append(members, refMembers(q.OtherObjects)...)
	members = append(members, questionMembers(q.Children)...)
	return members
}

// ---------------------------------------------------------------------------
// Lookups over the outline
// ---------------------------------------------------------------------------

// question finds a question of the outline by object id, walking the tree. The
// walk is bounded by the tree the read returned and happens only when a
// coordinate names a question: the default (coarse) view looks nothing up.
func (b researchMapBuilder) question(objectID string) (rsg.OutlineQuestion, bool) {
	var walk func(qs []rsg.OutlineQuestion) (rsg.OutlineQuestion, bool)
	walk = func(qs []rsg.OutlineQuestion) (rsg.OutlineQuestion, bool) {
		for _, q := range qs {
			if q.ObjectID == objectID {
				return q, true
			}
			if found, ok := walk(q.Children); ok {
				return found, true
			}
		}
		return rsg.OutlineQuestion{}, false
	}
	return walk(b.out.Questions)
}

// finding finds a finding of the outline by object id.
func (b researchMapBuilder) finding(objectID string) (rsg.OutlineFinding, bool) {
	for _, f := range b.out.Findings {
		if f.ObjectID == objectID {
			return f, true
		}
	}
	return rsg.OutlineFinding{}, false
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// researchMapGroup is one bucket of a rollup: the role, its human label and the
// rows it stands for.
type researchMapGroup struct {
	role    string
	label   string
	members []researchMapMember
}

func refMembers(refs []rsg.OutlineObjectRef) []researchMapMember {
	members := make([]researchMapMember, 0, len(refs))
	for _, ref := range refs {
		members = append(members, researchMapMember{
			ObjectID:   ref.ObjectID,
			ObjectType: ref.ObjectType,
			Title:      ref.Title,
			BranchID:   ref.BranchID,
		})
	}
	return members
}

// questionMembers normalizes child questions into members: the sub-question
// bucket's rows are questions like any other row, carrying their own state.
func questionMembers(questions []rsg.OutlineQuestion) []researchMapMember {
	members := make([]researchMapMember, 0, len(questions))
	for _, q := range questions {
		members = append(members, researchMapMember{
			ObjectID:   q.ObjectID,
			ObjectType: "research_question",
			Title:      q.Title,
			BranchID:   q.BranchID,
			Statement:  q.Statement,
			State:      q.QuestionState,
		})
	}
	return members
}

// claimMembers normalizes a finding's claim references. A claim the project
// cannot resolve keeps its pinned version id and gets no object link — the rule
// the outline list follows ("not in this project"), never a link built from a
// coordinate the read did not return.
func claimMembers(f rsg.OutlineFinding) []researchMapMember {
	members := make([]researchMapMember, 0, len(f.Claims))
	for _, c := range f.Claims {
		member := researchMapMember{
			VersionID:  c.VersionID,
			ObjectType: mapRoleClaim,
			Title:      c.Title,
		}
		if c.Resolved && c.ObjectID != "" {
			member.ObjectID = c.ObjectID
			member.BranchID = c.BranchID
		}
		members = append(members, member)
	}
	return members
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// layoutResearchMap places the columns and connects them. The geometry comes
// from the same layout the object detail page's graphs use (layoutLayers): one
// implementation of "columns of boxes", so the two surfaces cannot disagree
// about what a layered picture is. The map's columns are its layers.
//
// nodes is the flattened column list, in draw order; it is what the edges are
// built from, so a node the layout dropped can be neither drawn nor connected.
// path is the resolved coordinate, and it is also the edge set: column col
// hangs off path[col-1], the node the reader opened to get there.
func layoutResearchMap(columns [][]researchMapNode, nodes []researchMapNode, path []string) researchMapDiagram {
	layers := make([][]graphNode, 0, len(columns))
	for _, column := range columns {
		layer := make([]graphNode, 0, len(column))
		for _, node := range column {
			// The layout sizes a box for the label it draws, so it measures the
			// truncated one — a 300-character title must not make a 2-metre box.
			layer = append(layer, graphNode{
				Key: node.Key, ID: node.ID, Label: node.ShortLabel(), Type: node.Type, Kind: node.Kind,
			})
		}
		layers = append(layers, layer)
	}
	placed := layoutLayers("research-map", layers)
	byKey := make(map[string]graphNode, len(placed.Nodes))
	for _, n := range placed.Nodes {
		byKey[n.Key] = n
	}
	diagram := researchMapDiagram{MarkerID: placed.MarkerID, Width: placed.Width, Height: placed.Height}
	for _, node := range nodes {
		box, ok := byKey[node.Key]
		if !ok {
			continue
		}
		node.X, node.Y, node.W, node.H = box.X, box.Y, box.W, box.H
		diagram.Nodes = append(diagram.Nodes, node)
	}
	// Every node outside the first column hangs off the node of the column
	// before it that the reader opened: column col's parent is path[col-1] —
	// the root for the bucket column, the bucket for the member column, and
	// for the last column the selected node itself. The edge set is therefore
	// exactly the drill path, and each edge's role is its target's own role,
	// the value that target's table row prints. A column whose parent is not
	// on the path (a coordinate that fell back) has no arrow rather than an
	// invented one.
	for col := 1; col < len(columns); col++ {
		if col-1 >= len(path) {
			break
		}
		parent, ok := researchMapNodeAt(diagram.Nodes, col-1, path[col-1])
		if !ok {
			break
		}
		for _, node := range diagram.Nodes {
			if node.Col != col {
				continue
			}
			diagram.Edges = append(diagram.Edges, researchMapEdge{
				From:   parent.Key,
				To:     node.Key,
				Role:   node.Role,
				X1:     parent.X + parent.W,
				Y1:     parent.Y + parent.H/2,
				X2:     node.X,
				Y2:     node.Y + node.H/2,
				LabelX: (parent.X + parent.W + node.X) / 2,
				LabelY: (parent.Y + parent.H/2 + node.Y + node.H/2) / 2,
			})
		}
	}
	return diagram
}

// researchMapNodeAt is the drawn node at one column with one element key — how
// every lookup by coordinate is done, so a node that was not drawn can never
// be named by a link, an edge or the panel.
func researchMapNodeAt(nodes []researchMapNode, col int, elem string) (researchMapNode, bool) {
	for _, node := range nodes {
		if node.Col == col && node.Elem == elem {
			return node, true
		}
	}
	return researchMapNode{}, false
}

// researchMapKey is a node's identity in the picture: its column index and its
// element key. Two columns can hold an element with the same key (a bucket
// role and an object id are different namespaces), so the identity is the pair.
func researchMapKey(col int, elem string) string {
	return "c" + strconv.Itoa(col) + ":" + elem
}

// researchMapRows is the table's rows: one per drawn node, in draw order.
func researchMapRows(nodes []researchMapNode) []researchMapRow {
	rows := make([]researchMapRow, 0, len(nodes))
	for _, node := range nodes {
		row := researchMapRow{
			Key:        node.Key,
			Label:      node.Label,
			Role:       node.Role,
			Kind:       node.Kind,
			State:      node.State,
			Count:      node.Count,
			Href:       node.Href,
			DetailHref: node.DetailHref,
			Selected:   node.Selected,
			Parent:     node.Parent,
		}
		if node.Remaining > 0 {
			row.RemainingNote = pluralCount(node.Remaining, "row") + " are not drawn in the map — the outline list has them all."
		}
		rows = append(rows, row)
	}
	return rows
}

// ---------------------------------------------------------------------------
// Panel, crumbs and notes
// ---------------------------------------------------------------------------

// detail builds the right column for the resolved coordinate: the selected
// node's own facts, the buckets it rolls up into, and the rows it stands for.
// With nothing selected it is the instruction that says how to select one.
func (b researchMapBuilder) detail(path []string, m researchMap) researchMapDetail {
	if len(path) == 0 {
		return researchMapDetail{
			Empty: true,
			EmptyNote: "No node selected. Choose a " + b.rootNoun() + " in the map — or the same row in the " +
				"table below it — to drill into what it holds. The map opens coarse on purpose: what is below a " +
				"node is drawn once the node is chosen.",
		}
	}
	node, found := researchMapNodeAt(m.Nodes, len(path)-1, path[len(path)-1])
	if !found {
		return researchMapDetail{Empty: true, EmptyNote: "The selected node is not in this map."}
	}
	detail := researchMapDetail{
		Kind:       node.Kind,
		Label:      node.Label,
		Type:       node.Type,
		State:      node.State,
		Statement:  node.Statement,
		Role:       node.Role,
		Parent:     node.Parent,
		Count:      node.Count,
		Href:       node.Href,
		DetailHref: node.DetailHref,
	}
	if node.Remaining > 0 {
		detail.CountNote = pluralCount(node.Remaining, "row") + " are not drawn in the map — the outline list has them all."
	}
	// The buckets the selected node rolls up into: the same groups the map
	// draws, each with the coordinate that drills into it. A question's panel
	// therefore reaches its findings through the finding bucket, which is the
	// path docs/06 §5 describes.
	//
	// The coordinate is emitted only when the map can answer it (panelFocus):
	// a bucket of a node at the map's deepest step would sit one step past
	// researchMapDepth, and resolve() would drop the reader's click back onto a
	// shallower node. Such a bucket is stated as text instead — the rows are
	// real and the count is real, the map simply has no page for them.
	suppressed := 0
	for _, bucket := range b.buckets(node.Elem) {
		// The selected node IS the path's last element, so a bucket's coordinate
		// is that path extended by the bucket's role.
		href := b.panelFocus(path, bucket.Role)
		if href == "" {
			suppressed++
		}
		detail.Buckets = append(detail.Buckets, researchMapDetailBucket{
			Role:       bucket.Role,
			Label:      bucket.Label,
			Count:      bucket.Count,
			CountLabel: pluralCount(bucket.Count, "row"),
			Href:       href,
		})
	}
	// The rows the selected node stands for: a bucket's members — or a member
	// question's own rows — bounded, with the count of what is beyond the
	// bound.
	members := node.Members
	shown := 0
	for _, member := range members {
		if shown == researchMapDetailBudget {
			detail.RowsNote = pluralCount(len(members), "row") + " in all — the outline list has every one of them."
			break
		}
		shown++
		row := researchMapDetailRow{
			Label: member.Label(),
			Type:  member.ObjectType,
			State: member.State,
		}
		if member.ObjectID != "" {
			row.Href = b.panelFocus(path, member.ObjectID)
			if row.Href == "" {
				suppressed++
			}
			// The object page is not a map coordinate and is therefore never
			// suppressed: whatever the map's depth, the row it could not drill
			// into stays reachable.
			row.DetailHref = b.links.object(member.BranchID, member.ObjectID)
		}
		detail.Rows = append(detail.Rows, row)
	}
	if suppressed > 0 {
		detail.DepthNote = "The map draws " + pluralCount(researchMapDepth, "step") +
			": what is below this node is on its object detail page."
	}
	switch {
	case node.Kind == mapKindBucket || node.Kind == "research_question":
		detail.Note = "This panel lists what the node stands for; the object detail page holds a version's " +
			"provenance and evidence (docs/06 §5: drill down to Claim / Evidence / Object)."
	}
	return detail
}

// crumbs is the drill path as a breadcrumb: the map's coarse root, then one
// crumb per resolved step with the last one current. A reader can therefore
// always walk back up with the keyboard.
func (b researchMapBuilder) crumbs(path []string) []researchMapCrumb {
	crumbs := []researchMapCrumb{{Label: "Research map", Href: b.links.focus(b.view, nil)}}
	for i := range path {
		label := path[i]
		switch i {
		case 0:
			if b.view == researchViewFindings {
				if f, ok := b.finding(path[i]); ok {
					label = researchMapTitle(f.Title, f.ObjectID)
				}
			} else if q, ok := b.question(path[i]); ok {
				label = researchMapTitle(q.Title, q.ObjectID)
			}
		case 1:
			label = researchMapRoleLabel(path[i])
		default:
			label = researchMapTitle(label, label)
		}
		crumbs = append(crumbs, researchMapCrumb{
			Label:   label,
			Href:    b.links.focus(b.view, path[:i+1]),
			Current: i == len(path)-1,
		})
	}
	return crumbs
}

// note states what the map drew and what it did not. An aggregate that does not
// say it is an aggregate is a lie by omission, and a budget that does not say
// it truncated is a silent one.
func (b researchMapBuilder) note(m researchMap) string {
	hidden := 0
	for _, node := range m.Nodes {
		hidden += node.Remaining
	}
	note := pluralCount(len(m.Nodes), "node") + " drawn"
	if hidden > 0 {
		note += " · " + pluralCount(hidden, "row") + " aggregated into a remainder node"
	}
	return note + " · the outline list beside the map has every row."
}

// emptyNote is the map's empty state.
func (b researchMapBuilder) emptyNote() string {
	if b.view == researchViewFindings {
		return "No findings yet. A finding appears in the map once this project has one."
	}
	return "No research questions yet. The map organizes itself by question once this project has one."
}

// researchMapTitle is a node's display title: the title, or the object id when
// the read carried no title — never a placeholder word that reads like a title.
func researchMapTitle(title, objectID string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return objectID
}

// researchMapRoleLabel is the human label of a bucket role.
func researchMapRoleLabel(role string) string {
	switch role {
	case mapRoleHypothesis:
		return "Hypotheses"
	case mapRoleFinding:
		return "Findings"
	case mapRoleAddresses:
		return "Other objects"
	case mapRoleSubQuestion:
		return "Sub-questions"
	case mapRoleClaim:
		return "Claims"
	default:
		return role
	}
}

// researchMapShortLabel truncates a label for the picture. The table beside the
// diagram carries the full text, so nothing is lost that the list does not state
// in full.
func researchMapShortLabel(label string) string {
	runes := []rune(label)
	if len(runes) <= researchMapLabelRunes {
		return label
	}
	return string(runes[:researchMapLabelRunes-1]) + "…"
}
