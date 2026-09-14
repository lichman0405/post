package gitprovider

import (
	"context"
	"errors"
	"fmt"
)

// Git ↔ RSG reconciliation (T0309): the periodic drift check between the
// two worlds — the canonical store (PostgreSQL: branches, git_branch_refs,
// project_states, provisions) and the GitProvider (Gitea refs and
// repositories). docs/16 §5: every accepted state commit saves git
// commit/ref + rsg state hash, a background reconciliation job checks
// drift, and ANY drift is a high-severity operational alert.
//
// One pass verifies three dimensions:
//
//	branch ref  — every synced mapping row's provider ref must exist at the
//	             recorded head, every closed row's ref must be gone, and no
//	             provider ref may exist that no branch row names.
//	state hash  — every project_states row with a git_commit_sha must carry
//	             state_hash = GitStateHash(git_commit_sha): the state's
//	             identity is a pure function of its pinned commit (T0305),
//	             so a row that disagrees is canonical-store corruption or a
//	             write-path bug — both high severity.
//	mapping     — the trigger-maintained invariants, re-verified from the
//	             outside: 1:1 branch ↔ git_branch_refs, the derived git_ref
//	             (00031), the provision row of every provisioned project,
//	             and the provider repository behind every provision row.
//
// The reconciler NEVER repairs. Every detected drift becomes a finding row
// with a concrete repair proposal (what a repair would do — re-enqueue the
// branch-ref sync, record the unrecorded push, restore the derived state
// hash, re-provision), and the proposal is stored, not applied: the
// acceptance criterion "repair proposal 不静默修" holds by construction —
// this service writes only the run/finding rows and the audit row a new
// finding appends, nothing on the checked surfaces.
//
// Failure shape: a canonical-store failure aborts the pass (the run row
// stays unfinished — a missing check is visible, never silent); a provider
// failure aborts only the provider-side checks and is recorded as
// provider_error on the run — a down provider must not be mistaken for
// "every ref missing", so provider findings are neither created (false
// findings) nor resolved (they were not re-verified) on a pass that could
// not see the provider. A nil port runs the canonical-store checks only
// (the API wiring passes nil when the provider is not configured).

// FindingKind names the drift class of one finding (the
// git_reconciliation_findings.kind check constraint).
type FindingKind string

// The seven drift classes the pass distinguishes. Provider-side kinds
// (ref_missing, ref_head_moved, dangling_ref, unmapped_ref,
// repository_missing) are only created or resolved by passes whose
// provider checks completed.
const (
	// FindingRefMissing: a synced mapping row whose provider ref does not
	// exist — the ref was deleted out-of-band or the sync never landed.
	FindingRefMissing FindingKind = "ref_missing"
	// FindingRefHeadMoved: the provider ref's head is not the recorded
	// head — a push the platform never recorded (a lost webhook, a push
	// during an outage) or an out-of-band history rewrite.
	FindingRefHeadMoved FindingKind = "ref_head_moved"
	// FindingDanglingRef: a closed mapping row whose ref still exists —
	// the close deletion never happened or the ref was recreated.
	FindingDanglingRef FindingKind = "dangling_ref"
	// FindingUnmappedRef: a provider ref no branch row of the project
	// names — a ref created outside the semantic model (the backdoor the
	// git-compat layer must not become, docs/16 §2).
	FindingUnmappedRef FindingKind = "unmapped_ref"
	// FindingRepositoryMissing: a provision row whose provider repository
	// no longer exists.
	FindingRepositoryMissing FindingKind = "repository_missing"
	// FindingStateHashMismatch: a git-pinned project state whose stored
	// state_hash disagrees with GitStateHash(git_commit_sha).
	FindingStateHashMismatch FindingKind = "state_hash_mismatch"
	// FindingMappingViolation: a trigger-maintained mapping invariant
	// broken (branch ↔ git_branch_refs 1:1, the derived git_ref, or the
	// provision row of a provisioned project).
	FindingMappingViolation FindingKind = "mapping_violation"
)

// FindingSeverityHigh is the one severity the reconciler ever writes:
// docs/16 §5 makes ANY drift high severity, and migration 00047 pins the
// rule at the storage layer (severity CHECK = 'high').
const FindingSeverityHigh = "high"

// driftAuditAction is the audit_log.action of the row every NEW finding
// appends (the domain's action naming: dot-separated, lowercase — the
// Activity feed renders it without a new read surface).
const driftAuditAction = "git.reconciliation.drift_detected"

// providerKinds are the finding kinds that only passes with a working
// provider may create or resolve.
var providerKinds = []FindingKind{
	FindingRefMissing, FindingRefHeadMoved, FindingDanglingRef,
	FindingUnmappedRef, FindingRepositoryMissing,
}

// canonicalKinds are the finding kinds a provider-less pass still creates
// and resolves.
var canonicalKinds = []FindingKind{FindingStateHashMismatch, FindingMappingViolation}

// ReconcilerPort is the provider slice the reconciler needs — reads only,
// the reconciler never mutates provider state.
type ReconcilerPort interface {
	// GetBranch reads one branch ref. ErrNotFound when it does not exist.
	GetBranch(ctx context.Context, repo Repository, name string) (BranchRef, error)
	// ListBranches lists every branch ref of the repository (the refs
	// API — the /branches API cannot answer for refs that arrived by
	// push).
	ListBranches(ctx context.Context, repo Repository) ([]BranchRef, error)
	// GetRepository reads one repository. ErrNotFound when it does not
	// exist.
	GetRepository(ctx context.Context, owner, name string) (Repository, error)
}

// CheckedRef is one mapping row the pass compares against the provider:
// what the ref must be (or must not be), and where.
type CheckedRef struct {
	BranchID string
	// ProjectID is the branch's project (every finding is project-scoped).
	ProjectID string
	// GitRef is the derived ref (refs/heads/<name> — 00031's guard).
	GitRef string
	// Name is the branch name (the ref without the prefix).
	Name string
	// Owner and Repo name the provisioned provider repository.
	Owner string
	Repo  string
	// HeadSHA is the recorded tip (git_branch_refs.head_sha): the value
	// the provider ref must be at, or the final head for a closed row.
	HeadSHA string
}

// GitStateCheck is one git-pinned project state the pass re-derives the
// hash of.
type GitStateCheck struct {
	StateID    string
	ProjectID  string
	BranchID   *string
	CommitSHA  string
	StoredHash string
}

// MappingViolationName names the broken invariant inside a
// FindingMappingViolation (stored in detail.violation).
type MappingViolationName string

// The three mapping invariants the pass re-verifies.
const (
	// MappingBranchUnmapped: a branches row with no git_branch_refs row
	// (the 00031 trigger maintains the 1:1 — a broken row means a
	// deleted mapping or a bypassed trigger).
	MappingBranchUnmapped MappingViolationName = "branch_unmapped"
	// MappingDerivedRefMismatch: a mapping row whose git_ref is not the
	// derived refs/heads/<name>.
	MappingDerivedRefMismatch MappingViolationName = "derived_ref_mismatch"
	// MappingProvisionMissing: a project claiming provisioned whose
	// git_repository_provisions row is gone.
	MappingProvisionMissing MappingViolationName = "provision_mapping_missing"
)

// MappingViolation is one broken mapping invariant.
type MappingViolation struct {
	Violation MappingViolationName
	ProjectID string
	BranchID  string
	GitRef    string
}

// ProvisionedRepo is one git_repository_provisions row: the provider
// repository the canonical store expects to exist.
type ProvisionedRepo struct {
	Owner     string
	Name      string
	ProjectID string
}

// NewFinding is one drift instance the pass hands to the store: the
// finding row plus the audit row a NEW finding appends in the same
// transaction. Detail and Repair are the observed facts and the proposed
// (never applied) repair, marshaled to jsonb by the store.
type NewFinding struct {
	ProjectID string
	Kind      FindingKind
	// SubjectRef is the stable subject of the finding (the dedupe key's
	// third part): the git ref for ref findings, the state id for
	// state-hash findings, the branch id for branch-mapping violations,
	// "<owner>/<name>" for repository findings.
	SubjectRef string
	// TargetRef is the audit row's target_ref: the git ref when the
	// finding is about one, empty otherwise.
	TargetRef string
	Detail    map[string]any
	Repair    map[string]any
}

// FindingKey names one open-finding dedupe/resolution slot:
// (kind, project, subject).
type FindingKey struct {
	Kind       FindingKind
	ProjectID  string
	SubjectRef string
}

// RunSummary is the outcome of one completed pass. The scope counts are
// one column one meaning: MappingViolations counts the broken mapping
// invariants the canonical-side check FOUND, RepositoriesChecked counts
// the provisioned repositories whose existence the provider-side check
// VERIFIED — problems found vs points checked, never conflated.
type RunSummary struct {
	RunID               string
	RefsChecked         int
	StatesChecked       int
	MappingViolations   int
	RepositoriesChecked int
	FindingsOpened      int
	FindingsOpen        int
	FindingsResolved    int
	ProviderError       string
}

// ReconcilerStore is the canonical-store port the reconciler needs. The
// concrete adapter is *PGReconcilerStore in this package — the only writer
// of the git_reconciliation_* tables and the audit rows the findings
// append (same ownership discipline as PGBranchRefStore).
type ReconcilerStore interface {
	// BeginRun opens the pass's run row (started_at set, counts zero).
	BeginRun(ctx context.Context) (runID string, err error)
	// FinishRun closes the pass's run row with the summary: the counts,
	// the provider error (when the provider-side checks aborted) and the
	// current number of open findings. It RETURNS that open-finding count
	// (read in the same transaction as the run update): the summary is
	// passed by value, so the store cannot hand the caller the count by
	// writing its local copy — a zero here while findings are open would
	// make the sweep log a misleading 'findings_open: 0'. A pass that
	// never reaches this stays unfinished — visible evidence of a failed
	// check.
	FinishRun(ctx context.Context, sum RunSummary) (open int, err error)
	// MustExistRefs lists the synced mapping rows whose provider ref must
	// exist at HeadSHA.
	MustExistRefs(ctx context.Context) ([]CheckedRef, error)
	// MustBeGoneRefs lists the closed mapping rows whose provider ref
	// must not exist.
	MustBeGoneRefs(ctx context.Context) ([]CheckedRef, error)
	// GitStates lists the git-pinned project states to re-derive hashes
	// for.
	GitStates(ctx context.Context) ([]GitStateCheck, error)
	// MappingViolations lists the broken mapping invariants.
	MappingViolations(ctx context.Context) ([]MappingViolation, error)
	// ProvisionedRepos lists the provision rows whose provider
	// repositories must exist.
	ProvisionedRepos(ctx context.Context) ([]ProvisionedRepo, error)
	// BranchNames returns the branch names of one project (the ref names
	// the provider may legitimately carry).
	BranchNames(ctx context.Context, projectID string) (map[string]bool, error)
	// RecordFinding inserts one finding (deduped against the open
	// finding of the same key — a repeated observation re-uses the open
	// row, no alert spam) and, when it inserted, appends the audit row
	// in the same transaction. inserted false means the finding was
	// already open.
	RecordFinding(ctx context.Context, runID string, f NewFinding) (inserted bool, err error)
	// ResolveFindings resolves the open findings of the given kinds whose
	// key is NOT in drifted — the pass that observes a drift gone closes
	// the finding. Kinds not listed are untouched (a provider-less pass
	// neither creates nor resolves provider findings).
	ResolveFindings(ctx context.Context, drifted []FindingKey, kinds []FindingKind) (resolved int, err error)
}

// Reconciler runs one Git ↔ RSG verification pass.
type Reconciler struct {
	port  ReconcilerPort // nil: canonical-store checks only
	store ReconcilerStore
}

// NewReconciler wires the reconciler. port may be nil (no provider
// configured): the pass still verifies state hashes and mappings.
func NewReconciler(port ReconcilerPort, store ReconcilerStore) *Reconciler {
	return &Reconciler{port: port, store: store}
}

// Reconcile runs one pass: open the run, verify the canonical dimensions,
// verify the provider dimensions when a port is wired, resolve the
// findings the pass no longer observes, close the run. It repairs nothing.
//
// A canonical-store failure aborts the pass and is returned as err (the
// run row stays unfinished). A provider failure aborts only the
// provider-side checks and lands in ProviderError — never as err, never
// as false findings: a provider that cannot answer must not be mistaken
// for one that lost all its refs.
func (r *Reconciler) Reconcile(ctx context.Context) (RunSummary, error) {
	runID, err := r.store.BeginRun(ctx)
	if err != nil {
		return RunSummary{}, fmt.Errorf("reconciliation: begin run: %w", err)
	}
	sum := RunSummary{RunID: runID}
	var drifted []FindingKey

	// Canonical-store dimensions (run regardless of the provider).
	if err := r.checkStateHashes(ctx, &sum, &drifted); err != nil {
		return sum, err
	}
	if err := r.checkMappings(ctx, &sum, &drifted); err != nil {
		return sum, err
	}

	// Provider dimensions: one failure aborts the rest of this section
	// and is recorded, not fatal. Findings created or resolved by this
	// section require a pass that actually saw the provider.
	resolveKinds := append([]FindingKind{}, canonicalKinds...)
	if r.port != nil {
		if perr := r.checkProvider(ctx, &sum, &drifted); perr != nil {
			sum.ProviderError = perr.Error()
		} else {
			resolveKinds = append(resolveKinds, providerKinds...)
		}
	}

	resolved, err := r.store.ResolveFindings(ctx, drifted, resolveKinds)
	if err != nil {
		return sum, fmt.Errorf("reconciliation: resolve findings: %w", err)
	}
	sum.FindingsResolved = resolved

	open, err := r.store.FinishRun(ctx, sum)
	if err != nil {
		return sum, fmt.Errorf("reconciliation: finish run: %w", err)
	}
	sum.FindingsOpen = open
	return sum, nil
}

// checkStateHashes verifies the state-hash dimension: every git-pinned
// project state must carry the derived GitStateHash of its commit.
func (r *Reconciler) checkStateHashes(ctx context.Context, sum *RunSummary, drifted *[]FindingKey) error {
	states, err := r.store.GitStates(ctx)
	if err != nil {
		return fmt.Errorf("reconciliation: list git states: %w", err)
	}
	for _, s := range states {
		sum.StatesChecked++
		derived := GitStateHash(s.CommitSHA)
		if derived == s.StoredHash {
			continue
		}
		key := FindingKey{Kind: FindingStateHashMismatch, ProjectID: s.ProjectID, SubjectRef: s.StateID}
		inserted, err := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
			ProjectID:  s.ProjectID,
			Kind:       FindingStateHashMismatch,
			SubjectRef: s.StateID,
			Detail: map[string]any{
				"state_id":       s.StateID,
				"git_commit_sha": s.CommitSHA,
				"stored_hash":    s.StoredHash,
				"derived_hash":   derived,
			},
			// The hash is a derived fact of the pinned commit (T0305's
			// identity function): the repair restores the derivation. The
			// row itself is append-only-guarded (00014), so a real repair
			// needs the deliberate override path — the proposal names the
			// exact value, it does not apply it.
			Repair: map[string]any{
				"action":       "restore_derived_state_hash",
				"state_id":     s.StateID,
				"derived_hash": derived,
			},
		})
		if err != nil {
			return fmt.Errorf("reconciliation: record state-hash finding: %w", err)
		}
		*drifted = append(*drifted, key)
		if inserted {
			sum.FindingsOpened++
		}
	}
	return nil
}

// checkMappings verifies the mapping dimension: the trigger-maintained
// invariants re-checked from the outside (1:1 branch ↔ mapping row, the
// derived git_ref, the provision row of a provisioned project).
func (r *Reconciler) checkMappings(ctx context.Context, sum *RunSummary, drifted *[]FindingKey) error {
	violations, err := r.store.MappingViolations(ctx)
	if err != nil {
		return fmt.Errorf("reconciliation: list mapping violations: %w", err)
	}
	for _, v := range violations {
		sum.MappingViolations++
		subject := v.BranchID
		repair := map[string]any{}
		switch v.Violation {
		case MappingBranchUnmapped:
			subject = v.BranchID
			repair = map[string]any{
				"action":    "insert_mapping_row",
				"branch_id": v.BranchID,
				"git_ref":   v.GitRef,
			}
		case MappingDerivedRefMismatch:
			subject = v.BranchID
			repair = map[string]any{
				"action":    "restore_derived_ref",
				"branch_id": v.BranchID,
				"git_ref":   v.GitRef,
			}
		case MappingProvisionMissing:
			subject = v.ProjectID
			repair = map[string]any{
				"action":     "re_provision",
				"project_id": v.ProjectID,
			}
		}
		key := FindingKey{Kind: FindingMappingViolation, ProjectID: v.ProjectID, SubjectRef: subject}
		inserted, err := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
			ProjectID:  v.ProjectID,
			Kind:       FindingMappingViolation,
			SubjectRef: subject,
			TargetRef:  v.GitRef,
			Detail: map[string]any{
				"violation": string(v.Violation),
				"branch_id": v.BranchID,
				"git_ref":   v.GitRef,
			},
			Repair: repair,
		})
		if err != nil {
			return fmt.Errorf("reconciliation: record mapping finding: %w", err)
		}
		*drifted = append(*drifted, key)
		if inserted {
			sum.FindingsOpened++
		}
	}
	return nil
}

// checkProvider verifies the provider dimensions: every provision row's
// repository exists, every synced mapping row's ref exists at the recorded
// head, every closed row's ref is gone, and no ref exists that no branch
// row names. The first provider failure aborts the section and is
// returned — recorded on the run as provider_error by the caller, never
// turned into findings. A repository that is missing gets its finding and
// is then skipped by the per-repo checks — the repository_missing finding
// already covers that drift, and a 404 on every ref of it would be noise.
func (r *Reconciler) checkProvider(ctx context.Context, sum *RunSummary, drifted *[]FindingKey) error {
	repos, err := r.store.ProvisionedRepos(ctx)
	if err != nil {
		return fmt.Errorf("reconciliation: list provisioned repos: %w", err)
	}

	// Repository existence first: a missing repository makes every
	// per-repo ref check pointless for that repo, and one provider outage
	// aborts the whole section anyway.
	present := make(map[string]bool, len(repos))
	for _, p := range repos {
		if _, err := r.port.GetRepository(ctx, p.Owner, p.Name); err != nil {
			if errors.Is(err, ErrNotFound) {
				key := FindingKey{Kind: FindingRepositoryMissing, ProjectID: p.ProjectID, SubjectRef: p.Owner + "/" + p.Name}
				inserted, ferr := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
					ProjectID:  p.ProjectID,
					Kind:       FindingRepositoryMissing,
					SubjectRef: p.Owner + "/" + p.Name,
					Detail: map[string]any{
						"owner": p.Owner,
						"name":  p.Name,
					},
					Repair: map[string]any{
						"action":     "re_provision",
						"project_id": p.ProjectID,
					},
				})
				if ferr != nil {
					return fmt.Errorf("reconciliation: record repository finding: %w", ferr)
				}
				*drifted = append(*drifted, key)
				if inserted {
					sum.FindingsOpened++
				}
				continue
			}
			return fmt.Errorf("%s/%s: %w", p.Owner, p.Name, err)
		}
		present[p.Owner+"/"+p.Name] = true
		sum.RepositoriesChecked++
	}

	// Unmapped refs: a provider ref no branch row of the project names —
	// a ref created outside the semantic model. The proposal names the two
	// repairs without choosing: adopting the ref into the RSG or deleting
	// it is a semantic decision (L3), the reconciler only records the
	// facts and the options.
	for _, p := range repos {
		if !present[p.Owner+"/"+p.Name] {
			continue // already a repository_missing finding
		}
		refs, err := r.port.ListBranches(ctx, Repository{Owner: p.Owner, Name: p.Name})
		if err != nil {
			return fmt.Errorf("%s/%s: list refs: %w", p.Owner, p.Name, err)
		}
		known, err := r.store.BranchNames(ctx, p.ProjectID)
		if err != nil {
			return fmt.Errorf("reconciliation: list branch names: %w", err)
		}
		for _, ref := range refs {
			sum.RefsChecked++
			if known[ref.Name] {
				continue
			}
			// The protected main ref (T0302) is platform-managed, not a
			// semantic branch: every provisioned repository carries it and
			// no branch row names it, so it is never unmapped drift.
			if MainRef == "refs/heads/"+ref.Name {
				continue
			}
			gitRef := "refs/heads/" + ref.Name
			key := FindingKey{Kind: FindingUnmappedRef, ProjectID: p.ProjectID, SubjectRef: gitRef}
			inserted, ferr := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
				ProjectID:  p.ProjectID,
				Kind:       FindingUnmappedRef,
				SubjectRef: gitRef,
				TargetRef:  gitRef,
				Detail: map[string]any{
					"git_ref":       gitRef,
					"provider_head": ref.HeadSHA,
					"owner":         p.Owner,
					"name":          p.Name,
				},
				Repair: map[string]any{
					"action":  "adopt_or_delete",
					"options": []string{"create_semantic_branch_from_head", "delete_ref"},
					"note":    "adopting the ref into the RSG or deleting it is a semantic decision for the project owner — the reconciler records the drift, it does not choose",
				},
			})
			if ferr != nil {
				return fmt.Errorf("reconciliation: record unmapped-ref finding: %w", ferr)
			}
			*drifted = append(*drifted, key)
			if inserted {
				sum.FindingsOpened++
			}
		}
	}

	// Known refs: synced rows must be at the recorded head, closed rows
	// must be gone.
	mustExist, err := r.store.MustExistRefs(ctx)
	if err != nil {
		return fmt.Errorf("reconciliation: list refs that must exist: %w", err)
	}
	for _, c := range mustExist {
		if !present[c.Owner+"/"+c.Repo] {
			continue // already a repository_missing finding
		}
		sum.RefsChecked++
		ref, err := r.port.GetBranch(ctx, Repository{Owner: c.Owner, Name: c.Repo}, c.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			key := FindingKey{Kind: FindingRefMissing, ProjectID: c.ProjectID, SubjectRef: c.GitRef}
			inserted, ferr := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
				ProjectID:  c.ProjectID,
				Kind:       FindingRefMissing,
				SubjectRef: c.GitRef,
				TargetRef:  c.GitRef,
				Detail: map[string]any{
					"git_ref":       c.GitRef,
					"recorded_head": c.HeadSHA,
					"owner":         c.Owner,
					"name":          c.Repo,
				},
				Repair: map[string]any{
					"action":    "enqueue_branch_ref_sync",
					"direction": "create",
					"branch_id": c.BranchID,
				},
			})
			if ferr != nil {
				return fmt.Errorf("reconciliation: record missing-ref finding: %w", ferr)
			}
			*drifted = append(*drifted, key)
			if inserted {
				sum.FindingsOpened++
			}
		case err != nil:
			return fmt.Errorf("%s/%s %s: %w", c.Owner, c.Repo, c.GitRef, err)
		case ref.HeadSHA != c.HeadSHA:
			key := FindingKey{Kind: FindingRefHeadMoved, ProjectID: c.ProjectID, SubjectRef: c.GitRef}
			inserted, ferr := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
				ProjectID:  c.ProjectID,
				Kind:       FindingRefHeadMoved,
				SubjectRef: c.GitRef,
				TargetRef:  c.GitRef,
				Detail: map[string]any{
					"git_ref":       c.GitRef,
					"recorded_head": c.HeadSHA,
					"provider_head": ref.HeadSHA,
					"owner":         c.Owner,
					"name":          c.Repo,
				},
				Repair: map[string]any{
					"action":  "record_unrecorded_push",
					"before":  c.HeadSHA,
					"after":   ref.HeadSHA,
					"git_ref": c.GitRef,
				},
			})
			if ferr != nil {
				return fmt.Errorf("reconciliation: record moved-ref finding: %w", ferr)
			}
			*drifted = append(*drifted, key)
			if inserted {
				sum.FindingsOpened++
			}
		}
	}

	mustBeGone, err := r.store.MustBeGoneRefs(ctx)
	if err != nil {
		return fmt.Errorf("reconciliation: list refs that must be gone: %w", err)
	}
	for _, c := range mustBeGone {
		if !present[c.Owner+"/"+c.Repo] {
			continue // already a repository_missing finding
		}
		sum.RefsChecked++
		ref, err := r.port.GetBranch(ctx, Repository{Owner: c.Owner, Name: c.Repo}, c.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			// Gone: the goal state.
		case err != nil:
			return fmt.Errorf("%s/%s %s: %w", c.Owner, c.Repo, c.GitRef, err)
		default:
			key := FindingKey{Kind: FindingDanglingRef, ProjectID: c.ProjectID, SubjectRef: c.GitRef}
			inserted, ferr := r.store.RecordFinding(ctx, sum.RunID, NewFinding{
				ProjectID:  c.ProjectID,
				Kind:       FindingDanglingRef,
				SubjectRef: c.GitRef,
				TargetRef:  c.GitRef,
				Detail: map[string]any{
					"git_ref":       c.GitRef,
					"provider_head": ref.HeadSHA,
					"owner":         c.Owner,
					"name":          c.Repo,
				},
				Repair: map[string]any{
					"action":    "enqueue_branch_ref_sync",
					"direction": "close",
					"branch_id": c.BranchID,
				},
			})
			if ferr != nil {
				return fmt.Errorf("reconciliation: record dangling-ref finding: %w", ferr)
			}
			*drifted = append(*drifted, key)
			if inserted {
				sum.FindingsOpened++
			}
		}
	}
	return nil
}
