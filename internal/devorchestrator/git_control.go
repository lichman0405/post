package devorchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The Supervisor-only Git control plane (T0012 requirement 6: "only
// Supervisor performs commit/push/PR/merge and tooling refuses when a gate is
// red"). Every action first runs the four-gate assertion (CheckMergeGate)
// against the on-disk evidence records and REFUSES — exit before any git/gh
// invocation — while any gate is red or missing. Every action that does run
// is recorded as a GitRecord evidence file, so the merge trail is auditable
// from disk alone. gh is resolved from PATH, so e2e tests substitute a fake
// recorder; git runs in the task worktree (commit) and the repo root
// (ref/push plumbing).

// GitControlOpts parametrizes one Supervisor git/PR action.
type GitControlOpts struct {
	RepoRoot  string
	GatesPath string
	TaskID    string
	RunID     string
	CommitMsg string // default: rendered from the DAG entry
	PRTitle   string // default: rendered from the DAG entry
	PRBody    string // default: rendered from the DAG entry
	DagPath   string
	StatePath string
}

// GateRefusalError is returned when a git/PR action is refused by a red
// gate. It is the mechanical refusal the Supervisor's tooling must obey —
// the action never reaches git or gh.
type GateRefusalError struct {
	Action  string
	Reasons []string
}

func (e *GateRefusalError) Error() string {
	return fmt.Sprintf("%s refused by the four-gate assertion: %s", e.Action, strings.Join(e.Reasons, "; "))
}

// assertGateGreen runs the four-gate assertion and converts a red result
// into a refusal error carrying every reason (the reasons are the evidence).
func assertGateGreen(opts *GitControlOpts, action string) (*Gate4Result, error) {
	res, err := CheckMergeGate(opts.RepoRoot, opts.GatesPath, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if res.Status != "passed" {
		return res, &GateRefusalError{Action: action, Reasons: res.Reasons}
	}
	return res, nil
}

// loadWorktreeRecord returns the registry record of the task's Worker (the
// worktree and branch live there); missing = the task was never spawned.
func loadWorktreeRecord(repoRoot, taskID string) (*WorkerRecord, error) {
	rec, err := LoadRegistry(repoRoot, taskID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no recorded Worker for %s — commit requires a spawned and collected task worktree", taskID)
	}
	return rec, nil
}

// CommitTask commits the collected worktree diff on the task branch. It
// refuses while any gate is red (the four-gate assertion) and refuses when
// the worktree has no changes. The commit is created in the task worktree on
// its task branch — the exact contents collect verified.
func CommitTask(opts *GitControlOpts) (string, error) {
	gateRes, err := assertGateGreen(opts, "commit")
	if err != nil {
		return "", err
	}
	rec, err := loadWorktreeRecord(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return "", err
	}
	// Nothing to commit is a refusal, not an empty success — an empty commit
	// would look like a deliverable.
	out, err := gitOutput(rec.Worktree, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("checking the worktree for changes: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("worktree %s has no changes — nothing to commit", rec.Worktree)
	}
	msg := opts.CommitMsg
	if msg == "" {
		store, err := OpenStore(opts.DagPath, opts.StatePath)
		if err != nil {
			return "", err
		}
		spec := store.dag.Get(opts.TaskID)
		if spec == nil {
			return "", fmt.Errorf("unknown task %s in task DAG", opts.TaskID)
		}
		msg = fmt.Sprintf("[%s] %s\n\nCollected by rddev; four-gate evidence in\n.rddev/runtime/gates/%s/ (gate status %s).\n", opts.TaskID, spec.Title, opts.TaskID, gateRes.Status)
	}
	_, err = gitOutput(rec.Worktree, "add", "-A")
	if err != nil {
		return "", fmt.Errorf("staging the worktree diff: %w", err)
	}
	if _, err := gitOutput(rec.Worktree, "commit", "-m", msg); err != nil {
		return "", fmt.Errorf("committing the worktree diff: %w", err)
	}
	sha, err := gitOutput(rec.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading the new commit sha: %w", err)
	}
	if _, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordGit+"-commit", runIDOr(opts.RunID), &GitRecord{
		recordMeta: recordMeta{RecordType: RecordGit, TaskID: opts.TaskID, RunID: runIDOr(opts.RunID), At: nowRFC3339()},
		Action:     "commit",
		Branch:     rec.Branch,
		CommitSHA:  sha,
		GateStatus: gateRes.Status,
	}); err != nil {
		return "", err
	}
	return sha, nil
}

func runIDOr(runID string) string {
	if runID == "" {
		return NewRunID()
	}
	return runID
}

// PushTask pushes the task branch to origin (the normalized canonical
// remote). It refuses while any gate is red and requires a local commit the
// remote does not have (an empty push is a no-op, not a deliverable).
func PushTask(opts *GitControlOpts) error {
	gateRes, err := assertGateGreen(opts, "push")
	if err != nil {
		return err
	}
	rec, err := loadWorktreeRecord(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return err
	}
	remote, err := gitOutput(rec.Worktree, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(remote) == "" {
		return fmt.Errorf("resolving origin for the push: %w (a canonical origin is required before any push)", err)
	}
	// Only the task branch moves; `git push origin <branch>` pushes exactly
	// the collected commit and nothing else.
	if _, err := gitOutput(rec.Worktree, "push", "origin", rec.Branch); err != nil {
		return fmt.Errorf("pushing %s: %w", rec.Branch, err)
	}
	if _, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordGit+"-push", runIDOr(opts.RunID), &GitRecord{
		recordMeta: recordMeta{RecordType: RecordGit, TaskID: opts.TaskID, RunID: runIDOr(opts.RunID), At: nowRFC3339()},
		Action:     "push",
		Branch:     rec.Branch,
		GateStatus: gateRes.Status,
	}); err != nil {
		return err
	}
	return nil
}

// runGh runs the gh CLI from PATH (fakeable in e2e) and returns its combined
// output. A missing gh is an explicit error — PR operations are real remote
// actions, never silently skipped.
func runGh(dir string, args ...string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh not found on PATH — PR operations require the GitHub CLI")
	}
	cmd := exec.Command(gh, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// OpenPR opens a pull request for the task branch (base main). It refuses
// while any gate is red and refuses when a PR for the branch already exists
// (gh pr view succeeds). The returned number is the PR's own identifier.
func OpenPR(opts *GitControlOpts) (string, error) {
	gateRes, err := assertGateGreen(opts, "pr open")
	if err != nil {
		return "", err
	}
	rec, err := loadWorktreeRecord(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return "", err
	}
	if _, err := runGh(opts.RepoRoot, "pr", "view", rec.Branch, "--json", "number"); err == nil {
		return "", fmt.Errorf("a PR for branch %s already exists — refusing to open a duplicate", rec.Branch)
	}
	title := opts.PRTitle
	body := opts.PRBody
	if title == "" {
		store, err := OpenStore(opts.DagPath, opts.StatePath)
		if err != nil {
			return "", err
		}
		spec := store.dag.Get(opts.TaskID)
		if spec == nil {
			return "", fmt.Errorf("unknown task %s in task DAG", opts.TaskID)
		}
		title = fmt.Sprintf("[%s] %s", opts.TaskID, spec.Title)
		body = fmt.Sprintf("Gate evidence: .rddev/runtime/gates/%s/ (four-gate status %s).\n", opts.TaskID, gateRes.Status)
	}
	number, err := runGh(opts.RepoRoot, "pr", "create", "--base", "main", "--head", rec.Branch, "--title", title, "--body", body)
	if err != nil {
		return "", fmt.Errorf("opening the PR: %w", err)
	}
	number = strings.TrimSpace(number)
	if _, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordGit+"-pr-open", runIDOr(opts.RunID), &GitRecord{
		recordMeta: recordMeta{RecordType: RecordGit, TaskID: opts.TaskID, RunID: runIDOr(opts.RunID), At: nowRFC3339()},
		Action:     "pr-open",
		Branch:     rec.Branch,
		PRNumber:   number,
		GateStatus: gateRes.Status,
	}); err != nil {
		return "", err
	}
	return number, nil
}

// MergePR merges the task's PR (squash) after the four-gate assertion. The
// refusal is demonstrated mechanically: a red G4 (missing/red G2 job, stale
// evidence, missing review) exits before gh is ever invoked. On success the
// accepted -> merged state transition is applied — the merge tooling owns the
// bookkeeping, exactly like spawn owns ready -> running.
func MergePR(opts *GitControlOpts) (string, error) {
	gateRes, err := assertGateGreen(opts, "pr merge")
	if err != nil {
		return "", err
	}
	rec, err := loadWorktreeRecord(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return "", err
	}
	out, err := runGh(opts.RepoRoot, "pr", "merge", rec.Branch, "--squash", "--delete-branch")
	if err != nil {
		return "", fmt.Errorf("merging the PR: %w", err)
	}
	// The state transition is part of the merge action; a merge whose
	// bookkeeping fails must be reported, not silently re-merged later.
	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return "", err
	}
	if _, err := store.Transition(opts.TaskID, StateMerged, runIDOr(opts.RunID), "pr merge via rddev (four gates green)"); err != nil {
		return "", fmt.Errorf("PR merged but the accepted -> merged transition failed: %w", err)
	}
	if _, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordGit+"-pr-merge", runIDOr(opts.RunID), &GitRecord{
		recordMeta:  recordMeta{RecordType: RecordGit, TaskID: opts.TaskID, RunID: runIDOr(opts.RunID), At: nowRFC3339()},
		Action:      "pr-merge",
		Branch:      rec.Branch,
		MergeCommit: strings.TrimSpace(out),
		GateStatus:  gateRes.Status,
	}); err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GitActionResult is the CLI-visible outcome of one Supervisor git/PR action.
type GitActionResult struct {
	TaskID string `json:"task_id"`
	Action string `json:"action"`
	Result string `json:"result"` // commit sha | PR number | merge sha | pushed
	Gate   string `json:"gate"`   // the four-gate status the action ran under
}

// RunGitControl dispatches one action and shapes the result for the CLI.
func RunGitControl(opts *GitControlOpts, action string) (*GitActionResult, error) {
	gateRes, err := CheckMergeGate(opts.RepoRoot, opts.GatesPath, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if gateRes.Status != "passed" {
		return nil, &GateRefusalError{Action: action, Reasons: gateRes.Reasons}
	}
	switch action {
	case "commit":
		sha, err := CommitTask(opts)
		if err != nil {
			return nil, err
		}
		return &GitActionResult{TaskID: opts.TaskID, Action: "commit", Result: sha, Gate: gateRes.Status}, nil
	case "push":
		if err := PushTask(opts); err != nil {
			return nil, err
		}
		return &GitActionResult{TaskID: opts.TaskID, Action: "push", Result: "pushed", Gate: gateRes.Status}, nil
	case "pr-open":
		number, err := OpenPR(opts)
		if err != nil {
			return nil, err
		}
		return &GitActionResult{TaskID: opts.TaskID, Action: "pr-open", Result: number, Gate: gateRes.Status}, nil
	case "pr-merge":
		sha, err := MergePR(opts)
		if err != nil {
			return nil, err
		}
		return &GitActionResult{TaskID: opts.TaskID, Action: "pr-merge", Result: sha, Gate: gateRes.Status}, nil
	}
	return nil, fmt.Errorf("unknown git action %q (valid: commit, push, pr-open, pr-merge)", action)
}

// taskWorktreeDiff returns the COMPLETE change in the task worktree against
// its baseline: the tracked diff plus every untracked file, which `git diff`
// omits entirely (used by the review prompt renderer).
//
// The omission is not cosmetic. A task that adds files adds them untracked, so
// the review input described a fraction of the work: T0101's diff.txt carried
// 9 of its 39 changed paths, and a Reviewer that trusted the document would
// have reviewed under a quarter of the change and been entitled to approve the
// rest unseen. The second Reviewer noticed and read the worktree instead —
// which is exactly the diligence a review input must not depend on.
func taskWorktreeDiff(rec *WorkerRecord) (string, error) {
	out, err := gitOutput(rec.Worktree, "diff", rec.BaselineSHA, "--")
	if err != nil {
		return "", fmt.Errorf("diffing the worktree against the baseline: %w", err)
	}
	untracked, err := gitOutput(rec.Worktree, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("listing untracked files for the review diff: %w", err)
	}
	var b strings.Builder
	if out != "" {
		b.WriteString(out)
		b.WriteString("\n")
	}
	for _, p := range strings.Split(untracked, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs := filepath.Join(rec.Worktree, p)
		// Lstat, not Stat: a symlink is reported as the link, never followed.
		// The paths come from the Worker's own tree, so a symlink pointing at
		// /etc/… or a credential file would otherwise have the SUPERVISOR read
		// the target and embed it in a durable artifact (the review input),
		// turning "summarise the change" into an arbitrary-file-read primitive.
		// Git records a symlink as its target path, and so do we.
		st, err := os.Lstat(abs)
		if err != nil {
			return "", fmt.Errorf("stat-ing untracked %s for the review diff: %w", p, err)
		}
		if !st.Mode().IsRegular() {
			if st.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(abs)
				if err != nil {
					return "", fmt.Errorf("reading symlink %s for the review diff: %w", p, err)
				}
				fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 120000\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1 @@\n+%s\n\\ No newline at end of file\n", p, p, p, target)
			}
			// Directories (a gitlink or an empty dir) carry no content.
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", fmt.Errorf("reading untracked %s for the review diff: %w", p, err)
		}
		mode := "100644"
		if st.Mode()&0o111 != 0 {
			mode = "100755"
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode %s\n--- /dev/null\n+++ b/%s\n", p, p, mode, p)
		// A NUL byte means Git would call it binary; say so rather than emit a
		// body that is not a valid patch.
		if bytes.IndexByte(data, 0) >= 0 {
			fmt.Fprintf(&b, "Binary files /dev/null and b/%s differ\n", p)
			continue
		}
		body := strings.TrimSuffix(string(data), "\n")
		if body == "" {
			continue
		}
		lines := strings.Split(body, "\n")
		fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
		for _, l := range lines {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String(), nil
}
