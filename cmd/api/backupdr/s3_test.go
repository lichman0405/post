package backupdr

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The S3 half of the drill talks to a real object store, so these tests do
// too — and skip LOUDLY when there is none, the required-test shape
// tests/integration/git_reconciliation_test.go establishes. A silently
// skipped S3 test would be the same defect as a silently skipped restore
// drill: the run looks green and nothing was verified.
//
// Endpoint and credentials come from the same variables the product reads
// (POST_BLOB_ENDPOINT / POST_BLOB_ACCESS_KEY / POST_BLOB_SECRET_KEY /
// POST_BLOB_BUCKET, internal/config/config.go) with the dev-stack defaults
// infra/docker documents. Nothing here reaches a production deployment's
// credentials (docs/25 §32 forbids a Worker doing that at all).

const s3TestBucket = "post-drill-unit"

func s3TestClient(t *testing.T) *s3Client {
	t.Helper()
	endpoint := envDefault("POST_BLOB_ENDPOINT", "http://127.0.0.1:9000")
	client, err := newS3Client(endpoint,
		envDefault("POST_BLOB_ACCESS_KEY", "minio_dev"),
		envDefault("POST_BLOB_SECRET_KEY", "minio_dev_pw"),
		envDefault("POST_BLOB_USE_TLS", "false") == "true")
	if err != nil {
		t.Fatalf("s3: build client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.CreateBucket(ctx, s3TestBucket); err != nil {
		t.Skipf("s3: no object store at %s (%v) — the drill's blob half needs MinIO "+
			"(`make infra-up`); CI has none, so the real coverage runs at the G3 gate", endpoint, err)
	}
	return client
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestS3RoundTrip proves the hand-rolled SigV4 client can actually talk to
// the configured store: sign a PUT, read the bytes back with a signed GET,
// list them, and delete. Every one of those is a different signature
// (different method, different canonical query, different payload hash), so
// a client that got the canonical request wrong fails here rather than
// halfway through a restore.
func TestS3RoundTrip(t *testing.T) {
	client := s3TestClient(t)
	ctx := context.Background()

	key := "unit/roundtrip-" + strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "-") + ".bin"
	body := []byte("the drill's blob half round-trips exactly these bytes")
	t.Cleanup(func() {
		_ = client.DeleteObject(context.Background(), s3TestBucket, key)
	})

	if err := client.PutObject(ctx, s3TestBucket, key, body); err != nil {
		t.Fatalf("put: %v (signed PUT refused — check endpoint, region and credentials)", err)
	}
	got, err := client.GetObject(ctx, s3TestBucket, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("get returned %d bytes %q, want %q", len(got), got, body)
	}

	listed, err := client.ListObjects(ctx, s3TestBucket, "unit/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *objectInfo
	for i := range listed {
		if listed[i].Key == key {
			found = &listed[i]
		}
	}
	if found == nil {
		t.Fatalf("list under unit/ did not return %s (got %d objects)", key, len(listed))
	}
	if found.Size != int64(len(body)) {
		t.Fatalf("listed size = %d, want %d", found.Size, len(body))
	}

	if err := client.DeleteObject(ctx, s3TestBucket, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := client.GetObject(ctx, s3TestBucket, key); err == nil {
		t.Fatal("get after delete succeeded — the delete did not land")
	}
}

// TestS3SignatureRejectsWrongCredentials is the negative half: the client
// must be able to be told no. A signer that accepted anything (or that the
// store answered without checking) would make every storage assertion in
// the drill unfalsifiable, so the wrong secret must be refused.
func TestS3SignatureRejectsWrongCredentials(t *testing.T) {
	good := s3TestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bad, err := newS3Client(envDefault("POST_BLOB_ENDPOINT", "http://127.0.0.1:9000"),
		envDefault("POST_BLOB_ACCESS_KEY", "minio_dev"),
		"definitely-not-the-secret",
		false)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = bad.GetObject(ctx, s3TestBucket, "unit/does-not-matter")
	if err == nil {
		t.Fatal("a request signed with the wrong secret key was accepted — storage assertions in the drill would be meaningless")
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") &&
		!strings.Contains(err.Error(), "AccessDenied") &&
		!strings.Contains(err.Error(), "InvalidAccessKeyId") &&
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong credentials were refused, but not by the signature check: %v", err)
	}
	t.Logf("wrong-secret request refused as expected: %v", err)

	// The same call with the real credential must still work, so the
	// refusal above is about the signature and not about the object.
	if _, err := good.ListObjects(ctx, s3TestBucket, "unit/"); err != nil {
		t.Fatalf("list with the correct credentials failed: %v", err)
	}
}

// TestS3CanonicalQuery pins the signature's canonical query encoding: keys
// and values encoded, pairs sorted. Getting this wrong produces signatures
// that fail only for the requests that carry a query string, which is
// exactly the kind of bug that survives a happy-path smoke test.
func TestS3CanonicalQuery(t *testing.T) {
	got := canonicalQuery(map[string][]string{
		"list-type":          {"2"},
		"prefix":             {"a b/c"},
		"continuation-token": {"x+y=z"},
	})
	want := "continuation-token=x%2By%3Dz&list-type=2&prefix=a%20b%2Fc"
	if got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
}

// TestS3URIEncodeKeepsPathSeparators: bucket and key segments are encoded
// with "/" preserved, query components are not.
func TestS3URIEncodeKeepsPathSeparators(t *testing.T) {
	if got := uriEncode("a/b c", false); got != "a/b%20c" {
		t.Fatalf("uriEncode(path) = %q", got)
	}
	if got := uriEncode("a/b c", true); got != "a%2Fb%20c" {
		t.Fatalf("uriEncode(query) = %q", got)
	}
}

// TestS3ClientRefusesEmptyEndpoint: a misconfigured drill must fail at
// construction, not send an unsigned request to an empty host.
func TestS3ClientRefusesEmptyEndpoint(t *testing.T) {
	if _, err := newS3Client("  ", "a", "b", false); err == nil {
		t.Fatal("an empty blob endpoint was accepted")
	}
	if _, err := newS3Client("http://", "a", "b", false); err == nil {
		t.Fatal("an endpoint with no host was accepted")
	}
}
