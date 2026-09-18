package gitprovider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Push ingestion (T0305): the platform half of the push webhook — verify
// the provider's HMAC signature, inspect the changed files of the push
// over the git protocol (the payload's commit list is truncated by the
// provider — checked against the running instance), classify each changed
// file (known scientific manifest vs unstructured), and record the
// delivery plus the candidate semantic diff through IngestStore. The
// store's dedupe key makes redelivered webhooks a no-op: the acceptance
// criterion "重复 webhook 不重复 state" holds at the database level, not
// by caller discipline.
//
// The canonical receiver path is POST /api/v1/git/hooks/gitea
// (the handler lives in internal/gitprovider/push_ingestion_http.go,
// wired into the API server in cmd/api/main.go — outside the session
// guard, because the HMAC signature IS the authentication). The provider
// signs the raw body with the per-repository secret T0301 stored in
// git_repository_provisions: X-Gitea-Signature = hex(hmac-sha256(secret,
// raw body)) (checked against the running instance).

// ZerosSHA is the all-zero object id the provider uses for "no commit":
// before is zeros when the push created the ref, after is zeros when the
// push deleted it (this instance delivers ref deletions as its own
// "delete" event, so a zeros after never arrives on a push hook — the
// receiver still tolerates it).
const ZerosSHA = "0000000000000000000000000000000000000000"

// isZerosSHA reports whether s is the provider's "no commit" marker.
func isZerosSHA(s string) bool { return s == ZerosSHA }

// VerifyPushSignature checks the provider's delivery signature in constant
// time: hex(hmac-sha256(secret, raw body)) against the per-repository
// secret. The body must be the RAW bytes the provider signed — parsing
// must not happen first, and the signature check must not compare on
// anything but those bytes.
func VerifyPushSignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// PushCommit is the slice of the delivered commit list the ingestion keeps
// for audit (the provider already bounds the list).
type PushCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Author  string `json:"author_login"`
}

// PushEvent is the parsed, delivery-shaped slice of a push webhook the
// ingestion consumes. It is what the provider signs — ParsePushEvent maps
// the provider's payload onto it and validates the fields the ingestion
// depends on (ref shape, full commit SHAs).
type PushEvent struct {
	// Ref is the pushed ref, refs/heads/<name> for branch pushes (the
	// only kind this instance delivers on a push hook).
	Ref string
	// Before/After are the full commit SHAs the push moved the ref
	// between; zeros means "no commit on that side".
	Before string
	After  string
	// RepositoryID is the provider's numeric repository id — the delivery
	// is mapped to its canonical project through it
	// (git_repository_provisions.gitea_repo_id).
	RepositoryID int64
	// Owner and Name are the provider-side repository coordinates, for
	// the git-protocol calls only (never identity).
	Owner string
	Name  string
	// Pusher is the provider login that pushed (audit only — identity
	// mapping to platform users is T0304's).
	Pusher string
	// TotalCommits is the provider's own commit count; it may exceed the
	// delivered Commits list.
	TotalCommits int
	// Commits is the delivered commit list, as the provider bounded it.
	Commits []PushCommit
	// DeliveryID is the provider's per-attempt delivery id
	// (X-Gitea-Delivery) — correlation only, never the dedupe key.
	DeliveryID string
}

// pushPayload is the provider's push delivery shape the receiver parses
// (a subset of Gitea's payload, checked against the running instance).
type pushPayload struct {
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
	Total  int    `json:"total_commits"`
	Pusher struct {
		Login string `json:"login"`
	} `json:"pusher"`
	Repository struct {
		ID    int64 `json:"id"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
	Commits []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
		Author  struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"commits"`
}

// ErrNotABranchPush reports a delivery whose ref is not a branch ref: the
// delivery is legal, the ingestion simply has nothing to do for it.
var ErrNotABranchPush = errors.New("gitprovider: not a branch push")

// ParsePushEvent parses one delivery body. The body must be the raw bytes
// the provider signed (the caller verifies the signature separately).
// ErrNotABranchPush is returned TOGETHER with the parsed event for a ref
// outside refs/heads/: the receiver still verifies the delivery's
// signature (using the event's repository id) before it decides there is
// nothing to ingest. A plain error for malformed JSON or a payload whose
// SHAs are not full commit ids.
func ParsePushEvent(body []byte, deliveryID string) (PushEvent, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return PushEvent{}, fmt.Errorf("gitprovider: parse push payload: %w", err)
	}
	ev := PushEvent{
		Ref:          p.Ref,
		Before:       p.Before,
		After:        p.After,
		RepositoryID: p.Repository.ID,
		Owner:        p.Repository.Owner.Login,
		Name:         p.Repository.Name,
		Pusher:       p.Pusher.Login,
		TotalCommits: p.Total,
		DeliveryID:   deliveryID,
	}
	for _, c := range p.Commits {
		ev.Commits = append(ev.Commits, PushCommit{ID: c.ID, Message: c.Message, Author: c.Author.Login})
	}
	if !strings.HasPrefix(p.Ref, "refs/heads/") {
		return ev, fmt.Errorf("%w: %s", ErrNotABranchPush, p.Ref)
	}
	if !isZerosSHA(p.Before) && !isFullSHA(p.Before) {
		return PushEvent{}, fmt.Errorf("gitprovider: payload before is not a full commit sha")
	}
	if !isZerosSHA(p.After) && !isFullSHA(p.After) {
		return PushEvent{}, fmt.Errorf("gitprovider: payload after is not a full commit sha")
	}
	return ev, nil
}

// FileKind classifies one changed file of a push.
type FileKind string

// The two classifications the inspection produces: a file whose content
// validated against a known scientific schema, or everything else (kept —
// T0306's unstructured change state — but not a candidate).
const (
	FileKindManifest     FileKind = "semantic_manifest"
	FileKindUnstructured FileKind = "unstructured"
)

// ClassifiedChange is one changed path with its inspection result.
type ClassifiedChange struct {
	Path string
	Kind ChangeKind
	// File is the classification: semantic_manifest when the content
	// matched a known scientific schema.
	File FileKind
	// SchemaID is the matched schema's registry id (File=manifest only).
	SchemaID string
	// ContentSHA256 is the sha256 hex of the content the classification
	// inspected (File=manifest only; the pushed content for added and
	// modified, the pre-push content for removed).
	ContentSHA256 string
}

// SemanticCandidate is one entry of the candidate RSG diff: a changed
// manifest whose content matched a known scientific schema. The candidate
// is the PROPOSAL the push implies — create, update or delete of the
// manifest's content — awaiting the semantic validation pipeline; T0305
// never applies it.
type SemanticCandidate struct {
	Path     string
	Kind     ChangeKind
	SchemaID string
	// Content is the manifest's raw JSON (the pushed content for added
	// and modified, the pre-push content for removed) — the proposal the
	// candidate carries.
	Content json.RawMessage
}

// MainPushVerdict is the store's answer for one delivery that moves
// refs/heads/main: the two canonical facts the Git side of the frozen-main
// rule (T0601) is decided on.
type MainPushVerdict struct {
	// Frozen is projects.main_frozen for the project the provider
	// repository is provisioned for.
	Frozen bool
	// PlatformMerge reports whether the delivery's after commit is the
	// merge commit the platform recorded for that project's main — the
	// newest semantic merge whose Git step completed on refs/heads/main
	// (T0406's record, written by CompleteGitStep). It is the ONLY main
	// delivery a frozen project still accepts, and it is false for
	// everything else — an unrecorded merge, an older recorded one, a
	// repository mapping to no project, a commit the platform never
	// merged.
	PlatformMerge bool
}

// IngestStore is the canonical-store port push ingestion writes through.
// The concrete adapter is *PGPushIngestStore in this package.
type IngestStore interface {
	// WebhookSecretByRepoID returns the provisioned repository's webhook
	// HMAC secret (T0301's stored value — the only authority, the
	// provider never returns it). ErrNotFound when no provision row
	// carries the repository id.
	WebhookSecretByRepoID(ctx context.Context, giteaRepoID int64) (string, error)
	// MainPushVerdict answers the frozen-main rule for one delivery that
	// moves refs/heads/main (T0601): whether the project's main is frozen,
	// and whether afterSHA is the merge commit the platform itself
	// recorded for that main. The ingestion asks both questions in ONE
	// read, before it looks at the delivery at all, and refuses when the
	// answer is frozen and not the platform's own merge.
	//
	// A repository id that maps to no provision row answers the zero
	// verdict: an unprovisioned delivery has no project whose main could
	// be frozen, and T0305's existing handling of it (recorded, mapped to
	// no branch) is unchanged by this rule.
	MainPushVerdict(ctx context.Context, giteaRepoID int64, afterSHA string) (MainPushVerdict, error)
	// IngestPush records one delivery and its inspection result in one
	// transaction: the ingestion row (dedupe on repository + ref +
	// after — a duplicate is a complete no-op and reports inserted
	// false), the changed-file rows, the candidate rows, the branch
	// ref's head pointer (guarded — it advances only when it is still
	// where the push started, or while the ref is still unborn for a
	// creation push; a refused advance is recorded on the row as
	// head_skip_reason = 'stale_before' or 'stale_creation'), and the
	// pushed head as a project state.
	IngestPush(ctx context.Context, in IngestPushParams) (inserted bool, err error)
	// SourceLineEvidence returns the source ref's own change evidence at
	// headSHA (T0804): per path, the nearest-to-head record of the pushes
	// ingested on that ref, walked back from headSHA exactly as that
	// ref's own semantic flag is derived — plus the candidate content
	// those records proposed. The fork import records it as the copy's
	// evidence when the source ref has no recorded fork point to diff
	// against, so the copied branch's flag can never be cleaner than the
	// line it copies.
	//
	// A ref whose head carries no ingested delivery answers no records —
	// the same missing evidence the derivation itself reports, never a
	// search for another head to walk from. A failed read is an error:
	// the answer is a claim about the canonical record, and a failed
	// read makes no claim.
	SourceLineEvidence(ctx context.Context, giteaRepoID int64, gitRef, headSHA string) ([]ClassifiedChange, []SemanticCandidate, error)
}

// IngestPushParams carries one delivery's inspection result into the
// store. The store derives project, branch and parent state itself — the
// service passes facts only.
type IngestPushParams struct {
	Event      PushEvent
	Changes    []ClassifiedChange
	Candidates []SemanticCandidate
}

// PushIngester is the push ingestion application service: all policy lives
// here — signature verification (stateless, exposed for the receiver), the
// git-diff inspection, the manifest classification — and every canonical
// fact goes through IngestStore.
type PushIngester struct {
	port  GitPort
	store IngestStore
	reg   *schemareg.Registry
}

// NewPushIngester wires the ingester. reg is the canonical schema registry
// (the same instance the validation surface uses) — manifest
// classification validates against it, never against a private copy.
func NewPushIngester(port GitPort, store IngestStore, reg *schemareg.Registry) *PushIngester {
	return &PushIngester{port: port, store: store, reg: reg}
}

// Ingest processes one VERIFIED delivery: inspect → classify → record.
// Idempotency lives in the store's dedupe key; a redelivered webhook
// returns (false, nil) and writes nothing. A zeros after (a ref-deletion
// delivery) is recorded without inspection — there is no pushed head to
// inspect and the provider deletes refs through its own delete event on
// this instance, so this path is defensive.
//
// A delivery that moves refs/heads/main into a FROZEN project is refused
// before anything else happens (T0601, MainFrozenRefusalError): before the
// provider is asked for the changed files, before any file is read, and
// before the store writes a row. The Git-side rule docs/09 §3 states —
// main advances only through a Research PR merge — is enforced on both
// sides of the platform: the semantic write side refuses in the states
// adapter's transaction (MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN), and this is
// the Git side. The refusal is the delivery's whole outcome: nothing is
// recorded for it, not even as a violation, because the platform has no
// legitimate reading of a direct write to a frozen main to record.
//
// EXCEPT the platform's own merge, which reaches this endpoint as a plain
// push. The provider delivers the merge it just performed as a push of
// refs/heads/main whose after is the merge commit and whose payload carries
// no merge marker at all (T0409's MergePullRequest is an ordinary provider
// merge — checked against the running instance), so a rule that refuses
// every main delivery while frozen refuses the governed path too: main's
// recorded head, the pushed-head state and the branch's semantic marks
// would never be written, and T0309's reconciler would report the ref
// having moved with no ingestion behind it — `ref_head_moved` drift the
// platform caused itself, on every merge, for good. The seam is exactly one
// delivery wide and it is pinned to a RECORD, never to a shape: a main
// delivery is admitted while frozen only when its after is the merge commit
// this project's main has recorded (MainPushVerdict.PlatformMerge). A
// foreign direct push, a merge performed outside the platform (the
// provider's own UI/API writes no record here), a push back onto an older
// recorded merge commit, and a delivery whose after is anything else are
// all refused — no record, no seam, and the store answers false whenever it
// cannot prove the match.
//
// The check covers a main deletion too (a zeros after), which is why it
// sits above that branch: deleting main is not a smaller write than
// pushing to it, and zeros is no commit any merge recorded.
func (i *PushIngester) Ingest(ctx context.Context, ev PushEvent) (bool, error) {
	if err := i.refuseFrozenMain(ctx, ev); err != nil {
		return false, err
	}
	params := IngestPushParams{Event: ev}
	if isZerosSHA(ev.After) {
		return i.store.IngestPush(ctx, params)
	}
	base := ev.Before
	if isZerosSHA(base) {
		base = ""
	}
	repo := Repository{Owner: ev.Owner, Name: ev.Name, ID: ev.RepositoryID}
	changes, candidates, err := i.Inspect(ctx, repo, base, ev.After)
	if err != nil {
		return false, err
	}
	params.Changes, params.Candidates = changes, candidates
	return i.store.IngestPush(ctx, params)
}

// refuseFrozenMain applies the T0601 rule to a delivery: a delivery that
// moves refs/heads/main into a frozen project is refused before anything
// else happens (see Ingest's documentation for the whole rule and the
// one-delivery seam for the platform's own merges). It is shared with
// IngestCopy so the copy path cannot classify a main delivery differently
// from the push path.
func (i *PushIngester) refuseFrozenMain(ctx context.Context, ev PushEvent) error {
	if ev.Ref != MainRef {
		return nil
	}
	verdict, err := i.store.MainPushVerdict(ctx, ev.RepositoryID, ev.After)
	if err != nil {
		return err
	}
	if verdict.Frozen && !verdict.PlatformMerge {
		return &MainFrozenRefusalError{Ref: ev.Ref, RepositoryID: ev.RepositoryID}
	}
	return nil
}

// Inspect is the content half of an ingestion: the changed set of
// base..head in repo (the whole tree when base is empty — a ref's first
// push), every path classified against the canonical registry by
// classify. It is the ONE implementation of "look at the content that
// arrived" in this package; the push path (Ingest) and the fork import
// (IngestCopy) both call it, so the two cannot drift into two checks.
func (i *PushIngester) Inspect(ctx context.Context, repo Repository, base, head string) ([]ClassifiedChange, []SemanticCandidate, error) {
	changes, err := i.port.ChangedFiles(ctx, repo, base, head)
	if err != nil {
		return nil, nil, err
	}
	classified := make([]ClassifiedChange, 0, len(changes))
	var candidates []SemanticCandidate
	for _, c := range changes {
		change, content := i.classify(ctx, repo, base, head, c)
		classified = append(classified, change)
		if change.File == FileKindManifest {
			candidates = append(candidates, SemanticCandidate{
				Path:     change.Path,
				Kind:     change.Kind,
				SchemaID: change.SchemaID,
				Content:  json.RawMessage(content),
			})
		}
	}
	return classified, candidates, nil
}

// CopySource names the content a fork's copy came from (T0804): the
// repository the source commit lives in, the ref it is on, and the fork
// point the platform has recorded for that ref. The copied commit itself is
// the delivery's after — one fact, spelled once.
type CopySource struct {
	// Repository is the source repository: where the inspection runs, and
	// where the evidence of a copy with no recorded fork point is read.
	// A fork's commit is one object reachable through two repositories.
	Repository Repository
	// Ref is the source line's ref, refs/heads/<name>, inside that
	// repository.
	Ref string
	// ForkPoint is the source ref's recorded fork point
	// (git_branch_refs.fork_sha): the state the source line itself
	// diverged from, and therefore the baseline the copy's divergence is
	// diffed against. Empty when the platform has none recorded.
	ForkPoint string
}

// IngestCopy records a delivery whose content was inspected in ANOTHER
// repository (T0804's fork import).
//
// A fork's branch is a copy of a commit that lives in the repository it
// was copied from: the same object, reachable through two repositories.
// Diffing the copy against the fork's own empty history would say every
// path in the tree is new — including the platform's own project
// bootstrap file, which no ingestion ever wrote and which no later push
// can ever remove, so the branch's semantic flag would stick at
// unstructured_changes forever (docs/16 §4.1 forbids exactly that). What
// the copy CONTRIBUTES is its divergence from the line it copies.
//
// Where that divergence is measured from is the whole of this function,
// and the invariant is: at the moment of the import the copied branch's
// flag is never CLEANER than the source line's.
//
//   - src.ForkPoint recorded: the source line diverged from a state the
//     platform knows, so the copy's divergence is that state's content
//     diffed against ev.After — computed in content (the source
//     repository) by the same Inspect the push path uses, and recorded
//     against ev's repository and ref (the fork's).
//   - src.ForkPoint empty: there is no recorded baseline, and a copy
//     measured against nothing comes out CLEANER than the line it copies
//     — a line whose own flag is unstructured_changes (an unparseable
//     file pushed to it) would fork into a branch marked
//     semantic_complete, open a PR with that content and merge it. What
//     the copy carries instead is the source line's OWN evidence at the
//     copied commit: the per-path records its flag is derived from, read
//     by the same walk that derives it (IngestStore.SourceLineEvidence).
//     The copied branch's flag is then the source line's own judgement of
//     the same content — equal to it, never cleaner — and it is ordinary
//     evidence, so the rule every push obeys clears it: a later push
//     whose record for the same path is nearer to the head.
//
// What the no-fork-point route does NOT carry is content the platform
// never ingested: a source head no delivery recorded (a webhook that has
// not arrived, a ref adopted outside the platform) yields no records, and
// the copy derives as every branch with missing evidence does
// (docs/16 §4.1: missing evidence is not evidence of unstructured
// content). See forkimport.go's header for the route and its residual.
//
// Ordering is the caller's contract (ForkImporter.Import): the row must
// be written before the copy lands. The dedupe key is (repository, ref,
// after) and all three are known before the copy, so the provider's own
// webhook for the copy — production's normal case — collapses onto this
// row instead of inspecting the copy's whole tree and poisoning the flag
// with the bootstrap file. Nothing about the recorded facts changes: the
// row names the same commit the copy puts on the ref.
func (i *PushIngester) IngestCopy(ctx context.Context, ev PushEvent, src CopySource) (bool, error) {
	if err := i.refuseFrozenMain(ctx, ev); err != nil {
		return false, err
	}
	params := IngestPushParams{Event: ev}
	if isZerosSHA(ev.After) {
		return i.store.IngestPush(ctx, params)
	}
	changes, candidates, err := i.copyEvidence(ctx, ev, src)
	if err != nil {
		return false, err
	}
	params.Changes, params.Candidates = changes, candidates
	return i.store.IngestPush(ctx, params)
}

// copyEvidence is what one copy contributes, as the change and candidate
// rows the delivery records. The recorded fork point (when the platform has
// one) is a content diff; without one it is the source line's own recorded
// evidence, so the copy cannot come out cleaner than its source.
func (i *PushIngester) copyEvidence(ctx context.Context, ev PushEvent, src CopySource) ([]ClassifiedChange, []SemanticCandidate, error) {
	if src.ForkPoint != "" {
		return i.Inspect(ctx, src.Repository, src.ForkPoint, ev.After)
	}
	return i.store.SourceLineEvidence(ctx, src.Repository.ID, src.Ref, ev.After)
}

// classify inspects one changed path: only files whose path ends in .json
// are read and validated against the known scientific schemas (the
// canonical manifest format is JSON — specs/schemas); everything else is
// unstructured without a provider read. A read failure, an oversize file,
// or content that matches no known schema is unstructured — the file is
// kept (T0306's retention), it is simply not a semantic candidate. The
// returned content is the manifest's raw JSON when classified (nil
// otherwise) — it becomes the candidate's proposed content.
func (i *PushIngester) classify(ctx context.Context, repo Repository, base, head string, c FileChange) (ClassifiedChange, []byte) {
	out := ClassifiedChange{Path: c.Path, Kind: c.Kind, File: FileKindUnstructured}
	if !strings.HasSuffix(c.Path, ".json") {
		return out, nil
	}
	sha := head
	if c.Kind == ChangeRemoved {
		sha = base
	}
	content, err := i.port.ReadFile(ctx, repo, sha, c.Path)
	if err != nil {
		return out, nil
	}
	schemaID, ok := classifyManifest(i.reg, content)
	if !ok {
		return out, nil
	}
	sum := sha256.Sum256(content)
	out.File = FileKindManifest
	out.SchemaID = schemaID
	out.ContentSHA256 = hex.EncodeToString(sum[:])
	return out, content
}

// manifestSchemaIDs are the registered schemas that identify a manifest
// WITHOUT a declared object type: the canonical state manifest itself and
// the two untyped domain shapes (relation's properties.type is a free
// string, not a const — the registry's TypeConst does not see it). They
// are the registry's canonical $ids (the shape the validation surface
// stores); tried in this order — first match wins.
var manifestSchemaIDs = []string{
	schemareg.CanonicalNamespace + "rsg-manifest.schema.json",
	schemareg.CanonicalNamespace + "evidence-assertion.schema.json",
	schemareg.CanonicalNamespace + "relation.schema.json",
}

// classifyManifest validates one JSON document against the known
// scientific schemas. A document declaring an object "type" must match the
// schema that pins that type (a document claiming to be a material that
// violates the material schema is NOT a manifest — it is broken); when no
// registered schema pins the declared type at all — the type is a free
// string the domain shapes carry, like a relation's "uses" — the document
// falls through to the untyped manifest list and matches by shape, which
// is how relation documents classify. A document without a type matches
// the untyped list directly. Everything else is not a known manifest.
func classifyManifest(reg *schemareg.Registry, doc []byte) (string, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(doc, &probe) == nil && probe.Type != "" {
		for _, ref := range reg.List() {
			if t, ok := reg.TypeConst(ref); ok && t == probe.Type {
				if reg.Validate(ref, doc) == nil {
					return ref.ID, true
				}
				return "", false
			}
		}
		// No registered schema pins this type: a free-string type, not a
		// contradicted one. Fall through and let the untyped shapes decide.
	}
	for _, id := range manifestSchemaIDs {
		if reg.Validate(schemareg.Ref{ID: id, Version: schemareg.CanonicalV1}, doc) == nil {
			return id, true
		}
	}
	return "", false
}

// GitStateHash derives the state identity of a git-pushed branch head:
// sha256 hex over the canonical JSON {"git_commit_sha": "<after>"}. It is
// deterministic (the same commit always maps to the same state, which is
// what makes a redelivered webhook collide instead of duplicating a
// state) and distinct from the domain's transition hashes by construction
// (it pins the commit, not a parent + operations pair).
func GitStateHash(afterSHA string) string {
	payload, _ := json.Marshal(struct {
		GitCommitSHA string `json:"git_commit_sha"`
	}{afterSHA})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
