package rsg

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// ResearchOutline (T0211) is the Research page's data shape: the project's
// scientific objects aggregated by research question and by finding — the
// outline the page renders, and the JSON contract the route answers for
// non-HTML clients. Counts are auxiliary annotations only: the outline's
// organization is questions (a tree through parent_question_id) and
// findings (their claims and the questions they address), never an object
// type or files listing.
//
// Linkage is the relation graph: an object belongs to a question's
// outline only through an addresses_question relation whose target pins
// the question. The hypothesis payload's advisory question_id (docs/08,
// semantics.HintHypothesisQuestionRef) is deliberately NOT a second
// linkage mechanism — the outline shows what the graph says, and an
// advisory field must not silently pretend to be a relation.
type ResearchOutline struct {
	ProjectID string
	// ProjectSlug / ProjectName are the entry gate's project facts (the
	// page renders them; the gate already fetched the project, so nothing
	// extra is read).
	ProjectSlug string
	ProjectName string
	// Questions is the question tree: roots first, children nested (a
	// question whose parent is unknown in the project renders as a root).
	Questions []OutlineQuestion
	// Findings is every finding of the project with its claim references
	// (resolved to titles when the claim lives in this project) and the
	// questions it addresses.
	Findings []OutlineFinding
	// Unassigned is every object that is neither a question nor a finding
	// and addresses no question (sorted by title — the list fallback's
	// remainder, with type badges, not a type-grouped dump).
	Unassigned []OutlineObjectRef
	// Counts is the auxiliary summary (header annotation).
	Counts OutlineCounts
}

// OutlineQuestion is one research question node of the outline.
type OutlineQuestion struct {
	ObjectID string
	// VersionID is the outline's as-of version of the question (the
	// project-wide newest version).
	VersionID string
	// BranchID is the as-of version's branch — the drill-down link's
	// branch coordinate. Empty when the version carries no branch (the
	// page then renders the title without a link: the outline must not
	// fabricate a branch coordinate).
	BranchID string
	Title    string
	// Statement is the question text (payload statement; empty when the
	// payload does not carry one).
	Statement     string
	QuestionState string
	// ParentQuestionID is the payload's parent reference ("" = root). A
	// parent outside the project does not affect the tree.
	ParentQuestionID string
	// Children are the nested sub-questions, same ordering rule as roots.
	Children []OutlineQuestion
	// Hypotheses / Findings / OtherObjects are the objects whose
	// addresses_question relation targets this question, grouped by role.
	// Each list is sorted by title.
	Hypotheses   []OutlineObjectRef
	Findings     []OutlineObjectRef
	OtherObjects []OutlineObjectRef
}

// OutlineObjectRef is one object reference in the outline (a row of an
// addressed-by list or the unassigned remainder).
type OutlineObjectRef struct {
	ObjectID   string
	ObjectType string
	Title      string
	// BranchID is the referenced object's as-of version branch (the
	// drill-down link coordinate; empty = no link).
	BranchID string
}

// OutlineFinding is one finding of the findings view.
type OutlineFinding struct {
	ObjectID  string
	VersionID string
	// BranchID is the finding's as-of version branch (drill-down
	// coordinate; empty = no link).
	BranchID string
	Title    string
	// Statement is the human-readable finding summary (payload statement).
	Statement   string
	FindingType string
	// Assessment is the payload's current assessment state
	// (preliminary/accepted/contested/unresolved/superseded/aborted).
	Assessment string
	// Claims are the finding's claim_version_refs, resolved to the claim
	// object when it lives in this project (Resolved=true); an external or
	// missing claim renders its pinned version id.
	Claims []OutlineClaimRef
	// Questions are the questions this finding addresses
	// (addresses_question relations where the finding is the source).
	Questions []OutlineObjectRef
}

// OutlineClaimRef is one claim reference of a finding.
type OutlineClaimRef struct {
	// ObjectID is empty when the pinned version does not resolve to a
	// claim object of this project (an unknown version id, or another
	// project's claim version — never rendered, docs/45).
	ObjectID  string
	VersionID string
	// Title is the pinned claim version's own title (the version the
	// finding cited, which may be superseded by now — "Nothing
	// disappears; state only evolves").
	Title string
	// BranchID is the pinned claim version's branch (drill-down
	// coordinate; empty = no link).
	BranchID string
	// Resolved reports whether the pinned version resolves to a claim
	// object of THIS project, whatever version the object is at now — a
	// ref pinned to a superseded claim version still resolves, and
	// Resolved=false really means "not a claim of this project".
	Resolved bool
}

// OutlineCounts is the auxiliary summary the header and each question
// annotate with — never the page's organization.
type OutlineCounts struct {
	Questions  int
	Findings   int
	Hypotheses int
	Claims     int
	// OtherObjects counts everything that is not a question, finding,
	// hypothesis or claim.
	OtherObjects int
}

// ResearchOutline builds the project's outline (T0211). The read is
// exactly as visible as the project: the entry gate is the same
// visibility-aware projects.Get requireRead the RSG query runs (T0209) —
// a caller who may not read the project answers projects.ErrProjectNotFound
// (existence hiding, docs/45), and the outline only ever contains this
// project's own rows (the project-wide unfiltered slice, no traversal), so
// no other project's object, relation or existence can enter it.
//
// A store failure aborts (fail closed): the caller never receives a
// silently partial outline.
func (s *Service) ResearchOutline(ctx context.Context, r projects.Reader, projectID string) (ResearchOutline, error) {
	if projectID == "" {
		return ResearchOutline{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	// Entry gate first — the same order as Query, so the denial precedes
	// every store read.
	project, err := s.projects.Get(ctx, r, projectID)
	if err != nil {
		return ResearchOutline{}, wrapError(err)
	}
	return s.buildResearchOutline(ctx, project)
}

// buildResearchOutline runs the outline reads and the aggregation for an
// already-gated project (the entry gate fetched the project). The project
// overview (T0212) shares this builder: its gate runs once and the fetched
// project serves both the overview facts and the outline.
func (s *Service) buildResearchOutline(ctx context.Context, project domain.Project) (ResearchOutline, error) {
	if s.queries == nil {
		return ResearchOutline{}, fmt.Errorf("%w: no query port configured", ErrStore)
	}

	objRows, err := s.queries.ListObjectVersions(ctx, project.ID, nil, nil)
	if err != nil {
		return ResearchOutline{}, wrapError(err)
	}
	relRows, err := s.queries.ListRelationVersions(ctx, project.ID, nil, nil)
	if err != nil {
		return ResearchOutline{}, wrapError(err)
	}
	// The findings' claim_version_refs pin exact claim version ids, and the
	// project slice carries only each object's NEWEST version — a ref
	// pinned to a superseded claim version needs its pinned row fetched
	// explicitly, or the outline would say "not in this project" about a
	// claim that lives in the project (and then list the same claim in the
	// unassigned remainder). The fetch runs here, in the service (which
	// holds ctx and the query port), not inside the pure buildOutline.
	claimRefRows, err := s.fetchClaimRefRows(ctx, project.ID, objRows)
	if err != nil {
		return ResearchOutline{}, wrapError(err)
	}
	out := buildOutline(project.ID, objRows, relRows, claimRefRows)
	out.ProjectSlug = project.Slug
	out.ProjectName = project.Name
	return out, nil
}

// fetchClaimRefRows resolves the findings' pinned claim_version_refs to
// their claim rows of THIS project: one batch fetch by version id (the
// existing QueryPort method, no new query), keeping only the rows whose
// container is a claim of the project. A ref to another project's claim
// version stays unresolved — the outline must not leak a foreign claim's
// existence or title (docs/45), so "not in this project" stays true.
func (s *Service) fetchClaimRefRows(ctx context.Context, projectID string, objRows []ObjectQueryRow) ([]ObjectQueryRow, error) {
	ids := make(map[string]bool)
	for i := range objRows {
		if objRows[i].Object.ObjectType != "finding" {
			continue
		}
		for _, vid := range parseOutlinePayload(objRows[i].Version.Payload).claimVersionRefs {
			ids[vid] = true
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	versionIDs := make([]string, 0, len(ids))
	for vid := range ids {
		versionIDs = append(versionIDs, vid)
	}
	rows, err := s.queries.ListObjectVersionsByIDs(ctx, versionIDs)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectQueryRow, 0, len(rows))
	for _, row := range rows {
		if row.Object.ProjectID == projectID && row.Object.ObjectType == "claim" {
			out = append(out, row)
		}
	}
	return out, nil
}

// outlinePayload is the minimal parsed view of one version payload. Every
// field is tolerant: a missing or mistyped field renders empty — the
// outline must degrade, never fail, on payloads it does not understand.
type outlinePayload struct {
	statement        string
	questionState    string
	parentQuestionID string
	findingType      string
	assessment       string
	claimVersionRefs []string
}

func parseOutlinePayload(raw json.RawMessage) outlinePayload {
	var p outlinePayload
	if len(raw) == 0 {
		return p
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return p
	}
	p.statement = payloadString(m["statement"])
	p.questionState = payloadString(m["question_state"])
	p.parentQuestionID = payloadString(m["parent_question_id"])
	p.findingType = payloadString(m["finding_type"])
	p.assessment = payloadString(m["assessment"])
	if refs, ok := m["claim_version_refs"].([]any); ok {
		for _, ref := range refs {
			if s, ok := ref.(string); ok && s != "" {
				p.claimVersionRefs = append(p.claimVersionRefs, s)
			}
		}
	}
	return p
}

// branchIDOf derefs a version's branch id (nullable in the schema); the
// outline renders a missing branch as "" — no link is fabricated.
func branchIDOf(v domain.ScientificObjectVersion) string {
	if v.BranchID == nil {
		return ""
	}
	return *v.BranchID
}

func payloadString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// buildOutline aggregates the unfiltered project slice into the outline.
// claimRefRows are the pinned claim rows the service fetched for the
// findings' claim_version_refs (in-project claims only) — the project
// slice alone carries only each object's newest version.
func buildOutline(projectID string, objRows []ObjectQueryRow, relRows []RelationQueryRow, claimRefRows []ObjectQueryRow) ResearchOutline {
	out := ResearchOutline{ProjectID: projectID}

	// Index every object row: by object id (the container) and by version
	// id (the finding payload's claim_version_refs pin version ids).
	type row struct {
		obj     ObjectQueryRow
		payload outlinePayload
	}
	byObject := make(map[string]*row, len(objRows))
	byVersion := make(map[string]*row, len(objRows))
	for i := range objRows {
		r := &row{obj: objRows[i], payload: parseOutlinePayload(objRows[i].Version.Payload)}
		byObject[r.obj.Object.ID] = r
		byVersion[r.obj.Version.ID] = r
	}

	// Questions and findings are the outline's axes; hypotheses and claims
	// participate inside them.
	questionIDs := make([]string, 0)
	findingIDs := make([]string, 0)
	for id, r := range byObject {
		switch r.obj.Object.ObjectType {
		case "research_question":
			questionIDs = append(questionIDs, id)
		case "finding":
			findingIDs = append(findingIDs, id)
		}
	}
	slices.Sort(questionIDs)
	slices.Sort(findingIDs)

	// addresses_question linkage: per question object, the objects whose
	// relation targets the question, deduped by source object (two edges to
	// different versions of the same question are one object in the
	// outline). Relation endpoints pin exact object versions — an edge
	// stays pinned to the version it was created against, and nothing
	// re-pins it when an endpoint object gains a version. The outline
	// therefore resolves endpoints by their container object (the endpoint
	// context the query joins): an edge pinned to a superseded version
	// still counts, and the rendered title/branch come from the object's
	// current row. The relation list is this project's own slice — the edges
	// its STATE LINEAGE carries, already authorized by their pins' carriers —
	// so the endpoint contexts are trusted here. They are keyed by container
	// object, and after a merge accepted an external fork's proposal those
	// containers are the CONTRIBUTOR's while the versions are this project's
	// (ADR-027 Decision 1): the linkage still runs on the object ids the query
	// joined, exactly as it does for the project's own objects.
	questionAddressedBy := make(map[string][]OutlineObjectRef)
	questionAddressedSeen := make(map[string]map[string]bool)
	for _, rel := range relRows {
		if rel.Version.RelationType != "addresses_question" {
			continue
		}
		target, ok := byObject[rel.Target.ObjectID]
		if !ok {
			continue
		}
		if target.obj.Object.ObjectType != "research_question" {
			continue
		}
		source, ok := byObject[rel.Source.ObjectID]
		if !ok || source.obj.Object.ID == target.obj.Object.ID {
			continue
		}
		seen := questionAddressedSeen[target.obj.Object.ID]
		if seen == nil {
			seen = make(map[string]bool)
			questionAddressedSeen[target.obj.Object.ID] = seen
		}
		if seen[source.obj.Object.ID] {
			continue
		}
		seen[source.obj.Object.ID] = true
		questionAddressedBy[target.obj.Object.ID] = append(questionAddressedBy[target.obj.Object.ID], OutlineObjectRef{
			ObjectID:   source.obj.Object.ID,
			ObjectType: source.obj.Object.ObjectType,
			Title:      source.obj.Version.Title,
			BranchID:   branchIDOf(source.obj.Version),
		})
	}

	// The outline views share one membership rule: an object is part of
	// the question/finding axes when it is a question, a finding, or
	// addresses a question (relations only — payload question_id is
	// advisory and not a linkage).
	assigned := make(map[string]bool, len(objRows))
	for _, id := range questionIDs {
		assigned[id] = true
	}
	for _, id := range findingIDs {
		assigned[id] = true
	}
	for _, refs := range questionAddressedBy {
		for _, ref := range refs {
			assigned[ref.ObjectID] = true
		}
	}

	// --- the question tree ---
	nodes := make(map[string]*OutlineQuestion, len(questionIDs))
	for _, id := range questionIDs {
		r := byObject[id]
		nodes[id] = &OutlineQuestion{
			ObjectID:         id,
			VersionID:        r.obj.Version.ID,
			BranchID:         branchIDOf(r.obj.Version),
			Title:            r.obj.Version.Title,
			Statement:        r.payload.statement,
			QuestionState:    r.payload.questionState,
			ParentQuestionID: r.payload.parentQuestionID,
		}
	}

	// Group the addressed-by lists per question by role.
	for _, id := range questionIDs {
		for _, ref := range questionAddressedBy[id] {
			slot := &nodes[id].OtherObjects
			switch ref.ObjectType {
			case "hypothesis":
				slot = &nodes[id].Hypotheses
			case "finding":
				slot = &nodes[id].Findings
			}
			*slot = append(*slot, ref)
		}
	}

	// Link children to parents with a white/gray/black walk up the parent
	// chain, guarded against reference cycles (a malformed payload could
	// name an ancestor): the edge that would close a cycle is dropped —
	// its source renders as a root — so the outline never loops and never
	// drops a question.
	childrenOf := make(map[string][]string, len(questionIDs))
	rootIDs := outlineTreeRoots(questionIDs, nodes, childrenOf)
	for _, id := range rootIDs {
		attachOutlineChildren(nodes, childrenOf, id, &out.Questions)
	}
	// Roots render in the same title order children use at every level.
	slices.SortFunc(out.Questions, func(a, b OutlineQuestion) int {
		if c := strings.Compare(a.Title, b.Title); c != 0 {
			return c
		}
		return strings.Compare(a.ObjectID, b.ObjectID)
	})

	// --- the findings view ---
	// claimsByVersion resolves the findings' pinned claim_version_refs:
	// every claim row of the project slice (the newest versions) plus the
	// explicitly fetched pinned claim rows, so a ref pinned to a
	// superseded claim version still resolves — Resolved=false is then
	// reserved for version ids that are not a claim of THIS project
	// (foreign rows never arrive here: the service filtered them).
	claimsByVersion := make(map[string]*row, len(objRows))
	for versionID, r := range byVersion {
		if r.obj.Object.ObjectType == "claim" {
			claimsByVersion[versionID] = r
		}
	}
	for i := range claimRefRows {
		r := &row{obj: claimRefRows[i], payload: parseOutlinePayload(claimRefRows[i].Version.Payload)}
		if _, ok := claimsByVersion[claimRefRows[i].Version.ID]; !ok {
			claimsByVersion[claimRefRows[i].Version.ID] = r
		}
	}
	out.Findings = make([]OutlineFinding, 0, len(findingIDs))
	for _, id := range findingIDs {
		r := byObject[id]
		f := OutlineFinding{
			ObjectID:    id,
			VersionID:   r.obj.Version.ID,
			BranchID:    branchIDOf(r.obj.Version),
			Title:       r.obj.Version.Title,
			Statement:   r.payload.statement,
			FindingType: r.payload.findingType,
			Assessment:  r.payload.assessment,
		}
		for _, versionID := range r.payload.claimVersionRefs {
			ref := OutlineClaimRef{VersionID: versionID}
			if claim, ok := claimsByVersion[versionID]; ok {
				ref.ObjectID = claim.obj.Object.ID
				ref.Title = claim.obj.Version.Title
				ref.BranchID = branchIDOf(claim.obj.Version)
				ref.Resolved = true
				// A claim referenced by a finding belongs to the findings
				// axis — it must not also land in the unassigned remainder.
				assigned[ref.ObjectID] = true
			}
			f.Claims = append(f.Claims, ref)
		}
		slices.SortFunc(f.Claims, func(a, b OutlineClaimRef) int {
			// Resolved claims (by title) before unresolved (by version id).
			if a.Resolved != b.Resolved {
				if a.Resolved {
					return -1
				}
				return 1
			}
			if c := strings.Compare(a.Title, b.Title); c != 0 {
				return c
			}
			return strings.Compare(a.VersionID, b.VersionID)
		})
		// The questions this finding addresses, as the source (deduped by
		// question object — two edges to different versions of the same
		// question are one question in the outline). Endpoints resolve by
		// container object for the same reason as the question side: an
		// edge pinned to a superseded version still counts.
		questionSeen := make(map[string]bool)
		for _, rel := range relRows {
			if rel.Version.RelationType != "addresses_question" {
				continue
			}
			source, ok := byObject[rel.Source.ObjectID]
			if !ok || source.obj.Object.ID != id {
				continue
			}
			target, ok := byObject[rel.Target.ObjectID]
			if !ok || target.obj.Object.ObjectType != "research_question" {
				continue
			}
			if questionSeen[target.obj.Object.ID] {
				continue
			}
			questionSeen[target.obj.Object.ID] = true
			f.Questions = append(f.Questions, OutlineObjectRef{
				ObjectID:   target.obj.Object.ID,
				ObjectType: target.obj.Object.ObjectType,
				Title:      target.obj.Version.Title,
				BranchID:   branchIDOf(target.obj.Version),
			})
		}
		sortOutlineRefs(f.Questions)
		out.Findings = append(out.Findings, f)
	}

	// --- the unassigned remainder ---
	for id, r := range byObject {
		if assigned[id] {
			continue
		}
		out.Unassigned = append(out.Unassigned, OutlineObjectRef{
			ObjectID:   id,
			ObjectType: r.obj.Object.ObjectType,
			Title:      r.obj.Version.Title,
			BranchID:   branchIDOf(r.obj.Version),
		})
	}
	sortOutlineRefs(out.Unassigned)

	// --- auxiliary counts ---
	for _, r := range byObject {
		switch r.obj.Object.ObjectType {
		case "research_question":
			out.Counts.Questions++
		case "finding":
			out.Counts.Findings++
		case "hypothesis":
			out.Counts.Hypotheses++
		case "claim":
			out.Counts.Claims++
		default:
			out.Counts.OtherObjects++
		}
	}
	return out
}

// outlineTreeRoots partitions the questions into roots and parent→children
// links, dropping the edge that would close a parent-reference cycle (its
// source becomes a root). Deterministic: ids are visited in sorted order.
func outlineTreeRoots(questionIDs []string, nodes map[string]*OutlineQuestion, childrenOf map[string][]string) []string {
	const (
		colorWhite = 0
		colorGray  = 1
		colorBlack = 2
	)
	color := make(map[string]int, len(questionIDs))
	roots := make([]string, 0)

	var visit func(id string)
	visit = func(id string) {
		color[id] = colorGray
		parent := nodes[id].ParentQuestionID
		if _, ok := nodes[parent]; !ok {
			// The parent is missing or outside the project: a root.
			color[id] = colorBlack
			roots = append(roots, id)
			return
		}
		if color[parent] == colorWhite {
			visit(parent)
		}
		if color[parent] == colorBlack {
			childrenOf[parent] = append(childrenOf[parent], id)
			color[id] = colorBlack
			return
		}
		// The parent is gray: it is on the walk in progress, so the
		// parent edge would close a cycle. Drop that edge — this question
		// renders as a root.
		color[id] = colorBlack
		roots = append(roots, id)
	}
	for _, id := range questionIDs {
		if color[id] == colorWhite {
			visit(id)
		}
	}
	return roots
}

// attachOutlineChildren appends the subtree rooted at id to dst, children
// sorted by title at every level.
func attachOutlineChildren(nodes map[string]*OutlineQuestion, childrenOf map[string][]string, id string, dst *[]OutlineQuestion) {
	node := nodes[id]
	childIDs := childrenOf[id]
	slices.SortFunc(childIDs, func(a, b string) int {
		if c := strings.Compare(nodes[a].Title, nodes[b].Title); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	for _, childID := range childIDs {
		attachOutlineChildren(nodes, childrenOf, childID, &node.Children)
	}
	sortOutlineRefs(node.Hypotheses)
	sortOutlineRefs(node.Findings)
	sortOutlineRefs(node.OtherObjects)
	*dst = append(*dst, *node)
}

// sortOutlineRefs orders object references by title (type badge is a
// per-row annotation — ordering by type would turn the list into a type
// grouping, which is exactly the dump shape the outline must not be).
func sortOutlineRefs(refs []OutlineObjectRef) {
	slices.SortFunc(refs, func(a, b OutlineObjectRef) int {
		if c := strings.Compare(a.Title, b.Title); c != 0 {
			return c
		}
		if c := strings.Compare(a.ObjectType, b.ObjectType); c != 0 {
			return c
		}
		return strings.Compare(a.ObjectID, b.ObjectID)
	})
}
