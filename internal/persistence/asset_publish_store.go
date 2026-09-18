package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// AssetPublishStore is the production adapter for the asset publish
// command (T0705): the Idempotency-Key ledger read the command checks
// before doing any work, and the publish transaction itself.
//
// # What the transaction is, and why the gate runs INSIDE it
//
// docs/22 §7 requires a command to re-run its own gate server-side rather
// than trust a precheck, and docs/23 §4 makes the impact preview part of
// the publication decision. The publish therefore resolves the repository
// state, re-runs assets.Gate and assets.Preview over it, and only then
// writes — but "only then" is not enough on its own: between a check and
// an insert, another transaction can make a private dependency public, or
// attach a blob openly, or publish the version this one is about to. So
// the whole sequence runs in ONE transaction, under the project row lock
// every membership and governance write takes (GetProjectByIDForUpdate):
//
//	lock the project  →  re-check the ledger (replay is a read)
//	                  →  resolve the current state
//	                  →  re-run Gate and Preview over it
//	                  →  refuse (writing nothing), or
//	                     insert the asset row (a create only)
//	                     insert the immutable version row
//	                     insert the version's declared credits
//	                     insert the ledger row
//	                     append the audit row
//	                     record the research event and its outbox row
//
// The lock is what makes the re-check meaningful: a concurrent publish of
// the same asset takes the same lock, so it cannot slip a version in
// between this one's preview and this one's insert.
//
// # What a refusal leaves behind: nothing
//
// A refused publish returns *assetpublish.PublishRefused, which carries
// the COMPLETE impact preview it was decided on (docs/23 §4: the caller
// is told which dependency it must fix, not that it may not publish). The
// transaction has written nothing at that point, and the error unwinds it
// — with one exception worth stating: when the refusal happens AFTER the
// asset insert (a create whose version turns out to collide), the insert
// is rolled back with the rest, so a refused publish never leaves an empty
// asset behind.
//
// # The version row is immutable and only ever INSERTed
//
// research_asset_versions carries the 00014 append-only row trigger, so an
// UPDATE or DELETE of an existing version is refused by the database
// itself. This store never attempts one: a repeat publication of the same
// (asset, version) is answered from the UNIQUE(asset_id, version) guard as
// assetpublish.ErrVersionImmutable, and the row that already exists is
// left exactly as it is.
//
// # The ladder's asset gate runs here too, over the row about to be written
//
// assets.Gate is the publish checklist of docs/11 §3 as a domain verdict;
// the validation ladder is the same checklist as a gate spec, and its
// asset gate adds the one check no fact can carry — asset_schema, which
// validates the research-asset-version ENTITY DOCUMENT. That document has
// no column of its own: it is rendered from the values this transaction is
// about to write (see renderAssetVersionDocument), and it is rendered
// here, in the transaction, so what the ladder validates is the row that
// lands and not a description of it. Leaving the caller-owned
// GateResult.Facts.Document/.Ref empty would fail the check closed, which
// is the right direction but a silent one; filling them from the row makes
// the check mean what it says (T0702's RESULT risk 3).
type AssetPublishStore struct {
	pool *pgxpool.Pool
	val  *rsgvalidation.Validator
}

// NewAssetPublishStore builds the store on pool, with the ladder's gate
// engine for the asset-document check. The pool may be lazy (OpenLazy):
// the API keeps starting while PostgreSQL is down.
func NewAssetPublishStore(pool *pgxpool.Pool, val *rsgvalidation.Validator) *AssetPublishStore {
	return &AssetPublishStore{pool: pool, val: val}
}

// LookupCreation implements assetpublish.StorePort: the version an
// Idempotency-Key already published, or nil when the key has no ledger
// entry yet. The command checks it before resolving anything, so a replay
// never re-runs a gate (a replay is a read).
//
// A project id that is not a uuid text cannot have published anything: the
// key's scope is a project row, and answering "no entry" for one that
// cannot exist is the truth rather than an error. The authorization has
// already run by the time this is called, so this cannot be used to probe.
func (s *AssetPublishStore) LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*assetpublish.PublishedVersion, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	q := sqlc.New(s.pool)
	versionID, err := q.GetAssetPublishCreation(ctx, sqlc.GetAssetPublishCreationParams{
		ProjectID:      pID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read asset publish creation: %w", err)
	}
	row, err := q.GetPublishedAssetVersion(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("persistence: read published asset version: %w", err)
	}
	out := publishedVersionFromRow(row)
	return &out, nil
}

// Publish implements assetpublish.StorePort. See the type doc for the
// transaction it runs; this method is the transaction.
func (s *AssetPublishStore) Publish(ctx context.Context, req assetpublish.PublishRequest) (assetpublish.PublishedVersion, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return assetpublish.PublishedVersion{}, assetpublish.ErrProjectNotFound
	}
	publishedBy, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return assetpublish.PublishedVersion{}, fmt.Errorf("%w: publishing actor id: %v", assetpublish.ErrStore, err)
	}
	// One correlation id for the whole publish: the request's when the
	// observability middleware attached one, else a fresh one — the same
	// fallback the release store uses, so the audit row, the research
	// event and the outbox row always share one trace id.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return assetpublish.PublishedVersion{}, fmt.Errorf("%w: publish correlation id: %v", assetpublish.ErrStore, err)
		}
		correlationID = id.String()
	}

	var out assetpublish.PublishedVersion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		project, err := q.GetProjectByIDForUpdate(ctx, projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assetpublish.ErrProjectNotFound
			}
			return err
		}

		// The ledger, re-checked under the project lock. The command's
		// LookupCreation ran outside any transaction, so it is an
		// optimization; this read is the guarantee, because the lock
		// serializes every publish of this project.
		if req.IdempotencyKey != nil {
			versionID, err := q.GetAssetPublishCreation(ctx, sqlc.GetAssetPublishCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
			})
			switch {
			case err == nil:
				row, err := q.GetPublishedAssetVersion(ctx, versionID)
				if err != nil {
					return err
				}
				out = publishedVersionFromRow(row)
				return nil
			case errors.Is(err, pgx.ErrNoRows):
				// first publish with this key — fall through to the work
			default:
				return err
			}
		}

		// The state, the gate and the preview, over THIS transaction's
		// view of the repository. The resolver is the same one the
		// read-only preview route uses (one definition of "does this pin
		// resolve"), built over the transaction instead of the pool.
		state, err := NewAssetStateStore(tx).ResolvePreviewState(ctx, req.Candidate)
		if err != nil {
			return err
		}
		if req.NewAsset && state.Asset != nil {
			return assetpublish.ErrAssetExists
		}
		preview, err := assets.Preview(assets.PreviewRequest{ProjectID: req.ProjectID, Candidate: req.Candidate}, state)
		if err != nil {
			return err
		}
		if reasons := refusalReasons(preview, req.NewAsset); len(reasons) > 0 {
			return &assetpublish.PublishRefused{Preview: preview, Reasons: reasons}
		}
		// The gate the preview ran refused nothing (a refusal would be in
		// PublishBlockers), so the candidate's canonical manifest bytes
		// are available — and they are what the row stores, so the hash
		// the version is published under covers exactly the bytes a reader
		// will read back (T0702's residual risk 2).
		gate, err := assets.Gate(req.Candidate)
		if err != nil {
			// Unreachable: every refusal Gate raises is also a preview
			// blocker, and the blockers above refused first. It is kept
			// because "unreachable" is a claim about two components
			// agreeing, and a claim that is checked costs one call.
			return &assetpublish.PublishRefused{
				Preview: preview,
				Reasons: []string{"the publish gate refused the candidate: " + err.Error()},
			}
		}
		// The rights document the row stores, rendered once: the column
		// takes these bytes and the entity document the ladder validates
		// embeds them, so the two can never disagree.
		rightsJSON, err := rightsBytes(gate)
		if err != nil {
			return err
		}
		// The ladder's asset gate, over the entity document of the row
		// about to be written — the last check before the INSERT, and the
		// only one that reads the document itself.
		reasons, err := requirePublishedAssetDocument(s.val, req.ProjectID, req.Candidate, gate, rightsJSON)
		if err != nil {
			return err
		}
		if len(reasons) > 0 {
			return &assetpublish.PublishRefused{Preview: preview, Reasons: reasons}
		}

		assetID, assetPID, err := s.resolveAsset(ctx, q, req, state, preview)
		if err != nil {
			return err
		}
		version, err := q.PublishResearchAssetVersion(ctx, sqlc.PublishResearchAssetVersionParams{
			AssetID:         assetID,
			Version:         req.Candidate.Version,
			SourceReleaseID: sourceReleaseID(req.Candidate.OriginRefs),
			Manifest:        gate.ManifestJSON,
			RightsJson:      rightsJSON,
			Visibility:      string(req.Candidate.Visibility),
			IntegrityHash:   req.Candidate.IntegrityHash,
			PublishedBy:     publishedBy,
			OriginRefs:      req.Candidate.OriginRefs,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				strings.Contains(pgErr.ConstraintName, "research_asset_versions_asset_id_version") {
				return assetpublish.ErrVersionImmutable
			}
			return err
		}
		// The version's declared credits, in this same transaction: a
		// version row that committed carries exactly the creators its
		// publish declared, and a publish that rolls back leaves none
		// behind (T0711 — 00082's asset_version_parties; the gap
		// internal/assets/page.go named was that the gate validated these
		// ids and no table stored them).
		if err := insertVersionCredits(ctx, q, version.ID, publishedBy, req.Candidate.CreatorIDs); err != nil {
			return err
		}
		out = assetpublish.PublishedVersion{
			ID:            pgUUIDToText(version.ID),
			AssetID:       pgUUIDToText(version.AssetID),
			AssetPID:      assetPID,
			Version:       version.Version,
			Visibility:    assets.Visibility(version.Visibility),
			IntegrityHash: version.IntegrityHash,
			OriginRefs:    version.OriginRefs,
			PublishedBy:   pgUUIDToText(version.PublishedBy),
			PublishedAt:   version.PublishedAt.Time,
			Manifest:      json.RawMessage(version.Manifest),
			RightsJSON:    json.RawMessage(version.RightsJson),
		}

		// The usages the version declares (T0707): a project using an exact
		// asset version is recorded HERE, inside the publish's transaction, so
		// the row exists exactly when the version that declares it does. The
		// declarations are the command's (assets.PublishedUsages over the
		// manifest's pins); what this store adds is the resolution the
		// transaction owns — the pin → version ROW id the usage is keyed by —
		// read from the same resolution the preview above ran, so a pin that
		// resolves to nothing records nothing rather than half an identity.
		if err := recordUsageDeclarations(ctx, q, projectID, req.Usages, state.Pins); err != nil {
			return err
		}

		if req.IdempotencyKey != nil {
			versionID, err := textUUID(out.ID)
			if err != nil {
				return err
			}
			if _, err := q.CreateAssetPublishCreation(ctx, sqlc.CreateAssetPublishCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
				AssetVersionID: versionID,
			}); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
					strings.Contains(pgErr.ConstraintName, "asset_publish_creations_project_id_idempotency_key") {
					// Unreachable under the project lock (two publishes
					// with one key serialize, so the second reads the
					// first's ledger row above), and mapped rather than
					// wrapped because if it ever DID fire, what happened
					// is exactly what IDEMPOTENCY_CONFLICT names: the key
					// is already taken.
					return assetpublish.ErrIdempotencyConflict
				}
				return err
			}
		}

		// The audit row names the assigned version id — the store writes
		// it after the insert, so the command left the target ref empty.
		audit := req.Audit
		audit.TargetRef = "asset_version:" + out.ID
		audit.CorrelationID = correlationID
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return recordAssetVersionPublished(ctx, q, project.Visibility, publishedBy, projectID, correlationID, out)
	})
	if err != nil {
		return assetpublish.PublishedVersion{}, mapAssetPublishError(err)
	}
	return out, nil
}

// recordUsageDeclarations writes the asset_dependencies rows one publish
// declares (T0707): the project's use of each pinned version, keyed by that
// version's ROW id.
//
// The resolution it needs is the one the transaction already ran: resolved
// carries one entry per pin that names a stored version (assets.StoredPin,
// read by the same AssetStateStore the preview above used), with the row id
// beside the visibility. A declaration whose pin is not in that set names no
// version the repository holds, so there is no asset_version_id to write and
// the row is skipped — the pin itself is still rendered by the page as an
// unresolved dependency, which is the honest form of that fact.
//
// One INSERT per declaration, not a batch: a publish declares a handful of
// versions, the statement is a single-row upsert (RecordAssetDependency),
// and they run inside the transaction that already holds the project's lock.
// The upsert makes the write idempotent for the case the primary key
// collapses — two versions of one asset pinning the same version — which is
// why the same project republishing a usage is one row rather than a
// conflict.
func recordUsageDeclarations(ctx context.Context, q *sqlc.Queries, projectID pgtype.UUID, declarations []assets.UsageDeclaration, resolved []assets.StoredPin) error {
	if len(declarations) == 0 {
		return nil
	}
	versionIDByPin := make(map[assets.DependencyPin]string, len(resolved))
	for _, pin := range resolved {
		if pin.VersionID != "" {
			versionIDByPin[pin.Pin] = pin.VersionID
		}
	}
	for _, decl := range declarations {
		versionID, ok := versionIDByPin[decl.Pin]
		if !ok {
			continue
		}
		assetVersionID, err := textUUID(versionID)
		if err != nil {
			return fmt.Errorf("persistence: record asset usage: resolved version id %q: %w", versionID, err)
		}
		if err := q.RecordAssetDependency(ctx, sqlc.RecordAssetDependencyParams{
			ProjectID:         projectID,
			AssetVersionID:    assetVersionID,
			DependencyType:    string(decl.Type),
			VisibilityOfUsage: string(decl.VisibilityOfUsage),
		}); err != nil {
			return fmt.Errorf("persistence: record asset usage %q: %w", decl.Pin, err)
		}
	}
	return nil
}

// resolveAsset decides which research_assets row the version belongs to
// and returns its id and pid: the one the create inserted, or the one the
// candidate's pid names (which the preview above has already resolved, and
// already checked for type and project, so a mismatch never reaches here).
func (s *AssetPublishStore) resolveAsset(ctx context.Context, q *sqlc.Queries, req assetpublish.PublishRequest, state assets.CurrentState, preview assets.ImpactPreview) (pgtype.UUID, string, error) {
	if !req.NewAsset {
		if state.Asset == nil {
			// Unreachable: PREVIEW_ASSET_UNKNOWN is a blocker and the
			// refusal above fired. Kept as the honest answer for the case
			// where the two components' rules were edited apart.
			return pgtype.UUID{}, "", assetpublish.ErrAssetNotFound
		}
		assetID, err := textUUID(state.Asset.ID)
		if err != nil {
			return pgtype.UUID{}, "", fmt.Errorf("%w: stored asset id %q: %v", assetpublish.ErrStore, state.Asset.ID, err)
		}
		return assetID, string(state.Asset.PID), nil
	}
	row, err := q.CreateResearchAssetWithPID(ctx, sqlc.CreateResearchAssetWithPIDParams{
		AssetType:       string(req.Candidate.AssetType),
		Slug:            req.Slug,
		Title:           req.Title,
		OriginProjectID: mustProjectUUID(preview.ProjectID),
		Pid:             string(req.Candidate.AssetPID),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			strings.Contains(pgErr.ConstraintName, "research_assets_pid") {
			return pgtype.UUID{}, "", assetpublish.ErrAssetExists
		}
		return pgtype.UUID{}, "", err
	}
	return row.ID, row.Pid, nil
}

// mustProjectUUID converts the project id the transaction already
// validated (GetProjectByIDForUpdate succeeded for it) back to the pgtype
// form. The conversion cannot fail, and a zero uuid on a hypothetical
// failure would write the asset into no project — so the failure branch
// returns the zero value and the insert's NOT NULL foreign key refuses it.
func mustProjectUUID(projectID string) pgtype.UUID {
	u, err := textUUID(projectID)
	if err != nil {
		return pgtype.UUID{}
	}
	return u
}

// refusalReasons decides whether the preview refuses the publication, and
// names what it refused over.
//
// Every reason is taken FROM the preview — the refusal type carries the
// preview itself, and these lines are a reading of it, never a second
// opinion about it.
//
// Three lists are read, and they are the three assets.Preview computes for
// this decision:
//
//   - RightsBlockers: what the rights declaration itself refuses. A
//     publication whose rights document is refused cannot be stored: the
//     rights document is immutable once published.
//   - PublishBlockers: the candidate's own refusals — the publish
//     checklist (docs/11 §3) and the resolutions that failed. ONE of them
//     is tolerated, and only when this publish CREATES the asset:
//     PREVIEW_ASSET_UNKNOWN ("the pid names no stored asset"). For a
//     create that is not a refusal, it is the description of the work —
//     the asset does not exist yet because this publish is what creates
//     it, and the pid was minted a moment ago by assets.NewPID. For a
//     request that NAMED a pid it is a refusal, and the asset is not
//     silently created under an identity the caller supplied.
//   - the BLOCKING entries of PrivateDependencies: docs/23 §4's no-hidden
//     private dependency leak, which is the whole reason this re-check
//     exists. An entry that is private but not blocking (a blob that is
//     not open while the declaration does not promise open data, a pin
//     whose target this publication would not widen) is reported to the
//     caller and does NOT stop the publish — the preview's own verdict
//     (ImpactPreview.Publishable) is the same conjunction.
func refusalReasons(preview assets.ImpactPreview, newAsset bool) []string {
	var reasons []string
	for _, b := range preview.RightsBlockers {
		reasons = append(reasons, "rights: "+b.Code+": "+b.Detail)
	}
	for _, b := range preview.PublishBlockers {
		if newAsset && b.Code == assets.CodePreviewAssetUnknown {
			continue
		}
		reasons = append(reasons, b.Code+": "+b.Detail)
	}
	for _, dep := range preview.PrivateDependencies {
		if !dep.Blocking {
			continue
		}
		reasons = append(reasons, "private dependency ("+string(dep.Kind)+" "+dep.Ref+"): "+dep.Detail)
	}
	return reasons
}

// sourceReleaseID extracts the release a version was published from, when
// its origin refs name one. research_asset_versions.source_release_id is
// the typed form of that pin (nullable — a version published from an
// accepted state names no release), and it is derived from the refs the
// row stores rather than from a second field a caller could contradict
// them with.
func sourceReleaseID(refs []string) pgtype.UUID {
	for _, raw := range refs {
		kind, value, ok := assets.ParseOriginRef(raw)
		if !ok || kind != assets.KindRelease {
			continue
		}
		if u, err := textUUID(value); err == nil {
			return u
		}
	}
	return pgtype.UUID{}
}

// rightsBytes renders the stored rights document. gate.Rights parsed from
// the candidate's bytes, and the row stores the candidate's canonical
// rights form — the parse is what proved it readable, not a re-render.
//
// A marshal failure is reported rather than fallen back from: the column
// is NOT NULL, and a nil document would be a silently different row (and,
// in the entity document below, a `"rights": null` the schema refuses for
// a reason that has nothing to do with the caller).
func rightsBytes(gate assets.GateResult) ([]byte, error) {
	raw, err := gate.Rights.Marshal()
	if err != nil {
		return nil, fmt.Errorf("%w: render the rights document: %v", assetpublish.ErrStore, err)
	}
	return raw, nil
}

// publishedAssetDocumentSchema addresses the entity schema the published
// version document is validated against: the canonical
// research-asset-version schema (specs/schemas/), unless an extension
// registers its own — the ref the asset gate's asset_schema check reads
// off validation.AssetFacts.Ref.
func publishedAssetDocumentSchema() schemareg.Ref {
	return schemareg.Ref{
		ID:      schemareg.CanonicalNamespace + "research-asset-version.schema.json",
		Version: schemareg.CanonicalV1,
	}
}

// assetVersionDocument is the research-asset-version entity document of
// one published version (specs/schemas/research-asset-version.schema.json):
// the row's own fields, plus the dependency pins the manifest declares,
// plus the stored rights document. The schema is closed
// (additionalProperties: false), so this type is the whole document and
// adding a field here is a change to the published entity shape.
//
// id, project_id, created_by and created_at are absent deliberately: they
// are server-authoritative row facts the v1 schema does not require of the
// entity document, and the fields that ARE here are the published facts a
// later reader re-verifies (the pid and the label that name the version,
// the provenance pins, the rights declaration, the hash, the credit).
type assetVersionDocument struct {
	AssetID        string                 `json:"asset_id"`
	Version        string                 `json:"version"`
	AssetType      assets.Type            `json:"asset_type"`
	OriginRefs     []string               `json:"origin_refs"`
	Rights         json.RawMessage        `json:"rights"`
	IntegrityHash  string                 `json:"integrity_hash"`
	CreatorIDs     []string               `json:"creator_ids"`
	DependencyRefs []assets.DependencyPin `json:"dependency_refs"`
}

// renderAssetVersionDocument renders the entity document from exactly the
// values this publish is about to write: the pid and label the row is
// keyed by, the type, the provenance pins the origin_refs column stores,
// the rights bytes the rights_json column stores, the integrity hash the
// version is published under, the credited creators, and — from the
// manifest, which IS stored and hashed — the dependency pins
// (research_asset_versions has no pin column: a version's pins live inside
// its manifest).
//
// The three lists are spelled [] rather than null when empty. The schema
// types them as arrays and null is not an array, so null would refuse the
// document for a difference that means nothing; an empty list is refused
// where a rule actually demands entries (origin_refs: minItems 1), which
// is the check's business and not a rendering accident.
func renderAssetVersionDocument(c assets.PublishCandidate, gate assets.GateResult, rightsJSON []byte) (json.RawMessage, error) {
	doc := assetVersionDocument{
		AssetID:        string(c.AssetPID),
		Version:        c.Version,
		AssetType:      c.AssetType,
		OriginRefs:     c.OriginRefs,
		Rights:         json.RawMessage(rightsJSON),
		IntegrityHash:  c.IntegrityHash,
		CreatorIDs:     c.CreatorIDs,
		DependencyRefs: gate.Manifest.DependencyPins,
	}
	if doc.OriginRefs == nil {
		doc.OriginRefs = []string{}
	}
	if doc.CreatorIDs == nil {
		doc.CreatorIDs = []string{}
	}
	if doc.DependencyRefs == nil {
		doc.DependencyRefs = []assets.DependencyPin{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: render the version document: %v", assetpublish.ErrStore, err)
	}
	return raw, nil
}

// publishJudgesChecks are the asset gate's checks whose facts the publish
// path itself produces, and therefore the only failures it can act on.
//
// The gate spec (internal/rsg/validation GateAsset) has three further
// groups, and none of them can be answered by a publication:
//
//   - the per-member object checks (schema_*, identity_fields,
//     authoritative_fields, payload_integrity) and the chain checks
//     (state_linkage, commit_linkage, actor_consistency,
//     chain_integrity, branch_empty) read a BRANCH's state chain and the
//     versions its states created. A publish has no branch: it writes one
//     asset version, and the object-level correctness of the research
//     state it descends from was judged where that state was created and
//     accepted (the commit, PR and main gates).
//   - release_review / release_rights / release_from_main read the review
//     and policy pins of the STATE the published content descends from
//     (releases.Service.ReleaseFacts, filled by the release domain). They
//     belong to the release gate that runs over main's snapshot. A publish
//     asserting them would invent a product rule — "an asset may only be
//     published from a state whose review record is approved" — that
//     neither docs/11 §3 nor the permission matrix states, and that a
//     caller's own source pin cannot establish. What the publish checklist
//     does demand of provenance is that the version pin an accepted state
//     or a release and that the pin resolves over real current state, and
//     both are checked: asset_source_pinned below, and
//     assets.Preview's ref resolution before the gate.
//
// What is left is the checklist the publish path owns (docs/11 §3), the
// same nine facts assets.Gate refused one by one, plus asset_schema — the
// one check that needs the rendered document and therefore can only run
// here. Running the ladder over the whole asset spec and refusing on any
// of its results would refuse every publish for facts about another
// object's lifecycle; filtering to these is not a relaxation, it is the
// boundary of what this command is entitled to decide.
var publishJudgesChecks = map[rsgvalidation.CheckID]bool{
	rsgvalidation.CheckAssetSourcePinned:   true,
	rsgvalidation.CheckAssetVersionPinned:  true,
	rsgvalidation.CheckAssetContributors:   true,
	rsgvalidation.CheckAssetRights:         true,
	rsgvalidation.CheckAssetVisibility:     true,
	rsgvalidation.CheckAssetDependencyPins: true,
	rsgvalidation.CheckAssetIntegrityHash:  true,
	rsgvalidation.CheckAssetMetadata:       true,
	rsgvalidation.CheckAssetSchema:         true,
}

// requirePublishedAssetDocument runs the validation ladder's asset gate
// over the entity document of the row about to be written, and returns the
// reasons the publish path refuses it (nil when it refuses nothing).
//
// The facts come from assets.Gate — the same candidate, the same rules —
// with the two CALLER-OWNED fields filled here: Document, the rendered row,
// and Ref, the canonical research-asset-version schema. Those two are the
// reason this second run exists at all; leaving them empty would fail
// asset_schema closed (checkAssetSchema refuses an absent document), which
// is the right verdict for the wrong reason.
func requirePublishedAssetDocument(val *rsgvalidation.Validator, projectID string, cand assets.PublishCandidate, gate assets.GateResult, rightsJSON []byte) ([]string, error) {
	doc, err := renderAssetVersionDocument(cand, gate, rightsJSON)
	if err != nil {
		return nil, err
	}
	facts := gate.Facts
	facts.Document = doc
	facts.Ref = publishedAssetDocumentSchema()
	if val == nil {
		// A store wired without the engine cannot judge the document, and
		// "cannot check" is not "passed": the publish is refused rather
		// than written unchecked.
		return nil, fmt.Errorf("%w: the asset document gate was not wired", assetpublish.ErrStore)
	}
	report := val.Validate(rsgvalidation.GateAsset, rsgvalidation.Snapshot{
		ProjectID: projectID,
		Asset:     &facts,
	})
	var reasons []string
	for _, failure := range report.BlockingFailures() {
		if !publishJudgesChecks[failure.Check] {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", failure.Check, failure.Detail))
	}
	return reasons, nil
}

// assetVersionPublishedPayload is the payload of the
// research_asset.version_published research event
// (specs/events/event-types.yaml): the envelope fields the
// research_events row carries are the columns; payload_version, which has
// no column, travels inside the payload.
type assetVersionPublishedPayload struct {
	PayloadVersion int      `json:"payload_version"`
	AssetID        string   `json:"asset_id"`
	AssetVersionID string   `json:"asset_version_id"`
	Version        string   `json:"version"`
	Visibility     string   `json:"visibility"`
	IntegrityHash  string   `json:"integrity_hash"`
	OriginRefs     []string `json:"origin_refs"`
}

// recordAssetVersionPublished writes the research_asset.version_published
// research event and its outbox row inside the publish transaction
// (docs/53: the event commits with the state change). The event's
// visibility is the project's at publish time — the event is as visible as
// the project that carries it. correlationID is the publish's trace id, so
// the audit row, the event and the outbox row share it.
func recordAssetVersionPublished(ctx context.Context, q *sqlc.Queries, visibility string, actorID, projectID pgtype.UUID, correlationID string, v assetpublish.PublishedVersion) error {
	payload, err := json.Marshal(assetVersionPublishedPayload{
		PayloadVersion: 1,
		AssetID:        v.AssetPID,
		AssetVersionID: v.ID,
		Version:        v.Version,
		Visibility:     string(v.Visibility),
		IntegrityHash:  v.IntegrityHash,
		OriginRefs:     v.OriginRefs,
	})
	if err != nil {
		return fmt.Errorf("persistence: render asset version event payload: %w", err)
	}
	if _, err := q.RecordResearchEvent(ctx, sqlc.RecordResearchEventParams{
		EventType:     "research_asset.version_published",
		ActorID:       actorID,
		ProjectID:     projectID,
		Visibility:    visibility,
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: record asset version event: %w", err)
	}
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		EventType:     "research_asset.version_published",
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: enqueue asset version outbox event: %w", err)
	}
	return nil
}

// publishedVersionFromRow converts one GetPublishedAssetVersion row.
func publishedVersionFromRow(row sqlc.GetPublishedAssetVersionRow) assetpublish.PublishedVersion {
	return assetpublish.PublishedVersion{
		ID:            pgUUIDToText(row.ID),
		AssetID:       pgUUIDToText(row.AssetID),
		AssetPID:      row.AssetPid,
		Version:       row.Version,
		Visibility:    assets.Visibility(row.Visibility),
		IntegrityHash: row.IntegrityHash,
		OriginRefs:    row.OriginRefs,
		PublishedBy:   pgUUIDToText(row.PublishedBy),
		PublishedAt:   row.PublishedAt.Time,
		Manifest:      json.RawMessage(row.Manifest),
		RightsJSON:    json.RawMessage(row.RightsJson),
	}
}

// mapAssetPublishError keeps the store's own sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapAssetPublishError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, assetpublish.ErrProjectNotFound),
		errors.Is(err, assetpublish.ErrAssetNotFound),
		errors.Is(err, assetpublish.ErrAssetExists),
		errors.Is(err, assetpublish.ErrVersionImmutable),
		errors.Is(err, assetpublish.ErrIdempotencyConflict),
		errors.Is(err, assetpublish.ErrRefused),
		errors.Is(err, assetpublish.ErrStore):
		return err
	}
	return fmt.Errorf("persistence: asset publish write: %w", err)
}
