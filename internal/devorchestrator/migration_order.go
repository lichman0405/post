package devorchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Migration merge order.
//
// A migration number is allocated at dispatch (migration_numbers.go) so that
// concurrent Workers never collide. That is a rule about NAMES. This file is
// about a second, sharper rule about ORDER, and it exists because the two are
// not the same rule and only the first one was enforced.
//
// The runner is goose v3 (internal/persistence/migrate.go), and goose REFUSES
// out-of-order migrations by default: `Provider.Up` halts with
//
//	found 1 missing (out-of-order) migration: [00035_x.sql]
//
// when the database has already applied a HIGHER version. The option that
// permits it, WithAllowOutofOrder, is not set, and setting it would trade a
// loud refusal for a silently reordered schema history. So a merge order that
// puts 00036 on main before 00034 is not cosmetic: every database that has
// already migrated to 00036 stops migrating afterwards. That is the
// development database today and every deployed one later.
//
// CI cannot catch it. Every CI job migrates a FRESH database, where the files
// are applied in lexical order and the set is complete: 1, 2, ... 34, 35, 36.
// The failure exists only for a database that has already applied the higher
// number, which is exactly the state CI never has. A green CI on such a merge
// is not evidence about the upgrade path, so the check has to happen before the
// merge, on the thing that would create the gap.
//
// The driver merges whichever task is ready first — it has no notion of
// migration order — and that is the correct place for that ignorance: nothing
// about a task being ready says its migration may land next. So the refusal
// lives in the merge action itself, which is the one door every merge goes
// through, whether it is the driver or a human on the other side.

// migrationFilesInWorktree lists the migration file names a worktree holds.
//
// The file system, not Git: the work of a task in flight is an UNCOMMITTED
// working-tree diff (that is what a Worker produces), so a query against a
// ref would answer "this task has no migrations" for every task that has one.
func migrationFilesInWorktree(worktree string) []string {
	entries, err := os.ReadDir(filepath.Join(worktree, "infra", "migrations"))
	if err != nil {
		return nil // no worktree, or no migrations: nothing to carry
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if migrationFileRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// migrationsOnIntegrationBranch returns the migration file names the
// integration branch has, keyed by base name.
//
// The tip is resolved and read the way every other reader of "what is main"
// resolves it, rather than from the local checkout: a merge performed through
// the forge leaves the local branch where it was, so a check that read the
// working tree would answer "00034 has not merged yet" for a task whose merge
// it just performed — and then refuse the next merge for a reason that is no
// longer true.
func migrationsOnIntegrationBranch(repoRoot string) (map[string]bool, error) {
	tip, err := IntegrationTip(repoRoot)
	if err != nil {
		return nil, err
	}
	out, err := gitOutput(repoRoot, "ls-tree", "--name-only", tip, "infra/migrations/")
	if err != nil {
		return nil, fmt.Errorf("listing the migrations of %s at %s: %w", DefaultBaseBranch, tip, err)
	}
	onMain := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			onMain[filepath.Base(line)] = true
		}
	}
	return onMain, nil
}

// pendingMigrationNumbers returns, sorted, the numbers of the migrations a
// worktree holds that the integration branch does not have yet — the
// migrations that would land on it if this task merged now.
//
// A file present on the branch is not pending however new the worktree is: a
// worktree whose baseline predates a merge still holds that merge's migration
// file, and counting it would make every task block every other. The set
// difference is the precise question, and unlike comparing numbers to a
// maximum it needs no assumption about which side is ahead.
func pendingMigrationNumbers(files []string, onIntegration map[string]bool) []int {
	var out []int
	for _, name := range files {
		if onIntegration[name] {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// assertMigrationMergeOrder refuses a merge that would put a migration on the
// integration branch while a LOWER-numbered one is still waiting in another
// task's worktree.
//
// Not "refuses a merge whose number is below the branch's highest": by the time
// that is true the gap already exists, and refusing then would leave the
// lower-numbered task permanently unmergeable — a deadlock in place of a
// hazard. The question has to be asked of the tasks that are still HOLDING
// migrations, and it has to be asked before the first gap is created.
//
// It deliberately does not consult the task state. The state is bookkeeping;
// the worktree is the thing that would merge. A task parked in `rejected` with
// a migration in its tree still blocks the higher numbers, and that is the
// intent: either it merges, or the work it is holding is removed — which is a
// decision, not a timeout.
func assertMigrationMergeOrder(repoRoot string, rec *WorkerRecord) error {
	// A task that carries no migration cannot create a gap, and that is asked
	// first and of the file system alone: this runs on every merge, and merging
	// a task that only touches Go code must not come to depend on being able to
	// read the integration branch.
	mine := migrationFilesInWorktree(rec.Worktree)
	if len(mine) == 0 {
		return nil
	}
	onIntegration, err := migrationsOnIntegrationBranch(repoRoot)
	if err != nil {
		// Known to be carrying migrations and unable to read what is already
		// applied: refusing is the honest answer. The order is not known to be
		// wrong, it is unknown, and an unknown order for migrations is the one
		// thing this function exists to not merge.
		return fmt.Errorf("refusing to merge %s: it carries migrations, and the migrations already on %s cannot be read, so the merge order cannot be checked: %w",
			rec.TaskID, DefaultBaseBranch, err)
	}
	pending := pendingMigrationNumbers(mine, onIntegration)
	if len(pending) == 0 {
		return nil
	}
	lowest := pending[0]

	type blocker struct {
		task   string
		number int
	}
	var blockers []blocker
	entries, err := os.ReadDir(WorktreesDir(repoRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no worktrees at all: nothing else is holding a migration
		}
		return fmt.Errorf("listing the task worktrees to check the migration order: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == rec.TaskID {
			continue
		}
		// `<TASK>-review` is a Review Worker's scratch directory, not a task
		// worktree: it is never merged, so nothing in it can land on main.
		if strings.HasSuffix(e.Name(), "-review") {
			continue
		}
		wt := filepath.Join(WorktreesDir(repoRoot), e.Name())
		for _, n := range pendingMigrationNumbers(migrationFilesInWorktree(wt), onIntegration) {
			if n < lowest {
				blockers = append(blockers, blocker{task: e.Name(), number: n})
			}
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].number != blockers[j].number {
			return blockers[i].number < blockers[j].number
		}
		return blockers[i].task < blockers[j].task
	})
	first := blockers[0]

	return fmt.Errorf(
		"refusing to merge %s: it carries migration %05d, and %s still holds %05d, which %s does not have yet. "+
			"The migration runner (goose v3, internal/persistence/migrate.go) refuses out-of-order migrations: a database that has already applied %05d halts with "+
			"\"found 1 missing (out-of-order) migration\" before it will apply %05d, and CI cannot see it because every CI job migrates a fresh database. "+
			"Merge %s (migration %05d) first, or remove the work it is holding if that task is not going to merge",
		rec.TaskID, lowest, first.task, first.number, DefaultBaseBranch, lowest, first.number, first.task, first.number)
}
