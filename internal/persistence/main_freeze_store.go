package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/mainfreeze"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// MainFreezeStore is the production mainfreeze.StorePort adapter over
// PostgreSQL (sqlc generated queries, pgx).
//
// Freeze is the task's transaction boundary: the flag, the audit row and
// the domain event happen in ONE transaction, so a frozen main is never
// observable without the record of who froze it and when (docs/26, docs/53)
// and a failed freeze leaves the project exactly as it was.
type MainFreezeStore struct {
	pool *pgxpool.Pool
}

// NewMainFreezeStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewMainFreezeStore(pool *pgxpool.Pool) *MainFreezeStore {
	return &MainFreezeStore{pool: pool}
}

// Freeze implements mainfreeze.StorePort.
//
// The compare-and-swap decides everything (FreezeProjectMain):
//
//   - one row back — this call won the flag for the project: the audit row
//     and the project.main_frozen event are written on the same
//     transaction, and the result is the newly frozen project.
//   - zero rows — either the project does not exist or it was already
//     frozen. One read tells them apart (the same shape
//     StateStore.branchCASConflict uses for its own zero-row outcome). The
//     already-frozen case is the idempotent one: the same state is
//     returned, and NOTHING is written — no second audit row, no second
//     event — which is what makes a repeated request (the same
//     Idempotency-Key or a different one) a no-op by construction rather
//     than by remembering keys in a ledger.
//
// There is no read-then-write anywhere in this path: the flag is read only
// after the CAS has been lost, to explain an outcome that has already been
// decided.
func (s *MainFreezeStore) Freeze(ctx context.Context, req mainfreeze.FreezeRequest) (mainfreeze.Result, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return mainfreeze.Result{}, mainfreeze.ErrProjectNotFound
	}
	actorID, err := textUUID(req.ActorID)
	if err != nil {
		return mainfreeze.Result{}, fmt.Errorf("%w: freezing actor id: %v", mainfreeze.ErrStore, err)
	}
	// One correlation id for the whole freeze: the request's when the
	// observability middleware attached one, else a fresh one — the same
	// fallback the release and publish stores use, so the audit row, the
	// research event and the outbox row always share one trace id.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return mainfreeze.Result{}, fmt.Errorf("%w: freeze correlation id: %v", mainfreeze.ErrStore, err)
		}
		correlationID = id.String()
	}

	var out mainfreeze.Result
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.FreezeProjectMain(ctx, projectID)
		switch {
		case err == nil:
			// This call won the compare-and-swap: the flag is set and this
			// transaction owns the record of it. The audit row is stamped with
			// the SAME correlation id as the event (the caller may not know
			// one — the command is not request-scoped), so the operator reads
			// the freeze's three records as one transaction's facts rather
			// than having to infer it from timestamps.
			audit := req.Audit
			if audit.CorrelationID == "" {
				audit.CorrelationID = correlationID
			}
			if err := appendAudit(ctx, q, audit); err != nil {
				return err
			}
			if err := recordMainFrozenEvent(ctx, tx, row, actorID, correlationID); err != nil {
				return err
			}
			out = mainfreeze.Result{
				ProjectID:  pgUUIDToText(row.ID),
				MainFrozen: row.MainFrozen,
			}
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			// The CAS found no unfrozen project: either there is no such
			// project, or main is already frozen. One read decides, and in
			// the already-frozen case this transaction writes nothing.
			current, rerr := q.GetProjectByID(ctx, projectID)
			if errors.Is(rerr, pgx.ErrNoRows) {
				return mainfreeze.ErrProjectNotFound
			}
			if rerr != nil {
				return rerr
			}
			if !current.MainFrozen {
				// Unreachable under READ COMMITTED — the CAS loses only to a
				// committed freeze (a concurrent transaction's uncommitted
				// update blocks this statement rather than zeroing it), and
				// projects rows are never deleted or reset — so a false flag
				// here means the row changed under rules this store does not
				// know about. Refuse rather than report a freeze that did
				// not happen.
				return fmt.Errorf("%w: the freeze compare-and-swap found no unfrozen project and the project is not frozen", mainfreeze.ErrStore)
			}
			out = mainfreeze.Result{
				ProjectID:     pgUUIDToText(current.ID),
				MainFrozen:    current.MainFrozen,
				AlreadyFrozen: true,
			}
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return mainfreeze.Result{}, mapMainFreezeError(err)
	}
	return out, nil
}

// The domain event this store produces (specs/events/event-types.yaml:14,
// `project.main_frozen` — docs/18 §2 names the same event `main.frozen` in
// prose; the machine-readable vocabulary is the spec's, and this package
// uses the spec's spelling, the same rule T0409 followed for
// `pull_request.merged`). One event per freeze, recorded into the outbox
// inside the freeze's own transaction (docs/53): the event commits with
// the flag or not at all.
//
// It is the AUDIT action's name spelled the same way and nothing more:
// the two vocabularies are separate registries (internal/domain holds the
// audit actions, specs/events holds the event types) and neither is
// derived from the other — the note on domain.ActionProjectMainFrozen
// records the same coincidence domain.ActionPullRequestMerged does.
const eventProjectMainFrozen = "project.main_frozen"

// mainFrozenEventPayload is the payload of project.main_frozen. It carries
// identity/reference only (docs/52「Workers」: no bulk content in events):
// the project (the envelope column and the payload) and the flag's value,
// which is what a subscriber routing on this event needs. The audit row
// carries the rest (who, when, which request).
type mainFrozenEventPayload struct {
	// PayloadVersion is the envelope's payload_version; it has no column of
	// its own, so it travels inside the payload (the same shape
	// pull_request.merged and research_asset.version_published use).
	PayloadVersion int `json:"payload_version"`
	// MainFrozen is the flag's value after the action: always true for this
	// event, stated rather than implied so a consumer needs no knowledge of
	// which actions exist.
	MainFrozen bool `json:"main_frozen"`
}

// recordMainFrozenEvent writes project.main_frozen into the outbox inside
// the freeze transaction (docs/53: the event commits with the state
// change; the dispatcher publishes it into research_events afterwards).
//
// The visibility is fail-closed: the project's own visibility value, and
// anything but the canonical public value is private. The flag is never
// widened by a spelling this store does not recognize, which is the same
// rule merge.eventVisibility applies to the branch it is about — an event
// is never more visible than its subject (docs/12 §3).
func recordMainFrozenEvent(ctx context.Context, tx pgx.Tx, project sqlc.Project, actorID pgtype.UUID, correlationID string) error {
	payload, err := json.Marshal(mainFrozenEventPayload{
		PayloadVersion: 1,
		MainFrozen:     project.MainFrozen,
	})
	if err != nil {
		return fmt.Errorf("persistence: render main frozen event payload: %w", err)
	}
	if err := events.Record(ctx, tx, events.Event{
		EventType:     eventProjectMainFrozen,
		ActorID:       pgUUIDToText(actorID),
		ProjectID:     pgUUIDToText(project.ID),
		Visibility:    mainFrozenEventVisibility(project.Visibility),
		CorrelationID: correlationID,
		Payload:       payload,
	}); err != nil {
		return fmt.Errorf("persistence: record main frozen event: %w", err)
	}
	return nil
}

// mainFrozenEventVisibility maps a project's visibility to the event
// vocabulary (docs/12: an event is never more visible than its subject).
// Anything but the canonical public value falls back to private — fail
// closed: the project adapter validates its own values, and the event
// never guesses.
func mainFrozenEventVisibility(v string) string {
	if v == string(domain.VisibilityPublic) {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}

// mapMainFreezeError keeps the store's own sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapMainFreezeError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, mainfreeze.ErrProjectNotFound),
		errors.Is(err, mainfreeze.ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", mainfreeze.ErrStore, err)
}
