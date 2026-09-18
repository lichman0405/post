package backupdr

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// A minimal S3 client for the drill's blob half, over net/http and
// crypto/hmac only.
//
// Why hand-rolled: the blob store is reached by the *platform* through the
// internal/storage port (still a scaffold — doc.go and nothing else
// today), and the drill needs the bytes themselves, not the port's future
// semantics. Vendoring an S3 SDK would mean editing go.mod/go.sum, which
// this task's scope explicitly forbids, so the four operations the drill
// actually performs — list, get, put, delete — are signed here with the
// one algorithm MinIO speaks: AWS Signature Version 4, path-style.
//
// It is deliberately not a general S3 client: no multipart, no presigned
// URLs, no virtual-host addressing, no session tokens. Every limitation is
// a call site the drill does not have.

// s3Client speaks S3 to one endpoint with one credential pair.
type s3Client struct {
	endpoint  *url.URL
	accessKey string
	secretKey string
	region    string
	http      *http.Client
}

// newS3Client parses an endpoint URL ("http://127.0.0.1:9000"). useTLS is
// the config's POST_BLOB_USE_TLS flag: when set and the URL carries no
// scheme, https is assumed.
func newS3Client(endpoint, accessKey, secretKey string, useTLS bool) (*s3Client, error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return nil, fmt.Errorf("blob endpoint is empty")
	}
	if !strings.Contains(raw, "://") {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		raw = scheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("blob endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("blob endpoint %q has no host", endpoint)
	}
	return &s3Client{
		endpoint:  u,
		accessKey: accessKey,
		secretKey: secretKey,
		// MinIO's default region when none is configured. SigV4 needs a
		// region string; MinIO accepts us-east-1 for any bucket it serves.
		region: "us-east-1",
		http:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// objectInfo is one listed object: its key, size and ETag (the server's
// own verdict). The drill never trusts the ETag as the content identity —
// see blobRecord.Hash — it is recorded because it is what the store says.
type objectInfo struct {
	Key  string
	Size int64
	ETag string
}

// do signs and performs one request. body is the already-buffered request
// payload (nil for GET/DELETE): buffering keeps the payload hash and the
// wire bytes identical, which is what a content-addressed client needs.
// query is signed in its canonical sorted form.
func (c *s3Client) do(ctx context.Context, method, bucket, key string, query url.Values, body []byte) (*http.Response, error) {
	payloadHash := hex.EncodeToString(sha256Sum(nil))
	if body != nil {
		payloadHash = hex.EncodeToString(sha256Sum(body))
	}

	path := "/" + uriEncode(bucket, false)
	if key != "" {
		path += "/" + uriEncode(key, false)
	}

	u := &url.URL{Scheme: c.endpoint.Scheme, Host: c.endpoint.Host, Path: path, RawQuery: query.Encode()}
	// The canonical URI must be the encoded path exactly as it goes on the
	// wire, so RawPath is pinned to it and URL.EscapedPath() is stable.
	u.RawPath = path

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytesReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	// Signed headers: host (set by the transport, signed from req.Host)
	// plus the two we set above, in sorted order.
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + u.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		method,
		path,
		canonicalQuery(query),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(c.signingKey(dateStamp), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, scope, signedHeaders, signature))

	return c.http.Do(req)
}

// signingKey is the AWS4 derivation: date → region → service → "aws4_request".
func (c *s3Client) signingKey(dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+c.secretKey), []byte(dateStamp))
	k = hmacSHA256(k, []byte(c.region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

// ListObjects lists every object under prefix, following continuation
// tokens to the end. It is the whole listing: a truncated listing that
// returned quietly would let the drill back up a prefix of the bucket and
// call it complete, so a missing NextContinuationToken is the only way the
// loop ends.
func (c *s3Client) ListObjects(ctx context.Context, bucket, prefix string) ([]objectInfo, error) {
	var out []objectInfo
	token := ""
	for {
		q := url.Values{"list-type": {"2"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, bucket, "", q, nil)
		if err != nil {
			return nil, err
		}
		raw, err := readAndClose(resp, "list objects in "+bucket)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				ETag         string `xml:"ETag"`
				StorageClass string `xml:"StorageClass"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("list objects in %s: parse response: %w", bucket, err)
		}
		for _, o := range parsed.Contents {
			if strings.HasSuffix(o.Key, "/") {
				continue // a directory marker, not an object
			}
			out = append(out, objectInfo{Key: o.Key, Size: o.Size, ETag: strings.Trim(o.ETag, `"`)})
		}
		if !parsed.IsTruncated {
			return out, nil
		}
		if parsed.NextContinuationToken == "" {
			return nil, fmt.Errorf("list objects in %s: response says truncated with no continuation token", bucket)
		}
		token = parsed.NextContinuationToken
	}
}

// GetObject reads one object's bytes.
func (c *s3Client) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, bucket, key, nil, nil)
	if err != nil {
		return nil, err
	}
	return readAndClose(resp, "get "+bucket+"/"+key)
}

// PutObject writes one object's bytes.
func (c *s3Client) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	resp, err := c.do(ctx, http.MethodPut, bucket, key, nil, body)
	if err != nil {
		return err
	}
	if _, err := readAndClose(resp, "put "+bucket+"/"+key); err != nil {
		return err
	}
	return nil
}

// DeleteObject removes one object. Used only on the drill's own run-scoped
// target bucket.
func (c *s3Client) DeleteObject(ctx context.Context, bucket, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, key, nil, nil)
	if err != nil {
		return err
	}
	_, err = readAndClose(resp, "delete "+bucket+"/"+key)
	return err
}

// CreateBucket creates the bucket if it does not exist. A 409
// (BucketAlreadyOwnedByYou) is success: the drill's target bucket is
// run-scoped, so an existing one can only be a re-run.
func (c *s3Client) CreateBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodPut, bucket, "", nil, nil)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	// The body carries the store's own explanation and is the only useful
	// thing in a failed create; a body that could not be read is reported as
	// such rather than as an empty message.
	if readErr != nil {
		return fmt.Errorf("create bucket %s: %s (the response body could not be read: %w)",
			bucket, resp.Status, readErr)
	}
	return fmt.Errorf("create bucket %s: %s: %s", bucket, resp.Status, truncate(string(body), 400))
}

// BucketExists probes the bucket without reading the whole listing.
func (c *s3Client) BucketExists(ctx context.Context, bucket string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, bucket, "", nil, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("head bucket %s: %s", bucket, resp.Status)
	}
}

// readAndClose drains and closes a response, turning a non-2xx into an
// error that names the operation and carries the server's own words.
func readAndClose(resp *http.Response, what string) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", what, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: %s: %s", what, resp.Status, truncate(string(body), 400))
	}
	if int64(len(body)) > maxObjectBytes {
		return nil, fmt.Errorf("%s: object exceeds the %d-byte drill limit", what, int64(maxObjectBytes))
	}
	return body, nil
}

// canonicalQuery is the SigV4 canonical query string: encoded key/value
// pairs sorted by key, joined with "&".
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for j, v := range vs {
			if j > 0 {
				sb.WriteByte('&')
			}
			sb.WriteString(uriEncode(k, true))
			sb.WriteByte('=')
			sb.WriteString(uriEncode(v, true))
		}
	}
	return sb.String()
}

// uriEncode is the SigV4 URI encoder. encodeSlash=false keeps "/" literal
// (path segments); encodeSlash=true is for query components.
func uriEncode(s string, encodeSlash bool) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			sb.WriteByte(ch)
		case ch == '/' && !encodeSlash:
			sb.WriteByte(ch)
		default:
			fmt.Fprintf(&sb, "%%%02X", ch)
		}
	}
	return sb.String()
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
