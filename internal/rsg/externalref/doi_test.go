package externalref

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The DOI adapter is tested against a local server (httptest) with the
// client injected via WithDoiClient — no network, and the URL policy is
// deliberately the injected transport's responsibility (WithDoiClient's
// documented contract). The SSRF guard has its own tests (ssrf_test.go).

// doiServer records what the adapter asked for and answers canned
// responses. The recorded request is mutex-protected: the handler runs
// in the server's goroutine, the assertions read it from the test's.
func doiServer(t *testing.T, status int, contentType, body string) (*httptest.Server, func() *http.Request) {
	t.Helper()
	var mu sync.Mutex
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r
		mu.Unlock()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() *http.Request {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestDoiFetcherFetchSuccess(t *testing.T) {
	csl := `{"title":"An example paper","version":"v2","DOI":"10.1000/XYZ","publisher":"Test Press"}`
	srv, lastReq := doiServer(t, http.StatusOK, "application/vnd.citationstyles.csl+json", csl)

	f := NewDoiFetcher(WithDoiBaseURL(srv.URL), WithDoiClient(srv.Client()))
	meta, err := f.Fetch(context.Background(), Ref{
		SourceType:         "publication",
		ExternalIdentifier: "DOI:10.1000/XYZ", // uppercase + scheme: normalized before the request
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if meta.UpstreamVersion != "v2" {
		t.Errorf("UpstreamVersion = %q, want %q", meta.UpstreamVersion, "v2")
	}
	// The metadata is canonical JSON carrying every upstream field.
	var doc map[string]any
	if err := json.Unmarshal(meta.Metadata, &doc); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if doc["title"] != "An example paper" {
		t.Errorf("metadata title = %v", doc["title"])
	}
	// The request went to /<normalized-doi> with the CSL-JSON Accept —
	// DOI resolution is the URL path, not a query parameter.
	if got := lastReq(); got == nil {
		t.Fatal("no request reached the server")
	} else {
		if got.URL.Path != "/10.1000/xyz" {
			t.Errorf("request path = %q, want /10.1000/xyz (normalized DOI)", got.URL.Path)
		}
		if got.Header.Get("Accept") != "application/vnd.citationstyles.csl+json" {
			t.Errorf("Accept = %q, want the CSL-JSON media type", got.Header.Get("Accept"))
		}
	}
}

func TestDoiFetcherFetchUpstreamFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		status     int
		content    string
		identifier string
		maxBytes   int64
		want       error
	}{
		{
			name:       "404",
			status:     http.StatusNotFound,
			content:    "not here",
			identifier: "10.1000/absent",
			want:       ErrUpstreamUnavailable,
		},
		{
			name:       "500",
			status:     http.StatusInternalServerError,
			content:    "boom",
			identifier: "10.1000/broken",
			want:       ErrUpstreamUnavailable,
		},
		{
			name:       "not JSON",
			status:     http.StatusOK,
			content:    "<html>not a document</html>",
			identifier: "10.1000/html",
			want:       ErrInvalidUpstream,
		},
		{
			name:       "oversize document",
			status:     http.StatusOK,
			content:    `{"pad":"` + strings.Repeat("x", 512) + `"}`,
			identifier: "10.1000/big",
			maxBytes:   64,
			want:       ErrInvalidUpstream,
		},
		{
			name:       "non-DOI identifier",
			status:     http.StatusOK,
			content:    "{}",
			identifier: "arXiv:2401.00001",
			want:       ErrNotResolvable,
		},
		{
			name:       "empty identifier",
			status:     http.StatusOK,
			content:    "{}",
			identifier: "",
			want:       ErrNotResolvable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := doiServer(t, tc.status, "application/json", tc.content)
			opts := []DoiOption{WithDoiBaseURL(srv.URL), WithDoiClient(srv.Client())}
			if tc.maxBytes > 0 {
				opts = append(opts, WithDoiMaxBytes(tc.maxBytes))
			}
			f := NewDoiFetcher(opts...)
			_, err := f.Fetch(ctx, Ref{SourceType: "publication", ExternalIdentifier: tc.identifier})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Fetch error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDoiFetcherUpstreamVersionOptional pins the version extraction
// contract: a document without a version field reports an empty
// UpstreamVersion (the snapshot column is nullable) — absence is a fact
// about upstream, not an error.
func TestDoiFetcherUpstreamVersionOptional(t *testing.T) {
	srv, _ := doiServer(t, http.StatusOK, "application/json", `{"title":"no version here"}`)
	f := NewDoiFetcher(WithDoiBaseURL(srv.URL), WithDoiClient(srv.Client()))
	meta, err := f.Fetch(context.Background(), Ref{SourceType: "web", ExternalIdentifier: "10.1000/nover"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if meta.UpstreamVersion != "" {
		t.Errorf("UpstreamVersion = %q, want empty when upstream reports none", meta.UpstreamVersion)
	}
}
