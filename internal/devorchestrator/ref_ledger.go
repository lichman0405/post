package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The Supervisor's ref ledger: every ref the Supervisor creates or moves is
// RECORDED here, at the moment it happens, by the tooling that does it.
//
// It exists because of what the collect gate has to decide — "did this Worker
// create a ref?" — and because the first three answers to it were wrong. A
// Worker creating a ref is a Git control-plane act the guard denies, so a new
// ref during a run is a finding; except that the Supervisor itself creates refs
// while Workers run (one per dispatch, one per pull request), and those are not
// findings.
//
//	Rule 1: "any new ref is a finding" — rejected T0101 because main merely
//	        MOVED while two unrelated PRs merged (L1-20260912-35).
//	Rule 2: "only the task namespace is judged" — let a planted tag through,
//	        and the existing test caught it (L1-20260913-14).
//	Rule 3: "the ref is the Supervisor's when its commit carries the
//	        Supervisor's author email" — this one is FAIL-OPEN. The author (and
//	        committer) identity is a field the committer chooses:
//	        `git -c user.email=… commit-tree` forges it in one flag, so a Worker
//	        that got past the guard could launder a ref it created by claiming
//	        the Supervisor's identity. A security check that reads a
//	        committer-controlled field is bypassable by exactly the adversary it
//	        exists for, and documenting the limitation did not make it smaller.
//
// So attribution here is a RECORD, not an inference. The Supervisor's own
// tooling writes down the refs it creates (spawn, commit, rebaseline, and a
// reconciliation pass for dispatches that predate this file), collect exempts a
// new ref only when it is written down, and everything else — a tag, a branch,
// a ref under a task name no dispatch ever used — is unattributable and is a
// finding. Nothing a Worker can do while obeying its contract enters this file:
// it lives in the Supervisor-owned runtime dir, and every write is made by
// rddev on the Supervisor's behalf.
const supervisorRefsFile = "supervisor-refs.json"

// SupervisorRef is one recorded ref: what it is called, what it pointed at when
// it was recorded, who recorded it and for which task.
type SupervisorRef struct {
	Name   string `json:"name"`
	SHA    string `json:"sha"`
	Source string `json:"source"` // spawn | commit | rebaseline | reconcile | adopt
	TaskID string `json:"task_id,omitempty"`
	At     string `json:"at"`
}

// RefLedger is the on-disk ledger document.
type RefLedger struct {
	Version int             `json:"version"`
	Refs    []SupervisorRef `json:"refs"`
}

// SupervisorRefsPath returns the ledger path in the Supervisor-owned runtime
// dir, next to the driver's lock, status and decisions files.
func SupervisorRefsPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".rddev", "runtime", supervisorRefsFile)
}

// NormalizeRefName accepts a full ref name, a "heads/…" or "tags/…" shorthand,
// or a bare branch name, and returns the full ref name.
func NormalizeRefName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", fmt.Errorf("a ref name is required")
	case strings.HasPrefix(n, "refs/"):
		return n, nil
	case strings.HasPrefix(n, "heads/"), strings.HasPrefix(n, "tags/"):
		return "refs/" + n, nil
	default:
		return "refs/heads/" + n, nil
	}
}

// ReadSupervisorRefs returns the ledger keyed by full ref name.
//
// An unreadable ledger is an ERROR, never an empty ledger. Collect calls this
// to decide what is a finding; "the file did not parse" must not silently
// become a verdict in either direction — not "the Supervisor created nothing"
// (which would reject every concurrent dispatch) and not "everything is the
// Supervisor's" (which is the fail-open this file exists to end).
func ReadSupervisorRefs(repoRoot string) (map[string]SupervisorRef, error) {
	led, err := readRefLedger(repoRoot)
	if err != nil {
		return nil, err
	}
	out := make(map[string]SupervisorRef, len(led.Refs))
	for _, r := range led.Refs {
		if r.Name != "" {
			out[r.Name] = r
		}
	}
	return out, nil
}

// ListSupervisorRefs returns the ledger as a name-ordered slice for display
// (`rddev refs list`). A missing ledger is an empty list, not an error.
func ListSupervisorRefs(repoRoot string) ([]SupervisorRef, error) {
	led, err := readRefLedger(repoRoot)
	if err != nil {
		return nil, err
	}
	out := append([]SupervisorRef{}, led.Refs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readRefLedger reads the raw document; a missing file is an empty ledger.
func readRefLedger(repoRoot string) (*RefLedger, error) {
	path := SupervisorRefsPath(repoRoot)
	// A symlink here would make collect read an arbitrary file with the
	// Supervisor's reach (and the write path below replaces a link rather than
	// following it, so only the read side needs saying out loud).
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &RefLedger{Version: 1, Refs: []SupervisorRef{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the Supervisor ref ledger: %w", err)
	}
	var led RefLedger
	if err := json.Unmarshal(data, &led); err != nil {
		return nil, fmt.Errorf("the Supervisor ref ledger %s does not parse (%v) — fix or remove it deliberately; an unreadable ledger cannot be judged as an empty one", path, err)
	}
	return &led, nil
}

// RecordSupervisorRef records (or re-records) a ref as the Supervisor's own.
// Idempotent per name: the newest record for a name wins, so a ref that moves
// through commit or rebaseline keeps one entry, holding the sha it moved to.
// The read-modify-write runs under the same lock family as every other shared
// state file, so the driver and a Supervisor CLI cannot lose each other's
// writes.
func RecordSupervisorRef(repoRoot, name, sha, source, taskID string) error {
	full, err := NormalizeRefName(name)
	if err != nil {
		return err
	}
	path := SupervisorRefsPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the runtime dir for the ref ledger: %w", err)
	}
	lock, err := lockPathFor(path)
	if err != nil {
		return err
	}
	unlock, err := lockFile(lock)
	if err != nil {
		return err
	}
	defer unlock()

	led, err := readRefLedger(repoRoot)
	if err != nil {
		return err
	}
	entry := SupervisorRef{Name: full, SHA: sha, Source: source, TaskID: taskID, At: nowRFC3339()}
	replaced := false
	for i := range led.Refs {
		if led.Refs[i].Name == full {
			led.Refs[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		led.Refs = append(led.Refs, entry)
	}
	sort.Slice(led.Refs, func(i, j int) bool { return led.Refs[i].Name < led.Refs[j].Name })
	led.Version = 1
	if err := writeFileAtomic(path, marshalIndentBytes(led)); err != nil {
		return fmt.Errorf("writing the Supervisor ref ledger: %w", err)
	}
	return nil
}

// AdoptSupervisorRef records a ref the Supervisor created outside its tooling —
// a branch opened by hand for an investigation. The sha is READ from the ref
// rather than supplied, so the record cannot claim a state the ref is not in,
// and a ref that does not exist is an error rather than a silent no-op.
func AdoptSupervisorRef(repoRoot, name string) (*SupervisorRef, error) {
	full, err := NormalizeRefName(name)
	if err != nil {
		return nil, err
	}
	sha, err := gitOutput(repoRoot, "rev-parse", "--verify", full)
	if err != nil {
		return nil, fmt.Errorf("no such ref %s: %w", full, err)
	}
	if err := RecordSupervisorRef(repoRoot, full, sha, "adopt", ""); err != nil {
		return nil, err
	}
	return &SupervisorRef{Name: full, SHA: sha, Source: "adopt", At: nowRFC3339()}, nil
}

// ReconcileSupervisorRefs records the task branch of every dispatch on disk
// whose branch still exists.
//
// Spawn records its branch from now on, but a dispatch that predates the ledger
// has no entry, and a Worker running across the upgrade would otherwise look
// like a ref its sibling created — a false finding produced by the fix itself.
// Only branches that the Supervisor's OWN record names are adopted: the
// authoritative spawn record, and only when the ref actually exists. A ref
// under a task-shaped name that no dispatch ever recorded stays a finding —
// which is the whole point of the ledger.
//
// The registry is deliberately NOT consulted, though it too names a branch. It
// lives in .rddev/workers/<TASK>/, a directory the Worker must be able to write
// (RESULT.json is its deliverable), so a registry-sourced entry would let a
// Worker nominate the very ref the check exists to report — it would create a
// branch and then write the record that exempts it. That is the same fail-open
// as the author-email rule, one level up: a trust anchor sourced from the gated
// party. A dispatch with no authoritative record is adopted by hand instead
// (`rddev refs adopt <name>`), which is a Supervisor act on a ref the
// Supervisor can see.
func ReconcileSupervisorRefs(repoRoot string) ([]string, error) {
	// Both dispatch records: the authoritative spawn record lives under
	// runtime/tasks (T0012), the registry under workers/. A dispatch from an
	// older layout may have only one of them.
	ids := map[string]bool{}
	for _, dir := range []string{RuntimeTasksRoot(repoRoot), WorkersDir(repoRoot)} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("scanning %s to reconcile the ref ledger: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				ids[e.Name()] = true
			}
		}
	}
	taskIDs := make([]string, 0, len(ids))
	for id := range ids {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)

	var recorded []string
	for _, taskID := range taskIDs {
		// An unreadable or absent authoritative record means "nothing this
		// dispatch recorded", never "ask the Worker instead": skipping leaves
		// the ref a finding, which is the fail-closed direction. (Collect treats
		// an unreadable authoritative record as an error in its own right, so
		// skipping here cannot hide one.)
		gate, err := LoadGateInputs(repoRoot, taskID)
		if err != nil || gate == nil || gate.Branch == "" {
			continue
		}
		branch := gate.Branch
		ref := "refs/heads/" + branch
		sha, err := gitOutput(repoRoot, "rev-parse", "--verify", ref)
		if err != nil {
			continue // merged and deleted, or never created: nothing to record
		}
		if err := RecordSupervisorRef(repoRoot, ref, sha, "reconcile", taskID); err != nil {
			return recorded, err
		}
		recorded = append(recorded, ref)
	}
	sort.Strings(recorded)
	return recorded, nil
}
