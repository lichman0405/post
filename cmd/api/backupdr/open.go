package backupdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
)

// The five opens of docs/37 §V1 验收, verbatim:
//
//	必须至少做一次空环境 restore test，并成功打开 Seed Project、Release、
//	Asset、Files、Evidence Graph
//
// "成功打开" means a read that returns the object. It does NOT mean the
// process started, the database accepted a connection, or a table exists —
// so every open below is a read through a real product read path that ends
// in a non-empty answer, and an open that returns nothing is a FAILED open,
// not a quieter kind of pass. The drill reports all five individually; four
// out of five is a fail.
//
// # Which layer each open goes through, and why
//
// The five do not have five equivalent read paths in the product, so the
// drill does not pretend they do. Each OpenedObject carries Via, naming the
// layer that actually answered, so a reviewer can see exactly how strong
// each open is rather than reading "all five opened" as five equal claims:
//
//	Seed Project    projects.Service.Get — the service the project API
//	                calls, with the read authorization evaluated by the
//	                real policy engine and the restored owner's membership
//	                as the reader. A project the restored environment
//	                cannot authorize a read of is not an opened project.
//	Release         releases.Command.Manifest — the product's manifest
//	                EXPORT, which re-verifies the stored document against
//	                its manifest_hash and REFUSES to serve a release whose
//	                content no longer verifies. The strongest of the five:
//	                it fails closed on drift.
//	Files           gitprovider.FilesReader over the real GiteaAdapter —
//	                the Files API's own service, reading the restored
//	                repository over the Git protocol.
//	Asset           the stored research_asset_versions row, verified with
//	                the publish gate's own hash rule (assets.ManifestHash).
//	                No read route for an asset version exists yet
//	                (cmd/api/assetshttp wires the publish paths only), so
//	                there is no service to call; the drill says so instead
//	                of inventing a route.
//	Evidence Graph  resolutions.PGStore.ListEvidenceForObjectVersion — the
//	                conflict-resolution package's evidence read, which is
//	                the product's only reader of evidence_assertions. There
//	                is no Evidence Graph endpoint at all (T0506 is
//	                unlanded), so the store is the whole surface that
//	                exists.
//
// Nothing here writes. Every call is a read on a restored environment.

// The five object kinds, spelled as docs/37 §V1 验收 spells them.
const (
	ObjectSeedProject   = "Seed Project"
	ObjectRelease       = "Release"
	ObjectAsset         = "Asset"
	ObjectFiles         = "Files"
	ObjectEvidenceGraph = "Evidence Graph"
)

// RequiredObjects is all five, in the order the document names them.
var RequiredObjects = []string{ObjectSeedProject, ObjectRelease, ObjectAsset, ObjectFiles, ObjectEvidenceGraph}

// OpenedObject is one open's outcome. Opened is the verdict and Detail is
// what came back — the drill refuses to call an open successful without
// naming the object it got, because "the call returned no error" is what a
// read of an empty environment also produces.
type OpenedObject struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Via    string `json:"via"`
	Opened bool   `json:"opened"`
	Detail string `json:"detail"`
}

// Opened is the whole set, with the one summary the acceptance criterion
// is actually about.
type Opened struct {
	Objects []OpenedObject `json:"objects"`
}

// AllOpened reports whether all five opened. Four is not a pass
// (docs/37 §V1 验收 names five).
func (o Opened) AllOpened() bool {
	got := map[string]bool{}
	for _, obj := range o.Objects {
		if obj.Opened {
			got[obj.Kind] = true
		}
	}
	for _, kind := range RequiredObjects {
		if !got[kind] {
			return false
		}
	}
	return true
}

// Missing names the objects that did not open.
func (o Opened) Missing() []string {
	got := map[string]bool{}
	for _, obj := range o.Objects {
		if obj.Opened {
			got[obj.Kind] = true
		}
	}
	var out []string
	for _, kind := range RequiredObjects {
		if !got[kind] {
			out = append(out, kind)
		}
	}
	return out
}

// OpenParams carries the credential mintToken produced for this restore.
// It is passed in rather than minted here because the caller is the one
// that revokes it: an open that minted its own token would leave a
// credential behind on the failure path.
type OpenParams struct {
	ProjectID  string
	ReleaseID  string
	AssetID    string
	GiteaToken string
	FilesOwner string
	FilesRepo  string
}

// OpenFive opens all five objects on the restored environment.
//
// A failure to open one object does not abort the others: the acceptance
// criterion is about all five, and a drill that stopped at the first
// failure would report one missing object when the useful answer is which
// of the five actually work. Every open is therefore attempted, and the
// caller checks AllOpened.
func OpenFive(ctx context.Context, cfg Config, pool *pgxpool.Pool, p OpenParams) (Opened, error) {
	var out Opened

	// --- Seed Project -------------------------------------------------
	projOpen := OpenedObject{
		Kind: ObjectSeedProject,
		ID:   p.ProjectID,
		Via:  "projects.Service.Get (read authorization through the real policy engine)",
	}
	project, projErr := openProject(ctx, pool, p.ProjectID)
	switch {
	case projErr != nil:
		projOpen.Detail = projErr.Error()
	default:
		projOpen.Opened = true
		projOpen.Detail = fmt.Sprintf("slug=%s name=%q visibility=%s created_at=%s",
			project.Slug, project.Name, project.Visibility, project.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	out.Objects = append(out.Objects, projOpen)

	// --- Release ------------------------------------------------------
	relOpen := OpenedObject{
		Kind: ObjectRelease,
		ID:   p.ReleaseID,
		Via:  "releases.Command.Manifest (the export, which refuses a document that does not verify)",
	}
	manifest, relErr := openRelease(ctx, pool, p.ProjectID, p.ReleaseID)
	switch {
	case relErr != nil:
		relOpen.Detail = relErr.Error()
	default:
		relOpen.Opened = true
		relOpen.Detail = fmt.Sprintf("%d bytes of canonical manifest document, sha256(manifest)=%s",
			len(manifest), sha256Hex(manifest)[:16])
	}
	out.Objects = append(out.Objects, relOpen)

	// --- Asset --------------------------------------------------------
	assetOpen := OpenedObject{
		Kind: ObjectAsset,
		ID:   p.AssetID,
		Via:  "research_asset_versions row + assets.ManifestHash (no read route exists yet — assetshttp wires publish only)",
	}
	assetDetail, assetErr := openAsset(ctx, pool, p.AssetID)
	switch {
	case assetErr != nil:
		assetOpen.Detail = assetErr.Error()
	default:
		assetOpen.Opened = true
		assetOpen.Detail = assetDetail
	}
	out.Objects = append(out.Objects, assetOpen)

	// --- Files --------------------------------------------------------
	filesOpen := OpenedObject{
		Kind: ObjectFiles,
		ID:   p.FilesOwner + "/" + p.FilesRepo,
		Via:  "gitprovider.FilesReader over the real GiteaAdapter (the Files API's own service)",
	}
	filesDetail, filesErr := openFiles(ctx, cfg, pool, p)
	switch {
	case filesErr != nil:
		filesOpen.Detail = filesErr.Error()
	default:
		filesOpen.Opened = true
		filesOpen.Detail = filesDetail
	}
	out.Objects = append(out.Objects, filesOpen)

	// --- Evidence Graph -----------------------------------------------
	evOpen := OpenedObject{
		Kind: ObjectEvidenceGraph,
		ID:   p.ReleaseID,
		Via:  "resolutions.PGStore.ListEvidenceForObjectVersion (the product's only evidence_assertions reader; T0506 has no endpoint)",
	}
	evDetail, evErr := openEvidenceGraph(ctx, pool, p.ReleaseID)
	switch {
	case evErr != nil:
		evOpen.Detail = evErr.Error()
	default:
		evOpen.Opened = true
		evOpen.Detail = evDetail
	}
	out.Objects = append(out.Objects, evOpen)

	return out, nil
}

// openProject reads the project the way the project API reads it: through
// projects.Service, with the restored owner's membership as the reader.
//
// The reader is the restored OWNER, resolved from the restored membership
// table — not a synthetic superuser. A drill that opened the project as an
// all-powerful identity would not be testing whether the restore produced a
// project anyone can read.
func openProject(ctx context.Context, pool *pgxpool.Pool, projectID string) (projectView, error) {
	if projectID == "" {
		return projectView{}, fmt.Errorf("seed project: no project id to open")
	}
	var ownerID string
	if err := pool.QueryRow(ctx, `
		SELECT user_id::text FROM project_memberships
		 WHERE project_id = $1 AND role = 'owner'
		 ORDER BY user_id LIMIT 1`, projectID).Scan(&ownerID); err != nil {
		return projectView{}, fmt.Errorf("seed project: resolve the restored owner: %w", err)
	}
	svc := projects.NewService(persistence.NewProjectStore(pool), nil, authz.NewMatrixEngine())
	project, err := svc.Get(ctx, projects.Reader{UserID: ownerID, Authenticated: true}, projectID)
	if err != nil {
		return projectView{}, fmt.Errorf("seed project: %w", err)
	}
	return projectView{
		Slug:       project.Slug,
		Name:       project.Name,
		Visibility: string(project.Visibility),
		CreatedAt:  project.CreatedAt,
	}, nil
}

// projectView is the slice of domain.Project an open records. It exists so
// open.go does not have to name the domain type in its signatures, and so
// the recorded detail is a deliberate choice rather than whatever the
// domain struct happens to carry.
type projectView struct {
	Slug       string
	Name       string
	Visibility string
	CreatedAt  time.Time
}

// openRelease reads the release through the product's manifest export.
//
// The Command is wired with the real release store and nothing else. The
// write-path ports are left nil ON PURPOSE: Command.Manifest and Command.Get
// reach only the store (their doc says visibility was resolved by the
// caller), so every write method on this Command would nil-panic rather
// than write. That is the property the drill wants — the opener has no
// working write path at all — and it is why this is not "half-wiring" but
// the minimum surface a read needs.
func openRelease(ctx context.Context, pool *pgxpool.Pool, projectID, releaseID string) ([]byte, error) {
	if projectID == "" || releaseID == "" {
		return nil, fmt.Errorf("release: no release to open")
	}
	cmd := releases.NewCommand(nil, nil, nil, nil, nil, nil, persistence.NewReleaseStore(pool), nil)
	raw, err := cmd.Manifest(ctx, projectID, releaseID)
	if err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("release: the export returned an empty document")
	}
	return raw, nil
}

// openAsset reads the stored asset version and re-derives its integrity
// hash with the publish gate's own rule.
//
// Both halves matter and neither is enough alone: reading the row proves
// the restore carried it, and re-deriving the hash proves the bytes that
// arrived are the bytes that were published. A row whose manifest no longer
// digests to its integrity_hash is an open FAILURE — the object is present
// and unusable.
func openAsset(ctx context.Context, pool *pgxpool.Pool, assetID string) (string, error) {
	if assetID == "" {
		return "", fmt.Errorf("asset: no asset version to open")
	}
	var raw []byte
	var integrity string
	var pid string
	if err := pool.QueryRow(ctx,
		`SELECT v.manifest::text, v.integrity_hash, a.pid
		   FROM research_asset_versions v
		   JOIN research_assets a ON a.id = v.asset_id
		  WHERE v.id = $1`,
		assetID).Scan(&raw, &integrity, &pid); err != nil {
		return "", fmt.Errorf("asset: read stored version: %w", err)
	}
	// The manifest is read back as the stored bytes and hashed through the
	// assets package's own rule (assets.ManifestHash), not through a
	// re-rendering invented here: the digest is the product's, so a fixture
	// or a restore that produced a subtly different document fails this
	// check instead of passing it.
	got, err := assetManifestHash(raw)
	if err != nil {
		return "", fmt.Errorf("asset: %w", err)
	}
	if got != integrity {
		return "", fmt.Errorf("asset: the stored manifest digests to %s but the row records integrity_hash %s — "+
			"the object is present and does not verify", got, integrity)
	}
	return fmt.Sprintf("pid=%s integrity_hash=%s verified against the stored document (%d bytes)",
		pid, integrity, len(raw)), nil
}

// How long a provider read that came back "could not answer" is retried,
// and how long between tries. Small and bounded: this covers the settling
// window a push creates, not an outage. An outage still fails the open —
// eight tries over ~12s is a fraction of the drill's own budget.
const (
	providerReadAttempts = 8
	providerReadWait     = 1500 * time.Millisecond
)

// retryUnavailable runs one provider read, retrying ONLY the failure the
// port calls "the provider could not answer" (ErrUnavailable — what every
// 5xx maps to) and returning at once on anything else.
//
// Why a restore needs this, and why it is not a weakened assertion. A
// restore pushes every ref into a freshly created repository, and Gitea
// answers 500 to a CONTENTS read for a short window after that push while
// its own per-file metadata catches up. Measured, not assumed: the same
// request, with the same credential, on the same repository answers 500
// immediately after the restore and 200 moments later — and the /git/trees
// route (which reads raw git rather than Gitea's per-file rows) answers 200
// throughout, which is how the window was localised. Both alternatives are
// worse than retrying: retrying a 404 would turn "this file is not in the
// restored repository" into a timeout, and not retrying makes the Files
// open a coin flip on a race the restore itself creates. What must not
// move is the outcome — a read still unavailable after the bound FAILS the
// open, and the number of tries is reported.
func retryUnavailable(ctx context.Context, fn func() error) (int, error) {
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return attempt, nil
		}
		if !errors.Is(err, gitprovider.ErrUnavailable) || attempt >= providerReadAttempts {
			return attempt, err
		}
		select {
		case <-ctx.Done():
			return attempt, fmt.Errorf("%w while waiting for the provider (last answer: %v)", ctx.Err(), err)
		case <-time.After(providerReadWait):
		}
	}
}

// openFiles reads the restored repository through the Files API's own
// service, which resolves the repository through git_repository_provisions
// and then reads it over the Git protocol.
//
// This is the open that exercises the restore's coordinate rebinding: the
// provisions row was repointed at the drill's own owner, so a Files read
// that succeeds proves the rebinding is complete — a half-rebound database
// would resolve a repository that does not exist. It requires a non-empty
// directory AND a readable file, because a tree listing of nothing is what
// an empty repository returns.
func openFiles(ctx context.Context, cfg Config, pool *pgxpool.Pool, p OpenParams) (string, error) {
	if p.GiteaToken == "" {
		return "", fmt.Errorf("files: no provider token for the restored Git infrastructure")
	}
	adapter := gitprovider.NewGiteaAdapter(gitprovider.Config{
		BaseURL: cfg.Target.GiteaBaseURL,
		Token:   config.Secret(p.GiteaToken),
	})
	reader := gitprovider.NewFilesReader(adapter, gitprovider.NewUserAccessStore(pool))

	var tree gitprovider.TreeListing
	treeTries, err := retryUnavailable(ctx, func() error {
		var e error
		tree, e = reader.Tree(ctx, p.ProjectID, "", "")
		return e
	})
	if err != nil {
		return "", fmt.Errorf("files: list the repository root (%d tries): %w", treeTries, err)
	}
	if len(tree.Entries) == 0 {
		return "", fmt.Errorf("files: the restored repository's root is empty (tree sha %q)", tree.SHA)
	}

	var names []string
	var firstFile string
	for _, e := range tree.Entries {
		names = append(names, e.Name)
		if firstFile == "" && e.Type == "blob" {
			firstFile = e.Path
		}
	}
	sort.Strings(names)
	if firstFile == "" {
		return "", fmt.Errorf("files: the restored repository has no file at its root (entries: %s)",
			strings.Join(names, ", "))
	}
	var view gitprovider.FileView
	fileTries, err := retryUnavailable(ctx, func() error {
		var e error
		view, e = reader.File(ctx, p.ProjectID, "", firstFile)
		return e
	})
	if err != nil {
		return "", fmt.Errorf("files: read %s (%d tries): %w", firstFile, fileTries, err)
	}
	if view.SHA == "" || view.Size == 0 {
		return "", fmt.Errorf("files: read %s returned no content (sha %q, %d bytes)", firstFile, view.SHA, view.Size)
	}
	return fmt.Sprintf("tree %s at main: %d entries [%s]; read %s = %d bytes sha %s (kind %s); "+
		"provider tries: tree %d, file %d",
		tree.SHA, len(tree.Entries), strings.Join(names, ", "),
		firstFile, view.Size, view.SHA[:min(12, len(view.SHA))], view.Kind, treeTries, fileTries), nil
}

// evidencePin is the slice of a release manifest's embedded state content
// the Evidence Graph open needs: the object versions the released state
// contains. Reading them out of the release document is deliberate —
// the document is the pinned snapshot, so the evidence read is asked about
// the objects the release actually names, not about whatever the current
// database happens to hold.
type evidencePin struct {
	ObjectVersions []struct {
		ID string `json:"id"`
	} `json:"object_versions"`
}

// openEvidenceGraph reads the evidence assertions of the released state's
// objects through the product's only evidence reader.
//
// The object version ids come from the restored release document itself, so
// this open is also a second, independent proof that the restored release
// row and the restored evidence rows describe the same snapshot.
func openEvidenceGraph(ctx context.Context, pool *pgxpool.Pool, releaseID string) (string, error) {
	if releaseID == "" {
		return "", fmt.Errorf("evidence graph: no release to anchor the read")
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT manifest::text FROM releases WHERE id = $1`, releaseID).Scan(&raw); err != nil {
		return "", fmt.Errorf("evidence graph: read the release document: %w", err)
	}
	pin, ok := parseStatePin(raw)
	if !ok {
		return "", fmt.Errorf("evidence graph: the release document carries no state content to anchor an evidence read")
	}
	var versions []string
	var doc struct {
		State struct {
			Content evidencePin `json:"content"`
		} `json:"state"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("evidence graph: parse the release document: %w", err)
	}
	for _, ov := range doc.State.Content.ObjectVersions {
		if ov.ID != "" {
			versions = append(versions, ov.ID)
		}
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("evidence graph: the released state (git_ref %s) names no object versions",
			derefOr(pin.GitRef, "none"))
	}

	store := resolutions.NewPGStore(pool)
	total := 0
	for _, v := range versions {
		items, err := store.ListEvidenceForObjectVersion(ctx, v)
		if err != nil {
			return "", fmt.Errorf("evidence graph: read the assertions of object version %s: %w", v, err)
		}
		total += len(items)
	}
	if total == 0 {
		return "", fmt.Errorf("evidence graph: the read path works but returned no assertions for %d released "+
			"object versions — an empty graph is not an opened graph", len(versions))
	}
	return fmt.Sprintf("%d assertions over %d released object versions (evidence_assertions via resolutions.PGStore)",
		total, len(versions)), nil
}

func derefOr(s *string, def string) string {
	if s == nil || *s == "" {
		return def
	}
	return *s
}
