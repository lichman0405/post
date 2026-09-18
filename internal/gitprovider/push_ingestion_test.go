package gitprovider_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Push ingestion unit tests (T0305): signature verification, payload
// parsing, the git-protocol inspection, the manifest classification, and
// the store handoff. The canonical store itself (dedupe transaction,
// head-pointer update) runs in the integration suite against PostgreSQL.

// pushPayloadJSON builds a delivery-shaped Gitea push payload (the shape
// the running instance delivers; the receiver parses only the fields the
// ingestion depends on).
func pushPayloadJSON(ref, before, after string, repoID int64, commits int) string {
	p := map[string]any{
		"ref":    ref,
		"before": before,
		"after":  after,
		"repository": map[string]any{
			"id":    repoID,
			"name":  "p-abc",
			"owner": map[string]any{"login": "post-git-svc"},
		},
		"pusher":        map[string]any{"login": "post-git-svc"},
		"total_commits": commits,
		"commits": []map[string]any{
			{
				"id":      after,
				"message": "push",
				"author":  map[string]any{"login": "post-git-svc"},
			},
		},
	}
	b, _ := json.Marshal(p)
	return string(b)
}

const testSHA = "0123456789012345678901234567890123456789"
const testSHA2 = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

func TestVerifyPushSignature(t *testing.T) {
	secret := "webhook-secret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if !gitprovider.VerifyPushSignature(secret, body, want) {
		t.Fatal("valid signature rejected")
	}
	if gitprovider.VerifyPushSignature(secret, []byte(`{"ref":"refs/heads/main" }`), want) {
		t.Fatal("tampered body accepted")
	}
	if gitprovider.VerifyPushSignature("other-secret", body, want) {
		t.Fatal("wrong secret accepted")
	}
	if gitprovider.VerifyPushSignature("", body, want) {
		t.Fatal("empty secret accepted")
	}
	if gitprovider.VerifyPushSignature(secret, body, "") {
		t.Fatal("empty signature accepted")
	}
	if gitprovider.VerifyPushSignature(secret, body, "not-hex") {
		t.Fatal("non-hex signature accepted")
	}
	if gitprovider.VerifyPushSignature(secret, body, strings.ToUpper(want)) {
		t.Fatal("upper-case signature accepted: the provider signs lowercase hex")
	}
}

func TestParsePushEventPayloadMapping(t *testing.T) {
	ev, err := gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"refs/heads/semantic", testSHA, testSHA2, 42, 1)), "delivery-1")
	if err != nil {
		t.Fatalf("ParsePushEvent: %v", err)
	}
	if ev.Ref != "refs/heads/semantic" {
		t.Errorf("Ref = %q", ev.Ref)
	}
	if ev.Before != testSHA || ev.After != testSHA2 {
		t.Errorf("Before/After = %q/%q", ev.Before, ev.After)
	}
	if ev.RepositoryID != 42 || ev.Owner != "post-git-svc" || ev.Name != "p-abc" {
		t.Errorf("repository mapping = %d/%q/%q", ev.RepositoryID, ev.Owner, ev.Name)
	}
	if ev.Pusher != "post-git-svc" || ev.TotalCommits != 1 {
		t.Errorf("pusher/total = %q/%d", ev.Pusher, ev.TotalCommits)
	}
	if len(ev.Commits) != 1 || ev.Commits[0].ID != testSHA2 || ev.Commits[0].Author != "post-git-svc" {
		t.Errorf("commits = %+v", ev.Commits)
	}
	if ev.DeliveryID != "delivery-1" {
		t.Errorf("DeliveryID = %q", ev.DeliveryID)
	}
}

func TestParsePushEventBranchCreation(t *testing.T) {
	// A branch-creation push delivers before = all zeros — the shape the
	// running instance delivers for the first push of a ref.
	ev, err := gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"refs/heads/first", gitprovider.ZerosSHA, testSHA, 7, 0)), "d")
	if err != nil {
		t.Fatalf("ParsePushEvent: %v", err)
	}
	if ev.Before != gitprovider.ZerosSHA {
		t.Errorf("Before = %q, want zeros", ev.Before)
	}
}

func TestParsePushEventRejects(t *testing.T) {
	// A tag push is a legal delivery the ingestion has nothing to do for —
	// but the parsed event still carries the repository id, so the receiver
	// can verify the delivery's signature before ignoring it.
	ev, err := gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"refs/tags/v1", testSHA, testSHA2, 1, 0)), "d")
	if !errors.Is(err, gitprovider.ErrNotABranchPush) {
		t.Errorf("tag ref: err = %v, want ErrNotABranchPush", err)
	}
	if ev.RepositoryID != 1 || ev.Ref != "refs/tags/v1" {
		t.Errorf("tag ref: event = %+v, want the parsed event alongside the error", ev)
	}
	_, err = gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"", testSHA, testSHA2, 1, 0)), "d")
	if !errors.Is(err, gitprovider.ErrNotABranchPush) {
		t.Errorf("empty ref: err = %v, want ErrNotABranchPush", err)
	}
	if _, err := gitprovider.ParsePushEvent([]byte(`{"ref":`), "d"); err == nil {
		t.Fatal("malformed JSON accepted")
	} else if errors.Is(err, gitprovider.ErrNotABranchPush) {
		t.Fatal("malformed JSON misclassified as a non-branch push")
	}
	// Short SHAs are not full commit ids: the inspection depends on them.
	if _, err := gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"refs/heads/x", "abcd", testSHA2, 1, 0)), "d"); err == nil {
		t.Fatal("short before sha accepted")
	}
	if _, err := gitprovider.ParsePushEvent([]byte(pushPayloadJSON(
		"refs/heads/x", testSHA, "abcd", 1, 0)), "d"); err == nil {
		t.Fatal("short after sha accepted")
	}
}

func TestGitStateHash(t *testing.T) {
	// The hash is sha256 over the canonical JSON {"git_commit_sha": sha} —
	// pinned here so a later change to the identity breaks loudly.
	h := gitprovider.GitStateHash(testSHA)
	payload, _ := json.Marshal(struct {
		GitCommitSHA string `json:"git_commit_sha"`
	}{testSHA})
	sum := sha256.Sum256(payload)
	if h != hex.EncodeToString(sum[:]) {
		t.Errorf("GitStateHash = %q, want %q", h, hex.EncodeToString(sum[:]))
	}
	// Determinism across repeated calls: the same commit must always
	// yield the same state identity — a redelivered webhook relies on the
	// hash colliding, never varying. Both calls are genuinely evaluated
	// (stored in separate variables, so a per-call state leak fails the
	// comparison rather than two copies of one expression).
	h1 := gitprovider.GitStateHash(testSHA)
	h2 := gitprovider.GitStateHash(testSHA)
	if h1 != h2 {
		t.Fatal("GitStateHash is not deterministic")
	}
	if gitprovider.GitStateHash(testSHA) == gitprovider.GitStateHash(testSHA2) {
		t.Fatal("distinct commits share a state hash")
	}
}

func TestChangedFilesScripted(t *testing.T) {
	var fetchArgs, diffArgs []string
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit, // init --bare
		func(c gitCall) (string, error) { // fetch head+base
			fetchArgs = append(fetchArgs, c.args[2:]...)
			return "", nil
		},
		func(c gitCall) (string, error) { // diff-tree
			diffArgs = append(diffArgs, c.args[2:]...)
			return "M\x00a.json\x00A\x00b.json\x00D\x00gone.json\x00", nil
		},
	)))
	changes, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	want := []gitprovider.FileChange{
		{Path: "a.json", Kind: gitprovider.ChangeModified},
		{Path: "b.json", Kind: gitprovider.ChangeAdded},
		{Path: "gone.json", Kind: gitprovider.ChangeRemoved},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v", changes)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, changes[i], want[i])
		}
	}
	// The fetch must request BOTH endpoints in one shallow pass: the head
	// and the base, from the repository's push URL.
	if len(fetchArgs) != 5 || fetchArgs[0] != "fetch" || fetchArgs[1] != "--depth=1" ||
		fetchArgs[3] != testSHA2 || fetchArgs[4] != testSHA ||
		!strings.HasSuffix(fetchArgs[2], "/post-git-svc/p-abc.git") {
		t.Errorf("fetch args = %v", fetchArgs)
	}
	// The diff names base then head — the direction of the push.
	if want := []string{"diff-tree", "-r", "-z", "--name-status", testSHA, testSHA2}; !slices.Equal(diffArgs, want) {
		t.Errorf("diff args = %v, want %v", diffArgs, want)
	}
}

func TestChangedFilesRootFallback(t *testing.T) {
	// The base commit became unreachable (a force push rewrote history):
	// the fetch of the base fails and the diff degrades to --root against
	// the head alone — every file at the head is candidate content.
	var fetches []string
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		func(c gitCall) (string, error) { // fetch with base fails
			fetches = append(fetches, strings.Join(c.args[2:], " "))
			return "fatal: couldn't find remote ref", errors.New("git: exit status 128")
		},
		func(c gitCall) (string, error) { // refetch head alone
			fetches = append(fetches, strings.Join(c.args[2:], " "))
			return "", nil
		},
		func(c gitCall) (string, error) { // --root diff
			if want := "--root"; !slices.Contains(c.args, want) {
				t.Errorf("diff args = %v, want --root", c.args)
			}
			return "A\x00a.json\x00", nil
		},
	)))
	changes, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.json" || changes[0].Kind != gitprovider.ChangeAdded {
		t.Errorf("changes = %+v", changes)
	}
	if len(fetches) != 2 || !strings.Contains(fetches[1], testSHA2) || strings.Contains(fetches[1], testSHA+" ") {
		t.Errorf("fetches = %v", fetches)
	}
}

func TestChangedFilesEmptyBase(t *testing.T) {
	// A branch-creation push (base empty): one fetch of the head and a
	// --root diff, no base endpoint on the fetch at all.
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		func(c gitCall) (string, error) {
			if want := []string{"fetch", "--depth=1"}; !slices.Equal(c.args[2:4], want) {
				t.Errorf("fetch args = %v, want prefix %v", c.args[2:], want)
			}
			if len(c.args) != 6 {
				t.Errorf("fetch args = %v, want exactly the head, no base", c.args[2:])
			}
			return "", nil
		},
		func(c gitCall) (string, error) {
			if want := "--root"; !slices.Contains(c.args, want) {
				t.Errorf("diff args = %v, want --root", c.args)
			}
			return "", nil
		},
	)))
	changes, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, "", testSHA)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want empty (no-change diff)", changes)
	}
}

func TestChangedFilesDiffFailure(t *testing.T) {
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		okGit,
		failGit("fatal: bad object"),
	)))
	_, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestChangedFilesUnexpectedStatus(t *testing.T) {
	// A diff-tree status the parser does not know is a provider-shape
	// surprise: surface it as unavailable, never guess.
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		okGit,
		func(gitCall) (string, error) { return "R100\x00a.json\x00", nil },
	)))
	_, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestChangedFilesTypechangeStatus(t *testing.T) {
	// A type change (file → symlink etc.) surfaces as its own status; it
	// must not fail the whole ingestion — a 503 on this shape would put
	// the provider into a permanent redelivery loop. It maps to modified:
	// the file at the path changed, and no rename detection is in play.
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		okGit,
		func(gitCall) (string, error) { return "T\x00link.json\x00", nil },
	)))
	changes, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	want := []gitprovider.FileChange{{Path: "link.json", Kind: gitprovider.ChangeModified}}
	if len(changes) != 1 || changes[0] != want[0] {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
}

func TestChangedFilesFetchFailurePropagates(t *testing.T) {
	// A fetch failure that is NOT a missing ref must propagate: it is a
	// transient provider failure, and degrading to a --root diff would
	// record every file at the head as added, in append-only rows that
	// cannot be rewritten. The receiver answers 503 and the provider
	// redelivers. Only the missing-ref shape (ErrNotFound) may degrade.
	var fetches int
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		func(c gitCall) (string, error) { // fetch head+base: provider blip
			fetches++
			return "fatal: remote error: service unavailable", errors.New("git: exit status 128")
		},
	)))
	_, err := a.ChangedFiles(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, testSHA2)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	// The scripted runner also fails the test on any further call: the
	// counter asserts the --root refetch never happened.
	if fetches != 1 {
		t.Errorf("fetch attempts = %d, want exactly 1 (no --root fallback retry)", fetches)
	}
}

func TestReadFileScripted(t *testing.T) {
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		okGit,
		func(c gitCall) (string, error) { // cat-file -s: the size probe
			if want := []string{"cat-file", "-s"}; !slices.Equal(c.args[2:4], want) {
				t.Errorf("cat-file args = %v, want prefix %v", c.args[2:], want)
			}
			if spec := testSHA + ":manifests/a.json"; !slices.Contains(c.args, spec) {
				t.Errorf("cat-file args = %v, want spec %q", c.args, spec)
			}
			return "19\n", nil
		},
		func(c gitCall) (string, error) { // git show
			if want := testSHA + ":manifests/a.json"; !slices.Contains(c.args, want) {
				t.Errorf("show args = %v, want spec %q", c.args, want)
			}
			return `{"type":"material"}`, nil
		},
	)))
	content, err := a.ReadFile(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, "manifests/a.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != `{"type":"material"}` {
		t.Errorf("content = %q", content)
	}
}

func TestReadFileOversize(t *testing.T) {
	// A file beyond the 1 MiB read bound must not balloon the ingestion:
	// the size is probed BEFORE the content is read (git show buffers the
	// whole blob into the runner before any check could see it), so the
	// oversize content is never read at all — the extra scripted call
	// would fail the test — and the error reports the bound (classification
	// falls back to unstructured), not a provider failure.
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		okGit, // fetch the head
		func(c gitCall) (string, error) { // cat-file -s: oversize probe
			if want := []string{"cat-file", "-s"}; !slices.Equal(c.args[2:4], want) {
				t.Errorf("cat-file args = %v, want prefix %v", c.args[2:], want)
			}
			return strconv.Itoa((1<<20)+1) + "\n", nil
		},
	)))
	_, err := a.ReadFile(context.Background(),
		gitprovider.Repository{Owner: "post-git-svc", Name: "p-abc", ID: 1}, testSHA, "big.json")
	if err == nil {
		t.Fatal("oversize file read succeeded")
	}
	if errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("oversize read mapped to a provider failure: %v", err)
	}
}

// fakeIngestStore records the store handoff of the ingester (the
// transaction behavior runs against PostgreSQL in the integration suite).
type fakeIngestStore struct {
	secret     string
	secretErr  error
	calls      []string
	params     []gitprovider.IngestPushParams
	inserted   bool
	ingestErr  error
	mainFrozen bool
	// platformMergeAfter is the commit this project's main has recorded as
	// its own merge: the fake pledges the seam ONLY for a verdict asked
	// about exactly this after SHA. A fake that answered true for every
	// delivery would model a bypass, not the rule.
	platformMergeAfter string
	// verdictAfters records the after SHA each verdict was asked about.
	verdictAfters []string
	frozenErr     error
	// sourceEvidence is what the source line's evidence read answers
	// (T0804's no-fork-point route): the records the source line's own
	// completeness flag is derived from, as the copy carries them.
	sourceEvidence     []gitprovider.ClassifiedChange
	sourceCandidates   []gitprovider.SemanticCandidate
	sourceEvidenceErr  error
	evidenceAskedAbout []string
}

func (f *fakeIngestStore) WebhookSecretByRepoID(context.Context, int64) (string, error) {
	if f.secretErr != nil {
		return "", f.secretErr
	}
	if f.secret != "" {
		return f.secret, nil
	}
	return "secret", nil
}

// MainPushVerdict answers the T0601 rule's Git-side question. It is
// recorded in calls like every other store handoff, so a test can show the
// refusal happened before the provider was asked anything — and it records
// the after SHA it was asked about, so a test can show the seam is keyed on
// the delivery's own commit and not on something the ingester invented.
func (f *fakeIngestStore) MainPushVerdict(_ context.Context, _ int64, afterSHA string) (gitprovider.MainPushVerdict, error) {
	f.calls = append(f.calls, "main_frozen")
	f.verdictAfters = append(f.verdictAfters, afterSHA)
	return gitprovider.MainPushVerdict{
		Frozen:        f.mainFrozen,
		PlatformMerge: f.platformMergeAfter != "" && afterSHA == f.platformMergeAfter,
	}, f.frozenErr
}

func (f *fakeIngestStore) IngestPush(_ context.Context, in gitprovider.IngestPushParams) (bool, error) {
	f.calls = append(f.calls, "ingest")
	f.params = append(f.params, in)
	return f.inserted, f.ingestErr
}

// SourceLineEvidence answers the source line's evidence (T0804). It records
// the ref and commit it was asked about, so a test can show the read is
// keyed on the copied line and the copied commit rather than on whatever
// head the store happened to find.
func (f *fakeIngestStore) SourceLineEvidence(_ context.Context, _ int64, gitRef, headSHA string) ([]gitprovider.ClassifiedChange, []gitprovider.SemanticCandidate, error) {
	f.calls = append(f.calls, "source_evidence")
	f.evidenceAskedAbout = append(f.evidenceAskedAbout, gitRef+"@"+headSHA)
	if f.sourceEvidenceErr != nil {
		return nil, nil, f.sourceEvidenceErr
	}
	return f.sourceEvidence, f.sourceCandidates, nil
}

// validMaterialDoc is a minimal document the material schema accepts.
func validMaterialDoc() string {
	return `{
	  "id": "mat-0001",
	  "type": "material",
	  "version": 1,
	  "project_id": "p1",
	  "title": "Steel 316L",
	  "lifecycle_state": "active",
	  "schema_ref": {"id": "` + schemareg.CanonicalNamespace + `material.schema.json", "version": "1"},
	  "created_by": "u1",
	  "created_at": "2026-09-14T00:00:00Z"
	}`
}

// validManifestDoc is a minimal state manifest the rsg-manifest schema
// accepts (untyped — no properties.type). It carries the T0206 shape: the
// format version is the exporter's constant (a literal would silently
// drift from internal/rsg/manifest), git_ref is the state's recorded
// commit sha string, schema_refs/policy_refs are present (empty: the
// fixture pins no schema or policy), and the state_hash is the REAL
// digest — computed with the exporter's own rule (manifest.Digest over the
// canonical JSON of the semantic content below minus generated_at and
// state_hash), so the document verifies the way a re-exported manifest
// would.
func validManifestDoc() string {
	sha := testSHA
	content := struct {
		FormatVersion    string   `json:"format_version"`
		ProjectID        string   `json:"project_id"`
		StateID          string   `json:"state_id"`
		ObjectVersions   []any    `json:"object_versions"`
		RelationVersions []any    `json:"relation_versions"`
		SchemaRefs       []string `json:"schema_refs"`
		PolicyRefs       []string `json:"policy_refs"`
		BlobRefs         []any    `json:"blob_refs"`
		GitRef           *string  `json:"git_ref"`
	}{
		FormatVersion:    manifest.FormatV1,
		ProjectID:        "p1",
		StateID:          "s1",
		ObjectVersions:   []any{},
		RelationVersions: []any{},
		SchemaRefs:       []string{},
		PolicyRefs:       []string{},
		BlobRefs:         []any{},
		GitRef:           &sha,
	}
	contentJSON, _ := json.Marshal(content)
	stateHash := manifest.Digest(contentJSON)
	return `{
	  "format_version": "` + manifest.FormatV1 + `",
	  "project_id": "p1",
	  "state_id": "s1",
	  "generated_at": "2026-09-14T00:00:00Z",
	  "object_versions": [],
	  "relation_versions": [],
	  "schema_refs": [],
	  "policy_refs": [],
	  "blob_refs": [],
	  "git_ref": "` + testSHA + `",
	  "state_hash": "` + stateHash + `"
	}`
}

func testIngester(t *testing.T, port gitprovider.GitPort, store gitprovider.IngestStore) *gitprovider.PushIngester {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return gitprovider.NewPushIngester(port, store, reg)
}

func pushEvent(after string) gitprovider.PushEvent {
	return gitprovider.PushEvent{
		Ref:          "refs/heads/semantic",
		Before:       testSHA,
		After:        after,
		RepositoryID: 1,
		Owner:        "post-git-svc",
		Name:         "p-abc",
		Pusher:       "post-git-svc",
		TotalCommits: 1,
		DeliveryID:   "d-1",
	}
}

func TestIngestClassifiesManifest(t *testing.T) {
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded},
			{Path: "README.md", Kind: gitprovider.ChangeModified},
		},
		files: map[string][]byte{
			testSHA2 + ":manifests/mat.json": []byte(validMaterialDoc()),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), pushEvent(testSHA2))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	if len(store.params) != 1 {
		t.Fatalf("store handoffs = %d", len(store.params))
	}
	in := store.params[0]
	if len(in.Changes) != 2 {
		t.Fatalf("changes = %+v", in.Changes)
	}
	mat := in.Changes[0]
	if mat.Path != "manifests/mat.json" || mat.File != gitprovider.FileKindManifest ||
		mat.SchemaID != schemareg.CanonicalNamespace+"material.schema.json" {
		t.Errorf("classified = %+v", mat)
	}
	if mat.ContentSHA256 == "" {
		t.Error("manifest change carries no content hash")
	}
	if sum := sha256.Sum256([]byte(validMaterialDoc())); mat.ContentSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("ContentSHA256 = %q, want sha256 of the document", mat.ContentSHA256)
	}
	readme := in.Changes[1]
	if readme.File != gitprovider.FileKindUnstructured || readme.SchemaID != "" || readme.ContentSHA256 != "" {
		t.Errorf("README classified = %+v, want unstructured", readme)
	}
	if len(in.Candidates) != 1 {
		t.Fatalf("candidates = %+v", in.Candidates)
	}
	cand := in.Candidates[0]
	if cand.Path != "manifests/mat.json" || cand.Kind != gitprovider.ChangeAdded ||
		cand.SchemaID != schemareg.CanonicalNamespace+"material.schema.json" {
		t.Errorf("candidate = %+v", cand)
	}
	if string(cand.Content) != validMaterialDoc() {
		t.Errorf("candidate content = %s, want the raw document", cand.Content)
	}
	// The README was never read — unstructured files do not touch the
	// provider; exactly one file read happened (the manifest).
	var reads int
	for _, c := range port.calls {
		if c == "read-file" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("provider reads = %d, want exactly one (the manifest); calls %v", reads, port.calls)
	}
}

func TestIngestRemovedManifestReadsAtBase(t *testing.T) {
	// A removed manifest is classified on its PRE-push content (the base
	// commit's version) — that is what the delete proposes to remove.
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "manifests/mat.json", Kind: gitprovider.ChangeRemoved},
		},
		files: map[string][]byte{
			testSHA + ":manifests/mat.json": []byte(validMaterialDoc()),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	if _, err := ingester.Ingest(context.Background(), pushEvent(testSHA2)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	cand := store.params[0].Candidates[0]
	if cand.Kind != gitprovider.ChangeRemoved ||
		cand.SchemaID != schemareg.CanonicalNamespace+"material.schema.json" {
		t.Errorf("candidate = %+v, want a removed material", cand)
	}
}

func TestIngestUntypedManifest(t *testing.T) {
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "state.json", Kind: gitprovider.ChangeAdded},
		},
		files: map[string][]byte{
			testSHA2 + ":state.json": []byte(validManifestDoc()),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	if _, err := ingester.Ingest(context.Background(), pushEvent(testSHA2)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	cand := store.params[0].Candidates[0]
	if cand.SchemaID != schemareg.CanonicalNamespace+"rsg-manifest.schema.json" {
		t.Errorf("candidate schema = %q, want the canonical rsg-manifest id", cand.SchemaID)
	}
}

func TestIngestClassifiesRelationManifest(t *testing.T) {
	// A relation document declares a free-string type ("uses") that no
	// registered schema pins with a TypeConst: classification must fall
	// through to the untyped manifest list and match it by shape, so
	// relation.schema.json is reachable and the relation becomes a
	// candidate. (With the old early return this path was dead code.)
	relation := `{
	  "id": "rel-0001",
	  "type": "uses",
	  "version": 1,
	  "source_version_ref": "mat-0001@1",
	  "target_version_ref": "mat-0002@1",
	  "created_by": "u1",
	  "created_at": "2026-09-14T00:00:00Z"
	}`
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "relations/rel.json", Kind: gitprovider.ChangeAdded},
		},
		files: map[string][]byte{
			testSHA2 + ":relations/rel.json": []byte(relation),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	if _, err := ingester.Ingest(context.Background(), pushEvent(testSHA2)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	ch := store.params[0].Changes[0]
	if ch.File != gitprovider.FileKindManifest {
		t.Fatalf("relation classified = %q, want semantic_manifest", ch.File)
	}
	if ch.SchemaID != schemareg.CanonicalNamespace+"relation.schema.json" {
		t.Errorf("relation schema = %q, want the canonical relation id", ch.SchemaID)
	}
	if len(store.params[0].Candidates) != 1 {
		t.Errorf("candidates = %+v, want the relation candidate", store.params[0].Candidates)
	}
}

func TestIngestBrokenManifestIsUnstructured(t *testing.T) {
	// A document DECLARING type material that violates the material schema
	// is broken, not a manifest — it must not become a candidate.
	broken := `{"id":"mat-1","type":"material","version":1}`
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded},
		},
		files: map[string][]byte{
			testSHA2 + ":manifests/mat.json": []byte(broken),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	if _, err := ingester.Ingest(context.Background(), pushEvent(testSHA2)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := store.params[0].Changes[0].File; got != gitprovider.FileKindUnstructured {
		t.Errorf("broken manifest classified = %q, want unstructured", got)
	}
	if len(store.params[0].Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", store.params[0].Candidates)
	}
}

func TestIngestGarbageJSONIsUnstructured(t *testing.T) {
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "notes.json", Kind: gitprovider.ChangeAdded},
		},
		files: map[string][]byte{
			testSHA2 + ":notes.json": []byte(`not json at all`),
		},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	if _, err := ingester.Ingest(context.Background(), pushEvent(testSHA2)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := store.params[0].Changes[0].File; got != gitprovider.FileKindUnstructured {
		t.Errorf("garbage JSON classified = %q, want unstructured", got)
	}
	if len(store.params[0].Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", store.params[0].Candidates)
	}
}

func TestIngestReadFailureIsUnstructured(t *testing.T) {
	// A provider read failure on one file degrades that file to
	// unstructured: the push is still ingested.
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{
			{Path: "manifests/mat.json", Kind: gitprovider.ChangeModified},
		},
		readFileErr: errors.New("boom"),
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), pushEvent(testSHA2))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false")
	}
	if got := store.params[0].Changes[0].File; got != gitprovider.FileKindUnstructured {
		t.Errorf("unreadable file classified = %q, want unstructured", got)
	}
}

func TestIngestChangedFilesErrorPropagates(t *testing.T) {
	port := &fakePort{changedFilesErr: gitprovider.ErrUnavailable}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	_, err := ingester.Ingest(context.Background(), pushEvent(testSHA2))
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("store called %d times on an inspection failure", len(store.calls))
	}
}

func TestIngestDuplicatePassThrough(t *testing.T) {
	// The store's dedupe verdict is the service's verdict: a duplicate
	// delivery reports inserted=false and the receiver answers 204 all the
	// same (it IS the acceptance criterion 重复 webhook 不重复 state).
	port := &fakePort{changedFiles: nil}
	store := &fakeIngestStore{inserted: false}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), pushEvent(testSHA2))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if inserted {
		t.Fatal("inserted = true, want false (duplicate)")
	}
}

func TestIngestZerosAfterSkipsInspection(t *testing.T) {
	// A zeros after (ref deletion — defensive on this instance, which
	// delivers deletions as their own event) records without inspection:
	// there is no pushed head to diff.
	port := &fakePort{changedFilesErr: errors.New("must not be called")}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	ev := pushEvent(gitprovider.ZerosSHA)
	inserted, err := ingester.Ingest(context.Background(), ev)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false")
	}
	for _, c := range port.calls {
		if c == "changed-files" {
			t.Fatal("provider diff ran for a zeros after")
		}
	}
	if len(store.params) != 1 || len(store.params[0].Changes) != 0 {
		t.Errorf("handoff = %+v, want a bare event", store.params)
	}
}

// The Git-side half of the frozen-main rule (T0601): a delivery that moves
// refs/heads/main into a frozen project is refused, and the refusal is the
// delivery's whole outcome — no provider call, no canonical row. The
// control below is the same delivery into an UNFROZEN project, which runs
// the ordinary pipeline: without it, a bug that refused every main push
// would look identical to a working rule.

func mainPushEvent(after string) gitprovider.PushEvent {
	ev := pushEvent(after)
	ev.Ref = gitprovider.MainRef
	return ev
}

func TestIngestRefusesMainPushToFrozenProject(t *testing.T) {
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded}},
		files:        map[string][]byte{testSHA2 + ":manifests/mat.json": []byte(validMaterialDoc())},
	}
	store := &fakeIngestStore{inserted: true, mainFrozen: true}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), mainPushEvent(testSHA2))
	if err == nil {
		t.Fatal("a direct push to a frozen main was ingested")
	}
	var refusal *gitprovider.MainFrozenRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *MainFrozenRefusalError", err)
	}
	if refusal.Code() != gitprovider.CodeMainFrozenDirectWrite {
		t.Errorf("code = %q, want %q", refusal.Code(), gitprovider.CodeMainFrozenDirectWrite)
	}
	if inserted {
		t.Error("inserted = true on a refused delivery")
	}
	// The provider is asked NOTHING: the refusal precedes the diff and the
	// file reads (the task's "the refusal happens before the provider
	// action" requirement, which is why the check sits at the top of Ingest
	// rather than beside the store call).
	if len(port.calls) != 0 {
		t.Errorf("provider calls = %v, want none (the refusal precedes the provider action)", port.calls)
	}
	// And nothing is written: the store was consulted for the flag and
	// never for an ingestion.
	for _, c := range store.calls {
		if c == "ingest" {
			t.Fatalf("store calls = %v, want no ingestion", store.calls)
		}
	}
}

// The seam the frozen rule needs (T0601): the platform's own merge reaches
// the ingester as an ordinary main push, so the refusal above would refuse
// the governed path too. The store pledges exactly one delivery — the one
// whose after is the merge commit the project's main recorded — and the two
// tests below are the two sides of that pledge on ONE instrument: the same
// frozen project accepts its own merge and refuses a foreign push.

// foreignSHA is a commit the frozen project's main never recorded as its
// merge: a real full SHA, so nothing but the rule can explain a refusal.
const foreignSHA = "3333333333333333333333333333333333333333"

func TestIngestAcceptsThePlatformsOwnMergeIntoFrozenMain(t *testing.T) {
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded}},
		files:        map[string][]byte{testSHA2 + ":manifests/mat.json": []byte(validMaterialDoc())},
	}
	store := &fakeIngestStore{inserted: true, mainFrozen: true, platformMergeAfter: testSHA2}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), mainPushEvent(testSHA2))
	if err != nil {
		t.Fatalf("the platform's own merge into frozen main was refused: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	// The seam reads the RECORD: the verdict was asked about this
	// delivery's own after SHA, not about the repository or the pusher.
	if len(store.verdictAfters) != 1 || store.verdictAfters[0] != testSHA2 {
		t.Fatalf("verdicts asked about %v, want the delivery's after %s", store.verdictAfters, testSHA2)
	}
	if len(port.calls) == 0 {
		t.Error("provider was never asked for the diff of the accepted merge")
	}
	if len(store.params) != 1 {
		t.Fatalf("store handoffs = %d, want 1", len(store.params))
	}
}

func TestIngestRefusesForeignPushToFrozenMainWithARecordedMerge(t *testing.T) {
	// Same frozen project, same recorded merge, different delivery: the
	// seam is one merge wide. A store that pledged any main push (or an
	// ingester that skipped the verdict) lets this through.
	port := &fakePort{changedFilesErr: errors.New("must not be called")}
	store := &fakeIngestStore{inserted: true, mainFrozen: true, platformMergeAfter: testSHA2}
	ingester := testIngester(t, port, store)

	_, err := ingester.Ingest(context.Background(), mainPushEvent(foreignSHA))
	var refusal *gitprovider.MainFrozenRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *MainFrozenRefusalError", err)
	}
	if refusal.Code() != gitprovider.CodeMainFrozenDirectWrite {
		t.Errorf("code = %q, want %q", refusal.Code(), gitprovider.CodeMainFrozenDirectWrite)
	}
	if len(port.calls) != 0 {
		t.Errorf("provider calls = %v, want none (the refusal precedes the provider action)", port.calls)
	}
	for _, c := range store.calls {
		if c == "ingest" {
			t.Fatalf("store calls = %v, want no ingestion", store.calls)
		}
	}
}

func TestIngestAllowsMainPushToUnfrozenProject(t *testing.T) {
	// The instrument can say no AND yes: the same delivery, in a project
	// whose main is not frozen, runs the ordinary pipeline end to end.
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded}},
		files:        map[string][]byte{testSHA2 + ":manifests/mat.json": []byte(validMaterialDoc())},
	}
	store := &fakeIngestStore{inserted: true}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), mainPushEvent(testSHA2))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	if len(port.calls) == 0 {
		t.Error("provider was never asked for the diff")
	}
	if len(store.params) != 1 {
		t.Fatalf("store handoffs = %d, want 1", len(store.params))
	}
}

func TestIngestRefusesMainDeletionInFrozenProject(t *testing.T) {
	// Deleting main is not a smaller write than pushing to it, and the
	// check sits above the zeros-after branch for exactly that reason.
	port := &fakePort{changedFilesErr: errors.New("must not be called")}
	store := &fakeIngestStore{inserted: true, mainFrozen: true}
	ingester := testIngester(t, port, store)

	_, err := ingester.Ingest(context.Background(), mainPushEvent(gitprovider.ZerosSHA))
	var refusal *gitprovider.MainFrozenRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *MainFrozenRefusalError", err)
	}
	for _, c := range store.calls {
		if c == "ingest" {
			t.Fatalf("store calls = %v, want no ingestion", store.calls)
		}
	}
}

func TestIngestResearchBranchUnaffectedByFreeze(t *testing.T) {
	// projects.main_frozen is a project-level switch about MAIN: a research
	// branch push into the same frozen project ingests exactly as before.
	port := &fakePort{
		changedFiles: []gitprovider.FileChange{{Path: "manifests/mat.json", Kind: gitprovider.ChangeAdded}},
		files:        map[string][]byte{testSHA2 + ":manifests/mat.json": []byte(validMaterialDoc())},
	}
	store := &fakeIngestStore{inserted: true, mainFrozen: true}
	ingester := testIngester(t, port, store)

	inserted, err := ingester.Ingest(context.Background(), pushEvent(testSHA2))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	if len(store.params) != 1 {
		t.Fatalf("store handoffs = %d, want 1", len(store.params))
	}
}
