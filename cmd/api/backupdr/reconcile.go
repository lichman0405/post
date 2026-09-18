package backupdr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The post-restore reconciliation of docs/37 §一致性:
//
//	恢复后运行 reconciliation：DB ↔ Git refs ↔ blob hashes ↔ release manifests
//
// Four axes, run against a RESTORED environment:
//
//	git_refs          — every recorded branch ref exists in the restored
//	                    repository at the recorded commit, and no ref the
//	                    restored repository carries is unrecorded.
//	blob_hashes       — every blobs row's object exists in the restored
//	                    store and its bytes digest to the recorded
//	                    content_hash (docs/17 §3: content hash + blob id is
//	                    the blob's identity; the size is the same row's
//	                    size_bytes).
//	release_manifests — every release row's stored document still digests
//	                    to its manifest_hash, verified by the release
//	                    package's own VerifyHash — the same check the
//	                    product's manifest export performs before it serves
//	                    a release.
//	release_pins      — the cross axis the arrows in the document describe:
//	                    each release manifest's embedded state pins a git
//	                    ref and a set of blob hashes, and each of those pins
//	                    must resolve against the restored Git repositories
//	                    and the restored object store. This is the axis that
//	                    can only pass if the other three are consistent
//	                    with the manifests, which is why it is separate
//	                    from them.
//
// # NEVER repairs
//
// Drift is reported, never repaired — the semantics cmd/api/reconciliation.go
// states for the Git ↔ RSG loop and migration 00047 pins at the storage
// layer ("the reconciler NEVER repairs … the proposal is recorded, not
// applied"). Two things make that structural here rather than a promise:
//
//  1. ReconcileSource — the ONLY surface Reconcile can reach — declares
//     reads and nothing else. There is no write method for a repair to
//     call, the same construction internal/gitprovider's ReconcilerPort
//     uses for the provider half.
//  2. Each finding carries a RepairProposal: the exact action a repair
//     WOULD take. It is returned in the report, so a human can read the
//     options; nothing executes it.
//
// The reconciler's own bookkeeping — the audit row a finding appends — is
// the one write in this file, and it is a report, not a repair: it goes to
// audit_log, which the schema makes immutable (migrations 00014/00015).

// Axis names the comparison group a finding belongs to.
type Axis string

const (
	AxisGitRefs          Axis = "git_refs"
	AxisBlobHashes       Axis = "blob_hashes"
	AxisReleaseManifests Axis = "release_manifests"
	AxisReleasePins      Axis = "release_pins"
)

// AllAxes is every axis a pass runs, in the order the document names them.
var AllAxes = []Axis{AxisGitRefs, AxisBlobHashes, AxisReleaseManifests, AxisReleasePins}

// Finding kinds. One per distinguishable way an axis can disagree, because
// a single "drift" kind would leave an operator to re-derive which of two
// very different repairs applies.
const (
	KindRepoUnmapped          = "repo_unmapped"                     // a branch ref's repository is not resolvable in the target
	KindRefMissing            = "ref_missing"                       // the recorded ref is not in the restored repository
	KindRefHeadMoved          = "ref_head_moved"                    // the restored ref is at a different commit
	KindUnmappedRef           = "unmapped_ref"                      // a restored ref no branch row records
	KindBlobMissing           = "blob_missing"                      // a blobs row's object is not in the restored store
	KindBlobHashMismatch      = "blob_content_hash_mismatch"        // the restored bytes do not digest to the recorded hash
	KindBlobSizeMismatch      = "blob_size_mismatch"                // the restored size is not the recorded size
	KindReleaseUnverifiable   = "release_manifest_unverifiable"     // the stored release document does not verify
	KindReleaseRowMismatch    = "release_manifest_row_mismatch"     // the document's hash is not the row's manifest_hash
	KindAssetManifestMismatch = "asset_manifest_integrity_mismatch" // a published asset version's document does not digest to its integrity_hash
	KindReleaseStateMissing   = "release_state_missing"
	KindReleaseStateMismatch  = "release_state_hash_mismatch"
	KindReleaseGitPinMissing  = "release_git_pin_missing"  // the release's pinned commit is not an object in the restored repository
	KindReleaseBlobMissing    = "release_blob_pin_missing" // a blob hash a release manifest pins has no row
)

// SeverityHigh is the one severity the pass writes, as migration 00047
// pins for the project's other reconciliation surface: "ANY drift is a
// high-severity operational alert" (docs/16 §5).
const SeverityHigh = "high"

// driftAuditAction is the audit_log.action of the row every finding
// appends. Dotted and lowercase, the naming internal/domain/audit.go uses;
// the Activity feed renders it without a new read surface.
const driftAuditAction = "backup.restore.drift_detected"

// Finding is one drift instance. The field set is the one
// git_reconciliation_findings uses, minus the columns that table's CHECK
// constraints pin to the Git ↔ RSG classes (see doc.go for why the findings
// ride audit_log instead).
type Finding struct {
	Axis      Axis   `json:"axis"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject_ref"`
	ProjectID string `json:"project_id,omitempty"`
	Expected  string `json:"expected"`
	Actual    string `json:"actual"`
	// RepairProposal is what a repair WOULD do. It is recorded and never
	// applied: the reconciler holds no method to apply it with, and the
	// decision to repair is a human's.
	RepairProposal string `json:"repair_proposal"`
}

// ReconcileReport is one pass's result.
type ReconcileReport struct {
	PassID string `json:"pass_id"`
	// SnapshotTimestamp is the backup instant the target was restored to,
	// read back from the restore manifest — this is what makes the report
	// answer "reconciled against which moment" rather than "at some point".
	SnapshotTimestamp string       `json:"snapshot_timestamp"`
	Checked           map[Axis]int `json:"checked"`
	Findings          []Finding    `json:"findings"`
	Verdict           string       `json:"verdict"`
}

// Clean reports whether the pass found no drift.
func (r ReconcileReport) Clean() bool { return len(r.Findings) == 0 }

// ReconcileSource is the ENTIRE surface the reconciler may reach. Every
// method reads. That is the structural half of "the reconciler NEVER
// repairs": a repair is not something this pass declines to do, it is
// something it has no way to do.
type ReconcileSource interface {
	// BranchRefs reads the canonical branch/ref mapping rows.
	BranchRefs(ctx context.Context) ([]BranchRefRow, error)
	// Blobs reads the canonical blob rows.
	Blobs(ctx context.Context) ([]BlobRow, error)
	// Releases reads the canonical release rows.
	Releases(ctx context.Context) ([]ReleaseRow, error)
	// AssetVersions reads the published asset version rows.
	AssetVersions(ctx context.Context) ([]AssetVersionRow, error)
	// States maps project_states ids to their state hash and pinned commit.
	States(ctx context.Context) (map[string]StateRow, error)
	// ReadObject returns one object's bytes from the restored store.
	ReadObject(ctx context.Context, storageKey string) ([]byte, error)
	// RemoteRefs returns the refs of one restored repository.
	RemoteRefs(ctx context.Context, owner, repo string) (map[string]string, error)
	// HasCommit reports whether a commit is an object of one restored
	// repository — the Git half of the release-manifest axis, which asks
	// about a pinned commit that need not be any ref's tip.
	HasCommit(ctx context.Context, owner, repo, sha string) (bool, error)
	// DefaultRefs names each restored repository's default ref, so the
	// reverse "unrecorded ref" check does not mistake the default branch
	// for a ref nobody recorded.
	DefaultRefs(ctx context.Context, owner, repo string) (string, error)
}

// BranchRefRow is one canonical branch ↔ provider ref mapping row joined to
// the coordinates of its project's repository.
type BranchRefRow struct {
	ProjectID string
	Owner     string
	Repo      string
	GitRef    string
	HeadSHA   string
	SyncState string
}

// BlobRow is one canonical blob row: the identity pair (content_hash,
// size_bytes) and where the bytes live.
type BlobRow struct {
	ID          string
	ContentHash string
	SizeBytes   int64
	StorageKey  string
}

// ReleaseRow is one release row: the project it belongs to, its pinned
// state, the stored manifest document and the hash that document must
// digest to.
type ReleaseRow struct {
	ID           string
	ProjectID    string
	Version      string
	StateID      string
	Manifest     []byte
	ManifestHash string
}

// AssetVersionRow is one published asset version row.
type AssetVersionRow struct {
	ID            string
	Manifest      []byte
	IntegrityHash string
}

// StateRow is one project state's identity.
type StateRow struct {
	ProjectID    string
	StateHash    string
	GitCommitSHA *string
}

// objectResolver resolves a blob row's storage key against the restored
// store, counting reads so the report can say how many points were
// actually checked rather than how many rows were listed.
// FindingSink is where a pass RECORDS what it found, as opposed to what it
// returns.
//
// It is a required parameter of every pass rather than something a caller
// does afterwards, and that is the whole point. docs/37 §V1 验收 makes drift
// "a finding plus an audit line"; if the audit line were a separate call,
// the pass would be correct and the record would be a caller's
// responsibility — and a caller who forgot would get a green report about
// drift nobody was ever told about.
//
// It carries no repair method, by construction: a sink can record a
// proposal, and there is nothing on this interface that could apply one.
type FindingSink interface {
	RecordFindings(ctx context.Context, passID string, findings []Finding) error
}

type ReconcileParams struct {
	Source            ReconcileSource
	PassID            string
	SnapshotTimestamp string
	// Sink is where the findings are recorded. It is required: a nil sink is
	// refused before any axis is read, so a pass cannot run to completion
	// with nowhere to put what it found.
	Sink FindingSink
}

// Reconcile runs one pass over the four axes.
//
// A source failure is a pass failure: the report is not produced, because a
// report that silently skipped an axis would be a green light for a check
// that never ran. The caller sees the error and the drill fails.
func Reconcile(ctx context.Context, p ReconcileParams) (*ReconcileReport, error) {
	if p.Sink == nil {
		return nil, fmt.Errorf("reconcile: no finding sink — a pass whose findings go only into its return value " +
			"reports drift to nobody (docs/37 §V1 验收: 漂移是 finding + audit row)")
	}
	rep := &ReconcileReport{
		PassID:            p.PassID,
		SnapshotTimestamp: p.SnapshotTimestamp,
		Checked:           map[Axis]int{},
		Verdict:           "clean",
	}

	branchRefs, err := p.Source.BranchRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read branch refs: %w", err)
	}
	blobs, err := p.Source.Blobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read blobs: %w", err)
	}
	releases, err := p.Source.Releases(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read releases: %w", err)
	}
	assetVersions, err := p.Source.AssetVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read asset versions: %w", err)
	}
	states, err := p.Source.States(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read states: %w", err)
	}

	// blobByHash indexes the canonical rows the pin axis resolves against.
	blobByHash := map[string]BlobRow{}
	for _, b := range blobs {
		blobByHash[b.ContentHash] = b
	}

	rep.Findings = append(rep.Findings, checkGitRefs(ctx, p.Source, branchRefs, &rep.Checked)...)
	rep.Findings = append(rep.Findings, checkBlobHashes(ctx, p.Source, blobs, &rep.Checked)...)
	rep.Findings = append(rep.Findings, checkReleaseManifests(releases, assetVersions, &rep.Checked)...)
	rep.Findings = append(rep.Findings, checkReleasePins(ctx, p.Source, releases, states, blobByHash, &rep.Checked)...)

	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Axis != b.Axis {
			return a.Axis < b.Axis
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	if len(rep.Findings) > 0 {
		rep.Verdict = "drift_detected"
	}
	// Recorded AFTER the verdict is settled and BEFORE the report is
	// returned: a caller that receives a pass has a pass whose findings are
	// already in the record. A clean pass records nothing, which is not the
	// same as skipping the call — the sink is still asked, so "zero rows"
	// is the sink's answer rather than an omission by this function.
	if err := p.Sink.RecordFindings(ctx, p.PassID, rep.Findings); err != nil {
		return nil, fmt.Errorf("reconcile: record findings: %w", err)
	}
	return rep, nil
}

// checkGitRefs is the "DB ↔ Git refs" axis: the recorded mapping must
// describe the restored repositories, in both directions.
func checkGitRefs(ctx context.Context, src ReconcileSource, refs []BranchRefRow, checked *map[Axis]int) []Finding {
	var findings []Finding
	// observed[repo][refname] records what the restored repository holds,
	// so the reverse check has something to compare after the forward one.
	observed := map[string]map[string]string{}
	recorded := map[string]map[string]bool{}

	for _, r := range refs {
		repoKey := r.Owner + "/" + r.Repo
		if r.Owner == "" || r.Repo == "" {
			findings = append(findings, Finding{
				Axis: AxisGitRefs, Kind: KindRepoUnmapped, ProjectID: r.ProjectID,
				Subject:  "ref:" + r.GitRef,
				Expected: "the project's repository is provisioned and resolvable",
				Actual:   "no repository coordinate for the project",
				RepairProposal: "re-provision the project's repository (the provisioning path is T0301) — " +
					"the drill does not do this",
			})
			continue
		}
		if _, ok := observed[repoKey]; !ok {
			remote, err := src.RemoteRefs(ctx, r.Owner, r.Repo)
			if err != nil {
				// A repository the target cannot answer about is a finding
				// of its own, not a pass-wide failure: the rest of the axis
				// still has something true to say.
				findings = append(findings, Finding{
					Axis: AxisGitRefs, Kind: KindRepoUnmapped, ProjectID: r.ProjectID,
					Subject:  "repo:" + repoKey,
					Expected: "the restored repository is readable",
					Actual:   "reading its refs failed: " + redactSecrets(err.Error()),
					RepairProposal: "restore the repository again (the mirror is in the backup under git/) — " +
						"the drill does not do this",
				})
				observed[repoKey] = map[string]string{}
				continue
			}
			observed[repoKey] = remote
			recorded[repoKey] = map[string]bool{}
		}
		if recorded[repoKey] == nil {
			recorded[repoKey] = map[string]bool{}
		}
		recorded[repoKey][r.GitRef] = true

		(*checked)[AxisGitRefs]++
		got, ok := observed[repoKey][r.GitRef]
		switch {
		case !ok:
			findings = append(findings, Finding{
				Axis: AxisGitRefs, Kind: KindRefMissing, ProjectID: r.ProjectID,
				Subject:  "ref:" + repoKey + ":" + r.GitRef,
				Expected: r.GitRef + " at " + r.HeadSHA + " (recorded head_sha)",
				Actual:   "the ref is not present in the restored repository",
				RepairProposal: "push the recorded head to " + r.GitRef + " from the backup mirror (git/" +
					r.Owner + "/" + r.Repo + ".git) — the drill does not do this",
			})
		case got != r.HeadSHA:
			findings = append(findings, Finding{
				Axis: AxisGitRefs, Kind: KindRefHeadMoved, ProjectID: r.ProjectID,
				Subject:  "ref:" + repoKey + ":" + r.GitRef,
				Expected: r.HeadSHA,
				Actual:   got,
				RepairProposal: "reconcile the mapping row to the restored head, or restore the recorded head — " +
					"which of the two is correct is a human's call; the drill does not apply either",
			})
		}
	}

	// The reverse direction: a ref the restored repository carries that no
	// mapping row records. The default ref is excluded — it is created by
	// provisioning and pinned by project_states rather than by a branch
	// row, so calling it unrecorded would be a finding the drill invented.
	for repoKey, refsOfRepo := range observed {
		owner, repo, _ := strings.Cut(repoKey, "/")
		def, err := src.DefaultRefs(ctx, owner, repo)
		if err != nil {
			findings = append(findings, Finding{
				Axis: AxisGitRefs, Kind: KindRepoUnmapped,
				Subject:        "repo:" + repoKey,
				Expected:       "the restored repository names a default ref",
				Actual:         "reading it failed: " + redactSecrets(err.Error()),
				RepairProposal: "restore the repository again — the drill does not do this",
			})
			continue
		}
		names := make([]string, 0, len(refsOfRepo))
		for name := range refsOfRepo {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == def {
				continue
			}
			if recorded[repoKey][name] {
				continue
			}
			findings = append(findings, Finding{
				Axis: AxisGitRefs, Kind: KindUnmappedRef,
				Subject:  "ref:" + repoKey + ":" + name,
				Expected: "every restored ref is named by a branch mapping row",
				Actual:   "the ref is at " + refsOfRepo[name] + " and no row records it",
				RepairProposal: "adopt the ref into the RSG (a semantic decision) or delete it — the drill " +
					"does neither",
			})
		}
	}
	return findings
}

// checkBlobHashes is the "DB ↔ blob hashes" axis: each recorded blob's
// bytes, read back out of the restored store, must digest to the recorded
// content hash.
//
// The comparison is content, never count: a store with the right NUMBER of
// objects and the wrong bytes passes a count check and fails this one.
//
// A blob this axis finds broken is reported here and only here — one broken
// byte range, one finding. It used to also return a per-key usability map
// for the pin axis to suppress a second row with; that suppression was a
// no-op (see checkReleasePins) and both the map and the guard went with it.
func checkBlobHashes(ctx context.Context, src ReconcileSource, blobs []BlobRow, checked *map[Axis]int) []Finding {
	var findings []Finding
	for _, b := range blobs {
		(*checked)[AxisBlobHashes]++
		raw, err := src.ReadObject(ctx, b.StorageKey)
		if err != nil {
			findings = append(findings, Finding{
				Axis: AxisBlobHashes, Kind: KindBlobMissing,
				Subject:  "blob:" + b.ID,
				Expected: "an object at storage key " + b.StorageKey,
				Actual:   "reading it failed: " + redactSecrets(err.Error()),
				RepairProposal: "re-upload the object from the backup's blobs/" + b.ContentHash +
					" — the drill does not do this",
			})
			continue
		}
		got := sha256Hex(raw)
		if got != b.ContentHash {
			findings = append(findings, Finding{
				Axis: AxisBlobHashes, Kind: KindBlobHashMismatch,
				Subject:  "blob:" + b.ID,
				Expected: b.ContentHash,
				Actual:   got,
				RepairProposal: "re-upload the object whose bytes digest to " + b.ContentHash +
					" (the backup holds them at blobs/" + b.ContentHash + ") — the drill does not do this",
			})
			continue
		}
		if int64(len(raw)) != b.SizeBytes {
			findings = append(findings, Finding{
				Axis: AxisBlobHashes, Kind: KindBlobSizeMismatch,
				Subject:  "blob:" + b.ID,
				Expected: fmt.Sprintf("%d bytes (blobs.size_bytes)", b.SizeBytes),
				Actual:   fmt.Sprintf("%d bytes", len(raw)),
				RepairProposal: "the row and the object disagree on size; re-derive one from the other — " +
					"the drill does not decide which is right",
			})
			continue
		}
	}
	return findings
}

// checkReleaseManifests is the "DB ↔ release manifests" axis: the stored
// document must still digest to the hash recorded beside it. The release
// package owns the digest rule, so the check is delegated to it — a second
// implementation here would be a second definition of what a release
// manifest hashes to.
func checkReleaseManifests(releases []ReleaseRow, assetVersions []AssetVersionRow, checked *map[Axis]int) []Finding {
	var findings []Finding
	for _, r := range releases {
		(*checked)[AxisReleaseManifests]++
		subject := "release:" + r.ID + "@" + r.Version
		if !releaseManifestVerifies(r.Manifest) {
			findings = append(findings, Finding{
				Axis: AxisReleaseManifests, Kind: KindReleaseUnverifiable, ProjectID: r.ProjectID,
				Subject:  subject,
				Expected: "the stored document's own manifest_hash covers it (releases.ReleaseManifest.VerifyHash)",
				Actual:   "it does not — the document and its embedded hash disagree",
				RepairProposal: "the release is immutable by design (docs/37 §需要备份, CLAUDE.md §9.5): restore the " +
					"release row from the backup rather than editing it — the drill does not do this",
			})
			continue
		}
		if got := embeddedManifestHash(r.Manifest); got != r.ManifestHash {
			findings = append(findings, Finding{
				Axis: AxisReleaseManifests, Kind: KindReleaseRowMismatch, ProjectID: r.ProjectID,
				Subject:        subject,
				Expected:       r.ManifestHash + " (releases.manifest_hash)",
				Actual:         got + " (the digest inside the stored document)",
				RepairProposal: "restore the release row from the backup — the drill does not do this",
			})
		}
	}
	for _, a := range assetVersions {
		(*checked)[AxisReleaseManifests]++
		got, err := assetManifestHash(a.Manifest)
		if err != nil {
			findings = append(findings, Finding{
				Axis: AxisReleaseManifests, Kind: KindAssetManifestMismatch,
				Subject:        "asset_version:" + a.ID,
				Expected:       "a parseable manifest document",
				Actual:         err.Error(),
				RepairProposal: "restore the asset version row from the backup — the drill does not do this",
			})
			continue
		}
		if got != a.IntegrityHash {
			findings = append(findings, Finding{
				Axis: AxisReleaseManifests, Kind: KindAssetManifestMismatch,
				Subject:        "asset_version:" + a.ID,
				Expected:       a.IntegrityHash + " (research_asset_versions.integrity_hash)",
				Actual:         got + " (the digest of the stored manifest document)",
				RepairProposal: "restore the asset version row from the backup — the drill does not do this",
			})
		}
	}
	return findings
}

// statePin is the subset of a release manifest's embedded state content
// the pin axis reads. Declared locally rather than by importing the state
// manifest model: what the axis compares is the CONTENT's own claims, and
// parsing them into the full model would make this check depend on that
// model's evolution rather than on the bytes the release pinned.
type statePin struct {
	GitRef   *string   `json:"git_ref"`
	BlobRefs []blobPin `json:"blob_refs"`
}

type blobPin struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// checkReleasePins is the cross axis: what the release manifests PIN must
// resolve against the restored Git repositories and the restored object
// store. It is the axis the document's arrows make the whole point of the
// exercise — a release whose pinned commit is not in the restored
// repository, or whose pinned blob has no row in the restored store, is a
// release that restored to something other than what it recorded. (A pinned
// blob whose row exists but whose bytes are wrong is the blob_hashes axis's
// finding, not this one's — see the loop below.)
func checkReleasePins(ctx context.Context, src ReconcileSource, releases []ReleaseRow, states map[string]StateRow,
	blobByHash map[string]BlobRow, checked *map[Axis]int) []Finding {
	var findings []Finding
	for _, r := range releases {
		pin, ok := parseStatePin(r.Manifest)
		if !ok {
			// A document that does not parse is already a
			// release_manifests finding; reporting it again here would
			// double-count one defect.
			continue
		}
		st, hasState := states[r.StateID]
		if !hasState {
			findings = append(findings, Finding{
				Axis: AxisReleasePins, Kind: KindReleaseStateMissing, ProjectID: r.ProjectID,
				Subject:        "release:" + r.ID,
				Expected:       "project state " + r.StateID + " present in the restored database",
				Actual:         "no such state row",
				RepairProposal: "restore the state row from the backup — the drill does not do this",
			})
			continue
		}
		(*checked)[AxisReleasePins]++
		// The manifest's pinned state hash must be the row's.
		if h := embeddedStateHash(r.Manifest); h != "" && h != st.StateHash {
			findings = append(findings, Finding{
				Axis: AxisReleasePins, Kind: KindReleaseStateMismatch, ProjectID: r.ProjectID,
				Subject:  "release:" + r.ID,
				Expected: h + " (the hash the release manifest pinned)",
				Actual:   st.StateHash + " (project_states.state_hash)",
				RepairProposal: "the release is an immutable snapshot: restore the state row it pinned — " +
					"the drill does not do this",
			})
		}
		// The state's pinned commit must exist in the restored repository.
		if st.GitCommitSHA != nil && *st.GitCommitSHA != "" {
			repo, ok := repoForProject(ctx, src, r.ProjectID)
			if !ok {
				findings = append(findings, Finding{
					Axis: AxisReleasePins, Kind: KindReleaseGitPinMissing, ProjectID: r.ProjectID,
					Subject:        "release:" + r.ID,
					Expected:       "the project's repository is resolvable to check commit " + *st.GitCommitSHA,
					Actual:         "no repository coordinate for the project",
					RepairProposal: "re-provision the repository — the drill does not do this",
				})
			} else {
				found, err := src.HasCommit(ctx, repo.Owner, repo.Repo, *st.GitCommitSHA)
				switch {
				case err != nil:
					findings = append(findings, Finding{
						Axis: AxisReleasePins, Kind: KindReleaseGitPinMissing, ProjectID: r.ProjectID,
						Subject:        "release:" + r.ID,
						Expected:       "commit " + *st.GitCommitSHA + " present in " + repo.Owner + "/" + repo.Repo,
						Actual:         "reading the repository failed: " + redactSecrets(err.Error()),
						RepairProposal: "restore the repository from the backup mirror — the drill does not do this",
					})
				case !found:
					findings = append(findings, Finding{
						Axis: AxisReleasePins, Kind: KindReleaseGitPinMissing, ProjectID: r.ProjectID,
						Subject:  "release:" + r.ID,
						Expected: "commit " + *st.GitCommitSHA + " present in " + repo.Owner + "/" + repo.Repo,
						Actual:   "the commit is not an object of the restored repository",
						RepairProposal: "restore the repository from the backup mirror (git/" + repo.Owner + "/" +
							repo.Repo + ".git) — the drill does not do this",
					})
				}
			}
		}
		// Every blob the manifest pins must resolve to a restored row. The
		// lookup key IS the pinned hash, so finding the row already
		// establishes blobs.content_hash == bp.Hash; whether the bytes behind
		// that row still digest to it is the blob_hashes axis's question
		// (checkBlobHashes digests every restored object), so this loop
		// reports exactly one thing of its own — a pinned hash with no row at
		// all. One broken byte range stays one finding: the blob axis's is the
		// one that is filed, and TestReconcileDetectsTamperedBlobBytes asserts
		// the total is one rather than trusting that nothing re-reports it.
		for _, bp := range pin.BlobRefs {
			if _, has := blobByHash[bp.Hash]; !has {
				findings = append(findings, Finding{
					Axis: AxisReleasePins, Kind: KindReleaseBlobMissing, ProjectID: r.ProjectID,
					Subject:  "release:" + r.ID + ":blob:" + bp.ID,
					Expected: "a blobs row whose content_hash is " + bp.Hash + " (pinned by the release manifest)",
					Actual:   "no such row in the restored database",
					RepairProposal: "restore the blob row and its object from the backup (blobs/" + bp.Hash +
						") — the drill does not do this",
				})
				continue
			}
		}
	}
	return findings
}

// releaseManifestVerifies delegates to the release package: its VerifyHash
// is the one definition of what a release manifest hashes to, and the
// product's own manifest export refuses a document that fails it
// (releases.Command.Manifest).
func releaseManifestVerifies(raw []byte) bool {
	m, ok := parseReleaseManifest(raw)
	if !ok {
		return false
	}
	return m.VerifyHash()
}

// embeddedManifestHash reads the manifest_hash field inside a stored release
// document.
func embeddedManifestHash(raw []byte) string {
	var doc struct {
		ManifestHash string `json:"manifest_hash"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.ManifestHash
}

// embeddedStateHash reads the pinned state hash out of a stored release
// document.
func embeddedStateHash(raw []byte) string {
	var doc struct {
		State struct {
			StateHash string `json:"state_hash"`
		} `json:"state"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.State.StateHash
}

// parseStatePin reads the state content a release manifest pinned.
func parseStatePin(raw []byte) (statePin, bool) {
	var doc struct {
		State struct {
			Content json.RawMessage `json:"content"`
		} `json:"state"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return statePin{}, false
	}
	if len(doc.State.Content) == 0 {
		return statePin{}, false
	}
	var pin statePin
	if err := json.Unmarshal(doc.State.Content, &pin); err != nil {
		return statePin{}, false
	}
	return pin, true
}

// repoRef is one project's repository coordinate.
type repoRef struct{ Owner, Repo string }

// repoForProject resolves a project's repository through the source's own
// branch rows. A separate query would be a second definition of the mapping
// the git_refs axis already reads.
func repoForProject(ctx context.Context, src ReconcileSource, projectID string) (repoRef, bool) {
	refs, err := src.BranchRefs(ctx)
	if err != nil {
		return repoRef{}, false
	}
	for _, r := range refs {
		if r.ProjectID == projectID && r.Owner != "" && r.Repo != "" {
			return repoRef{Owner: r.Owner, Repo: r.Repo}, true
		}
	}
	return repoRef{}, false
}
