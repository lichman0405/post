package backupdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/releases"
	rsgmanifest "github.com/lichman0405/post/internal/rsg/manifest"
)

// The reconciliation's own rules, tested without any infrastructure, so a
// failure here names the rule that broke rather than a service that was
// down.

// ---------------------------------------------------------------------------
// The fake source: a consistent environment, and the ability to break it

// fakeSource is an in-memory restored environment. It exists so the drift
// classes can be produced ONE AT A TIME and the finding for each can be
// asserted by name — against real infrastructure a single injected fault
// fans out into several findings, and "some findings appeared" would not
// tell a reader which check fired.
type fakeSource struct {
	branchRefs []BranchRefRow
	blobs      []BlobRow
	objects    map[string][]byte // storage key → bytes
	releases   []ReleaseRow
	assets     []AssetVersionRow
	states     map[string]StateRow
	remoteRefs map[string]map[string]string // "owner/repo" → ref → sha
	defaultRef map[string]string
	commits    map[string]bool // "owner/repo@sha"
}

// readCount counts the object reads, so a test can prove the reconciler
// actually read the bytes rather than trusting a size or an ETag.
func (f *fakeSource) ReadObject(_ context.Context, key string) ([]byte, error) {
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake: no object at %s", key)
	}
	return body, nil
}

func (f *fakeSource) BranchRefs(context.Context) ([]BranchRefRow, error) { return f.branchRefs, nil }
func (f *fakeSource) Blobs(context.Context) ([]BlobRow, error)           { return f.blobs, nil }
func (f *fakeSource) Releases(context.Context) ([]ReleaseRow, error)     { return f.releases, nil }
func (f *fakeSource) AssetVersions(context.Context) ([]AssetVersionRow, error) {
	return f.assets, nil
}
func (f *fakeSource) States(context.Context) (map[string]StateRow, error) { return f.states, nil }
func (f *fakeSource) RemoteRefs(_ context.Context, owner, repo string) (map[string]string, error) {
	refs, ok := f.remoteRefs[owner+"/"+repo]
	if !ok {
		return nil, fmt.Errorf("fake: no repository %s/%s", owner, repo)
	}
	return refs, nil
}
func (f *fakeSource) DefaultRefs(_ context.Context, owner, repo string) (string, error) {
	ref, ok := f.defaultRef[owner+"/"+repo]
	if !ok {
		return "", fmt.Errorf("fake: no default ref for %s/%s", owner, repo)
	}
	return ref, nil
}
func (f *fakeSource) HasCommit(_ context.Context, owner, repo, sha string) (bool, error) {
	return f.commits[owner+"/"+repo+"@"+sha], nil
}

const (
	fakeProjectID = "0f8c1a2e-0000-4000-8000-000000000001"
	fakeStateID   = "0f8c1a2e-0000-4000-8000-000000000002"
	fakeReleaseID = "0f8c1a2e-0000-4000-8000-000000000003"
	fakeAssetID   = "0f8c1a2e-0000-4000-8000-000000000004"
	fakeBlobID    = "0f8c1a2e-0000-4000-8000-000000000005"
	fakeOwner     = "drill-src"
	fakeRepo      = "drill-project"
	fakeStorage   = "blobs/" + fakeBlobID
	fakeCommit    = "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee"
	fakeStateHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// newFakeSource builds an environment in which every axis agrees. Each test
// then breaks exactly one thing.
// recordingSink is the unit tests' FindingSink: it keeps what a pass
// recorded so a test can compare it with what the pass returned.
type recordingSink struct {
	passID   string
	findings []Finding
	calls    int
	failWith error
}

func (r *recordingSink) RecordFindings(_ context.Context, passID string, findings []Finding) error {
	r.calls++
	r.passID = passID
	r.findings = append([]Finding(nil), findings...)
	return r.failWith
}

func newRecordingSink() *recordingSink { return &recordingSink{} }

func newFakeSource(t *testing.T) (*fakeSource, []byte) {
	t.Helper()
	blobBytes := []byte("the pinned blob's bytes, exactly as published")
	blobHash := sha256Hex(blobBytes)

	manifest := newFakeReleaseManifest(t, blobHash)

	return &fakeSource{
		branchRefs: []BranchRefRow{{
			ProjectID: fakeProjectID, Owner: fakeOwner, Repo: fakeRepo,
			GitRef: "refs/heads/main", HeadSHA: fakeCommit, SyncState: "synced",
		}},
		blobs: []BlobRow{{
			ID: fakeBlobID, ContentHash: blobHash, SizeBytes: int64(len(blobBytes)), StorageKey: fakeStorage,
		}},
		objects: map[string][]byte{fakeStorage: blobBytes},
		releases: []ReleaseRow{{
			ID: fakeReleaseID, ProjectID: fakeProjectID, Version: "1.0.0",
			StateID: fakeStateID, Manifest: manifest.Raw, ManifestHash: manifest.Hash,
		}},
		assets: []AssetVersionRow{},
		states: map[string]StateRow{
			fakeStateID: {ProjectID: fakeProjectID, StateHash: fakeStateHash, GitCommitSHA: strptr(fakeCommit)},
		},
		remoteRefs: map[string]map[string]string{
			fakeOwner + "/" + fakeRepo: {"refs/heads/main": fakeCommit},
		},
		defaultRef: map[string]string{fakeOwner + "/" + fakeRepo: "refs/heads/main"},
		commits:    map[string]bool{fakeOwner + "/" + fakeRepo + "@" + fakeCommit: true},
	}, blobBytes
}

type fakeManifest struct {
	Raw  []byte
	Hash string
}

// newFakeReleaseManifest builds a release document the product's own export
// path would accept: the fields through releases.ReleaseManifest, the
// digest through the product's own ContentCanonicalJSON, and the state pin
// embedded exactly as a real release embeds it. Nothing here reimplements a
// hash rule — that is the point of delegating to releases.
func newFakeReleaseManifest(t *testing.T, pinnedBlobHash string) fakeManifest {
	t.Helper()
	stateContent, err := json.Marshal(map[string]any{
		"git_ref": fakeCommit,
		"blob_refs": []map[string]string{
			{"id": fakeBlobID, "hash": pinnedBlobHash},
		},
	})
	if err != nil {
		t.Fatalf("marshal state content: %v", err)
	}
	m := releases.ReleaseManifest{
		FormatVersion: releases.FormatV1,
		ProjectID:     fakeProjectID,
		StateID:       fakeStateID,
		Version:       "1.0.0",
		State: releases.StatePin{
			StateHash: fakeStateHash,
			Content:   json.RawMessage(stateContent),
		},
		Schemas: []releases.SchemaPin{},
		Reviews: []releases.ReviewRecord{},
	}
	hashInput, err := m.ContentCanonicalJSON()
	if err != nil {
		t.Fatalf("canonical content: %v", err)
	}
	m.ManifestHash = rsgmanifest.Digest(hashInput)
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical document: %v", err)
	}
	return fakeManifest{Raw: raw, Hash: m.ManifestHash}
}

func strptr(s string) *string { return &s }

func reconcileFake(t *testing.T, src *fakeSource) *ReconcileReport {
	t.Helper()
	sink := newRecordingSink()
	rep, err := Reconcile(context.Background(), ReconcileParams{
		Source: src, PassID: "unit-pass", SnapshotTimestamp: "2026-09-16T00:00:00Z", Sink: sink,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return rep
}

// ---------------------------------------------------------------------------
// The control: the reconciler must be able to find drift at all

// TestReconcileCleanOnAConsistentEnvironment is the baseline the drift
// tests are read against. A reconciler that reported findings on a
// consistent environment would make every drift assertion below
// worthless, because "a finding appeared" would be true either way.
func TestReconcileCleanOnAConsistentEnvironment(t *testing.T) {
	src, _ := newFakeSource(t)
	rep := reconcileFake(t, src)
	if !rep.Clean() {
		t.Fatalf("a consistent environment produced %d findings: %v", len(rep.Findings), rep.Findings)
	}
	if rep.Verdict != "clean" {
		t.Fatalf("verdict = %q, want clean", rep.Verdict)
	}
	// Every axis must have been CHECKED. A clean verdict from a pass that
	// looked at nothing is the failure mode this asserts against.
	for _, axis := range AllAxes {
		if rep.Checked[axis] == 0 {
			t.Fatalf("axis %s was not checked at all (checked=%v)", axis, rep.Checked)
		}
	}
}

// TestReconcileDetectsTamperedBlobBytes is the acceptance criterion
// "故意破坏一样东西，reconciliation 必须报出来". The bytes are replaced with
// different bytes of the SAME length, so a size comparison — the cheaper
// check the drill deliberately does not rely on — would still pass.
func TestReconcileDetectsTamperedBlobBytes(t *testing.T) {
	src, original := newFakeSource(t)

	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[0] ^= 0xff
	src.objects[fakeStorage] = tampered

	rep := reconcileFake(t, src)
	if rep.Clean() {
		t.Fatal("replacing a restored blob's bytes produced no finding — the reconciler cannot report drift")
	}

	var kinds []string
	for _, f := range rep.Findings {
		kinds = append(kinds, f.Kind)
	}
	// The broken bytes ARE a pinned blob's: newFakeReleaseManifest pins
	// exactly the blob this test tampers with, so the release-pin axis has
	// this blob in view too. One broken byte range is one finding — the
	// blob_hashes axis files it and the pin axis does not file a second row
	// about it. A guard used to sit in the pin loop claiming to ensure that;
	// it ensured nothing (its whole body was a `continue` as the last
	// statement of the loop body), so the property is asserted here instead
	// of being assumed from a comment. A pass that reports this twice, or
	// not at all, fails HERE.
	if len(rep.Findings) != 1 {
		t.Fatalf("the tampered pinned blob produced %d findings (%v), want exactly 1 — the blob_hashes "+
			"axis's, not one per axis that can see the blob", len(rep.Findings), kinds)
	}
	want := KindBlobHashMismatch
	if !contains(kinds, want) {
		t.Fatalf("findings %v do not include %s", kinds, want)
	}
	for _, f := range rep.Findings {
		if f.Kind != want {
			continue
		}
		if f.Expected != sha256Hex(original) {
			t.Fatalf("finding expected %q, want the recorded content_hash %q", f.Expected, sha256Hex(original))
		}
		if f.Actual != sha256Hex(tampered) {
			t.Fatalf("finding actual %q, want the digest of the restored bytes %q", f.Actual, sha256Hex(tampered))
		}
		if f.RepairProposal == "" {
			t.Fatal("the finding carries no repair proposal — a reader has been told what is wrong and not what could be done")
		}
	}
}

// TestReconcileNeverRepairs is the "no automatic repair path" pin, at the
// unit level: after a pass that found drift, every surface the finding is
// about is byte-for-byte what it was before the pass. The reconciler has
// no writer to call — ReconcileSource declares none — and this asserts the
// consequence rather than the construction.
func TestReconcileNeverRepairs(t *testing.T) {
	src, original := newFakeSource(t)
	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0xff
	src.objects[fakeStorage] = tampered
	src.remoteRefs[fakeOwner+"/"+fakeRepo]["refs/heads/main"] = "0000000000000000000000000000000000000000"
	src.states[fakeStateID] = StateRow{ProjectID: fakeProjectID, StateHash: "sha256:wrong", GitCommitSHA: strptr(fakeCommit)}

	before := snapshotFake(src)
	rep := reconcileFake(t, src)
	if rep.Clean() {
		t.Fatal("three broken surfaces produced no findings")
	}
	after := snapshotFake(src)
	if before != after {
		t.Fatalf("the reconciler CHANGED the environment it was checking:\nbefore %s\nafter  %s", before, after)
	}
}

// snapshotFake renders every mutable surface of the fake source, so
// TestReconcileNeverRepairs compares whole values rather than the fields a
// writer would most plausibly have touched.
func snapshotFake(f *fakeSource) string {
	var sb strings.Builder
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "object %s %s\n", k, sha256Hex(f.objects[k]))
	}
	for _, b := range f.blobs {
		fmt.Fprintf(&sb, "blobrow %s %s %d\n", b.ID, b.ContentHash, b.SizeBytes)
	}
	for _, r := range f.releases {
		fmt.Fprintf(&sb, "release %s %s\n", r.ID, r.ManifestHash)
	}
	for _, r := range f.branchRefs {
		fmt.Fprintf(&sb, "ref %s %s\n", r.GitRef, r.HeadSHA)
	}
	for name, st := range f.states {
		fmt.Fprintf(&sb, "state %s %s %v\n", name, st.StateHash, st.GitCommitSHA)
	}
	repos := make([]string, 0, len(f.remoteRefs))
	for k := range f.remoteRefs {
		repos = append(repos, k)
	}
	sort.Strings(repos)
	for _, k := range repos {
		names := make([]string, 0, len(f.remoteRefs[k]))
		for n := range f.remoteRefs[k] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&sb, "remote %s %s %s\n", k, n, f.remoteRefs[k][n])
		}
	}
	return sb.String()
}

// TestReconcileFindsAMissingRefAndAnUnrecordedRef: the git_refs axis runs
// in both directions. A ref the mapping records but the repository lacks,
// and a ref the repository carries that no row records, are different
// findings — treating the second as silence would let a restore that
// pushed everything plus a stray ref look clean.
func TestReconcileFindsAMissingRefAndAnUnrecordedRef(t *testing.T) {
	src, _ := newFakeSource(t)
	src.remoteRefs[fakeOwner+"/"+fakeRepo] = map[string]string{
		"refs/heads/feature": fakeCommit,
	}
	rep := reconcileFake(t, src)

	kinds := findingKinds(rep)
	if !contains(kinds, KindRefMissing) {
		t.Fatalf("findings %v do not include %s", kinds, KindRefMissing)
	}
	if !contains(kinds, KindUnmappedRef) {
		t.Fatalf("findings %v do not include %s", kinds, KindUnmappedRef)
	}
}

// TestReconcileTreatsTheDefaultRefAsMapped: the reverse check must exclude
// the repository's default ref, which provisioning creates and no branch
// row maps. Without the exclusion every restore would report one finding
// that is not drift — and a check that always fires is a check nobody
// reads.
func TestReconcileTreatsTheDefaultRefAsMapped(t *testing.T) {
	src, _ := newFakeSource(t)
	rep := reconcileFake(t, src)
	if !rep.Clean() {
		t.Fatalf("the default ref was reported as unrecorded: %v", rep.Findings)
	}
}

// TestReconcileFindsAReleasePinThatDoesNotResolve: the cross axis. The
// release manifest pins a blob hash that no blobs row carries — the state
// the four-way chain exists to catch, and one that the git_refs and
// blob_hashes axes both pass cleanly on their own.
func TestReconcileFindsAReleasePinThatDoesNotResolve(t *testing.T) {
	src, _ := newFakeSource(t)
	src.blobs = nil // the row is gone; the object is still in the store
	rep := reconcileFake(t, src)
	if !contains(findingKinds(rep), KindReleaseBlobMissing) {
		t.Fatalf("findings %v do not include %s", findingKinds(rep), KindReleaseBlobMissing)
	}
}

// TestReconcileFindsAnUnverifiableReleaseDocument: a release whose stored
// document no longer digests to its manifest_hash. The axis must notice
// WITHOUT the export path being involved — the export refuses such a
// release, and a reconciliation that only ran through the export would
// report the refusal as an error rather than as drift.
func TestReconcileFindsAnUnverifiableReleaseDocument(t *testing.T) {
	src, _ := newFakeSource(t)
	broken := append([]byte(nil), src.releases[0].Manifest...)
	var doc map[string]any
	if err := json.Unmarshal(broken, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["version"] = "9.9.9"
	rebroken, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	src.releases[0].Manifest = rebroken

	rep := reconcileFake(t, src)
	if !contains(findingKinds(rep), KindReleaseUnverifiable) {
		t.Fatalf("findings %v do not include %s", findingKinds(rep), KindReleaseUnverifiable)
	}
}

// TestReconcileFailsThePassWhenAnAxisCannotBeRead: a source read failure is
// an error, not a clean axis. Reporting "clean" for an axis whose rows
// could not be read would be the single worst outcome this package could
// produce — a green light for a check that never ran.
func TestReconcileFailsThePassWhenAnAxisCannotBeRead(t *testing.T) {
	src, _ := newFakeSource(t)
	src.objects = map[string][]byte{} // the row is there; the object read fails
	rep, err := Reconcile(context.Background(), ReconcileParams{Source: src, PassID: "p", Sink: newRecordingSink()})
	if err != nil {
		t.Fatalf("an unreadable object should be a finding on its axis, not a pass failure: %v", err)
	}
	if !contains(findingKinds(rep), KindBlobMissing) {
		t.Fatalf("findings %v do not include %s", findingKinds(rep), KindBlobMissing)
	}

	// A source whose ROW read fails is the pass failure.
	if _, err := Reconcile(context.Background(), ReconcileParams{Source: &brokenRowSource{}, Sink: newRecordingSink()}); err == nil {
		t.Fatal("a failed row read produced a report instead of an error")
	}
}

type brokenRowSource struct{ fakeSource }

func (*brokenRowSource) BranchRefs(context.Context) ([]BranchRefRow, error) {
	return nil, fmt.Errorf("database is down")
}

func findingKinds(rep *ReconcileReport) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.Kind)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// "The reconciler NEVER repairs", structurally

// TestReconcileSourceDeclaresNoWriteMethod pins the report-only rule at the
// only place it can be pinned without trusting a promise: the interface.
//
// The companion half is as important as the assertion. A test that only
// checked "no write method on the interface" would also pass if the
// package had no way to write anything at all, which would make it a test
// about a vacuum. So it also asserts that the drill's real collaborators DO
// carry the write operations a repair would need — proving the restriction
// is a choice made at the interface, not an absence of capability.
func TestReconcileSourceDeclaresNoWriteMethod(t *testing.T) {
	iface := reflect.TypeOf((*ReconcileSource)(nil)).Elem()
	if iface.NumMethod() == 0 {
		t.Fatal("the interface has no methods — the assertion below would be vacuous")
	}
	for i := 0; i < iface.NumMethod(); i++ {
		name := strings.ToLower(iface.Method(i).Name)
		for _, bad := range writeishPrefixes {
			if strings.HasPrefix(name, bad) {
				t.Fatalf("ReconcileSource declares %s — a repair could be written through the reconciler's own surface",
					iface.Method(i).Name)
			}
		}
	}

	// The control: the capability exists, one layer down, and none of it is
	// reachable through the reconciler's interface. Without this half the
	// assertion above would also pass in a package that could not write
	// anything at all — a test about a vacuum.
	for _, capa := range []struct {
		name string
		typ  reflect.Type
	}{
		{"object store", reflect.TypeOf((*objectWriter)(nil)).Elem()},
		{"git provider", reflect.TypeOf((*repoWriter)(nil)).Elem()},
	} {
		if capa.typ.NumMethod() == 0 {
			t.Fatalf("%s declares no write method — the control is vacuous", capa.name)
		}
		for i := 0; i < capa.typ.NumMethod(); i++ {
			name := capa.typ.Method(i).Name
			if _, reachable := iface.MethodByName(name); reachable {
				t.Fatalf("ReconcileSource exposes %s.%s — a repair could be written through it", capa.name, name)
			}
			lower := strings.ToLower(name)
			writeish := false
			for _, bad := range writeishPrefixes {
				if strings.HasPrefix(lower, bad) {
					writeish = true
				}
			}
			if !writeish {
				t.Fatalf("%s.%s does not read as a write — the control no longer proves a write capability exists",
					capa.name, name)
			}
		}
	}
}

var writeishPrefixes = []string{"put", "write", "create", "delete", "insert", "update", "repair", "apply", "set", "push", "revoke", "mint"}

// objectWriter and repoWriter are the write capabilities the drill's
// collaborators actually have. The compile-time assertions below are what
// makes TestReconcileSourceDeclaresNoWriteMethod's control non-vacuous:
// delete a write method from s3Client or gitClient and the test file stops
// compiling, so the control cannot silently become a check of nothing.
type objectWriter interface {
	PutObject(ctx context.Context, bucket, key string, body []byte) error
	DeleteObject(ctx context.Context, bucket, key string) error
	CreateBucket(ctx context.Context, bucket string) error
}

type repoWriter interface {
	createRepo(ctx context.Context, org, repo string) error
	deleteRepo(ctx context.Context, org, repo string) error
	deleteOrg(ctx context.Context, org string) error
	pushMirror(ctx context.Context, mirror, owner, repo string) error
	createOrg(ctx context.Context, org string) error
}

var (
	_ objectWriter = (*s3Client)(nil)
	_ repoWriter   = (*gitClient)(nil)
)

// TestDrillSourceSatisfiesTheReadOnlyPort is the compile-time half of the
// same rule: the concrete source the drill hands the reconciler is a
// ReconcileSource and nothing wider is handed over.
func TestDrillSourceSatisfiesTheReadOnlyPort(t *testing.T) {
	var _ ReconcileSource = (*drillSource)(nil)
}

// ---------------------------------------------------------------------------
// The sink contract: drift is a finding AND an audit line, and the pass is
// what puts it in the record.

// TestReconcileRefusesAPassWithNoSink pins the requirement rather than the
// courtesy: a pass with nowhere to record its findings does not run at all.
// The alternative — run, return the findings, hope the caller writes them —
// is the failure mode this interface exists to make impossible, and it is
// invisible precisely because the pass itself looks correct.
func TestReconcileRefusesAPassWithNoSink(t *testing.T) {
	src, _ := newFakeSource(t)
	snapshotFake(src)
	if _, err := Reconcile(context.Background(), ReconcileParams{Source: src, PassID: "p"}); err == nil {
		t.Fatal("a pass with no sink ran and returned a report")
	}
}

// TestReconcileRecordsExactlyWhatItReports: the findings a caller sees and
// the findings that were recorded are the same findings. A pass that
// reported more than it recorded would be drift in a report and nowhere
// else; one that recorded more than it reported would be a finding nobody
// can act on.
func TestReconcileRecordsExactlyWhatItReports(t *testing.T) {
	src, original := newFakeSource(t)
	// Break one thing so the pass has something to report.
	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[0] ^= 0xff
	src.objects[fakeStorage] = tampered

	sink := newRecordingSink()
	rep, err := Reconcile(context.Background(), ReconcileParams{
		Source: src, PassID: "pass-7", SnapshotTimestamp: "2026-09-16T00:00:00Z", Sink: sink,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Clean() {
		t.Fatal("the fixture was broken on purpose and the pass came back clean")
	}
	if sink.calls != 1 {
		t.Fatalf("the sink was called %d times, want exactly 1 per pass", sink.calls)
	}
	if sink.passID != "pass-7" {
		t.Fatalf("the sink recorded pass id %q, want the pass's own %q", sink.passID, "pass-7")
	}
	if len(sink.findings) != len(rep.Findings) {
		t.Fatalf("recorded %d findings and reported %d", len(sink.findings), len(rep.Findings))
	}
	for i := range sink.findings {
		if sink.findings[i] != rep.Findings[i] {
			t.Fatalf("finding %d differs: recorded %+v, reported %+v", i, sink.findings[i], rep.Findings[i])
		}
	}
}

// TestReconcileAsksTheSinkEvenWhenClean: zero recorded rows on a clean pass
// is the SINK's answer (it was asked and had nothing to write), not an
// omission by the pass. The distinction matters when someone later asks
// "did the pass that found nothing run?" — the record has to be able to say
// that it was asked.
func TestReconcileAsksTheSinkEvenWhenClean(t *testing.T) {
	src, _ := newFakeSource(t)
	snapshotFake(src)
	sink := newRecordingSink()
	rep, err := Reconcile(context.Background(), ReconcileParams{Source: src, PassID: "clean-pass", Sink: sink})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("the fixture is consistent but the pass reported %v", rep.Findings)
	}
	if sink.calls != 1 {
		t.Fatalf("a clean pass called the sink %d times, want 1", sink.calls)
	}
	if len(sink.findings) != 0 {
		t.Fatalf("a clean pass recorded %v", sink.findings)
	}
}

// TestReconcileFailsWhenTheRecordCannotBeWritten: a finding that could not
// be recorded is a failed pass, not a successful pass with a caveat. The
// returned error is the difference between "drift, and it is in the record"
// and "drift, and the record is incomplete".
func TestReconcileFailsWhenTheRecordCannotBeWritten(t *testing.T) {
	src, original := newFakeSource(t)
	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[0] ^= 0xff
	src.objects[fakeStorage] = tampered
	sink := newRecordingSink()
	sink.failWith = errors.New("audit_log is unreachable")
	if _, err := Reconcile(context.Background(), ReconcileParams{Source: src, PassID: "p", Sink: sink}); err == nil {
		t.Fatal("the sink refused the findings and the pass still returned a report")
	}
}

// TestFindingSinkDeclaresNoRepair method is the structural half of
// "report, never repair": a sink can record a proposal, and there is no
// method on the interface through which one could be applied. A future
// method named Repair/Apply/Fix would fail here, which is the point — this
// is the rule that has to be re-argued rather than the one that gets added
// quietly.
func TestFindingSinkDeclaresNoRepair(t *testing.T) {
	var sink FindingSink
	typ := reflect.TypeOf(&sink).Elem()
	if typ.NumMethod() != 1 || typ.Method(0).Name != "RecordFindings" {
		var names []string
		for i := 0; i < typ.NumMethod(); i++ {
			names = append(names, typ.Method(i).Name)
		}
		t.Fatalf("FindingSink declares %v; the contract is exactly RecordFindings", names)
	}
	// And the name itself must not be a write verb: "Record" appends to a
	// log, "Repair" would change the environment.
	for _, banned := range []string{"repair", "apply", "fix", "heal", "restore", "put", "write", "delete"} {
		if strings.Contains(strings.ToLower(typ.Method(0).Name), banned) {
			t.Fatalf("FindingSink's method is named %q, which reads as a repair path", typ.Method(0).Name)
		}
	}
}
