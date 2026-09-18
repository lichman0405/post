package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rights"
)

// KnowledgePublishStore is the production adapter for the knowledge
// publication command (T0805): the read that resolves what the decision is
// about, the Idempotency-Key ledger read the command checks before doing
// any work, the publish transaction itself, and the public read
// (GET /knowledge/{knowledgeId}).
//
// # What the transaction is, and why the gate runs INSIDE it
//
// docs/22 §7 requires a command to re-run its own gate server-side rather
// than trust a precheck. The publish therefore resolves the version, the
// project, the review record and the existing publication, re-runs
// knowledgepublish.Judge over them, and only then writes — but "only then"
// is not enough on its own: between a check and an insert another
// transaction can accept a research PR (adding a review the decision
// reads), or publish this very version. So the whole sequence runs in ONE
// transaction, under the project row lock every membership and governance
// write takes (GetProjectByIDForUpdate):
//
//	lock the project  →  re-check the ledger (a replay is a read)
//	                  →  resolve the current facts
//	                  →  re-run Judge over them
//	                  →  refuse (writing nothing), or
//	                     insert the publication row
//	                     insert the ledger row
//	                     append the audit row
//	                     record the research event + its outbox row
//
// # What is deliberately NOT here
//
//   - No visibility predicate on any read. The audience rule is
//     knowledgepublish.AudienceFor, in Go, and a second SQL-shaped copy of
//     it in this file is how two answers to "who may read this" start to
//     disagree. ResolvePublishedKnowledge fetches the row and lets the
//     caller apply AudienceFor.
//   - No second definition of the review record: the decision reads
//     ListReleaseReviews and groups it with the release store's own
//     helper, so "this version passed review" is one read shared with the
//     release gate.
//   - No unique-index reliance for the one-publication-per-version rule:
//     that is owner ruling L3-20260916-1 #3, an application rule, and it
//     is enforced by the re-run of Judge below (see the migration's note).
type KnowledgePublishStore struct {
	pool *pgxpool.Pool
}

// NewKnowledgePublishStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewKnowledgePublishStore(pool *pgxpool.Pool) *KnowledgePublishStore {
	return &KnowledgePublishStore{pool: pool}
}

// ResolveFacts implements knowledgepublish.StorePort: everything the
// publication decision reads, over the pool (the preview path) or over a
// transaction (the publish path, via resolveFactsTx).
//
// It answers ErrVersionNotFound for a version that does not exist and for
// one that belongs to another project: the query takes the project id as
// an input, so the two are the same query result and cannot be told apart
// (docs/45: no foreign entity existence leaks).
func (s *KnowledgePublishStore) ResolveFacts(ctx context.Context, projectID, objectVersionID string) (knowledgepublish.Facts, error) {
	return resolveKnowledgeFacts(ctx, sqlc.New(s.pool), projectID, objectVersionID)
}

// resolveKnowledgeFacts is ResolveFacts over a caller-supplied querier,
// so the publish transaction resolves over ITS OWN view rather than
// through a second connection that would see a different snapshot.
func resolveKnowledgeFacts(ctx context.Context, q *sqlc.Queries, projectID, objectVersionID string) (knowledgepublish.Facts, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return knowledgepublish.Facts{}, knowledgepublish.ErrProjectNotFound
	}
	vID, err := textUUID(objectVersionID)
	if err != nil {
		// A reference that is not a uuid names no version. It is answered
		// as "no such version" rather than as a validation failure: the
		// command already refused a malformed reference before any read,
		// so reaching here with one means the id came from elsewhere.
		return knowledgepublish.Facts{}, knowledgepublish.ErrVersionNotFound
	}
	row, err := q.ResolvePublicationFactRows(ctx, sqlc.ResolvePublicationFactRowsParams{
		MainBranchName:  domain.MainBranchName,
		ObjectVersionID: vID,
		ProjectID:       pID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledgepublish.Facts{}, knowledgepublish.ErrVersionNotFound
	}
	if err != nil {
		return knowledgepublish.Facts{}, fmt.Errorf("%w: resolve knowledge publication facts: %v", knowledgepublish.ErrStore, err)
	}
	facts := knowledgepublish.Facts{
		ObjectVersionID:    pgUUIDToText(row.ObjectVersionID),
		ObjectID:           pgUUIDToText(row.ObjectID),
		ObjectType:         row.ObjectType,
		ProjectID:          pgUUIDToText(row.ProjectID),
		Title:              row.Title,
		LifecycleState:     row.LifecycleState,
		VisibilityPolicyID: uuidTextPtr(row.VisibilityPolicyID),
		StateID:            pgUUIDToText(row.StateID),
		MainBranchID:       pgUUIDToText(row.MainBranchID),
		ProjectVisibility:  row.ProjectVisibility,
	}
	facts.Reviews, err = listPublicationReviews(ctx, q, facts.StateID, facts.MainBranchID)
	if err != nil {
		return knowledgepublish.Facts{}, err
	}
	published, err := q.GetKnowledgePublicationByObjectVersion(ctx, vID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// never published — Published stays nil, which is what Judge reads
	case err != nil:
		return knowledgepublish.Facts{}, fmt.Errorf("%w: read existing publication: %v", knowledgepublish.ErrStore, err)
	default:
		existing := publishedFromRow(published)
		facts.Published = &existing
	}
	return facts, nil
}

// listPublicationReviews reads the review record of the version's lineage
// into main over the caller's querier — the SAME query the release gate
// reads (ListReleaseReviews) and the same grouping helper, so the two
// surfaces cannot come to different conclusions about whether a lineage
// was reviewed.
//
// A missing state or a project without a main branch yields an empty
// record rather than an error, which is the release store's own rule: the
// caller's state read already reported not-found, and a state without
// merged PRs legitimately has no review record. Judge then refuses the
// publication, as it should — an unreviewed version may not become
// published.
func listPublicationReviews(ctx context.Context, q *sqlc.Queries, stateID, mainBranchID string) ([]releases.ReviewRecord, error) {
	stateUUID, err := textUUID(stateID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid state id %q: %v", knowledgepublish.ErrStore, stateID, err)
	}
	if mainBranchID == "" {
		return nil, nil
	}
	mainUUID, err := textUUID(mainBranchID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid main branch id %q: %v", knowledgepublish.ErrStore, mainBranchID, err)
	}
	rows, err := q.ListReleaseReviews(ctx, sqlc.ListReleaseReviewsParams{
		MainBranchID: mainUUID,
		StateID:      stateUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: list publication reviews: %v", knowledgepublish.ErrStore, err)
	}
	return listReleaseReviewsFromRows(rows), nil
}

// LookupCreation implements knowledgepublish.StorePort: the publication an
// Idempotency-Key already wrote, or nil when the key has no ledger entry
// yet. The command checks it before resolving anything, so a replay never
// re-runs a decision (a replay is a read).
//
// A project id that is not a uuid text cannot have published anything: the
// key's scope is a project row, and answering "no entry" for one that
// cannot exist is the truth rather than an error. The authorization has
// already run by the time this is called, so this cannot be used to probe.
func (s *KnowledgePublishStore) LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*knowledgepublish.Published, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	q := sqlc.New(s.pool)
	publicationID, err := q.GetKnowledgePublicationCreation(ctx, sqlc.GetKnowledgePublicationCreationParams{
		ProjectID:      pID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: read knowledge publication creation: %v", knowledgepublish.ErrStore, err)
	}
	row, err := q.GetKnowledgePublication(ctx, publicationID)
	if err != nil {
		return nil, fmt.Errorf("%w: read published knowledge object: %v", knowledgepublish.ErrStore, err)
	}
	out := publishedFromRow(row)
	return &out, nil
}

// Publish implements knowledgepublish.StorePort. See the type doc for the
// transaction it runs; this method is the transaction.
func (s *KnowledgePublishStore) Publish(ctx context.Context, req knowledgepublish.PublishRequest) (knowledgepublish.Published, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return knowledgepublish.Published{}, knowledgepublish.ErrProjectNotFound
	}
	publishedBy, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return knowledgepublish.Published{}, fmt.Errorf("%w: publishing actor id: %v", knowledgepublish.ErrStore, err)
	}
	// One correlation id for the whole publish: the request's when the
	// observability middleware attached one, else a fresh one — the same
	// fallback the asset and release stores use, so the audit row, the
	// research event and the outbox row always share one trace id.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return knowledgepublish.Published{}, fmt.Errorf("%w: publish correlation id: %v", knowledgepublish.ErrStore, err)
		}
		correlationID = id.String()
	}

	var out knowledgepublish.Published
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		project, err := q.GetProjectByIDForUpdate(ctx, projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return knowledgepublish.ErrProjectNotFound
			}
			return err
		}

		// The ledger, re-checked under the project lock. The command's
		// LookupCreation ran outside any transaction, so it is an
		// optimization; this read is the guarantee, because the lock
		// serializes every publish of this project.
		if req.IdempotencyKey != nil {
			publicationID, err := q.GetKnowledgePublicationCreation(ctx, sqlc.GetKnowledgePublicationCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
			})
			switch {
			case err == nil:
				row, err := q.GetKnowledgePublication(ctx, publicationID)
				if err != nil {
					return err
				}
				out = publishedFromRow(row)
				return nil
			case errors.Is(err, pgx.ErrNoRows):
				// first publish with this key — fall through to the work
			default:
				return err
			}
		}

		// The facts over THIS transaction's view, and the decision over
		// them. This is the re-run: the preview the caller saw decided
		// over the state at that moment, and this decides over the state
		// the write actually commits against.
		facts, err := resolveKnowledgeFacts(ctx, q, req.ProjectID, req.ObjectVersionID)
		if err != nil {
			return err
		}
		// The rights document is the REQUEST's, not a re-read of anything:
		// the publication carries the declaration the publisher published
		// under, and the row's bytes are the canonical form of the
		// document the command parsed. A row read back would be a
		// different publication.
		facts.Rights = req.Facts.Rights
		if reasons := knowledgepublish.Judge(facts); len(reasons) > 0 {
			return &knowledgepublish.PublicationRefused{
				Preview: knowledgepublish.PreviewOf(facts),
				Reasons: refusalReasonLines(reasons),
			}
		}
		rightsJSON := rightsBytesForPublication(req.RightsJSON, facts.Rights)
		publication, err := q.PublishKnowledgePublication(ctx, sqlc.PublishKnowledgePublicationParams{
			ObjectVersionID: mustUUID(req.ObjectVersionID),
			PublicVersion:   req.PublicVersion,
			RightsJson:      rightsJSON,
			PublishedBy:     publishedBy,
			Pid:             req.PID,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				(constraintNames(pgErr, "knowledge_publications_object_version_id_public_version_key") ||
					constraintNames(pgErr, "knowledge_publications_pid_uniq")) {
				// UNIQUE(object_version_id, public_version) means this
				// exact name is taken for this version — which, since the
				// version is published at most once, means the decision
				// above lost a race (the project lock serializes publishes
				// of one project, so this is a row written by another
				// path). The pid index colliding means the generator
				// repeated itself, which is a defect rather than a
				// caller's error — both are answered as "already
				// published", which is the outcome the caller can act on.
				//
				// NOTE: the constraint name is matched on its SUFFIX, not
				// on the string above, because PostgreSQL truncates
				// identifiers to 63 characters and the generated name is
				// exactly at the boundary in some versions.
				return knowledgepublish.ErrAlreadyPublished
			}
			return err
		}
		out = publishedFromRow(publication)

		if req.IdempotencyKey != nil {
			publicationUUID, err := textUUID(out.ID)
			if err != nil {
				return err
			}
			if _, err := q.CreateKnowledgePublicationCreation(ctx, sqlc.CreateKnowledgePublicationCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
				PublicationID:  publicationUUID,
			}); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
					pgErr.ConstraintName == "knowledge_publication_creations_project_id_idempotency_key" {
					// Unreachable under the project lock (two publishes
					// with one key serialize, so the second reads the
					// first's ledger row above), and mapped rather than
					// wrapped because if it ever DID fire, what happened
					// is exactly what IDEMPOTENCY_CONFLICT names: the key
					// is already taken.
					return knowledgepublish.ErrIdempotencyConflict
				}
				return err
			}
		}

		// The audit row names the assigned publication id — the store
		// writes it after the insert, so the command left the target ref
		// empty.
		audit := req.Audit
		audit.TargetRef = "knowledge_publication:" + out.ID
		audit.CorrelationID = correlationID
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return recordKnowledgeVersionPublished(ctx, q, project.Visibility, publishedBy, projectID, correlationID, out)
	})
	if err != nil {
		return knowledgepublish.Published{}, err
	}
	return out, nil
}

// GetPublishedKnowledge implements knowledgepublish.ReadPort: the
// publication the pid names, with everything AudienceFor decides on.
//
// It applies NO audience filter, and that is deliberate: the rule is Go's
// (knowledgepublish.AudienceFor), and a SQL copy of it here is how the
// read path and the publish decision would start to disagree. The caller
// applies the rule to what this returns and answers not-found when it
// refuses.
//
// A pid that names nothing and a string that is not a pid at all are both
// "false, nil": neither can resolve, and a caller must not be able to tell
// them apart (docs/45).
func (s *KnowledgePublishStore) GetPublishedKnowledge(ctx context.Context, pid string) (knowledgepublish.PublishedKnowledge, bool, error) {
	if !knowledgepublish.ValidPID(pid) {
		return knowledgepublish.PublishedKnowledge{}, false, nil
	}
	row, err := sqlc.New(s.pool).ResolvePublishedKnowledge(ctx, pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledgepublish.PublishedKnowledge{}, false, nil
	}
	if err != nil {
		return knowledgepublish.PublishedKnowledge{}, false, fmt.Errorf("%w: resolve published knowledge: %v", knowledgepublish.ErrStore, err)
	}
	doc, parseErr := rights.Parse(row.RightsJson)
	out := knowledgepublish.PublishedKnowledge{
		PID:                row.Pid,
		PublicVersion:      row.PublicVersion,
		PublishedBy:        pgUUIDToText(row.PublishedBy),
		PublishedAt:        row.PublishedAt.Time,
		Rights:             doc,
		RightsValid:        parseErr == nil,
		ObjectVersionID:    pgUUIDToText(row.ObjectVersionID),
		ObjectID:           pgUUIDToText(row.ObjectID),
		ObjectType:         row.ObjectType,
		Title:              row.Title,
		LifecycleState:     row.LifecycleState,
		SchemaID:           row.SchemaID,
		SchemaVersion:      row.SchemaVersion,
		IntegrityHash:      row.IntegrityHash,
		VisibilityPolicyID: uuidTextPtr(row.VisibilityPolicyID),
		ProjectID:          pgUUIDToText(row.ProjectID),
		ProjectVisibility:  row.ProjectVisibility,
	}
	return out, true, nil
}

// publishedFromRow converts one stored publication. RightsJSON is carried
// as stored, byte for byte: the row's document is what the publication
// published under, and re-rendering it here would be a second, silent
// version of it.
func publishedFromRow(row sqlc.KnowledgePublication) knowledgepublish.Published {
	return knowledgepublish.Published{
		ID:              pgUUIDToText(row.ID),
		PID:             row.Pid,
		ObjectVersionID: pgUUIDToText(row.ObjectVersionID),
		PublicVersion:   row.PublicVersion,
		RightsJSON:      json.RawMessage(row.RightsJson),
		PublishedBy:     pgUUIDToText(row.PublishedBy),
		PublishedAt:     row.PublishedAt.Time,
	}
}

// rightsBytesForPublication renders the bytes the publication row stores.
//
// It prefers req.RightsJSON — the command's canonical marshalling of the
// document it parsed — and falls back to re-rendering the parsed document
// only if those bytes are absent. Both are the same document; the fallback
// exists so that a caller that filled only Facts still cannot store a NULL
// (the column is NOT NULL, and a nil there would be a row the read path
// cannot render). A document that will not marshal at all is an error, not
// a nil: storing an empty declaration would be publishing under terms
// nobody wrote.
func rightsBytesForPublication(raw []byte, doc rights.Document) []byte {
	if len(raw) > 0 {
		return raw
	}
	b, err := doc.Marshal()
	if err != nil {
		return nil
	}
	return b
}

// refusalReasonLines renders Judge's entries as the one-line reasons a
// PublicationRefused carries beside the whole preview. They are derived
// from the preview's own entries (never from anything the preview does not
// hold), and they are a convenience: the preview is the record.
func refusalReasonLines(reasons []knowledgepublish.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Code+": "+r.Detail)
	}
	return out
}

// constraintNames reports whether a unique violation is attributable to
// one of the named constraints, matched on the SUFFIX because PostgreSQL
// truncates identifiers to 63 characters (a generated constraint name that
// lands exactly on the boundary is stored truncated, and comparing the
// full name would then never match).
func constraintNames(pgErr *pgconn.PgError, names ...string) bool {
	for _, name := range names {
		if pgErr.ConstraintName == name {
			return true
		}
		if len(name) > 63 && len(pgErr.ConstraintName) == 63 && name[:63] == pgErr.ConstraintName {
			return true
		}
	}
	return false
}

// knowledgeVersionPublishedPayload is the knowledge.version_published
// research event (specs/events/event-types.yaml): the envelope fields the
// research_events row carries are the columns; payload_version, which has
// no column, travels inside the payload.
//
// The event name is the MACHINE-READABLE spelling from
// specs/events/event-types.yaml, not docs/18's older `knowledge.published`.
//
// The identities follow research_asset.version_published's convention
// (asset_id carries the asset's pid, asset_version_id the version's row
// id): publication_id carries the publication's PID — the public identity
// a reader cites, since knowledge_publications.id never leaves the
// process — and object_version_id carries the scientific object VERSION
// the publication is about.
type knowledgeVersionPublishedPayload struct {
	PayloadVersion  int    `json:"payload_version"`
	PublicationID   string `json:"publication_id"`
	ObjectVersionID string `json:"object_version_id"`
	PublicVersion   string `json:"public_version"`
	ProjectID       string `json:"project_id"`
}

// recordKnowledgeVersionPublished writes the
// knowledge.version_published research event and its outbox row inside
// the publish transaction (docs/53: the event commits with the state
// change). The event's visibility is the PROJECT's at publish time — the
// event is as visible as the project that carries it, exactly as
// research_asset.version_published is. That is the EVENT's visibility and
// not the publication's audience: the audience rule
// (knowledgepublish.AudienceFor) is about reads of the publication, and
// the two are different questions — see the RESULT's note on the
// subscription side.
//
// correlationID is the publish's trace id, so the audit row, the event
// and the outbox row share it.
func recordKnowledgeVersionPublished(ctx context.Context, q *sqlc.Queries, visibility string, actorID, projectID pgtype.UUID, correlationID string, p knowledgepublish.Published) error {
	payload, err := json.Marshal(knowledgeVersionPublishedPayload{
		PayloadVersion:  1,
		PublicationID:   p.PID,
		ObjectVersionID: p.ObjectVersionID,
		PublicVersion:   p.PublicVersion,
		ProjectID:       pgUUIDToText(projectID),
	})
	if err != nil {
		return fmt.Errorf("persistence: render knowledge version event payload: %w", err)
	}
	const eventType = "knowledge.version_published"
	if _, err := q.RecordResearchEvent(ctx, sqlc.RecordResearchEventParams{
		EventType:     eventType,
		ActorID:       actorID,
		ProjectID:     projectID,
		Visibility:    visibility,
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: record knowledge version event: %w", err)
	}
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		EventType:     eventType,
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: enqueue knowledge version outbox event: %w", err)
	}
	return nil
}

// uuidTextPtr renders a nullable uuid column as a pointer to its text
// form, and NULL as nil — the shape knowledgepublish.Facts and the read
// model use for the version's visibility axis, where nil and "" mean
// different things ("inherits the project" versus "an id").
func uuidTextPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := pgUUIDToText(u)
	return &s
}

// mustUUID converts a text uuid that the caller has already validated.
// knowledgepublish's command refuses a reference that is not a uuid before
// any read, so a value reaching here has passed validUUIDText; an
// unparseable one would be the store being called from somewhere else, and
// pgtype's zero UUID matches no row.
func mustUUID(s string) pgtype.UUID {
	u, err := textUUID(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return u
}
