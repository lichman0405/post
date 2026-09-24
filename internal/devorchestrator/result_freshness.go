package devorchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Whose RESULT.json is this? (#264)
//
// A re-dispatch reuses the task's result dir and the same file name.
// ReworkWorker keeps the worktree and the result dir (the session is resumed,
// the diff stays), RespawnWorker resets the *worktree* only — and neither
// touches .rddev/workers/<TASK>/, which is where RESULT.json lives. The
// review path has known this since it was written: SpawnReview archives the
// previous verdict (archivePreviousVerdict) AND review collect refuses a
// verdict that predates its own run. The worker path had neither half, so a
// rework whose Worker never rewrote its RESULT.json was collected on the
// PREVIOUS attempt's document — the defect #264 named, and the one that let a
// rejected attempt's own completion claim (acceptance entries, tests,
// follow-ups and all) come back as this round's, green.
//
// The repair is a criterion, not a patch. Deleting the file at re-dispatch
// would hide the stale case without answering the question it raises: after
// the fact nothing would say whether the document collect judged was written
// by this attempt. What follows is that question, asked of the document — and
// the answer is recorded in the collect report like every other check.
//
// TWO INSTRUMENTS, because each one alone is blind to a documented shape:
//
//   - DispatchedAt (time): the document must not predate the instant this
//     attempt was dispatched. Blind to a document whose mtime was refreshed
//     in place (a restore, a copy, a checkout that re-materialises the file):
//     the bytes are the previous attempt's, the timestamp says "now", and the
//     time comparison accepts it.
//   - ResultSHAAtSpawn (content): the document must not be byte-for-byte the
//     one that was already in the result dir at dispatch. Blind to a
//     re-dispatch that rewrites its own document identically — see the
//     false-hit note below — and inert for a first dispatch, where there was
//     no earlier document to fingerprint.
//
// Neither instrument reads the document's own claims: task_id and status are
// written by the Worker, so a stale document carries the right task_id and a
// completed status, which is precisely why its content had to be tied to an
// instant the Worker does not write.
//
// WHAT THIS CANNOT DO, stated rather than left to be discovered. It cannot
// tell a Worker that was reworked and re-delivered the previous attempt's
// document byte-for-byte from one that wrote nothing at all — and by this
// contract's own words ("write it EARLY and overwrite it as evidence
// improves") those are the same report. Both are refused, with a message that
// says what to do about it. Every other shape is placed by the two
// instruments, and the directions that matter are pinned in
// result_freshness_test.go: a stale document is refused even when its mtime
// has been refreshed, and a document this attempt wrote and then rewrote
// (the contract's normal shape) is accepted.

// FreshnessInstrument names one way of placing a RESULT.json in time. It is
// part of the collect report, so a reader can tell which question was asked.
type FreshnessInstrument string

const (
	// FreshnessByDispatch names the mtime-vs-dispatch-time question.
	FreshnessByDispatch FreshnessInstrument = "dispatch-time"
	// FreshnessByContent names the byte-identity-to-the-fingerprint question.
	FreshnessByContent FreshnessInstrument = "dispatch-content"
)

// ResultFreshness is one instrument's finding. Refused is the verdict; Detail
// is the evidence either way (which instant, which digest) — a refusal that
// does not say what it measured is unactionable for the Worker that has to
// rewrite the document.
type ResultFreshness struct {
	Instrument FreshnessInstrument
	Refused    bool
	Detail     string
}

// resultDigestAt is the sha256 of the RESULT.json present in a task result
// dir, or "" when there is none. It is what WriteGateInputs records at
// dispatch; a result dir that does not exist yet is simply "no document".
func resultDigestAt(resultDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(resultDir, "RESULT.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the RESULT.json present at dispatch in %s: %w", resultDir, err)
	}
	return sha256Hex(data), nil
}

// sha256Hex is the digest form the freshness record and the review path's
// fingerprints both use.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// CheckResultFreshness judges whether the document at resultPath belongs to
// the attempt being collected, and returns one finding per instrument.
//
// gate is the authoritative dispatch record and rec the registry entry; a nil
// gate (a pre-T0012 spawn) degrades the time instrument to the registry's
// StartedAt and drops the content instrument, the same way every other
// authoritative-input check in Collect degrades.
//
// No document at all yields no findings: a missing RESULT.json is the schema
// check's to report, and two checks saying "missing" would only make the
// refusal longer.
func CheckResultFreshness(resultPath string, gate *GateInputs, rec *WorkerRecord) ([]ResultFreshness, error) {
	st, err := os.Stat(resultPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("statting RESULT.json for the freshness check: %w", err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, fmt.Errorf("reading RESULT.json for the freshness check: %w", err)
	}
	digest := sha256Hex(data)

	findings := []ResultFreshness{}

	// The content instrument: is this the document that was already here?
	// Only answerable when dispatch recorded one — for a first dispatch the
	// fingerprint is empty and the question does not apply. A refusal here is
	// the more specific of the two, so it is reported first.
	if gate != nil && gate.ResultSHAAtSpawn != "" {
		f := ResultFreshness{Instrument: FreshnessByContent}
		if digest == gate.ResultSHAAtSpawn {
			f.Refused = true
			f.Detail = fmt.Sprintf(
				"RESULT.json is byte-for-byte the document that was already in %s when this attempt was dispatched (sha256 %s, as recorded at dispatch) — no new RESULT.json was written for this attempt, so this is the earlier round's document. Write the document for THIS attempt (status, tests, acceptance evidence, risks) and collect again",
				filepath.Dir(resultPath), digest)
		} else {
			f.Detail = fmt.Sprintf("RESULT.json (sha256 %s) is not the document dispatch found in the result dir (sha256 %s) — it was written during this attempt", digest, gate.ResultSHAAtSpawn)
		}
		findings = append(findings, f)
	}

	// The time instrument: was it written after this attempt was dispatched?
	dispatched, source := "", ""
	switch {
	case gate != nil && gate.DispatchedAt != "":
		dispatched, source = gate.DispatchedAt, "the authoritative dispatch record"
	case rec != nil && rec.StartedAt != "":
		// Pre-T0012 spawn (no authoritative record): the registry's start is
		// the only reading there is. It is taken after the process exists, so
		// it is LATER than the dispatch instant and can only ever accept more
		// than the authoritative value would — it cannot manufacture a refusal
		// the dispatch record would not also produce, which is the direction
		// that matters for a degraded path.
		dispatched, source = rec.StartedAt, "the registry record (no authoritative dispatch record)"
	}
	f := ResultFreshness{Instrument: FreshnessByDispatch}
	switch at, perr := time.Parse(time.RFC3339, dispatched); {
	case dispatched == "":
		// Fail closed, and loudly, for the reason the ref ledger fails closed:
		// with no reading at all, "which attempt wrote this document" is
		// unknowable, and answering it either way would be worse than
		// refusing. A registry without started_at is not a shape this code
		// writes (it is a required field of worker-registry.schema.json).
		f.Refused = true
		f.Detail = "no dispatch instant is recorded for this attempt (the authoritative record carries none and the registry has no started_at) — the document cannot be attributed to a run"
	case perr != nil:
		f.Refused = true
		f.Detail = fmt.Sprintf("the dispatch instant recorded in %s is not a usable RFC 3339 time (%q) — a document cannot be attributed to a run whose start does not parse", source, dispatched)
	case st.ModTime().Before(at):
		// Both times at full precision: rendered to the second they are the
		// same string whenever the two fall inside one second, and the refusal
		// would read "written 11:51:44Z, before this attempt was dispatched
		// (11:51:44Z)" — a sentence that asks the reader to take on faith the
		// very ordering it asserts (the same reasoning as review collect's
		// verdict check).
		f.Refused = true
		f.Detail = fmt.Sprintf("RESULT.json was written %s, before this attempt was dispatched (%s, from %s) — it is an earlier round's document, and collecting it would record that round's claims as this one's work",
			st.ModTime().UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano), source)
	default:
		f.Detail = fmt.Sprintf("RESULT.json was written %s, after this attempt was dispatched (%s, from %s)",
			st.ModTime().UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano), source)
	}
	findings = append(findings, f)
	return findings, nil
}
