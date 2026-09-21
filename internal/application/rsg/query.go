package rsg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
)

// Query returns one RSG graph slice: the objects and relations of a
// project, filtered by type and pinned to a state or branch, optionally
// expanded by a depth-limited recursive relation traversal (T0209).
//
// Authorization (the task's rule verbatim): every read is authorized with
// the requireRead shape of internal/application/projects/service.go — the
// visibility-aware matrix action per project, public projects asking
// ActionReadPublicProject, private projects ActionReadPrivateProject (the
// membership role decides), evaluated through the project gate
// (projects.Service.Get IS requireRead). Three consequences:
//
//   - the entry gate runs first: a caller who may not read the project
//     (anonymous or non-member on a private project) answers
//     projects.ErrProjectNotFound — not-found, never forbidden — exactly
//     like an unknown project, so the project's existence (and every
//     relation in it) is not disclosed (read existence hiding, docs/45);
//   - every traversal hop re-runs the same per-project check: an edge or
//     node of another project enters the result only when its project
//     passes requireRead for this caller — filtering the seed alone is
//     the classic leak shape this rule exists to forbid;
//   - a seed edge is included only when both of its pinned endpoint
//     versions are CARRIED by a project this caller may read, and a hop
//     only when the projects it names (its own and both endpoints') are
//     visible: an edge whose pins or projects this caller cannot read
//     would otherwise leak its pinned version ids through its payload. The
//     seed is authorized by the pins' carriers and not by the containers
//     they hang on, because a seed edge is content this project's own
//     lineage carries — after a merged external fork the containers belong
//     to the contributor (ADR-027 Decisions 1-2; query.go's pin check).
//
// Slice pinning (L1): StateID (or BranchID → its head state) selects the
// "as-of" view — each object/relation renders at its newest version whose
// state is the pinned state or an ancestor of it (the state lineage). A
// state's direct members alone would be one transition's diff, not a
// graph slice. No pin = the project-wide view (each object/relation at its
// newest version overall).
//
// Selection (L1): no filters = the whole slice; ObjectTypes given = those
// objects plus the induced edges (relations whose both endpoints match);
// RelationTypes given = those relations plus their endpoint nodes;
// both = the intersection (matching edges whose endpoints match too).
// Depth > 0 expands from every node of the slice: each hop adds the edges
// touching the frontier version and their pinned neighbor versions, any
// type and any (visible) project, bidirectional, up to MaxQueryDepth.
//
// A store failure anywhere — including on a traversal hop — aborts the
// query (fail closed): the caller never receives a silently partial
// slice.
func (s *Service) Query(ctx context.Context, r projects.Reader, projectID string, in QueryInput) (QueryResult, error) {
	if projectID == "" {
		return QueryResult{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	// Entry gate first: the query is exactly as visible as its project.
	// A denied read answers projects.ErrProjectNotFound (the existence
	// hiding of the project surface) — acceptance: not-found, never
	// forbidden.
	if _, err := s.projects.Get(ctx, r, projectID); err != nil {
		return QueryResult{}, wrapError(err)
	}
	in, err := normalizeQueryInput(in)
	if err != nil {
		return QueryResult{}, err
	}
	if s.queries == nil {
		return QueryResult{}, fmt.Errorf("%w: no query port configured", ErrStore)
	}

	lineage, stateID, branchID, err := s.resolveSlice(ctx, projectID, in)
	if err != nil {
		return QueryResult{}, err
	}

	vis := newQueryVisibility(s.projects, r)
	vis.seen[projectID] = true // the entry gate authorized the query project

	// Candidates: the project's objects and relations at their as-of
	// versions. Object types are pushed down to SQL; the relation list is
	// unfiltered here because the induced-edge rule needs the endpoint
	// types of every relation of the slice.
	objRows, err := s.queries.ListObjectVersions(ctx, projectID, in.ObjectTypes, lineage)
	if err != nil {
		return QueryResult{}, wrapError(err)
	}
	relRows, err := s.queries.ListRelationVersions(ctx, projectID, nil, lineage)
	if err != nil {
		return QueryResult{}, wrapError(err)
	}

	objectFilter := len(in.ObjectTypes) > 0
	relationFilter := len(in.RelationTypes) > 0

	// Nodes: a selected object renders at its as-of version; a relation
	// endpoint renders at the exact pinned version. nodeRows holds the
	// rows already in hand; pending holds the version ids whose rows are
	// batch-fetched once at the end (endpoints and traversal neighbors).
	nodeRows := make([]ObjectQueryRow, 0, len(objRows))
	nodeByVersion := make(map[string]bool, len(objRows))
	pending := map[string]bool{}
	addNode := func(versionID string) bool {
		if nodeByVersion[versionID] || pending[versionID] {
			return false
		}
		pending[versionID] = true
		return true
	}

	typeMatched := make(map[string]bool, len(objRows))
	for _, row := range objRows {
		matched := !objectFilter || slices.Contains(in.ObjectTypes, row.Object.ObjectType)
		typeMatched[row.Object.ID] = matched
		if (!objectFilter && !relationFilter) || (objectFilter && matched) {
			nodeByVersion[row.Version.ID] = true
			nodeRows = append(nodeRows, row)
		}
	}

	// Relation selection. An edge is kept when it passes the type
	// filters, the induced-edge rule, and the pin authorization below.
	//
	// The seed rows are the edges THIS project's lineage carries (the read
	// above is lineage-scoped, and projectID passed the entry gate), so the
	// authorization cannot run on the containers the rows wear: after a merge
	// accepted an external fork's proposal the relation row and its "own"
	// object are the contributor's, while the pins are versions the merge
	// wrote into this project's states (ADR-027 Decisions 1-2 — the state
	// carries, the project authorizes, `project_id` is not the criterion for
	// what is in this RSG). Authorizing on the container hid the project's own
	// accepted content from it, which is the defect T0818 fixes.
	//
	// What stays checkable, and what a payload of pinned version ids could
	// otherwise disclose, is each pin's CARRIER: an endpoint version carried by
	// a state of a project this caller cannot read prunes the edge (defensive
	// — the merge refuses to write an edge to a version the accepted state does
	// not contain, but a direct store write need not). For every pin written in
	// the project owning its container the two rules agree.
	relationResults := make([]RelationResult, 0, len(relRows))
	seenEdges := make(map[string]bool, len(relRows))
	for _, row := range relRows {
		if relationFilter && !slices.Contains(in.RelationTypes, row.Version.RelationType) {
			continue
		}
		if objectFilter && !(typeMatched[row.Source.ObjectID] && typeMatched[row.Target.ObjectID]) {
			continue
		}
		visible, err := vis.pins(ctx, row.Source.CarriedBy, row.Target.CarriedBy)
		if err != nil {
			return QueryResult{}, err
		}
		if !visible {
			continue
		}
		seenEdges[row.Relation.ID] = true
		relationResults = append(relationResults, RelationResult{Relation: row.Relation, Version: row.Version})
		addNode(row.Source.VersionID)
		addNode(row.Target.VersionID)
	}

	// Recursive traversal: expand from every node of the slice, one BFS
	// level per hop, each hop re-authorized (the per-hop project checks).
	if in.Depth > 0 {
		frontier := make([]string, 0, len(nodeByVersion)+len(pending))
		for vid := range nodeByVersion {
			frontier = append(frontier, vid)
		}
		for vid := range pending {
			frontier = append(frontier, vid)
		}
		for level := 0; level < in.Depth && len(frontier) > 0; level++ {
			hops, err := s.queries.ListAdjacentRelationVersions(ctx, frontier, lineage)
			if err != nil {
				return QueryResult{}, wrapError(err)
			}
			next := make([]string, 0, len(hops))
			frontierSet := toSet(frontier)
			for _, hop := range hops {
				visible, err := vis.edge(ctx, hop.Relation.ProjectID, hop.Source.ProjectID, hop.Target.ProjectID)
				if err != nil {
					return QueryResult{}, err
				}
				if !visible {
					continue
				}
				if !seenEdges[hop.Relation.ID] {
					seenEdges[hop.Relation.ID] = true
					relationResults = append(relationResults, RelationResult{Relation: hop.Relation, Version: hop.Version})
				}
				// The neighbor is the endpoint that was not already on
				// the frontier (an edge between two frontier nodes
				// contributes no new node).
				srcIn, tgtIn := frontierSet[hop.Source.VersionID], frontierSet[hop.Target.VersionID]
				neighbor := hop.Source
				if srcIn && !tgtIn {
					neighbor = hop.Target
				}
				if addNode(neighbor.VersionID) {
					next = append(next, neighbor.VersionID)
				}
			}
			frontier = next
		}
	}

	// Batch-fetch the pinned rows of every node that entered through an
	// endpoint or a traversal hop (rendering only — their projects
	// already passed the per-hop checks above).
	if len(pending) > 0 {
		ids := make([]string, 0, len(pending))
		for vid := range pending {
			ids = append(ids, vid)
		}
		rows, err := s.queries.ListObjectVersionsByIDs(ctx, ids)
		if err != nil {
			return QueryResult{}, wrapError(err)
		}
		for _, row := range rows {
			if !nodeByVersion[row.Version.ID] {
				nodeByVersion[row.Version.ID] = true
				nodeRows = append(nodeRows, row)
			}
		}
	}

	objects := make([]ObjectResult, 0, len(nodeRows))
	for _, row := range nodeRows {
		objects = append(objects, ObjectResult{Object: row.Object, Version: row.Version})
	}
	sortObjectResults(objects)
	sortRelationResults(relationResults)
	return QueryResult{
		ProjectID: projectID,
		StateID:   stateID,
		BranchID:  branchID,
		Objects:   objects,
		Relations: relationResults,
	}, nil
}

// normalizeQueryInput verifies the query shape and normalizes the type
// lists (dedupe, no empties, token shape). Type names that are well-formed
// but unknown simply match nothing — no write through the ports can store
// an object/relation type outside the canonical catalogs, so an unknown
// type has no rows by construction (same contract as
// relations.ListVersionsByType).
func normalizeQueryInput(in QueryInput) (QueryInput, error) {
	if in.StateID != "" && in.BranchID != "" {
		return in, fmt.Errorf("%w: state_id and branch_id are mutually exclusive", ErrValidation)
	}
	if in.Depth < 0 || in.Depth > MaxQueryDepth {
		return in, fmt.Errorf("%w: depth must be between 0 and %d", ErrValidation, MaxQueryDepth)
	}
	types, err := normalizeTypeTokens(in.ObjectTypes)
	if err != nil {
		return in, fmt.Errorf("%w: object_type: %v", ErrValidation, err)
	}
	in.ObjectTypes = types
	types, err = normalizeTypeTokens(in.RelationTypes)
	if err != nil {
		return in, fmt.Errorf("%w: relation_type: %v", ErrValidation, err)
	}
	in.RelationTypes = types
	return in, nil
}

// normalizeTypeTokens dedupes a type list and bounds every token to the
// canonical snake_case shape (a path or URI can never name a type). An
// empty list comes back nil — the port contract says nil = no filter,
// never "an empty filter that matches nothing".
func normalizeTypeTokens(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, errors.New("empty type token")
		}
		if !typeTokenRe.MatchString(t) {
			return nil, fmt.Errorf("%q is not a valid type name", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// resolveSlice turns the query's state/branch pin into the lineage the
// as-of reads filter on. An explicit state that is missing or belongs to
// another project answers states.ErrStateNotFound — the same outcome for
// both, so no state existence leaks (docs/45). A branch is project-verified
// through the branch gate, then resolves to its head state's lineage. No
// pin = nil lineage = the project-wide newest-version slice.
func (s *Service) resolveSlice(ctx context.Context, projectID string, in QueryInput) (lineage []string, stateID, branchID string, err error) {
	switch {
	case in.StateID != "":
		lineage, err = s.queries.ListStateLineage(ctx, projectID, in.StateID)
		if err != nil {
			return nil, "", "", wrapError(err)
		}
		if len(lineage) == 0 {
			return nil, "", "", states.ErrStateNotFound
		}
		return lineage, in.StateID, "", nil
	case in.BranchID != "":
		if _, err := s.branches.Get(ctx, projectID, in.BranchID); err != nil {
			return nil, "", "", wrapError(err)
		}
		head, err := s.states.GetBranchHead(ctx, in.BranchID)
		if err != nil {
			return nil, "", "", wrapError(err)
		}
		if head.ProjectID != projectID {
			// A branch of another project's head is not a slice of this
			// project (defensive: the branch gate checked the branch row).
			return nil, "", "", states.ErrStateNotFound
		}
		lineage, err = s.queries.ListStateLineage(ctx, projectID, head.ID)
		if err != nil {
			return nil, "", "", wrapError(err)
		}
		if len(lineage) == 0 {
			return nil, "", "", states.ErrStateNotFound
		}
		return lineage, head.ID, in.BranchID, nil
	default:
		return nil, "", "", nil
	}
}

// queryVisibility resolves and caches the per-project requireRead outcome
// of one query run: the project gate IS requireRead
// (projects.Service.Get), so every project is checked exactly once per
// query. A denied or unknown project is invisible (pruned from the slice);
// a store failure is returned as an error so the caller fails closed.
type queryVisibility struct {
	projects ProjectGate
	reader   projects.Reader
	seen     map[string]bool
}

func newQueryVisibility(p ProjectGate, r projects.Reader) *queryVisibility {
	return &queryVisibility{projects: p, reader: r, seen: map[string]bool{}}
}

// project reports whether the caller may read the project (requireRead
// shape). ErrProjectNotFound — denied or unknown — means invisible.
func (v *queryVisibility) project(ctx context.Context, projectID string) (bool, error) {
	if seen, ok := v.seen[projectID]; ok {
		return seen, nil
	}
	if v.projects == nil {
		return false, fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	_, err := v.projects.Get(ctx, v.reader, projectID)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			v.seen[projectID] = false
			return false, nil
		}
		return false, wrapError(err)
	}
	v.seen[projectID] = true
	return true, nil
}

// pins reports whether one seed edge's pinned endpoint versions may enter the
// slice: the project that CARRIES each pinned version must pass requireRead.
// A pin whose carriage this caller cannot read — including an empty carriage,
// which names no readable project — prunes the edge, because an included edge
// discloses its pins' version ids through its payload.
//
// It deliberately does not look at the containers the pinned versions hang on:
// for a version this project's lineage carries, the container is reported as
// it is (ADR-027 Decision 1), so gating the seed on the container hides the
// project's own accepted content from it — the T0818 defect. The traversal
// keeps the container rule (edge, below): its hops are not lineage-scoped, so
// the projects a hop names are the only ones there are to authorize.
func (v *queryVisibility) pins(ctx context.Context, carriers ...string) (bool, error) {
	for _, p := range carriers {
		visible, err := v.project(ctx, p)
		if err != nil {
			return false, err
		}
		if !visible {
			return false, nil
		}
	}
	return true, nil
}

// edge reports whether one traversal hop may enter the slice: the relation's
// own project and both endpoint projects must each pass the per-hop
// requireRead. A hidden project prunes the hop — an included hop would
// otherwise disclose the hidden project's pinned version ids through its
// payload. Hops are the rows the caller has NOT already been authorized for by
// its own state lineage (the adjacency read is not project-filtered), which is
// why the container rule lives here and the carrier rule in pins.
func (v *queryVisibility) edge(ctx context.Context, edgeProject, sourceProject, targetProject string) (bool, error) {
	for _, p := range []string{edgeProject, sourceProject, targetProject} {
		visible, err := v.project(ctx, p)
		if err != nil {
			return false, err
		}
		if !visible {
			return false, nil
		}
	}
	return true, nil
}

// toSet indexes a version id list for membership tests.
func toSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// sortObjectResults orders the slice deterministically (oldest first, id
// as the tie-breaker — the same ordering the underlying list queries use).
func sortObjectResults(results []ObjectResult) {
	slices.SortFunc(results, func(a, b ObjectResult) int {
		if c := a.Version.CreatedAt.Compare(b.Version.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Version.ID, b.Version.ID)
	})
}

// sortRelationResults orders the edges the same way.
func sortRelationResults(results []RelationResult) {
	slices.SortFunc(results, func(a, b RelationResult) int {
		if c := a.Version.CreatedAt.Compare(b.Version.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Version.ID, b.Version.ID)
	})
}
