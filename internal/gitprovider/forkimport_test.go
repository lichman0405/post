package gitprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The fork import's own test (T0804). It runs against fakes for the provider
// and the canonical store, and against the REAL ingester (testIngester) —
// the point of the import is that it feeds the same inspection a push feeds,
// so the test must not stub the inspection out.

const (
	sourceProject = "11111111-1111-4111-8111-111111111111"
	targetProject = "22222222-2222-4222-8222-222222222222"
	sourceHead    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baselineSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	targetOldHead = "cccccccccccccccccccccccccccccccccccccccc"
	// forkActor is the user the import is made for — the forker. A
	// transition names an actor, so the import has no shape without one.
	forkActor = "33333333-3333-4333-8333-333333333333"
)

// fakeForkImportStore answers the two reads the importer makes: the
// provision rows and the source line's recorded fork point.
type fakeForkImportStore struct {
	repos     map[string]gitprovider.ForkRepoRef
	repoErr   error
	forkPoint map[string]string
	pointErr  error
	pointRefs []string
}

func (f *fakeForkImportStore) ForkRepo(_ context.Context, projectID string) (gitprovider.ForkRepoRef, error) {
	if f.repoErr != nil {
		return gitprovider.ForkRepoRef{}, f.repoErr
	}
	ref, ok := f.repos[projectID]
	if !ok {
		return gitprovider.ForkRepoRef{}, gitprovider.ErrRepoNotProvisioned
	}
	return ref, nil
}

func (f *fakeForkImportStore) SourceForkPoint(_ context.Context, projectID, gitRef string) (string, error) {
	f.pointRefs = append(f.pointRefs, projectID+":"+gitRef)
	if f.pointErr != nil {
		return "", f.pointErr
	}
	return f.forkPoint[gitRef], nil
}

func forkImportStore() *fakeForkImportStore {
	return &fakeForkImportStore{
		repos: map[string]gitprovider.ForkRepoRef{
			sourceProject: {ProjectID: sourceProject, Owner: "post-git-svc", Name: "p-source", RepoID: 41},
			targetProject: {ProjectID: targetProject, Owner: "post-git-svc", Name: "p-fork", RepoID: 42},
		},
		forkPoint: map[string]string{},
	}
}

// callLog is a cross-object order log: the import's contract is the ORDER
// of its steps (inspect → record → copy), and only a shared log can show
// the interleaving of two collaborators' calls.
type callLog struct{ entries []string }

func (l *callLog) add(entry string) { l.entries = append(l.entries, entry) }

type loggedPort struct {
	*fakePort
	log *callLog
}

func (p *loggedPort) ChangedFiles(ctx context.Context, repo gitprovider.Repository, base, head string) ([]gitprovider.FileChange, error) {
	p.log.add("inspect:" + repo.Name + ":" + tag(base) + ":" + tag(head))
	return p.fakePort.ChangedFiles(ctx, repo, base, head)
}

func (p *loggedPort) ImportBranch(ctx context.Context, spec gitprovider.ImportBranchSpec) (gitprovider.BranchRef, error) {
	p.log.add("copy:" + spec.Source.Name + "->" + spec.Target.Name)
	return p.fakePort.ImportBranch(ctx, spec)
}

type loggedStore struct {
	*fakeIngestStore
	log *callLog
}

func (s *loggedStore) IngestPush(ctx context.Context, in gitprovider.IngestPushParams) (bool, error) {
	s.log.add("record:" + in.Event.Ref)
	return s.fakeIngestStore.IngestPush(ctx, in)
}

func (s *loggedStore) SourceLineEvidence(ctx context.Context, repoID int64, gitRef, headSHA string) ([]gitprovider.ClassifiedChange, []gitprovider.SemanticCandidate, error) {
	s.log.add("source_evidence:" + gitRef + "@" + tag(headSHA))
	return s.fakeIngestStore.SourceLineEvidence(ctx, repoID, gitRef, headSHA)
}

func tag(sha string) string {
	if sha == "" {
		return "root"
	}
	if sha == gitprovider.ZerosSHA {
		return "zeros"
	}
	if len(sha) > 6 {
		return sha[:6]
	}
	return sha
}

// forkImportHarness wires the importer with a shared order log.
type forkImportHarness struct {
	port   *loggedPort
	raw    *fakePort
	ingest *loggedStore
	store  *fakeForkImportStore
	log    *callLog
	imp    *gitprovider.ForkImporter
}

func newForkImportHarness(t *testing.T) *forkImportHarness {
	t.Helper()
	log := &callLog{}
	raw := &fakePort{
		branchByName: map[string]gitprovider.BranchRef{
			"feature":      {Name: "feature", HeadSHA: sourceHead},
			"fork/feature": {Name: "fork/feature", HeadSHA: targetOldHead},
			"main":         {Name: "main", HeadSHA: sourceHead},
			// a research line the platform created without a base state's
			// commit: the ref syncer's default-branch fallback records no
			// fork point for it (the shape T0804's no-baseline route is for).
			"line-r1": {Name: "line-r1", HeadSHA: sourceHead},
		},
		importRef: gitprovider.BranchRef{Name: "fork/feature", HeadSHA: sourceHead},
	}
	ingest := &loggedStore{fakeIngestStore: &fakeIngestStore{inserted: true}, log: log}
	store := forkImportStore()
	port := &loggedPort{fakePort: raw, log: log}
	return &forkImportHarness{
		port: port, raw: raw, ingest: ingest, store: store, log: log,
		imp: gitprovider.NewForkImporter(port, store, testIngester(t, port, ingest)),
	}
}

func (h *forkImportHarness) entries() []string { return h.log.entries }

// TestForkImportRecordsTheCopyBeforeItLands pins the design's ordering
// contract: the delivery's evidence is resolved, RECORDED, and only then
// copied. Recording first is what makes the provider's own webhook for the
// copy collapse onto this row (same repository + ref + after) instead of
// inspecting the copy's whole tree — a fork branch would otherwise be
// blamed for the platform's own bootstrap file and stick at
// unstructured_changes forever (docs/16 §4.1).
func TestForkImportRecordsTheCopyBeforeItLands(t *testing.T) {
	h := newForkImportHarness(t)
	h.ingest.sourceEvidence = []gitprovider.ClassifiedChange{
		{Path: "notes/raw.csv", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}
	res, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// No fork point is recorded for the copied line, so there is no content
	// baseline to diff against: the copy carries the source line's own
	// evidence, read from the canonical record. It is never diffed against
	// the copied tree (the platform's own bootstrap file would mark every
	// fork unstructured_changes for good) and never against the copied
	// commit itself (a copy cleaner than its own source).
	want := []string{
		"source_evidence:refs/heads/feature@" + tag(sourceHead),
		"record:refs/heads/fork/feature",
		"copy:p-source->p-fork",
	}
	if len(h.entries()) != len(want) {
		t.Fatalf("calls = %v, want %v", h.entries(), want)
	}
	for i := range want {
		if h.entries()[i] != want[i] {
			t.Fatalf("calls = %v, want %v", h.entries(), want)
		}
	}
	if res.SourceSHA != sourceHead || res.TargetHeadSHA != sourceHead || res.BeforeSHA != targetOldHead {
		t.Fatalf("result = %+v, want source/head %s and before %s", res, sourceHead, targetOldHead)
	}
	if !res.Inserted {
		t.Fatalf("Inserted = false, want the store's answer")
	}
	if len(h.ingest.params) != 1 {
		t.Fatalf("recorded deliveries = %d, want 1", len(h.ingest.params))
	}
	ev := h.ingest.params[0].Event
	if ev.RepositoryID != 42 || ev.Name != "p-fork" {
		t.Fatalf("recorded against repo %d/%s, want the fork's 42/p-fork", ev.RepositoryID, ev.Name)
	}
	if ev.Ref != "refs/heads/fork/feature" || ev.After != sourceHead {
		t.Fatalf("delivery = %s@%s, want refs/heads/fork/feature@%s", ev.Ref, ev.After, sourceHead)
	}
	if ev.Before != targetOldHead {
		t.Fatalf("delivery before = %s, want the target's pre-import head %s", ev.Before, targetOldHead)
	}
	if ev.Pusher != "post-fork-service" {
		t.Fatalf("pusher = %q, want the platform's fork service", ev.Pusher)
	}
	if len(h.ingest.params[0].Changes) != 1 || h.ingest.params[0].Changes[0].File != gitprovider.FileKindUnstructured {
		t.Fatalf("recorded changes = %+v, want the source line's own junk record", h.ingest.params[0].Changes)
	}
	if h.ingest.params[0].Changes[0].Path != "notes/raw.csv" {
		t.Fatalf("recorded path = %q, want the source line's record", h.ingest.params[0].Changes[0].Path)
	}
	// No content was read for that copy at all: the evidence came from the
	// canonical record, not from a diff of the copied tree.
	if len(h.raw.changedRepos) != 0 || len(h.raw.changedRanges) != 0 {
		t.Fatalf("the copy diffed content (%v, %v), want the source line's records only",
			h.raw.changedRepos, h.raw.changedRanges)
	}
	// The copy names the source commit and the target branch.
	if len(h.raw.importSpecs) != 1 {
		t.Fatalf("copies = %d, want 1", len(h.raw.importSpecs))
	}
	spec := h.raw.importSpecs[0]
	if spec.SourceSHA != sourceHead || spec.Source.Name != "p-source" || spec.Target.Name != "p-fork" || spec.Name != "fork/feature" {
		t.Fatalf("copy spec = %+v, want %s from p-source into p-fork@fork/feature", spec, sourceHead)
	}
}

// TestForkImportUsesTheLinesRecordedForkPoint is the semantic-flag design's
// core: the copy is measured against the fork point the copied LINE
// diverged from, not against the fork branch's own empty history. With a
// recorded fork point the diff runs baseline..head.
func TestForkImportUsesTheLinesRecordedForkPoint(t *testing.T) {
	h := newForkImportHarness(t)
	h.store.forkPoint["refs/heads/feature"] = baselineSHA
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := h.raw.changedRanges[0]; got != baselineSHA+":"+sourceHead {
		t.Fatalf("diff = %s, want %s:%s", got, baselineSHA, sourceHead)
	}
	if h.store.pointRefs[0] != sourceProject+":refs/heads/feature" {
		t.Fatalf("fork point asked for %v, want the copied line", h.store.pointRefs)
	}
}

// TestForkImportCarriesTheSourceLinesEvidenceWithoutAForkPoint: a line the
// platform recorded no fork point for (it was created without a base
// state's commit, so the ref syncer's default-branch fallback left
// git_branch_refs.fork_sha NULL) has no content baseline to diff against.
// The copy then carries the source LINE's own evidence — the records that
// line's completeness flag is derived from — so a line whose own pushes
// carry unparseable content forks into a branch marked
// unstructured_changes, instead of a copy that is cleaner than its source
// and merges the same content. What is NOT carried is the copied tree: the
// records are per path the line itself changed, which is why a clean line's
// copy stays clean (no bootstrap file, no parent content) and a legitimate
// fork is not locked behind a mark only a real push could clear.
func TestForkImportCarriesTheSourceLinesEvidenceWithoutAForkPoint(t *testing.T) {
	h := newForkImportHarness(t)
	h.ingest.sourceEvidence = []gitprovider.ClassifiedChange{
		{Path: "data/raw.csv", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/line-r1",
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// The read is keyed on the copied LINE and the copied COMMIT: the
	// evidence of another ref, or of another head, would describe content
	// this copy does not bring.
	if len(h.ingest.evidenceAskedAbout) != 1 {
		t.Fatalf("evidence reads = %v, want exactly one", h.ingest.evidenceAskedAbout)
	}
	if got := h.ingest.evidenceAskedAbout[0]; got != "refs/heads/line-r1@"+sourceHead {
		t.Fatalf("evidence read %s, want the copied line at the copied commit", got)
	}
	if len(h.raw.changedRepos) != 0 {
		t.Fatalf("the copy diffed content (%v); with no baseline there is nothing to diff against",
			h.raw.changedRepos)
	}
	if len(h.ingest.params) != 1 || len(h.ingest.params[0].Changes) != 1 {
		t.Fatalf("recorded changes = %+v, want the source line's record", h.ingest.params)
	}
	if got := h.ingest.params[0].Changes[0]; got.Path != "data/raw.csv" ||
		got.File != gitprovider.FileKindUnstructured || got.Kind != gitprovider.ChangeAdded {
		t.Fatalf("recorded change = %+v, want the source line's junk record", got)
	}
}

// TestForkImportCarriesTheSourceLinesCandidates: the manifest records the
// source line changed travel with their proposed content, because a
// delivery of a manifest records a candidate (git_push_semantic_candidates)
// and the copy brings the same documents. The content comes from the source
// delivery's own row — the platform already holds it — so the copy re-reads
// no content and re-creates no candidate for anything the line did not
// change.
func TestForkImportCarriesTheSourceLinesCandidates(t *testing.T) {
	h := newForkImportHarness(t)
	h.ingest.sourceEvidence = []gitprovider.ClassifiedChange{
		{Path: "manifests/mat-r1.json", Kind: gitprovider.ChangeAdded,
			File: gitprovider.FileKindManifest, SchemaID: "https://open-rd.example/schemas/material.schema.json",
			ContentSHA256: "0f1e2d"},
	}
	h.ingest.sourceCandidates = []gitprovider.SemanticCandidate{
		{Path: "manifests/mat-r1.json", Kind: gitprovider.ChangeAdded,
			SchemaID: "https://open-rd.example/schemas/material.schema.json",
			Content:  json.RawMessage(`{"id":"mat-r1","type":"material"}`)},
	}
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/line-r4",
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	got := h.ingest.params[0]
	if len(got.Changes) != 1 || got.Changes[0].File != gitprovider.FileKindManifest ||
		got.Changes[0].SchemaID == "" || got.Changes[0].ContentSHA256 != "0f1e2d" {
		t.Fatalf("recorded changes = %+v, want the source line's manifest record", got.Changes)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Path != "manifests/mat-r1.json" ||
		string(got.Candidates[0].Content) != `{"id":"mat-r1","type":"material"}` {
		t.Fatalf("recorded candidates = %+v, want the source line's own proposal", got.Candidates)
	}
}

// TestForkImportPropagatesEvidenceFailures: an evidence read that fails must
// fail the import rather than fall through to a copy with no evidence at all
// — a failed read makes no claim about the canonical record, and a copy that
// silently claims nothing is the default value this route exists to refuse.
func TestForkImportPropagatesEvidenceFailures(t *testing.T) {
	h := newForkImportHarness(t)
	h.ingest.sourceEvidenceErr = errors.New("store down")
	_, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/line-r1",
		TargetBranch:    "fork/feature",
	})
	if err == nil || err.Error() != "store down" {
		t.Fatalf("Import = %v, want the store's failure", err)
	}
	if len(h.raw.importSpecs) != 0 {
		t.Fatalf("a failed evidence read still copied content: %v", h.raw.importSpecs)
	}
}

// TestForkImportDefaultsToTheSourceDefaultBranch: an unnamed source ref is
// the source repository's default branch, resolved by NAME and then read —
// the head, not the branch name.
func TestForkImportDefaultsToTheSourceDefaultBranch(t *testing.T) {
	h := newForkImportHarness(t)
	h.raw.getRepoSet = true
	h.raw.getRepo = gitprovider.Repository{Owner: "post-git-svc", Name: "p-source", ID: 41, DefaultBranch: "main"}
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if h.raw.branchGot[0] != "main" {
		t.Fatalf("source read %q, want the default branch by name", h.raw.branchGot[0])
	}
	if h.ingest.params[0].Event.After != sourceHead {
		t.Fatalf("delivery after = %s, want the default branch's head %s", h.ingest.params[0].Event.After, sourceHead)
	}
}

// TestForkImportRefusesWhenTheSourceHeadIsUnreachable: a source ref that
// does not exist is refused, and the refusal lands before anything is
// recorded or copied — the fork does not get a delivery for content that
// never arrived.
func TestForkImportRefusesWhenTheSourceHeadIsUnreachable(t *testing.T) {
	h := newForkImportHarness(t)
	h.raw.branchByNameErr = map[string]error{"feature": gitprovider.ErrNotFound}
	_, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("Import = %v, want ErrNotFound", err)
	}
	if len(h.entries()) != 0 {
		t.Fatalf("a refused import wrote: %v", h.entries())
	}
}

// TestForkImportAppliesTheFrozenMainRule: the copy path resolves the same
// refs/heads/main rule the push path does. The service never targets main,
// so this is the backstop — and a backstop that is not exercised is not
// known to exist.
func TestForkImportAppliesTheFrozenMainRule(t *testing.T) {
	h := newForkImportHarness(t)
	h.ingest.mainFrozen = true
	_, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "main",
	})
	var refusal *gitprovider.MainFrozenRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("Import onto a frozen main = %v, want MainFrozenRefusalError", err)
	}
	if len(h.entries()) != 0 {
		t.Fatalf("a refused import touched the provider or the store: %v", h.entries())
	}
}

// TestForkImportPropagatesStoreFailures: a fork-point read that fails must
// fail the import rather than silently fall back to "no baseline" — the
// fallback is a claim about the canonical record, and a failed read makes
// no claim.
func TestForkImportPropagatesForkPointFailures(t *testing.T) {
	h := newForkImportHarness(t)
	h.store.pointErr = errors.New("store down")
	_, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	})
	if err == nil || err.Error() != "store down" {
		t.Fatalf("Import = %v, want the store's failure", err)
	}
	if len(h.entries()) != 0 {
		t.Fatalf("a failed fork-point read ran the import: %v", h.entries())
	}
}

// TestForkImportNeedsAProvisionedPair: a project with no repository has
// nothing to copy from or into.
func TestForkImportNeedsAProvisionedPair(t *testing.T) {
	h := newForkImportHarness(t)
	delete(h.store.repos, targetProject)
	_, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	})
	if !errors.Is(err, gitprovider.ErrRepoNotProvisioned) {
		t.Fatalf("Import = %v, want ErrRepoNotProvisioned", err)
	}
	if len(h.entries()) != 0 {
		t.Fatalf("an unprovisioned pair reached the provider: %v", h.entries())
	}
}

// TestForkImportNeedsItsShape: the request's identity fields are required.
func TestForkImportNeedsItsShape(t *testing.T) {
	h := newForkImportHarness(t)
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{SourceProjectID: sourceProject}); !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("Import without a target = %v, want ErrConflict", err)
	}
	// The actor is one of them: the copy lands a state transition, and a
	// transition made for nobody is not a transition this platform records
	// (T0817). The refusal is checked BEFORE any read, so the harness's
	// store and provider are both untouched.
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		TargetBranch:    "fork/feature",
	}); !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("Import without an actor = %v, want ErrConflict", err)
	}
	if len(h.entries()) != 0 {
		t.Fatalf("an actorless import reached the provider: %v", h.entries())
	}
}

// TestForkImportDeliveryIDIsDerivedFromTheDelivery: deliverables are
// correlation only, but two different copies must never share one id.
func TestForkImportDeliveryIDIsDerived(t *testing.T) {
	h := newForkImportHarness(t)
	if _, err := h.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	first := h.ingest.params[0].Event.DeliveryID
	if first == "" {
		t.Fatalf("delivery id is empty")
	}
	h2 := newForkImportHarness(t)
	h2.raw.branchByName["feature"] = gitprovider.BranchRef{Name: "feature", HeadSHA: targetOldHead}
	if _, err := h2.imp.Import(context.Background(), gitprovider.ForkImportRequest{
		SourceProjectID: sourceProject,
		TargetProjectID: targetProject,
		ActorID:         forkActor,
		SourceRef:       "refs/heads/feature",
		TargetBranch:    "fork/feature",
	}); err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if h2.ingest.params[0].Event.DeliveryID == first {
		t.Fatalf("two different copies share the delivery id %q", first)
	}
}
