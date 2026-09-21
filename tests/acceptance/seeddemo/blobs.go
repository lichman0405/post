package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pathFixtureBlob is the fourth path label, and the only one that writes
// straight to the database. It exists because this build has no blob write
// path at all: internal/storage is a port declaration with no adapter, no
// HTTP route uploads or attaches a blob, and no application service mentions
// one. The repo's own knowledge E2E test states the consequence and the
// remedy (tests/integration/knowledge_e2e_test.go, attachBlob): "The rows are
// fixture data — this build has no upload route — but they are REAL rows: the
// merge's integrity engine refuses a payload whose blob reference resolves to
// nothing in the proposal's manifest, so a fixture that skipped this would
// prove the chain merges only by not checking."
//
// Both the refusal and the requirement are real, and both are product code:
//
//   - internal/rsg/integrity's blobRefsResolve (blocking, at the PR gate)
//     refuses a payload naming a blob the proposal's manifest does not hold;
//   - internal/rsg/validation's requiredFields at GateMain (blocking) demands
//     a dataset's blob_ids and a calculation's output_blob_ids be non-empty
//     before the state is accepted into main.
//
// So a demo whose datasets and calculations carry no files could never merge
// into main, and one whose payloads name blobs that do not exist would be
// refused by the product's own integrity engine. The seed therefore writes
// the minimal fixture the product's checks accept — a blobs row whose
// content_hash and size_bytes are the sha256 and length of the file content
// the plan declares, plus the attachment that makes it part of the version's
// state manifest — and labels the path as a database write everywhere it
// appears: in the build report, in the RESULT and in ops/seed-demo.md. It is
// reported as a follow-up issue rather than hidden.
//
// What this deliberately does NOT do: invent bytes that are not in the plan.
// The content is examples/seed-demo/demo-plan.json's own `file.content`, so
// the hash is verifiable (`seeddemo verify` recomputes it) and the demo's
// datasets are exactly the files it documents. The blob's integrity_state
// stays 'pending' because nothing in this build ever stores or verifies the
// bytes — claiming 'verified' would be the lie this path is trying to avoid.
const pathFixtureBlob = "database (blob fixture rows: this build has no blob upload/attach route — see ops/seed-demo.md)"

// blobFieldFor names the payload field an object type's files live in. It
// mirrors internal/rsg/integrity/engine.go's blobRefFields and
// internal/rsg/validation/spec.go's typeRequiredFields: the two product
// tables that decide which types need a file and where it is written.
var blobFieldFor = map[string]string{
	"dataset":     "blob_ids",
	"calculation": "output_blob_ids",
}

// blobRoleFor is the attachment_role the fixture records. It is the role the
// product's own blob fixture uses ("data" for a dataset, "output" for a
// calculation), and it is the only place a role is chosen at all — the
// checks above look at the payload field, not at the role.
var blobRoleFor = map[string]string{
	"dataset":     "data",
	"calculation": "output",
}

// blobStore is the seed's one direct database writer. It is a separate type
// from discover on purpose: that one is read-only by contract ("Nothing here
// writes"), and the single write path deserves to be impossible to miss.
type blobStore struct{ pool *pgxpool.Pool }

func (s *blobStore) contentHash(content string) (hash string, size int) {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:]), len(content)
}

// ensureBlob writes the blob row for one plan object, or returns the row an
// earlier run wrote. The identity is (content_hash, size_bytes), which is the
// table's own unique key, so a re-run finds the same row rather than a second
// copy of the same file.
func (s *blobStore) ensureBlob(ctx context.Context, projectSlug, key, mediaType, content, createdBy string) (blobID string, created bool, err error) {
	hash, size := s.contentHash(content)
	err = s.pool.QueryRow(ctx,
		`select id from blobs where content_hash = $1 and size_bytes = $2`, hash, size).Scan(&blobID)
	if err == nil {
		return blobID, false, nil
	}
	if !isNoRows(err) {
		return "", false, fmt.Errorf("look up the blob for %s: %w", key, err)
	}
	storageKey := "s3://post-blobs/seed-demo/" + projectSlug + "/" + key + extensionFor(mediaType)
	err = s.pool.QueryRow(ctx, `
		insert into blobs (content_hash, size_bytes, media_type, storage_key, integrity_state, created_by)
		values ($1, $2, $3, $4, 'pending', $5)
		returning id`, hash, size, mediaType, storageKey, createdBy).Scan(&blobID)
	if err != nil {
		return "", false, fmt.Errorf("insert the blob for %s: %w", key, err)
	}
	return blobID, true, nil
}

// findBlobByContent looks the blob row up by the hash and size of the file
// content the plan declares, without writing anything. It is the read half of
// ensureBlob, and it is what a merge refresh needs: the file is already in
// the database by then, and writing it again would hide a bug behind an
// insert.
func (s *blobStore) findBlobByContent(ctx context.Context, content string) (blobID string, found bool, err error) {
	hash, size := s.contentHash(content)
	err = s.pool.QueryRow(ctx,
		`select id from blobs where content_hash = $1 and size_bytes = $2`, hash, size).Scan(&blobID)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("look up the blob by content hash: %w", err)
	}
	return blobID, true, nil
}

// attachBlob records the attachment that makes the blob a member of the
// version's state manifest (queries/manifest.sql reads blobs through
// blob_attachments, filtered by the attachment's own state). The state is the
// version's creating state, read here because the object route returns the
// version id and not the state it was written in.
func (s *blobStore) attachBlob(ctx context.Context, blobID, versionID, role, accessLevel string) (attachedAt string, created bool, err error) {
	if err := s.pool.QueryRow(ctx,
		`select state_id::text from scientific_object_versions where id = $1`, versionID).Scan(&attachedAt); err != nil {
		return "", false, fmt.Errorf("read the state of version %s: %w", versionID, err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		select exists (
			select 1 from blob_attachments
			where blob_id = $1 and scientific_object_version_id = $2 and attachment_role = $3
		)`, blobID, versionID, role).Scan(&exists); err != nil {
		return "", false, fmt.Errorf("look up the attachment: %w", err)
	}
	if exists {
		return attachedAt, false, nil
	}
	if _, err := s.pool.Exec(ctx, `
		insert into blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		values ($1, $2, $3, $4, $5)`, blobID, versionID, role, accessLevel, attachedAt); err != nil {
		return "", false, fmt.Errorf("attach the blob to version %s: %w", versionID, err)
	}
	return attachedAt, true, nil
}

// accessLevelFor reads the access level the object's own payload declares.
// A dataset's schema fixes access_level to open|restricted, and the asset
// publish gate compares the two halves of the same statement: a manifest that
// promises open data access over a blob that is not open is refused
// (internal/assets' preview, "the rights declaration states data_access=open
// while this blob is not open"). Deriving the attachment from the object is
// what keeps the two from contradicting each other — and it keeps the demo's
// private branch data restricted, which is the difference the selective
// publication story is about.
//
// Anything that declares nothing is restricted: widening access is an
// explicit act (docs/12 §3), never a default.
func accessLevelFor(o PlanObject) string {
	if level, ok := o.Payload["access_level"].(string); ok && (level == "open" || level == "restricted") {
		return level
	}
	return "restricted"
}

func extensionFor(mediaType string) string {
	switch mediaType {
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	default:
		return ".bin"
	}
}

// ensureObjectFile writes the plan's file for one object as a blob row, and
// returns the payload field and value the object's payload must carry. It is
// the DB-path half of ensureObject, named so the caller cannot write the
// object without it.
func (b *builder) ensureObjectFile(ctx context.Context, o PlanObject, createdBy string) (field string, blobID string, err error) {
	field, ok := blobFieldFor[o.Type]
	if !ok {
		if o.File != nil {
			return "", "", fmt.Errorf("object %s declares a file but its type %s has no blob field (blobFieldFor)", o.Key, o.Type)
		}
		return "", "", nil
	}
	if o.File == nil || strings.TrimSpace(o.File.Content) == "" {
		return "", "", fmt.Errorf("object %s is a %s with no file: the main gate blocks a %s whose %s is empty (internal/rsg/validation requiredFields), and the integrity engine refuses a payload naming a blob that does not exist", o.Key, o.Type, o.Type, field)
	}
	store := &blobStore{pool: b.db.pool}
	id, created, err := store.ensureBlob(ctx, b.plan.Project.Slug, o.Key, o.File.MediaType, o.File.Content, createdBy)
	if err != nil {
		return "", "", err
	}
	hash, size := store.contentHash(o.File.Content)
	status, detail := "reused", "already present (same content hash and size)"
	if created {
		status, detail = "created", fmt.Sprintf("bytes=%d sha256=%s", size, truncate(hash, 16))
	}
	b.item("file for "+o.Type+" "+o.Key, status, pathFixtureBlob,
		fmt.Sprintf("%s=%s media_type=%s %s", field, id, o.File.MediaType, detail))
	return field, id, nil
}

// attachObjectFile records the attachment of the object's blob to the version
// the API just created, in that version's state.
//
// branchName names the branch that version lives on, and it is part of the
// item's name on purpose: one object can be attached twice in a single build —
// once on the branch that wrote it, and once on main when the merge
// re-materialises the object as a new version — and two items sharing a name
// would read as a duplicate write rather than as the merge's two versions.
func (b *builder) attachObjectFile(ctx context.Context, o PlanObject, blobID, versionID, branchName string) error {
	store := &blobStore{pool: b.db.pool}
	role := blobRoleFor[o.Type]
	access := accessLevelFor(o)
	stateID, created, err := store.attachBlob(ctx, blobID, versionID, role, access)
	if err != nil {
		return err
	}
	status, detail := "reused", "already attached"
	if created {
		status, detail = "created", "attached"
	}
	b.item("attachment "+role+" for "+o.Key+" on "+branchName, status, pathFixtureBlob,
		fmt.Sprintf("blob_id=%s version_id=%s state_id=%s access_level=%s — %s", blobID, versionID, stateID, access, detail))
	return nil
}
