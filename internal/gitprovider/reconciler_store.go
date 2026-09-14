package gitprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The canonical-store side of Git ↔ RSG reconciliation (T0309): the only
// writer of the git_reconciliation_* tables (migration 00047) and of the
// audit rows new findings append. Plain pgx, same discipline as the
// provisioning and branch-ref stores: these columns belong to this
// package, sqlc queries would put them on the shared persistence surface.
//
// The store never touches the checked surfaces: no update to
// git_branch_refs, project_states, branches or git_repository_provisions —
// a repair proposal is recorded, never applied.

// PGBranchRefStore-style adapter for the reconciliation tables. It
// implements ReconcilerStore.
type PGReconcilerStore struct {
	pool *pgxpool.Pool
}

// NewReconcilerStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewReconcilerStore(pool *pgxpool.Pool) *PGReconcilerStore {
	return &PGReconcilerStore{pool: pool}
}

// BeginRun implements ReconcilerStore: the run row is opened up front, so
// a pass that crashes or fails stays visible as an unfinished run — a
// missing check is evidence, never silence.
func (s *PGReconcilerStore) BeginRun(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO git_reconciliation_runs DEFAULT VALUES RETURNING id::text`).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// FinishRun implements ReconcilerStore: the run row receives its outcome.
// The open-finding count is read inside the same transaction as the run
// update, so the run's findings_open is the count as of pass end — and it
// is RETURNED to the caller (the summary is by value, so writing a local
// copy would never reach it).
func (s *PGReconcilerStore) FinishRun(ctx context.Context, sum RunSummary) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var providerErr *string
	if sum.ProviderError != "" {
		providerErr = &sum.ProviderError
	}
	if _, err := tx.Exec(ctx,
		`UPDATE git_reconciliation_runs
		    SET finished_at = now(),
		        refs_checked = $2, states_checked = $3,
		        mapping_violations = $4, repositories_checked = $5,
		        findings_opened = $6, findings_resolved = $7, provider_error = $8
		  WHERE id = $1`,
		sum.RunID, sum.RefsChecked, sum.StatesChecked,
		sum.MappingViolations, sum.RepositoriesChecked,
		sum.FindingsOpened, sum.FindingsResolved, providerErr); err != nil {
		return 0, err
	}
	var open int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM git_reconciliation_findings WHERE status = 'open'`).Scan(&open); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE git_reconciliation_runs SET findings_open = $2 WHERE id = $1`,
		sum.RunID, open); err != nil {
		return 0, err
	}
	return open, tx.Commit(ctx)
}

// mustExistSQL lists the synced mapping rows whose provider ref must exist
// at the recorded head, joined to the provision coordinates. A branch
// whose project lost its provision row does not appear here — its absence
// is itself a finding (mapping_violation provision_mapping_missing).
const mustExistSQL = `
	SELECT r.branch_id::text, b.project_id::text, r.git_ref, b.name, p.owner, p.name, r.head_sha
	  FROM git_branch_refs r
	  JOIN branches b ON b.id = r.branch_id
	  JOIN git_repository_provisions p ON p.project_id = b.project_id
	 WHERE r.sync_state = 'synced'
	 ORDER BY r.git_ref`

// MustExistRefs implements ReconcilerStore.
func (s *PGReconcilerStore) MustExistRefs(ctx context.Context) ([]CheckedRef, error) {
	rows, err := s.pool.Query(ctx, mustExistSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckedRef
	for rows.Next() {
		var c CheckedRef
		if err := rows.Scan(&c.BranchID, &c.ProjectID, &c.GitRef, &c.Name, &c.Owner, &c.Repo, &c.HeadSHA); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// mustBeGoneSQL lists the closed mapping rows whose provider ref must not
// exist. head_sha is the final head recorded before deletion (00031) —
// informational for the finding.
const mustBeGoneSQL = `
	SELECT r.branch_id::text, b.project_id::text, r.git_ref, b.name, p.owner, p.name,
	       COALESCE(r.head_sha, '')
	  FROM git_branch_refs r
	  JOIN branches b ON b.id = r.branch_id
	  JOIN git_repository_provisions p ON p.project_id = b.project_id
	 WHERE r.sync_state = 'closed'
	 ORDER BY r.git_ref`

// MustBeGoneRefs implements ReconcilerStore.
func (s *PGReconcilerStore) MustBeGoneRefs(ctx context.Context) ([]CheckedRef, error) {
	rows, err := s.pool.Query(ctx, mustBeGoneSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckedRef
	for rows.Next() {
		var c CheckedRef
		if err := rows.Scan(&c.BranchID, &c.ProjectID, &c.GitRef, &c.Name, &c.Owner, &c.Repo, &c.HeadSHA); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GitStates implements ReconcilerStore: every git-pinned project state
// (the rows push ingestion creates with GitStateHash — T0305). Ordered for
// determinism; V1 scale makes a full scan per pass acceptable.
func (s *PGReconcilerStore) GitStates(ctx context.Context) ([]GitStateCheck, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, project_id::text, branch_id::text, git_commit_sha, state_hash
		   FROM project_states
		  WHERE git_commit_sha IS NOT NULL
		  ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitStateCheck
	for rows.Next() {
		var g GitStateCheck
		if err := rows.Scan(&g.StateID, &g.ProjectID, &g.BranchID, &g.CommitSHA, &g.StoredHash); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MappingViolations implements ReconcilerStore: the three mapping
// invariants re-verified from the outside. They can only be broken by a
// deleted mapping row, a bypassed or disabled guard trigger, or a deleted
// provision row — exactly the drift the check exists to catch.
func (s *PGReconcilerStore) MappingViolations(ctx context.Context) ([]MappingViolation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT 'branch_unmapped'::text, b.project_id::text, b.id::text, b.git_ref
		  FROM branches b
		 WHERE NOT EXISTS (SELECT 1 FROM git_branch_refs r WHERE r.branch_id = b.id)
		UNION ALL
		SELECT 'derived_ref_mismatch', b.project_id::text, r.branch_id::text, r.git_ref
		  FROM git_branch_refs r
		  JOIN branches b ON b.id = r.branch_id
		 WHERE r.git_ref <> 'refs/heads/' || b.name
		UNION ALL
		SELECT 'provision_mapping_missing', p.id::text, '', NULL
		  FROM projects p
		 WHERE p.provision_status = 'provisioned'
		   AND NOT EXISTS (SELECT 1 FROM git_repository_provisions g WHERE g.project_id = p.id)
		 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MappingViolation
	for rows.Next() {
		var v MappingViolation
		var gitRef *string
		if err := rows.Scan(&v.Violation, &v.ProjectID, &v.BranchID, &gitRef); err != nil {
			return nil, err
		}
		if gitRef != nil {
			v.GitRef = *gitRef
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ProvisionedRepos implements ReconcilerStore.
func (s *PGReconcilerStore) ProvisionedRepos(ctx context.Context) ([]ProvisionedRepo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT owner, name, project_id::text FROM git_repository_provisions ORDER BY owner, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProvisionedRepo
	for rows.Next() {
		var p ProvisionedRepo
		if err := rows.Scan(&p.Owner, &p.Name, &p.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BranchNames implements ReconcilerStore: the branch names of one project
// (the ref names the provider may legitimately carry for it).
func (s *PGReconcilerStore) BranchNames(ctx context.Context, projectID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name FROM branches WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// RecordFinding implements ReconcilerStore: one finding row (deduped
// against the open finding of the same key by 00047's partial unique
// index — a repeated observation of an open drift re-uses the row, so a
// pass that observes the same drift again does not spam alerts) plus, for
// a NEW finding, the audit row — both in one transaction. The audit row is
// the "high-severity alert/audit" half of the acceptance criterion:
// via='system' (no actor — the reconciler is the platform), the stable
// action name, the project scope, and severity 'high' in the metadata.
func (s *PGReconcilerStore) RecordFinding(ctx context.Context, runID string, f NewFinding) (bool, error) {
	detail, err := json.Marshal(f.Detail)
	if err != nil {
		return false, fmt.Errorf("reconciler store: marshal detail: %w", err)
	}
	repair, err := json.Marshal(f.Repair)
	if err != nil {
		return false, fmt.Errorf("reconciler store: marshal repair proposal: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var findingID string
	err = tx.QueryRow(ctx,
		`INSERT INTO git_reconciliation_findings
		   (run_id, project_id, kind, severity, subject_ref, detail, repair_proposal)
		 VALUES ($1, $2, $3, 'high', $4, $5, $6)
		 ON CONFLICT (kind, project_id, subject_ref) WHERE status = 'open' DO NOTHING
		 RETURNING id::text`,
		runID, f.ProjectID, string(f.Kind), f.SubjectRef, detail, repair).Scan(&findingID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The open finding of this key exists: the drift was already
		// alerted. No new row, no new audit row.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var targetRef *string
	if f.TargetRef != "" {
		targetRef = &f.TargetRef
	}
	metadata, err := json.Marshal(map[string]any{
		"severity":   FindingSeverityHigh,
		"kind":       string(f.Kind),
		"run_id":     runID,
		"finding_id": findingID,
	})
	if err != nil {
		return false, fmt.Errorf("reconciler store: marshal audit metadata: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (actor_id, via, action, target_ref, project_id, correlation_id, metadata)
		 VALUES (NULL, 'system', $1, $2, $3, $4, $5)`,
		driftAuditAction, targetRef, f.ProjectID, runID, metadata); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveFindings implements ReconcilerStore: open findings of the given
// kinds whose key the pass did NOT observe again are resolved — the drift
// is gone (healed by the intended paths — the webhook arrived, the syncer
// ran — or repaired by an operator; the reconciler does not ask which).
// Kinds not listed are untouched: a pass whose provider checks failed
// never resolves provider findings it could not re-verify.
func (s *PGReconcilerStore) ResolveFindings(ctx context.Context, drifted []FindingKey, kinds []FindingKind) (int, error) {
	dk := make([]string, 0, len(drifted))
	dp := make([]string, 0, len(drifted))
	ds := make([]string, 0, len(drifted))
	for _, k := range drifted {
		dk = append(dk, string(k.Kind))
		dp = append(dp, k.ProjectID)
		ds = append(ds, k.SubjectRef)
	}
	kk := make([]string, 0, len(kinds))
	for _, k := range kinds {
		kk = append(kk, string(k))
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE git_reconciliation_findings f
		    SET status = 'resolved'
		  WHERE f.status = 'open'
		    AND f.kind = ANY($1)
		    AND NOT EXISTS (
		      SELECT 1 FROM unnest($2::text[], $3::text[], $4::text[]) AS v(kind, project, subject)
		       WHERE v.kind = f.kind AND v.project = f.project_id::text AND v.subject = f.subject_ref)`,
		kk, dk, dp, ds)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
