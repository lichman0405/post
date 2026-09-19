package search

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/rights"
)

// The projection's source readers (T0901): one query + one scan per entity
// type, each producing the Document the table in projection.go declares.
//
// # Why the queries live here and not in internal/persistence/queries
//
// The projection's two canonical queries already exist and are reused
// verbatim — the write is UpsertSearchDocument
// (internal/persistence/queries/search.sql:5-15, the ON CONFLICT that makes
// the projection idempotent) and the read stays SearchDocuments. What this
// file adds is the SOURCE half: the reads of the entities being indexed.
// They are plain SQL over pgx in the package that owns the projection, the
// shape internal/events uses for its fan-outs' claims and cmd/api/explorehttp
// uses for its explore reads (docs/52: explicit SQL, no ORM).
//
// # The scans are the only place a document is built
//
// Both the incremental path (projector.go) and the rebuild (rebuild.go) call
// the query + scan pairs below, so a rebuild cannot produce a document the
// event path would not: the pair is the definition of "what this entity
// indexes as", cited once.

// pidShape mirrors research_assets_pid_format (00064) and
// knowledge_publications_pid_format (00083): 26 Crockford base32 characters,
// the identity a citation resolves.
var pidShape = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

// uuidShape is PostgreSQL's own uuid text form (lower case, dashed).
var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// documentSource is one entity type's reader: the two queries and the scan
// that turns a row of either into a Document.
//
// byKey and every are the SAME projection over different row sets — the one
// the event names, and all of them (the rebuild). They are written as a
// shared prefix plus a different tail so the two cannot drift: an entity
// projected from its event and the same entity projected by a rebuild are
// built by one scan and one query body.
type documentSource struct {
	// byKey reads the one entity the named identity resolves to.
	byKey string
	// every reads every entity of this kind, in a deterministic order.
	every string
	// scan builds the Document from one row of either query.
	scan func(row rowScanner) (Document, error)
}

// rowScanner is the shared shape of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// documentSources maps every entity type to its reader. The unit suite pins
// that this map and the rule table cover exactly the same entity types: a
// rule without a reader would project nothing, and a reader without a rule
// would be a projection nobody triggers.
var documentSources = map[string]documentSource{
	EntityState:     stateSource(),
	EntityRelease:   releaseSource(),
	EntityAsset:     assetSource(),
	EntityKnowledge: knowledgeSource(),
}

// --------------------------------------------------------------------------
// A research asset

// assetSelectBody reads one asset with the project that owns it and the
// version the network currently sees (the LATEST published one).
//
// "Latest" rather than "the version this event named" is deliberate and is
// what lets the rebuild and the event path agree: both project the asset's
// CURRENT published state, so a second publish replaces the row (the
// upsert's ON CONFLICT) instead of leaving the asset indexed twice. The
// timestamp is the versions' published_at with the row id as the tie-break,
// so two versions published in one transaction still order deterministically.
const assetSelectBody = `
SELECT a.pid,
       a.title,
       a.slug,
       a.asset_type,
       p.id::text,
       p.visibility,
       v.version,
       v.visibility
FROM research_assets a
JOIN projects p ON p.id = a.origin_project_id
JOIN LATERAL (
    SELECT version, visibility
    FROM research_asset_versions
    WHERE asset_id = a.id
    ORDER BY published_at DESC, id DESC
    LIMIT 1
) v ON true`

func assetSource() documentSource {
	return documentSource{
		byKey: assetSelectBody + `
WHERE a.pid = $1`,
		every: assetSelectBody + `
ORDER BY a.pid`,
		scan: func(row rowScanner) (Document, error) {
			var (
				pid, title, slug, assetType string
				projectID, projectVis       string
				version, versionVis         string
			)
			if err := row.Scan(&pid, &title, &slug, &assetType, &projectID, &projectVis, &version, &versionVis); err != nil {
				return Document{}, err
			}
			structured, err := structuredFacets(EntityAsset, projectID, map[string]string{
				"pid":        pid,
				"slug":       slug,
				"asset_type": assetType,
				"version":    version,
			})
			if err != nil {
				return Document{}, err
			}
			return Document{
				EntityRef:  EntityRef(EntityAsset, pid),
				EntityType: EntityAsset,
				// The asset's own axis is the published VERSION's visibility
				// (research_asset_versions.visibility, 00064); a public asset
				// version in a private project is still not the network's.
				Visibility: projectedVisibility(versionVis == VisibilityPublic, projectVis),
				ProjectID:  projectID,
				Title:      title,
				Content:    joinContent(title, slug, assetType),
				Structured: structured,
			}, nil
		},
	}
}

// --------------------------------------------------------------------------
// A published knowledge object

// knowledgeSelectBody reads one publication with the version, the object and
// the project it belongs to — the exact four inputs
// knowledgepublish.AudienceFor decides with (its three axes: the version's
// own visibility_policy_id, the project's visibility, and the rights
// document's metadata axis).
//
// The rows the audience function cannot resolve are read rather than filtered
// out: an unreadable rights document must reach the decision as an
// unresolvable axis, not vanish from the query.
const knowledgeSelectBody = `
SELECT kp.pid,
       sov.title,
       so.object_type,
       kp.public_version,
       sov.lifecycle_state,
       p.id::text,
       p.visibility,
       sov.visibility_policy_id::text,
       kp.rights_json
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id`

func knowledgeSource() documentSource {
	return documentSource{
		byKey: knowledgeSelectBody + `
WHERE kp.pid = $1`,
		every: knowledgeSelectBody + `
ORDER BY kp.pid`,
		scan: func(row rowScanner) (Document, error) {
			var (
				pid, title, objectType, publicVersion, lifecycle string
				projectID, projectVis                            string
				policyID                                         *string
				rightsJSON                                       []byte
			)
			if err := row.Scan(&pid, &title, &objectType, &publicVersion, &lifecycle,
				&projectID, &projectVis, &policyID, &rightsJSON); err != nil {
				return Document{}, err
			}
			structured, err := structuredFacets(EntityKnowledge, projectID, map[string]string{
				"pid":             pid,
				"object_type":     objectType,
				"public_version":  publicVersion,
				"lifecycle_state": lifecycle,
			})
			if err != nil {
				return Document{}, err
			}
			return Document{
				EntityRef:  EntityRef(EntityKnowledge, pid),
				EntityType: EntityKnowledge,
				// The publication's own axis is not a column: it is the answer
				// the publish side already gives to "may the network read
				// this" (docs/12 §2, 发布不等于公开). The SAME function is
				// asked — not a second copy of the three-axis rule — so a
				// publication the read path refuses cannot be indexed as
				// public, and one the read path serves cannot be indexed as
				// private-only.
				Visibility: knowledgeProjectedVisibility(projectVis, policyID, rightsJSON),
				ProjectID:  projectID,
				Title:      title,
				Content:    joinContent(title, objectType, publicVersion),
				Structured: structured,
			}, nil
		},
	}
}

// knowledgeProjectedVisibility applies knowledgepublish.AudienceFor — the
// publication's own audience rule — and maps its two answers onto the
// projection's visibility vocabulary.
//
// A rights document this build cannot parse resolves NO axis, so it can
// never reach AudienceNetwork: parse gives the zero document, whose metadata
// axis is the empty string, which AudienceFor refuses. The failure is
// therefore already fail-closed one call down; the explicit branch here
// exists so that stays true if someone later teaches rights.Parse to be
// lenient.
func knowledgeProjectedVisibility(projectVisibility string, policyID *string, rightsJSON []byte) string {
	doc, err := rights.Parse(rightsJSON)
	if err != nil {
		return VisibilityPrivate
	}
	if knowledgepublish.AudienceFor(projectVisibility, policyID, doc) == knowledgepublish.AudienceNetwork {
		return VisibilityPublic
	}
	return VisibilityPrivate
}

// --------------------------------------------------------------------------
// A release

// releaseSelectBody reads one release with its project. A release has no
// visibility axis of its own (releases carries no such column, 00011): the
// project is the whole axis, which is why the scan passes ownAxisPublic=true
// and lets the project's visibility decide.
const releaseSelectBody = `
SELECT r.id::text,
       r.title,
       r.version,
       r.manifest_hash,
       p.id::text,
       p.visibility
FROM releases r
JOIN projects p ON p.id = r.project_id`

func releaseSource() documentSource {
	return documentSource{
		byKey: releaseSelectBody + `
WHERE r.id = $1::uuid`,
		every: releaseSelectBody + `
ORDER BY r.id`,
		scan: func(row rowScanner) (Document, error) {
			var (
				id, title, version, manifestHash string
				projectID, projectVis            string
			)
			if err := row.Scan(&id, &title, &version, &manifestHash, &projectID, &projectVis); err != nil {
				return Document{}, err
			}
			structured, err := structuredFacets(EntityRelease, projectID, map[string]string{
				"release_id":    id,
				"version":       version,
				"manifest_hash": manifestHash,
			})
			if err != nil {
				return Document{}, err
			}
			return Document{
				EntityRef:  EntityRef(EntityRelease, id),
				EntityType: EntityRelease,
				Visibility: projectedVisibility(true, projectVis),
				ProjectID:  projectID,
				Title:      title,
				Content:    joinContent(title, version),
				Structured: structured,
			}, nil
		},
	}
}

// --------------------------------------------------------------------------
// An accepted project state

// stateSelectBody reads one state with the commit that produced it, its
// branch and its project.
//
// The state's own axis is its BRANCH's visibility: it is the axis the RSG
// service records the state.committed event with
// (internal/application/rsg/events.go: eventVisibility(branch.Visibility)),
// so the projection and the event agree about how visible the transition
// was — and both are refused by the project's own visibility.
//
// The join to state_commits is what makes the set of projected states the
// set that was committed: a state is a state transition (docs/03), the
// genesis root carries no commit row (state_commits.branch_id is NOT NULL,
// internal/domain/state.go), and a rebuild must not invent an index entry
// for a row no transition ever announced.
const stateSelectBody = `
SELECT ps.id::text,
       sc.message,
       b.name,
       b.visibility,
       coalesce(ps.git_commit_sha, ''),
       p.id::text,
       p.visibility
FROM project_states ps
JOIN state_commits sc ON sc.result_state_id = ps.id
JOIN branches b ON b.id = ps.branch_id
JOIN projects p ON p.id = ps.project_id`

func stateSource() documentSource {
	return documentSource{
		byKey: stateSelectBody + `
WHERE ps.id = $1::uuid`,
		every: stateSelectBody + `
ORDER BY ps.id`,
		scan: func(row rowScanner) (Document, error) {
			var (
				id, message, branchName, branchVis, commitSHA string
				projectID, projectVis                         string
			)
			if err := row.Scan(&id, &message, &branchName, &branchVis, &commitSHA, &projectID, &projectVis); err != nil {
				return Document{}, err
			}
			structured, err := structuredFacets(EntityState, projectID, map[string]string{
				"state_id":   id,
				"branch":     branchName,
				"commit_sha": commitSHA,
			})
			if err != nil {
				return Document{}, err
			}
			return Document{
				EntityRef:  EntityRef(EntityState, id),
				EntityType: EntityState,
				Visibility: projectedVisibility(branchVis == VisibilityPublic, projectVis),
				ProjectID:  projectID,
				Title:      stateTitle(branchName, message),
				Content:    joinContent(message, branchName),
				Structured: structured,
			}, nil
		},
	}
}

// stateTitle renders a state's title: its commit message's first line, with
// the branch it was committed on. A state has no title column of its own —
// the sentence the author wrote IS what the transition is, and the branch
// says where.
func stateTitle(branchName, message string) string {
	first, _, _ := strings.Cut(message, "\n")
	first = strings.TrimSpace(first)
	if branchName == "" {
		return first
	}
	return "[" + branchName + "] " + first
}

// joinContent assembles a document's searchable text from the source
// fields, one per line, dropping the empty ones: the FTS index is over
// title || content, and a blank line per missing facet would only add
// noise. The fields are the entity's own — never the event payload's.
func joinContent(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

// errNoSource: a rule names an entity type with no reader. In production it
// is unreachable — the unit suite pins that the rule table and this map
// cover the same entity types — and it is a sentinel so a caller can tell
// "this build has no reader for that" from a query failure.
var errNoSource = errors.New("search: no source reader for entity type")

// sourceFor returns the reader for an entity type.
func sourceFor(entityType string) (documentSource, error) {
	src, ok := documentSources[entityType]
	if !ok {
		return documentSource{}, fmt.Errorf("%w %q", errNoSource, entityType)
	}
	return src, nil
}
