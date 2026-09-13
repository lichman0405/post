package devorchestrator

import (
	"bytes"
	"encoding/json"
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

// DefaultBaseBranch is the integration branch: PRs target it and a task
// branch's contribution is measured against the merge-base with it.
const DefaultBaseBranch = "main"

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
	// The commit moved the task branch; the ledger follows the ref so its record
	// describes where the ref actually is (ref_ledger.go).
	if err := RecordSupervisorRef(opts.RepoRoot, "refs/heads/"+rec.Branch, sha, "commit", opts.TaskID); err != nil {
		return "", fmt.Errorf("recording refs/heads/%s in the Supervisor ref ledger: %w", rec.Branch, err)
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

// runGhJSON runs gh and returns stdout even when it exits non-zero.
// `gh pr checks` exits 1 when a check is failing — the very case whose output
// we need in order to say which one — so a runner that discards stdout on
// failure cannot express the refusal.
func runGhJSON(dir string, args ...string) ([]byte, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil, fmt.Errorf("gh not found on PATH — PR operations require the GitHub CLI")
	}
	cmd := exec.Command(gh, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok && len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// assertRequiredChecksGreen refuses to merge while the PR's required checks
// are not all green ON GITHUB.
//
// The four-gate assertion reads records written by the LOCAL G2 run, which
// executes CI's steps on this machine. That is a faithful replica, not the
// thing itself: a runner image differs, a step can behave differently there,
// and the historical defect this tooling exists to prevent was a merge that
// happened while GitHub's own CI was red. The local record can therefore be
// green while the PR is red, and the merge must refuse on the PR.
func assertRequiredChecksGreen(repoRoot, branch string, required []string) error {
	if len(required) == 0 {
		return nil
	}
	out, err := runGhJSON(repoRoot, "pr", "checks", branch, "--json", "name,state")
	if err != nil {
		return fmt.Errorf("reading the PR's checks for %s: %w", branch, err)
	}
	var checks []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &checks); err != nil {
		return fmt.Errorf("parsing gh pr checks output for %s: %w (got %q)", branch, err, strings.TrimSpace(string(out)))
	}
	state := map[string]string{}
	for _, c := range checks {
		state[c.Name] = c.State
	}
	var missing, bad []string
	for _, want := range required {
		got, ok := state[want]
		switch {
		case !ok:
			missing = append(missing, want)
		case got != "SUCCESS":
			bad = append(bad, want+" ("+got+")")
		}
	}
	if len(missing) > 0 || len(bad) > 0 {
		return fmt.Errorf("the PR's required checks are not all green on GitHub — missing: %s; not passing: %s — the local G2 record is a replica of CI, not CI itself",
			strings.Join(missing, ", "), strings.Join(bad, ", "))
	}
	return nil
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
	number, err := runGh(opts.RepoRoot, "pr", "create", "--base", DefaultBaseBranch, "--head", rec.Branch, "--title", title, "--body", body)
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
	gatesPath := opts.GatesPath
	if gatesPath == "" {
		gatesPath = DefaultGatesPath
	}
	spec, err := gateSpecAt(opts.RepoRoot, gatesPath)
	if err != nil {
		return "", err
	}
	if err := assertRequiredChecksGreen(opts.RepoRoot, rec.Branch, spec.RequiredJobs); err != nil {
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
	base := taskDiffBase(rec)
	// Raw, not trimmed: a diff's trailing whitespace is part of it. See
	// gitOutputRaw — trimming it here produced patches `git apply` called
	// corrupt, so prepareIntegrationTree could not build the tree G2 grades.
	out, err := gitOutputRaw(rec.Worktree, "diff", base, "--")
	if err != nil {
		return "", fmt.Errorf("diffing the worktree against the baseline: %w", err)
	}
	// -z, because these names are used as PATHS below (Lstat, ReadFile) and
	// git's default C-quoting turns a name that needs quoting into a string no
	// filesystem has.
	untracked, err := gitPaths(rec.Worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("listing untracked files for the review diff: %w", err)
	}
	var b strings.Builder
	if out != "" {
		b.WriteString(out)
		// The diff already ends in a newline; a second one would leave a blank
		// line, which is where the trimmed version's short hunk used to be
		// reported as corruption. Keep exactly one.
		if !strings.HasSuffix(out, "\n") {
			b.WriteString("\n")
		}
	}
	for _, p := range untracked {
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
		// The body is built from the raw bytes. It used to be TrimSuffix'd and
		// re-emitted one "\n" per line, which added a byte to a file that had no
		// final newline and turned a file that was a single newline into an empty
		// one — silently, on the advance path, whose whole promise is that it
		// carries the task's work across. Both are byte-level corruption of the
		// deliverable and neither shows up in a path list.
		lines := strings.Split(string(data), "\n")
		finalNewline := false
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1] // Split leaves an empty tail for a file that ends with a newline
			finalNewline = true
		}
		if len(lines) == 0 {
			// An empty file. The header above is the whole patch and git creates
			// the file from it — checked against git, not assumed.
			continue
		}
		fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
		for i, l := range lines {
			b.WriteString("+" + l + "\n")
			if i == len(lines)-1 && !finalNewline {
				b.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return b.String(), nil
}

// taskDiffBase is the commit a task's contribution is measured from: where its
// branch diverged from main, not the recorded baseline. RebaselineTask takes
// its "before" path set from here as well, so the set it compares after the
// advance describes the same thing the patch does.
func taskDiffBase(rec *WorkerRecord) string {
	base := rec.BaselineSHA
	if mb, err := gitOutput(rec.Worktree, "merge-base", DefaultBaseBranch, "HEAD"); err == nil && mb != "" {
		base = mb
	}
	return base
}
