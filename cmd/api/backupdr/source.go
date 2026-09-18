package backupdr

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Building, and reaching, the read-only port over a restored environment.
//
// The distinction this file exists to hold: NewRestoredSource hands out the
// reconciler's surface, which reads and nothing else. PutRestoredObject and
// GetRestoredObject are the operator's surface — the bytes of one object,
// written and read whole — and they are deliberately NOT on ReconcileSource,
// so no code path that starts at Reconcile can reach a write. The drill's
// own test uses them to inject the drift docs/37 §V1 验收 asks for ("故意破坏
// 一样东西"), which is exactly the case a read-only port must not be able
// to perform itself.

// SourceConfig names one restored environment and the credentials to reach
// it. The credentials are the dev stack's, read from the existing
// environment (docs/25 §32 forbids a Worker reaching production deployment
// credentials; nothing here reads one).
type SourceConfig struct {
	Env
	BlobAccessKey string
	BlobSecretKey string
	// GiteaToken is a run-scoped token the caller minted. Empty means the
	// Git half is not reachable — which is a failure of the check, not a
	// skipped axis.
	GiteaToken string
	RunID      string
}

// NewRestoredSource builds the reconciler's read-only port over a restored
// environment. The returned pool is the caller's to close.
//
// The Git client is built here and handed to the source only as a
// ReconcileSource, so the write methods the same client carries (createRepo,
// pushMirror, deleteOrg …) are not reachable from anything the reconciler
// holds.
func NewRestoredSource(ctx context.Context, cfg SourceConfig) (ReconcileSource, *pgxpool.Pool, error) {
	if cfg.PostgresURL == "" {
		return nil, nil, fmt.Errorf("drill source: no postgres url for the restored environment")
	}
	if cfg.BlobBucket == "" {
		return nil, nil, fmt.Errorf("drill source: no blob bucket for the restored environment")
	}
	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, nil, fmt.Errorf("drill source: open the restored database: %w", err)
	}
	s3, err := newS3Client(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, false)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	git := newGitClient(cfg.GiteaBaseURL, cfg.GiteaToken, cfg.RunID)
	return newDrillSource(pool, cfg.BlobBucket, s3, git), pool, nil
}

// PutRestoredObject writes one object into a restored environment's store.
//
// It is the operator's and the drill test's surface, not the reconciler's:
// a reconciliation must never call it, and cannot — it is not on
// ReconcileSource. Its documented use is (a) an operator re-uploading an
// object by hand, and (b) a test injecting the drift the acceptance
// criterion names, which has to be able to damage the environment the
// reconciler is about to read.
func PutRestoredObject(ctx context.Context, cfg SourceConfig, bucket, key string, body []byte) error {
	s3, err := newS3Client(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, false)
	if err != nil {
		return err
	}
	if bucket == "" {
		bucket = cfg.BlobBucket
	}
	return s3.PutObject(ctx, bucket, key, body)
}

// GetRestoredObject reads one object whole out of a restored environment's
// store. It is the reader half of PutRestoredObject: after a pass that
// found drift, a caller has to be able to look at the bytes and see that
// the reconciler left them exactly as broken as it found them.
func GetRestoredObject(ctx context.Context, cfg SourceConfig, bucket, key string) ([]byte, error) {
	s3, err := newS3Client(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, false)
	if err != nil {
		return nil, err
	}
	if bucket == "" {
		bucket = cfg.BlobBucket
	}
	return s3.GetObject(ctx, bucket, key)
}

// EnsureBucket creates one of the drill's run-scoped buckets if it is not
// there yet. Like PutRestoredObject this is the operator's surface, not the
// reconciler's: it is how a caller prepares a source or target environment
// before any pass runs.
func EnsureBucket(ctx context.Context, cfg SourceConfig, bucket string) error {
	if bucket == "" {
		bucket = cfg.BlobBucket
	}
	if bucket == "" {
		return fmt.Errorf("drill source: no bucket to create")
	}
	s3, err := newS3Client(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, false)
	if err != nil {
		return err
	}
	return s3.CreateBucket(ctx, bucket)
}

// DeleteRestoredObject removes one object from a restored environment's
// store. It is cleanup for the run-scoped buckets the drill creates and
// owns; it is not on ReconcileSource, so no pass can reach it.
func DeleteRestoredObject(ctx context.Context, cfg SourceConfig, bucket, key string) error {
	if bucket == "" {
		bucket = cfg.BlobBucket
	}
	s3, err := newS3Client(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, false)
	if err != nil {
		return err
	}
	return s3.DeleteObject(ctx, bucket, key)
}

// mustS3 builds a store client for a path that has already validated its
// credentials (Restore and Backup both construct one before this point), so
// a failure here is a programming error rather than an operator's mistake.
func mustS3(endpoint, accessKey, secretKey string) *s3Client {
	c, err := newS3Client(endpoint, accessKey, secretKey, false)
	if err != nil {
		panic(fmt.Sprintf("drill: blob endpoint already validated but refused: %v", err))
	}
	return c
}
