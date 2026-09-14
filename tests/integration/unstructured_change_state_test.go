package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Required test "unstructured negative" (T0306): the branch semantic
// completeness flag and the gates that refuse an incomplete branch. Pure
// database — the flag is a store fact, no provider needed:
//
//   - a push carrying an unstructured change marks the branch
//     unstructured_changes, and the changed-file rows are ALL recorded
//     (任意文件 push 不丢失 — the unstructured file is retained, never
//     dropped);
//   - while the flag is unstructured_changes, opening a pull request from
//     the branch and completing the branch merge are BOTH refused by the
//     database (00042's gates) — the negative the task is named for;
//   - an API state commit does NOT clear the flag (there is no linkage
//     between an API commit and the files it would resolve — clearing on
//     any commit would be the bypass 不可绕过 semantic validation merge
//     forbids);
//   - the flag returns to semantic_complete exactly when the ingestion
//     evidence at the head resolves every outstanding path: the same path
//     replaced by a known manifest, or removed.
func TestUnstructuredChangeStateNegative(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0306")

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "unstructured-neg@example.com", "hash", "unstructured-neg", "Unstructured Negative")
	if err != nil {
		t.Fatalf("unstructured negative: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "unstructured-neg",
		Name:            "Unstructured Negative",
		Purpose:         "T0306 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("unstructured negative: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'unstructured-neg-repo', 44, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("unstructured negative: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("4", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("unstructured negative: insert fork state: %v", err)
	}
	svc := branches.NewService(persistence.NewBranchStore(pool))
	branch, err := svc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "semantic",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("unstructured negative: create branch: %v", err)
	}
	mainBranch, err := svc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "main",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("unstructured negative: create main branch: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', fork_sha = $1, head_sha = $1, synced_at = now()
		WHERE branch_id = $2`, forkSHA, branch.ID); err != nil {
		t.Fatalf("unstructured negative: sync the refs row: %v", err)
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	shaC := strings.Repeat("c", 40)
	shaD := strings.Repeat("d", 40)
	store := gitprovider.NewPushIngestStore(pool)
	event := func(before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          "refs/heads/semantic",
			Before:       before,
			After:        after,
			RepositoryID: 44,
			Owner:        "post-git-svc",
			Name:         "unstructured-neg-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("unstructured negative: IngestPush(%s): %v", ev.DeliveryID, err)
		}
		return inserted
	}
	flagOf := func(branchID string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT semantic_state
			FROM git_branch_semantic_states WHERE branch_id = $1`, branchID).Scan(&state); err != nil {
			t.Fatalf("unstructured negative: probe semantic state: %v", err)
		}
		return state
	}
	changeCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_changes`).Scan(&n); err != nil {
			t.Fatalf("unstructured negative: probe change count: %v", err)
		}
		return n
	}
	stateIDOf := func(sha string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM project_states
			WHERE project_id = $1 AND git_commit_sha = $2
			ORDER BY created_at, id LIMIT 1`, project.ID, sha).Scan(&id); err != nil {
			t.Fatalf("unstructured negative: probe state of %s: %v", sha, err)
		}
		return id
	}
	// isSemanticGateError reports whether err is the database's P0001
	// refusal (00042's gates raise exactly that — never a silent zero-row).
	isSemanticGateError := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "P0001"
	}
	mergeBranch := func(branchID string) error {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE branches SET lifecycle_state = 'merged'
			WHERE id = $1`, branchID)
		return err
	}
	openPR := func(number int64, sourceBranchID, proposedStateID string) error {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO pull_requests
			(project_id, number, source_branch_id, target_branch_id,
			 base_state_id, proposed_state_id, title, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, 'merge it', $7)`,
			project.ID, number, sourceBranchID, mainBranch.ID, forkStateID, proposedStateID, user.ID)
		return err
	}

	// ---- Push A: one known manifest + one unstructured data file. The
	// ingestion accepts BOTH (任意文件 push 不丢失): the unstructured
	// file's row is recorded like any other, and the branch is marked
	// unstructured_changes.
	if !ingest(event(forkSHA, shaA, "d-a"), []gitprovider.ClassifiedChange{
		{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindManifest, SchemaID: "mat", ContentSHA256: "mat-sum"},
		{Path: "data/raw.csv", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("unstructured negative: push A not inserted")
	}
	if got := flagOf(branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Fatalf("unstructured negative: flag after A = %q, want unstructured_changes", got)
	}
	if got := changeCount(); got != 2 {
		t.Fatalf("unstructured negative: changes after A = %d, want 2 (the unstructured file is retained, not dropped)", got)
	}

	// ---- The negative: while the flag is unstructured_changes, the
	// database itself refuses both a formal PR from the branch and the
	// branch's merge transition — no code path can bypass it.
	if err := openPR(1, branch.ID, stateIDOf(shaA)); !isSemanticGateError(err) {
		t.Errorf("unstructured negative: PR insert on an incomplete branch = %v, want the semantic gate's P0001 refusal", err)
	}
	if err := mergeBranch(branch.ID); !isSemanticGateError(err) {
		t.Errorf("unstructured negative: merge of an incomplete branch = %v, want the semantic gate's P0001 refusal", err)
	}
	var lifecycle string
	if err := pool.QueryRow(ctx, `SELECT lifecycle_state FROM branches WHERE id = $1`, branch.ID).Scan(&lifecycle); err != nil || lifecycle != "active" {
		t.Errorf("unstructured negative: lifecycle after refused merge = %q (%v), want still active", lifecycle, err)
	}

	// ---- An API state commit does NOT clear the flag: the rows the
	// commit writes (a state + a commit record) are not ingestion
	// evidence, and clearing on any commit would be the bypass the
	// acceptance criterion forbids.
	var apiStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, branch_id, parent_state_id, state_hash, manifest_version)
		VALUES ($1, $2, $3, 'api-commit-state-hash', 'v1') RETURNING id::text`,
		project.ID, branch.ID, forkStateID).Scan(&apiStateID); err != nil {
		t.Fatalf("unstructured negative: insert api state: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO state_commits
		(project_id, branch_id, base_state_id, result_state_id, actor_id, via, message, operation_summary)
		VALUES ($1, $2, $3, $4, $5, 'api', 'agent fills semantics through the API', '[]')`,
		project.ID, branch.ID, forkStateID, apiStateID, user.ID); err != nil {
		t.Fatalf("unstructured negative: insert api commit: %v", err)
	}
	if got := flagOf(branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("unstructured negative: flag after an API commit = %q, want still unstructured_changes (API commits are not ingestion evidence)", got)
	}
	if err := mergeBranch(branch.ID); !isSemanticGateError(err) {
		t.Errorf("unstructured negative: merge after an API commit = %v, want the gate still refusing (no bypass)", err)
	}

	// ---- Push B: the agent replaces data/raw.csv with a valid manifest
	// at the SAME path — the evidence resolves the outstanding path, the
	// flag returns to semantic_complete, and the PR gate opens (the insert
	// goes through). The merge itself is saved for the very end: a
	// completed merge is terminal and cannot re-test the gates.
	if !ingest(event(shaA, shaB, "d-b"), []gitprovider.ClassifiedChange{
		{Path: "data/raw.csv", Kind: gitprovider.ChangeModified, File: gitprovider.FileKindManifest, SchemaID: "dataset", ContentSHA256: "csv-manifest-sum"},
	}) {
		t.Fatal("unstructured negative: push B not inserted")
	}
	if got := flagOf(branch.ID); got != string(gitprovider.BranchSemanticComplete) {
		t.Fatalf("unstructured negative: flag after B = %q, want semantic_complete (the manifest replacement resolved the path)", got)
	}
	if err := openPR(1, branch.ID, stateIDOf(shaB)); err != nil {
		t.Errorf("unstructured negative: PR insert on a complete branch = %v, want success (the gate opens with the flag)", err)
	}

	// ---- Push C: a new unstructured file — the flag follows the head's
	// CURRENT evidence, not history; the gates close again.
	if !ingest(event(shaB, shaC, "d-c"), []gitprovider.ClassifiedChange{
		{Path: "data/notes.md", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("unstructured negative: push C not inserted")
	}
	if got := flagOf(branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Fatalf("unstructured negative: flag after C = %q, want unstructured_changes", got)
	}
	if err := openPR(2, branch.ID, stateIDOf(shaC)); !isSemanticGateError(err) {
		t.Errorf("unstructured negative: PR insert after C = %v, want the gate refusing again", err)
	}
	if err := mergeBranch(branch.ID); !isSemanticGateError(err) {
		t.Errorf("unstructured negative: merge after C = %v, want the gate refusing again (lifecycle still active)", err)
	}

	// ---- Push D: the unstructured file is removed — removal resolves it
	// (the file is gone, nothing left to understand), the flag clears, and
	// the merge completes.
	if !ingest(event(shaC, shaD, "d-d"), []gitprovider.ClassifiedChange{
		{Path: "data/notes.md", Kind: gitprovider.ChangeRemoved, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("unstructured negative: push D not inserted")
	}
	if got := flagOf(branch.ID); got != string(gitprovider.BranchSemanticComplete) {
		t.Fatalf("unstructured negative: flag after D = %q, want semantic_complete (the removal resolved the path)", got)
	}
	if err := mergeBranch(branch.ID); err != nil {
		t.Errorf("unstructured negative: merge after D = %v, want success", err)
	}
}

// TestUnstructuredFlagFollowsHeadChainNotDeliveryOrder is the
// out-of-order companion of the negative test: the flag derives from the
// ingestion evidence AT the head pointer, walking the push chain over the
// (before, after) links — a delivery recorded while its advance was
// refused (its before no longer matched the head) is a fact of the
// delivery but NOT part of the head's chain, so its rows never move the
// flag. The newer push B (which resolves data/raw.csv) delivered FIRST
// must not clear the older push A's unstructured change: the head is still
// the fork point, and when A arrives the flag must be A's.
func TestUnstructuredFlagFollowsHeadChainNotDeliveryOrder(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0306")

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "unstructured-order@example.com", "hash", "unstructured-order", "Unstructured Order")
	if err != nil {
		t.Fatalf("unstructured order: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "unstructured-order",
		Name:            "Unstructured Order",
		Purpose:         "T0306 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("unstructured order: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'unstructured-order-repo', 45, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("unstructured order: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("5", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("unstructured order: insert fork state: %v", err)
	}
	branch, err := branches.NewService(persistence.NewBranchStore(pool)).Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "semantic",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("unstructured order: create branch: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', fork_sha = $1, head_sha = $1, synced_at = now()
		WHERE branch_id = $2`, forkSHA, branch.ID); err != nil {
		t.Fatalf("unstructured order: sync the refs row: %v", err)
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	store := gitprovider.NewPushIngestStore(pool)
	event := func(before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          "refs/heads/semantic",
			Before:       before,
			After:        after,
			RepositoryID: 45,
			Owner:        "post-git-svc",
			Name:         "unstructured-order-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("unstructured order: IngestPush(%s): %v", ev.DeliveryID, err)
		}
		return inserted
	}
	flagOf := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT semantic_state
			FROM git_branch_semantic_states WHERE branch_id = $1`, branch.ID).Scan(&state); err != nil {
			t.Fatalf("unstructured order: probe semantic state: %v", err)
		}
		return state
	}

	// B (the newer push, resolving data/raw.csv into a manifest) is
	// delivered FIRST: its before (shaA) does not match the head (the fork
	// point) — the advance is refused, the delivery is recorded.
	if !ingest(event(shaA, shaB, "d-b"), []gitprovider.ClassifiedChange{
		{Path: "data/raw.csv", Kind: gitprovider.ChangeModified, File: gitprovider.FileKindManifest, SchemaID: "dataset", ContentSHA256: "csv-manifest-sum"},
	}) {
		t.Fatal("unstructured order: B not inserted")
	}
	if got := flagOf(); got != string(gitprovider.BranchSemanticComplete) {
		t.Errorf("unstructured order: flag after refused B = %q, want semantic_complete (the head is still the fork point; B is not part of its chain)", got)
	}

	// A arrives: its before (the fork point) matches — the head advances,
	// and the flag must now be A's unstructured change. B's resolution row
	// is off-chain and must NOT clear it.
	if !ingest(event(forkSHA, shaA, "d-a"), []gitprovider.ClassifiedChange{
		{Path: "data/raw.csv", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("unstructured order: A not inserted")
	}
	if got := flagOf(); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("unstructured order: flag after A = %q, want unstructured_changes (B's off-chain resolution must not clear A's head)", got)
	}
}

// TestUnstructuredCreationPushMarksBranch covers the ref-creation path
// (before is the zero SHA): the flag is computed from the creation push's
// own evidence, and a later push that resolves the path clears it.
func TestUnstructuredCreationPushMarksBranch(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0306")

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "unstructured-creation@example.com", "hash", "unstructured-creation", "Unstructured Creation Flag")
	if err != nil {
		t.Fatalf("unstructured creation: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "unstructured-creation",
		Name:            "Unstructured Creation Flag",
		Purpose:         "T0306 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("unstructured creation: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'unstructured-creation-repo', 46, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("unstructured creation: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("6", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("unstructured creation: insert fork state: %v", err)
	}
	branch, err := branches.NewService(persistence.NewBranchStore(pool)).Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "fresh",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("unstructured creation: create branch: %v", err)
	}
	// The ref is still unborn: head_sha stays NULL (the branch-creation
	// sync has not landed — the creation push below is what births it).
	store := gitprovider.NewPushIngestStore(pool)
	flagOf := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT semantic_state
			FROM git_branch_semantic_states WHERE branch_id = $1`, branch.ID).Scan(&state); err != nil {
			t.Fatalf("unstructured creation: probe semantic state: %v", err)
		}
		return state
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("unstructured creation: IngestPush(%s): %v", ev.DeliveryID, err)
		}
		return inserted
	}
	event := func(before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          "refs/heads/fresh",
			Before:       before,
			After:        after,
			RepositoryID: 46,
			Owner:        "post-git-svc",
			Name:         "unstructured-creation-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}

	shaX := strings.Repeat("7", 40)
	shaY := strings.Repeat("8", 40)
	if !ingest(event(gitprovider.ZerosSHA, shaX, "d-x"), []gitprovider.ClassifiedChange{
		{Path: "data/junk.json", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("unstructured creation: creation push not inserted")
	}
	if got := flagOf(); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Fatalf("unstructured creation: flag after creation push = %q, want unstructured_changes", got)
	}
	if !ingest(event(shaX, shaY, "d-y"), []gitprovider.ClassifiedChange{
		{Path: "data/junk.json", Kind: gitprovider.ChangeModified, File: gitprovider.FileKindManifest, SchemaID: "mat", ContentSHA256: "junk-manifest-sum"},
	}) {
		t.Fatal("unstructured creation: follow-up push not inserted")
	}
	if got := flagOf(); got != string(gitprovider.BranchSemanticComplete) {
		t.Errorf("unstructured creation: flag after resolution = %q, want semantic_complete", got)
	}
}
